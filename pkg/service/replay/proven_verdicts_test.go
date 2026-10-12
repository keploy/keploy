package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// missesByTest answers the harness's per-test GetMockErrors calls in test
// order: reads[i] is what the agent hands back for the (i+1)th test.
func missesByTest(reads ...func() ([]models.UnmatchedCall, error)) func() ([]models.UnmatchedCall, error) {
	var mu sync.Mutex
	n := 0
	return func() ([]models.UnmatchedCall, error) {
		mu.Lock()
		defer mu.Unlock()
		i := n
		n++
		if i < len(reads) && reads[i] != nil {
			return reads[i]()
		}
		return nil, nil
	}
}

func setSwitch(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv(provenswitch.Env, "1")
	} else {
		t.Setenv(provenswitch.Env, "")
	}
}

func switchName(on bool) string {
	if on {
		return "switch on"
	}
	return "switch off"
}

// sessionTierHTTP is the mock an ordinary recording of an HTTP app produces:
// tagged HTTP_CLIENT, which DeriveLifetime's lax kind fallback makes
// session-tier, so today's tier-based assertion never checks it.
func sessionTierHTTP(name string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"type": "HTTP_CLIENT"}}}
	m.DeriveLifetime()
	return m
}

// mappedTo makes every test of the harness own the named mocks, loaded as given.
func mappedTo(h *prRun, mocks ...*models.Mock) {
	h.mocks.filtered = append(h.mocks.filtered, mocks...)
	h.mappings.onDisk = map[string][]models.MockEntry{}
	for _, tc := range h.cases {
		for _, m := range mocks {
			h.mappings.onDisk[tc.Name] = append(h.mappings.onDisk[tc.Name], models.MockEntry{Name: m.Name, Kind: string(m.Kind)})
		}
	}
}

func resultOf(t *testing.T, h *prRun, name string) models.TestResult {
	t.Helper()
	for _, r := range h.report.report.Tests {
		if r.TestCaseID == name {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", name, h.report.report.Tests)
	return models.TestResult{}
}

// THE ROW: a test that no longer makes a call it recorded. The mock is the
// shape every plain HTTP recording has (session-tier by the kind fallback), so
// today the assertion filters it out, reports NOT CHECKED and passes the test.
// Under the switch eligibility is membership: the test owns the mock, did not
// consume it, and fails, and so does its set.
func TestRunTestSet_UnconsumedTestMockFailsTheTest(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 1, 0)
			if m := sessionTierHTTP("mock-1"); m.TestModeInfo.Lifetime != models.LifetimeSession {
				t.Fatalf("precondition: the fixture must be session-tier, got %s", m.TestModeInfo.Lifetime)
			}
			mappedTo(h, sessionTierHTTP("mock-1"))

			status := h.run(t)
			got := resultOf(t, h, "test-1")
			if !on {
				if status != models.TestSetStatusPassed || got.Status != models.TestStatusPassed || got.Result.DepsChecked || got.FailureInfo.UnconsumedMocks != nil {
					t.Fatalf("switch off: set %s, test %s, checked %v, unconsumed %v; want PASSED, PASSED, not checked (today's tier filter), none named", status, got.Status, got.Result.DepsChecked, got.FailureInfo.UnconsumedMocks)
				}
				return
			}
			if status != models.TestSetStatusFailed || got.Status != models.TestStatusFailed {
				t.Fatalf("set %s, test %s; a test that did not make a recorded call must fail, and its set with it", status, got.Status)
			}
			if !got.Result.DepsChecked || len(models.MissingDepNames(got.Result.DepResult)) != 1 {
				t.Fatalf("checked %v, rows %+v; the report must say which recorded call was not made", got.Result.DepsChecked, got.Result.DepResult)
			}
			// The evidence names the mock, not only a display row.
			if want := []models.UnconsumedMock{{Name: "mock-1", Kind: string(models.HTTP)}}; !reflect.DeepEqual(got.FailureInfo.UnconsumedMocks, want) {
				t.Fatalf("unconsumed mocks %+v, want %+v", got.FailureInfo.UnconsumedMocks, want)
			}
		})
	}
}

// The control the row needs: a test that did make its recorded call passes
// under the switch. The consumed mock is session-tier too, which today's
// consumed-side filter drops; if the two sides disagreed the consumed call
// would read as unconsumed and a healthy test would go red.
func TestRunTestSet_ConsumedTestMockPasses(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 1, 0)
	mappedTo(h, sessionTierHTTP("mock-1"))
	h.replayer.hookImpl = prHooks{consumed: []models.MockState{{Name: "mock-1", Kind: models.HTTP, Type: "HTTP_CLIENT", Lifetime: models.LifetimeSession}}}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("set %s; a test that made its recorded call must pass: %+v", status, h.report.report.Tests)
	}
}

// A protocol-phase mock in a mapping (a handshake the recorder tagged config)
// is not a test mock: not consuming it in a test fails nothing.
func TestRunTestSet_ProtocolPhaseMockIsNotOwned(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 1, 0)
	mappedTo(h, &models.Mock{Name: "handshake", Kind: models.Postgres, Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}})

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("set %s; a protocol-phase mock must not be required of a test: %+v", status, h.report.report.Tests)
	}
}

// A call that got no recorded answer fails its test under the switch, even
// though the response matched; a DNS miss does not. With the switch off a miss
// is reported and the test passes, as today.
func TestRunTestSet_MissFailsTheTest(t *testing.T) {
	httpMiss := func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST payments/v1/charges"}}, nil
	}
	dnsMiss := func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A example.internal"}}, nil
	}
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 3, 0)
			h.instr.mockErrorsFor = missesByTest(nil, httpMiss, dnsMiss)
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)

			status := h.run(t)
			if on {
				// Decided with the rest of the verdict: nothing says test-2
				// passed, and the report says why it failed.
				for _, e := range logs.FilterMessage("result").All() {
					if strings.Contains(fmt.Sprint(e.ContextMap()["testcase id"]), "test-2") && strings.Contains(fmt.Sprint(e.ContextMap()["passed"]), "true") {
						t.Fatalf("test-2's result line says it passed: %v", e.ContextMap())
					}
				}
				if got := resultOf(t, h, "test-2"); !hasCategory(got, models.CallUnmatched) {
					t.Fatalf("test-2 categories %v; want UNMATCHED_CALL, the reason it failed", got.FailureInfo.Category)
				}
			}
			want := map[string]models.TestStatus{"test-1": models.TestStatusPassed, "test-2": models.TestStatusPassed, "test-3": models.TestStatusPassed}
			wantSet := models.TestSetStatusPassed
			if on {
				want["test-2"], wantSet = models.TestStatusFailed, models.TestSetStatusFailed
			}
			for name, ws := range want {
				if got := resultOf(t, h, name); got.Status != ws {
					t.Fatalf("%s is %s, want %s", name, got.Status, ws)
				}
			}
			rep := h.report.report
			wantFailed := 0
			if on {
				wantFailed = 1
			}
			if status != wantSet || rep.Failure != wantFailed || rep.Success != 3-wantFailed {
				t.Fatalf("set %s (passed %d, failed %d); want %s (passed %d, failed %d)", status, rep.Success, rep.Failure, wantSet, 3-wantFailed, wantFailed)
			}
		})
	}
}

// verdictHooks stands in for enterprise's hooks: it sets per-test verdicts in
// BeforeTestResult, as the auto-noise hook does when it flips a case.
type verdictHooks struct {
	prHooks
	set func([]models.TestResult)
}

func (h verdictHooks) BeforeTestResult(_ context.Context, _, _ string, results []models.TestResult) error {
	h.set(results)
	return nil
}

func setStatus(name string, status models.TestStatus) func([]models.TestResult) {
	return func(results []models.TestResult) {
		for i := range results {
			if results[i].TestCaseID == name {
				results[i].Status = status
			}
		}
	}
}

// Per-test verdicts from the BeforeTestResult hook drive the counters, the
// status and so the exit code under the switch, in both directions. Today the
// hook's verdict reaches the persisted rows only, and the counters and status
// disagree with them.
func TestRunTestSet_HookVerdictsDriveTheSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wrongBody bool
		set       func([]models.TestResult)
		// the set's status and failure count without and with the switch
		off, on   models.TestSetStatus
		offF, onF int
	}{
		{"the hook passes a failed test", true, setStatus("test-1", models.TestStatusPassed), models.TestSetStatusFailed, models.TestSetStatusPassed, 1, 0},
		{"the hook fails a passed test", false, setStatus("test-1", models.TestStatusFailed), models.TestSetStatusPassed, models.TestSetStatusFailed, 0, 1},
	} {
		for _, on := range []bool{false, true} {
			t.Run(tc.name+"/"+switchName(on), func(t *testing.T) {
				setSwitch(t, on)
				h := newPartialRunHarness(t, 2, 0)
				hooks := prHooks{}
				if tc.wrongBody {
					hooks.wrongBody = map[string]bool{"test-1": true}
				}
				h.replayer.hookImpl = verdictHooks{prHooks: hooks, set: tc.set}

				status := h.run(t)
				wantStatus, wantF := tc.off, tc.offF
				if on {
					wantStatus, wantF = tc.on, tc.onF
				}
				rep := h.report.report
				if status != wantStatus || rep.Failure != wantF || rep.Success+rep.Failure != 2 {
					t.Fatalf("set %s (passed %d, failed %d); want %s with %d failed", status, rep.Success, rep.Failure, wantStatus, wantF)
				}
				if on && h.replayer.totalTestFailed != wantF {
					t.Fatalf("run total failed %d, want %d: the exit code reads it", h.replayer.totalTestFailed, wantF)
				}
			})
		}
	}
}

// The streaming pass owns test mocks too.
func TestRunTestSet_StreamingTestFailsOnAnUnconsumedTestMock(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 1, 0)
			h.streaming("test-1")
			mappedTo(h, sessionTierHTTP("mock-1"))

			status := h.run(t)
			want, wantSet := models.TestStatusPassed, models.TestSetStatusPassed
			if on {
				want, wantSet = models.TestStatusFailed, models.TestSetStatusFailed
			}
			got := resultOf(t, h, "test-1")
			if got.Status != want || status != wantSet {
				t.Fatalf("test %s, set %s; want %s, %s", got.Status, status, want, wantSet)
			}
			if !on {
				return
			}
			// Its report says why, as a main-loop test's does: the presence
			// row, the category, and the mock by name.
			if !got.Result.DepsChecked || len(models.MissingDepNames(got.Result.DepResult)) != 1 || !hasCategory(got, models.DependencyMissing) {
				t.Fatalf("checked %v, rows %+v, categories %v; a streamed test failed by an unconsumed test mock must say which call was not made", got.Result.DepsChecked, got.Result.DepResult, got.FailureInfo.Category)
			}
			if len(got.FailureInfo.UnconsumedMocks) != 1 || got.FailureInfo.UnconsumedMocks[0].Name != "mock-1" {
				t.Fatalf("unconsumed mocks %+v, want mock-1", got.FailureInfo.UnconsumedMocks)
			}
		})
	}
}

// consumedReadFails stands in for an agent whose consumed-mock read fails.
type consumedReadFails struct{ prHooks }

func (consumedReadFails) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, errors.New("get consumed mocks returned status 500")
}

// A test that owns test mocks has no verdict when what it consumed cannot be
// read: under the switch its set is not judged, rather than passing on
// evidence nobody saw.
func TestRunTestSet_UnreadConsumptionLeavesTheSetUnjudged(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 1, 0)
			mappedTo(h, sessionTierHTTP("mock-1"))
			h.replayer.hookImpl = consumedReadFails{}

			status := h.run(t)
			if !on {
				if status != models.TestSetStatusPassed {
					t.Fatalf("switch off: set %s, want PASSED as today", status)
				}
				return
			}
			if status != models.TestSetStatusInternalErr {
				t.Fatalf("set %s; want INTERNAL_ERR, neither PASSED nor FAILED", status)
			}
			if reason := h.report.report.FailureReason; !strings.Contains(reason, "not judged") || !strings.Contains(reason, "test-1") {
				t.Fatalf("failure reason %q does not say the set is not judged, and for which test", reason)
			}
			if !resultOf(t, h, "test-1").FailureInfo.TestMocksUnread {
				t.Fatal("test-1's row does not say what it consumed could not be read: a hook or a judge would read its PASSED as a verdict")
			}
			// Not judged, not broken: the run goes on to the next set.
			if !h.replayer.notJudged("test-set-0") || abortAfterTestSet(status, utils.Native, h.replayer.notJudged("test-set-0")) {
				t.Fatal("an unjudged set must be marked so that the run goes on to the next set")
			}
		})
	}
}

// failsNthConsumedRead is prHooks whose nth read of what was consumed fails:
// for a streamed test, the third (after the set's first, the pre-stream read)
// is the read after the stream, of what the test consumed while streaming.
type failsNthConsumedRead struct {
	prHooks
	n     int32
	calls *atomic.Int32
}

func (h failsNthConsumedRead) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	if h.calls.Add(1) == h.n {
		return nil, errors.New("get consumed mocks returned status 500")
	}
	return h.prHooks.GetConsumedMocks(ctx)
}

// A streamed test whose consumption after the stream could not be read has no
// verdict either: its set is not judged, and its row says so.
func TestRunTestSet_AStreamedTestWhoseLastReadFailedLeavesTheSetUnjudged(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 1, 0)
	h.streaming("test-1")
	mappedTo(h, sessionTierHTTP("mock-1"))
	h.replayer.hookImpl = failsNthConsumedRead{prHooks: prHooks{consumed: []models.MockState{{Name: "mock-1", Kind: models.HTTP}}}, n: 3, calls: &atomic.Int32{}}

	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("set %s; want INTERNAL_ERR: what the streamed test consumed while streaming was never read", status)
	}
	if !resultOf(t, h, "test-1").FailureInfo.TestMocksUnread {
		t.Fatal("test-1's row does not say what it consumed could not be read")
	}
}

func TestVerdictCountsCountsEachTestOnceByItsWorstResult(t *testing.T) {
	results := []models.TestResult{
		{TestCaseID: "a", Status: models.TestStatusPassed},
		{TestCaseID: "b", Status: models.TestStatusPassed},
		{TestCaseID: "b", Status: models.TestStatusFailed}, // a duplicate row can never make a run greener
		{TestCaseID: "c", Status: models.TestStatusObsolete},
		{TestCaseID: "d", Status: models.TestStatusIgnored},
	}
	p, f, o, i := verdictCounts(results)
	if p != 1 || f != 1 || o != 1 || i != 1 {
		t.Fatalf("passed %d failed %d obsolete %d ignored %d; want 1 each", p, f, o, i)
	}
}

func TestStatusFromVerdictsKeepsWhatNoVerdictCanVouchFor(t *testing.T) {
	for _, tc := range []struct {
		current models.TestSetStatus
		failed  int
		want    models.TestSetStatus
	}{
		{models.TestSetStatusPassed, 1, models.TestSetStatusFailed},
		{models.TestSetStatusFailed, 0, models.TestSetStatusPassed},
		{models.TestSetStatusAppHalted, 0, models.TestSetStatusAppHalted},
		{models.TestSetStatusInternalErr, 0, models.TestSetStatusInternalErr},
		{models.TestSetStatusUserAbort, 0, models.TestSetStatusUserAbort},
	} {
		if got := statusFromVerdicts(tc.current, tc.failed); got != tc.want {
			t.Errorf("statusFromVerdicts(%s, %d) = %s, want %s", tc.current, tc.failed, got, tc.want)
		}
	}
}

func TestConsumedOutsideAssertionFollowsTheSwitch(t *testing.T) {
	sessionHTTP := models.MockState{Name: "m", Kind: models.HTTP, Type: "HTTP_CLIENT", Lifetime: models.LifetimeSession}
	config := models.MockState{Name: "c", Kind: models.Postgres, Type: "config", Lifetime: models.LifetimeSession}
	dns := models.MockState{Name: "d", Kind: models.DNS}
	setSwitch(t, false)
	if !consumedOutsideAssertion(sessionHTTP) {
		t.Fatal("switch off: a session-tier consumed mock is outside the assertion, as today")
	}
	setSwitch(t, true)
	if consumedOutsideAssertion(sessionHTTP) {
		t.Fatal("switch on: a consumed session-tier HTTP mock is a call the test made; dropping it would read as unconsumed")
	}
	if !consumedOutsideAssertion(config) || !consumedOutsideAssertion(dns) {
		t.Fatal("switch on: protocol-phase and DNS consumption stay outside the assertion")
	}
}

func TestUnconsumedTestMocks(t *testing.T) {
	expected := []models.MockEntry{{Name: "m1", Kind: "Http"}, {Name: "m2", Kind: "Http"}, {Name: "dns", Kind: "DNS"}, {Name: "hs", Kind: "Postgres"}}
	kinds := map[string]models.Kind{"m1": models.HTTP, "m2": models.HTTP, "dns": models.DNS, "hs": models.Postgres}
	excluded := map[string]bool{"hs": true}
	consumed := []models.MockState{{Name: "m1", Kind: models.HTTP, Lifetime: models.LifetimeSession}}

	setSwitch(t, false)
	if owned, missing := unconsumedTestMocks(true, expected, consumed, kinds, excluded); owned != 0 || missing != nil {
		t.Fatalf("switch off: owned %d, missing %v; want nothing", owned, missing)
	}
	setSwitch(t, true)
	if owned, missing := unconsumedTestMocks(false, expected, consumed, kinds, excluded); owned != 0 || missing != nil {
		t.Fatalf("unarmed: owned %d, missing %v; nothing is attributable to one test", owned, missing)
	}
	owned, missing := unconsumedTestMocks(true, expected, consumed, kinds, excluded)
	if owned != 2 || len(missing) != 1 || missing[0] != (models.UnconsumedMock{Name: "m2", Kind: "Http"}) {
		t.Fatalf("owned %d, missing %v; want 2 owned and [m2] missing", owned, missing)
	}
}

// A set whose tests' consumption could not be read and one of whose tests'
// misses could not all be read is not judged for both, and says both.
func TestRunTestSet_ANotJudgedSetGivesEveryReason(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 1, 0)
	mappedTo(h, sessionTierHTTP("mock-1"))
	h.replayer.hookImpl = consumedReadFails{}
	h.instr.mockErrorsFor = misses(failedRead)

	status := h.run(t)
	if status != models.TestSetStatusInternalErr {
		t.Fatalf("set %s; want INTERNAL_ERR", status)
	}
	reason := h.report.report.FailureReason
	if !strings.Contains(reason, "consumed could not be read") || !strings.Contains(reason, "no mock answered") && !strings.Contains(reason, "could not all be read") {
		t.Fatalf("failure reason %q does not give both reasons the set is not judged", reason)
	}
}

// A streamed test whose answer failed and whose mock set diverged is demoted
// to OBSOLETE, which leaves its set passing, and one whose answer matched
// passes. Under the switch, one that also left one of its test mocks
// unconsumed is FAILED and fails its set, whatever its answer did. The
// mismatch is said as FAILED, for that reason, and nothing says it passed:
// its result line says passed false. (main's streamed control row,
// TestRunTestSet_AStreamedTemplateFailureIsNeverDemotedToObsolete, with its
// mapped mock-1 never consumed.)
func TestRunTestSet_AStreamedTestWithAnUnconsumedTestMockIsNeverObsolete(t *testing.T) {
	const expected = `{"orders":[{"id":"ord_LIT","status":"paid"}]}`
	for _, c := range []struct {
		name, answer string
		off          models.TestStatus
	}{
		{"the answer failed", `{"orders":[{"id":"ord_OTHER","status":"paid"}]}`, models.TestStatusObsolete},
		{"the answer matched", expected, models.TestStatusPassed},
	} {
		for _, on := range []bool{false, true} {
			t.Run(c.name+"/"+switchName(on), func(t *testing.T) {
				setSwitch(t, on)
				app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
					mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
						a.record(r)
						w.Header().Set("Content-Type", "application/x-ndjson")
						w.WriteHeader(http.StatusOK)
						_, _ = io.WriteString(w, c.answer+"\n")
					})
				})
				conf := &tmplConf{}
				cases := []*models.TestCase{
					tmplCase(1, app.addr, "GET", "/feed", nil, "", models.HTTPResp{
						StatusCode: 200, StatusMessage: "OK",
						Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
						Body:       expected,
						StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: expected}}}},
					}),
				}
				h := newTmplHarness(t, conf, cases, nil)
				h.r.mappingDB = &mappedDB{tests: map[string][]models.MockEntry{"test-1": {{Name: "mock-1", Kind: string(models.HTTP)}}}}
				h.r.hookImpl = &consumingHooks{TestHooks: h.r.hookImpl}
				core, logs := observer.New(zap.DebugLevel)
				h.r.logger = zap.New(core)
				// Its console is pinned in TestRunTestSet_AProvenFailurePrintsNoPassLine.
				st := h.run(t)
				want := c.off
				if on {
					want = models.TestStatusFailed
				}
				if r := h.result(t, "test-1"); r.Status != want {
					t.Fatalf("test-1: %s, want %s — %s", r.Status, want, why(r))
				}
				if (st == models.TestSetStatusFailed) != on {
					t.Errorf("the set is %s; want it failed exactly under the switch", st)
				}
				if !on {
					return
				}
				if want := []models.UnconsumedMock{{Name: "mock-1", Kind: string(models.HTTP)}}; !reflect.DeepEqual(h.result(t, "test-1").FailureInfo.UnconsumedMocks, want) {
					t.Errorf("unconsumed mocks %+v, want %+v: the evidence names the mock", h.result(t, "test-1").FailureInfo.UnconsumedMocks, want)
				}
				said := logs.FilterMessageSnippet("mock mapping mismatch detected for streaming testcase; marking it as FAILED").All()
				if len(said) != 1 || said[0].ContextMap()["reason"] != vetoUnconsumedTestMock {
					t.Errorf("the mismatch was said %+v; want once, as FAILED, for the unconsumed test mock", said)
				}
				if n := len(logs.FilterMessageSnippet("marking as obsolete").All()) + len(logs.FilterMessageSnippet("mismatch ignored because streaming testcase passed").All()); n != 0 {
					t.Errorf("the mismatch was also said as obsolete or ignored (%d)", n)
				}
				for _, e := range logs.FilterMessage("result").All() {
					if strings.Contains(fmt.Sprint(e.ContextMap()["passed"]), "true") {
						t.Errorf("a result line says the test passed: %v", e.ContextMap())
					}
				}
			})
		}
	}
}

// Under the switch the veto of an OBSOLETE demotion is the switch's, whatever
// flags were passed: the line names KEPLOY_PROVEN=1, not a flag the user never
// set.
func TestVetoFlagNameNamesTheSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			want := "--assert-dependencies"
			if on {
				want = provenswitch.Env + "=1"
			}
			if got := vetoFlagName(true, false); got != want {
				t.Fatalf("vetoFlagName = %q, want %q", got, want)
			}
			if on {
				for _, v := range [][2]bool{{false, true}, {true, true}} {
					if got := vetoFlagName(v[0], v[1]); got != want {
						t.Fatalf("vetoFlagName%v = %q, want %q", v, got, want)
					}
				}
			}
			if got := vetoFlagName(false, false); got != "" {
				t.Fatalf("vetoFlagName with no veto = %q, want none", got)
			}
		})
	}
}

// Every missing dependency is named by its mock, in recorded order, past the
// cap on the report's display rows: the evidence for each unconsumed test
// mock is the mock's name.
func TestBuildDepResultsNamesEveryMissingMock(t *testing.T) {
	var expected []models.MockEntry
	kinds := map[string]models.Kind{}
	for i := 0; i < depMissingRowCap+5; i++ {
		name := fmt.Sprintf("mock-%d", i)
		expected = append(expected, models.MockEntry{Name: name, Kind: string(models.HTTP)})
		kinds[name] = models.HTTP
	}
	consumed := []models.MockState{{Name: "mock-0", Kind: models.HTTP}}
	dep := buildDepResults(expected, consumed, true, nil, kinds, nil)
	if len(dep.Missing) != depMissingRowCap+4 || dep.Missing[0].Name != "mock-1" || dep.Missing[len(dep.Missing)-1].Name != fmt.Sprintf("mock-%d", depMissingRowCap+4) {
		t.Fatalf("missing %d mocks from %v to %v; want every one of the %d, in recorded order", len(dep.Missing), dep.Missing[0], dep.Missing[len(dep.Missing)-1], depMissingRowCap+4)
	}
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			r := &models.TestResult{}
			attachDepResults(r, models.TestStatusFailed, dep, true)
			want := 0
			if on {
				want = len(dep.Missing)
			}
			if got := len(r.FailureInfo.UnconsumedMocks); got != want {
				t.Fatalf("%d unconsumed mocks named, want %d: every one, exactly under the switch", got, want)
			}
		})
	}
}

// Under the switch the assertion is asked for whatever the flags say: when it
// cannot run (here, not instrument mode) the one warning says so, naming the
// switch. The streaming pass judges its tests on their test mocks under the
// switch, so a set with streaming tests is not called inert for them.
func TestWarnDependencyAssertionInertUnderTheSwitch(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		instrument, streaming bool
		want                  string
	}{
		{"not instrument mode", false, false, provenswitch.Env + "=1 is inert for this test set"},
		{"streaming tests", true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			core, logs := observer.New(zap.WarnLevel)
			r := &Replayer{logger: zap.New(core), instrument: tc.instrument, config: &config.Config{}}
			warned := false
			r.warnDependencyAssertionInert("test-set-0", true, true, false, tc.streaming, false, &warned)
			entries := logs.All()
			if tc.want == "" {
				if len(entries) != 0 {
					t.Fatalf("warned %v; the switch judges streaming tests on their test mocks", entries)
				}
				return
			}
			if len(entries) != 1 || !strings.Contains(entries[0].Message, tc.want) {
				t.Fatalf("warned %v; want one line saying %q", entries, tc.want)
			}
		})
	}
}

// A before-test-result hook cannot pass a test with evidence against it, nor
// demote it to OBSOLETE: it stays FAILED, and its set with it. It may still
// quarantine it (IGNORED). The rule is held once, after every hook, so a hook
// that tries is caught whichever it is.
func TestRunTestSet_AHookCannotPassATestWithEvidenceAgainstIt(t *testing.T) {
	httpMiss := func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST payments/v1/charges"}}, nil
	}
	for _, tc := range []struct {
		name    string
		set     models.TestStatus
		want    models.TestStatus
		wantSet models.TestSetStatus
	}{
		{"passed", models.TestStatusPassed, models.TestStatusFailed, models.TestSetStatusFailed},
		{"obsolete", models.TestStatusObsolete, models.TestStatusFailed, models.TestSetStatusFailed},
		{"quarantined", models.TestStatusIgnored, models.TestStatusIgnored, models.TestSetStatusPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			h := newPartialRunHarness(t, 2, 0)
			h.instr.mockErrorsFor = missesByTest(nil, httpMiss)
			h.replayer.hookImpl = verdictHooks{set: setStatus("test-2", tc.set)}
			status := h.run(t)
			if got := resultOf(t, h, "test-2"); got.Status != tc.want || status != tc.wantSet {
				t.Fatalf("the hook set test-2 %s: test %s, set %s; want %s, %s", tc.set, got.Status, status, tc.want, tc.wantSet)
			}
		})
	}
}

// The per-test verdicts drive the set only when every test's result row is
// there to count. A failed test whose row could not be written would
// otherwise leave no FAILED row, and the set would read as PASSED.
func TestRunTestSet_AMissingResultRowDoesNotPassTheSet(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-2": true}}
	h.report.failInsertOf = map[string]error{"test-2": errors.New("report store is unwritable")}
	if status := h.run(t); status == models.TestSetStatusPassed {
		t.Fatalf("set %s with test-2 FAILED and its row unwritten; want it not PASSED", status)
	}
}

// An unconsumed test mock another test of the set consumed is named with that
// test: the call was made, by a sibling, and until each test's consumption is
// exact the order the shared pool served the two in can decide which test it
// counts for. Said, not hidden.
func TestMarkConsumedByNamesTheSibling(t *testing.T) {
	results := []models.TestResult{
		{TestCaseID: "test-1", FailureInfo: models.FailureInfo{UnconsumedMocks: []models.UnconsumedMock{{Name: "mock-a"}, {Name: "mock-b"}}}},
		{TestCaseID: "test-2"},
	}
	consumers := map[string][]string{}
	noteConsumers(consumers, "test-1", []models.MockState{{Name: "mock-x"}})
	noteConsumers(consumers, "test-2", []models.MockState{{Name: "mock-a"}})
	pairs := markConsumedBy(results, consumers)
	got := results[0].FailureInfo.UnconsumedMocks
	if got[0].ConsumedBy != "test-2" || got[1].ConsumedBy != "" || len(pairs) != 1 {
		t.Fatalf("unconsumed %+v, pairs %v; want mock-a consumed by test-2 and mock-b by no one", got, pairs)
	}
}

// Under the switch a streamed test's mock-set comparison leaves out of what it
// consumed the mocks its assertion does not own, as it leaves them out of
// what it expects; with the switch off it reads all of it, as before.
func TestStreamAssertedConsumption(t *testing.T) {
	consumed := []models.MockState{{Name: "ping", Kind: models.MySQL}, {Name: "call", Kind: models.HTTP}, {Name: "dns", Kind: models.DNS}}
	excluded := map[string]bool{"ping": true}
	setSwitch(t, false)
	if got := streamAssertedConsumption(consumed, excluded); len(got) != 3 {
		t.Fatalf("switch off: %v; want every consumed mock, as before", got)
	}
	setSwitch(t, true)
	if got := streamAssertedConsumption(consumed, excluded); len(got) != 1 || got[0].Name != "call" {
		t.Fatalf("switch on: %v; want only the owned call", got)
	}
}

// A streamed test a call of which got no recorded answer fails under the
// switch, decided with its verdict: nothing says it passed, and its row says
// why.
func TestRunTestSet_AStreamedTestWithAMissFails(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 1, 0)
	h.streaming("test-1")
	h.instr.mockErrorsFor = missesByTest(func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /quotes"}}, nil
	})
	core, logs := observer.New(zap.InfoLevel)
	h.replayer.logger = zap.New(core)
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("set %s; want FAILED", status)
	}
	got := resultOf(t, h, "test-1")
	if got.Status != models.TestStatusFailed || !hasCategory(got, models.CallUnmatched) {
		t.Fatalf("test-1 %s, categories %v; want FAILED with UNMATCHED_CALL", got.Status, got.FailureInfo.Category)
	}
	for _, e := range logs.FilterMessage("result").All() {
		if strings.Contains(fmt.Sprint(e.ContextMap()["passed"]), "true") {
			t.Fatalf("a result line says the streamed test passed: %v", e.ContextMap())
		}
	}
}

// A test's unconsumed test mock that a sibling consumed is named with that
// sibling in the report row.
func TestRunTestSet_AnUnconsumedTestMockNamesTheSiblingThatConsumedIt(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	mappedTo(h, sessionTierHTTP("mock-1"))
	var calls atomic.Int32
	h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
		// The set's first read, then test-1's (nothing), then test-2's.
		if calls.Add(1) == 3 {
			return []models.MockState{{Name: "mock-1", Kind: models.HTTP}}
		}
		return nil
	}}
	h.run(t)
	u := resultOf(t, h, "test-1").FailureInfo.UnconsumedMocks
	if len(u) != 1 || u[0].Name != "mock-1" || u[0].ConsumedBy != "test-2" {
		t.Fatalf("test-1 unconsumed %+v; want mock-1, consumed by test-2", u)
	}
}

// wrongAnswerHooks answers an HTTP test with something that is no HTTP
// response, the path RunTestSet fails as an internal error.
type wrongAnswerHooks struct{ prHooks }

func (wrongAnswerHooks) SimulateRequest(context.Context, *models.TestCase, string) (interface{}, error) {
	return "not a response", nil
}

// The misses of a test are read once under the switch, before its answer is
// compared, and a result written for it on another path carries that read:
// a second read would find the agent's channel drained.
func TestRunTestSet_AnInternalErrorResultKeepsTheTestsMisses(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 1, 0)
	h.replayer.hookImpl = wrongAnswerHooks{}
	h.instr.mockErrorsFor = missesByTest(func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /quotes"}}, nil
	})
	h.run(t)
	got := resultOf(t, h, "test-1")
	if got.Status != models.TestStatusFailed || len(got.FailureInfo.UnmatchedCalls) != 1 || !hasCategory(got, models.CallUnmatched) {
		t.Fatalf("test-1 %s with unmatched calls %+v, categories %v; want FAILED with its miss and UNMATCHED_CALL", got.Status, got.FailureInfo.UnmatchedCalls, got.FailureInfo.Category)
	}
}

// The rows drive the set only when every scored test has its row: with a
// failed test's row lost, the loop's FAILED stands; with the rows complete,
// they decide, in both directions.
func TestTallyFromVerdictsNeedsEveryRow(t *testing.T) {
	loop := setTally{success: 1, failure: 1, status: models.TestSetStatusFailed}
	onlyPassed := []models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusPassed}}
	if got := tallyFromVerdicts(loop, onlyPassed, false); got != loop {
		t.Fatalf("a set short of a row became %+v; want the loop's %+v", got, loop)
	}
	if got := tallyFromVerdicts(loop, onlyPassed, true); got.status != models.TestSetStatusPassed || got.failure != 0 || got.success != 1 {
		t.Fatalf("complete rows that all passed gave %+v; want PASSED, 1 passed", got)
	}
	failed := []models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusFailed}}
	if got := tallyFromVerdicts(setTally{success: 1, status: models.TestSetStatusPassed}, failed, true); got.status != models.TestSetStatusFailed || got.failure != 1 {
		t.Fatalf("a complete failed row gave %+v; want FAILED", got)
	}
}

// RunTestSet hands the tally whether every row is there.
func TestRunTestSetTalliesWithTheRowsCompleteness(t *testing.T) {
	_, fn := runTestSetSource(t)
	calls := callArgIdents(fn, "tallyFromVerdicts")
	if len(calls) != 1 || !calls[0]["resultsComplete"] {
		t.Fatalf("tallyFromVerdicts calls %v; want one, given resultsComplete", calls)
	}
}

// A streamed test whose answer is wrong is FAILED under the switch, whatever
// its mock set did: here it made its recorded call and a session-tier call
// another test owns, which the switch counts (consumedOutsideAssertion), so
// its mock set diverges. Demoting it to OBSOLETE would pass a set that fails
// with the switch off.
func TestRunTestSet_AStreamedWrongAnswerIsNeverObsoleteUnderTheSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 1, 0)
			h.streaming("test-1")
			mappedTo(h, sessionTierHTTP("mock-1"))
			session := func(name string) models.MockState {
				return models.MockState{Name: name, Kind: models.HTTP, Type: "HTTP_CLIENT", Lifetime: models.LifetimeSession}
			}
			h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-1": true}, consumed: []models.MockState{session("mock-1"), session("mock-x")}}
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)

			status := h.run(t)
			if got := resultOf(t, h, "test-1"); got.Status != models.TestStatusFailed || status != models.TestSetStatusFailed {
				t.Fatalf("test %s, set %s; a wrong answer must fail the test and its set", got.Status, status)
			}
			if !on {
				return
			}
			said := logs.FilterMessageSnippet("mock mapping mismatch detected for streaming testcase").All()
			if len(said) != 1 || !strings.Contains(said[0].Message, "as FAILED") || said[0].ContextMap()["reason"] != provenswitch.Env+"=1" {
				t.Fatalf("the mismatch was said %+v; want once, as FAILED, naming the switch", said)
			}
		})
	}
}
