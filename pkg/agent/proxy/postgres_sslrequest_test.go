package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestIsPostgresSSLRequest(t *testing.T) {
	// The Postgres SSLRequest packet on the wire is exactly 8 bytes:
	//   int32 length = 8        → 00 00 00 08
	//   int32 code   = 80877103 → 04 d2 16 2f
	canonical := []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}

	tests := []struct {
		name string
		buf  []byte
		want bool
	}{
		{"canonical", canonical, true},
		{"canonical with trailing bytes", append(canonical, 0xff, 0xff), true},
		{"too short — 7 bytes", canonical[:7], false},
		{"too short — empty", []byte{}, false},
		{"length wrong", []byte{0x00, 0x00, 0x00, 0x09, 0x04, 0xd2, 0x16, 0x2f}, false},
		{"code first byte wrong", []byte{0x00, 0x00, 0x00, 0x08, 0x05, 0xd2, 0x16, 0x2f}, false},
		{"code last byte wrong", []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x30}, false},
		{"all zero", make([]byte, 16), false},
		{"TLS ClientHello prefix", []byte{0x16, 0x03, 0x01, 0x00, 0xc8, 0x01, 0x00, 0x00}, false},
		{"HTTP GET prefix", []byte{'G', 'E', 'T', ' ', '/', ' ', 'H', 'T'}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPostgresSSLRequest(tc.buf)
			if got != tc.want {
				t.Fatalf("isPostgresSSLRequest(%v) = %v, want %v", tc.buf, got, tc.want)
			}
		})
	}
}

type mockDestInfo struct {
	addr *agent.NetworkAddress
	err  error
}

func (m *mockDestInfo) Get(ctx context.Context, srcPort uint16) (*agent.NetworkAddress, error) {
	return m.addr, m.err
}

func (m *mockDestInfo) Delete(ctx context.Context, srcPort uint16) error {
	return nil
}

func TestPostgresSSLRequest_RefuseSSL(t *testing.T) {
	// Ensure InterceptPostgresSSLRequest is disabled for this test.
	agent.InterceptPostgresSSLRequest = false

	logger, _ := zap.NewDevelopment()
	destInfo := &mockDestInfo{
		addr: &agent.NetworkAddress{
			Version:  4,
			IPv4Addr: 2130706433, // 127.0.0.1
			Port:     5432,
		},
	}

	opts := config.New()
	p := New(logger, destInfo, opts)
	p.setSession(&agent.Session{
		ID:   1,
		Mode: models.MODE_TEST,
		OutgoingOptions: models.OutgoingOptions{
			Mocking: true,
		},
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	errCh := make(chan error, 1)
	go func() {
		serverConn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer serverConn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		errCh <- p.handleConnection(ctx, serverConn)
	}()

	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer clientConn.Close()

	// Send Postgres SSLRequest
	canonicalSSLRequest := []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}
	if _, err := clientConn.Write(canonicalSSLRequest); err != nil {
		t.Fatalf("failed to write Postgres SSLRequest: %v", err)
	}

	// Read reply
	reply := make([]byte, 1)
	if _, err := clientConn.Read(reply); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if reply[0] != 'N' {
		t.Errorf("expected SSLResponse 'N', got %q", reply[0])
	}

	// Close the connection so handleConnection finishes with EOF
	clientConn.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("handleConnection failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("handleConnection timed out")
	}
}
