package proxy

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestSentWantWaitsForWhatTheCallsCompletedOrMayStillSend holds a wait's
// first want from the shim's counts (sentWant): every byte the calls
// completed with once none is pending, every byte handed to one while some
// are, and unknown for a side that received more than was handed.
func TestSentWantWaitsForWhatTheCallsCompletedOrMayStillSend(t *testing.T) {
	for _, row := range []struct {
		name          string
		received      uint64
		s             appSends
		want          uint64
		sending, fail bool
	}{
		{name: "none pending: what the calls completed with", received: 10, s: appSends{done: 300, started: 400}, want: 300},
		{name: "none pending, all received", received: 300, s: appSends{done: 300, started: 300}, want: 300},
		{name: "a call pending: every byte handed, asked again", received: 262_000, s: appSends{done: 0, started: 8 << 20, pending: 1}, want: 8 << 20, sending: true},
		{name: "received exactly what was handed", received: 400, s: appSends{done: 400, started: 400}, want: 400},
		{name: "received more than was handed: unknown", received: 401, s: appSends{done: 400, started: 400}, fail: true},
		{name: "more than was handed while a call is pending: unknown", received: 401, s: appSends{started: 400, pending: 2}, fail: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			want, sending, err := sentWant(row.received, row.s)
			if row.fail {
				if !errors.Is(err, models.ErrProxyConnsUnsupported) {
					t.Fatalf("sentWant = %d, %v, %v; want an error wrapping ErrProxyConnsUnsupported", want, sending, err)
				}
				return
			}
			if err != nil || want != row.want || sending != row.sending {
				t.Fatalf("sentWant = %d, %v, %v; want %d, %v, nil", want, sending, err, row.want, row.sending)
			}
		})
	}
}

// TestSentAgainSettlesOnceNoCallIsPending holds a wait's want after a later
// read of the counts (sentAgain): unchanged while a call is pending; once none
// is, what the calls completed with, never more than the earlier want (calls
// made after the wait began cannot hold it back).
func TestSentAgainSettlesOnceNoCallIsPending(t *testing.T) {
	for _, row := range []struct {
		name           string
		want, received uint64
		s              appSends
		next           uint64
		sending, fail  bool
	}{
		{name: "still pending: the earlier want, asked again", want: 8 << 20, received: 1 << 20, s: appSends{done: 0, started: 9 << 20, pending: 1}, next: 8 << 20, sending: true},
		{name: "the pending call was cancelled: what completed", want: 8 << 20, received: 1 << 20, s: appSends{done: 1 << 20, started: 8 << 20}, next: 1 << 20},
		{name: "calls made since the wait: the earlier want", want: 500, received: 100, s: appSends{done: 900, started: 900}, next: 500},
		{name: "received exactly what was handed", want: 500, received: 900, s: appSends{done: 900, started: 900}, next: 500},
		{name: "received more than was handed: unknown", want: 500, received: 901, s: appSends{done: 900, started: 900}, fail: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			next, sending, err := sentAgain(row.want, row.received, row.s)
			if row.fail {
				if !errors.Is(err, models.ErrProxyConnsUnsupported) {
					t.Fatalf("sentAgain = %d, %v, %v; want an error wrapping ErrProxyConnsUnsupported", next, sending, err)
				}
				return
			}
			if err != nil || next != row.next || sending != row.sending {
				t.Fatalf("sentAgain = %d, %v, %v; want %d, %v, nil", next, sending, err, row.next, row.sending)
			}
		})
	}
}

// TestSentWroteAtCloseCountsWhatMayStillArrive holds what a close records the
// application had written (sentWrote): what the calls completed with once
// none is pending, every byte handed to one while some are, and no FIN.
func TestSentWroteAtCloseCountsWhatMayStillArrive(t *testing.T) {
	if w := sentWrote(appSends{done: 300, started: 400}); w.want != 300 || w.fin {
		t.Fatalf("none pending: %+v; want 300 bytes and no FIN", w)
	}
	if w := sentWrote(appSends{done: 300, started: 400, pending: 1}); w.want != 400 || w.fin {
		t.Fatalf("a call pending: %+v; want 400 bytes and no FIN", w)
	}
}
