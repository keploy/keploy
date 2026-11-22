package proxy

import (
	"sync/atomic"
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
	mm, held, restaged, answered := heldAfterARepeatedRead(t)
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

// A restage that lands while a held call is being answered once more lands
// before the call's staging check, and the call is refused, or after its
// answer: never between them, where the old call would be answered once
// more under the set staged after it. The row starts a restage at the
// instant between the check and the answer (onAnswerAgain) and holds it
// just before it takes the round lock: when the lock is free there, the
// restage lands there, and the call must be refused, spending nothing. The
// check and the answer hold the lock, so the restage waits, lands after the
// answer, and the call's next ask is refused for it.
func TestARestageNeverLandsBetweenAHeldCallsCheckAndItsAnswerOnceMore(t *testing.T) {
	mm, held, restaged, answered := heldAfterARepeatedRead(t)
	epoch := mm.stagingEpoch.Load()
	round := &mm.exact.round
	var armed atomic.Bool
	waiting, letGo, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	round.onWait = func() {
		if armed.CompareAndSwap(true, false) {
			close(waiting)
			<-letGo
		}
	}
	landedBetween, hooked := false, false
	round.onAnswerAgain = func() {
		round.onAnswerAgain = nil // once: the call's next ask runs no restage
		hooked = true
		armed.Store(true)
		go func() {
			defer close(done)
			mm.bumpStagingEpoch()
		}()
		<-waiting
		free := round.mu.TryLock()
		if free {
			round.mu.Unlock()
		}
		close(letGo)
		if free {
			<-done
			landedBetween = true
		}
	}
	t.Cleanup(func() { round.onWait, round.onAnswerAgain = nil, nil })
	again, ok := held.(integrations.RepeatReadAnswerer).AnswerRepeatRead(answered)
	<-done
	if !hooked {
		t.Fatal("precondition: no held call reached its answer once more")
	}
	if mm.stagingEpoch.Load() == epoch {
		t.Fatal("precondition: the restage did not land")
	}
	if landedBetween {
		if ok {
			t.Fatalf("a restage landed between a held call's staging check and its answer, and the old call was answered once more with %s", again.Mock.Name)
		}
		t.Fatal("a restage landed between a held call's staging check and its answer: the round lock does not hold them together")
	}
	if !ok || again.Mock.Name != "r-1" {
		t.Fatalf("the held call, its staging in force through its check and answer, was answered once more %+v (%v); want the read's last recording r-1", again, ok)
	}
	if _, ok := held.(integrations.RepeatReadAnswerer).AnswerRepeatRead(answered); ok {
		t.Error("the held call's next ask, after the restage landed, was answered once more")
	}
	if !restaged() {
		t.Error("the call that asked after the staging moved does not say it was restaged")
	}
}

// A restage that lands as a held call goes for the round lock to be
// answered once more (onWait, before the lock is taken) is seen by the
// call's staging check, made holding the lock: the call is refused.
func TestARestageAsAHeldCallGoesForTheRoundLockIsSeenByItsCheck(t *testing.T) {
	mm, held, restaged, answered := heldAfterARepeatedRead(t)
	round := &mm.exact.round
	var armed atomic.Bool
	armed.Store(true)
	round.onWait = func() {
		if armed.CompareAndSwap(true, false) {
			mm.bumpStagingEpoch()
		}
	}
	t.Cleanup(func() { round.onWait = nil })
	if again, ok := held.(integrations.RepeatReadAnswerer).AnswerRepeatRead(answered); ok {
		t.Fatalf("a held call whose staging moved as it went for the round lock was answered once more with %s", again.Mock.Name)
	}
	if armed.Load() {
		t.Fatal("precondition: the held call took no round lock to be answered once more")
	}
	if !restaged() {
		t.Error("the call does not say it was restaged")
	}
	if again, ok := mm.AnswerRepeatRead(answered); !ok || again.Mock.Name != "r-1" {
		t.Fatalf("a call of the new staging answered once more %+v (%v); want the read's last recording r-1: the refusal spent it", again, ok)
	}
}

// heldAfterARepeatedRead is a manager serving a test whose read GET /api/r
// was recorded twice with one answer and made twice, and a call held to the
// staging it was marked in, with the test's answered recordings it read.
func heldAfterARepeatedRead(t *testing.T) (*MockManager, integrations.MockMemDb, func() bool, []*models.Mock) {
	t.Helper()
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
	return mm, held, restaged, answered
}
