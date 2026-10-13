package proxy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestLooksLikeTLSClientHello(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"tls1.0 record", []byte{0x16, 0x03, 0x01, 0x02, 0x00}, true},
		{"tls1.3 record version", []byte{0x16, 0x03, 0x03}, true},
		{"upper bound 3.3", []byte{0x16, 0x03, 0x03}, true},
		{"just past bound 3.4", []byte{0x16, 0x03, 0x04}, false},
		{"minor 5", []byte{0x16, 0x03, 0x05}, false},
		{"major 2", []byte{0x16, 0x02, 0x01}, false},
		{"alert type", []byte{0x15, 0x03, 0x03}, false},
		{"http1", []byte("GET / HTTP/1.1\r\n"), false},
		{"h2 preface", []byte(clientPreface), false},
		{"too short", []byte{0x16, 0x03}, false},
		{"wrong minor", []byte{0x16, 0x03, 0x09}, false},
	}
	for _, c := range cases {
		if got := looksLikeTLSClientHello(c.in); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// A TLS handshake reaching the ingress listener must be relayed to the
// relocated listener byte for byte, not answered as HTTP/1.
func TestIngressRelaysTLSHandshakeUntouched(t *testing.T) {
	backend, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer backend.Close()
	hello := append([]byte{0x16, 0x03, 0x01, 0x00, 0x30}, make([]byte, 60)...)
	got := make(chan []byte, 1)
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, len(hello))
		_, _ = io.ReadFull(c, buf)
		got <- buf
		_, _ = c.Write([]byte{0x16, 0x03, 0x03, 0x00, 0x01, 0xAA})
	}()

	pm := &IngressProxyManager{logger: zap.NewNop()}
	cliSide, srvSide := net.Pipe()
	go pm.handleConnection(context.Background(), srvSide, backend.Addr().String(), zap.NewNop(), nil, make(chan struct{}, 1), 8443)

	if _, err := cliSide.Write(hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	reply := make([]byte, 6)
	_ = cliSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(cliSide, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply[0] != 0x16 || reply[5] != 0xAA {
		t.Fatalf("reply was not the backend's TLS bytes: %x", reply)
	}
	select {
	case b := <-got:
		if string(b) != string(hello) {
			t.Fatalf("backend saw altered bytes")
		}
	case <-time.After(time.Second):
		t.Fatal("backend never received the handshake")
	}
	_ = cliSide.Close()
}

// When the upstream closes first, the client side must see EOF rather than
// hang until it closes itself.
func TestIngressRelayHalfClosesTowardClient(t *testing.T) {
	backend, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 64)
		_, _ = io.ReadFull(c, buf[:5])
		_, _ = c.Write([]byte{0x16, 0x03, 0x03, 0x00, 0x01, 0xBB})
		_ = c.Close()
	}()

	front, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer front.Close()
	pm := &IngressProxyManager{logger: zap.NewNop()}
	go func() {
		c, err := front.Accept()
		if err != nil {
			return
		}
		pm.handleConnection(context.Background(), c, backend.Addr().String(), zap.NewNop(), nil, make(chan struct{}, 1), 8443)
	}()

	cli, err := net.Dial("tcp4", front.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	// Only the 24-byte preface read is needed for detection; send enough.
	hello := append([]byte{0x16, 0x03, 0x01, 0x00, 0x30}, make([]byte, 60)...)
	if _, err := cli.Write(hello); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, err := io.ReadAll(cli)
	if err != nil {
		t.Fatalf("client never saw EOF after upstream closed: %v (got %x)", err, data)
	}
	if len(data) != 6 || data[5] != 0xBB {
		t.Fatalf("unexpected reply %x", data)
	}
}
