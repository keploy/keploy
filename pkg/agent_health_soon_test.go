package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeClock is an agentHealthClock whose time moves only when the test moves
// it. agentHealthTicker hands each pause it takes to the test (sleeps), which
// moves the time on by it (advance) and wakes the loop; an agent's answer that
// takes a while moves it on as the answer is given. Which check comes when is
// then the same on a machine however busy, and a check runs out of time only
// when the time on this clock passes its deadline.
type fakeClock struct {
	sleeps chan fakeSleep
	// spun is closed when the loop pauses a second time after its ctx ended:
	// it went on checking.
	spun  chan struct{}
	ended atomic.Int32

	mu       sync.Mutex
	now      time.Time
	timeouts map[*fakeTimeout]struct{}
}

// fakeSleep is one pause of the loop: how long it asked for, and the channel
// the test closes to end it.
type fakeSleep struct {
	d    time.Duration
	wake chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		sleeps:   make(chan fakeSleep),
		spun:     make(chan struct{}),
		now:      time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		timeouts: map[*fakeTimeout]struct{}{},
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() == nil {
		s := fakeSleep{d: d, wake: make(chan struct{})}
		select {
		case c.sleeps <- s:
			select {
			case <-s.wake:
				return true
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}
	}
	if c.ended.Add(1) == 2 {
		close(c.spun)
	}
	return false
}

func (c *fakeClock) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	inner, cancel := context.WithCancel(ctx)
	to := &fakeTimeout{Context: inner, cancel: cancel}
	c.mu.Lock()
	to.deadline = c.now.Add(d)
	c.timeouts[to] = struct{}{}
	c.mu.Unlock()
	c.advance(0)
	return to, func() {
		c.mu.Lock()
		delete(c.timeouts, to)
		c.mu.Unlock()
		cancel()
	}
}

// advance moves the time on by d (not at all when d is not positive), ends
// each check whose deadline it reaches, and says how many it ended.
func (c *fakeClock) advance(d time.Duration) int {
	c.mu.Lock()
	if d > 0 {
		c.now = c.now.Add(d)
	}
	var due []*fakeTimeout
	for to := range c.timeouts {
		if !c.now.Before(to.deadline) {
			due = append(due, to)
			delete(c.timeouts, to)
		}
	}
	c.mu.Unlock()
	for _, to := range due {
		to.expire()
	}
	return len(due)
}

// underWay is the ctx of each check under way: made by WithTimeout, and
// neither cancelled by the loop nor run out of time on the clock yet.
func (c *fakeClock) underWay() []*fakeTimeout {
	c.mu.Lock()
	defer c.mu.Unlock()
	var checks []*fakeTimeout
	for to := range c.timeouts {
		checks = append(checks, to)
	}
	return checks
}

// fakeTimeout is the context fakeClock.WithTimeout gives: it ends, with
// context.DeadlineExceeded, when the clock reaches its deadline.
type fakeTimeout struct {
	context.Context
	deadline time.Time
	cancel   context.CancelFunc
	expired  atomic.Bool
}

func (to *fakeTimeout) expire() {
	if to.Context.Err() == nil {
		to.expired.Store(true)
	}
	to.cancel()
}

func (to *fakeTimeout) Err() error {
	err := to.Context.Err()
	if err != nil && to.expired.Load() {
		return context.DeadlineExceeded
	}
	return err
}

// healthAgent is an agent's /agent/health that answers 200 once up has passed
// on clock since begin, 503 before, and records when each check came.
type healthAgent struct {
	clock *fakeClock
	begin time.Time
	up    time.Duration
	// slow is how long each answer to a check that comes slowFrom or later
	// takes to be given, on clock; answers before come at once. A check that
	// runs out of time on the way gets none, as through a slow path.
	slow     time.Duration
	slowFrom time.Duration
	// held, when set, is told of each check, which then gets no answer at
	// all: the agent holds it until the checker gives up.
	held chan struct{}
	// done is closed when the test ends (startOn).
	done chan struct{}
	mu   sync.Mutex
	at   []time.Duration
}

func (h *healthAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/agent/health" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	since := h.clock.Now().Sub(h.begin)
	h.mu.Lock()
	h.at = append(h.at, since)
	h.mu.Unlock()
	if h.held != nil {
		select {
		case h.held <- struct{}{}:
		default:
		}
		h.unanswered(w, r)
		return
	}
	slow := time.Duration(0)
	if since >= h.slowFrom {
		slow = h.slow
	}
	if h.clock.advance(slow) > 0 {
		// The checker gave up; it closes the connection.
		h.unanswered(w, r)
		return
	}
	if since < h.up {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`"OK"`))
}

// unanswered gives a check no answer until the checker closes the connection.
// One that never does gets a 503 when the test ends, so that the test's
// server can close and the test's failure is said.
func (h *healthAgent) unanswered(w http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-h.done:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func (h *healthAgent) checks() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.at...)
}

// tickerRun is keploy's agent health check running on a fakeClock against
// a healthAgent.
type tickerRun struct {
	clock  *fakeClock
	ready  chan bool
	cancel context.CancelFunc
}

// startOn starts keploy's agent health check against h as AgentHealthTicker
// does, on a fakeClock of its own.
func startOn(t *testing.T, h *healthAgent) *tickerRun {
	t.Helper()
	resetProbeOnce(t)
	clock := newFakeClock()
	h.clock, h.begin, h.done = clock, clock.Now(), make(chan struct{})
	srv := httptest.NewServer(h)
	// Cleanups run last first: the check's ctx ends, then a check left
	// held (unanswered) is let go, then the server closes.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(h.done) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan bool, 1)
	go func() {
		defer close(ready)
		agentHealthTicker(ctx, zap.NewNop(), clock, srv.URL+"/agent", func(http.Header) bool {
			select {
			case ready <- true:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	return &tickerRun{clock: clock, ready: ready, cancel: cancel}
}

// stuck is how long, on the time of day, a row waits for the loop's next
// pause or its return before it fails: a check that never returned (one whose
// request outlives its ctx) or a loop that hangs. It can only fail a row,
// never pass one, and no row's answer depends on it: the times the loop's
// rows assert are all on the fake clock.
const stuck = 2 * time.Minute

// next waits for the loop's next pause, and says it (paused true), or for the
// loop to return, and says whether it found the agent ready. A loop that does
// neither for stuck fails the test.
func (run *tickerRun) next(t *testing.T, h *healthAgent) (s fakeSleep, paused, ready bool) {
	t.Helper()
	select {
	case s = <-run.clock.sleeps:
		return s, true, false
	case found, open := <-run.ready:
		return fakeSleep{}, false, open && found
	case <-time.After(stuck):
		t.Fatalf("a check never returned: the loop neither paused nor returned for %v (checks at %v)", stuck, h.checks())
		return fakeSleep{}, false, false
	}
}

// nextPause is the loop's next pause; a loop that returned instead fails the
// test.
func (run *tickerRun) nextPause(t *testing.T, h *healthAgent) fakeSleep {
	t.Helper()
	s, paused, ready := run.next(t, h)
	if !paused {
		t.Fatalf("the loop returned (found the agent ready: %v) where it should have paused for the next check (checks at %v)", ready, h.checks())
	}
	return s
}

// readyOn runs keploy's agent health check against h, moving its clock on by
// each pause it takes, and says how long after its start it found the agent
// ready, and when the agent saw each check, all on that clock. A check that
// has not found the agent a minute in, or within 1200 pauses (one each 50 ms
// for a minute), fails the test, as does a check that never returns (stuck).
func readyOn(t *testing.T, h *healthAgent) (time.Duration, []time.Duration) {
	t.Helper()
	run := startOn(t, h)
	for pauses := 1; ; pauses++ {
		s, paused, ready := run.next(t, h)
		if !paused {
			require.True(t, ready, "the agent was never found ready (checks at %v)", h.checks())
			return run.clock.Now().Sub(h.begin), h.checks()
		}
		if run.clock.Now().Add(s.d).Sub(h.begin) > time.Minute || pauses > 1200 {
			t.Fatalf("the agent, up %v in, was not found ready a minute or 1200 pauses in (%d checks, the first at %v)", h.up, len(h.checks()), h.checks()[:min(len(h.checks()), 5)])
		}
		run.clock.advance(s.d)
		close(s.wake)
	}
}

// paced is when checks first in and every apart come, up to and including
// the first at or after up.
func paced(first, every, up time.Duration) []time.Duration {
	at := []time.Duration{first}
	for at[len(at)-1] < up {
		at = append(at, at[len(at)-1]+every)
	}
	return at
}

// keploy's wait for the agent it started finds it ready within one
// AgentHealthEvery (50 ms) of its coming up, however long it took to start: not at
// the next whole second (a 1 s ticker found an agent up 1.4 s in at 2 s), nor
// at the next check of a schedule whose pauses double up to a second (one up
// 320 ms in at 630 ms, one up 1.4 s in at 2.27 s).
func TestAgentHealthTicker_FindsAnAgentWithinACheckOfItsComingUp(t *testing.T) {
	for _, up := range []time.Duration{0, 300 * time.Millisecond, 320 * time.Millisecond, 1400 * time.Millisecond} {
		took, at := readyOn(t, &healthAgent{up: up})
		require.GreaterOrEqual(t, took, up, "found ready before it was up")
		require.Less(t, took, up+50*time.Millisecond,
			"an agent up %v after the wait began was found ready only after %v (checks at %v), want within %v of its coming up", up, took, at, AgentHealthEvery)
	}
}

// The first check comes 10 ms in, before the first whole pause: an agent up
// at once is found then.
func TestAgentHealthTicker_FirstLooksSoon(t *testing.T) {
	took, at := readyOn(t, &healthAgent{up: 0})
	require.Equal(t, []time.Duration{10 * time.Millisecond}, at, "the first check came at %v, want 10 ms in", at)
	require.Equal(t, 10*time.Millisecond, took)
}

// The checks before the agent is up are no busy loop, and no backoff: they
// come 50 ms apart (AgentHealthEvery), however long the agent takes to start,
// the first 10 ms in: an agent up 1.4 s in is checked 28 times before it is
// up, plus the check that finds it. The times are written out, not multiples
// of AgentHealthEvery, so any other pace fails this row.
func TestAgentHealthTicker_ChecksAStartingAgentAtAFixedPace(t *testing.T) {
	_, at := readyOn(t, &healthAgent{up: 1400 * time.Millisecond})
	want := paced(10*time.Millisecond, 50*time.Millisecond, 1400*time.Millisecond)
	require.Len(t, want, 29)
	require.Equal(t, want, at, "an agent up 1.4 s in was checked at %v, want each 50 ms from 10 ms in", at)
}

// The pause is timed from the start of the check before: checks that each
// take 30 ms still come 50 ms apart (AgentHealthEvery), not 80 ms.
func TestAgentHealthTicker_PacesFromTheStartOfEachCheck(t *testing.T) {
	took, at := readyOn(t, &healthAgent{up: time.Second, slow: 30 * time.Millisecond})
	require.Equal(t, paced(10*time.Millisecond, 50*time.Millisecond, time.Second), at,
		"checks taking 30 ms each came at %v, want each 50 ms from 10 ms in: the pause follows the check's start", at)
	require.Equal(t, 1040*time.Millisecond, took, "the check at 1010 ms takes 30 ms to find the agent")
}

// A check that runs out of time (agentHealthFirstWait, 500 ms) is followed at
// once by one that waits twice as long: an agent behind a path whose answers
// each take 700 ms (a port-forward over a 245 ms link takes about 735 ms) is
// found by the second check, not cut off at every one.
func TestAgentHealthTicker_WaitsLongerAfterACheckRunsOutOfTime(t *testing.T) {
	took, at := readyOn(t, &healthAgent{up: 0, slow: 700 * time.Millisecond})
	require.Equal(t, []time.Duration{10 * time.Millisecond, 710 * time.Millisecond}, at,
		"answers that take 700 ms were asked for at %v, want at 10 ms and at once after the first ran out of time", at)
	require.Equal(t, 1410*time.Millisecond, took)
}

// Only a check that runs out of time makes the next one wait longer: an
// answer that says the agent is not up yet (503) says nothing about how long
// the path takes. Three of them, at 10, 60 and 110 ms, leave the wait at
// agentHealthFirstWait (500 ms), so the first answer that takes 700 ms (the
// check at 160 ms) is cut off, and the check after it, given 1 s, finds the
// agent.
func TestAgentHealthTicker_WaitsLongerOnlyAfterACheckRunsOutOfTime(t *testing.T) {
	took, at := readyOn(t, &healthAgent{up: 150 * time.Millisecond, slowFrom: 150 * time.Millisecond, slow: 700 * time.Millisecond})
	require.Equal(t, []time.Duration{10 * time.Millisecond, 60 * time.Millisecond, 110 * time.Millisecond, 160 * time.Millisecond, 860 * time.Millisecond}, at,
		"three 503s, then answers that take 700 ms, were asked for at %v, want the check at 160 ms cut off at 500 ms and one more", at)
	require.Equal(t, 1560*time.Millisecond, took)
}

// The check ends when its ctx does: it makes no check after, says the agent
// was never found ready (closes its channel), and returns.
func TestAgentHealthTicker_StopsWhenItsContextEnds(t *testing.T) {
	h := &healthAgent{up: time.Hour}
	run := startOn(t, h)
	for range 3 {
		s := run.nextPause(t, h)
		run.clock.advance(s.d)
		close(s.wake)
	}
	// The pause after the third check.
	run.nextPause(t, h)
	run.cancel()
	select {
	case ready, open := <-run.ready:
		require.False(t, open || ready, "the agent was found ready after the check's ctx ended")
	case <-run.clock.spun:
		t.Fatalf("the check went on after its ctx ended (checks at %v)", h.checks())
	case <-time.After(stuck):
		t.Fatalf("the check neither returned nor paused for %v after its ctx ended (checks at %v)", stuck, h.checks())
	}
	require.Equal(t, []time.Duration{10 * time.Millisecond, 60 * time.Millisecond, 110 * time.Millisecond}, h.checks())
}

// A check under way ends when the loop's ctx does, not when it runs out of
// time: an agent that holds a check without answering does not keep a caller
// that gave up waiting out the check's wait (agentHealthFirstWait, and up to
// agentHealthMaxWait, 30 s, after slow answers). The clock does not move
// after the check begins, so its own deadline is never reached here.
func TestAgentHealthTicker_EndsACheckUnderWayWhenItsContextEnds(t *testing.T) {
	h := &healthAgent{up: 0, held: make(chan struct{}, 1)}
	run := startOn(t, h)
	s := run.nextPause(t, h)
	run.clock.advance(s.d)
	close(s.wake)
	select {
	case <-h.held:
	case <-time.After(stuck):
		t.Fatalf("the first check never reached the agent in %v", stuck)
	}
	checks := run.clock.underWay()
	require.Len(t, checks, 1, "one check is under way")
	run.cancel()
	require.Error(t, checks[0].Err(), "the check under way did not end when the loop's ctx did")
	_, paused, ready := run.next(t, h)
	require.False(t, paused, "the loop paused for another check after its ctx ended")
	require.False(t, ready, "the agent was found ready after the loop's ctx ended")
	require.Equal(t, []time.Duration{10 * time.Millisecond}, h.checks())
	require.Equal(t, 10*time.Millisecond, run.clock.Now().Sub(h.begin), "the clock moved while the check was under way")
}

// keploy's own agentHealthClock is the time of day: Now is time.Now, a Sleep
// lasts d (no time at all for a d of zero or less), or ends when its ctx does
// and says so, and WithTimeout's ctx runs out of time d after it was made.
func TestWallClock_KeepsTheTimeOfDay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var clock agentHealthClock = wallClock{}
		require.True(t, clock.Now().Equal(time.Now()), "Now is %v at %v", clock.Now(), time.Now())

		began := time.Now()
		require.True(t, clock.Sleep(t.Context(), 20*time.Millisecond), "a sleep whose ctx never ended said it did")
		require.Equal(t, 20*time.Millisecond, time.Since(began), "a sleep of 20 ms")
		for _, d := range []time.Duration{0, -time.Second} {
			began = time.Now()
			require.True(t, clock.Sleep(t.Context(), d), "a sleep of %v whose ctx never ended said it did", d)
			require.Zero(t, time.Since(began), "a sleep of %v waited", d)
		}

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		began = time.Now()
		require.False(t, clock.Sleep(ctx, time.Hour), "a sleep whose ctx ended 10 ms in said it lasted")
		require.Equal(t, 10*time.Millisecond, time.Since(began), "a sleep whose ctx ended 10 ms in")
		require.False(t, clock.Sleep(ctx, time.Hour), "a sleep whose ctx had ended said it lasted")

		timed, stop := clock.WithTimeout(t.Context(), 500*time.Millisecond)
		defer stop()
		began = time.Now()
		select {
		case <-timed.Done():
		case <-time.After(time.Hour):
		}
		require.Equal(t, 500*time.Millisecond, time.Since(began), "a ctx with a timeout of 500 ms")
		require.ErrorIs(t, timed.Err(), context.DeadlineExceeded)
	})
}
