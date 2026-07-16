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

	md := m.Spec.Metadata
	if md[models.MetaAsync] != "true" || md[models.MetaAsyncLane] != "L" {
		t.Fatalf("not stamped async: %+v", md)
	}
	if md[models.MetaAnchorAfter] != "T2" || md[models.MetaAnchorPos] != "2" || md[models.MetaAsyncSeq] != "1" {
		t.Fatalf("delivery mid-T2 must anchor to effective testcase T2/pos2: %+v", md)
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
	if m.Spec.Metadata[models.MetaAnchorAfter] != "T1" || m.Spec.Metadata[models.MetaAnchorPos] != "1" {
		t.Fatalf("gap delivery must anchor to last started test T1/pos1: %+v", m.Spec.Metadata)
	}
}

func TestStartupAnchorBeforeFirstTest(t *testing.T) {
	r := newAsyncRec()
	m := egress("lane", time.Unix(500, 0))
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})
	if m.Spec.Metadata[models.MetaAnchorAfter] != models.AnchorStartup ||
		m.Spec.Metadata[models.MetaAnchorPos] != "0" {
		t.Fatalf("pre-first-test egress must anchor to startup/0: %+v", m.Spec.Metadata)
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

		md := m.Spec.Metadata
		if md[models.MetaAnchorAfter] != "startup" || md[models.MetaAnchorPos] != "2" {
			t.Fatalf("expected anchor to latest-started window named %q at pos 2, got: %+v",
				"startup", md)
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
	if got := m.Spec.Metadata[models.MetaAsyncLane]; got != want {
		t.Fatalf("stamped lane = %q, want generated %q", got, want)
	}
	if m.Spec.Metadata[models.MetaAsync] != "true" {
		t.Fatalf("nameless lane egress must still be stamped async: %+v", m.Spec.Metadata)
	}
}

func TestNonLaneEgressUntouched(t *testing.T) {
	r := newAsyncRec()
	m := egress("normal", time.Unix(2000, 0))
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})
	if _, ok := m.Spec.Metadata[models.MetaAsync]; ok {
		t.Fatalf("non-lane egress must not be stamped: %+v", m.Spec.Metadata)
	}
}

// A poll lane (Type ends in "Poll") stamps MetaAsyncPoll + pollDurationMs and
// re-kinds the mock via the registry; a non-poll lane leaves both untouched.
func TestPollLaneStampsPollMetaAndReKinds(t *testing.T) {
	models.RegisterPollKind(models.HTTP, models.HttpPoll)
	lane := models.AsyncLane{Type: "httpPoll", Match: map[string]string{"x": "y"}}
	r := NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"http": laneStub{}}) // keyed by BaseType

	m := egress("lane", time.Unix(1000, 0))
	m.Kind = models.HTTP
	m.Spec.ReqTimestampMock = time.Unix(999, 0) // open 1s before resolve
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})

	if m.Spec.Metadata[models.MetaAsyncPoll] != "true" {
		t.Fatalf("poll lane must stamp MetaAsyncPoll: %+v", m.Spec.Metadata)
	}
	if m.Spec.Metadata[models.MetaPollDurationMs] != "1000" {
		t.Fatalf("pollDurationMs = %q want 1000", m.Spec.Metadata[models.MetaPollDurationMs])
	}
	if m.Kind != models.HttpPoll {
		t.Fatalf("poll-lane mock kind = %q want HttpPoll", m.Kind)
	}
}

// TestPollLaneClampsNegativePollDurationToZero pins the clamp at
// asynchook.go ~lines 97-100: a poll mock whose ResTimestampMock lands BEFORE
// its ReqTimestampMock (clock skew / test fixture oddity) must record
// pollDurationMs as "0", never a negative number.
func TestPollLaneClampsNegativePollDurationToZero(t *testing.T) {
	models.RegisterPollKind(models.HTTP, models.HttpPoll)
	lane := models.AsyncLane{Type: "httpPoll", Match: map[string]string{"x": "y"}}
	r := NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"http": laneStub{}}) // keyed by BaseType

	m := egress("lane", time.Unix(1000, 0)) // ResTimestampMock = 1000
	m.Kind = models.HTTP
	m.Spec.ReqTimestampMock = time.Unix(1001, 0) // opens AFTER resolve -> negative raw duration
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})

	if m.Spec.Metadata[models.MetaAsyncPoll] != "true" {
		t.Fatalf("poll lane must stamp MetaAsyncPoll: %+v", m.Spec.Metadata)
	}
	if m.Spec.Metadata[models.MetaPollDurationMs] != "0" {
		t.Fatalf("pollDurationMs = %q want clamped 0 (ResTimestampMock before ReqTimestampMock)",
			m.Spec.Metadata[models.MetaPollDurationMs])
	}
}

// TestAsyncPollMockExcludedFromPerTestMapping pins the invariant that a mock
// stamped async — including a poll delivery stamped MetaAsyncPoll="true" and
// re-kinded to models.HttpPoll — never appears in a testcase's per-test mock
// mapping. resolveMappingEntries excludes purely on MetaAsync (asyncMockIDs),
// never on Kind, so this must hold regardless of the mock's re-kinding. This
// pins the seam at pkg/service/record/record.go's Mock-loop goroutine
// (~line 593: `if mock.Spec.Metadata[models.MetaAsync] == "true"` stores the
// tempID into asyncMockIDs) feeding resolveMappingEntries (~line 91). That
// full wiring lives inside Recorder.Start, which requires a live agent
// connection, so this test reaches resolveMappingEntries directly and
// reconstructs the Mock-loop's asyncMockIDs bookkeeping by hand — noted here
// as the reachability limit per the review brief.
func TestAsyncPollMockExcludedFromPerTestMapping(t *testing.T) {
	models.RegisterPollKind(models.HTTP, models.HttpPoll)
	lane := models.AsyncLane{Type: "httpPoll", Match: map[string]string{"x": "y"}}
	rec := NewAsyncRecorder(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"http": laneStub{}})

	pollMock := egress("lane", time.Unix(1000, 0))
	pollMock.Name = "poll-mock-1"
	pollMock.Kind = models.HTTP
	pollMock.Spec.ReqTimestampMock = time.Unix(999, 0)
	_ = rec.BeforeMockInsert(context.Background(), &MockContext{Mock: pollMock})

	if pollMock.Spec.Metadata[models.MetaAsync] != "true" || pollMock.Spec.Metadata[models.MetaAsyncPoll] != "true" {
		t.Fatalf("setup: expected poll mock stamped async+poll, got %+v", pollMock.Spec.Metadata)
	}
	if pollMock.Kind != models.HttpPoll {
		t.Fatalf("setup: expected poll mock re-kinded to HttpPoll, got %q", pollMock.Kind)
	}

	// Mirror record.go's Mock-loop bookkeeping (~lines 593-595): every mock
	// lands in correlationMap, but only ones stamped MetaAsync also land in
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
