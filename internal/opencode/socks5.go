package opencode

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// socks5DialContext implements the CONNECT subset of RFC 1928 and RFC 1929 so
// SOCKS-only WARP or proxy pools work without adding a dependency.
func (p *Proxy) socks5DialContext(proxy *url.URL) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, address string) (net.Conn, error) {
		host, portText, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, p.cfg.BaseURL.Hostname()) {
			return nil, errors.New("unexpected OpenCode upstream address")
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port <= 0 || port > 65535 {
			return nil, errors.New("invalid OpenCode upstream port")
		}
		dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		proxyAddress := proxy.Host
		if proxy.Port() == "" {
			proxyAddress = net.JoinHostPort(proxy.Hostname(), "1080")
		}
		conn, err := dialer.DialContext(ctx, "tcp", proxyAddress)
		if err != nil {
			return nil, err
		}
		deadline := time.Now().Add(10 * time.Second)
		if expires, ok := ctx.Deadline(); ok && expires.Before(deadline) {
			deadline = expires
		}
		_ = conn.SetDeadline(deadline)
		if err := socks5Connect(conn, host, port, proxy.User); err != nil {
			conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

func socks5Connect(conn net.Conn, host string, port int, user *url.Userinfo) error {
	methods := []byte{0x00}
	if user != nil {
		methods = []byte{0x00, 0x02}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 0x05 || reply[1] == 0xff {
		return errors.New("SOCKS5 proxy rejected authentication methods")
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		if user == nil {
			return errors.New("SOCKS5 proxy requires credentials")
		}
		username := []byte(user.Username())
		password, _ := user.Password()
		passwordBytes := []byte(password)
		if len(username) > 255 || len(passwordBytes) > 255 {
			return errors.New("SOCKS5 credentials are too long")
		}
		request := append([]byte{0x01, byte(len(username))}, username...)
		request = append(request, byte(len(passwordBytes)))
		request = append(request, passwordBytes...)
		if _, err := conn.Write(request); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return err
		}
		if reply[0] != 0x01 || reply[1] != 0x00 {
			return errors.New("SOCKS5 proxy authentication failed")
		}
	default:
		return errors.New("SOCKS5 proxy selected an unsupported authentication method")
	}
	request := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			request = append(request, 0x01)
			request = append(request, ipv4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("SOCKS5 destination host is too long")
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	request = append(request, portBytes...)
	if _, err := conn.Write(request); err != nil {
		return err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if header[0] != 0x05 || header[1] != 0x00 || header[2] != 0x00 {
		return errors.New("SOCKS5 proxy could not connect to the upstream")
	}
	var addressBytes int
	switch header[3] {
	case 0x01:
		addressBytes = 4
	case 0x04:
		addressBytes = 16
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return err
		}
		addressBytes = int(length[0])
	default:
		return errors.New("SOCKS5 proxy returned an invalid address type")
	}
	discard := make([]byte, addressBytes+2)
	_, err := io.ReadFull(conn, discard)
	return err
}
