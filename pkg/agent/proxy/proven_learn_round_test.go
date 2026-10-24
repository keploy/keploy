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
// round. Under the background rules the pool is learned onto between tests
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

// s12Orders stages, under exact serving with the background rules, the
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

// While the store shows a test's view, the background answers a call
// exactly or not at all, and no drift is learned onto it, whether the learn
// tracker has that test open or not: the pool is learned onto between tests
// only. Two schedules the replay makes put the tracker out of step with the
// store's test view: keploy mock's scope flow opens a test's view before the
// tracker begins it, and a test whose misses' read did not complete never
// ended on the tracker. Read as between tests there, a drifted call of the
// test was answered from the pool, its item learned onto mock-pool as
// noise: a false green. Between tests the same call is learned onto the
// pool, as the background rules have it.
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
// generation, not the store's revision, and the next pass, on the store's
// same view, with the tracker caught up with it, serves the call. Told by the store's revision alone, the
// refusal read as a recording the store would not hand out, and a call its
// recording answers exactly missed: a false red. Two schedules the replay
// makes: keploy mock's scope flow opens a test's view before the tracker
// begins it. (The other, the replay ending a test on the store before the
// tracker, is a round of no one test: TestALearnStepDecidesNothingOfACallReadAfterTheStoreEndedATestTheTrackerHasOpen.)
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

// A call read where keploy's store ended test-1 (its pool view) while the
// learn tracker still has test-1 open is of no one round: test-1's late call
// or the round between tests' early one, and nothing tells which. It misses,
// with no reason, naming test-1, and nothing is learned, whether it differs
// from the pool's mock-pool or is it exactly, whether it was made after its
// test case's request or before it (it waits for nothing), under today's
// rules and proven routing. Read as between tests, a call that is exactly
// test-1's own mock-own1 was paired with mock-pool, and the item it differs
// in learned onto it as noise: a false green. Once the tracker ends test-1
// too, the round is between tests, and a call is answered from the pool.
func TestALearnStepDecidesNothingOfACallReadAfterTheStoreEndedATestTheTrackerHasOpen(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, body := range []string{`{"item":"pear"}`, `{"item":"apple"}`} {
			for _, waits := range []bool{false, true} {
				name := map[bool]string{false: "today's rules", true: "proven routing"}[routed] + "/" + body + "/" +
					map[bool]string{false: "a call made after the request", true: "a call made before it"}[waits]
				t.Run(name, func(t *testing.T) {
					p, mm := heldOrders(t, routed)
					mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
					p.ContinueTestCaseCapture()
					req := &models.TestRequest{Test: "test-1"}
					if !waits {
						p.TakeTestRequest(req)
					}
					mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
					if snap, _ := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker}); snap.Ended != "test-1" {
						t.Fatalf("precondition: the round reads %v (%s), ended %q; want test-1 ended on the store alone", snap.Phase, snap.Boundary, snap.Ended)
					}
					// A call that waits for its test case's request is
					// given it, and fails the row: it was decided in no
					// round, and waited for nothing.
					p.learnTracker.OnAwait(func() {
						t.Error("the call waited for its test case's request in a round of no one test")
						p.TakeTestRequest(req)
					})
					code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(body))
					roundMiss(t, p, mm, code, got, "keploy record's full round read this request after keploy's store ended test-1 and before its learn tracker did, so the call is of neither test-1 nor the round between tests")
					p.EndTestErrorCapture()
					if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"apple"}`)); code != http.StatusOK || got != `{"order":0}` {
						t.Fatalf("a call between tests, once the tracker ended test-1 too, got %d %s; want the pool's mock-pool", code, got)
					}
				})
			}
		}
	}
}

// heldOrders stages, under exact serving, the pool's mock-pool (POST /orders
// {"item":"apple"}), test-1's own mock-own1 ({"item":"pear"}) and test-2's
// own mock-own2 ({"item":"plum"}), served by proven routing or by today's
// rules, and serves the pool view. The proxy holds the manager, as the
// agent's does, so the learn tracker's boundaries reach it.
func heldOrders(t *testing.T, routed bool) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
	own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
	own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: now.Add(500 * time.Millisecond), End: now.Add(800 * time.Millisecond)},
		{Start: now.Add(time.Second), End: now.Add(2 * time.Second)},
	})
	mm.SetMocksWithWindow([]*models.Mock{pool, own1, own2}, nil, models.BaseTime, now.Add(time.Hour))
	if routed {
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-pool"}, nil)
	}
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	return p, mm
}

// heldMiss fails t unless the call was answered by nothing, learned nothing
// onto any recording, and missed once, with no reason, saying where keploy
// read it (where) and what the replay moved (moved).
func heldMiss(t *testing.T, p *Proxy, mm *MockManager, code int, got, where, moved string) {
	t.Helper()
	roundMiss(t, p, mm, code, got, "keploy record's full round read this request "+where+", and the replay "+moved+" before keploy could answer it")
}

// roundMiss fails t unless the call was answered by nothing, learned nothing
// onto any recording, and missed once, with no reason, its next steps
// saying why.
func roundMiss(t *testing.T, p *Proxy, mm *MockManager, code int, got, why string) {
	t.Helper()
	for name, st := range r3States(mm) {
		if len(st.ReqBodyNoise) > 0 {
			t.Errorf("%s learned %v as noise from a call of another round", name, st.ReqBodyNoise)
		}
	}
	if code == http.StatusOK {
		t.Fatalf("a call whose take the replay refused by moving its round got %d %s; want a miss", code, got)
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 1 || misses[0].Reason != "" || !strings.Contains(misses[0].NextSteps, why) {
		t.Fatalf("the miss reads %+v; want one, with no reason, saying %q", misses, why)
	}
}

// A call of a test whose take the replay refused by moving the store's view
// under it (it ended the test, or ended it and began the next) is decided in
// the round it was made in or in none: it misses, saying the replay ended
// its test, and nothing is learned onto the pool's or the next test's
// recordings. Read again as the round then stood, the call (exactly test-1's
// own mock-own1) was answered between tests from mock-pool, or in test-2
// from test-2's own mock-own2, and the item it differs from them in learned
// there as noise: a false green whose verdict depended on where the
// boundary fell. Both orders of the end (the store's pool view first, the
// tracker's End first), and the store's move alone under the call (its pool
// view, and test-2's view where test-2 begins, while the tracker still has
// test-1 open: there, before, the call was read again in a round whose
// recordings no test owns), under today's rules and proven routing, for a
// call made after its test case's request and for one that waited for it.
func TestALearnStepDecidesNothingOfACallInARoundTheStoreMovedOnFrom(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, next := range []bool{false, true} {
			for _, order := range []string{"the store ends it first", "the tracker ends it first", "the store alone ends it under the call"} {
				for _, waits := range []bool{false, true} {
					name := map[bool]string{false: "today's rules", true: "proven routing"}[routed] + "/" +
						map[bool]string{false: "the test ends", true: "the next test begins"}[next] + "/" +
						order + "/" +
						map[bool]string{false: "a call made after its request", true: "a call that waited for it"}[waits]
					t.Run(name, func(t *testing.T) {
						p, mm := heldOrders(t, routed)
						mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
						if waits {
							p.ContinueTestCaseCapture()
						} else {
							p.ContinueTestErrorCapture()
						}
						// The boundary lands as the call's take is about to
						// take the round lock: after its read, before its
						// check.
						var moved atomic.Bool
						mm.exact.round.onWait = func() {
							if !moved.CompareAndSwap(false, true) {
								return
							}
							switch order {
							case "the store ends it first":
								mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
								p.EndTestErrorCapture()
							case "the tracker ends it first":
								p.EndTestErrorCapture()
								mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
							default:
								mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
							}
							if next {
								mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
								if order != "the store alone ends it under the call" {
									p.ContinueTestErrorCapture()
								}
							}
						}
						t.Cleanup(func() { mm.exact.round.onWait = nil })
						opts := models.OutgoingOptions{Learn: true, LearnPool: true}
						var code int
						var got string
						if !waits {
							code, got = r3Send(t, p, mm, opts, roundPost(`{"item":"pear"}`))
						} else {
							waiting := make(chan struct{}, 1)
							p.learnTracker.OnAwait(func() {
								select {
								case waiting <- struct{}{}:
								default:
								}
							})
							sent := make(chan struct{})
							go func() {
								defer close(sent)
								code, got = r3Send(t, p, mm, opts, roundPost(`{"item":"pear"}`))
							}()
							select {
							case <-waiting:
							case <-sent:
								t.Fatalf("precondition: the call did not wait for its test case's request (got %d %s)", code, got)
							}
							p.TakeTestRequest(&models.TestRequest{Test: "test-1"})
							<-sent
						}
						if !moved.Load() {
							t.Fatal("precondition: no boundary landed under the call's take")
						}
						heldMiss(t, p, mm, code, got, "in test-1", "ended that test")
					})
				}
			}
		}
	}
}

// A call made while the store showed test-1's view that the learn tracker
// had not begun yet (keploy mock's scope flow opens the view first) is of
// test-1 by the store's view: once the replay ended test-1 on the store under
// its take, the tracker never having begun it, the two agree again, between
// tests, but the store moved on and the tracker caught up with nothing. The
// call misses, saying keploy read it while the store showed test-1's view
// with no test open on the tracker; read again between tests, it was
// answered from mock-pool and its item learned there as noise.
func TestALearnStepDecidesNothingOfACallOnceTheStoreEndedATestTheTrackerNeverBegan(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "today's rules", true: "proven routing"}[routed], func(t *testing.T) {
			p, mm := heldOrders(t, routed)
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			var moved atomic.Bool
			mm.exact.round.onWait = func() {
				if moved.CompareAndSwap(false, true) {
					mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				}
			}
			t.Cleanup(func() { mm.exact.round.onWait = nil })
			code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"pear"}`))
			if !moved.Load() {
				t.Fatal("precondition: no boundary landed under the call's take")
			}
			heldMiss(t, p, mm, code, got, "while keploy's store showed test-1's view, with no test open on its learn tracker", "ended that test")
		})
	}
}

// A call whose take the tracker's begin of the test the store shows refused
// (keploy mock's scope flow opens the view first), that test awaiting its
// test case's request, is read again in that test's round, waits there for
// the request, and is answered once it came: the round the tracker caught
// up with is the call's from then on (learnRound.movedFrom), so the pass
// after the wait is held to it, not to the first pass's, read before the
// test began, which would read the begun test as a move.
func TestALearnStepWaitsInTheRoundTheTrackerCaughtUpWith(t *testing.T) {
	p, mm := s12Orders(t)
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
	var moved atomic.Bool
	mm.exact.round.onWait = func() {
		if moved.CompareAndSwap(false, true) {
			p.ContinueTestCaseCapture()
		}
	}
	t.Cleanup(func() { mm.exact.round.onWait = nil })
	waiting := make(chan struct{}, 1)
	p.learnTracker.OnAwait(func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	})
	sent := make(chan struct{})
	var code int
	var got string
	go func() {
		defer close(sent)
		code, got = r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, "GET /status HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
	}()
	select {
	case <-waiting:
	case <-sent:
		t.Fatalf("precondition: the call did not wait for its test case's request (got %d %s)", code, got)
	}
	if !moved.Load() {
		t.Fatal("precondition: the tracker's begin did not land under the call's take")
	}
	p.TakeTestRequest(&models.TestRequest{Test: "test-1"})
	<-sent
	if code != http.StatusOK || got != `{"up":1}` {
		t.Fatalf("a call read again in the test the tracker began under its take, its request come, got %d %s; want test-1's mock-own1", code, got)
	}
}

// A take refused as the learn tracker moved to another test is no catch-up,
// though the two may agree after it: the call misses. A catch-up lands only
// on the test the call's first pass read. Where the store showed test-2's
// view while the tracker still had test-1 open, the tracker ending test-1
// and beginning test-2 agrees with the view, but the tracker had another
// test open when the call was read: it is test-1's late call or test-2's
// early one, and nothing tells which (caught up, the call that is exactly
// test-2's own mock-own2 was answered from it). Where the tracker began
// test-2 before the store opened it, the tracker ending test-2 and beginning
// test-3 with the store opening test-3's view agrees too, in another test
// than the one the call was read in (caught up, the call was paired with
// test-3's own mock-own2 and its item learned onto it), and the tracker
// ending test-2 with no view of it agrees with the store's pool view, in the
// round between tests, not the test the call was read in (caught up, the
// call was paired with mock-pool and its item learned onto it). Where the store shows test-1's view
// the tracker ended first, the tracker's begin of the next test, before the
// store opened it, agrees with no view.
func TestALearnStepDecidesNothingOfACallOnceTheTrackerMovedToAnotherTest(t *testing.T) {
	for _, tc := range []struct {
		name, body, where, moved string
		at                       func(p *Proxy, mm *MockManager)
		boundary                 func(p *Proxy, mm *MockManager)
	}{
		// A call no recording is exactly (none is named as made
		// elsewhere), which the round between tests pairs with mock-pool.
		{"the store opened test-2 while the tracker had test-1 open, then the tracker began test-2", `{"item":"plum"}`,
			"in the test then running", "ended that test",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
			},
			func(p *Proxy, mm *MockManager) {
				p.EndTestErrorCapture()
				p.ContinueTestErrorCapture()
			}},
		{"the tracker began test-2 before the store opened it, then ended it", `{"item":"fig"}`,
			"in the test then running", "ended that test",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				p.EndTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.ContinueTestErrorCapture()
			},
			func(p *Proxy, mm *MockManager) { p.EndTestErrorCapture() }},
		{"the tracker began test-2 before the store opened it, then moved to test-3 as the store opened test-3", `{"item":"fig"}`,
			"in the test then running", "ended that test",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				p.EndTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.ContinueTestErrorCapture()
			},
			func(p *Proxy, mm *MockManager) {
				p.EndTestErrorCapture()
				p.ContinueTestErrorCapture()
				mm.ServeExactTest("test-3", []*models.Mock{roundMock(mm, "mock-own2")})
			}},
		{"the tracker ended test-1, then began the next test before the store opened it", `{"item":"pear"}`,
			"while keploy's store showed test-1's view, with no test open on its learn tracker", "began a test",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				p.EndTestErrorCapture()
			},
			func(p *Proxy, mm *MockManager) { p.ContinueTestErrorCapture() }},
	} {
		for _, routed := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "today's rules", true: "proven routing"}[routed], func(t *testing.T) {
				p, mm := heldOrders(t, routed)
				tc.at(p, mm)
				var moved atomic.Bool
				mm.exact.round.onWait = func() {
					if moved.CompareAndSwap(false, true) {
						tc.boundary(p, mm)
					}
				}
				t.Cleanup(func() { mm.exact.round.onWait = nil })
				code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(tc.body))
				if !moved.Load() {
					t.Fatal("precondition: no boundary landed under the call's take")
				}
				heldMiss(t, p, mm, code, got, tc.where, tc.moved)
			})
		}
	}
}

// A take refused as the store installed another view under it is no
// catch-up where the tracker did not lead: between tests the store serving
// another pool view (its recordings may be others), and a staging of the set
// again. The call misses, saying
// what moved, and is learned onto nothing; read again, it was paired with
// mock-pool between tests and its item learned there as noise.
func TestALearnStepDecidesNothingOfACallOnceTheStoreMovedItsViewAlone(t *testing.T) {
	for _, tc := range []struct {
		name, body, where, moved string
		at                       func(p *Proxy, mm *MockManager)
		boundary                 func(p *Proxy, mm *MockManager)
	}{
		{"between tests, the store serves another pool view", `{"item":"fig"}`,
			"in the round between tests", "served another view of its recordings between tests",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.EndTestErrorCapture()
			},
			func(p *Proxy, mm *MockManager) { mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"}) }},
		{"the replay stages the set again in test-1", `{"item":"pear"}`,
			"in test-1", "staged a test set again",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
			},
			func(p *Proxy, mm *MockManager) {
				mm.SetMocksWithWindow(mm.StagedMocks(), nil, models.BaseTime, time.Now().Add(time.Hour))
			}},
	} {
		for _, routed := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "today's rules", true: "proven routing"}[routed], func(t *testing.T) {
				p, mm := heldOrders(t, routed)
				tc.at(p, mm)
				var moved atomic.Bool
				mm.exact.round.onWait = func() {
					if moved.CompareAndSwap(false, true) {
						tc.boundary(p, mm)
					}
				}
				t.Cleanup(func() { mm.exact.round.onWait = nil })
				code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(tc.body))
				if !moved.Load() {
					t.Fatal("precondition: no boundary landed under the call's take")
				}
				heldMiss(t, p, mm, code, got, tc.where, tc.moved)
			})
		}
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

// A call pairs with nothing once the replay staged again after its first
// read. The store counts stagings and cannot tell the same set staged again
// from the next one, and a recording of the next set must never answer a
// call of the set before, nor be spent on it: the call misses, a false red
// at worst, whichever set lands under it. Here the set staged again under
// the call is the same (the same names, mock-x answering otherwise now) or
// the next (other names, set2-y the same request): the call is answered by
// neither, and no recording of the staging in force is claimed for it.
func TestAPairingPairsNothingOnceTheReplayStagedAgainUnderTheCall(t *testing.T) {
	now := time.Now()
	set := func(own, x, answer string) []*models.Mock {
		return []*models.Mock{
			r3Mock(own, now.Add(100*time.Millisecond), "GET", "/status", "", `{"up":true}`),
			r3Mock(x, now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, answer),
		}
	}
	for _, tc := range []struct {
		name  string
		again []*models.Mock
		x     string
	}{
		{"the same set", set("mock-own", "mock-x", `{"order":2}`), "mock-x"},
		{"the next set", set("set2-own", "set2-y", `{"order":"from set 2"}`), "set2-y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			mm := NewMockManager(nil, nil, zap.NewNop())
			mm.SetMocksWithWindow(set("mock-own", "mock-x", `{"order":1}`), nil, models.BaseTime, now.Add(time.Hour))
			mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
			mm.SetPairing(true)
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			p.ContinueTestErrorCapture()
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own")})
			// The call's first take is the pairing's claim: the set is
			// staged again before it lands.
			staged := false
			mm.exact.round.onWait = func() {
				if staged {
					return
				}
				staged = true
				mm.SetMocksWithWindow(tc.again, nil, models.BaseTime, now.Add(time.Hour))
			}
			code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, roundPost(`{"item":"apple"}`))
			mm.exact.round.onWait = nil
			if !staged {
				t.Fatalf("precondition: the call took nothing (answered %d %s)", code, got)
			}
			if code == http.StatusOK {
				t.Fatalf("a call of the staging before was answered %d %s by a pairing in the staging in force", code, got)
			}
			if mm.exact.claimed.has(mm.StagingEpoch(), tc.x) {
				t.Fatalf("the staging in force's %s was claimed for a call of the staging before", tc.x)
			}
		})
	}
}

// gapRound is s12Orders between test-1 and test-2: test-1 (mock-own1's) ran
// and ended, the store shows the pool view, whose mock-pool answers the gap
// and is learned onto there, and keploy test opened test-2's capture window
// (BeginTestCaseCapture): the learn step's test-2 awaits its test case's
// request, which the filter that opens the test brings (TakeTestRequest)
// before the store installs test-2's view.
func gapRound(t *testing.T) (*Proxy, *MockManager) {
	t.Helper()
	p, mm := s12Orders(t)
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
	p.ContinueTestErrorCapture()
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	if _, err := p.GetMockErrors(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.BeginTestCaseCapture()
	return p, mm
}

// A call that reaches the learn step through the agent's MockManager (one
// read of the round, RoundSnapshot) after keploy test opened test-2's
// capture window and before the filter that opens the test brought its test
// case's request waits for the request, and is learned from as a call made
// after it: its item that drifted is noise either way, learned onto the
// pool recording it is answered from, and one the request holds the
// recorded word of is refused either way. The round's snapshot says the
// test awaits its request until the request came. A snapshot that dropped
// it decided the call at once, against a request it could not read: by
// where the scheduler put the call, two outcomes. The call is ordered
// before the request through the tracker's OnAwait, through no clock, 200
// times each.
func TestACallInATestCasesGapIsLearnedThroughTheRoundAsOneAfterItsRequest(t *testing.T) {
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	call := roundPost(`{"item":"apricot"}`)
	request := func(q string) *models.TestRequest {
		return &models.TestRequest{Test: "test-2", Parts: []models.TestRequestPart{
			{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/json"},
			{Name: "body", Recorded: `{"q":"` + q + `"}`, Sent: `{"q":"` + q + `"}`},
		}}
	}
	outcome := func(mm *MockManager, code int, got string) string {
		return strconv.Itoa(code) + " " + got + ", learned " + strconv.FormatBool(len(r3States(mm)["mock-pool"].ReqBodyNoise) > 0)
	}
	awaiting := func(mm *MockManager, p *Proxy) bool {
		snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
		return ok && snap.Learn.Awaiting
	}
	for _, q := range []string{"books", "apple"} {
		for i := 0; i < 200; i++ {
			// The request first.
			p, mm := gapRound(t)
			if !awaiting(mm, p) {
				t.Fatal("the round's snapshot does not say test-2 awaits its request after its capture window opened")
			}
			p.TakeTestRequest(request(q))
			if awaiting(mm, p) {
				t.Fatal("the round's snapshot says test-2 awaits its request after the request came")
			}
			code, got := r3Send(t, p, mm, opts, call)
			after := outcome(mm, code, got)
			if q == "books" && after != `200 {"order":0}, learned true` || q != "books" && strings.HasPrefix(after, "200 ") {
				t.Fatalf("q %s, after the request: %s", q, after)
			}
			// The call first, in the gap: the request comes once the call
			// waits for it, never before.
			p, mm = gapRound(t)
			waiting := make(chan struct{}, 1)
			p.learnTracker.OnAwait(func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			})
			answered := make(chan struct{})
			gave := make(chan bool, 1)
			go func() {
				select {
				case <-waiting:
					p.TakeTestRequest(request(q))
					gave <- true
				case <-answered:
					gave <- false
				}
			}()
			code, got = r3Send(t, p, mm, opts, call)
			close(answered)
			if !<-gave {
				t.Fatalf("q %s: the call in the gap was answered (%d %s) without waiting for its test case's request", q, code, got)
			}
			if before := outcome(mm, code, got); before != after {
				t.Fatalf("q %s: in the gap %s, after the request %s: two outcomes by where the call fell", q, before, after)
			}
		}
	}
}
