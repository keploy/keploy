package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// healthAgent is an agent's /agent/health that answers 200 once up has passed
// since the test began, 503 before, and records when each check came.
type healthAgent struct {
	begin time.Time
	up    time.Duration
	mu    sync.Mutex
	at    []time.Duration
}

func (h *healthAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/agent/health" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	since := time.Since(h.begin)
	h.mu.Lock()
	h.at = append(h.at, since)
	h.mu.Unlock()
	if since < h.up {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`"OK"`))
}

func (h *healthAgent) checks() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.at...)
}

// readyAfter starts the ticker at checkInterval against an agent up after up,
// and says how long after its start it found the agent ready, and when the
// agent saw each check.
func readyAfter(t *testing.T, up, checkInterval time.Duration) (time.Duration, []time.Duration) {
	t.Helper()
	resetProbeOnce(t)
	h := &healthAgent{up: up}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	readyCh := make(chan bool, 1)
	h.begin = time.Now()
	go AgentHealthTicker(ctx, zap.NewNop(), srv.URL+"/agent", readyCh, checkInterval)
	ready, open := <-readyCh
	took := time.Since(h.begin)
	require.True(t, open && ready, "the agent was never found ready")
	return took, h.checks()
}

// keploy's wait for the agent it started checks the agent's health once a
// second at most. An agent up at the wait's start, or a few tens of
// milliseconds into it, is found then, not at the first whole second: the
// first check comes 10 ms in, each pause after it twice the last one.
func TestAgentHealthTicker_FindsAnAgentUpSoonAfterItsStartSoon(t *testing.T) {
	for _, up := range []time.Duration{0, 150 * time.Millisecond} {
		took, at := readyAfter(t, up, time.Second)
		require.Less(t, took, up+700*time.Millisecond,
			"an agent up %v after the wait began was found ready only after %v (checks at %v): the first check waits a whole interval", up, took, at)
		require.GreaterOrEqual(t, took, up, "found ready before it was up")
	}
}

// The checks before the agent is up are no busy loop: each pause is twice the
// last one, so an agent up 300 ms in has been checked a handful of times,
// not every 10 ms.
func TestAgentHealthTicker_ChecksAnAgentStillStartingLessOftenEachTime(t *testing.T) {
	_, at := readyAfter(t, 300*time.Millisecond, time.Second)
	require.LessOrEqual(t, len(at), 8, "an agent up 300 ms in was checked %d times (%v): no longer pause after each check", len(at), at)
}

// The pauses grow to checkInterval and no further: an agent slow to start is
// checked about once every checkInterval, so one up 700 ms in, at a 50 ms
// interval, is found within about that interval of coming up.
func TestAgentHealthTicker_PausesAtMostCheckInterval(t *testing.T) {
	took, at := readyAfter(t, 700*time.Millisecond, 50*time.Millisecond)
	require.Less(t, took, 700*time.Millisecond+250*time.Millisecond,
		"an agent up 700 ms in, checked at most 50 ms apart, was found only after %v (checks at %v): the pauses grew past checkInterval", took, at)
}
