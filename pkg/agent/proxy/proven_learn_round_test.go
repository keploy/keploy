package proxy

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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
			// test-2's own call of mock-b is still answered from it, in
			// test-2 on both the store and the tracker. (A tracker begin
			// here, the store still on test-2's view, is the tracker ahead
			// of the store: the call waits for test-3's view,
			// TestALearnStepWaitsForTheStoreToOpenTheTestTheTrackerBegan.)
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
	// test-2's own call of mock-b is still answered from it, in test-2 on
	// both the store and the tracker. (A tracker begin here, the store
	// still on test-2's view, is the tracker ahead of the store: the call
	// waits for test-3's view,
	// TestALearnStepWaitsForTheStoreToOpenTheTestTheTrackerBegan.)
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
// only. Two schedules put the tracker out of step with the store's test
// view: a store that opens a test's view before the tracker begins it (no
// agent flow does so today: keploy test's loops and keploy mock's scope flow
// both begin the tracker's test first), and a test whose misses' read did
// not complete never ended on the tracker. Read as between tests there, a
// drifted call of the
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
			// test-1, the store first: the view, then the tracker's begin.
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

// A call made at test-2's begin, awaiting its test case's request, read
// where the learn tracker began test-2 before keploy's store opened it (the
// tracker ahead of the store: integrations.RoundSnapshot.TrackerAhead), is
// decided with one verdict whichever way its request and the store's install
// fall, ordered through the round's and the tracker's wait hooks, through no
// clock. Where the store still shows test-1's view (a state keploy test's
// flow never leaves in force), the call is of test-2: it waits for the store
// to open test-2 and for its request, whichever comes first, and is decided
// in test-2's round, test-2's own mock-own2 answering a call that is exactly
// it. (Before, a request that came before the install left the call of no
// one round, a miss: two verdicts by where the install fell.) Where the
// store shows its pool view (integrations.RoundSnapshot.PoolsCall: keploy
// test's begin of every test, whose view the agent installs only once every
// call made before is answered), the call is the pool's: it waits for its
// request, as any call read before the request was given does, never for the
// store, and is decided in the round between tests, mock-pool answering a
// call that is exactly it whether the request came before the call or as it
// waited. A store that opens test-2 as the pool's call waits for its request
// (a flow without keploy test's settle) moved the round between tests under
// it: the moved miss, naming no recording made elsewhere. Under today's
// rules and proven routing.
func TestALearnStepReadsTheRoundOnceMoreAfterAnInstall(t *testing.T) {
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	request := func(p *Proxy) { p.TakeTestRequest(&models.TestRequest{Test: "test-2"}) }
	install := func(mm *MockManager) {
		mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
	}
	ahead := func(t *testing.T, p *Proxy, mm *MockManager, pools bool) {
		t.Helper()
		snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
		if !ok || !snap.TrackerAhead() || snap.PoolsCall() != pools {
			t.Fatalf("precondition: the round's snapshot does not read the tracker ahead of the store (the pool's call %v): %+v", pools, snap.Boundary)
		}
	}
	for _, routed := range []bool{false, true} {
		rules := map[bool]string{false: "today's rules", true: "proven routing"}[routed]
		for _, installFirst := range []bool{true, false} {
			t.Run(rules+"/the store still shows test-1's view/"+map[bool]string{true: "the store opens test-2, then the request comes", false: "the request comes before the store opens test-2"}[installFirst], func(t *testing.T) {
				p, mm := heldOrders(t, routed)
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				p.EndTestErrorCapture()
				p.ContinueTestCaseCapture()
				ahead(t, p, mm, false)
				var storeWait, requestWait atomic.Bool
				mm.exact.round.onAwaitMove = func() {
					if storeWait.CompareAndSwap(false, true) {
						if !installFirst {
							request(p)
						}
						install(mm)
					}
				}
				p.learnTracker.OnAwait(func() {
					if installFirst && requestWait.CompareAndSwap(false, true) {
						request(p)
					}
				})
				code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"plum"}`))
				mm.exact.round.onAwaitMove = nil
				p.learnTracker.OnAwait(nil)
				if !storeWait.Load() || installFirst && !requestWait.Load() {
					t.Fatalf("the call was decided (%d %s) without waiting for the store (%v) or for its request (%v)", code, got, storeWait.Load(), requestWait.Load())
				}
				if code != http.StatusOK || got != `{"order":2}` {
					t.Fatalf("the call that waited for test-2 got %d %s; want test-2's mock-own2", code, got)
				}
			})
		}
		for _, order := range []string{"the request comes before the call", "the request comes as the call waits for it", "the store opens test-2 as the call waits for its request"} {
			t.Run(rules+"/the store shows its pool view/"+order, func(t *testing.T) {
				p, mm := heldOrders(t, routed)
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				p.EndTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.ContinueTestCaseCapture()
				ahead(t, p, mm, true)
				requestFirst := order == "the request comes before the call"
				if requestFirst {
					request(p)
				}
				var storeWait, requestWait atomic.Bool
				mm.exact.round.onAwaitMove = func() {
					// Ends the wait, so the row fails rather than hangs.
					if storeWait.CompareAndSwap(false, true) {
						install(mm)
					}
				}
				p.learnTracker.OnAwait(func() {
					if requestWait.CompareAndSwap(false, true) {
						if order == "the store opens test-2 as the call waits for its request" {
							install(mm)
						}
						request(p)
					}
				})
				code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"apple"}`))
				mm.exact.round.onAwaitMove = nil
				p.learnTracker.OnAwait(nil)
				if storeWait.Load() {
					t.Fatalf("the pool's call read as test-2 opens waited for the store to open test-2 (%d %s)", code, got)
				}
				if requestWait.Load() == requestFirst {
					t.Fatalf("the pool's call awaited its request %v; its request came before it %v", requestWait.Load(), requestFirst)
				}
				if order == "the store opens test-2 as the call waits for its request" {
					misses := heldMiss(t, p, mm, code, got, "in the round between tests, as keploy's learn tracker began the next test", "began a test")
					for _, m := range misses {
						if strings.Contains(m.NextSteps, "which keploy recorded where this replay did not make it") {
							t.Fatalf("the moved miss of the pool's call names a recording made elsewhere: %s", m.NextSteps)
						}
					}
					return
				}
				if code != http.StatusOK || got != `{"order":0}` {
					t.Fatalf("the pool's call read as test-2 opens got %d %s; want mock-pool's answer", code, got)
				}
			})
		}
	}
}

// A take refused because the learn tracker began or ended a test between a
// pass's read and its take is read again: the boundary moves the round's
// generation, not the store's revision, and the next pass, on the store's
// same view, with the tracker caught up with it, serves the call. Told by the store's revision alone, the
// refusal read as a recording the store would not hand out, and a call its
// recording answers exactly missed: a false red. Two schedules: a store that
// opens a test's view before the tracker begins it (no agent flow does so
// today: both begin the tracker's test first). (The other, the replay ending
// a test on the store before the
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

// A call read where keploy's store shows test-1's view and the learn tracker
// has no test open is of no one round to learn in
// (integrations.RoundSnapshot.ViewAlone): test-1's call (the tracker ended
// test-1 while the store still shows its view, as where a test's end did not
// install the pool view) or the round between tests', and
// nothing tells which. It is answered from the store's view only by a
// recording it matches exactly (test-1's own mock-own1, the in-flight gap
// recording mock-gap the view shows), and learns nothing: a call that differs
// misses, decided at once and never waiting (for a test case's request, or
// for the round to move), under today's rules and proven routing. On a set
// without the background rules, the view's in-flight gap recording is the
// pool's in that round, and the call was paired with it and the item it
// differs in learned onto it as noise: a false green; the miss now says why,
// with no reason. Under the background rules the view shows the pool only to
// answer exactly, so the call misses as one the learn step may not learn
// onto. Once the tracker began test-1, the store's view agreed on, a call is
// test-1's and learns as any other.
func TestACallReadWhereTheStoreShowsATestViewTheTrackerHasNoTestOpenBesideLearnsNothing(t *testing.T) {
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	const alone = "keploy record's full round read this request while keploy's store showed test-1's view and its learn tracker had no test open, so the call is of neither test-1 nor the round between tests to learn in"
	// mock-gap is recorded between test-1's window and test-2's: a call in
	// flight into test-1, which test-1's view shows.
	gap := func(now time.Time) []*models.Mock {
		return []*models.Mock{r3Mock("mock-gap", now.Add(900*time.Millisecond), "POST", "/orders", `{"item":"kiwi"}`, `{"order":9}`)}
	}
	view := func(mm *MockManager) {
		mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1"), roundMock(mm, "mock-gap")})
	}
	states := []struct {
		name string
		at   func(p *Proxy, mm *MockManager)
	}{
		{"the store opened test-1 before the tracker began it", func(p *Proxy, mm *MockManager) { view(mm) }},
		{"the tracker ended test-1 before the store did", func(p *Proxy, mm *MockManager) {
			p.ContinueTestErrorCapture()
			view(mm)
			p.EndTestErrorCapture()
		}},
	}
	exact := map[string]string{
		`{"item":"pear"}`: `200 {"order":1}, learned onto , misses []`,
		`{"item":"kiwi"}`: `200 {"order":9}, learned onto , misses []`,
	}
	for _, routed := range []bool{false, true} {
		rules := map[bool]string{false: "today's rules", true: "proven routing"}[routed]
		for _, st := range states {
			for _, body := range []string{`{"item":"fig"}`, `{"item":"pear"}`, `{"item":"kiwi"}`} {
				t.Run(rules+"/"+st.name+"/"+body, func(t *testing.T) {
					p, mm := heldOrdersWith(t, routed, zap.NewNop(), gap)
					st.at(p, mm)
					snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
					if !ok || snap.ViewAlone() == "" || snap.NoOneRound() != "" || snap.TrackerAhead() {
						t.Fatalf("precondition: the round reads %v (%s); want test-1's view with no test open on the tracker", snap.Phase, snap.Boundary)
					}
					p.learnTracker.OnAwait(func() { t.Error("the call waited for a test case's request with no test open on the tracker") })
					mm.OnAwaitRoundMove(func() { t.Error("the call waited for the round to move") })
					code, got := r3Send(t, p, mm, opts, roundPost(body))
					if w, ok := exact[body]; ok {
						if o := roundOutcome(t, p, mm, code, got); o != w {
							t.Fatalf("a call exactly a recording the view shows reads %s; want %s", o, w)
						}
						return
					}
					if !routed {
						roundMiss(t, p, mm, code, got, alone)
						return
					}
					if o := roundOutcome(t, p, mm, code, got); code == http.StatusOK || strings.Contains(o, "learned onto m") {
						t.Fatalf("a call that differs from every recording reads %s; want a miss, nothing learned", o)
					}
				})
			}
			if st.name != "the store opened test-1 before the tracker began it" {
				continue
			}
			t.Run(rules+"/"+st.name+"/then the tracker begins it", func(t *testing.T) {
				p, mm := heldOrdersWith(t, routed, zap.NewNop(), gap)
				st.at(p, mm)
				p.ContinueTestErrorCapture()
				if snap, _ := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker}); snap.Phase != integrations.RoundInTest || snap.ViewAlone() != "" {
					t.Fatalf("precondition: the round reads %v (%s); want test-1 open on both", snap.Phase, snap.Boundary)
				}
				code, got := r3Send(t, p, mm, opts, roundPost(`{"item":"fig"}`))
				if o := roundOutcome(t, p, mm, code, got); code != http.StatusOK || !strings.Contains(o, "learned onto m") {
					t.Fatalf("a call of test-1, once the tracker began it, reads %s; want it answered, the item learned", o)
				}
			})
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
	return heldOrdersLogging(t, routed, zap.NewNop())
}

// heldOrdersLogging is heldOrders with the logger the row reads, set before
// the drain starts (startedDrainProxyLogging).
func heldOrdersLogging(t *testing.T, routed bool, logger *zap.Logger) (*Proxy, *MockManager) {
	t.Helper()
	return heldOrdersWith(t, routed, logger, nil)
}

// heldOrdersWith is heldOrdersLogging with more recordings staged beside
// mock-pool, mock-own1 and mock-own2, made from the time those are recorded
// after (more may be nil).
func heldOrdersWith(t *testing.T, routed bool, logger *zap.Logger, more func(now time.Time) []*models.Mock) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxyLogging(t, 100, logger)
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
	staged := []*models.Mock{pool, own1, own2}
	if more != nil {
		staged = append(staged, more(now)...)
	}
	mm.SetMocksWithWindow(staged, nil, models.BaseTime, now.Add(time.Hour))
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
func heldMiss(t *testing.T, p *Proxy, mm *MockManager, code int, got, where, moved string) []models.UnmatchedCall {
	t.Helper()
	return roundMiss(t, p, mm, code, got, "keploy record's full round read this request "+where+", and the replay "+moved+" before keploy could answer it")
}

// roundMiss fails t unless the call was answered by nothing, learned nothing
// onto any recording, and missed once, with no reason, its next steps
// saying why. It returns the misses it read: the read takes them, so a
// later read finds none.
func roundMiss(t *testing.T, p *Proxy, mm *MockManager, code int, got, why string) []models.UnmatchedCall {
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
	return misses
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
// had not begun yet (a store-led begin, which no agent flow makes today) is of
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
// (a store-led begin, which no agent flow makes today), that test awaiting its
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

// A take refused as the learn tracker moved to another test or set is no
// catch-up: the call misses, saying what moved. Where the store shows test-1's
// view the tracker ended first, the tracker's begin of the next test, before
// the store opened it, agrees with no view. Between tests, the tracker's new
// set under the take moves its number with the store's view unchanged: read
// again, the call (no recording is exactly it) was paired with mock-pool in a
// round of the next set's, and its item learned onto it.
func TestALearnStepDecidesNothingOfACallOnceTheTrackerMovedToAnotherTest(t *testing.T) {
	for _, tc := range []struct {
		name, body, where, moved string
		at                       func(p *Proxy, mm *MockManager)
		boundary                 func(p *Proxy, mm *MockManager)
	}{
		// A call no recording is exactly (none is named as made
		// elsewhere), which the round between tests pairs with mock-pool.
		{"between tests, the tracker begins a new set", `{"item":"fig"}`,
			"in the round between tests", "moved on to another test or test set",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				p.EndTestErrorCapture()
			},
			func(p *Proxy, mm *MockManager) { p.learnBoundary(func(t *learn.Tracker) { t.NewSet() }) }},
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

// A call read where keploy's store and its learn tracker are apart on which
// test is open is of no one round (integrations.RoundSnapshot.NoOneRound):
// one test's late call or the other's early one, and nothing tells which. It
// misses on its first pass, with no reason, saying so, and nothing is
// learned or answered from that pass, whether the replay's next boundary
// lands under the call (under its take, where it took a recording) or after
// it returned, under today's rules and proven routing. The states: the store
// ended test-1 and opened test-2 while the tracker still had test-1 open;
// and the store's end of test-1 with test-2's call in flight, the tracker
// still in test-1. Decided on the first pass, a call that is exactly
// test-2's own mock-own2 was answered from it or missed, and one no
// recording is exactly learned onto mock-pool between tests or missed, as
// the boundary fell. The tracker ahead of the store is no such state: its
// call waits for the store
// (TestALearnStepWaitsForTheStoreToOpenTheTestTheTrackerBegan).
func TestALearnStepDecidesNothingOfACallReadWhereTheStoreAndTheTrackerAreApartOnItsTest(t *testing.T) {
	const (
		apart = "keploy record's full round read this request after keploy's store ended test-1 for test-2's view and before its learn tracker ended test-1, so the call is of neither test-1 nor test-2"
		ended = "keploy record's full round read this request after keploy's store ended test-1 and before its learn tracker did, so the call is of neither test-1 nor the round between tests"
	)
	for _, tc := range []struct {
		name, why string
		bodies    []string
		at        func(p *Proxy, mm *MockManager)
		boundary  func(p *Proxy, mm *MockManager)
	}{
		{"the store ended test-1 and opened test-2 while the tracker had test-1 open; the tracker ends test-1 and begins test-2", apart,
			[]string{`{"item":"plum"}`, `{"item":"pear"}`},
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
			},
			func(p *Proxy, mm *MockManager) {
				p.EndTestErrorCapture()
				p.ContinueTestErrorCapture()
			}},
		{"the store ended test-1 with test-2's call in flight while the tracker had test-1 open; the tracker ends test-1", ended,
			[]string{`{"item":"plum"}`},
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"}, roundMock(mm, "mock-own2"))
			},
			func(p *Proxy, mm *MockManager) { p.EndTestErrorCapture() }},
	} {
		for _, routed := range []bool{false, true} {
			for _, body := range tc.bodies {
				for _, under := range []bool{true, false} {
					name := tc.name + "/" + map[bool]string{false: "today's rules", true: "proven routing"}[routed] + "/" + body + "/" +
						map[bool]string{true: "the boundary lands under the call", false: "the boundary lands after the call"}[under]
					t.Run(name, func(t *testing.T) {
						p, mm := heldOrders(t, routed)
						tc.at(p, mm)
						// The boundary lands under the call's take, where it
						// takes one: armed only while the call is decided, not
						// under the report's own reads of the round.
						var moved, armed atomic.Bool
						if under {
							armed.Store(true)
							mm.exact.round.onWait = func() {
								if armed.Load() && moved.CompareAndSwap(false, true) {
									tc.boundary(p, mm)
								}
							}
							t.Cleanup(func() { mm.exact.round.onWait = nil })
						}
						code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(body))
						armed.Store(false)
						roundMiss(t, p, mm, code, got, tc.why)
						if moved.CompareAndSwap(false, true) {
							tc.boundary(p, mm)
						}
						for name, st := range r3States(mm) {
							if len(st.ReqBodyNoise) > 0 {
								t.Errorf("%s learned %v as noise once the boundary landed after a call of no one round", name, st.ReqBodyNoise)
							}
						}
					})
				}
			}
		}
	}
}

// A call read where the learn tracker is ahead of keploy's store, the store
// still showing test-1's view after the tracker began test-2 (a state keploy
// test's flow never leaves in force: a test's end installs the pool view
// first), waits for the store, through no clock
// (integrations.RoundSnapshot.TrackerAhead). Where the store opens test-2,
// the call is decided in test-2's round (its own mock-own2 answers a call
// that is exactly it), with one verdict whether the store opened test-2 as
// the call waited or before the call came; decided on its first pass, it was
// answered from test-1's view, missed, or named test-2's own mock-own2 made
// elsewhere (record's judging then bakes it into the worker scope), as the
// scheduler fell. Where test-2's window closes as the call waits (the
// tracker's End, its next Begin), the call gets the moved miss: nothing
// learned or answered, and no recording named made elsewhere. So too where
// the store opens test-2 and then ends it alone, for test-3's view, before
// the call reads the round again: the store caught up with the tracker's
// test and moved on past it, a round of no one test
// (integrations.RoundSnapshot.NoOneRound) the call was never made in, so it
// is no catch-up of the call's round but the replay ending that test under
// it, and the miss says so. Under today's rules and proven routing. The
// tracker ahead of the store's pool view is no wait:
// TestALearnStepDecidesThePoolsCallReadAsATestOpensBetweenTests.
func TestALearnStepWaitsForTheStoreToOpenTheTestTheTrackerBegan(t *testing.T) {
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	states := []struct {
		name string
		at   func(p *Proxy, mm *MockManager)
	}{
		{"the store still shows test-1's view", func(p *Proxy, mm *MockManager) {
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			p.EndTestErrorCapture()
			p.ContinueTestErrorCapture()
		}},
	}
	ahead := func(t *testing.T, p *Proxy, mm *MockManager) {
		t.Helper()
		snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
		if !ok || !snap.TrackerAhead() || snap.PoolsCall() {
			t.Fatalf("the round's snapshot does not read the tracker ahead of test-1's view: %+v", snap.Boundary)
		}
	}
	// waitThen runs resolve once the call waits for the store, and fails t
	// where the call returned without waiting.
	waitThen := func(t *testing.T, mm *MockManager, resolve func()) func() {
		var waited atomic.Bool
		mm.exact.round.onAwaitMove = func() {
			if waited.CompareAndSwap(false, true) {
				resolve()
			}
		}
		return func() {
			t.Helper()
			mm.exact.round.onAwaitMove = nil
			if !waited.Load() {
				t.Fatal("the call was decided without waiting for the store to open the test the tracker began")
			}
		}
	}
	outcome := roundOutcome
	for _, routed := range []bool{false, true} {
		for _, st := range states {
			for _, body := range []string{`{"item":"plum"}`, `{"item":"fig"}`, `{"item":"pear"}`} {
				name := map[bool]string{false: "today's rules", true: "proven routing"}[routed] + "/" + st.name + "/" + body
				t.Run(name+"/the store opens test-2", func(t *testing.T) {
					open := func(mm *MockManager) {
						mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
					}
					// The store opens test-2 before the call.
					p, mm := heldOrders(t, routed)
					st.at(p, mm)
					open(mm)
					code, got := r3Send(t, p, mm, opts, roundPost(body))
					before := outcome(t, p, mm, code, got)
					// The call first: it waits, and the store opens test-2.
					p, mm = heldOrders(t, routed)
					st.at(p, mm)
					ahead(t, p, mm)
					done := waitThen(t, mm, func() { open(mm) })
					code, got = r3Send(t, p, mm, opts, roundPost(body))
					done()
					if waited := outcome(t, p, mm, code, got); waited != before {
						t.Fatalf("opened as the call waited: %s; opened before the call: %s: two verdicts by where the scheduler put the call", waited, before)
					}
					if body == `{"item":"plum"}` && before != `200 {"order":2}, learned onto , misses []` {
						t.Fatalf("a call exactly test-2's own mock-own2, decided in test-2's round, reads %s; want it answered from mock-own2", before)
					}
				})
				for _, end := range []struct {
					name  string
					close func(t *testing.T, p *Proxy, mm *MockManager)
					why   string
				}{
					{"the tracker ends test-2", func(_ *testing.T, p *Proxy, _ *MockManager) { p.EndTestErrorCapture() }, "before keploy could answer it"},
					{"the tracker begins test-3", func(_ *testing.T, p *Proxy, _ *MockManager) { p.ContinueTestErrorCapture() }, "before keploy could answer it"},
					// The store's end of test-2 for test-3's view, the
					// tracker still on test-2, reads a round of no one test:
					// read as the store catching up, the call would take
					// that round's noOne miss ("after keploy's store ended
					// test-2 ..."), not its own round's move.
					{"the store opens test-2, then ends it alone for test-3's view", func(t *testing.T, p *Proxy, mm *MockManager) {
						mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
						mm.ServeExactTest("test-3", []*models.Mock{roundMock(mm, "mock-own3")})
						if snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker}); !ok || snap.NoOneRound() == "" || snap.TrackerAhead() {
							// Errorf: the call's goroutine runs the hook.
							t.Errorf("the round's snapshot does not read the store's end of test-2 alone: %+v", snap.Boundary)
						}
					}, "in the test then running, and the replay ended that test before keploy could answer it"},
				} {
					t.Run(name+"/"+end.name, func(t *testing.T) {
						p, mm := heldOrders(t, routed)
						st.at(p, mm)
						ahead(t, p, mm)
						done := waitThen(t, mm, func() { end.close(t, p, mm) })
						code, got := r3Send(t, p, mm, opts, roundPost(body))
						done()
						misses := roundMiss(t, p, mm, code, got, end.why)
						for _, m := range misses {
							if strings.Contains(m.NextSteps, "which keploy recorded where this replay did not make it") {
								t.Fatalf("the moved miss of a call the tracker's test made names a recording made elsewhere: %s", m.NextSteps)
							}
						}
					})
				}
			}
		}
	}
}

// A call waiting for the round to move (the learn tracker ahead of a store
// still showing test-1's view: MockManager.AwaitRoundMove) is woken by every
// move of the round, through no clock: the store opening test-2, the
// tracker's end of test-2, its begin of test-3, a withdrawn begin, a new
// test set, a staging, each reading the round moved, and the manager's
// close, which moves it no more and reads it unmoved. A round that moved
// between the call's snapshot and its wait is read moved at once, the wait
// never begun (it would wait for a move that came before it). The learn
// step's call woken so is decided in test-2's round where the store opened
// test-2, and otherwise misses with nothing learned or answered, naming no
// recording made elsewhere. A move that wakes no waiter fails the row
// through no clock: the waiter's channel still open right after the move
// (stillAwaitsMove) cancels its context, the one way left to end it.
func TestARoundWaiterIsWokenByEveryMoveOfTheRound(t *testing.T) {
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	ahead := func(t *testing.T) (*Proxy, *MockManager, integrations.RoundAt) {
		t.Helper()
		p, mm := heldOrders(t, true)
		mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
		p.ContinueTestErrorCapture()
		p.EndTestErrorCapture()
		p.ContinueTestErrorCapture()
		snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
		if !ok || !snap.TrackerAhead() || snap.PoolsCall() {
			t.Fatalf("precondition: the round's snapshot does not read the tracker ahead of test-1's view: %+v", snap.Boundary)
		}
		return p, mm, snap.At
	}
	for _, mv := range []struct {
		name  string
		move  func(p *Proxy, mm *MockManager)
		moved bool
		why   string
	}{
		{"the store opens test-2", func(p *Proxy, mm *MockManager) {
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
		}, true, ""},
		{"the tracker ends test-2", func(p *Proxy, mm *MockManager) { p.EndTestErrorCapture() }, true, "before keploy could answer it"},
		{"the tracker begins test-3", func(p *Proxy, mm *MockManager) { p.ContinueTestErrorCapture() }, true, "before keploy could answer it"},
		{"the replay withdraws test-2's begin", func(p *Proxy, mm *MockManager) { p.WithdrawTestErrorCapture() }, true, "before keploy could answer it"},
		{"the tracker begins a new test set", func(p *Proxy, mm *MockManager) {
			p.learnBoundary(func(t *learn.Tracker) { t.NewSet() })
		}, true, "before keploy could answer it"},
		{"the replay stages the set again", func(p *Proxy, mm *MockManager) {
			mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now().Add(time.Hour))
		}, true, "before keploy could answer it"},
		{"the manager closes", func(p *Proxy, mm *MockManager) { mm.Close() }, false, "of no round keploy can decide it in"},
	} {
		t.Run(mv.name+"/as the call waits", func(t *testing.T) {
			p, mm, at := ahead(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var waited atomic.Bool
			mm.exact.round.onAwaitMove = func() {
				if waited.CompareAndSwap(false, true) {
					mv.move(p, mm)
					if stillAwaitsMove(mm) {
						t.Errorf("%s did not wake the call waiting for the round to move", mv.name)
						cancel()
					}
				}
			}
			moved, err := mm.AwaitRoundMove(ctx, at)
			mm.exact.round.onAwaitMove = nil
			if err != nil || !waited.Load() || moved != mv.moved {
				t.Fatalf("a waiter the round moved under (%s) read moved %v (err %v, waited %v); want moved %v", mv.name, moved, err, waited.Load(), mv.moved)
			}
		})
		t.Run(mv.name+"/before the call waits", func(t *testing.T) {
			p, mm, at := ahead(t)
			mv.move(p, mm)
			var waited atomic.Bool
			mm.exact.round.onAwaitMove = func() {
				// Ends the wait, so the row fails rather than hangs.
				if waited.CompareAndSwap(false, true) {
					mm.Close()
				}
			}
			moved, err := mm.AwaitRoundMove(context.Background(), at)
			mm.exact.round.onAwaitMove = nil
			if err != nil || waited.Load() || moved != mv.moved {
				t.Fatalf("a waiter whose round moved (%s) before it waited read moved %v (err %v, waited %v); want moved %v at once", mv.name, moved, err, waited.Load(), mv.moved)
			}
		})
		t.Run(mv.name+"/the learn step's call", func(t *testing.T) {
			p, mm, _ := ahead(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var waited atomic.Bool
			mm.exact.round.onAwaitMove = func() {
				if waited.CompareAndSwap(false, true) {
					mv.move(p, mm)
					if stillAwaitsMove(mm) {
						t.Errorf("%s did not wake the learn step's call waiting for the round to move", mv.name)
						cancel()
					}
				}
			}
			code, got := r3SendIn(t, ctx, p, mm, opts, roundPost(`{"item":"plum"}`))
			mm.exact.round.onAwaitMove = nil
			if !waited.Load() {
				t.Fatalf("the call was decided (%d %s) without waiting for the round to move", code, got)
			}
			if mv.why == "" {
				if code != http.StatusOK || got != `{"order":2}` {
					t.Fatalf("the call woken by the store opening test-2 got %d %s; want test-2's own mock-own2", code, got)
				}
				return
			}
			for _, m := range roundMiss(t, p, mm, code, got, mv.why) {
				if strings.Contains(m.NextSteps, "which keploy recorded where this replay did not make it") {
					t.Fatalf("the miss of a call woken by %s names a recording made elsewhere: %s", mv.name, m.NextSteps)
				}
			}
		})
	}
}

// stillAwaitsMove reports whether a call still waits for the round to move:
// the channel its wait began on (AwaitRoundMove) not closed, which a move
// does as it wakes its waiters. Read right after a move, with one waiter, so
// no clock decides it.
func stillAwaitsMove(mm *MockManager) bool {
	r := &mm.exact.round
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.moveCh != nil
}

// roundOutcome is a call's answer, what was learned, and each miss's closest
// recording and whether it names one made elsewhere: the mark the call is
// decided with (its round's own recordings) is part of the verdict.
func roundOutcome(t *testing.T, p *Proxy, mm *MockManager, code int, got string) string {
	t.Helper()
	learned := []string{}
	for name, st := range r3States(mm) {
		if len(st.ReqBodyNoise) > 0 {
			learned = append(learned, name)
		}
	}
	sort.Strings(learned)
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	told := []string{}
	for _, m := range misses {
		told = append(told, m.ClosestMock+" made elsewhere "+strconv.FormatBool(strings.Contains(m.NextSteps, "which keploy recorded where this replay did not make it")))
	}
	return strconv.Itoa(code) + " " + got + ", learned onto " + strings.Join(learned, ",") + ", misses [" + strings.Join(told, "; ") + "]"
}

// A call read where the learn tracker began test-2 while keploy's store still
// shows its pool view after test-1 is the pool's
// (integrations.RoundSnapshot.PoolsCall): under exact serving that is keploy
// test's begin of every test, whose view the agent installs only once every
// call made before is answered, this one among them, and only then gives the
// application test-2's request. The call is decided at once, in the round
// between tests, and never waits for the store to open test-2 (a wait would
// hold the install it waits for: TestKeployTestOpensATestCaseWithAPoolCallReadInItsGap
// in the agent's package). Its verdict is the one of the same call read in
// the round between tests, before the tracker began test-2, but that its
// misses name no recording made elsewhere (its mark holds every recording:
// nothing tells which are test-2's own): mock-pool answers a call that is
// exactly it, and a call that is exactly test-2's own mock-own2, which the
// pool view does not offer, is not answered from mock-own2 (a miss naming
// none made elsewhere:
// TestALearnStepCaughtUpWithTheStoreMarksTheCallInTheTestItOpened). Under
// today's rules and proven routing.
func TestALearnStepDecidesThePoolsCallReadAsATestOpensBetweenTests(t *testing.T) {
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	between := func(p *Proxy, mm *MockManager) {
		mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
		p.ContinueTestErrorCapture()
		p.EndTestErrorCapture()
		mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	}
	for _, routed := range []bool{false, true} {
		for _, body := range []string{`{"item":"apple"}`, `{"item":"plum"}`, `{"item":"pear"}`, `{"item":"fig"}`} {
			t.Run(map[bool]string{false: "today's rules", true: "proven routing"}[routed]+"/"+body, func(t *testing.T) {
				p, mm := heldOrders(t, routed)
				between(p, mm)
				code, got := r3Send(t, p, mm, opts, roundPost(body))
				want := strings.ReplaceAll(roundOutcome(t, p, mm, code, got), "made elsewhere true", "made elsewhere false")

				p, mm = heldOrders(t, routed)
				between(p, mm)
				p.ContinueTestErrorCapture()
				snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
				if !ok || !snap.PoolsCall() {
					t.Fatalf("the round's snapshot does not read the tracker ahead of the store's pool view: %+v", snap.Boundary)
				}
				var waited atomic.Bool
				mm.exact.round.onAwaitMove = func() {
					// Ends the wait, so the row fails rather than hangs.
					if waited.CompareAndSwap(false, true) {
						mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
					}
				}
				code, got = r3Send(t, p, mm, opts, roundPost(body))
				mm.exact.round.onAwaitMove = nil
				if waited.Load() {
					t.Fatal("the pool's call read as test-2 opens waited for the store to open test-2")
				}
				if gap := roundOutcome(t, p, mm, code, got); gap != want {
					t.Fatalf("the pool's call read as test-2 opens: %s; read between tests: %s", gap, want)
				}
				if body == `{"item":"apple"}` && want != `200 {"order":0}, learned onto , misses []` {
					t.Fatalf("a call exactly mock-pool, read in the round between tests: %s; want it answered from mock-pool", want)
				}
				if body == `{"item":"plum"}` && strings.Contains(want, `{"order":2}`) {
					t.Fatalf("the pool's call exactly test-2's own mock-own2 was answered from it: %s", want)
				}

			})
		}
	}
}

// A call read while the learn tracker is ahead of a store still showing
// test-1's view holds every recording as its own (integrations.HoldsEvery)
// until the store opens the tracker's test; caught up there, it is decided
// in that test's round with that round's mark, read again: a call that is
// exactly test-1's own recording (GET /status, which test-2 never made)
// misses in test-2's round naming it as made elsewhere, the same as a call
// read once the store opened test-2. Kept to the first pass's mark, it named
// nothing: two verdicts by where the scheduler put the call. The tracker
// ahead of the store's pool view reads the pool's call
// (integrations.RoundSnapshot.PoolsCall): it waits for no install, and is
// decided in the round between tests with its mark holding every recording,
// so its miss of test-1's own GET /status, which names it made elsewhere
// read between tests, names none.
func TestALearnStepCaughtUpWithTheStoreMarksTheCallInTheTestItOpened(t *testing.T) {
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	status := "GET /status HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"
	staged := func(t *testing.T, routed bool) (*Proxy, *MockManager) {
		t.Helper()
		t.Setenv(provenswitch.Env, "1")
		p := startedDrainProxy(t, 100)
		p.learnTracker = learn.NewTracker()
		now := time.Now()
		pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
		own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "GET", "/status", "", `{"up":1}`)
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
	outcome := func(t *testing.T, p *Proxy, code int, got string) string {
		t.Helper()
		misses, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		told := []string{}
		for _, m := range misses {
			told = append(told, m.ClosestMock+" made elsewhere "+strconv.FormatBool(strings.Contains(m.NextSteps, "which keploy recorded where this replay did not make it")))
		}
		return strconv.Itoa(code) + " " + got + ", misses [" + strings.Join(told, "; ") + "]"
	}
	for _, routed := range []bool{false, true} {
		for _, st := range []struct {
			name string
			at   func(p *Proxy, mm *MockManager)
		}{
			{"the store still shows test-1's view", func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				p.EndTestErrorCapture()
				p.ContinueTestErrorCapture()
			}},
		} {
			t.Run(map[bool]string{false: "today's rules", true: "proven routing"}[routed]+"/"+st.name, func(t *testing.T) {
				open := func(mm *MockManager) { mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")}) }
				p, mm := staged(t, routed)
				st.at(p, mm)
				open(mm)
				code, got := r3Send(t, p, mm, opts, status)
				before := outcome(t, p, code, got)
				if !strings.Contains(before, "mock-own1 made elsewhere true") {
					t.Fatalf("precondition: test-1's own GET /status, read in test-2's round, reads %s; want it named as made elsewhere", before)
				}
				p, mm = staged(t, routed)
				st.at(p, mm)
				if snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker}); !ok || !snap.TrackerAhead() || snap.PoolsCall() {
					t.Fatalf("precondition: the round's snapshot does not read the tracker ahead of test-1's view: %+v", snap.Boundary)
				}
				var waited atomic.Bool
				mm.exact.round.onAwaitMove = func() {
					if waited.CompareAndSwap(false, true) {
						open(mm)
					}
				}
				code, got = r3Send(t, p, mm, opts, status)
				mm.exact.round.onAwaitMove = nil
				if !waited.Load() {
					t.Fatalf("the call was decided (%d %s) without waiting for the store to open test-2", code, got)
				}
				if after := outcome(t, p, code, got); after != before {
					t.Fatalf("caught up as the call waited: %s; read once the store opened test-2: %s: two verdicts by where the scheduler put the call", after, before)
				}
			})
		}
		t.Run(map[bool]string{false: "today's rules", true: "proven routing"}[routed]+"/the store shows its pool view after test-1", func(t *testing.T) {
			between := func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				p.EndTestErrorCapture()
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			}
			p, mm := staged(t, routed)
			between(p, mm)
			code, got := r3Send(t, p, mm, opts, status)
			read := outcome(t, p, code, got)
			if !strings.Contains(read, "mock-own1 made elsewhere true") {
				t.Fatalf("precondition: test-1's own GET /status, read between tests, reads %s; want it named as made elsewhere", read)
			}
			p, mm = staged(t, routed)
			between(p, mm)
			p.ContinueTestErrorCapture()
			if snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker}); !ok || !snap.PoolsCall() {
				t.Fatalf("precondition: the round's snapshot does not read the tracker ahead of the store's pool view: %+v", snap.Boundary)
			}
			var waited atomic.Bool
			mm.exact.round.onAwaitMove = func() {
				// Ends the wait, so the row fails rather than hangs.
				if waited.CompareAndSwap(false, true) {
					mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
				}
			}
			code, got = r3Send(t, p, mm, opts, status)
			mm.exact.round.onAwaitMove = nil
			if waited.Load() {
				t.Fatalf("the pool's call waited for the store to open test-2 (%d %s)", code, got)
			}
			if gap := outcome(t, p, code, got); code == http.StatusOK || strings.Count(gap, " made elsewhere ") != 1 || strings.Contains(gap, "made elsewhere true") {
				t.Fatalf("the pool's call read as test-2 opens: %s; want one miss naming no recording made elsewhere (between tests: %s)", gap, read)
			}
		})
	}
}

// A take refused as the store installed another view under it is no
// catch-up where the tracker did not lead: between tests the store serving
// another pool view (its recordings may be others), a staging of the set
// again, and a store that opened test-1 before the tracker began it opening
// test-2 over it, the tracker still between tests (the store led, and moved
// on from the view the call was read in to one the tracker never agreed
// on). The call misses, saying what moved, and is learned onto nothing;
// read again, it was paired with mock-pool between tests and its item
// learned there as noise, or, exactly test-1's own mock-own1, decided in
// test-2's view, a round it was not made in.
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
		{"the store opened test-1 before the tracker began it, then test-2", `{"item":"pear"}`,
			"while keploy's store showed test-1's view, with no test open on its learn tracker", "ended that test",
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			},
			func(p *Proxy, mm *MockManager) {
				mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
			}},
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
// capture window and before the store installed test-2's view (the replay's
// begin of every test: the tracker begins the test, its test case's request
// is read, the agent answers every call made before, then the store opens
// it) is the pool's (RoundSnapshot.PoolsCall): no call there is test-2's,
// since the application gets test-2's request only after the install. Made
// before its request, it waits for the request, never for the store (whose
// install, in keploy test's flow, waits for the call), and is decided in the
// round between tests, with one verdict whether the request came before the
// call or as it waited: the background rules learn a drifted item onto the
// pool between tests (q books), and refuse it where the test case's request
// holds the recorded word (q apple). Before, such a call was learned onto the pool as
// one between tests, or missed, by whether the store's install landed
// before its pass. A call read once the store opened test-2 is test-2's: the
// pool answers exactly or not at all in a test
// (TestALearnStepsClaimLandsOnlyInTheRoundItWasReadIn), so it misses and
// nothing is learned. A store that opens test-2 as the pool's call waits for
// its request (a flow without keploy test's settle) moved the round between
// tests under it: the moved miss. Ordered through the round's and the
// tracker's wait hooks, through no clock, 200 times each.
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
	install := func(mm *MockManager) { mm.ServeExactTest("test-2", nil) }
	want := map[string]string{"books": "200 {\"order\":0}, learned true", "apple": "502 keploy: no matching mock found for this HTTP request\n, learned false"}
	for _, q := range []string{"books", "apple"} {
		for i := 0; i < 200; i++ {
			// The request first, then the call, in the gap.
			p, mm := gapRound(t)
			if !awaiting(mm, p) {
				t.Fatal("the round's snapshot does not say test-2 awaits its request after its capture window opened")
			}
			p.TakeTestRequest(request(q))
			if awaiting(mm, p) {
				t.Fatal("the round's snapshot says test-2 awaits its request after the request came")
			}
			if snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker}); !ok || !snap.PoolsCall() {
				t.Fatalf("precondition: the round's snapshot does not read the pool's call in test-2's gap: %+v", snap.Boundary)
			}
			// No install comes: a call that waits for the store would wait
			// for good, so its wait ends the call, and the row fails.
			ctx, cancel := context.WithCancel(context.Background())
			mm.exact.round.onAwaitMove = func() {
				t.Errorf("q %s, the pool's call made after its request in test-2's gap waited for the store", q)
				cancel()
			}
			code, got := r3SendIn(t, ctx, p, mm, opts, call)
			mm.exact.round.onAwaitMove = nil
			cancel()
			first := outcome(mm, code, got)
			if first != want[q] {
				t.Fatalf("q %s, the request before the call in the gap: %s; want %s, decided in the round between tests", q, first, want[q])
			}
			for _, order := range []string{"the request as the call waits for it", "the install as the call waits for its request"} {
				p, mm = gapRound(t)
				var storeWait, requestWait atomic.Bool
				mm.exact.round.onAwaitMove = func() {
					// Ends the wait, so the row fails rather than hangs.
					if storeWait.CompareAndSwap(false, true) {
						install(mm)
					}
				}
				p.learnTracker.OnAwait(func() {
					if requestWait.CompareAndSwap(false, true) {
						if order == "the install as the call waits for its request" {
							install(mm)
						}
						p.TakeTestRequest(request(q))
					}
				})
				code, got = r3Send(t, p, mm, opts, call)
				mm.exact.round.onAwaitMove = nil
				p.learnTracker.OnAwait(nil)
				if storeWait.Load() || !requestWait.Load() {
					t.Fatalf("q %s, %s: the pool's call in the gap waited for the store (%v), or not for its request (%v): %d %s", q, order, storeWait.Load(), requestWait.Load(), code, got)
				}
				if order == "the install as the call waits for its request" {
					heldMiss(t, p, mm, code, got, "in the round between tests, as keploy's learn tracker began the next test", "began a test")
					continue
				}
				if waited := outcome(mm, code, got); waited != first {
					t.Fatalf("q %s: %s: %s; the request before the call: %s: two outcomes by where the call fell", q, order, waited, first)
				}
			}
			// The request and the install first: a call of test-2's.
			p, mm = gapRound(t)
			p.TakeTestRequest(request(q))
			install(mm)
			code, got = r3Send(t, p, mm, opts, call)
			if after := outcome(mm, code, got); strings.HasPrefix(after, "200 ") || !strings.HasSuffix(after, ", learned false") {
				t.Fatalf("q %s, read once the store opened test-2: %s; want a miss in test-2's round, nothing learned", q, after)
			}
		}
	}
}

// unstagedSet leaves p and mm, after test-1 of a set ended (the store on its
// pool view, keploy test's flow, or still on test-1's view), with the learn
// tracker in the next test set (keploy test's Mock of it: NewSet) and that
// set not staged, its round's snapshot reading so (SetUnstaged), neither the
// tracker ahead (TrackerAhead) nor the pool's call (PoolsCall).
func unstagedSet(t *testing.T, view string) (*Proxy, *MockManager) {
	t.Helper()
	return unstagedSetLogging(t, view, zap.NewNop())
}

// unstagedSetLogging is unstagedSet with the logger the row reads.
func unstagedSetLogging(t *testing.T, view string, logger *zap.Logger) (*Proxy, *MockManager) {
	t.Helper()
	p, mm := heldOrdersLogging(t, true, logger)
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
	p.ContinueTestErrorCapture()
	p.EndTestErrorCapture()
	if view == "pool" {
		mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	}
	p.learnBoundary(func(t *learn.Tracker) { t.NewSet() })
	snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
	if !ok || !snap.SetUnstaged() || snap.TrackerAhead() || snap.PoolsCall() || snap.NoOneRound() == "" || snap.Boundary != "the learn tracker began a test set the store has not staged" {
		t.Fatalf("precondition: the round's snapshot does not read a test set the store has not staged: %q unstaged %v ahead %v pools %v", snap.Boundary, snap.SetUnstaged(), snap.TrackerAhead(), snap.PoolsCall())
	}
	return p, mm
}

// A call read after keploy test's Mock of the next test set began a set on
// the learn tracker (NewSet) and before that set's staging is of no set the
// store holds: the store still shows the set before's staging, its pool view
// (keploy test's flow: the set's last test ended) or a test's view. The
// round's snapshot reads a set the store has not staged (SetUnstaged): the
// call is decided at once, waiting for nothing (a wait would end at the
// staging, racing its cut of the misses made before it), as the staging note
// says of a call the application makes between test sets, outside every
// test, nothing learned onto the set before's recordings and none named
// made elsewhere. Before, on the pool view, the call was read as one between
// tests of the set before, its drifted item learned onto that set's
// mock-pool as noise.
func TestACallReadAsTheTrackerBeginsATestSetTheStoreHasNotStagedLearnsNothing(t *testing.T) {
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	for _, view := range []string{"pool", "test-1"} {
		t.Run(view, func(t *testing.T) {
			p, mm := unstagedSet(t, view)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var waited atomic.Bool
			mm.exact.round.onAwaitMove = func() {
				// A wait no move ends: the call is cancelled, and fails.
				waited.Store(true)
				cancel()
			}
			code, got := r3SendIn(t, ctx, p, mm, opts, roundPost(`{"item":"apricot"}`))
			mm.exact.round.onAwaitMove = nil
			if waited.Load() {
				t.Fatalf("the call read in a test set the store has not staged waited (%d %s); want it decided at once", code, got)
			}
			for _, m := range roundMiss(t, p, mm, code, got, "keploy record's full round read this request as keploy's learn tracker began a test set its store had not staged") {
				if !strings.HasPrefix(m.NextSteps, stagingMissNote) || m.Kind != models.MissOutsideTest {
					t.Errorf("the miss reads %s %q; want it %s, led by the staging note", m.Kind, m.NextSteps, models.MissOutsideTest)
				}
				if strings.Contains(m.NextSteps, "which keploy recorded where this replay did not make it") {
					t.Fatalf("the miss names a recording made elsewhere: %s", m.NextSteps)
				}
			}
		})
	}
}

// A test set the store has not staged is never the tracker ahead, even with
// a test of it begun before its staging (no keploy test flow does so): no
// call read there waits for the store or is the pool's, every one is of no
// round (NoOneRound), and its mark is a staging's, outside every test.
func TestATestSetTheStoreHasNotStagedIsNeverTheTrackerAhead(t *testing.T) {
	for _, open := range []bool{false, true} {
		for _, test := range []string{"", "test-1"} {
			s := integrations.RoundSnapshot{Phase: integrations.RoundBoundary, Test: test, Learn: integrations.LearnRead{Bound: true, Number: 2, Open: open, SetAhead: true}}
			c := s.CallRound()
			if !s.SetUnstaged() || s.TrackerAhead() || s.PoolsCall() || s.NoOneRound() == "" || !c.Staging() || c.InTest() || !c.Held("any") {
				t.Errorf("open %v, view %q: unstaged %v ahead %v pools %v no one %q, mark staging %v in a test %v", open, test, s.SetUnstaged(), s.TrackerAhead(), s.PoolsCall(), s.NoOneRound(), c.Staging(), c.InTest())
			}
		}
	}
}

// The staging of the set the learn tracker began stages it (SetUnstaged no
// longer reads): a call the new set's application makes then is the new
// set's, read in its start-up, answered from its recordings, not refused as
// one made between test sets.
func TestACallAfterTheTrackersNewSetIsStagedIsTheNewSets(t *testing.T) {
	p, mm := unstagedSet(t, "pool")
	now := time.Now()
	pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
	mm.SetMocksWithWindow([]*models.Mock{pool}, nil, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
	if !ok || snap.SetUnstaged() || snap.Learn.SetAhead || snap.NoOneRound() != "" {
		t.Fatalf("the staging left the tracker's set unstaged: %q unstaged %v", snap.Boundary, snap.SetUnstaged())
	}
	if call := mm.MarkCallRound(); call.Staging() {
		t.Fatalf("a call marked after the new set's staging is marked as made in a staging")
	}
	if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"apple"}`)); code != http.StatusOK || !strings.Contains(got, `"order":0`) {
		t.Fatalf("the new set's call got %d %s; want its pool's answer", code, got)
	}
}

// A fresh agent's first test set is the set its staging stages, though
// keploy test's Mock begins that set on the learn tracker (NewSet) before the
// proxy holds a mock manager, so the set's start reaches the tracker alone
// (learnBoundary) and no manager's binding: the staging reads the tracker's
// own set (SetTrackerSetAt). The set's first test then reads in that test,
// never as a set the store has not staged, and a call there is learned from
// that test's own recording. A replacement agent the replay re-registers its
// set's mocks with (ensureAgentHoldsStoredMocks: a Mock without
// StartsTestSet) starts the same way. Before, the staging stored the
// manager's binding of the tracker's set (none: no boundary had reached it),
// the first routed begin bound the tracker's set, and from the set's first
// test on every snapshot and mark read the set unstaged: each learn call of
// record's full round missed as of no round, and each miss of a plain proven
// replay was filed outside every test with the staging note.
func TestAFreshAgentsFirstTestSetIsTheSetItsStagingStaged(t *testing.T) {
	for name, starts := range map[string]bool{"a test set's start": true, "a replacement agent's repair": false} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := New(zap.NewNop(), nil, &config.Config{})
			p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			p.StartErrorDrain(ctx)
			if p.getMockManager() != nil {
				t.Fatal("precondition: a fresh proxy holds a mock manager")
			}
			if err := p.Mock(ctx, models.OutgoingOptions{StartsTestSet: starts, Learn: true}); err != nil {
				t.Fatal(err)
			}
			mm := p.getMockManager()
			t.Cleanup(func() { p.setMockManager(nil) })
			now := time.Now()
			pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
			own := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
			if err := p.SetMocksWithWindow(ctx, []*models.Mock{pool, own}, nil, models.BaseTime, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			p.ContinueTestErrorCapture()
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
			if !ok || snap.Phase != integrations.RoundInTest || !snap.Open || snap.SetUnstaged() || snap.Learn.SetAhead || snap.NoOneRound() != "" {
				t.Errorf("test-1 of a fresh agent's first set reads %v (open %v, unstaged %v: %q); want in test-1", snap.Phase, snap.Open, snap.SetUnstaged(), snap.NoOneRound())
			}
			if c := mm.MarkCallRound(); !c.Read() || c.Staging() || !c.InTest() || c.Held("mock-pool") {
				t.Errorf("a call marked in test-1 of a fresh agent's first set reads staging %v, in a test %v, holding mock-pool %v; want test-1's", c.Staging(), c.InTest(), c.Held("mock-pool"))
			}
			code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, roundPost(`{"item":"fig"}`))
			if code != http.StatusOK || got != `{"order":1}` || len(r3States(mm)["mock-own1"].ReqBodyNoise) == 0 {
				t.Fatalf("test-1's drifted call of a fresh agent's first set got %d %s, mock-own1 learned %v; want it learned from mock-own1", code, got, r3States(mm)["mock-own1"].ReqBodyNoise)
			}
		})
	}
}

// The manager's mark of a call (MarkCallRound: the generic, MySQL and
// Postgres parsers' order) in a test set the store has not staged is the
// snapshot's (RoundSnapshot.CallRound): a staging's, outside every test,
// holding every recording, so the miss says it was refused as the set's
// mocks were staged, filed outside every test, and names no recording made
// elsewhere, on the pool view and on test-1's alike.
func TestTheManagersMarkInATestSetTheStoreHasNotStagedIsAStagings(t *testing.T) {
	for _, view := range []string{"pool", "test-1"} {
		t.Run(view, func(t *testing.T) {
			p, mm := unstagedSet(t, view)
			snap, _ := mm.RoundSnapshot(integrations.RoundQuery{Tracker: p.learnTracker})
			for name, call := range map[string]integrations.CallRound{"the manager's mark": mm.MarkCallRound(), "the snapshot's": snap.CallRound()} {
				if !call.Read() || !call.Staging() || call.InTest() || !call.Held("mock-own2") || !call.Held("mock-pool") {
					t.Errorf("%s reads staging %v, in a test %v, holding mock-own2 %v; want a staging's, outside every test, holding every recording", name, call.Staging(), call.InTest(), call.Held("mock-own2"))
				}
			}
		})
	}
}

// A miss whose call was marked in a test set the store had not staged is
// counted the same whichever way its report and the set's staging fall: its
// report reaching the drain before the staging (the set start's cut drops
// it) or after it (the drain drops it, its mark's staging before the set's),
// said either way, and the set's first test reads none of it. A miss marked
// after the staging is the set's either way (made before its first test).
// Before, a report the drain filed after the staging's cut was read as the
// set's, failing it: whether the call counted rested on the scheduler.
func TestAMissMarkedBeforeASetsStagingIsCountedTheSameEitherWay(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "the report before the staging", true: "the report after the staging"}[after], func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			p, mm := unstagedSetLogging(t, "pool", zap.New(core))
			p.dnsCache = newDNSCache()
			p.missCutPending.Store(true) // the set's Mock
			miss := func(summary string, call integrations.CallRound) models.ParserError {
				e := mockMiss(summary)
				integrations.StampMark(e.MismatchReport, call)
				return e
			}
			early := miss("POST /between-sets", mm.MarkCallRound())
			if !after {
				p.SendError(early)
				flushDrain(p)
			}
			if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if after {
				p.SendError(early)
			}
			p.SendError(miss("insert on events", mm.MarkCallRound()))
			flushDrain(p)
			p.BeginTestErrorCapture()
			got, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].ActualSummary != "insert on events" || !got[0].BeforeSet {
				t.Fatalf("the set's first test read %+v; want only the miss marked after its staging, as the set's", got)
			}
			if n := logs.FilterMessage(missesCutAtSetStart).FilterField(zap.Strings("calls", []string{"POST /between-sets"})).Len(); n != 1 {
				t.Fatalf("the cut of the miss marked before the staging was said %d times; want once: %+v", n, logs.All())
			}
		})
	}
}

// A call read between tests (the store's pool view, the learn tracker's
// test ended) that is exactly a recording the round does not offer, the
// test before's own (test-1's mock-own1, its late call, whether or not
// mock-own1 answered test-1 first) or the next test's (test-2's mock-own2,
// an early call), is that recording's call, made where it was not recorded:
// keploy learns nothing from it. Before, it was paired with the pool's
// mock-pool, the only recording of POST /orders the pool view offers, and
// answered from it, its item learned onto mock-pool as noise: a false green
// (any item then passed for mock-pool). Now it is answered from its own
// recording only by the pairing (a call that is exactly another test's
// recording, which this set's pairing offers none of), or misses naming
// that recording: as made elsewhere (the miss's lookup, for record's
// judging), or, once the recording answered already, by the learn step,
// which says the recording's test window. A call that drifted from mock-pool and is no recording is still
// learned onto the pool between tests. Under today's rules and proven
// routing.
func TestABetweenTestsCallThatIsExactlyARecordingElsewhereLearnsNothingOntoThePool(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, rec, answer string
			// answered: mock-own1 answered test-1's call in test-1 first.
			answered bool
		}{
			{"the test before's own", `{"item":"pear"}`, "mock-own1", `{"order":1}`, false},
			{"the test before's own, which answered its test", `{"item":"pear"}`, "mock-own1", `{"order":1}`, true},
			{"the next test's own", `{"item":"plum"}`, "mock-own2", `{"order":2}`, false},
			{"no recording: the pool's drift", `{"item":"fig"}`, "", "", false},
		} {
			name := map[bool]string{false: "today's rules", true: "proven routing"}[routed] + "/" + tc.name
			t.Run(name, func(t *testing.T) {
				p, mm := heldOrders(t, routed)
				own1 := roundMock(mm, "mock-own1")
				own2 := roundMock(mm, "mock-own2")
				mm.SeedRecordedWindows([]models.TestWindow{
					{TestCase: "test-1", Start: own1.Spec.ReqTimestampMock.Add(-200 * time.Millisecond), End: own1.Spec.ReqTimestampMock.Add(100 * time.Millisecond)},
					{TestCase: "test-2", Start: own2.Spec.ReqTimestampMock.Add(-500 * time.Millisecond), End: own2.Spec.ReqTimestampMock.Add(500 * time.Millisecond)},
				})
				opts := models.OutgoingOptions{Learn: true, LearnPool: true}
				mm.ServeExactTest("test-1", []*models.Mock{own1})
				p.ContinueTestErrorCapture()
				if tc.answered {
					if code, got := r3Send(t, p, mm, opts, roundPost(tc.body)); code != http.StatusOK || got != tc.answer {
						t.Fatalf("precondition: test-1's call got %d %s; want %s from mock-own1", code, got, tc.answer)
					}
				}
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				if _, err := p.GetMockErrors(context.Background()); err != nil {
					t.Fatal(err)
				}
				code, got := r3Send(t, p, mm, opts, roundPost(tc.body))
				learned := map[string][]string{}
				for n, st := range r3States(mm) {
					if len(st.ReqBodyNoise) > 0 {
						learned[n] = st.ReqBodyNoise["body"]
					}
				}
				misses, err := p.GetMockErrors(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if tc.rec == "" {
					if _, ok := learned["mock-pool"]; code != http.StatusOK || got != `{"order":0}` || !ok {
						t.Fatalf("between tests the pool's drift was not learned onto mock-pool: answered %d %s, learned %v, misses %+v", code, got, learned, misses)
					}
					return
				}
				if len(learned) > 0 || got == `{"order":0}` {
					t.Fatalf("a call that is exactly %s was paired with mock-pool between tests: answered %d %s, learned %v", tc.rec, code, got, learned)
				}
				if code == http.StatusOK {
					if tc.answered || got != tc.answer || len(misses) != 0 {
						t.Fatalf("a call that is exactly %s was answered %d %s (misses %+v); want it answered so only by the pairing, from %s, which answered nothing yet", tc.rec, code, got, misses, tc.rec)
					}
					return
				}
				if len(misses) != 1 || misses[0].ClosestMock != tc.rec || !strings.Contains(misses[0].NextSteps, tc.rec) {
					t.Fatalf("the miss reads %+v; want one naming %s", misses, tc.rec)
				}
				if tc.answered {
					why := "it is exactly mock-own1, which keploy recorded in test-1's window: keploy reads it as that recording's call made outside where it was recorded (a test's late or early call), not as a changed call of mock-pool"
					if misses[0].Reason != "" || !strings.Contains(misses[0].NextSteps, why) || strings.Contains(misses[0].NextSteps, "Request structure changed") {
						t.Fatalf("the miss of a call that is exactly mock-own1 once it answered test-1 reads %+v; want no reason, saying %q", misses[0], why)
					}
				} else if !strings.Contains(misses[0].NextSteps, "which keploy recorded where this replay did not make it") {
					t.Fatalf("the miss of a call that is exactly %s reads %+v; want it named as made elsewhere", tc.rec, misses[0])
				}
			})
		}
	}
}

// elsewhereOrders stages heldOrders' three recordings of POST /orders
// (mock-pool, the pool's; mock-own1 in test-1's window; mock-own2 in
// test-2's), of which trees picks what the store's trees hold, with more
// beside them, and src, if set, is the agent's disk source (the recordings it
// keeps on disk).
func elsewhereOrders(t *testing.T, routed bool, trees func(pool, own1, own2 *models.Mock) []*models.Mock, src func(own2 *models.Mock) RecordingSource) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxyLogging(t, 100, zap.NewNop())
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
	own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
	own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "test-1", Start: now.Add(500 * time.Millisecond), End: now.Add(800 * time.Millisecond)},
		{TestCase: "test-2", Start: now.Add(time.Second), End: now.Add(2 * time.Second)},
	})
	mm.SetMocksWithWindow(trees(pool, own1, own2), nil, models.BaseTime, now.Add(time.Hour))
	if src != nil {
		mm.SetRecordingSource(src(own2))
	}
	if routed {
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-pool"}, nil)
	}
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	return p, mm
}

// elsewhereSent is what a call was answered and what keploy learned onto
// which recording (its request-body noise), and the misses it left.
func elsewhereSent(t *testing.T, p *Proxy, mm *MockManager, body string) (int, string, map[string][]string, []models.UnmatchedCall) {
	t.Helper()
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(body))
	learned := map[string][]string{}
	for n, st := range r3States(mm) {
		if len(st.ReqBodyNoise) > 0 {
			learned[n] = st.ReqBodyNoise["body"]
		}
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return code, got, learned, misses
}

// elsewhereBetween opens test-1 (its own: own), ends it, and leaves the store
// on its pool view, between tests.
func elsewhereBetween(t *testing.T, p *Proxy, mm *MockManager, own ...string) {
	t.Helper()
	var view []*models.Mock
	for _, name := range own {
		view = append(view, roundMock(mm, name))
	}
	mm.ServeExactTest("test-1", view)
	p.ContinueTestErrorCapture()
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	if _, err := p.GetMockErrors(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A call between tests that is exactly test-1's own recording mock-v1, which
// declares a variable at its item (the layout keploy writes once a learn
// round baked one), whether it carries the recorded item or another (the
// variable's place differs on every run): it is mock-v1's call, made where it
// was not recorded, and learns nothing. Before, a recording that declares
// variables was never compared, the call was paired with mock-pool and its
// item learned onto mock-pool: a false green.
func TestABetweenTestsCallThatIsExactlyAVariablesRecordingLearnsNothingOntoThePool(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, body := range []string{`{"item":"kiwi"}`, `{"item":"lime"}`} {
			t.Run(map[bool]string{false: "today's rules", true: "proven routing"}[routed]+"/"+body, func(t *testing.T) {
				p, mm := elsewhereOrders(t, routed, func(pool, own1, own2 *models.Mock) []*models.Mock {
					v1 := r3Mock("mock-v1", own1.Spec.ReqTimestampMock.Add(-50*time.Millisecond), "POST", "/orders", `{"item":"kiwi"}`, `{"order":9}`)
					v1.Spec.Variables = models.MockVariables{"item": models.MockVariable{Recorded: "kiwi", Bind: "body.item"}}
					return []*models.Mock{pool, own1, own2, v1}
				}, nil)
				elsewhereBetween(t, p, mm, "mock-v1", "mock-own1")
				code, got, learned, misses := elsewhereSent(t, p, mm, body)
				t.Logf("answered %d %s, learned %v, misses %+v", code, got, learned, misses)
				if len(learned) > 0 || got == `{"order":0}` {
					t.Fatalf("a call that is exactly mock-v1 (modulo its variable) was paired with mock-pool between tests: answered %d %s, learned %v", code, got, learned)
				}
				if code == http.StatusOK {
					t.Fatalf("a call that is exactly mock-v1 between tests was answered %d %s; want a miss", code, got)
				}
				if len(misses) != 1 || misses[0].ClosestMock != "mock-v1" || strings.Contains(misses[0].NextSteps, "Request structure changed") {
					t.Fatalf("the miss reads %+v; want one naming mock-v1, with no word of a changed request", misses)
				}
			})
		}
	}
}

// A call between tests that is exactly test-2's own recording mock-own2,
// which the agent keeps on disk only (a test's own are staged only as that
// test opens): the store reads it back by the call's request keys, and the
// call learns nothing. Read from the trees alone, it was paired with mock-pool
// and its item learned onto it: a false green.
func TestABetweenTestsCallThatIsExactlyARecordingKeptOnDiskLearnsNothingOntoThePool(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "today's rules", true: "proven routing"}[routed], func(t *testing.T) {
			p, mm := elsewhereOrders(t, routed, func(pool, own1, own2 *models.Mock) []*models.Mock { return []*models.Mock{pool, own1} },
				func(own2 *models.Mock) RecordingSource { return elsewhereDisk(t, own2).RangeKeys })
			elsewhereBetween(t, p, mm, "mock-own1")
			code, got, learned, misses := elsewhereSent(t, p, mm, `{"item":"plum"}`)
			if len(learned) > 0 || got == `{"order":0}` {
				t.Fatalf("a call that is exactly mock-own2, kept on disk, was paired with mock-pool between tests: answered %d %s, learned %v", code, got, learned)
			}
			if len(misses) != 1 || misses[0].ClosestMock != "mock-own2" || strings.Contains(misses[0].NextSteps, "Request structure changed") {
				t.Fatalf("answered %d %s; the miss reads %+v; want one naming mock-own2, with no word of a changed request", code, got, misses)
			}
		})
	}
}

// elsewhereDisk is the agent's disk store holding ms, as it indexes them
// (models.RequestKeys).
func elsewhereDisk(t *testing.T, ms ...*models.Mock) *DiskMocks {
	t.Helper()
	d := newDiskWith(t, ms...)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// keyedSource is a disk store holding ms that, as DiskMocks.RangeKeys does,
// visits one only when a key asked is one of its models.RequestKeys, and
// reads it back with load.
func keyedSource(load func(*models.Mock) (*models.Mock, error), ms ...*models.Mock) RecordingSource {
	return func(kind models.Kind, keys []string, visit func(string, func() (*models.Mock, error)) bool) error {
		for _, m := range ms {
			if m.Kind != kind || !slices.ContainsFunc(models.RequestKeys(m), func(k string) bool { return slices.Contains(keys, k) }) {
				continue
			}
			if !visit(m.Name, func() (*models.Mock, error) { return load(m) }) {
				return nil
			}
		}
		return nil
	}
}

// A call between tests that is exactly test-2's own recording mock-v2, kept
// on disk only (as per-test recordings are), which declares a variable where
// the exact request key reads: a URL path segment (POST mints an id, the next
// call carries it in its path) or its X-Amz-Target. The call carries a value
// of its own there, so the store files mock-v2 under the learned key, the
// call reads it back, and it learns nothing. Before, mock-v2 was filed under
// its recorded value, no call that carried another asked for it, and the call
// was paired with mock-pool and its item learned onto it: a false green.
func TestABetweenTestsCallThatIsExactlyAVariablesRecordingKeptOnDiskLearnsNothingOntoThePool(t *testing.T) {
	type layout struct {
		path, amz, bind, recorded, sent, sentAmz string
	}
	for name, l := range map[string]layout{
		"path segment": {path: "/orders/abc", bind: "url.path[1]", recorded: "abc", sent: "/orders/qqq"},
		"X-Amz-Target": {path: "/", amz: "Orders.Put", bind: "header.X-Amz-Target", recorded: "Orders.Put", sent: "/", sentAmz: "Orders.Take"},
	} {
		for _, routed := range []bool{false, true} {
			t.Run(name+"/"+map[bool]string{false: "today's rules", true: "proven routing"}[routed], func(t *testing.T) {
				withAmz := func(m *models.Mock, amz string) *models.Mock {
					if amz != "" {
						m.Spec.HTTPReq.Header["X-Amz-Target"] = amz
					}
					return m
				}
				p, mm := elsewhereOrders(t, routed, func(pool, own1, own2 *models.Mock) []*models.Mock {
					return []*models.Mock{withAmz(r3Mock("mock-pool", pool.Spec.ReqTimestampMock, "POST", l.sent, `{"item":"pear"}`, `{"order":0}`), l.sentAmz), own1}
				}, func(own2 *models.Mock) RecordingSource {
					v2 := withAmz(r3Mock("mock-v2", own2.Spec.ReqTimestampMock, "POST", l.path, `{"item":"kiwi"}`, `{"order":9}`), l.amz)
					v2.Spec.Variables = models.MockVariables{"v": models.MockVariable{Recorded: l.recorded, Bind: l.bind}}
					return elsewhereDisk(t, v2).RangeKeys
				})
				elsewhereBetween(t, p, mm, "mock-own1")
				request := "POST " + l.sent + " HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n"
				if l.sentAmz != "" {
					request += "X-Amz-Target: " + l.sentAmz + "\r\n"
				}
				request += "Content-Length: 15\r\n\r\n" + `{"item":"kiwi"}`
				code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, request)
				for n, st := range r3States(mm) {
					if len(st.ReqBodyNoise) > 0 {
						t.Fatalf("a call that is exactly mock-v2 (modulo its variable), kept on disk, was paired between tests: answered %d %s, learned %v onto %s", code, got, st.ReqBodyNoise, n)
					}
				}
				misses, err := p.GetMockErrors(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if code == http.StatusOK || len(misses) != 1 || misses[0].ClosestMock != "mock-v2" || !strings.Contains(misses[0].NextSteps, "test-2's window") {
					t.Fatalf("answered %d %s; the miss reads %+v; want one naming mock-v2, recorded in test-2's window", code, got, misses)
				}
			})
		}
	}
}

// A recording of test-2, kept on disk, whose variable is at a place keploy
// cannot read (a hand edit, a newer keploy's place): the learn step cannot
// tell whether a call of its operation is exactly it, whatever the call
// carries where the place may be, so two such calls between tests learn
// nothing, each misses naming the recording and its place (not every
// recording of the set), and the agent warns of it once.
func TestACallOfARecordingWhoseVariablePlaceKeployCannotReadLearnsNothing(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "today's rules", true: "proven routing"}[routed], func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			p, mm := elsewhereOrders(t, routed, func(pool, own1, own2 *models.Mock) []*models.Mock {
				return []*models.Mock{r3Mock("mock-pool", pool.Spec.ReqTimestampMock, "POST", "/orders/xyz", `{"item":"pear"}`, `{"order":0}`), own1}
			}, func(own2 *models.Mock) RecordingSource {
				v2 := r3Mock("mock-v2", own2.Spec.ReqTimestampMock, "POST", "/orders/abc", `{"item":"kiwi"}`, `{"order":9}`)
				v2.Spec.Variables = models.MockVariables{"v": models.MockVariable{Recorded: "kiwi", Bind: "bogus.item"}}
				return elsewhereDisk(t, v2).RangeKeys
			})
			elsewhereBetween(t, p, mm, "mock-own1")
			h := httpint.New(zap.New(core))
			for _, sent := range []string{"/orders/qqq", "/orders/rrr"} {
				body := `{"item":"fig"}`
				code, got := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true},
					"POST "+sent+" HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
				if code == http.StatusOK {
					t.Fatalf("POST %s, which keploy cannot tell from mock-v2, was answered %d %s", sent, code, got)
				}
			}
			for n, st := range r3States(mm) {
				if len(st.ReqBodyNoise) > 0 {
					t.Fatalf("a call keploy cannot tell from mock-v2 learned %v onto %s", st.ReqBodyNoise, n)
				}
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			const why = `keploy cannot read where the recording mock-v2 declares its variable v ("bogus.item"), so it cannot tell whether this request is exactly mock-v2 rather than a changed call of mock-pool`
			if len(misses) != 2 {
				t.Fatalf("misses %+v; want two", misses)
			}
			for _, m := range misses {
				if !strings.Contains(m.NextSteps, why) || !strings.Contains(m.NextSteps, "This is a recording keploy cannot read") || strings.Contains(m.NextSteps, "every recording") {
					t.Fatalf("the miss reads %+v; want one saying %q, naming the recording keploy cannot read", m, why)
				}
			}
			if n := logs.FilterMessageSnippet("declares a variable at a place keploy cannot read").FilterField(zap.String("recording", "mock-v2")).Len(); n != 1 {
				t.Fatalf("the agent warned %d times of mock-v2's place; want once (%v)", n, logs.All())
			}
		})
	}
}

// A recording the store cannot read back from disk: the learn step cannot
// tell whether a call between tests that drifted from mock-pool is exactly
// it, so it learns nothing, and the miss says keploy could not read its own
// recordings (not that the request changed).
func TestALearnStepThatCannotReadARecordingBackLearnsNothing(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "today's rules", true: "proven routing"}[routed], func(t *testing.T) {
			p, mm := elsewhereOrders(t, routed, func(pool, own1, own2 *models.Mock) []*models.Mock { return []*models.Mock{pool, own1} },
				func(own2 *models.Mock) RecordingSource {
					return keyedSource(func(*models.Mock) (*models.Mock, error) { return nil, errors.New("disk mocks: short read") }, own2)
				})
			elsewhereBetween(t, p, mm, "mock-own1")
			code, got, learned, misses := elsewhereSent(t, p, mm, `{"item":"fig"}`)
			if len(learned) > 0 || code == http.StatusOK {
				t.Fatalf("a call keploy could not tell from a recording it cannot read back was answered %d %s, learned %v", code, got, learned)
			}
			const why = "keploy cannot read every recording of the test set (read the recording mock-own2 kept on disk: disk mocks: short read)"
			if len(misses) != 1 || !strings.Contains(misses[0].NextSteps, why) || !strings.Contains(misses[0].NextSteps, "keploy failing to read its own recordings") ||
				strings.Contains(misses[0].NextSteps, "Request structure changed") {
				t.Fatalf("the miss reads %+v; want one saying %q and that keploy failed to read its own recordings, with no word of a changed request", misses, why)
			}
		})
	}
}

// In a test, a call the learn step would pair with the test's own recording
// is checked against the recordings of EARLIER tests alone: in test-2, a late
// call of test-1 exactly test-1's mock-own1 learns nothing onto mock-own2 and
// misses naming mock-own1; in test-1, a call exactly test-2's mock-own2 (a
// value that drifted onto the next test's, an off-by-one counter, as often
// as an early call: keploy test sends test-2's request only once test-1's
// view ended) keeps its pairing with mock-own1, as before.
func TestAnInTestCallIsCheckedAgainstEarlierTestsRecordingsAlone(t *testing.T) {
	for _, routed := range []bool{false, true} {
		name := map[bool]string{false: "today's rules", true: "proven routing"}[routed]
		t.Run(name+"/in test-2, exactly test-1's own", func(t *testing.T) {
			p, mm := elsewhereOrders(t, routed, func(pool, own1, own2 *models.Mock) []*models.Mock { return []*models.Mock{pool, own1, own2} }, nil)
			elsewhereBetween(t, p, mm, "mock-own1")
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
			p.ContinueTestErrorCapture()
			code, got, learned, misses := elsewhereSent(t, p, mm, `{"item":"pear"}`)
			if len(learned) > 0 || got == `{"order":2}` {
				t.Fatalf("in test-2 a call that is exactly test-1's mock-own1 was paired with mock-own2: answered %d %s, learned %v", code, got, learned)
			}
			if code == http.StatusOK || len(misses) != 1 || misses[0].ClosestMock != "mock-own1" {
				t.Fatalf("answered %d %s, misses %+v; want one miss naming mock-own1", code, got, misses)
			}
		})
		t.Run(name+"/in test-1, exactly test-2's own", func(t *testing.T) {
			p, mm := elsewhereOrders(t, routed, func(pool, own1, own2 *models.Mock) []*models.Mock { return []*models.Mock{pool, own1, own2} }, nil)
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			code, got, learned, misses := elsewhereSent(t, p, mm, `{"item":"plum"}`)
			if _, ok := learned["mock-own1"]; code != http.StatusOK || got != `{"order":1}` || !ok || len(misses) != 0 {
				t.Fatalf("in test-1 a call that is exactly test-2's mock-own2 was answered %d %s, learned %v, misses %+v; want it paired with mock-own1 as before", code, got, learned, misses)
			}
		})
	}
}

// The scoped view forwards the store's staged recordings whatever its
// allowlist shows, those on disk too, and a read-back error; over a store
// that keeps no such reader, it visits the store's trees.
func TestAScopedViewRangesTheStoresStagedRecordings(t *testing.T) {
	now := time.Now()
	pool := r3Mock("mock-pool", now, "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
	own1 := r3Mock("mock-own1", now.Add(time.Second), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
	own2 := r3Mock("mock-own2", now.Add(2*time.Second), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SetMocksWithWindow([]*models.Mock{pool, own1}, nil, models.BaseTime, now.Add(time.Hour))
	var fail error
	mm.SetRecordingSource(func(kind models.Kind, keys []string, visit func(string, func() (*models.Mock, error)) bool) error {
		if kind == models.HTTP && len(keys) > 0 {
			visit(own2.Name, func() (*models.Mock, error) { return own2, fail })
		}
		return nil
	})
	visit := func(db integrations.MockMemDb) (string, error) {
		r, ok := db.(integrations.StagedRecordingReader)
		if !ok {
			t.Fatalf("%T reads no staged recordings", db)
		}
		var names []string
		err := r.RangeStagedRecordings(models.HTTP, []string{"POST /orders"}, func(m *models.Mock) bool { names = append(names, m.Name); return true })
		sort.Strings(names)
		return strings.Join(names, ","), err
	}
	if names, err := visit(&scopedMockDb{MockMemDb: mm}); err != nil || names != "mock-own1,mock-own2,mock-pool" {
		t.Fatalf("through the scoped view: %s %v; want mock-own1, mock-own2 (from disk), mock-pool", names, err)
	}
	type treesOnly struct {
		integrations.MockMemDb
		integrations.StagedReader
	}
	if names, err := visit(&scopedMockDb{MockMemDb: treesOnly{mm, mm}}); err != nil || names != "mock-own1,mock-pool" {
		t.Fatalf("over a store with no reader: %s %v; want its trees, mock-own1 and mock-pool", names, err)
	}
	fail = errors.New("short read")
	if _, err := visit(&scopedMockDb{MockMemDb: mm}); err == nil || !strings.Contains(err.Error(), "mock-own2") {
		t.Fatalf("a recording the store cannot read back gave %v; want an error naming mock-own2", err)
	}
}

// A retry cycle of record's full round (keploy replay's retryPassing) runs
// test-1 again after test-N, so recorded order is not replay order there. A
// late call of test-N read in test-1's window is exactly test-N's recording,
// made after test-1's window, which the learn step reads as a drift of
// test-1's own (elsewhereCounts): paired with it, its item would be learned
// as noise on test-1's recording, a false green. Once the set's learn step
// has ended (MockManager.SetLearnEnded, from the cycle's filters) the call is
// served as keploy test serves: it keeps no pairing with test-1's own,
// nothing is learned, and the session pairs nothing. In the set's first cycle
// the same call is paired and learned (the control the fix leaves as it was).
func TestARetryCyclesLateCallKeepsNoPairingWithTheTestsOwnRecordingAndLearnsNothing(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run("learn step ended "+strconv.FormatBool(ended), func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := New(zap.NewNop(), nil, &config.Config{})
			p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			p.StartErrorDrain(ctx)
			if err := p.Mock(ctx, models.OutgoingOptions{StartsTestSet: true, Learn: true}); err != nil {
				t.Fatal(err)
			}
			mm := p.getMockManager()
			t.Cleanup(func() { p.setMockManager(nil) })
			now := time.Now()
			own := r3Mock("mock-own", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":1}`)
			late := r3Mock("mock-late", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":2}`)
			if err := p.SetMocksWithWindow(ctx, []*models.Mock{own, late}, nil, models.BaseTime, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			// The staging's recorded windows: test-1's holds mock-own,
			// test-2's (recorded after it) mock-late.
			mm.SeedRecordedWindows([]models.TestWindow{
				{TestCase: "test-1", Start: now.Add(50 * time.Millisecond), End: now.Add(150 * time.Millisecond)},
				{TestCase: "test-2", Start: now.Add(650 * time.Millisecond), End: now.Add(750 * time.Millisecond)},
			})
			mm.ServeExactPool(models.PoolKeyNone, nil)
			// The cycle's first filter says whether the set's learn step
			// ended, then test-1 opens, owning mock-own.
			if err := p.SetLearnEnded(ended); err != nil {
				t.Fatal(err)
			}
			p.ContinueTestErrorCapture()
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own")})
			code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, roundPost(`{"item":"pear"}`))
			st := r3States(mm)
			paired := code == http.StatusOK && got == `{"order":1}`
			learned := len(st["mock-own"].ReqBodyNoise) > 0 || len(st["mock-own"].Variables) > 0
			if !ended {
				if !paired || !learned {
					t.Fatalf("precondition: in the set's first cycle the call is paired with test-1's own and learned: answered %d %s, learned %v", code, got, st["mock-own"].ReqBodyNoise)
				}
				return
			}
			if paired || learned || len(p.learnTracker.AnsweredMocks()) > 0 {
				t.Fatalf("a retry cycle's late call was paired with test-1's own recording: answered %d %s, mock-own learned %v %v, test-1 answered %v",
					code, got, st["mock-own"].ReqBodyNoise, st["mock-own"].Variables, exactNames(p.learnTracker.AnsweredMocks()))
			}
			if v, _ := mm.pairing(); v != nil {
				t.Fatal("the session still pairs once the set's learn step ended")
			}
			// A parser's connection finds no learn tracker (learn.TrackerFrom):
			// Mongo's learn step, which reads it per request, decides none.
			if learn.TrackerFrom(learn.WithTracker(ctx, p.learnTracker)) != nil {
				t.Fatal("a parser's connection still finds the learn tracker once the set's learn step ended: a learn step that reads it per request (Mongo's) would pair and learn out of recorded order")
			}
			// test-1's own call is still served exactly from its recording.
			if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, roundPost(`{"item":"apple"}`)); code != http.StatusOK || got != `{"order":1}` {
				t.Fatalf("test-1's own call got %d %s", code, got)
			}
			// A new session takes the learn step back up.
			if err := p.Mock(ctx, models.OutgoingOptions{StartsTestSet: true, Learn: true}); err != nil {
				t.Fatal(err)
			}
			if p.getMockManager().LearnEnded() {
				t.Fatal("a new session's learn step stays ended")
			}
			if learn.TrackerFrom(learn.WithTracker(ctx, p.learnTracker)) == nil {
				t.Fatal("a new session's parsers find no learn tracker")
			}
		})
	}
}
