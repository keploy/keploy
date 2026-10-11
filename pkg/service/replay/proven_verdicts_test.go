package replay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
				if status != models.TestSetStatusPassed || got.Status != models.TestStatusPassed || got.Result.DepsChecked {
					t.Fatalf("switch off: set %s, test %s, checked %v; want PASSED, PASSED, not checked (today's tier filter)", status, got.Status, got.Result.DepsChecked)
				}
				return
			}
			if status != models.TestSetStatusFailed || got.Status != models.TestStatusFailed {
				t.Fatalf("set %s, test %s; a test that did not make a recorded call must fail, and its set with it", status, got.Status)
			}
			if !got.Result.DepsChecked || len(models.MissingDepNames(got.Result.DepResult)) != 1 {
				t.Fatalf("checked %v, rows %+v; the report must say which recorded call was not made", got.Result.DepsChecked, got.Result.DepResult)
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

			status := h.run(t)
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
			if got := resultOf(t, h, "test-1"); got.Status != want || status != wantSet {
				t.Fatalf("test %s, set %s; want %s, %s", got.Status, status, want, wantSet)
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
		})
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
	if owned != 2 || len(missing) != 1 || missing[0] != "m2" {
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
