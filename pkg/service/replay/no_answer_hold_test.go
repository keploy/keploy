package replay

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
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
	// carriedOver is a carry-over HTTP call (noAnswerCarryOverOp) whose
	// recorder stated it per-test, recorded in the gap after test-7's window:
	// the agent's filter drops it there, and its carry-over tier reaches it
	// first at test-7's window (models.WindowSchedule.CarryOverReachable).
	// Consumed by no test.
	carriedOver *models.Mock
}

func (s noAnswerSet) all() []*models.Mock {
	return []*models.Mock{s.passed, s.inWindowSession, s.inWindowPerTest, s.mapped, s.orphan, s.inFlight, s.carriedOver}
}

// noAnswerCarryOverOp is the operation registerNoAnswerCarryOver makes a
// carry-over call, and noAnswerPushOp the one it makes a server push the set's
// staging files.
const (
	noAnswerCarryOverOp = "CarryOver"
	noAnswerPushOp      = "Push"
)

// noAnswerOp is the operation noAnswerHoldMock recorded m for.
func noAnswerOp(m *models.Mock) string {
	if m.Spec.HTTPReq != nil {
		return strings.TrimPrefix(m.Spec.HTTPReq.URL, "http://orders.internal/v1/")
	}
	return strings.TrimPrefix(m.Spec.Metadata["operation"], "/orders.Orders/")
}

// registerNoAnswerCarryOver registers, for the test's duration, the gRPC and
// HTTP calls of noAnswerCarryOverOp as carry-over mocks
// (models.RegisterCarryOver), as a broker's parser registers its publishes,
// and the gRPC calls of noAnswerPushOp as a models.StagingWithStartup need, as
// enterprise's Pulsar parser declares its server pushes
// (pulsarmock.IsServerPush): its push hub delivers each from its release
// window on (models.WindowSchedule.Released).
func registerNoAnswerCarryOver(t *testing.T) {
	t.Helper()
	carry := func(m *models.Mock) bool { return noAnswerOp(m) == noAnswerCarryOverOp }
	t.Cleanup(models.RegisterCarryOver(models.GRPC_EXPORT, carry))
	t.Cleanup(models.RegisterCarryOver(models.HTTP, carry))
	t.Cleanup(models.RegisterStagingNeed(models.GRPC_EXPORT, models.StagingNeed{
		Use:   models.StagingWithStartup,
		Match: func(m *models.Mock) bool { return noAnswerOp(m) == noAnswerPushOp },
	}))
}

// statedPerTest marks m as a mock its recorder stated per-test
// (models.MetadataLifetimeKey), as the HTTP and MySQL recorders do.
func statedPerTest(m *models.Mock) *models.Mock {
	m.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
	return m
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
		carriedOver:     statedPerTest(noAnswerHoldMock(models.HTTP, noAnswerCarryOverOp, at(73))),
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
	// The shipped window mode, which the agent and the hold read alike.
	h.replayer.config.Test.StrictMockWindow = true
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
		newCarryOverAttribution([]*models.TestCase{tc}), false, map[string][]models.MockEntry{"test-1": {{Name: "mock-mapped"}}}, nil, keep)
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

// walkedSet is a mock store that walks the given per-test and session pools
// (pkg.TestSetMockWalker).
type walkedSet struct {
	*prMockDB
	perTest, session []*models.Mock
}

func (w walkedSet) WalkTestSetMocks(_ context.Context, _ string, visit func(m *models.Mock, perTest, session bool)) error {
	for _, m := range w.perTest {
		visit(m, true, false)
	}
	for _, m := range w.session {
		visit(m, false, true)
	}
	return nil
}

// plannedProxy is managerProxy with the mock manager's planners as well, as
// the agent's proxy forwards them (proxy.Proxy.CarryOverLoadRange and
// InFlightFrom): the agent then loads the carry-over lookahead and the calls
// in flight into a window, as a replay's does.
type plannedProxy struct{ *managerProxy }

func (p plannedProxy) CarryOverLoadRange(start time.Time) (time.Time, time.Time, bool) {
	return p.mm.CarryOverLoadRange(start)
}
func (p plannedProxy) InFlightFrom(start time.Time) (time.Time, bool) {
	return p.mm.InFlightFrom(start)
}

// The agent's residencies of the set's per-test mocks (StoreMocksStream).
const (
	// onDisk: the on-disk store, the agent's default in the strict window
	// mode, which loads each window's mocks and the carry-over lookahead.
	onDisk = true
	// resident: the fallback when the store cannot be made, and the lax
	// mode's residency, where the window filter routes every mock.
	resident = false
)

// agentServes is how the agent itself serves m, of the per-test pool or the
// session pool, in the window of ws[test]: a real agent (agentsvc.New) holding
// the set's one mock as StoreMocksStream stores the stream the replayer sends,
// on disk or resident, its mock manager staged by the agent's own
// UpdateMockParams with the replayer's parameters: the set's staging, then
// every window in order up to ws[test]'s. The answer is the tier that holds m
// once that window is staged: the per-test tree, the startup tree, a release
// first at the window (the carry-over tier, which did not hold it at the
// window before; or a server push the staging handed the manager, which the
// push hub delivers from its release window on,
// models.WindowSchedule.Released), the session tree, else none.
func agentServes(t *testing.T, m *models.Mock, session, strict, disk bool, ws []models.TestWindow, test int) pkg.WindowServe {
	t.Helper()
	ctx, nop := context.Background(), zap.NewNop()
	mm := proxyPkg.NewMockManager(nil, nil, nop)
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	cfg := &config.Config{}
	cfg.Test.StrictMockWindow = disk
	// agentsvc.New registers its incoming proxy and the pool's idle
	// retention globally: both are put back as they were.
	prevIncoming := coreAgent.ActiveIncomingProxy
	t.Cleanup(func() {
		coreAgent.RegisterIncomingProxy(prevIncoming)
		proxyPkg.SetConnectionIdleRetention(0)
	})
	a := agentsvc.New(nop, nil, plannedProxy{&managerProxy{mm: mm}}, nil, nil, cfg)
	var stream bytes.Buffer
	enc := gob.NewEncoder(&stream)
	if err := enc.Encode(m); err != nil {
		t.Fatalf("encode the mock: %v", err)
	}
	header := models.MockStreamHeader{FilteredCount: 1}
	if session {
		header = models.MockStreamHeader{UnfilteredCount: 1}
	}
	if err := a.StoreMocksStream(ctx, header, gob.NewDecoder(&stream)); err != nil {
		t.Fatalf("store the mock: %v", err)
	}
	t.Cleanup(func() {
		// Storing again closes the on-disk store the stream made.
		cfg.Test.StrictMockWindow = false
		_ = a.StoreMocks(ctx, nil, nil)
	})
	stage := func(after, before time.Time, staging bool) {
		t.Helper()
		p := models.MockFilterParams{AfterTime: after, BeforeTime: before, StrictMockWindow: strict}
		if staging {
			p.FirstRecordedTestStart, p.RecordedWindows = ws[0].Start, ws
		}
		if err := a.UpdateMockParams(ctx, p); err != nil {
			t.Fatalf("UpdateMockParams: %v", err)
		}
	}
	holds := func(reads ...func() ([]*models.Mock, error)) bool {
		t.Helper()
		for _, read := range reads {
			got, err := read()
			if err != nil {
				t.Fatalf("read the mock manager's tier: %v", err)
			}
			for _, mk := range got {
				if mk != nil && mk.Name == m.Name {
					return true
				}
			}
		}
		return false
	}
	stage(models.BaseTime, time.Now(), true)
	staged := holds(mm.GetFilteredMocks, mm.GetStartupMocks, mm.GetUnFilteredMocks, mm.GetCarryOverMocks)
	for i := 0; i < test; i++ {
		stage(ws[i].Start, ws[i].End, false)
	}
	carriedBefore := holds(mm.GetCarryOverMocks)
	stage(ws[test].Start, ws[test].End, false)
	sched, req := models.NewWindowSchedule(ws), m.Spec.ReqTimestampMock
	pushed := staged && models.IsStagedAtStaging(m) && sched.Released(req, ws[test].Start) && (test == 0 || !sched.Released(req, ws[test-1].Start))
	served := pkg.NotServed
	switch {
	case holds(mm.GetFilteredMocks):
		served = pkg.ServedPerTest
	case holds(mm.GetStartupMocks):
		served = pkg.ServedStartup
	case pushed, holds(mm.GetCarryOverMocks) && !carriedBefore:
		served = pkg.ServedReleased
	case holds(mm.GetUnFilteredMocks):
		served = pkg.ServedSession
	}
	reChecksOnlyNarrow(t, mm, m, holds)
	return served
}

// reChecksOnlyNarrow requires the mock manager's re-checks of the open window
// to serve m only where its staging filed it. The windowed read
// (GetFilteredMocksInWindow) and the open test's ownership (OwnedByOpenTest)
// re-check the staged per-test tree and read nothing else. A consume
// (MarkMockAsUsed, whose out-of-window mark is consumedOutOfWindow) labels the
// consumption and files m in no tier. So a re-check can narrow what the
// staging built, never widen it, and the hold, built on the staging's serve
// decision, keeps at least what any read serves the window.
func reChecksOnlyNarrow(t *testing.T, mm *proxyPkg.MockManager, m *models.Mock, holds func(...func() ([]*models.Mock, error)) bool) {
	t.Helper()
	tiers := []struct {
		name string
		read func() ([]*models.Mock, error)
	}{
		{"per-test", mm.GetFilteredMocks},
		{"startup", mm.GetStartupMocks},
		{"carry-over", mm.GetCarryOverMocks},
		{"session", mm.GetUnFilteredMocks},
	}
	staged := make([]bool, len(tiers))
	for i, tier := range tiers {
		staged[i] = holds(tier.read)
	}
	if holds(mm.GetFilteredMocksInWindow) && !staged[0] {
		t.Errorf("the windowed read serves %s, which the staging did not file in the per-test tree", m.Name)
	}
	if mm.OwnedByOpenTest(m) && !staged[0] {
		t.Errorf("the open test owns %s, which the staging did not file in the per-test tree", m.Name)
	}
	mm.MarkMockAsUsed(*m)
	for i, tier := range tiers {
		if holds(tier.read) && !staged[i] {
			t.Errorf("consuming %s filed it in the %s tier, which the staging did not", m.Name, tier.name)
		}
	}
}

// stagedPastTheFilter is how the agent's mock manager serves m in the window
// sw when handed it as a per-test mock past the window filter, as the agent's
// carry-over lookahead hands it (appendCarryOver): a real proxy.MockManager
// staged over the set's recorded windows (sched), the set's staging, then the
// window before sw (prev), then sw. The answer is the tier that holds m once
// sw is staged, as agentServes reads it.
func stagedPastTheFilter(t *testing.T, m *models.Mock, sched *models.WindowSchedule, sw, prev pkg.ServeWindow) pkg.WindowServe {
	t.Helper()
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(sw.FirstStart)
	var windows []models.TestWindow
	for i := 0; i < sched.Len(); i++ {
		windows = append(windows, sched.Window(i))
	}
	mm.SeedRecordedWindows(windows)
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	holds := func(read func() ([]*models.Mock, error)) bool {
		got, err := read()
		if err != nil {
			t.Fatalf("read the mock manager's tier: %v", err)
		}
		for _, mk := range got {
			if mk != nil && mk.Name == m.Name {
				return true
			}
		}
		return false
	}
	mm.SetMocksWithWindow([]*models.Mock{m.DeepCopy()}, nil, prev.Start, prev.End)
	carriedAtPrev := holds(mm.GetCarryOverMocks)
	mm.SetMocksWithWindow([]*models.Mock{m.DeepCopy()}, nil, sw.Start, sw.End)
	req := m.Spec.ReqTimestampMock
	switch {
	case holds(mm.GetFilteredMocks):
		return pkg.ServedPerTest
	case holds(mm.GetStartupMocks):
		return pkg.ServedStartup
	case models.IsStagedAtStaging(m) && sched.Released(req, sw.Start) && !sched.Released(req, prev.Start):
		return pkg.ServedReleased
	case holds(mm.GetUnFilteredMocks):
		return pkg.ServedSession
	case holds(mm.GetCarryOverMocks) && !carriedAtPrev:
		return pkg.ServedReleased
	}
	return pkg.NotServed
}

// heldByTheAgent is what the hold keeps of m for the unanswered test whose
// window [start, end] is, read off how the agent serves it there on each of
// its residencies (agentServes): the union, since the replayer cannot tell
// which one the agent has. A mock either serves the window's own pool or
// releases first there is held; of what the session pool hands every window,
// one recorded in the window or at no time. Not the startup tier, nor a
// session-pool mock recorded elsewhere, which some test's consumption holds or
// none (the hold's stated limit).
func heldByTheAgent(m *models.Mock, start, end time.Time, served ...pkg.WindowServe) bool {
	req, res := m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
	recordedIn := req.IsZero() || res.IsZero() || !models.PlaceOf(m).Before(start) && !models.PlaceOf(m).After(end)
	for _, s := range served {
		if s == pkg.ServedPerTest || s == pkg.ServedReleased || s == pkg.ServedSession && recordedIn {
			return true
		}
	}
	return false
}

// holdRow is a mock of one lifetime and kind class, and how the hold and the
// agent serve it in test-7's window of the hold table's set (holdTableSet):
// want of the shared predicate (pkg.ServeInWindow on the hold's window), disk
// and resident of the real agent's two residencies (agentServes), held of the
// hold.
type holdRow struct {
	name     string
	mock     *models.Mock
	session  bool
	strict   bool
	want     pkg.WindowServe
	disk     pkg.WindowServe
	resident pkg.WindowServe
	held     bool
}

// holdTableSet is the hold table's set: eight tests, test-i recorded over
// [10i, 10i+2] seconds past base, the hour before now; at places a time there.
func holdTableSet() (cases []*models.TestCase, at func(s float64) time.Time) {
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at = func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	for i := 1; i <= 8; i++ {
		tc := &models.TestCase{Name: fmt.Sprintf("test-%d", i), Kind: models.HTTP}
		tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = at(float64(10*i)), at(float64(10*i+2))
		cases = append(cases, tc)
	}
	return cases, at
}

// checkHoldRow runs tc's mock through the real agent on both residencies, the
// shared predicate, the mock manager handed it past the filter, and the hold
// for test-7, and requires each to serve or hold it as tc says, and the hold
// to equal the union of what the agent serves test-7's window as its own.
func checkHoldRow(t *testing.T, cases []*models.TestCase, tc holdRow) {
	t.Helper()
	tc.mock.Name = "mock-under-test"
	tc.mock.DeriveLifetime()
	ws := recordedTestWindows(cases)
	sched := models.NewWindowSchedule(ws)
	const test = 6
	w := ws[test]
	if w.TestCase != "test-7" {
		t.Fatalf("precondition: window %d is %s's, not test-7's", test, w.TestCase)
	}
	disk := agentServes(t, tc.mock.DeepCopy(), tc.session, tc.strict, onDisk, ws, test)
	res := agentServes(t, tc.mock.DeepCopy(), tc.session, tc.strict, resident, ws, test)
	if disk != tc.disk || res != tc.resident {
		t.Errorf("the agent serves the mock in test-7's window as %d on disk and %d resident, want %d and %d", disk, res, tc.disk, tc.resident)
	}
	sw := serveWindowOf(sched, w, tc.strict, false)
	if got := pkg.ServeInWindow(tc.mock, tc.session, sw); got != tc.want {
		t.Errorf("pkg.ServeInWindow on the hold's window: %d, want %d", got, tc.want)
	}
	if !tc.session {
		strict, strictPrev := sw, serveWindowOf(sched, ws[test-1], tc.strict, false)
		strict.StrictPerCall, strict.StrictWindow = true, true
		strictPrev.StrictPerCall, strictPrev.StrictWindow = true, true
		want, _ := pkg.StageInWindow(zap.NewNop(), tc.mock, strict)
		if got := stagedPastTheFilter(t, tc.mock, sched, strict, strictPrev); got != want {
			t.Errorf("the mock manager, handed the mock past the filter, serves it as %d; pkg.StageInWindow, strict: %d", got, want)
		}
	}
	store := walkedSet{prMockDB: &prMockDB{}}
	if tc.session {
		store.session = []*models.Mock{tc.mock}
	} else {
		store.perTest = []*models.Mock{tc.mock}
	}
	r := &Replayer{logger: zap.NewNop(), mockDB: store, config: &config.Config{}}
	r.config.Test.StrictMockWindow = tc.strict
	keep := map[string]models.MockState{}
	held, err := r.holdNoAnswerMocks(context.Background(), "test-set-0", []string{"test-7"}, cases,
		newCarryOverAttribution(cases), false, nil, nil, keep)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	_, kept := keep[tc.mock.Name]
	if kept != tc.held {
		t.Errorf("the hold for test-7 keeps the mock: %v, want %v (held %+v)", kept, tc.held, held)
	}
	if union := heldByTheAgent(tc.mock, w.Start, w.End, disk, res); kept != union {
		t.Errorf("the hold for test-7 keeps the mock: %v; what the agent serves it on disk (%d) and resident (%d) says %v", kept, disk, res, union)
	}
}

// The hold asks the agent's own decision (pkg.ServeInWindow) for every class
// of mock, and holds what it serves the unanswered test's window as that
// window's own: its per-test pool, what it releases first, and of what every
// window is served, what was recorded in it or at no time. Each row runs one
// mock through the real agent on both its residencies (agentServes: on disk
// and resident), the shared predicate and the hold (holdNoAnswerMocks), and
// requires the hold to equal the union of what the two residencies serve the
// window as its own; the mock manager's re-checks of the window serve nothing
// past its staging (reChecksOnlyNarrow). The mock manager handed the mock past
// the filter, as the carry-over lookahead hands it, must stage it as the
// shared predicate says for it (pkg.StageInWindow, both strict flags set,
// placed whatever its lifetime): the manager keeps no rule of its own.
func TestHoldNoAnswerMocksHoldsWhatTheAgentServesTheTest(t *testing.T) {
	registerNoAnswerCarryOver(t)
	cases, at := holdTableSet()
	grpc := func(op string, s float64) *models.Mock { return noAnswerHoldMock(models.GRPC_EXPORT, op, at(s)) }
	http := func(op string, s float64) *models.Mock { return noAnswerHoldMock(models.HTTP, op, at(s)) }
	answered := func(m *models.Mock, s float64) *models.Mock {
		m.Spec.ResTimestampMock = at(s)
		return m
	}
	stamped := func(m *models.Mock, req, res time.Time) *models.Mock {
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = req, res
		return m
	}
	tagged := func(m *models.Mock, tag string) *models.Mock {
		m.Spec.Metadata["type"] = tag
		return m
	}
	sessionLifetime := func(m *models.Mock) *models.Mock {
		m.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimeSession
		return m
	}
	failure := func(s float64) *models.Mock {
		m := grpc("Refused", s)
		m.Kind = models.ConnectionFailure
		return m
	}
	const (
		strictMode = true
		laxMode    = false
		perTest    = false
		session    = true
	)
	for _, tc := range []holdRow{
		{"placed in the window", grpc("Window", 71), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"placed at the window's start exactly", grpc("AtStart", 70), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"placed at the window's end exactly", grpc("AtEnd", 72), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"a stated mock placed in the window", statedPerTest(http("Window", 71)), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"no recorded time", stamped(grpc("Untimed", 0), time.Time{}, time.Time{}), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"a request time outside the window and no response time", stamped(grpc("NoAnswerTime", 0), at(65), time.Time{}), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"a response time outside the window and no request time", stamped(grpc("NoAskTime", 0), time.Time{}, at(65)), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"in flight when the window began", answered(grpc("InFlight", 69.5), 70.5), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"a stated mock in flight when the window began, lax mode", answered(statedPerTest(http("InFlight", 69.5)), 70.5), perTest, laxMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"in flight when the window began, lax mode (promoted)", answered(grpc("InFlight", 69.5), 70.5), perTest, laxMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"in flight into the next window", answered(grpc("InFlightLater", 79.5), 80.5), perTest, strictMode, pkg.ServedSession, pkg.NotServed, pkg.ServedSession, false},
		{"sent and answered in the gap before the window", grpc("InGap", 69.5), perTest, strictMode, pkg.ServedSession, pkg.NotServed, pkg.ServedSession, false},
		{"a stated mock in the gap after the window", statedPerTest(http("After", 75)), perTest, strictMode, pkg.NotServed, pkg.NotServed, pkg.NotServed, false},
		{"a stated carry-over call in the gap after the window", statedPerTest(http(noAnswerCarryOverOp, 73)), perTest, strictMode, pkg.ServedReleased, pkg.ServedReleased, pkg.NotServed, true},
		{"a stated carry-over call the next window holds, reached a lookahead before it", statedPerTest(http(noAnswerCarryOverOp, 80.5)), perTest, strictMode, pkg.ServedReleased, pkg.ServedReleased, pkg.NotServed, true},
		{"a stated carry-over call in the gap before the window", statedPerTest(http(noAnswerCarryOverOp, 65)), perTest, strictMode, pkg.NotServed, pkg.NotServed, pkg.NotServed, false},
		{"an unstated carry-over call in the gap after the window (promoted resident)", grpc(noAnswerCarryOverOp, 73), perTest, strictMode, pkg.ServedReleased, pkg.ServedReleased, pkg.ServedSession, true},
		{"an unstated carry-over call the next window holds, reached a lookahead before it", grpc(noAnswerCarryOverOp, 80.5), perTest, strictMode, pkg.ServedReleased, pkg.ServedReleased, pkg.ServedSession, true},
		{"an unstated carry-over call in the gap before the window, released at an earlier window", grpc(noAnswerCarryOverOp, 65), perTest, strictMode, pkg.ServedSession, pkg.NotServed, pkg.ServedSession, false},
		{"an unstated carry-over call in the gap after the window, lax mode (promoted)", grpc(noAnswerCarryOverOp, 73), perTest, laxMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"a server push in the gap after the window", grpc(noAnswerPushOp, 73), perTest, strictMode, pkg.ServedReleased, pkg.ServedReleased, pkg.ServedReleased, true},
		{"a server push in the gap after the window, lax mode", grpc(noAnswerPushOp, 73.5), perTest, laxMode, pkg.ServedReleased, pkg.ServedReleased, pkg.ServedReleased, true},
		{"a server push in the gap before the window (promoted resident)", grpc(noAnswerPushOp, 65), perTest, strictMode, pkg.ServedSession, pkg.NotServed, pkg.ServedSession, false},
		{"a server push in the window", grpc(noAnswerPushOp, 71), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"startup traffic", grpc("Boot", 5), perTest, strictMode, pkg.ServedStartup, pkg.ServedStartup, pkg.ServedStartup, false},
		{"answered before asked", stamped(grpc("Skewed", 0), at(71), at(70.9)), perTest, strictMode, pkg.NotServed, pkg.NotServed, pkg.NotServed, false},
		{"a session-lifetime mock in the window", sessionLifetime(http("Session", 71)), perTest, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
		{"a session-lifetime mock in the gap after the window", sessionLifetime(http("Session", 75)), perTest, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"a config-tagged mock in the gap after the window", tagged(grpc("Config", 75), "config"), perTest, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"a connection-tagged mock in the window", tagged(grpc("Conn", 71), "connection"), perTest, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
		{"a connection failure in the window", failure(71), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"a connection failure in the gap after the window", failure(75), perTest, strictMode, pkg.NotServed, pkg.NotServed, pkg.NotServed, false},
		{"a connection failure before the first window", failure(5), perTest, strictMode, pkg.ServedStartup, pkg.ServedStartup, pkg.ServedStartup, false},
		{"a connection failure with no recorded time", stamped(failure(0), time.Time{}, time.Time{}), perTest, strictMode, pkg.NotServed, pkg.NotServed, pkg.NotServed, false},
		{"a session-lifetime mock before the first window", sessionLifetime(http("SessionBoot", 5)), perTest, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"a session-pool mock in the window", http("Shared", 71), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
		{"a session-pool mock in the gap after the window", http("Shared", 75), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"a session-pool mock with no recorded time", stamped(http("Shared", 0), time.Time{}, time.Time{}), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
		{"a session-pool mock at the window's start exactly", http("Shared", 70), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
		{"a session-pool mock at the window's end exactly", http("Shared", 72), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
		{"a session-pool mock just after the window's end", stamped(http("Shared", 0), at(72.0005), at(72.001)), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"a session-pool mock with a request time outside the window and no response time", stamped(http("Shared", 0), at(65), time.Time{}), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
		{"a session-pool mock with a response time outside the window and no request time", stamped(http("Shared", 0), time.Time{}, at(65)), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
		{"a session-lifetime mock answered before asked (the session short-circuit comes first)", stamped(http("Shared", 0), at(71), at(70.9)), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
	} {
		t.Run(tc.name, func(t *testing.T) { checkHoldRow(t, cases, tc) })
	}
}

// A server push recorded in the gap after the unanswered test, which the push
// hub delivers from that test's window on (models.WindowSchedule.Released), and
// which went unconsumed, survives the prune around that test on both formats,
// as the carry-over call in the same gap does.
func TestRunTestSetHoldsAServerPushReleasedAtTheUnansweredTestsWindow(t *testing.T) {
	timeout := prClientTimeoutErr(t)
	for _, format := range []string{"yaml", "gob"} {
		t.Run(format, func(t *testing.T) {
			h, set, extras, _ := noAnswerHoldRun(t, format, func(at noAnswerAt) *models.Mock {
				return noAnswerHoldMock(models.GRPC_EXPORT, noAnswerPushOp, at(73.5))
			})
			push := extras[0]
			hooks := h.replayer.hookImpl.(prHooks)
			hooks.simErr = map[string]error{"test-7": timeout}
			h.replayer.hookImpl = hooks
			_ = h.run(t)
			left := storedMockNames(t, h)
			if left[set.orphan.Name] {
				t.Fatalf("precondition: not pruned, keeps %v", sortedNames(left))
			}
			if !left[set.carriedOver.Name] {
				t.Fatalf("precondition: the carry-over call in the same gap was pruned, keeps %v", sortedNames(left))
			}
			if !left[push.Name] {
				t.Errorf("the push released at test-7's window (%s) was pruned; keeps %v", push.Name, sortedNames(left))
			}
		})
	}
}

// A carry-over call whose recorder stated no lifetime, as enterprise's Pulsar
// SEND and Redis carry-over mocks are, recorded in the gap after the
// unanswered test and unconsumed, survives the prune around that test on both
// formats. The agent's default, the strict window over its on-disk store,
// loads it by the carry-over lookahead at that test's window and serves it
// from its carry-over tier there (TestUpdateMockParamsServesAnUnstatedCarryOverFromTheCarryTier
// in pkg/service/agent); no window filter sees it to promote it to the session
// pool.
func TestRunTestSetHoldsAnUnstatedCarryOverReleasedAtTheUnansweredTestsWindow(t *testing.T) {
	timeout := prClientTimeoutErr(t)
	for _, format := range []string{"yaml", "gob"} {
		t.Run(format, func(t *testing.T) {
			h, set, extras, _ := noAnswerHoldRun(t, format, func(at noAnswerAt) *models.Mock {
				return noAnswerHoldMock(models.GRPC_EXPORT, noAnswerCarryOverOp, at(73.2))
			})
			carry := extras[0]
			if carry.StatesLifetime() {
				t.Fatal("precondition: the carry-over call states a lifetime")
			}
			hooks := h.replayer.hookImpl.(prHooks)
			hooks.simErr = map[string]error{"test-7": timeout}
			h.replayer.hookImpl = hooks
			_ = h.run(t)
			left := storedMockNames(t, h)
			if left[set.orphan.Name] {
				t.Fatalf("precondition: not pruned, keeps %v", sortedNames(left))
			}
			if !left[set.carriedOver.Name] {
				t.Fatalf("precondition: the stated carry-over call in the same gap was pruned, keeps %v", sortedNames(left))
			}
			if !left[carry.Name] {
				t.Errorf("the unstated carry-over call test-7's window releases (%s) was pruned; keeps %v", carry.Name, sortedNames(left))
			}
		})
	}
}

// heapSampling is the YAML mock store keploy replays with, sampling the heap
// wherever a read hands the reader mocks: in a walk's visitor, before each
// mock reaches the reader's, and as every other read returns its mocks, while
// they are held. peak is the most the heap grew over base; seen counts the
// mocks a walk visited.
type heapSampling struct {
	*mockdb.MockYaml
	base uint64
	peak *uint64
	seen *int
}

func (h heapSampling) sample() {
	runtime.GC()
	var st runtime.MemStats
	runtime.ReadMemStats(&st)
	if st.HeapAlloc > h.base && st.HeapAlloc-h.base > *h.peak {
		*h.peak = st.HeapAlloc - h.base
	}
}

func (h heapSampling) WalkTestSetMocks(ctx context.Context, set string, visit func(m *models.Mock, perTest, session bool)) error {
	return h.MockYaml.WalkTestSetMocks(ctx, set, func(m *models.Mock, perTest, session bool) {
		*h.seen++
		h.sample()
		visit(m, perTest, session)
	})
}

func (h heapSampling) IndexTestSetMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) (models.TestSetMocks, error) {
	out, err := h.MockYaml.IndexTestSetMocks(ctx, set, after, before, have, need)
	h.sample()
	return out, err
}

func (h heapSampling) GetTestSetMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) (models.TestSetMocks, error) {
	out, err := h.MockYaml.GetTestSetMocks(ctx, set, after, before, have, need)
	h.sample()
	return out, err
}

func (h heapSampling) GetFilteredMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) ([]*models.Mock, error) {
	out, err := h.MockYaml.GetFilteredMocks(ctx, set, after, before, have, need)
	h.sample()
	return out, err
}

func (h heapSampling) GetUnFilteredMocks(ctx context.Context, set string, after, before time.Time, have, need map[string]bool) ([]*models.Mock, error) {
	out, err := h.MockYaml.GetUnFilteredMocks(ctx, set, after, before, have, need)
	h.sample()
	return out, err
}

// The hold reads the set's mock file one mock at a time and keeps only the
// names of those it holds, so that a mock file of gigabytes cannot run the
// replayer out of memory: over a set of 1 MB mocks many times larger than
// the bound, on both formats, the heap grows by no more than a fixed bound of
// a few mocks' worth while the hold reads, however large the set.
// KEPLOY_TEST_HOLD_HEAP_SET_MB sets each format's set size in MB (64 by
// default; the no-OOM row runs it at 1024 on a test box, where the heap grew
// about 2.3 MB on both formats, as at 64).
func TestHoldNoAnswerMocksHoldsOneMockAtATime(t *testing.T) {
	const size = 1 << 20
	const bound = 8 * size
	n := 64
	if v := os.Getenv("KEPLOY_TEST_HOLD_HEAP_SET_MB"); v != "" {
		mb, err := strconv.Atoi(v)
		if err != nil || mb < 32 {
			t.Fatalf("KEPLOY_TEST_HOLD_HEAP_SET_MB=%q: want a whole number of MB, at least 32", v)
		}
		n = mb
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	var cases []*models.TestCase
	for i := 1; i <= 2; i++ {
		tc := &models.TestCase{Name: fmt.Sprintf("test-%d", i), Kind: models.HTTP}
		tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = at(float64(10*i)), at(float64(10*i+2))
		cases = append(cases, tc)
	}
	for _, format := range []string{"yaml", "gob"} {
		t.Run(format, func(t *testing.T) {
			if format == "gob" {
				t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
			} else {
				t.Setenv("KEPLOY_MOCK_FORMAT", "")
			}
			dir := t.TempDir()
			ys := mockdb.New(zap.NewNop(), dir, "mocks")
			// Every other mock is recorded in test-2's window, which the hold
			// keeps by name; the rest in the gap before it, which it does not.
			for i := 0; i < n; i++ {
				s := 14 + 4*float64(i)/float64(n)
				if i%2 == 0 {
					s = 20 + 2*float64(i)/float64(n)
				}
				m := noAnswerHoldMock(models.HTTP, fmt.Sprintf("big-%d", i), at(s))
				m.Spec.HTTPResp.Body = strings.Repeat(string(rune('a'+i%26)), size)
				if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
					t.Fatalf("InsertMock: %v", err)
				}
			}
			// The gob writer is asynchronous; Close drains it to the file.
			if err := ys.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			var peak uint64
			seen := 0
			keep := map[string]models.MockState{}
			runtime.GC()
			var st runtime.MemStats
			runtime.ReadMemStats(&st)
			store := heapSampling{MockYaml: mockdb.New(zap.NewNop(), dir, "mocks"), base: st.HeapAlloc, peak: &peak, seen: &seen}
			r := &Replayer{logger: zap.NewNop(), mockDB: store, config: &config.Config{}}
			r.config.Test.StrictMockWindow = true
			held, err := r.holdNoAnswerMocks(context.Background(), "set-0", []string{"test-2"}, cases,
				newCarryOverAttribution(cases), false, nil, nil, keep)
			if err != nil {
				t.Fatalf("hold: %v", err)
			}
			t.Logf("%s: %d mocks of %d bytes, the heap grew at most %d bytes while the hold read them", format, n, size, peak)
			if peak > bound {
				t.Fatalf("the heap grew %d bytes while the hold read a set of %d mocks of %d bytes: it holds more than a few mocks at a time (bound %d)", peak, n, size, bound)
			}
			if seen < n {
				t.Fatalf("the hold's walk visited %d of the set's %d mocks", seen, n)
			}
			if len(held) != 1 || held[0].kept != n/2 || len(keep) != n/2 {
				t.Fatalf("precondition: test-2 holds %+v, keep %d mocks; want the %d recorded in its window", held, len(keep), n/2)
			}
		})
	}
}
