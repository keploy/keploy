package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// roundPost is a POST /orders the app sends deps.example.
func roundPost(body string) string {
	return "POST /orders HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// roundOrders stages, under exact serving, the next test's two recordings of
// POST /orders, mock-a ({"item":"apple"}) and mock-b ({"item":"pear"}), at
// the recorded times given (an offset from now), and serves the gap before
// that test with a pool view whose ahead (the test's calls in flight when it
// began) is ahead. The proxy holds the manager, as the agent's does, so the
// learn tracker's boundaries reach it.
func roundOrders(t *testing.T, atA, atB time.Duration, ahead ...string) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	a := r3Mock("mock-a", now.Add(atA), "POST", "/orders", `{"item":"apple"}`, `{"order":1}`)
	b := r3Mock("mock-b", now.Add(atB), "POST", "/orders", `{"item":"pear"}`, `{"order":2}`)
	for _, m := range []*models.Mock{a, b} {
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SetMocksWithWindow([]*models.Mock{a, b}, nil, models.BaseTime, now.Add(time.Hour))
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	// test-1 runs (it owns nothing here) and ends: the pool view between it
	// and test-2 answers test-2's calls in flight.
	p.ContinueTestErrorCapture()
	mm.ServeExactTest("test-1", nil)
	var inFlight []*models.Mock
	for _, n := range ahead {
		inFlight = append(inFlight, roundMock(mm, n))
	}
	mm.ServeExactPool(models.PoolKeyNone, nil, inFlight...)
	if _, err := p.GetMockErrors(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p, mm
}

// An ahead answer carried into its test is the test's own, answered: a later
// call that is that recording again is one call more than the test recorded,
// and its miss names it. It is never paired with the test's other recording
// of the endpoint (mock-b), whose item would be learned as noise when the
// carried recording was made after it (its values are then no earlier place
// of mock-b's): a false green. The learn step read the carried answer in
// neither the view, which hides it, nor the tracker, which filed its serve
// under the last test. Made before mock-b, the call was refused, but as
// mock-b's changed request, a miss that names the wrong recording.
func TestALearnStepCountsAnAheadAnswerCarriedIntoItsTestAsSpent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		atA, atB time.Duration
		ahead    []string
	}{
		{"recorded after the test's other call", 200 * time.Millisecond, 100 * time.Millisecond, []string{"mock-b", "mock-a"}},
		{"recorded before the test's other call", 100 * time.Millisecond, 200 * time.Millisecond, []string{"mock-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mm := roundOrders(t, tc.atA, tc.atB, tc.ahead...)
			opts := models.OutgoingOptions{Learn: true}
			// The call in flight, answered in the gap by the pool view's ahead.
			if code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"apple"}`)); code != http.StatusOK || got != `{"order":1}` {
				t.Fatalf("precondition: the call in flight got %d %s", code, got)
			}
			// test-2 opens: mock-a is carried into it, answered.
			p.ContinueTestErrorCapture()
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-a"), roundMock(mm, "mock-b")})
			if _, consumed, _, _ := mm.ExactTestOutcome(); len(consumed) != 1 || consumed[0] != "mock-a" {
				t.Fatalf("precondition: test-2 opened with %v answered", consumed)
			}
			// The app sends the call in flight again.
			code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"apple"}`))
			st := r3States(mm)
			if code == http.StatusOK || len(st["mock-b"].ReqBodyNoise) > 0 {
				t.Fatalf("the call mock-a answered already was paired with mock-b: answered %d %s, mock-b learned %v", code, got, st["mock-b"].ReqBodyNoise)
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(misses) != 1 || misses[0].ClosestMock != "mock-a" || !strings.Contains(misses[0].NextSteps, "one call more") {
				t.Fatalf("the miss does not name mock-a as the call answered already: %+v", misses)
			}
			// test-2's own call of mock-b is still answered from it.
			p.ContinueTestErrorCapture()
			if code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"pear"}`)); code != http.StatusOK || got != `{"order":2}` {
				t.Fatalf("test-2's own call got %d %s", code, got)
			}
		})
	}
}

// At a test's end the store moves to the pool view before the learn tracker
// ends the test (the replay serves the pool, then reads the test's misses).
// A call in that gap is served by the pool view, whose ahead is the next
// test's calls in flight: no recording is the ended test's own, and none is
// paired as one, whatever the tracker still says. Read off the tracker, the
// next test's mock-a was paired as test-1's own, its item learned as noise
// in test-1's scope: a false green. In the gap the call is read as between
// tests, where it is the same miss.
func TestALearnStepPairsNothingAsTheEndedTestsOwnInTheGapAtItsEnd(t *testing.T) {
	for _, gap := range []string{"the store moved, the tracker not yet", "both moved"} {
		t.Run(gap, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			now := time.Now()
			a := r3Mock("mock-a", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":1}`)
			a.TestModeInfo.Lifetime, a.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
			mm := NewMockManager(nil, nil, zap.NewNop())
			mm.SetMocksWithWindow([]*models.Mock{a}, nil, models.BaseTime, now.Add(time.Hour))
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			p.ContinueTestErrorCapture()
			mm.ServeExactTest("test-1", nil)
			mm.ServeExactPool(models.PoolKeyNone, nil, roundMock(mm, "mock-a"))
			if gap == "both moved" {
				if _, err := p.GetMockErrors(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, roundPost(`{"item":"apricot"}`))
			st := r3States(mm)
			if code == http.StatusOK || len(st["mock-a"].ReqBodyNoise) > 0 || len(p.learnTracker.AnsweredMocks()) > 0 {
				t.Fatalf("a call in the gap was paired as test-1's own with test-2's mock-a: answered %d %s, mock-a learned %v, test-1 answered %v",
					code, got, st["mock-a"].ReqBodyNoise, exactNames(p.learnTracker.AnsweredMocks()))
			}
		})
	}
}

// An ahead answer the round voided (the test it was answered for did not
// open next) answered a call of the round: when that test opens later, a
// call that is the recording again is one call more than recorded, named,
// never answered from it a second time.
func TestALearnStepCountsAVoidedAheadAnswerAsSpent(t *testing.T) {
	p, mm := roundOrders(t, 100*time.Millisecond, 200*time.Millisecond, "mock-a")
	opts := models.OutgoingOptions{Learn: true}
	if code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"apple"}`)); code != http.StatusOK || got != `{"order":1}` {
		t.Fatalf("precondition: the call in flight got %d %s", code, got)
	}
	// The test mock-a was answered for does not open next.
	mm.ServeExactPool(models.PoolKeyNone, nil)
	p.ContinueTestErrorCapture()
	mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-a"), roundMock(mm, "mock-b")})
	if _, consumed, _, _ := mm.ExactTestOutcome(); len(consumed) != 0 {
		t.Fatalf("precondition: a voided answer is no consume of test-2: %v", consumed)
	}
	code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"apple"}`))
	if code == http.StatusOK {
		t.Fatalf("mock-a answered a second call of the round: %d %s", code, got)
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 1 || misses[0].ClosestMock != "mock-a" || !strings.Contains(misses[0].NextSteps, "one call more") {
		t.Fatalf("the miss does not name mock-a as the call answered already: %+v", misses)
	}
	p.ContinueTestErrorCapture()
	if code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"pear"}`)); code != http.StatusOK || got != `{"order":2}` {
		t.Fatalf("test-2's own call got %d %s", code, got)
	}
}

// A claim the learn step decided on one read of the round lands only in that
// round. Under S12's background rules the pool is learned onto between tests
// only: a call read at the start-up, paired with the pool's mock-pool, whose
// test opens before its claim lands, is looked at again in the test, where
// the pool answers exactly or not at all. Claimed against the store as it
// stood at the claim, the call was answered from mock-pool with its item
// learned as noise, in the test.
func TestALearnStepsClaimLandsOnlyInTheRoundItWasReadIn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
	own := r3Mock("mock-own", now.Add(1500*time.Millisecond), "GET", "/status", "", `{"up":true}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now.Add(time.Second), End: now.Add(2 * time.Second)}})
	mm.SetMocksWithWindow([]*models.Mock{pool, own}, nil, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-pool"}, nil)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	// The first take the call makes after its read: test-1 opens first.
	// (Every take of the call runs on its connection's goroutine: the
	// opening's own takes find it done.)
	r := &mm.exact.round
	opened := false
	r.onWait = func() {
		if opened {
			return
		}
		opened = true
		p.ContinueTestErrorCapture()
		mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own")})
	}
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"apricot"}`))
	r.onWait = nil
	if !opened {
		t.Fatalf("precondition: the call took nothing (answered %d %s)", code, got)
	}
	st := r3States(mm)
	if code == http.StatusOK || len(st["mock-pool"].ReqBodyNoise) > 0 {
		t.Fatalf("a claim read at the start-up landed in test-1: answered %d %s, mock-pool learned %v", code, got, st["mock-pool"].ReqBodyNoise)
	}
}

// s12Orders stages, under exact serving with S12's background rules, the
// pool's recording of POST /orders (mock-pool, {"item":"apple"}), recorded
// outside every test, and each test's own recording of GET /status, inside
// its recorded window: mock-own1 test-1's, mock-own2 test-2's. The pool view
// serves the start-up.
func s12Orders(t *testing.T) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
	own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "GET", "/status", "", `{"up":1}`)
	own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "GET", "/status", "", `{"up":2}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: now.Add(500 * time.Millisecond), End: now.Add(800 * time.Millisecond)},
		{Start: now.Add(time.Second), End: now.Add(2 * time.Second)},
	})
	mm.SetMocksWithWindow([]*models.Mock{pool, own1, own2}, nil, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-pool"}, nil)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	return p, mm
}

// While the store shows a test's view, S12's background answers a call
// exactly or not at all, and no drift is learned onto it, whether the learn
// tracker has that test open or not: the pool is learned onto between tests
// only. Two schedules the replay makes put the tracker out of step with the
// store's test view: keploy mock's scope flow opens a test's view before the
// tracker begins it, and a test whose misses' read did not complete never
// ended on the tracker. Read as between tests there, a drifted call of the
// test was answered from the pool, its item learned onto mock-pool as
// noise: a false green. Between tests the same call is learned onto the
// pool, as S12's rules have it.
func TestALearnStepLearnsNothingOntoThePoolWhileTheStoreShowsATest(t *testing.T) {
	learnPool := models.OutgoingOptions{Learn: true, LearnPool: true}
	for _, tc := range []struct {
		name    string
		at      func(p *Proxy, mm *MockManager)
		learned bool
	}{
		{"test-2's view, after a test whose end never reached the tracker, begun", func(p *Proxy, mm *MockManager) {
			// test-1, the scope flow: the view, then the tracker's begin.
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			// test-1 ends on the store; its misses' read never completes.
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
			p.ContinueTestErrorCapture()
		}, false},
		{"test-2's view, after a test whose end never reached the tracker, not begun yet", func(p *Proxy, mm *MockManager) {
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
		}, false},
		{"test-2's view before the tracker begins it", func(p *Proxy, mm *MockManager) {
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			if _, err := p.GetMockErrors(context.Background()); err != nil {
				t.Fatal(err)
			}
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
		}, false},
		{"between tests", func(p *Proxy, mm *MockManager) {
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			if _, err := p.GetMockErrors(context.Background()); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mm := s12Orders(t)
			tc.at(p, mm)
			code, got := r3Send(t, p, mm, learnPool, roundPost(`{"item":"apricot"}`))
			learned := len(r3States(mm)["mock-pool"].ReqBodyNoise) > 0
			if tc.learned {
				if code != http.StatusOK || got != `{"order":0}` || !learned {
					t.Fatalf("between tests the pool's drift was not learned: answered %d %s, learned %v", code, got, learned)
				}
				return
			}
			if code == http.StatusOK || learned {
				t.Fatalf("a drifted pool call while the store shows test-2 was learned onto the pool: answered %d %s, learned %v", code, got, learned)
			}
		})
	}
}

// A learn step whose take finds the view it read closing (an install under
// way) waits for the install and reads the round once more, in the view that
// follows: it never reads the closing view again and again until the
// install lands. The call in flight, made as test-2 opens, is answered from
// test-2's own mock-a.
func TestALearnStepReadsTheRoundOnceMoreAfterAnInstall(t *testing.T) {
	p, mm := roundOrders(t, 100*time.Millisecond, 200*time.Millisecond, "mock-a")
	opts := models.OutgoingOptions{Learn: true}
	p.ContinueTestErrorCapture()
	var reads atomic.Int64
	// A third read while the install is held is a read of the closing view
	// again: the row lets the install go on, and fails.
	spun := make(chan struct{})
	var held atomic.Bool
	mm.exact.round.onRead = func(step string) {
		if step == "view" && reads.Add(1) == 3 && held.Load() {
			close(spun)
		}
	}
	waits := make(chan struct{}, 1)
	mm.exact.onClosedWait = func() {
		select {
		case waits <- struct{}{}:
		default:
		}
	}
	sent := make(chan struct{})
	var code int
	var got string
	waited := false
	mm.exact.beforeInstall = func() {
		mm.exact.beforeInstall = nil
		held.Store(true)
		go func() {
			defer close(sent)
			code, got = r3Send(t, p, mm, opts, roundPost(`{"item":"apple"}`))
		}()
		// The install goes on once the call waits for it, returns, or reads
		// the closing view again.
		select {
		case <-waits:
			waited = true
		case <-sent:
		case <-spun:
		}
		held.Store(false)
	}
	mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-a"), roundMock(mm, "mock-b")})
	<-sent
	mm.exact.round.onRead, mm.exact.onClosedWait = nil, nil
	if !waited || reads.Load() != 2 {
		t.Fatalf("the call waited for the install %v, and read the round %d times; want it to wait, and read the round once before the install and once after", waited, reads.Load())
	}
	if code != http.StatusOK || got != `{"order":1}` {
		t.Fatalf("the call in flight got %d %s; want test-2's mock-a", code, got)
	}
}

// A take refused because the learn tracker began or ended a test between a
// pass's read and its take is read again: the boundary moves the round's
// generation, not the store's revision, and the next pass, of the round as
// it is now, serves the call. Told by the store's revision alone, the
// refusal read as a recording the store would not hand out, and a call its
// recording answers exactly missed: a false red. Two schedules the replay
// makes: keploy mock's scope flow opens a test's view before the tracker
// begins it, and the replay ends a test on the store (its pool view) before
// the tracker.
func TestALearnStepReadsAgainATakeALearnBoundaryRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		at       func(p *Proxy, mm *MockManager)
		boundary func(p *Proxy)
		request  string
		want     string
	}{
		{"the tracker begins the test the store shows",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			},
			func(p *Proxy) { p.ContinueTestErrorCapture() },
			"GET /status HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n", `{"up":1}`},
		{"the tracker ends the test the store ended",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			},
			func(p *Proxy) { p.EndTestErrorCapture() },
			roundPost(`{"item":"apple"}`), `{"order":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mm := s12Orders(t)
			tc.at(p, mm)
			before, _ := mm.RoundSnapshot(integrations.RoundQuery{})
			// The boundary lands as the call's take is about to take the
			// round lock: after its read, before its check.
			var moved atomic.Bool
			mm.exact.round.onWait = func() {
				if moved.CompareAndSwap(false, true) {
					tc.boundary(p)
				}
			}
			code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, tc.request)
			after, _ := mm.RoundSnapshot(integrations.RoundQuery{})
			if !moved.Load() || after.At.Gen == before.At.Gen {
				t.Fatalf("precondition: no boundary landed under the call's take (round %+v -> %+v)", before.At, after.At)
			}
			if code != http.StatusOK || got != tc.want {
				t.Fatalf("a call whose take a learn boundary refused got %d %s; want %s, read again", code, got, tc.want)
			}
		})
	}
}

// A voided ahead answer is spent for whichever test opens with it in its
// view: one the test shares with the tests recorded beside it
// (ServeExactTestConcurrent) as well as one of its own. A call that is it
// again is one call more, named; counted as spent only for the test's own,
// the shared recording answered a second call of the round.
func TestALearnStepCountsAVoidedAheadAnswerATestSharesAsSpent(t *testing.T) {
	p, mm := roundOrders(t, 100*time.Millisecond, 200*time.Millisecond, "mock-a")
	opts := models.OutgoingOptions{Learn: true}
	if code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"apple"}`)); code != http.StatusOK || got != `{"order":1}` {
		t.Fatalf("precondition: the call in flight got %d %s", code, got)
	}
	// The test mock-a was answered for does not open next: test-3 does, and
	// shares mock-a.
	p.ContinueTestErrorCapture()
	mm.ServeExactTestConcurrent("test-3", []*models.Mock{roundMock(mm, "mock-b")}, []*models.Mock{roundMock(mm, "mock-a")})
	code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"apple"}`))
	if code == http.StatusOK {
		t.Fatalf("the shared mock-a answered a second call of the round: %d %s", code, got)
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 1 || misses[0].ClosestMock != "mock-a" || !strings.Contains(misses[0].NextSteps, "one call more") {
		t.Fatalf("the miss does not name mock-a as the call answered already: %+v", misses)
	}
}

// A pairing's claim lands only in the staging its candidates were read in.
// The set is staged again between the read and the claim, under the same
// names (names are unique within a set only), mock-x answering otherwise
// now: the claim is refused, and the call is paired again in the staging
// in force, answered with its mock-x. Claimed by name in the new staging,
// the call was answered with the old staging's recording while the new
// one's was marked claimed.
func TestAPairingsClaimLandsOnlyInTheStagingItWasReadIn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	set := func(answer string) []*models.Mock {
		own := r3Mock("mock-own", now.Add(100*time.Millisecond), "GET", "/status", "", `{"up":true}`)
		x := r3Mock("mock-x", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, answer)
		return []*models.Mock{own, x}
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SetMocksWithWindow(set(`{"order":1}`), nil, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
	mm.SetPairing(true)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	p.ContinueTestErrorCapture()
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own")})
	// The call's first take is the pairing's claim: the set is staged again
	// before it lands.
	staged := false
	mm.exact.round.onWait = func() {
		if staged {
			return
		}
		staged = true
		mm.SetMocksWithWindow(set(`{"order":2}`), nil, models.BaseTime, now.Add(time.Hour))
	}
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, roundPost(`{"item":"apple"}`))
	mm.exact.round.onWait = nil
	if !staged {
		t.Fatalf("precondition: the call took nothing (answered %d %s)", code, got)
	}
	if code != http.StatusOK || got != `{"order":2}` {
		t.Fatalf("the call was answered %d %s; want the staging in force's mock-x, {\"order\":2}", code, got)
	}
}
