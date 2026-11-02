//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
)

// TestACutWaitMarksOnlyTheConnectionsItDidNotSeeReached pins which
// connections an end cut short marks with its test (waitPairTargets): those it
// had not seen reached. A connection whose bytes all arrived in the wait owes
// that test nothing: a loss on it later is the test open then.
func TestACutWaitMarksOnlyTheConnectionsItDidNotSeeReached(t *testing.T) {
	p, app1, side1, reader1 := heldPair(t, "tcp4", "127.0.0.1:0")
	msg := []byte("Q\x00\x00\x00\x0eSELECT 1;\x00")
	if _, err := app1.Write(msg); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader1, make([]byte, len(msg))); err != nil {
		t.Fatal(err)
	}
	app2, side2, _ := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
	queue(t, app2)
	ctx, cancel := context.WithCancel(agent.WithEndOf(context.Background(), "test-1"))
	cancel()
	if err := p.WaitPairedBytes(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("the end's wait = %v, want it cut short", err)
	}
	p.paired.mu.Lock()
	reached, cut := p.paired.byConn[side1], p.paired.byConn[side2]
	p.paired.mu.Unlock()
	if owners, open := reached.lostBy(1, uint64(len(msg))); owners != nil || !open {
		t.Fatalf("the reached connection's loss is owned by %q (open test: %v), want the open test's alone", owners, open)
	}
	if owners, _ := cut.lostBy(1, 1); !slices.Equal(owners, []string{"test-1"}) {
		t.Fatalf("the cut connection's first byte is owned by %q, want test-1's", owners)
	}
}

// TestALossAtTheCloseEndsAtTheApplicationsFIN pins the last byte of a loss
// counted at the proxy's close (lostTop, dataReceived): the application's FIN,
// once its side has taken it, is no byte of the loss, and nothing the
// application wrote follows it or a reset; else what the application had
// written as the close began; else any byte.
func TestALossAtTheCloseEndsAtTheApplicationsFIN(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    *proxySide
		wrote *appWrote
		want  uint64
	}{
		{"its FIN taken (CLOSE_WAIT)", &proxySide{received: 11, state: tcpCloseWait}, &appWrote{want: 20}, 10},
		{"its FIN taken, then the proxy's (LAST_ACK)", &proxySide{received: 11, state: tcpLastAck}, &appWrote{want: 20}, 10},
		{"both FINs crossed (CLOSING)", &proxySide{received: 11, state: tcpClosing}, &appWrote{want: 20}, 10},
		{"reset (CLOSE)", &proxySide{received: 11, state: tcpClose}, &appWrote{want: 20}, 11},
		{"a FIN alone", &proxySide{received: 1, state: tcpCloseWait}, &appWrote{want: 20}, 0},
		{"no FIN yet", &proxySide{received: 11, state: tcpEstablished}, &appWrote{want: 20}, 20},
		{"its side unread", &proxySide{received: 11, state: tcpCloseWait, err: errors.New("gone")}, &appWrote{want: 20}, 20},
		{"neither known", nil, nil, math.MaxUint64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := &pairedConn{}
			if tc.at != nil {
				pc.atClose.Store(tc.at)
			}
			if tc.wrote != nil {
				pc.wroteAtClose.Store(tc.wrote)
			}
			if got := lostTop(pc); got != tc.want {
				t.Fatalf("lostTop = %d, want %d", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		ps   proxySide
		want uint64
	}{
		{proxySide{received: 11, state: tcpEstablished}, 11},
		{proxySide{received: 11, state: tcpCloseWait}, 10},
		{proxySide{received: 11, state: tcpLastAck}, 10},
		{proxySide{received: 11, state: tcpClosing}, 10},
		{proxySide{received: 11, state: tcpTimeWait}, 10},
		{proxySide{received: 11, state: tcpClose}, 11},
		{proxySide{received: 0, state: tcpCloseWait}, 0},
	} {
		if got := dataReceived(tc.ps); got != tc.want {
			t.Errorf("dataReceived(%+v) = %d, want %d", tc.ps, got, tc.want)
		}
	}
}
