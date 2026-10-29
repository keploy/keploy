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
}

func (s noAnswerSet) all() []*models.Mock {
	return []*models.Mock{s.passed, s.inWindowSession, s.inWindowPerTest, s.mapped, s.orphan}
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

func (f failingReads) IndexTestSetMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) (models.TestSetMocks, error) {
	if f.fail.Load() {
		return models.TestSetMocks{}, errMockFileUnreadable
	}
	return f.MockYaml.IndexTestSetMocks(ctx, set, after, before, have, need)
}

// noAnswerHoldRun is the harness over noAnswerSet, stored in format (yaml or
// gob) through the YAML mock store keploy replays with, its reads failing
// once failReads is set. Its passing tests consume set.passed.
func noAnswerHoldRun(t *testing.T, format string) (*prRun, noAnswerSet, *atomic.Bool) {
	t.Helper()
	h := newPartialRunHarness(t, 8, 0)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	for i, tc := range h.cases {
		start := at(10 * (i + 1))
		tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = start, start.Add(2*time.Second)
	}
	set := noAnswerSet{
		passed:          noAnswerHoldMock(models.HTTP, "passed", at(81)),
		inWindowSession: noAnswerHoldMock(models.HTTP, "window", at(71)),
		inWindowPerTest: noAnswerHoldMock(models.GRPC_EXPORT, "Window", at(71).Add(500*time.Millisecond)),
		mapped:          noAnswerHoldMock(models.HTTP, "mapped", at(75)),
		orphan:          noAnswerHoldMock(models.HTTP, "orphan", at(65)),
	}
	switch format {
	case "gob":
		t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	default:
		t.Setenv("KEPLOY_MOCK_FORMAT", "")
	}
	ys := mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks")
	for _, m := range set.all() {
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
	return h, set, fail
}

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
// its recorded window's mocks (both pools) and its mapping entry's, while a
// mock in no window that nothing consumed is deleted. It used to hold every
// mock of the set, every cycle, for one test that kept timing out (#4662's
// whole-set rule): 2,327 mocks of a production recording.
func TestRunTestSetPrunesAroundATestThatGotNoAnswer(t *testing.T) {
	timeout := prClientTimeoutErr(t)
	for _, format := range []string{"yaml", "gob"} {
		t.Run(format, func(t *testing.T) {
			h, set, _ := noAnswerHoldRun(t, format)
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
			if got := fmt.Sprint(fields["keptMocks"]); got != "[3]" {
				t.Errorf("the line's keptMocks = %s, want [3]: test-7's two window mocks and its mapped one", got)
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
				h, set, fail := noAnswerHoldRun(t, format)
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
		map[string][]models.MockEntry{"test-1": {{Name: "mock-mapped"}}}, nil, nil, keep)
	if err == nil {
		t.Fatalf("a test with no recorded window was held: %v", held)
	}
	if len(keep) != 1 {
		t.Fatalf("a failed hold changed the keep-set: %v", keep)
	}
}
