//go:build !linux

package connseq

import (
	"net"
	"testing"
)

// Off Linux no socket is asked for a count: a TCP connection's Upstream has
// no socket counter, so it numbers by what Read has returned, as any other
// conn does (that numbering is upstream_test.go's, over a pipe, on every
// platform), and its socket is still there for what looks at it without
// reading (SyscallConn).
func TestOffLinuxATCPUpstreamAsksNoSocket(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	u := NewUpstream(c)
	if u.socket != nil {
		t.Fatal("a socket is asked for a count off Linux")
	}
	if _, err := u.SyscallConn(); err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
}
