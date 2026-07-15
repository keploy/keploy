package record

import (
	"context"
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

func TestNonLaneEgressUntouched(t *testing.T) {
	r := newAsyncRec()
	m := egress("normal", time.Unix(2000, 0))
	_ = r.BeforeMockInsert(context.Background(), &MockContext{Mock: m})
	if _, ok := m.Spec.Metadata[models.MetaAsync]; ok {
		t.Fatalf("non-lane egress must not be stamped: %+v", m.Spec.Metadata)
	}
}
