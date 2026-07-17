package record

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// laneStub matches any mock whose metadata has type == "lane".
type laneStub struct{}

func (laneStub) MatchesLane(m *models.Mock, _ models.AsyncLane) bool {
	return m != nil && m.Spec.Metadata["kind"] == "lane"
}
func (laneStub) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) {
	return true, ""
}
func (laneStub) EmptyResponse(_ models.AsyncLane) ([]byte, error) { return nil, nil }

func egress(kind string, completedAt time.Time) *models.Mock {
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"kind": kind},
		ResTimestampMock: completedAt, // async COMPLETION time drives the anchor
	}}
}

func newAsyncRec() *AsyncRecorder {
	lane := models.AsyncLane{Name: "L", Type: "http"}
	return NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"http": laneStub{}})
}

func TestAnchorIsEffectiveTestcaseDuringWindow(t *testing.T) {
	r := newAsyncRec()
	base := time.Unix(1000, 0)
	// T1 starts at base, T2 starts at base+5s (window START = HTTPReq.Timestamp)
	_ = r.AfterTestCaseInsert(context.Background(), &TestCaseContext{
		TestCase: &models.TestCase{Name: "T1", HTTPReq: models.HTTPReq{Timestamp: base}}})
	_ = r.AfterTestCaseInsert(context.Background(), &TestCaseContext{
		TestCase: &models.TestCase{Name: "T2", HTTPReq: models.HTTPReq{Timestamp: base.Add(5 * time.Second)}}})

	// delivery COMPLETES mid-T2 (base+6s): effective testcase = T2, effect from T3.
	m := egress("lane", base.Add(6*time.Second))
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})

	a := m.Spec.Async
	if a == nil || a.Lane != "L" {
		t.Fatalf("not stamped async: %+v", a)
	}
	if a.AnchorAfter != "T2" || a.AnchorPos != 2 || a.Seq != 1 {
		t.Fatalf("delivery mid-T2 must anchor to effective testcase T2/pos2: %+v", a)
	}
}

func TestAnchorInGapUsesLastStartedTest(t *testing.T) {
	r := newAsyncRec()
	base := time.Unix(1000, 0)
	_ = r.AfterTestCaseInsert(context.Background(), &TestCaseContext{
		TestCase: &models.TestCase{Name: "T1", HTTPReq: models.HTTPReq{Timestamp: base}}})
	// delivery completes after T1 started, before any later test -> anchor T1/pos1
	m := egress("lane", base.Add(2*time.Second))
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})
	if a := m.Spec.Async; a == nil || a.AnchorAfter != "T1" || a.AnchorPos != 1 {
		t.Fatalf("gap delivery must anchor to last started test T1/pos1: %+v", m.Spec.Async)
	}
}

func TestStartupAnchorBeforeFirstTest(t *testing.T) {
	r := newAsyncRec()
	m := egress("lane", time.Unix(500, 0))
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})
	if a := m.Spec.Async; a == nil || a.AnchorAfter != models.AnchorStartup || a.AnchorPos != 0 {
		t.Fatalf("pre-first-test egress must anchor to startup/0: %+v", m.Spec.Async)
	}
}

// TestAnchorOrderIndependentWithStartupNamedTest proves the anchor tie-break
// no longer relies on models.AnchorStartup as a "no candidate yet" sentinel:
// a testcase literally named "startup" that started LATE must still win over
// an earlier-started "T1", regardless of insertion order.
func TestAnchorOrderIndependentWithStartupNamedTest(t *testing.T) {
	base := time.Unix(1000, 0)
	early := base                          // T1 starts here
	late := base.Add(5 * time.Second)      // "startup"-named window starts here
	delivery := base.Add(10 * time.Second) // completes after both

	insertAndCheck := func(t *testing.T, insertStartupFirst bool) {
		r := newAsyncRec()
		startupWindow := &TestCaseContext{
			TestCase: &models.TestCase{Name: "startup", HTTPReq: models.HTTPReq{Timestamp: late}}}
		t1Window := &TestCaseContext{
			TestCase: &models.TestCase{Name: "T1", HTTPReq: models.HTTPReq{Timestamp: early}}}

		if insertStartupFirst {
			_ = r.AfterTestCaseInsert(context.Background(), startupWindow)
			_ = r.AfterTestCaseInsert(context.Background(), t1Window)
		} else {
			_ = r.AfterTestCaseInsert(context.Background(), t1Window)
			_ = r.AfterTestCaseInsert(context.Background(), startupWindow)
		}

		m := egress("lane", delivery)
		_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})

		a := m.Spec.Async
		if a == nil || a.AnchorAfter != "startup" || a.AnchorPos != 2 {
			t.Fatalf("expected anchor to latest-started window named %q at pos 2, got: %+v",
				"startup", a)
		}
	}

	t.Run("startup inserted first", func(t *testing.T) { insertAndCheck(t, true) })
	t.Run("startup inserted second", func(t *testing.T) { insertAndCheck(t, false) })
}

// A lane declared WITHOUT a name must still stamp a stable, non-empty lane
// key — the deterministic EffectiveName — so the replay engine (which derives
// the same key from the same lane config) can find the stream.
func TestGeneratedLaneNameStampedWhenNameOmitted(t *testing.T) {
	lane := models.AsyncLane{Type: "http", Match: map[string]string{"pathRegex": "^/poll$"}}
	r := NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"http": laneStub{}})

	m := egress("lane", time.Unix(500, 0))
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})

	want := lane.EffectiveName()
	if want == "" {
		t.Fatal("EffectiveName must be non-empty for a nameless lane")
	}
	if m.Spec.Async == nil || m.Spec.Async.Lane != want {
		t.Fatalf("stamped lane = %+v, want generated lane %q", m.Spec.Async, want)
	}
}

func TestNonLaneEgressUntouched(t *testing.T) {
	r := newAsyncRec()
	m := egress("normal", time.Unix(2000, 0))
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})
	if m.Spec.Async != nil {
		t.Fatalf("non-lane egress must not be stamped: %+v", m.Spec.Async)
	}
}

// A poll lane (Type ends in "Poll") sets Async.Poll + PollDurationMs and leaves
// the mock's base kind (Http) unchanged — poll-ness is Async.Poll, not a Kind.
func TestPollLaneStampsPollMeta(t *testing.T) {
	lane := models.AsyncLane{Type: "httpPoll", Match: map[string]string{"x": "y"}}
	r := NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"http": laneStub{}}) // keyed by BaseType

	m := egress("lane", time.Unix(1000, 0))
	m.Kind = models.HTTP
	m.Spec.ReqTimestampMock = time.Unix(999, 0) // open 1s before resolve
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})

	a := m.Spec.Async
	if a == nil || !a.Poll {
		t.Fatalf("poll lane must set Async.Poll: %+v", a)
	}
	if a.PollDurationMs != 1000 {
		t.Fatalf("PollDurationMs = %d want 1000", a.PollDurationMs)
	}
	if m.Kind != models.HTTP {
		t.Fatalf("poll-lane mock kind = %q want Http (poll-ness is not a kind)", m.Kind)
	}
}

// TestPollLaneClampsNegativePollDurationToZero pins the clamp at
// asynchook.go ~lines 97-100: a poll mock whose ResTimestampMock lands BEFORE
// its ReqTimestampMock (clock skew / test fixture oddity) must record
// pollDurationMs as "0", never a negative number.
func TestPollLaneClampsNegativePollDurationToZero(t *testing.T) {
	lane := models.AsyncLane{Type: "httpPoll", Match: map[string]string{"x": "y"}}
	r := NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"http": laneStub{}}) // keyed by BaseType

	m := egress("lane", time.Unix(1000, 0)) // ResTimestampMock = 1000
	m.Kind = models.HTTP
	m.Spec.ReqTimestampMock = time.Unix(1001, 0) // opens AFTER resolve -> negative raw duration
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})

	a := m.Spec.Async
	if a == nil || !a.Poll {
		t.Fatalf("poll lane must set Async.Poll: %+v", a)
	}
	if a.PollDurationMs != 0 {
		t.Fatalf("PollDurationMs = %d want clamped 0 (ResTimestampMock before ReqTimestampMock)",
			a.PollDurationMs)
	}
}

// TestAsyncPollMockExcludedFromPerTestMapping pins the invariant that an async
// mock — including a held poll delivery (Async.Poll=true, kind still Http) —
// never appears in a testcase's per-test mock mapping. resolveMappingEntries
// excludes purely on the async marker (asyncMockIDs), never on Kind, so this
// must hold. It pins the seam at pkg/service/record/record.go's Mock-loop
// goroutine (`if mock.Spec.Async != nil` stores the tempID into asyncMockIDs)
// feeding resolveMappingEntries. That full wiring lives inside Recorder.Start,
// which requires a live agent connection, so this test reaches
// resolveMappingEntries directly and reconstructs the Mock-loop's asyncMockIDs
// bookkeeping by hand — noted here as the reachability limit.
func TestAsyncPollMockExcludedFromPerTestMapping(t *testing.T) {
	lane := models.AsyncLane{Type: "httpPoll", Match: map[string]string{"x": "y"}}
	rec := NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"http": laneStub{}})

	pollMock := egress("lane", time.Unix(1000, 0))
	pollMock.Name = "poll-mock-1"
	pollMock.Kind = models.HTTP
	pollMock.Spec.ReqTimestampMock = time.Unix(999, 0)
	_ = rec.BeforeMockInsert(context.Background(), &MockContext{Mock: pollMock})

	if pollMock.Spec.Async == nil || !pollMock.Spec.Async.Poll {
		t.Fatalf("setup: expected poll mock stamped async+poll, got %+v", pollMock.Spec.Async)
	}
	if pollMock.Kind != models.HTTP {
		t.Fatalf("setup: poll mock keeps kind Http (poll-ness is not a kind), got %q", pollMock.Kind)
	}

	// Mirror record.go's Mock-loop bookkeeping: every mock lands in
	// correlationMap, but only async ones (Spec.Async != nil) also land in
	// asyncMockIDs. An ordinary sync mock gets no asyncMockIDs entry.
	var correlationMap, asyncMockIDs sync.Map
	correlationMap.Store(pollMock.Name, models.MockEntry{Name: pollMock.Name, Kind: string(pollMock.Kind)})
	asyncMockIDs.Store(pollMock.Name, struct{}{})

	const syncMockName = "sync-mock-1"
	correlationMap.Store(syncMockName, models.MockEntry{Name: syncMockName, Kind: string(models.HTTP)})

	r := &Recorder{logger: zap.NewNop()}
	mapping := models.TestMockMapping{TestName: "T1", MockIDs: []string{pollMock.Name, syncMockName}}
	got := r.resolveMappingEntries(mapping, &correlationMap, &asyncMockIDs)

	for _, e := range got {
		if e.Name == pollMock.Name {
			t.Fatalf("poll mock %q must be excluded from per-test mapping, got entries: %+v", pollMock.Name, got)
		}
	}
	if len(got) != 1 || got[0].Name != syncMockName {
		t.Fatalf("expected only the sync mock in the per-test mapping, got: %+v", got)
	}
}
