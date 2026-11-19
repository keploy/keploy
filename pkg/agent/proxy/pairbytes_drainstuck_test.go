//go:build linux || darwin

package proxy

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// awaitDrainLook waits, bounded, until the drain has armed its next look on
// clk, or answered (then got holds the answer, put back).
func awaitDrainLook(t *testing.T, clk *stall.ManualClock, got chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second) // only turns a hang into a failure
	for clk.Pending() == 0 {
		select {
		case err := <-got:
			t.Fatalf("the drain answered (%v) while its handler had made no progress for less than its bound", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
}

// drainAt is a drain, on a clock the test moves, of p, which holds one
// paired connection whose application end has ended.
func drainAt(p *Proxy) (*stall.ManualClock, chan error) {
	clk := &stall.ManualClock{}
	p.paired.clk = clk
	got := make(chan error, 1)
	go func() { got <- p.PairedBytesDrain(context.Background()) }()
	return clk, got
}

// stuckLosses are the owned losses a drain that gave up made.
func stuckLosses(t *testing.T, p *Proxy) []agent.OwnedLoss {
	t.Helper()
	var out []agent.OwnedLoss
	for _, o := range p.TakeOwnedLosses() {
		if o.Loss.Kind == models.CaptureLossAppBytesUntold && strings.Contains(o.Loss.What, "never finished closing it") {
			out = append(out, o)
		}
	}
	return out
}

// A handler that never closes a connection whose application end has ended
// holds the session's last read for good, and ended green (or judged on what
// was counted) once the CLI gave the read up. The drain is bounded by
// progress instead: once keploy could run for drainBudget with no connection
// it waits for forgotten or its close begun, it owns each stuck close as the
// session's, app_bytes_untold (not judged), naming its destination, the
// application's end and the session, and answers; no later drain waits for
// it. A close begun is progress: the bound starts again. Wall time keploy
// could not run never counts.
func TestADrainGivesUpAHandlerThatNeverClosesByName(t *testing.T) {
	t.Run("named, owned once, and never waited for again", func(t *testing.T) {
		p, app, served := stuckPair(t)
		p.PairedBytesSession(2)
		p.paired.destined(served, "10.0.0.5:5432")
		_ = app.Close()
		awaitState(t, served.(*net.TCPConn), peerEnded, "the application's FIN")
		clk, got := drainAt(p)
		awaitDrainLook(t, clk, got)
		for i := 0; i < 20; i++ { // wall time keploy could not run
			clk.Pass()
			awaitDrainLook(t, clk, got)
		}
		clk.Advance(drainBudget - stall.Tick)
		awaitDrainLook(t, clk, got)
		clk.Advance(stall.Tick)
		if err := drainAnswers(t, got, "once its bound passed with the handler stuck"); err != nil {
			t.Fatalf("the drain: %v; want an answer, the stuck close owned", err)
		}
		owned := stuckLosses(t, p)
		if len(owned) != 1 {
			t.Fatalf("owned %+v, want the stuck close once", owned)
		}
		o := owned[0]
		if o.Session != 2 || o.Test != "" || o.Open || o.Loss.Count != 1 || !o.Loss.Kind.NotJudged() {
			t.Fatalf("owned %+v, want the session's (2, no test), not judged, once", o)
		}
		for _, w := range []string{"to 10.0.0.5:5432", "from " + pairedConnOf(t, p, 1).app.String(), "in session 2"} {
			if !strings.Contains(o.Loss.What, w) {
				t.Fatalf("what %q, want it to name %q", o.Loss.What, w)
			}
		}
		// A later drain does not wait for it, and owns nothing more.
		if err := drainAnswers(t, drainReturns(context.Background(), p), "with only a close a drain already owned"); err != nil {
			t.Fatalf("the later drain: %v", err)
		}
		if again := stuckLosses(t, p); len(again) != 0 {
			t.Fatalf("the later drain owned %+v again", again)
		}
	})

	t.Run("a handler that named no destination", func(t *testing.T) {
		p, app, served := stuckPair(t)
		_ = app.Close()
		awaitState(t, served.(*net.TCPConn), peerEnded, "the application's FIN")
		clk, got := drainAt(p)
		awaitDrainLook(t, clk, got)
		clk.Advance(drainBudget)
		if err := drainAnswers(t, got, "once its bound passed"); err != nil {
			t.Fatal(err)
		}
		owned := stuckLosses(t, p)
		if len(owned) != 1 || !strings.Contains(owned[0].Loss.What, "a destination its handler had not named yet") {
			t.Fatalf("owned %+v, want it to say no destination was named", owned)
		}
	})

	t.Run("a close begun starts the bound again", func(t *testing.T) {
		p, app, served := stuckPair(t)
		_ = app.Close()
		awaitState(t, served.(*net.TCPConn), peerEnded, "the application's FIN")
		clk, got := drainAt(p)
		awaitDrainLook(t, clk, got)
		clk.Advance(drainBudget - stall.Tick)
		awaitDrainLook(t, clk, got)
		// The handler begins the close (its count runs), and is not done.
		if _, _, err := p.paired.closing(served, func(*pairedConn) {}); err != nil {
			t.Fatalf("premise: the close's read: %v", err)
		}
		clk.Pass() // the drain looks, and sees the close begun
		awaitDrainLook(t, clk, got)
		clk.Advance(drainBudget - stall.Tick)
		awaitDrainLook(t, clk, got)
		p.paired.remove(served)
		if err := drainAnswers(t, got, "once the handler was done"); err != nil {
			t.Fatal(err)
		}
		if owned := stuckLosses(t, p); len(owned) != 0 {
			t.Fatalf("owned %+v, want none: the handler made progress and finished", owned)
		}
	})

	t.Run("two drains at once own it once", func(t *testing.T) {
		p, app, served := stuckPair(t)
		_ = app.Close()
		awaitState(t, served.(*net.TCPConn), peerEnded, "the application's FIN")
		clk, got := drainAt(p)
		got2 := make(chan error, 1)
		go func() { got2 <- p.PairedBytesDrain(context.Background()) }()
		for clk.Pending() < 2 {
			time.Sleep(time.Millisecond)
		}
		clk.Advance(drainBudget)
		var wg sync.WaitGroup
		for _, c := range []chan error{got, got2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case <-c:
				case <-time.After(10 * time.Second):
					t.Error("a drain did not answer once its bound passed")
				}
			}()
		}
		wg.Wait()
		if owned := stuckLosses(t, p); len(owned) != 1 {
			t.Fatalf("owned %+v, want the stuck close once", owned)
		}
		// However the two met: a give-up of a close already owned owns
		// nothing more.
		p.drainGiveUp([]*pairedConn{pairedConnOf(t, p, 1)})
		if owned := stuckLosses(t, p); len(owned) != 0 {
			t.Fatalf("owned %+v again", owned)
		}
	})
}

// What a drain counts as progress (drainMoved): a connection it waited for
// that is forgotten, or whose close began since its last look. One that is
// newly waited for, or still waited for as it was, is none.
func TestWhatADrainCountsAsProgress(t *testing.T) {
	a, b, c := &pairedConn{}, &pairedConn{}, &pairedConn{}
	begun := proxySide{}
	b.atClose.Store(&begun)
	for _, row := range []struct {
		name    string
		waited  map[*pairedConn]bool
		closing []*pairedConn
		moved   bool
	}{
		{"the first look", nil, []*pairedConn{a}, false},
		{"nothing moved", map[*pairedConn]bool{a: false, b: true}, []*pairedConn{a, b}, false},
		{"one forgotten", map[*pairedConn]bool{a: false, b: true}, []*pairedConn{a}, true},
		{"a close begun since", map[*pairedConn]bool{a: false, b: false}, []*pairedConn{a, b}, true},
		{"a new one waited for", map[*pairedConn]bool{a: false}, []*pairedConn{a, c}, false},
		{"a new one whose close has begun", map[*pairedConn]bool{a: false}, []*pairedConn{a, b}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := drainMoved(row.waited, row.closing); got != row.moved {
				t.Fatalf("drainMoved = %v, want %v", got, row.moved)
			}
		})
	}
}
