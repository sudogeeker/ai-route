package opencode

import (
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestSOCKS5Connect(t *testing.T) {
	for _, credentials := range []*url.Userinfo{nil, url.UserPassword("route", "secret")} {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				result <- err
				return
			}
			defer conn.Close()
			greeting := make([]byte, 2)
			if _, err := io.ReadFull(conn, greeting); err != nil {
				result <- err
				return
			}
			methods := make([]byte, int(greeting[1]))
			if _, err := io.ReadFull(conn, methods); err != nil {
				result <- err
				return
			}
			method := byte(0x00)
			if credentials != nil {
				method = 0x02
			}
			if _, err := conn.Write([]byte{0x05, method}); err != nil {
				result <- err
				return
			}
			if method == 0x02 {
				header := make([]byte, 2)
				if _, err := io.ReadFull(conn, header); err != nil {
					result <- err
					return
				}
				values := make([]byte, int(header[1]))
				if _, err := io.ReadFull(conn, values); err != nil {
					result <- err
					return
				}
				if _, err := io.ReadFull(conn, header[:1]); err != nil {
					result <- err
					return
				}
				password := make([]byte, int(header[0]))
				if _, err := io.ReadFull(conn, password); err != nil {
					result <- err
					return
				}
				if string(values) != "route" || string(password) != "secret" {
					result <- io.ErrUnexpectedEOF
					return
				}
				if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
					result <- err
					return
				}
			}
			request := make([]byte, 4)
			if _, err := io.ReadFull(conn, request); err != nil {
				result <- err
				return
			}
			if request[0] != 0x05 || request[3] != 0x03 {
				result <- io.ErrUnexpectedEOF
				return
			}
			length := make([]byte, 1)
			if _, err := io.ReadFull(conn, length); err != nil {
				result <- err
				return
			}
			host := make([]byte, int(length[0]))
			if _, err := io.ReadFull(conn, host); err != nil {
				result <- err
				return
			}
			port := make([]byte, 2)
			if _, err := io.ReadFull(conn, port); err != nil {
				result <- err
				return
			}
			if string(host) != "opencode.ai" || binary.BigEndian.Uint16(port) != 443 {
				result <- io.ErrUnexpectedEOF
				return
			}
			result <- nil
			_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0})
		}()
		conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := socks5Connect(conn, "opencode.ai", 443, credentials); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		conn.Close()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		listener.Close()
	}
}

func TestSOCKS5RejectsUnsupportedAuthMethods(t *testing.T) {
	for _, item := range []struct {
		name  string
		user  *url.Userinfo
		reply []byte
	}{
		{name: "unsupported", reply: []byte{0x05, 0x01}},
		{name: "bad auth version", user: url.UserPassword("u", "p"), reply: []byte{0x02, 0x00}},
	} {
		t.Run(item.name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				greeting := make([]byte, 2)
				if _, err := io.ReadFull(conn, greeting); err != nil {
					serverDone <- err
					return
				}
				methods := make([]byte, int(greeting[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					serverDone <- err
					return
				}
				if _, err := conn.Write(item.reply); err != nil {
					serverDone <- err
					return
				}
				serverDone <- nil
			}()
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			err = socks5Connect(conn, "opencode.ai", 443, item.user)
			conn.Close()
			if err == nil {
				t.Fatal("unsupported authentication method was accepted")
			}
			if serverErr := <-serverDone; serverErr != nil {
				t.Fatal(serverErr)
			}
		})
	}
}
