package warp

import (
	"errors"
	"net"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

func TestSimpleBindSendAndClose(t *testing.T) {
	bind := &simpleBind{}
	receives, _, err := bind.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := bind.ParseEndpoint("127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	if err := bind.Send([][]byte{[]byte("probe")}, endpoint); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := bind.Close(); err != nil {
		t.Fatal(err)
	}
	for _, receive := range receives {
		done := make(chan error, 1)
		go func(receive conn.ReceiveFunc) {
			_, err := receive([][]byte{make([]byte, 64)}, []int{0}, []conn.Endpoint{nil})
			done <- err
		}(receive)
		select {
		case err := <-done:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("receive after close: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("receive did not unblock after close")
		}
	}
}
