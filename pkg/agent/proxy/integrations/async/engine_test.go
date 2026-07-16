package async

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func asyncMock(lane string, seq, anchorPos int, respBody string) *models.Mock {
	return &models.Mock{
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				models.MetaAsync:     "true",
				models.MetaAsyncLane: lane,
				models.MetaAnchorPos: strconv.Itoa(anchorPos),
				models.MetaAsyncSeq:  strconv.Itoa(seq),
			},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: respBody},
		},
	}
}

func newTestEngine(p AsyncParser) *Engine {
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	return NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"fake": p})
}

func TestServesInSeqOrderWhenArmed(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{
		asyncMock("L", 2, 0, "b"),
		asyncMock("L", 1, 0, "a"), // out of order on purpose
	})
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	got1, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
	got2, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
	if got1.Spec.HTTPResp.Body != "a" || got2.Spec.HTTPResp.Body != "b" {
		t.Fatalf("want a then b, got %q then %q", got1.Spec.HTTPResp.Body, got2.Spec.HTTPResp.Body)
	}
}

func TestGatedByAnchorPosition(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 1, "after-T1")}) // anchorPos=1
	lane := models.AsyncLane{Name: "L", Type: "fake"}

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
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "boot")}) // anchorPos=0 (startup)
	rec, _, _ := e.Decide(context.Background(), models.AsyncLane{Name: "L", Type: "fake"}, &models.Mock{})
	if rec == nil || rec.Spec.HTTPResp.Body != "boot" {
		t.Fatalf("startup mock should be armed immediately, got %v", rec)
	}
}

func TestKeepAliveWhenDrained(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "a")})
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	_, _, _ = e.Decide(context.Background(), lane, &models.Mock{}) // consume the only mock
	rec, ka, _ := e.Decide(context.Background(), lane, &models.Mock{})
	if rec != nil || string(ka) != "KA" {
		t.Fatalf("drained lane should keep-alive, got rec=%v ka=%q", rec, ka)
	}
}

func TestShapeMismatchServesAndFlags(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: false, empty: []byte("KA")})
	e.Load([]*models.Mock{asyncMock("L", 1, 0, "a")})
	rec, _, _ := e.Decide(context.Background(), models.AsyncLane{Name: "L", Type: "fake"}, &models.Mock{})
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
	lane := models.AsyncLane{Name: "K", Type: "other"}
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

// TestLoadIsIdempotent proves a second Load call with the same mocks is a
// no-op once Decide has already advanced a stream's cursor: it must not
// re-sort/re-serve an already-consumed mock.
func TestLoadIsIdempotent(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})
	mocks := []*models.Mock{
		asyncMock("L", 1, 0, "a"),
		asyncMock("L", 2, 0, "b"),
	}
	lane := models.AsyncLane{Name: "L", Type: "fake"}

	e.Load(mocks)
	got1, _, _ := e.Decide(context.Background(), lane, &models.Mock{}) // consumes "a", cursor -> 1
	if got1 == nil || got1.Spec.HTTPResp.Body != "a" {
		t.Fatalf("first Decide: want %q, got %v", "a", got1)
	}

	e.Load(mocks) // re-Load same mocks; must be a no-op

	got2, _, _ := e.Decide(context.Background(), lane, &models.Mock{}) // should serve "b", not re-serve "a"
	if got2 == nil || got2.Spec.HTTPResp.Body != "b" {
		t.Fatalf("second Decide after re-Load: want %q, got %v", "b", got2)
	}
}

type holdStub struct{}

func (holdStub) MatchesLane(*models.Mock, models.AsyncLane) bool { return true }
func (holdStub) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) {
	return true, ""
}
func (holdStub) EmptyResponse(models.AsyncLane) ([]byte, error) { return []byte("204"), nil }

// A poll delivery (MetaAsyncPoll) is HELD until completed reaches its anchorPos,
// then served — while concurrent AdvanceWindow calls never block (the hold must
// not sit on the lock). A non-poll delivery returns a keep-alive immediately.
func TestDecideHoldsPollUntilAnchor(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "httpPoll"}
	e := NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"http": holdStub{}})
	pollMock := &models.Mock{Kind: models.HttpPoll, Spec: models.MockSpec{Metadata: map[string]string{
		models.MetaAsync: "true", models.MetaAsyncPoll: "true", models.MetaAsyncLane: "L",
		models.MetaAsyncSeq: "1", models.MetaAnchorPos: "2",
	}}}
	e.Load([]*models.Mock{pollMock})

	done := make(chan *models.Mock, 1)
	go func() {
		rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{})
		done <- rec
	}()
	select {
	case <-done:
		t.Fatal("Decide returned before anchor reached; poll should be held")
	case <-time.After(50 * time.Millisecond):
	}
	e.AdvanceWindow() // windowSeen=true, completed stays 0
	e.AdvanceWindow() // completed=1
	e.AdvanceWindow() // completed=2 -> arms
	select {
	case rec := <-done:
		if rec != pollMock {
			t.Fatalf("held poll served wrong mock: %+v", rec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held poll never released after anchor reached")
	}
}

func TestDecidePollReleasesOnCtxCancel(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "httpPoll"}
	e := NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"http": holdStub{}})
	e.Load([]*models.Mock{{Kind: models.HttpPoll, Spec: models.MockSpec{Metadata: map[string]string{
		models.MetaAsync: "true", models.MetaAsyncPoll: "true", models.MetaAsyncLane: "L",
		models.MetaAsyncSeq: "1", models.MetaAnchorPos: "5",
	}}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []byte, 1)
	go func() { _, ka, _ := e.Decide(ctx, lane, &models.Mock{}); done <- ka }()
	cancel()
	select {
	case ka := <-done:
		if ka == nil {
			t.Fatal("ctx-cancelled poll must release with a keep-alive")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx-cancel did not release the held poll")
	}
}

// Plug-and-play: the hold is keyed on MetaAsyncPoll, NOT the HTTP kind. A
// non-HTTP kind carrying MetaAsyncPoll is held the same way.
func TestDecideHoldsNonHTTPPollKind(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "mongoPoll"}
	e := NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"mongo": holdStub{}})
	e.Load([]*models.Mock{{Kind: models.Mongo, Spec: models.MockSpec{Metadata: map[string]string{
		models.MetaAsync: "true", models.MetaAsyncPoll: "true", models.MetaAsyncLane: "L",
		models.MetaAsyncSeq: "1", models.MetaAnchorPos: "1",
	}}}})
	done := make(chan *models.Mock, 1)
	go func() { rec, _, _ := e.Decide(context.Background(), lane, &models.Mock{}); done <- rec }()
	select {
	case <-done:
		t.Fatal("non-HTTP poll returned before anchor; must be held on MetaAsyncPoll")
	case <-time.After(50 * time.Millisecond):
	}
	e.AdvanceWindow() // windowSeen
	e.AdvanceWindow() // completed=1 -> arms
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("non-HTTP poll never released")
	}
}

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
