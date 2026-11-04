package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A call held to its staging (HoldToRound) is answered once more only while
// the staging is still the call's. A restage can land between the call's
// read of the test's answered recordings and its ask to answer once more,
// and leave the same exact view in force: the view's own recordings then
// still pass the manager's check, so only the held store's own check stops
// the old call from being answered once more under the set staged after it.
// The call keeps the restaging as its refusal, and the refusal spends
// nothing: a call of the new staging is still answered once more.
func TestACallHeldToAStagingThatMovedIsNotAnsweredOnceMore(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	r0 := readMock("r-0", t0, "GET", "/api/r", "", `{}`)
	r1 := readMock("r-1", t0.Add(time.Second), "GET", "/api/r", "", `{}`)
	r := &models.RepeatReadMark{Name: "r", At: 1}
	r0.TestModeInfo.RepeatRead, r1.TestModeInfo.RepeatRead = r, r
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, []*models.Mock{r0, r1}, models.BaseTime, time.Now().Add(time.Hour))
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("t", []*models.Mock{r0, r1})
	for range 2 {
		if miss := sendForMiss(t, mm, models.OutgoingOptions{}, nil, readCall("GET", "/api/r", "")); miss != nil {
			t.Fatalf("a recorded call missed: %v", miss)
		}
	}
	held, restaged := mm.HoldToRound(integrations.NewCallRound(mm.stagingEpoch.Load(), nil))
	answered := held.(integrations.ExactConsumedReader).ConsumedTestMocks()
	if len(answered) != 2 || restaged() {
		t.Fatalf("precondition: the held store read %d answered recordings (restaged %v); want the read's 2", len(answered), restaged())
	}
	v := mm.exactView()
	mm.stagingEpoch.Add(1) // the set staged again, the same view in force
	if mm.exactView() != v {
		t.Fatal("precondition: the restage installed another view")
	}
	answerer, ok := held.(integrations.RepeatReadAnswerer)
	if !ok {
		t.Fatal("the held store answers no repeated read")
	}
	if again, ok := answerer.AnswerRepeatRead(answered); ok {
		t.Fatalf("a call held to a staging that moved was answered once more with %s", again.Mock.Name)
	}
	if !restaged() {
		t.Error("the call that asked after the staging moved does not say it was restaged")
	}
	if why := held.(interface{ Moved() string }).Moved(); why != restagedWhy {
		t.Errorf("the call keeps %q as its refusal; want %q", why, restagedWhy)
	}
	again, ok := mm.AnswerRepeatRead(answered)
	if !ok || again.Mock.Name != "r-1" {
		t.Fatalf("a call of the new staging answered once more %+v (%v); want the read's last recording r-1: the refusal spent it", again, ok)
	}
}
