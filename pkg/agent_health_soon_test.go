package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// slow is how long each answer before up takes.
	slow time.Duration
	mu   sync.Mutex
	at   []time.Duration
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
		time.Sleep(h.slow)
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

// startTicker starts keploy's agent health check as its callers do.
func startTicker(ctx context.Context, agentURI string, readyCh chan<- bool) {
	go AgentHealthTicker(ctx, zap.NewNop(), agentURI, readyCh)
}

// readyAfter starts the health check against an agent up after up, and says
// how long after its start it found the agent ready, and when the agent saw
// each check.
func readyAfter(t *testing.T, up time.Duration) (time.Duration, []time.Duration) {
	t.Helper()
	return readyAfterFrom(t, &healthAgent{up: up})
}

func readyAfterFrom(t *testing.T, h *healthAgent) (time.Duration, []time.Duration) {
	t.Helper()
	resetProbeOnce(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	readyCh := make(chan bool, 1)
	h.begin = time.Now()
	startTicker(ctx, srv.URL+"/agent", readyCh)
	ready, open := <-readyCh
	took := time.Since(h.begin)
	require.True(t, open && ready, "the agent was never found ready")
	return took, h.checks()
}

// keploy's wait for the agent it started finds it ready within about one
// AgentHealthEvery (50 ms) of its coming up, however long it took to start: not at
// the next whole second (a 1 s ticker found an agent up 1.4 s in at 2 s), nor
// at the next check of a schedule whose pauses double up to a second (one up
// 320 ms in at 630 ms, one up 1.4 s in at 2.27 s).
func TestAgentHealthTicker_FindsAnAgentWithinACheckOfItsComingUp(t *testing.T) {
	for _, up := range []time.Duration{0, 300 * time.Millisecond, 320 * time.Millisecond, 1400 * time.Millisecond} {
		took, at := readyAfter(t, up)
		require.GreaterOrEqual(t, took, up, "found ready before it was up")
		require.Less(t, took, up+150*time.Millisecond,
			"an agent up %v after the wait began was found ready only after %v (checks at %v), want within about %v of its coming up", up, took, at, AgentHealthEvery)
	}
}

// The first check comes about 10 ms in, before the first whole pause: an
// agent up at once is found then.
func TestAgentHealthTicker_FirstLooksSoon(t *testing.T) {
	_, at := readyAfter(t, 200*time.Millisecond)
	require.NotEmpty(t, at)
	require.Less(t, at[0], 40*time.Millisecond, "the first check came %v in (checks at %v), want about 10 ms", at[0], at)
}

// The checks before the agent is up are no busy loop, and no backoff: they
// come about 50 ms apart (AgentHealthEvery), however long the agent takes to
// start. Each check starts at least 50 ms after the start of the one before
// (one that took longer is followed at once, so two can reach the agent
// closer than that), the first 10 ms in: an agent up 1.4 s in is checked at
// most 28 times before it is up, plus the check that finds it (the row
// allows 30). The bounds are times and counts, not multiples of AgentHealthEvery, so a pace outside
// about 50-75 ms fails this row.
func TestAgentHealthTicker_ChecksAStartingAgentAtAFixedPace(t *testing.T) {
	_, at := readyAfter(t, 1400*time.Millisecond)
	gaps := make([]time.Duration, 0, len(at))
	for i := 1; i < len(at); i++ {
		gaps = append(gaps, at[i]-at[i-1])
	}
	require.GreaterOrEqual(t, len(gaps), 10, "an agent up 1.4 s in was checked %d times (at %v), want about one check each %v", len(at), at, AgentHealthEvery)
	require.LessOrEqual(t, len(at), 30, "an agent up 1.4 s in was checked %d times (at %v), want 30 at most: checks that start 50 ms apart at least, the first 10 ms in", len(at), at)
	sorted := slices.Clone(gaps)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	require.GreaterOrEqual(t, median, 45*time.Millisecond, "the checks came %v apart (gaps %v), want about 50 ms", median, gaps)
	require.Less(t, median, 75*time.Millisecond, "the checks came %v apart (gaps %v), want about 50 ms: the pauses grow", median, gaps)
}

// The pause is timed from the start of the check before: checks that each
// take 30 ms still come about 50 ms apart (AgentHealthEvery), not 80 ms.
func TestAgentHealthTicker_PacesFromTheStartOfEachCheck(t *testing.T) {
	_, at := readyAfterFrom(t, &healthAgent{up: time.Second, slow: 30 * time.Millisecond})
	gaps := make([]time.Duration, 0, len(at))
	for i := 1; i < len(at); i++ {
		gaps = append(gaps, at[i]-at[i-1])
	}
	require.GreaterOrEqual(t, len(gaps), 8, "an agent up 1 s in was checked %d times (at %v)", len(at), at)
	sorted := slices.Clone(gaps)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	require.Less(t, median, 70*time.Millisecond, "checks taking 30 ms each came %v apart (gaps %v), want about 50 ms: the pause follows the check's end", median, gaps)
}
