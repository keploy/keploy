package synhold

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

// TestOutcomeForAnswersAsTheDestinationDid: each way the proxy's dial can
// fail is answered with the packet that makes the application's connect fail
// with the same errno.
func TestOutcomeForAnswersAsTheDestinationDid(t *testing.T) {
	wrap := func(errno syscall.Errno) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
	}
	for _, tc := range []struct {
		err  error
		want Outcome
	}{
		{nil, Accept},
		{wrap(syscall.ECONNREFUSED), Refuse},
		{wrap(syscall.EHOSTUNREACH), HostUnreachable},
		{wrap(syscall.EHOSTDOWN), HostUnreachable},
		{wrap(syscall.ENETUNREACH), NetUnreachable},
		{wrap(syscall.ENETDOWN), NetUnreachable},
		{wrap(syscall.ETIMEDOUT), Drop},
		{&net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}, Drop},
		{fmt.Errorf("dial: %w", wrap(syscall.ECONNREFUSED)), Refuse},
		// Not the destination's answer: complete the handshake, as before.
		{wrap(syscall.EMFILE), Accept},
		{wrap(syscall.EADDRNOTAVAIL), Accept},
		{context.Canceled, Accept}, // the proxy is stopping
		{errors.New("something else"), Accept},
	} {
		if got := OutcomeFor(tc.err); got != tc.want {
			t.Errorf("OutcomeFor(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestOutcomeString(t *testing.T) {
	for o, want := range map[Outcome]string{
		Accept: "accept", Refuse: "refuse", HostUnreachable: "host-unreachable",
		NetUnreachable: "net-unreachable", Drop: "drop", Outcome(99): "unknown",
	} {
		if got := o.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", o, got, want)
		}
	}
}
