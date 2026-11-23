package plaintcp_test

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/utils/plaintcp"
	"go.keploy.io/server/v3/utils/plaintcp/plaintcptest"
)

// Listen's listener is plain TCP, on every TCP network and on every interface
// as on loopback, where Go's default would be MPTCP.
func TestListenIsPlainTCP(t *testing.T) {
	for _, c := range []struct{ network, addr string }{
		{"tcp", ":0"},
		{"tcp4", "0.0.0.0:0"},
		{"tcp4", "127.0.0.1:0"},
	} {
		t.Run(c.network+" "+c.addr, func(t *testing.T) {
			ln, err := plaintcp.Listen(context.Background(), c.network, c.addr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			plaintcptest.RequirePlainTCP(t, ln)
		})
	}
}

// Config's listener is plain TCP, and its control still runs on the socket
// before it is bound.
func TestConfigIsPlainTCPAndRunsItsControl(t *testing.T) {
	ran := 0
	lc := plaintcp.Config(func(_, _ string, _ syscall.RawConn) error {
		ran++
		return nil
	})
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if ran != 1 {
		t.Fatalf("the control ran %d times, want once", ran)
	}
	plaintcptest.RequirePlainTCP(t, ln)
}

// The check itself fails an MPTCP listener, so a row that calls it can fail.
// Where the kernel opens no MPTCP listener this is not reachable, said so.
func TestRequirePlainTCPFailsAnMPTCPListener(t *testing.T) {
	var lc net.ListenConfig
	lc.SetMultipathTCP(true)
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ft := &fakeT{TB: t}
	func() {
		defer func() { _ = recover() }() // fakeT's Fatal/Skip end the call as testing's do
		plaintcptest.RequirePlainTCP(ft, ln)
	}()
	switch {
	case ft.skipped:
		t.Skipf("no MPTCP listener here: %s", ft.msg)
	case !ft.failed:
		t.Fatal("RequirePlainTCP passed an MPTCP listener")
	}
}

// fakeT records a Fatal or a Skip instead of ending the test, ending only the
// call (a panic the caller recovers).
type fakeT struct {
	testing.TB
	failed, skipped bool
	msg             string
}

func (f *fakeT) Helper() {}
func (f *fakeT) Fatal(...any) {
	f.failed = true
	panic(f)
}
func (f *fakeT) Fatalf(string, ...any) {
	f.failed = true
	panic(f)
}
func (f *fakeT) Skip(...any) {
	f.skipped = true
	panic(f)
}
func (f *fakeT) Skipf(format string, args ...any) {
	f.skipped = true
	f.msg = fmt.Sprintf(format, args...)
	panic(f)
}
