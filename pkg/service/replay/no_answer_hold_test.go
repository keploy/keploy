package replay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// noAnswerSet is the recording the per-test hold rows replay: eight tests,
// each recorded in its own 2 s window 10 s apart, test-7 the one whose request
// gets no answer. The first six tests' windows are the startup band
// (startupMockCutoff keeps every mock before test-6's request), so every mock
// below is recorded after it, where only the keep-set decides.
type noAnswerSet struct {
	// passed is recorded in test-8's window and consumed by the passing tests.
	passed *models.Mock
	// inWindowSession and inWindowPerTest are recorded in test-7's window and
	// consumed by no test: an HTTP call, which a recorder that states no
	// lifetime leaves in the session pool, and a gRPC call, in the per-test
	// pool.
	inWindowSession, inWindowPerTest *models.Mock
	// mapped is recorded between test-7's and test-8's windows, consumed by no
	// test, and listed in test-7's mapping entry.
	mapped *models.Mock
	// orphan is recorded between test-6's and test-7's windows: in no window,
	// in no mapping, consumed by no test.
	orphan *models.Mock
	// inFlight is a gRPC call sent in the gap before test-7's window and
	// answered inside it: in flight when test-7 began, which the agent serves
	// test-7 (models.WindowSchedule.InFlightInto). Consumed by no test.
	inFlight *models.Mock
	// carriedOver is a carry-over gRPC call (noAnswerCarryOverOp) recorded in
	// the gap after test-7's window: the agent releases it at test-7's window
	// (models.WindowSchedule.ReleaseIndex). Consumed by no test.
	carriedOver *models.Mock
}

func (s noAnswerSet) all() []*models.Mock {
	return []*models.Mock{s.passed, s.inWindowSession, s.inWindowPerTest, s.mapped, s.orphan, s.inFlight, s.carriedOver}
}

// noAnswerCarryOverOp is the gRPC operation registerNoAnswerCarryOver makes a
// carry-over call.
const noAnswerCarryOverOp = "/orders.Orders/CarryOver"

// registerNoAnswerCarryOver registers noAnswerCarryOverOp's calls as carry-over
// mocks (models.RegisterCarryOver), as a broker's parser registers its
// publishes, for the test's duration.
func registerNoAnswerCarryOver(t *testing.T) {
	t.Helper()
	t.Cleanup(models.RegisterCarryOver(models.GRPC_EXPORT, func(m *models.Mock) bool {
		return m.Spec.Metadata["operation"] == noAnswerCarryOverOp
	}))
}

// noAnswerHoldMock is a mock of kind recorded at at, for its own call (op):
// no two of a set's mocks are the same call, so none is another's repeat.
func noAnswerHoldMock(kind models.Kind, op string, at time.Time) *models.Mock {
	m := &models.Mock{
		Version: models.GetVersion(),
		Kind:    kind,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
	}
	switch kind {
	case models.GRPC_EXPORT:
		m.Spec.Metadata["destAddr"], m.Spec.Metadata["operation"] = "orders.internal:50051", "/orders.Orders/"+op
		m.Spec.GRPCReq, m.Spec.GRPCResp = &models.GrpcReq{}, &models.GrpcResp{}
	default:
		m.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "http://orders.internal/v1/" + op, ProtoMajor: 1, ProtoMinor: 1}
		m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Body: `{"ok":true}`}
	}
	return m
}

// failingReads is the YAML mock store whose reads fail once fail is set.
type failingReads struct {
	*mockdb.MockYaml
	fail *atomic.Bool
}

var errMockFileUnreadable = errors.New("the mock file is unreadable")

func (f failingReads) GetFilteredMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) ([]*models.Mock, error) {
	if f.fail.Load() {
		return nil, errMockFileUnreadable
	}
	return f.MockYaml.GetFilteredMocks(ctx, set, after, before, have, need)
}

func (f failingReads) GetUnFilteredMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) ([]*models.Mock, error) {
	if f.fail.Load() {
		return nil, errMockFileUnreadable
	}
	return f.MockYaml.GetUnFilteredMocks(ctx, set, after, before, have, need)
}

func (f failingReads) GetTestSetMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) (models.TestSetMocks, error) {
	if f.fail.Load() {
		return models.TestSetMocks{}, errMockFileUnreadable
	}
	return f.MockYaml.GetTestSetMocks(ctx, set, after, before, have, need)
}

func (f failingReads) WalkTestSetMocks(ctx context.Context, set string, visit func(m *models.Mock, perTest, session bool)) error {
	if f.fail.Load() {
		return errMockFileUnreadable
	}
	return f.MockYaml.WalkTestSetMocks(ctx, set, visit)
}

func (f failingReads) IndexTestSetMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) (models.TestSetMocks, error) {
	if f.fail.Load() {
		return models.TestSetMocks{}, errMockFileUnreadable
	}
	return f.MockYaml.IndexTestSetMocks(ctx, set, after, before, have, need)
}

// noAnswerHoldRun is the harness over noAnswerSet and extra, stored in format
// (yaml or gob) through the YAML mock store keploy replays with, its reads
// failing once failReads is set. Its passing tests consume set.passed.
// noAnswerAt gives the recording's moments, in seconds.
func noAnswerHoldRun(t *testing.T, format string, extra ...func(at noAnswerAt) *models.Mock) (*prRun, noAnswerSet, []*models.Mock, *atomic.Bool) {
	t.Helper()
	registerNoAnswerCarryOver(t)
	h := newPartialRunHarness(t, 8, 0)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	for i, tc := range h.cases {
		start := at(float64(10 * (i + 1)))
		tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = start, start.Add(2*time.Second)
	}
	set := noAnswerSet{
		passed:          noAnswerHoldMock(models.HTTP, "passed", at(81)),
		inWindowSession: noAnswerHoldMock(models.HTTP, "window", at(71)),
		inWindowPerTest: noAnswerHoldMock(models.GRPC_EXPORT, "Window", at(71.5)),
		mapped:          noAnswerHoldMock(models.HTTP, "mapped", at(75)),
		orphan:          noAnswerHoldMock(models.HTTP, "orphan", at(65)),
		inFlight:        noAnswerHoldMock(models.GRPC_EXPORT, "InFlight", at(69.5)),
		carriedOver:     noAnswerHoldMock(models.GRPC_EXPORT, "CarryOver", at(73)),
	}
	set.inFlight.Spec.ResTimestampMock = at(70.5)
	var extras []*models.Mock
	for _, mk := range extra {
		extras = append(extras, mk(at))
	}
	switch format {
	case "gob":
		t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	default:
		t.Setenv("KEPLOY_MOCK_FORMAT", "")
	}
	ys := mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks")
	for _, m := range append(set.all(), extras...) {
		if err := ys.InsertMock(context.Background(), m, "test-set-0"); err != nil {
			t.Fatalf("InsertMock: %v", err)
		}
	}
	// The gob writer is asynchronous; Close drains it to the file.
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fail := &atomic.Bool{}
	h.replayer.mockDB = failingReads{MockYaml: mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks"), fail: fail}
	h.mappings.onDisk = map[string][]models.MockEntry{"test-7": {{Name: set.mapped.Name, Kind: string(models.HTTP)}}}
	h.replayer.hookImpl = prHooks{consumed: []models.MockState{{Name: set.passed.Name, Kind: models.HTTP}}}
	h.replayer.config.Test.RemoveUnusedMocks = true
	return h, set, extras, fail
}

// noAnswerAt is a moment of noAnswerSet's recording, s seconds in.
type noAnswerAt = func(s float64) time.Time

// storedMockNames reads back the names of the mocks the set's mock file holds.
func storedMockNames(t *testing.T, h *prRun) map[string]bool {
	t.Helper()
	set, err := mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks").GetTestSetMocks(context.Background(), "test-set-0", models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("read the mock file back: %v", err)
	}
	names := map[string]bool{}
	for _, m := range append(set.AllPerTest, set.AllSession...) {
		names[m.Name] = true
	}
	return names
}

func sortedNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// One test whose request got no answer (a client timeout) beside tests that
// passed: the prune runs, and keeps what that test's next replay is served,
// its recorded window's mocks (both pools), the call in flight when its window
// began, the carry-over call its window releases and its mapping entry's,
// while a mock in no window that nothing consumed is deleted. It used to hold every
// mock of the set, every cycle, for one test that kept timing out (#4662's
// whole-set rule): 2,327 mocks of a production recording.
func TestRunTestSetPrunesAroundATestThatGotNoAnswer(t *testing.T) {
	timeout := prClientTimeoutErr(t)
	for _, format := range []string{"yaml", "gob"} {
		t.Run(format, func(t *testing.T) {
			h, set, _, _ := noAnswerHoldRun(t, format)
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			hooks := h.replayer.hookImpl.(prHooks)
			hooks.simErr = map[string]error{"test-7": timeout}
			h.replayer.hookImpl = hooks
			_ = h.run(t)

			if rep := h.report.report; rep == nil || rep.Success != 7 || rep.Failure != 1 {
				t.Fatalf("precondition: want 7 passes and test-7 failed, got %+v", rep)
			}
			if got := h.mocks.pruneCalls(); got != 0 {
				t.Fatalf("precondition: the run pruned through the harness's fake store (%d calls)", got)
			}
			left := storedMockNames(t, h)
			for what, m := range map[string]*models.Mock{
				"the passing tests' consumed mock":                  set.passed,
				"test-7's window's session-pool mock":               set.inWindowSession,
				"test-7's window's per-test mock":                   set.inWindowPerTest,
				"the mock test-7's mapping entry lists (no window)": set.mapped,
				"the call in flight when test-7's window began":     set.inFlight,
				"the carry-over call test-7's window releases":      set.carriedOver,
			} {
				if !left[m.Name] {
					t.Errorf("%s (%s) was pruned; the file keeps %v", what, m.Name, sortedNames(left))
				}
			}
			if left[set.orphan.Name] {
				t.Errorf("the mock in no window, mapping or consume (%s) survived: the set was not pruned (it keeps %v)",
					set.orphan.Name, sortedNames(left))
			}
			if warns := logs.FilterMessageSnippet("skipping mock pruning").All(); len(warns) != 0 {
				t.Errorf("the prune ran and still said it was skipped: %v", warns[0].ContextMap())
			}
			infos := logs.FilterMessageSnippet("around the tests that got no answer").All()
			if len(infos) != 1 || infos[0].Level != zap.InfoLevel {
				t.Fatalf("want one Info line naming the held tests, got %d", len(infos))
			}
			fields := infos[0].ContextMap()
			if got := fmt.Sprint(fields["noAnswerTests"]); got != "[test-7]" {
				t.Errorf("the line's noAnswerTests = %s, want [test-7]", got)
			}
			if got := fmt.Sprint(fields["keptMocks"]); got != "[5]" {
				t.Errorf("the line's keptMocks = %s, want [5]: test-7's two window mocks, its in-flight and carry-over calls and its mapped one", got)
			}
		})
	}
}

// The per-test hold never prunes past what it cannot name: when the read of
// the set's mocks that holds the unanswered test's own fails, or a cancel (a
// stop's, which says nothing of the test it ended) ended the request, the
// whole set is held, as before, and the line says which.
func TestRunTestSetHoldsTheWholeSetWhenItCannotHoldTheUnansweredTest(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       func(*testing.T) error
		failReads bool
		hold      string
	}{
		{"the read of the set's mocks fails", prClientTimeoutErr, true, "could not be read"},
		{"a cancel ended the request", prCanceledErr, false, "a cancel (a stop)"},
	} {
		for _, format := range []string{"yaml", "gob"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				h, set, _, fail := noAnswerHoldRun(t, format)
				core, logs := observer.New(zap.WarnLevel)
				h.replayer.logger = zap.New(core)
				hooks := h.replayer.hookImpl.(prHooks)
				hooks.simErr = map[string]error{"test-7": tc.err(t)}
				h.replayer.hookImpl = hooks
				if tc.failReads {
					// The results are read back after the last test; the
					// hold's read comes after them.
					h.report.onRead = func() { fail.Store(true) }
				}
				_ = h.run(t)

				if rep := h.report.report; rep == nil || rep.Success != 7 || rep.Failure != 1 {
					t.Fatalf("precondition: want 7 passes and test-7 failed, got %+v", rep)
				}
				left := storedMockNames(t, h)
				for _, m := range set.all() {
					if !left[m.Name] {
						t.Errorf("%s was pruned; the whole set must be held (the file keeps %v)", m.Name, sortedNames(left))
					}
				}
				warns := logs.FilterMessageSnippet("skipping mock pruning").All()
				if len(warns) != 1 {
					t.Fatalf("want one \"skipping mock pruning\" warning, got %d", len(warns))
				}
				fields := warns[0].ContextMap()
				if fields["reason"] != "no_answer" {
					t.Errorf("the warning's reason = %v, want no_answer", fields["reason"])
				}
				if got := fmt.Sprint(fields["noAnswerHold"]); !strings.Contains(got, tc.hold) {
					t.Errorf("the warning's noAnswerHold = %q, want it to say %q", got, tc.hold)
				}
			})
		}
	}
}

// A test the hold cannot place, with no recorded window, fails the hold whole:
// keep is left as it was, so the caller holds the whole set.
func TestHoldNoAnswerMocksFailsClosedForATestWithNoWindow(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), mockDB: &prMockDB{}}
	keep := map[string]models.MockState{"mock-kept": {Name: "mock-kept"}}
	tc := &models.TestCase{Name: "test-1", Kind: models.HTTP}
	held, err := r.holdNoAnswerMocks(context.Background(), "test-set-0", []string{"test-1"}, []*models.TestCase{tc},
		newCarryOverAttribution([]*models.TestCase{tc}), map[string][]models.MockEntry{"test-1": {{Name: "mock-mapped"}}}, nil, nil, keep)
	if err == nil {
		t.Fatalf("a test with no recorded window was held: %v", held)
	}
	if len(keep) != 1 {
		t.Fatalf("a failed hold changed the keep-set: %v", keep)
	}
}

// consumesByTest is prHooks whose drains report, while a test named in byTest
// is the one last sent, what byTest says it consumed, and prHooks.consumed
// otherwise.
type consumesByTest struct {
	prHooks
	byTest map[string][]models.MockState
	last   *atomic.Value
}

func (h consumesByTest) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	h.last.Store(tc.Name)
	return h.prHooks.SimulateRequest(ctx, tc, testSetID)
}

func (h consumesByTest) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	if name, _ := h.last.Load().(string); h.byTest[name] != nil {
		return h.byTest[name], nil
	}
	return h.prHooks.GetConsumedMocks(ctx)
}

// A streaming test's pre-stream drain records what it consumed before its body
// read gets no answer: a mock it consumed outside its window, that no passing
// test consumed, is held by that consume alone, and the prune still runs.
func TestRunTestSetHoldsWhatAStreamingTestConsumedBeforeItsBodyReadTimedOut(t *testing.T) {
	timeout := prClientTimeoutErr(t)
	for _, format := range []string{"yaml", "gob"} {
		t.Run(format, func(t *testing.T) {
			// Sent and answered in the gap before test-7's window: no window
			// places it, in flight or carried over.
			h, set, extras, _ := noAnswerHoldRun(t, format, func(at noAnswerAt) *models.Mock {
				return noAnswerHoldMock(models.HTTP, "prestream", at(64))
			})
			preStream := extras[0]
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			h.streaming("test-7")
			hooks := h.replayer.hookImpl.(prHooks)
			hooks.streamErr = map[string]error{"test-7": timeout}
			h.replayer.hookImpl = consumesByTest{
				prHooks: hooks,
				byTest:  map[string][]models.MockState{"test-7": {{Name: preStream.Name, Kind: models.HTTP}}},
				last:    &atomic.Value{},
			}
			_ = h.run(t)

			if rep := h.report.report; rep == nil || rep.Success != 7 || rep.Failure != 1 {
				t.Fatalf("precondition: want 7 passes and test-7 failed, got %+v", rep)
			}
			left := storedMockNames(t, h)
			if left[set.orphan.Name] {
				t.Fatalf("precondition: the mock in no window, mapping or consume (%s) survived: the set was not pruned (it keeps %v)",
					set.orphan.Name, sortedNames(left))
			}
			if !left[preStream.Name] {
				t.Errorf("the mock test-7 consumed before its body read timed out (%s) was pruned; the file keeps %v",
					preStream.Name, sortedNames(left))
			}
			infos := logs.FilterMessageSnippet("around the tests that got no answer").All()
			if len(infos) != 1 {
				t.Fatalf("want one Info line naming the held tests, got %d", len(infos))
			}
			if got := fmt.Sprint(infos[0].ContextMap()["keptMocks"]); got != "[6]" {
				t.Errorf("the line's keptMocks = %s, want [6]: noAnswerSet's five and the one test-7 consumed", got)
			}
		})
	}
}

// The hold places each mock by the rules the agent serves the unanswered
// test's window by, and holds nothing those rules give another test.
func TestHoldNoAnswerMocksHoldsWhatTheAgentServesTheTest(t *testing.T) {
	registerNoAnswerCarryOver(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	var cases []*models.TestCase
	for i := 1; i <= 8; i++ {
		tc := &models.TestCase{Name: fmt.Sprintf("test-%d", i), Kind: models.HTTP}
		tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = at(float64(10*i)), at(float64(10*i+2))
		cases = append(cases, tc)
	}
	answered := func(m *models.Mock, s float64) *models.Mock {
		m.Spec.ResTimestampMock = at(s)
		return m
	}
	untimed := noAnswerHoldMock(models.GRPC_EXPORT, "Untimed", time.Time{})
	untimed.Spec.ResTimestampMock = time.Time{}
	for _, tc := range []struct {
		name string
		mock *models.Mock
		held bool
	}{
		{"in flight when the window began", answered(noAnswerHoldMock(models.GRPC_EXPORT, "InFlight", at(69.5)), 70.5), true},
		{"no recorded time", untimed, true},
		{"placed at the window's start exactly", noAnswerHoldMock(models.GRPC_EXPORT, "AtStart", at(70)), true},
		{"placed at the window's end exactly", noAnswerHoldMock(models.GRPC_EXPORT, "AtEnd", at(72)), true},
		{"a carry-over call recorded in the gap after the window", noAnswerHoldMock(models.GRPC_EXPORT, "CarryOver", at(73)), true},
		{"sent and answered in the gap before the window", noAnswerHoldMock(models.GRPC_EXPORT, "InGap", at(69.5)), false},
		{"in flight into the next window", answered(noAnswerHoldMock(models.GRPC_EXPORT, "InFlightLater", at(79.5)), 80.5), false},
		{"a carry-over call recorded in the gap before the window", noAnswerHoldMock(models.GRPC_EXPORT, "CarryOver", at(65)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mock.Name = "mock-under-test"
			start, end := testWindowOf(cases[6])
			sched := models.NewWindowSchedule(recordedTestWindows(cases))
			if from, ok := sched.GapBefore(start); ok && tc.mock.Spec.Metadata["operation"] != noAnswerCarryOverOp {
				// The agent's own filter is the reference for every mock but a
				// carry-over one, which its carry-over tier releases.
				perTest, _ := pkg.FilterPerTestAndLaxPromotedInFlightAware(context.Background(), zap.NewNop(),
					[]*models.Mock{tc.mock}, start, end, false, cases[0].HTTPReq.Timestamp, from, true)
				if served := len(perTest) == 1; served != tc.held {
					t.Fatalf("precondition: the agent's filter serves the mock to test-7: %v, want %v", served, tc.held)
				}
			}
			r := &Replayer{logger: zap.NewNop(), mockDB: &prMockDB{filtered: []*models.Mock{tc.mock}}}
			keep := map[string]models.MockState{}
			held, err := r.holdNoAnswerMocks(context.Background(), "test-set-0", []string{"test-7"}, cases,
				newCarryOverAttribution(cases), nil, nil, nil, keep)
			if err != nil {
				t.Fatalf("hold: %v", err)
			}
			if _, kept := keep[tc.mock.Name]; kept != tc.held {
				t.Errorf("the hold for test-7 keeps the mock: %v, want %v (held %+v)", kept, tc.held, held)
			}
		})
	}
}
