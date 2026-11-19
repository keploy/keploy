//go:build linux

package proxy

import (
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// writeUnread has the application write b, which the proxy never reads, and
// waits until the proxy's socket holds unread bytes of them, n in all.
func writeUnread(t *testing.T, app, served *net.TCPConn, b string, n int) {
	t.Helper()
	if _, err := app.Write([]byte(b)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		got, err := readUnread(served)
		if err == nil && got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise: the proxy's socket holds %d bytes unread (%v), want %d", got, err, n)
		}
	}
}

// ownedLossesOf is the owned losses of kind among owned.
func ownedLossesOf(owned []agent.OwnedLoss, kind models.CaptureLossKind) []agent.OwnedLoss {
	var out []agent.OwnedLoss
	for _, o := range owned {
		if o.Loss.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

// A connection the proxy accepted, and moved bytes on, in session 0; in
// session 1 the application writes bytes on it that the proxy never reads,
// and they are all lost at its close: the loss is session 1's, charged there,
// never stamped with session 0 (the last the proxy moved bytes in), which the
// agent would log and drop. Through the run of one test process (owned, worker
// 0's open test) and keploy test's counts (the test open now).
func TestBytesWrittenInALaterSessionAreThatSessionsLoss(t *testing.T) {
	t.Run("one test process", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		p.PairedBytesOneProcess(true)
		use(t, app, tracked)
		p.PairedBytesSession(1)
		writeUnread(t, app, proxyServed(t, p).(*net.TCPConn), "late", 4)
		if err := tracked.Close(); err != nil {
			t.Fatal(err)
		}
		got := ownedLossesOf(p.TakeOwnedLosses(), models.CaptureLossAppBytesLost)
		if len(got) != 1 || got[0].Session != 1 || !got[0].Open || got[0].Worker != 0 {
			t.Fatalf("owned %+v, want one app_bytes_lost of worker 0's open test, stamped with session 1", got)
		}
	})
	t.Run("keploy test's counts", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		p.PairedBytesSession(1)
		writeUnread(t, app, proxyServed(t, p).(*net.TCPConn), "late", 4)
		if err := tracked.Close(); err != nil {
			t.Fatal(err)
		}
		if n := lossesOf(p, models.CaptureLossAppBytesLost); n != 1 {
			t.Fatalf("counted %d app_bytes_lost, want 1: the test open now in session 1 is charged", n)
		}
		if got := ownedLossesOf(p.TakeOwnedLosses(), models.CaptureLossAppBytesLost); len(got) != 0 {
			t.Fatalf("owned %+v, want none: every byte lost was session 1's", got)
		}
	})
}

// keploy test begins a session at each test set; an application kept running
// into the next set keeps its connections. Bytes it wrote in set 1's session
// that the proxy never read, lost at a close counted in set 2's: the loss is
// set 1's, owned and stamped with its session (the agent logs it, once, and
// charges no run or test of set 2's with it), never in the counts that charge
// set 2's test open now. Bytes on both sides of set 2's mark are split there:
// set 2's share is counted, set 1's owned apart.
func TestAnEarlierSetsBytesLostInALaterSetAreNotItsCount(t *testing.T) {
	t.Run("every byte before the mark", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p).(*net.TCPConn)
		writeUnread(t, app, served, "tail", 4)
		p.PairedBytesSession(1)
		if err := tracked.Close(); err != nil {
			t.Fatal(err)
		}
		if n := lossesOf(p, models.CaptureLossAppBytesLost); n != 0 {
			t.Fatalf("counted %d app_bytes_lost, want 0: no byte lost was written in set 2's session", n)
		}
		got := ownedLossesOf(p.TakeOwnedLosses(), models.CaptureLossAppBytesLost)
		if len(got) != 1 || got[0].Session != 0 || !got[0].Open {
			t.Fatalf("owned %+v, want one app_bytes_lost stamped with set 1's session 0", got)
		}
	})
	t.Run("bytes on both sides of the mark", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p).(*net.TCPConn)
		writeUnread(t, app, served, "tail", 4)
		p.PairedBytesSession(1)
		writeUnread(t, app, served, "more", 8)
		if err := tracked.Close(); err != nil {
			t.Fatal(err)
		}
		if n := lossesOf(p, models.CaptureLossAppBytesLost); n != 1 {
			t.Fatalf("counted %d app_bytes_lost, want 1: set 2's share", n)
		}
		got := ownedLossesOf(p.TakeOwnedLosses(), models.CaptureLossAppBytesLost)
		if len(got) != 1 || got[0].Session != 0 || !got[0].Open || got[0].Test != "" {
			t.Fatalf("owned %+v, want set 1's share, a close (open, naming no test) stamped with session 0", got)
		}
	})
}
