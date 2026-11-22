package proxy

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// TestRecordViaSupervisorLeavesOutWhatAHoleCosts pins the one rule on the proxy
// path: after a capture hole, the test cases recorded while the connection
// carries traffic are left out, and once it goes idle they no longer are. That
// holds for a parser that re-aligns too: when it has re-aligned is known only
// once it gets there, a full queue behind the traffic, by when the test cases
// in between have been streamed.
func TestRecordViaSupervisorLeavesOutWhatAHoleCosts(t *testing.T) {
	t.Parallel()

	// oversized blows through the 4-byte cap and is dropped: a hole. followUp
	// fits.
	oversized := []byte("0123456789")
	followUp := []byte("ok")

	for _, tc := range []struct {
		name   string
		parser func(*feedProbe) integrations.Integrations
	}{
		{"a parser that cannot re-align", func(p *feedProbe) integrations.Integrations { return p }},
		{"a parser that re-aligns", func(p *feedProbe) integrations.Integrations { return resyncFeedProbe{p} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr := syncMock.New(nil)
			probe := newFeedProbe()
			h := newDesyncFeedHarnessCtx(t, syncMock.NewContext(context.Background(), mgr), tc.parser(probe), probe, 4)

			h.writeApp(t, oversized)
			waitForDest(t, h, string(oversized))
			// Still carrying traffic: a test case recorded now is left out.
			deadline := time.Now().Add(2 * time.Second)
			for {
				h.writeApp(t, followUp)
				now := time.Now()
				if ok, _ := mgr.WasMockOrphanedInWindow(now.Add(-time.Millisecond), now); ok {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no span covers the traffic after the hole: its test cases are saved without their mocks")
				}
				time.Sleep(5 * time.Millisecond)
			}
			// It stays covered for as long as the connection carries
			// traffic, past the idle grace that closes the span of a quiet
			// one: the relay notes each byte it forwards.
			for until := time.Now().Add(syncMock.UnrecordedIdleGrace + 500*time.Millisecond); time.Now().Before(until); {
				h.writeApp(t, followUp)
				time.Sleep(20 * time.Millisecond)
			}
			busy := time.Now()
			if ok, _ := mgr.WasMockOrphanedInWindow(busy.Add(-time.Millisecond), busy); !ok {
				t.Fatal("the span closed while the connection still carried traffic: test cases after it are saved without their mocks")
			}
			// Idle: the span closes, and later test cases are kept.
			waitUntilT(t, 5*time.Second, "the span closes once the connection is idle", func() bool {
				_, _, open := mgr.OrphanRangeCount()
				return open == 0
			})
			time.Sleep(20 * time.Millisecond)
			later := time.Now()
			if ok, _ := mgr.WasMockOrphanedInWindow(later, later); ok {
				t.Fatal("a test case after the connection went idle was left out")
			}
		})
	}
}

func waitForDest(t *testing.T, h *desyncFeedHarness, want string) {
	t.Helper()
	waitUntilT(t, 2*time.Second, "the destination receives "+want, func() bool {
		return len(h.destSeen()) >= len(want)
	})
}

func waitUntilT(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("not within %v: %s", timeout, what)
}
