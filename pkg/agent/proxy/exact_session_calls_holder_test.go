package proxy

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// ratesRule declares GET http://deps/echo/rates a session call exactly, by its
// method and whole path, as keploy.yaml's sessionCalls would.
var ratesRule = models.SessionCallRule{Host: "deps", Method: "GET", PathPrefix: "/echo/rates"}

// provenModes are the two ways keploy serves a replay exactly: with the proven
// switch unset and on.
var provenModes = []string{"", "1"}

// provenName names a row's switch mode.
func provenName(mode string) string {
	if mode == "1" {
		return "proven"
	}
	return "proven unset"
}

// holderSet stages, with the switch set to mode and rules declared: mock-0
// (R0), GET /echo/rates made at start-up, the pool's; and mock-1 (R1), the
// same call recorded otherAt after it by another test: T0, whose window holds
// it, when it is recorded after the start-up, else a test with no recorded
// window. T1's recorded window is 2s to 3s after the start-up, T2's 5s to
// 6s; neither recorded the call. It installs no view.
func holderSet(t *testing.T, mode string, otherAt time.Duration, rules ...models.SessionCallRule) *MockManager {
	t.Helper()
	t.Setenv(provenswitch.Env, mode)
	declareSessionCalls(t, rules...)
	startup := startUpCall("mock-0", 0, "/echo/rates", "R0")
	other := startUpCall("mock-1", otherAt, "/echo/rates", "R1")
	mm := stagedManager(t, nil, []*models.Mock{startup, other})
	mm.SetPoolNames([]string{"mock-0"})
	t0 := startup.Spec.ReqTimestampMock
	windows := []models.TestWindow{
		{TestCase: "T1", Start: t0.Add(2 * time.Second), End: t0.Add(3 * time.Second)},
		{TestCase: "T2", Start: t0.Add(5 * time.Second), End: t0.Add(6 * time.Second)},
	}
	if otherAt > 0 {
		windows = append(windows, models.TestWindow{TestCase: "T0", Start: t0.Add(otherAt - 100*time.Millisecond), End: t0.Add(otherAt + 100*time.Millisecond)})
	}
	mm.SeedRecordedWindows(windows)
	return mm
}

// ratesAnswered sends GET /echo/rates and fails the row unless it was
// answered want.
func ratesAnswered(t *testing.T, mm *MockManager, want, what string) {
	t.Helper()
	if got, r := httpMissCall(t, mm, "/echo/rates"); got != want {
		t.Fatalf("%s was answered %q (miss %+v); want %s", what, got, r, want)
	}
}

// ratesMissed sends GET /echo/rates and fails the row unless it missed; it
// returns the miss.
func ratesMissed(t *testing.T, mm *MockManager, what string) *models.MockMismatchReport {
	t.Helper()
	got, r := httpMissCall(t, mm, "/echo/rates")
	if r == nil {
		t.Fatalf("%s was answered %q; want a miss", what, got)
	}
	return r
}

// A call keploy.yaml's sessionCalls declare exactly is served to every test
// that recorded no such call, from its recording made at start-up, as often
// as the test makes it, whatever other test recorded the call too: a copy of
// the start-up recording derived into a test at the start-up's instant, one
// an earlier test recorded before T1's window, and one a later test recorded
// between T1's and T2's windows. Another test's recording is that test's
// alone, and never fixes what the declaration serves. Before, HTTP's walk of
// the call (ConsumeCursorSaturate, a session call's) was fixed by every
// recording of it the store staged, so in a test whose window came after the
// other test's recording it ended there, on a recording the test is never
// served, and the call missed in both switch modes: in T1 for the first two,
// in T2 for the third.
func TestADeclaredStartUpCallIsServedToATestWhileAnotherTestHoldsARecordingOfIt(t *testing.T) {
	for _, mode := range provenModes {
		for _, c := range []struct {
			name    string
			otherAt time.Duration
		}{
			{name: "a copy derived at the start-up instant into a test with no window", otherAt: 0},
			{name: "an earlier test's recording before the window", otherAt: time.Second},
			{name: "a later test's recording after the window", otherAt: 4 * time.Second},
		} {
			t.Run(provenName(mode)+"/"+c.name, func(t *testing.T) {
				mm := holderSet(t, mode, c.otherAt, ratesRule)
				mm.ServeExactPool("k", []string{"mock-0"})
				ratesAnswered(t, mm, "R0", "TestMain's call at start-up")
				mm.ServeExactTest("T1", nil)
				ratesAnswered(t, mm, "R0", "T1's call to the declared call")
				ratesAnswered(t, mm, "R0", "T1's second call to the declared call")
				mm.ServeExactPool("k", []string{"mock-0"})
				mm.ServeExactTest("T2", nil)
				ratesAnswered(t, mm, "R0", "T2's call to the declared call")
			})
		}
	}
}

// Under the background rules the start-up recording of a declared call
// answers every test that recorded no such call too, on the background's
// cursor: another test's recording of the call, which the background's walk
// held as well, never ends its sequence.
func TestUnderTheBackgroundRulesADeclaredStartUpCallIsServedWhileAnotherTestHoldsIt(t *testing.T) {
	for _, c := range []struct {
		name    string
		otherAt time.Duration
	}{
		{name: "a copy derived at the start-up instant into a test with no window", otherAt: 0},
		{name: "an earlier test's recording before the window", otherAt: time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			mm := holderSet(t, "1", c.otherAt, ratesRule)
			mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-0"}, nil)
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-0"})
			ratesAnswered(t, mm, "R0", "TestMain's call at start-up")
			mm.ServeExactTest("T1", nil)
			ratesAnswered(t, mm, "R0", "T1's call to the declared call")
			ratesAnswered(t, mm, "R0", "T1's second call to the declared call")
		})
	}
}

// A recording no view of T1 offers: another test's (T0's, its window around
// it), or, under today's rules, one of another pool slice's (the start-up of
// another worker: of the replay's pool, but not of the slice served before
// T1). Under the background rules a test is shown the whole pool, so another
// slice's recording is offered there.
const (
	noneUnoffered = ""
	anotherTests  = "another test's recording"
	anotherSlice  = "another pool slice's recording"
	unofferedAt   = 1500 * time.Millisecond
)

// t1Answers stages what recs makes, with a recording of GET /echo/rates no
// view of T1 offers (mock-1, R1, 1.5s after the start-up) when unoffered
// names one, with the switch set to mode and rules declared before either is
// made (DeriveLifetime reads them); serves the worker scope worker and the
// pool slice pool (under the background rules when bg) before T1, whose
// recorded window is 2s to 3s after the start-up unless noWindow; and returns
// what T1's n calls to GET /echo/rates were answered, "miss" for a miss.
func t1Answers(t *testing.T, mode string, bg, noWindow bool, rules []models.SessionCallRule, pool, worker []string, recs func() []*models.Mock, unoffered string, n int) []string {
	t.Helper()
	t.Setenv(provenswitch.Env, mode)
	declareSessionCalls(t, rules...)
	staged := recs()
	var windows []models.TestWindow
	if !noWindow {
		windows = append(windows, models.TestWindow{TestCase: "T1", Start: exactT0.Add(2 * time.Second), End: exactT0.Add(3 * time.Second)})
	}
	if unoffered != noneUnoffered {
		o := startUpCall("mock-1", unofferedAt, "/echo/rates", "R1")
		staged = append(staged, o)
		if unoffered == anotherTests {
			at := o.Spec.ReqTimestampMock
			windows = append(windows, models.TestWindow{TestCase: "T0", Start: at.Add(-100 * time.Millisecond), End: at.Add(100 * time.Millisecond)})
		}
	}
	mm := stagedManager(t, nil, staged)
	setPool := append([]string(nil), pool...)
	if unoffered == anotherSlice {
		setPool = append(setPool, "mock-1")
	}
	mm.SetPoolNames(setPool)
	mm.SeedRecordedWindows(windows)
	if len(worker) > 0 {
		mm.ServeExactWorker(worker)
	}
	key := "k"
	if bg {
		mm.ServeExactBackground(models.RecordedWithS12Routing, setPool, nil)
		key = models.PoolKeyNone
	}
	mm.ServeExactPool(key, pool)
	mm.ServeExactTest("T1", nil)
	var got []string
	for i := 0; i < n; i++ {
		body, r := httpMissCall(t, mm, "/echo/rates")
		if r != nil {
			body = "miss"
		}
		got = append(got, body)
	}
	return got
}

// A recording of a declared call that T1's view never offers (another
// test's, or another pool slice's) never changes what T1, which recorded no
// such call, is answered: T1's calls are answered as they are with no such
// recording staged, whatever else the view offers of the call. The walk keeps
// every recording the view offers, spent or not, so it never takes the
// sequence back: beside the worker scope's recordings of the call, T1 is
// answered first the last of them before its window, and never an earlier
// one after it; and a declared recording of another call never joins the
// walk. (What T1 is answered once the worker scope's recording of the call
// has answered is as before, and not pinned here.) Before, the unoffered
// recording, recorded after the rest and before T1's window, ended the walk,
// and T1 missed.
func TestARecordingTheViewNeverOffersChangesNothingADeclaredCallIsServed(t *testing.T) {
	cfgRule := models.SessionCallRule{Host: "deps", Method: "GET", PathPrefix: "/echo/cfg"}
	rates := func(extra ...*models.Mock) func() []*models.Mock {
		return func() []*models.Mock {
			return append([]*models.Mock{startUpCall("mock-0", 0, "/echo/rates", "R0")}, extra...)
		}
	}
	for _, c := range []struct {
		name         string
		bg, noWindow bool
		rules        []models.SessionCallRule
		pool, worker []string
		recs         func() []*models.Mock
		calls        int
		// want is what the first answers are; never, answers none may be.
		want, never []string
	}{
		{name: "the start-up recording alone", rules: []models.SessionCallRule{ratesRule}, pool: []string{"mock-0"},
			recs: rates(), calls: 2, want: []string{"R0", "R0"}},
		{name: "the start-up recording alone, no recorded window", noWindow: true, rules: []models.SessionCallRule{ratesRule}, pool: []string{"mock-0"},
			recs: rates(), calls: 3, want: []string{"R0", "R0", "R0"}},
		{name: "beside a worker-scope recording", rules: []models.SessionCallRule{ratesRule}, pool: []string{"mock-0"}, worker: []string{"mock-w"},
			calls: 3, want: []string{"W"}},
		{name: "beside two worker-scope recordings", rules: []models.SessionCallRule{ratesRule}, pool: []string{"mock-0"}, worker: []string{"mock-w1", "mock-w2"},
			calls: 3, want: []string{"W2"}, never: []string{"W1"}},
		{name: "beside a later start-up recording of another declared call", rules: []models.SessionCallRule{ratesRule, cfgRule}, pool: []string{"mock-0", "mock-c"},
			calls: 2, want: []string{"R0", "R0"}},
		{name: "the start-up recording alone, background rules", bg: true, rules: []models.SessionCallRule{ratesRule}, pool: []string{"mock-0"},
			recs: rates(), calls: 2, want: []string{"R0", "R0"}},
		{name: "beside an undeclared pool recording, background rules", bg: true, rules: []models.SessionCallRule{ratesRule}, pool: []string{"mock-0", "mock-p"},
			calls: 3, want: []string{"R0", "P", "miss"}},
	} {
		// The recordings a case makes are made once the case's rules are
		// installed (t1Answers), never here.
		switch c.name {
		case "beside a worker-scope recording":
			c.recs = func() []*models.Mock {
				return rates(workerProbe("mock-w", 500*time.Millisecond, "/echo/rates", "W"))()
			}
		case "beside two worker-scope recordings":
			c.recs = func() []*models.Mock {
				return rates(workerProbe("mock-w1", 500*time.Millisecond, "/echo/rates", "W1"), workerProbe("mock-w2", 1200*time.Millisecond, "/echo/rates", "W2"))()
			}
		case "beside a later start-up recording of another declared call":
			c.recs = func() []*models.Mock {
				return rates(startUpCall("mock-c", 1200*time.Millisecond, "/echo/cfg", "C"))()
			}
		case "beside an undeclared pool recording, background rules":
			// mock-p states no lifetime (workerProbe), so no rule declares
			// it, though ratesRule names its call.
			c.recs = func() []*models.Mock {
				return rates(workerProbe("mock-p", 200*time.Millisecond, "/echo/rates", "P"))()
			}
		}
		for _, mode := range provenModes {
			if c.bg && mode != "1" {
				continue // the background rules hold under the switch alone
			}
			unoffered := []string{anotherTests, anotherSlice}
			if c.bg {
				unoffered = unoffered[:1] // the whole pool is offered
			}
			for _, unoffered := range unoffered {
				t.Run(provenName(mode)+"/"+c.name+"/"+unoffered, func(t *testing.T) {
					alone := t1Answers(t, mode, c.bg, c.noWindow, c.rules, c.pool, c.worker, c.recs, noneUnoffered, c.calls)
					beside := t1Answers(t, mode, c.bg, c.noWindow, c.rules, c.pool, c.worker, c.recs, unoffered, c.calls)
					if len(alone) < len(c.want) || strings.Join(alone[:len(c.want)], ",") != strings.Join(c.want, ",") {
						t.Fatalf("with no unoffered recording T1 was answered %v; want it to begin %v", alone, c.want)
					}
					for _, n := range c.never {
						if slices.Contains(alone, n) {
							t.Fatalf("T1 was answered %v: %s, an earlier recording than one it was answered before", alone, n)
						}
					}
					if strings.Join(beside, ",") != strings.Join(alone, ",") {
						t.Fatalf("with %s of the call staged T1 was answered %v; with none %v", unoffered, beside, alone)
					}
				})
			}
		}
	}
}

// Between tests, a declared call's start-up recording answers the calls the
// application makes again (TestMain's, a refresh between tests) as it does
// with no other recording of the call staged: another test's recording of
// it, which the pool view never offers, never ends its walk there either.
// Before, the walk between tests ended on the other test's recording, and a
// second call missed. A walk between tests that holds no declared recording
// is as before, though the pool view tells a declared call of its slice:
// the pool's recording of a call an entry covers only by a path prefix
// answers once, then the walk ends on the other test's recording. Under the
// background rules, a recording of the slice the walk spent stays in it: the
// pool's undeclared recording of the call answers once, after the start-up
// one, and then the call misses, never the earlier start-up one again.
func TestBetweenTestsADeclaredCallIsServedWhileAnotherTestHoldsIt(t *testing.T) {
	for _, mode := range provenModes {
		t.Run(provenName(mode), func(t *testing.T) {
			mm := holderSet(t, mode, time.Second, ratesRule)
			mm.ServeExactPool("k", []string{"mock-0"})
			ratesAnswered(t, mm, "R0", "TestMain's call at start-up")
			ratesAnswered(t, mm, "R0", "TestMain's second call at start-up")
			mm.ServeExactTest("T1", nil)
			ratesAnswered(t, mm, "R0", "T1's call to the declared call")
			mm.ServeExactPool("k", []string{"mock-0"})
			ratesAnswered(t, mm, "R0", "the call between T1 and T2")
		})
		t.Run(provenName(mode)+"/an undeclared call beside a declared one", func(t *testing.T) {
			t.Setenv(provenswitch.Env, mode)
			cfgRule := models.SessionCallRule{Host: "deps", Method: "GET", PathPrefix: "/echo/cfg"}
			declareSessionCalls(t, cfgRule, models.SessionCallRule{Host: "deps", Method: "GET", PathPrefix: "/echo/r"})
			rates := startUpCall("mock-0", 0, "/echo/rates", "R0")
			cfg := startUpCall("mock-c", 100*time.Millisecond, "/echo/cfg", "C")
			other := startUpCall("mock-1", time.Second, "/echo/rates", "R1")
			if rates.DeclaredSessionCall() || rates.TestModeInfo.Consume != models.ConsumeCursorSaturate || !cfg.DeclaredSessionCall() {
				t.Fatal("precondition: /echo/rates is declared or no session call, or /echo/cfg is not declared")
			}
			mm := stagedManager(t, nil, []*models.Mock{rates, cfg, other})
			mm.SetPoolNames([]string{"mock-0", "mock-c"})
			mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "T0", Start: exactT0.Add(900 * time.Millisecond), End: exactT0.Add(1100 * time.Millisecond)}})
			mm.ServeExactPool("k", []string{"mock-0", "mock-c"})
			if sv, _ := mm.SequenceView(); sv.Declared == nil {
				t.Fatal("precondition: the pool view's sequence view tells no declared session call")
			}
			ratesAnswered(t, mm, "R0", "the first call to the undeclared call between tests")
			ratesMissed(t, mm, "the second call, its walk ending on the other test's recording")
		})
	}
	t.Run("background rules", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		declareSessionCalls(t, ratesRule)
		rates := startUpCall("mock-0", 0, "/echo/rates", "R0")
		// mock-p states no lifetime (workerProbe), so no rule declares it,
		// though ratesRule names its call.
		p := workerProbe("mock-p", 200*time.Millisecond, "/echo/rates", "P")
		other := startUpCall("mock-1", time.Second, "/echo/rates", "R1")
		mm := stagedManager(t, nil, []*models.Mock{rates, p, other})
		mm.SetPoolNames([]string{"mock-0", "mock-p"})
		mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "T0", Start: exactT0.Add(900 * time.Millisecond), End: exactT0.Add(1100 * time.Millisecond)}})
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-0", "mock-p"}, nil)
		mm.ServeExactPool(models.PoolKeyNone, []string{"mock-0", "mock-p"})
		ratesAnswered(t, mm, "R0", "the first call between tests")
		ratesAnswered(t, mm, "P", "the second call between tests")
		ratesMissed(t, mm, "the third call, the pool's undeclared recording spent")
	})
}

// The test that recorded a declared call itself is answered its own
// recording, and here, where that recording is the last of the call before
// its window, a call past it misses as one call more than the test recorded,
// as before, under today's rules and the background rules alike: the walk of
// the declared call keeps the recordings the view offers the test, its own
// among them, as it did. The next test, which recorded no such call, is
// answered the start-up recording.
func TestATestHoldingADeclaredCallMissesACallPastItsOwnRecording(t *testing.T) {
	for _, mode := range provenModes {
		for _, bg := range []bool{false, true} {
			if bg && mode != "1" {
				continue // the background rules hold under the switch alone
			}
			name := provenName(mode)
			if bg {
				name += "/background rules"
			}
			t.Run(name, func(t *testing.T) {
				mm := holderSet(t, mode, time.Second, ratesRule)
				key := "k"
				if bg {
					mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-0"}, nil)
					key = models.PoolKeyNone
				}
				mm.ServeExactPool(key, []string{"mock-0"})
				ratesAnswered(t, mm, "R0", "TestMain's call at start-up")
				mm.ServeExactTest("T0", []*models.Mock{roundMock(mm, "mock-1")})
				ratesAnswered(t, mm, "R1", "T0's call to the declared call it recorded")
				if r := ratesMissed(t, mm, "T0's call past its own recording"); r.MissKind != models.MissExtraCall {
					t.Fatalf("T0's call past its own recording missed as %q; want %q: %+v", r.MissKind, models.MissExtraCall, r)
				}
				// The next test, which recorded no such call, is served the
				// start-up recording.
				mm.ServeExactPool(key, []string{"mock-0"})
				mm.ServeExactTest("T1", nil)
				ratesAnswered(t, mm, "R0", "T1's call to the declared call")
			})
		}
	}
}

// A start-up call no keploy.yaml entry names exactly (none covers it, or one
// covers it only by a path prefix or with no method) is served as before while
// another test holds a recording of it: the test that recorded it is answered
// its own recording once and misses a call past it, as one call more than it
// recorded; a test that recorded no such call misses it, answered neither the
// start-up recording nor the other test's.
func TestAStartUpCallNotDeclaredExactlyIsServedAsBeforeWhileAnotherTestHoldsIt(t *testing.T) {
	for _, mode := range provenModes {
		for _, c := range []struct {
			name  string
			rules []models.SessionCallRule
			bg    bool
		}{
			{name: "undeclared"},
			{name: "undeclared, background rules", bg: true},
			{name: "declared by a path prefix", rules: []models.SessionCallRule{{Host: "deps", Method: "GET", PathPrefix: "/echo"}}},
			{name: "declared with no method", rules: []models.SessionCallRule{{Host: "deps", PathPrefix: "/echo/rates"}}},
		} {
			if c.bg && mode != "1" {
				continue // the background rules hold under the switch alone
			}
			t.Run(provenName(mode)+"/"+c.name, func(t *testing.T) {
				mm := holderSet(t, mode, time.Second, c.rules...)
				key := "k"
				if c.bg {
					mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-0"}, nil)
					key = models.PoolKeyNone
				}
				mm.ServeExactPool(key, []string{"mock-0"})
				ratesAnswered(t, mm, "R0", "TestMain's call at start-up")
				mm.ServeExactTest("T0", []*models.Mock{roundMock(mm, "mock-1")})
				ratesAnswered(t, mm, "R1", "T0's call it recorded")
				if r := ratesMissed(t, mm, "T0's call past its own recording"); r.MissKind != models.MissExtraCall {
					t.Fatalf("T0's call past its own recording missed as %q; want %q", r.MissKind, models.MissExtraCall)
				}
				mm.ServeExactPool(key, []string{"mock-0"})
				mm.ServeExactTest("T1", nil)
				if r := ratesMissed(t, mm, "T1's call it never recorded"); r.MissKind == models.MissExtraCall {
					t.Fatalf("T1's call it never recorded missed as one call more than it recorded: %+v", r)
				}
			})
		}
	}
}

// A walk of a call no entry names exactly is as before in a test whose view
// shows a declared session call of another call (its sequence view tells the
// declared recordings), while another test holds a recording of the call:
// the worker scope's recording of it answers once, by placement, with no
// window; and under the background rules the pool's recording of it (one an
// entry covers only by a path prefix) answers once on the background's
// cursor.
func TestAWalkOfAnUndeclaredCallIsAsBeforeBesideADeclaredOne(t *testing.T) {
	cfgRule := models.SessionCallRule{Host: "deps", Method: "GET", PathPrefix: "/echo/cfg"}
	t.Run("worker scope", func(t *testing.T) {
		for _, mode := range provenModes {
			t.Run(provenName(mode), func(t *testing.T) {
				t.Setenv(provenswitch.Env, mode)
				declareSessionCalls(t, cfgRule)
				cfg := startUpCall("mock-c", 0, "/echo/cfg", "C")
				w := workerProbe("mock-w", 500*time.Millisecond, "/echo/rates", "W")
				other := workerProbe("mock-1", time.Second, "/echo/rates", "R1")
				mm := stagedManager(t, nil, []*models.Mock{cfg, w, other})
				mm.SetPoolNames([]string{"mock-c"})
				t0 := cfg.Spec.ReqTimestampMock
				mm.SeedRecordedWindows([]models.TestWindow{
					{TestCase: "T0", Start: t0.Add(900 * time.Millisecond), End: t0.Add(1100 * time.Millisecond)},
					{TestCase: "T1", Start: t0.Add(2 * time.Second), End: t0.Add(3 * time.Second)},
				})
				mm.ServeExactWorker([]string{"mock-w"})
				mm.ServeExactPool("k", []string{"mock-c"})
				mm.ServeExactTest("T1", nil)
				if sv, _ := mm.SequenceView(); sv.Declared == nil {
					t.Fatal("precondition: T1's sequence view tells no declared session call")
				}
				if got, r := httpMissCall(t, mm, "/echo/cfg"); got != "C" {
					t.Fatalf("T1's call to the declared /echo/cfg was answered %q (miss %+v); want C", got, r)
				}
				ratesAnswered(t, mm, "W", "T1's call to the worker scope's call")
				ratesMissed(t, mm, "T1's second call, the worker scope's recording answered")
			})
		}
	})
	t.Run("background rules", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		declareSessionCalls(t, cfgRule, models.SessionCallRule{Host: "deps", Method: "GET", PathPrefix: "/echo/r"})
		cfg := startUpCall("mock-c", 0, "/echo/cfg", "C")
		pool := startUpCall("mock-0", 200*time.Millisecond, "/echo/rates", "R0")
		other := startUpCall("mock-1", time.Second, "/echo/rates", "R1")
		if pool.DeclaredSessionCall() || pool.TestModeInfo.Consume != models.ConsumeCursorSaturate {
			t.Fatalf("precondition: the pool's /echo/rates is declared (%v) or not a session call (%v)", pool.DeclaredSessionCall(), pool.TestModeInfo.Consume)
		}
		mm := stagedManager(t, nil, []*models.Mock{cfg, pool, other})
		mm.SetPoolNames([]string{"mock-c", "mock-0"})
		t0 := cfg.Spec.ReqTimestampMock
		mm.SeedRecordedWindows([]models.TestWindow{
			{TestCase: "T0", Start: t0.Add(900 * time.Millisecond), End: t0.Add(1100 * time.Millisecond)},
			{TestCase: "T1", Start: t0.Add(2 * time.Second), End: t0.Add(3 * time.Second)},
		})
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"mock-c", "mock-0"}, nil)
		mm.ServeExactPool(models.PoolKeyNone, []string{"mock-c", "mock-0"})
		mm.ServeExactTest("T1", nil)
		if sv, _ := mm.SequenceView(); sv.Declared == nil {
			t.Fatal("precondition: T1's sequence view tells no declared session call")
		}
		ratesAnswered(t, mm, "R0", "T1's call to the pool's /echo/rates")
		ratesMissed(t, mm, "T1's second call, the pool's recording spent")
	})
}
