package http

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// opaqueNetErr carries a real errno but renders a message that contains none
// of the substrings classifyUpstreamError greps for. That is not a contrivance:
// it is what Windows looks like. A reset there is errno 10054 rendered as
// "wsarecv: An existing connection was forcibly closed by the remote host",
// so the substring pass misses it and the recorded mock is marked
// upstream-error instead of upstream-unreachable. On Linux the two agree,
// which is why the substring-only version looked correct.
type opaqueNetErr struct{ err error }

func (o opaqueNetErr) Error() string { return "upstream exchange failed" }
func (o opaqueNetErr) Unwrap() error { return o.err }

func TestUpstreamErrorClassifiedByErrno(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"reset", opaqueNetErr{&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}},
		{"refused", opaqueNetErr{&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}},
		{"broken pipe", opaqueNetErr{&net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}}},
	}

	for _, c := range cases {
		if got := c.err.Error(); got != "upstream exchange failed" {
			t.Fatalf("harness: %s renders as %q, so the substring pass would catch it and this test would prove nothing", c.name, got)
		}

		status, _, marker := classifyUpstreamError(c.err)
		if status != 502 {
			t.Errorf("classifyUpstreamError(%s) status = %d, want 502", c.name, status)
		}
		if marker != "keploy-recorded-upstream-unreachable: true" {
			t.Errorf("classifyUpstreamError(%s) marker = %q, want the unreachable marker; "+
				"the errno was present but only the message text was consulted", c.name, marker)
		}

		if got := upstreamErrorClass(c.err); got != "unreachable" {
			t.Errorf("upstreamErrorClass(%s) = %q, want \"unreachable\"", c.name, got)
		}
	}
}

// The errno pass must not swallow classes that have their own handling.
func TestUpstreamErrorErrnoDoesNotStealOtherClasses(t *testing.T) {
	timeout := opaqueNetErr{&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}}
	if got := upstreamErrorClass(timeout); got != "timeout" {
		t.Errorf("upstreamErrorClass(timeout) = %q, want \"timeout\"", got)
	}

	plain := opaqueNetErr{errString("something structural")}
	if got := upstreamErrorClass(plain); got != "other" {
		t.Errorf("upstreamErrorClass(non-network) = %q, want \"other\"", got)
	}
	if _, _, marker := classifyUpstreamError(plain); marker != "keploy-recorded-upstream-error: true" {
		t.Errorf("classifyUpstreamError(non-network) marker = %q, want the generic error marker", marker)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
