package warp

import (
	"errors"
	"net"
	"net/netip"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
)

type simpleBind struct {
	mu     sync.Mutex
	ipv4   *net.UDPConn
	ipv6   *net.UDPConn
	closed bool
}

type simpleEndpoint struct {
	addr netip.AddrPort
}

func (b *simpleBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ipv4 != nil || b.ipv6 != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	b.closed = false
	var v4, v6 *net.UDPConn
	var err error
	if port == 0 {
		v4, err = net.ListenUDP("udp4", &net.UDPAddr{})
		if err == nil {
			port = uint16(v4.LocalAddr().(*net.UDPAddr).Port)
		}
	} else {
		v4, err = net.ListenUDP("udp4", &net.UDPAddr{Port: int(port)})
	}
	if err == nil {
		v6, _ = net.ListenUDP("udp6", &net.UDPAddr{Port: int(port)})
	}
	if v4 == nil && v6 == nil {
		v6, err = net.ListenUDP("udp6", &net.UDPAddr{Port: int(port)})
		if err != nil {
			return nil, 0, err
		}
		port = uint16(v6.LocalAddr().(*net.UDPAddr).Port)
	}
	var receives []conn.ReceiveFunc
	if v4 != nil {
		receives = append(receives, receiveUDP(v4))
	}
	if v6 != nil {
		receives = append(receives, receiveUDP(v6))
	}
	b.ipv4, b.ipv6 = v4, v6
	return receives, port, nil
}

func receiveUDP(udp *net.UDPConn) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, endpoints []conn.Endpoint) (int, error) {
		n, addr, err := udp.ReadFromUDPAddrPort(packets[0])
		if err != nil {
			return 0, err
		}
		sizes[0] = n
		endpoints[0] = &simpleEndpoint{addr: addr}
		return 1, nil
	}
}

func (b *simpleBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var errs []error
	if b.ipv4 != nil {
		errs = append(errs, b.ipv4.Close())
		b.ipv4 = nil
	}
	if b.ipv6 != nil {
		errs = append(errs, b.ipv6.Close())
		b.ipv6 = nil
	}
	return errors.Join(errs...)
}

func (b *simpleBind) SetMark(uint32) error { return nil }

func (b *simpleBind) Send(bufs [][]byte, endpoint conn.Endpoint) error {
	ep, ok := endpoint.(*simpleEndpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	b.mu.Lock()
	var udp *net.UDPConn
	if ep.addr.Addr().Is4() {
		udp = b.ipv4
	} else {
		udp = b.ipv6
	}
	closed := b.closed
	b.mu.Unlock()
	if closed || udp == nil {
		return net.ErrClosed
	}
	for _, buf := range bufs {
		if _, err := udp.WriteToUDPAddrPort(buf, ep.addr); err != nil {
			return err
		}
	}
	return nil
}

func (*simpleBind) ParseEndpoint(value string) (conn.Endpoint, error) {
	addr, err := netip.ParseAddrPort(value)
	if err != nil {
		return nil, err
	}
	return &simpleEndpoint{addr: addr}, nil
}

func (*simpleBind) BatchSize() int { return 1 }

func (e *simpleEndpoint) ClearSrc()           {}
func (e *simpleEndpoint) SrcToString() string { return "" }
func (e *simpleEndpoint) DstToString() string { return e.addr.String() }
func (e *simpleEndpoint) DstIP() netip.Addr   { return e.addr.Addr() }
func (e *simpleEndpoint) SrcIP() netip.Addr   { return netip.Addr{} }
func (e *simpleEndpoint) DstToBytes() []byte {
	value, _ := e.addr.MarshalBinary()
	return value
}
