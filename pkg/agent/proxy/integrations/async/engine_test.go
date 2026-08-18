package async

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func asyncMock(lane string, seq, anchorPos int, respBody string) *models.Mock {
	return &models.Mock{
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Async:    &models.AsyncMeta{Lane: lane, Seq: seq, AnchorPos: anchorPos},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: respBody},
		},
	}
}

func newTestEngine(p AsyncParser, laneName ...string) *Engine {
	name := "L"
	if len(laneName) > 0 {
		name = laneName[0]
	}
	lane := models.AsyncLane{Name: name, Type: "fake"}
	return NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"fake": p})
}

func TestServesCurrentEpochByCompleted(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	// epoch V0 at pos 0 (boot), epoch V1 at pos 2 (received after test-2).
	e.Load([]*models.Mock{
		asyncMock("L", 1, 0, "V0"),
		asyncMock("L", 2, 2, "V1"),
	})
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10} // keep the suite fast; default is 1s

	// completed=0 (boot) -> V0
	if rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{}); rec == nil || rec.Spec.HTTPResp.Body != "V0" {
		t.Fatalf("boot: want V0, got %v", rec)
	}
	e.AdvanceWindow() // windowSeen, completed=0
	e.AdvanceWindow() // completed=1 (after test-1)
	if rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{}); rec == nil || rec.Spec.HTTPResp.Body != "V0" {
		t.Fatalf("after test-1: want V0 (change not received), got %v", rec)
	}
	e.AdvanceWindow() // completed=2 (after test-2)
	if rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{}); rec == nil || rec.Spec.HTTPResp.Body != "V1" {
		t.Fatalf("after test-2: want V1, got %v", rec)
	}
}

func TestEpochIsReselectableNotConsumed(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "V0")})
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10} // keep the suite fast; default is 1s
	for i := 0; i < 3; i++ {
		if rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{}); rec == nil || rec.Spec.HTTPResp.Body != "V0" {
			t.Fatalf("poll %d: epoch must be re-selectable, got %v", i, rec)
		}
	}
}

func TestKeepAliveWhenNoEpochEffective(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 5, "late")})               // first epoch at pos 5
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}  // keep the suite fast; default is 1s
	rec, ka, _ := e.Decide(context.Background(), lane, &models.Mock{}) // completed=0 < 5
	if rec != nil || string(ka) != "KA" {
		t.Fatalf("no epoch effective yet: want keep-alive, got rec=%v ka=%q", rec, ka)
	}
}

func TestGatedByAnchorPosition(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 1, "after-T1")})          // anchorPos=1
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10} // keep the suite fast; default is 1s

	rec, ka, _ := e.Decide(context.Background(), lane, &models.Mock{}) // completed=0 -> not armed
	if rec != nil || string(ka) != "KA" {
		t.Fatalf("before anchor: want keep-alive, got rec=%v ka=%q", rec, ka)
	}
	e.OnTestComplete() // completed=1
	rec, ka, _ = e.Decide(context.Background(), lane, &models.Mock{})
	if rec == nil || rec.Spec.HTTPResp.Body != "after-T1" {
		t.Fatalf("after anchor: want the mock, got rec=%v ka=%q", rec, ka)
	}
}

func TestStartupArmedImmediately(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "boot")})                                                                   // anchorPos=0 (startup)
	rec, _, _ := e.Decide(context.Background(), models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}, &models.Mock{}) // small throttle: keep the suite fast
	if rec == nil || rec.Spec.HTTPResp.Body != "boot" {
		t.Fatalf("startup mock should be armed immediately, got %v", rec)
	}
}

func TestShapeMismatchServesAndFlags(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: false, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "a")})
	rec, _, _ := e.Decide(context.Background(), models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}, &models.Mock{}) // small throttle: keep the suite fast
	if rec == nil {
		t.Fatal("mismatch must still serve the recorded mock")
	}
	if snap := e.Report(); snap.Flag != 1 || snap.Pass != 0 {
		t.Fatalf("want 1 flag 0 pass, got %+v", snap)
	}
}

// Pluggability proof: a SECOND, different fake parser drives the same engine
// with zero engine changes.
type otherFake struct{ fakeParser }

func TestPluggableSecondTransport(t *testing.T) {
	lane := models.AsyncLane{Name: "K", Type: "other", ThrottleMs: 10} // keep the suite fast; default is 1s
	e := NewEngine(zap.NewNop(), []models.AsyncLane{lane},
		map[string]AsyncParser{"other": &otherFake{fakeParser{matches: true, shapeOK: true, empty: []byte("EK")}}})
	e.Load([]*models.Mock{asyncMock("K", 1, 0, "kafka-ish")})
	rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
	if rec == nil || rec.Spec.HTTPResp.Body != "kafka-ish" {
		t.Fatalf("engine must serve any transport unchanged, got %v", rec)
	}
}

// TestLaneForFirstDeclaredWins proves LaneFor is deterministic when more than
// one lane's parser could match the same live request: the FIRST lane passed
// to NewEngine must always win, regardless of Go's randomized map iteration
// order over e.lanes.
func TestLaneForFirstDeclaredWins(t *testing.T) {
	first := models.AsyncLane{Name: "first", Type: "fake"}
	second := models.AsyncLane{Name: "second", Type: "fake2"}
	e := NewEngine(zap.NewNop(), []models.AsyncLane{first, second}, map[string]AsyncParser{
		"fake":  &fakeParser{matches: true, shapeOK: true, empty: []byte("KA1")},
		"fake2": &fakeParser{matches: true, shapeOK: true, empty: []byte("KA2")},
	})
	for i := 0; i < 20; i++ {
		got, ok := e.LaneFor(&models.Mock{})
		if !ok || got.Name != "first" {
			t.Fatalf("iteration %d: want first-declared lane %q, got %+v (ok=%v)", i, first.Name, got, ok)
		}
	}
}

type holdStub struct{}

func (holdStub) MatchesLane(*models.Mock, models.AsyncLane) bool { return true }
func (holdStub) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) {
	return true, ""
}
func (holdStub) EmptyResponse(models.AsyncLane) ([]byte, error)         { return []byte("204"), nil }
func (holdStub) ResponseValueKey(*models.Mock, models.AsyncLane) string { return "" }

// TestLaneForResolvesByBaseType proves LaneFor routes a "httpPoll" lane's live
// request to the parser registered under its base type "http" — without a
// parser separately registered under the raw "httpPoll" key. Without this,
// LaneFor would never match a poll lane and the hold in Decide could never
// engage.
func TestLaneForResolvesByBaseType(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "httpPoll"}
	e := NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"http": holdStub{}})
	got, ok := e.LaneFor(&models.Mock{})
	if !ok || got.Name != "L" {
		t.Fatalf("want LaneFor to resolve httpPoll lane via base type %q, got ok=%v lane=%+v", "http", ok, got)
	}
}

// TestNonPollServesReselectableCurrentEpoch pins the intended NON-poll lane
// behavior after the value-epoch change (see Decide's scope note): a non-poll
// lane (IsPoll() == false) serves the current epoch — last with AnchorPos <=
// completed — RE-SELECTABLY, so repeated requests re-serve it rather than
// consuming each entry once and keep-aliving on drain (the old cursor model).
// This guards Decide's all-lanes selection scope so a later change can't
// silently revert non-poll lanes to one-shot ordered delivery.
func TestNonPollServesReselectableCurrentEpoch(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	lane := models.AsyncLane{Name: "L", Type: "fake"} // non-poll: IsPoll() == false
	e.Load([]*models.Mock{
		asyncMock("L", 1, 0, "A"), // epoch effective from pos 0
		asyncMock("L", 2, 1, "B"), // epoch effective from pos 1
	})
	// completed=0: current epoch is A, served on EVERY request (re-selectable,
	// never consumed, never keep-alived on "drain").
	for i := 0; i < 3; i++ {
		rec, ka, _ := e.Decide(context.Background(), lane, &models.Mock{})
		if rec == nil || rec.Spec.HTTPResp.Body != "A" {
			t.Fatalf("req %d @completed=0: want re-selectable A, got rec=%v ka=%q", i, rec, ka)
		}
	}
	// advance to completed=1: current epoch becomes B, also re-selectable.
	e.AdvanceWindow() // windowSeen; completed stays 0
	e.AdvanceWindow() // completed=1
	for i := 0; i < 3; i++ {
		rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
		if rec == nil || rec.Spec.HTTPResp.Body != "B" {
			t.Fatalf("req %d @completed=1: want re-selectable B, got %v", i, rec)
		}
	}
}
