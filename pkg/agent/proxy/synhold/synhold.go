// Package synhold holds an application's TCP handshake with the recording
// proxy until the proxy has dialled the application's real destination, and
// then answers the handshake the way that destination answered the proxy.
//
// The eBPF connect hook points an application's connect() at the proxy, whose
// listening socket completes every handshake at once. Without this package an
// application connecting to a server that is down therefore saw its connect
// SUCCEED, and only learnt otherwise from the EOF that followed when the proxy's
// own dial failed. Drivers treat that as a dead server, not a refused one, so
// retry-on-refused logic never ran; a blackholed or unreachable host looked
// connected and then hung. Holding the SYN until the destination answers keeps
// connect's result the destination's own: ECONNREFUSED, EHOSTUNREACH,
// ENETUNREACH, or a connect that never completes.
//
// The SYN is held in an NFQUEUE (nf_tables "queue", Linux 3.x and later); the
// answer is a netfilter "reject" (a TCP reset or an ICMP unreachable, built by
// the kernel). Both are the same on every kernel the agent supports. When the
// queue cannot be set up, Start fails and the proxy accepts handshakes at once,
// as it always did.
package synhold

import (
	"context"
	"errors"
	"net/netip"
	"syscall"
)

// Outcome is how a held handshake is answered.
type Outcome uint8

const (
	// Accept lets the handshake complete: the destination accepted the
	// proxy's connection.
	Accept Outcome = iota
	// Refuse answers the SYN with a TCP reset: the destination refused
	// (ECONNREFUSED).
	Refuse
	// HostUnreachable answers with an ICMP host-unreachable (EHOSTUNREACH).
	HostUnreachable
	// NetUnreachable answers with an ICMP no-route (ENETUNREACH).
	NetUnreachable
	// Drop leaves the SYN unanswered: the destination never answered, so the
	// application's connect times out on its own clock, as it would have.
	Drop
)

func (o Outcome) String() string {
	switch o {
	case Accept:
		return "accept"
	case Refuse:
		return "refuse"
	case HostUnreachable:
		return "host-unreachable"
	case NetUnreachable:
		return "net-unreachable"
	case Drop:
		return "drop"
	}
	return "unknown"
}

// OutcomeFor is the answer that gives an application's connect the result
// the proxy's own dial of the same destination got. An error that is not the
// destination's answer — the proxy stopping, its own resource limits —
// says nothing about the destination, so the handshake completes as it did
// before handshakes were held, and the proxy reports its own failure as it
// always has.
func OutcomeFor(err error) Outcome {
	switch {
	case err == nil:
		return Accept
	case errors.Is(err, syscall.ECONNREFUSED):
		return Refuse
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.EHOSTDOWN):
		return HostUnreachable
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.ENETDOWN):
		return NetUnreachable
	case errors.Is(err, syscall.ETIMEDOUT):
		return Drop
	}
	var t interface{ Timeout() bool }
	if errors.As(err, &t) && t.Timeout() {
		return Drop
	}
	return Accept
}

// Decision is how a held handshake is answered, with what the decision
// leaves behind for the connection (the upstream connection it dialled).
// Keep runs if the handshake is still held when the answer is given — before
// the answer, so whatever it publishes is in place when the connection
// arrives — and Discard runs instead if a newer connection from the same end
// replaced it meanwhile. Either may be nil.
type Decision struct {
	Outcome Outcome
	Keep    func()
	Discard func()
}

// Decide is asked once per held handshake, from its own goroutine, and may
// block for as long as the destination takes to answer; ctx ends when the
// handshake is abandoned or the Holder closes. client is the application's
// end of the connection and proxy the proxy's, both as the SYN carried them.
type Decide func(ctx context.Context, client, proxy netip.AddrPort) Decision
