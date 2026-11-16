package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A once chain's next recording that another connection takes just as the
// chain's call tries to take it answers nothing here: the call goes on to
// the matchers, and a line says another call claimed the recording first
// (chainNextClaimed), naming the series and the recording, never a value.
func TestAOnceChainsNextAnotherCallTookFirstIsSaid(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.InfoLevel)
	h := &HTTP{Logger: zap.New(core)}
	next := at(varMock("mock-2", "GET", "http://store/v1/key", "", 200, `{"version":1}`, nil), 2)
	db := &tookFirstDb{varDb: &varDb{perTest: []*models.Mock{next}}, took: "mock-2"}
	c := chainCall{next: "mock-2", series: "touch/-/1", call: "GET /v1/key"}
	got, err := h.chainNext(context.Background(), liveReq("GET", "http://store/v1/key", ""), db, c, h.newStrictNoise(nil, nil, nil), func() bool { return false })
	if got != nil || err != nil {
		t.Fatalf("the chain answered %v (%v); want nothing, the recording taken by another call", got, err)
	}
	if !db.other["mock-2"] {
		t.Fatal("precondition: the chain's call never tried to take the recording")
	}
	lines := logs.FilterMessageSnippet("a once series' chain did not answer a call on its connection").All()
	if len(lines) != 1 {
		t.Fatalf("%d lines say the chain did not answer; want 1", len(lines))
	}
	f := lines[0].ContextMap()
	if f["check"] != chainClaimed || f["next"] != "mock-2" || f["series"] != "touch/-/1" {
		t.Fatalf("the line says %v; want check %s, next mock-2, series touch/-/1", f, chainClaimed)
	}
	if answered := logs.FilterMessageSnippet("a once series' chain answered a call").Len(); answered != 0 {
		t.Fatalf("%d lines say the chain answered", answered)
	}
}

// The learn step reads a call again when the take of the recording its
// pass chose was refused (a claim lost to a store change): a once chain
// that left the call to the matchers says why on the first pass only, one
// line a call, however many passes read it.
func TestInTheFullRoundAChainsDeclineIsSaidOnceAcrossTheLearnStepsPasses(t *testing.T) {
	h, n, opts := learnOn(t)
	core, logs := observer.New(zap.InfoLevel)
	h.Logger = zap.New(core)
	tr := learn.NewTracker()
	tr.Begin()
	own := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"id":"o-1"}`, nil, `{"ok":true}`)
	db := &flakyClaimDb{learnDb: &learnDb{perTest: []*models.Mock{own}}}
	lc := &learnChain{
		follow: integrations.OnceFollow{Next: "mock-9", Series: "touch/-/1"},
		c:      chainCall{series: "touch/-/1", call: "POST /v1/orders"},
		strict: h.newStrictNoise(nil, nil, nil),
	}
	// A deadline, so a learn step that never stops reading the call fails
	// the row rather than hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var call integrations.CallRound
	served, miss, err := h.learnMatchIn(learn.WithTracker(ctx, tr), live("POST", "/v1/orders", `{"id":"o-1"}`, nil), db, n, opts, integrations.CallRound{}, &call, lc)
	if err != nil || served == nil || !db.lost {
		t.Fatalf("served %v miss %+v err %v lost %v; want answered on the second look, after a lost claim", served, miss, err, db.lost)
	}
	lines := logs.FilterMessageSnippet("a once series' chain did not answer a call on its connection").All()
	if len(lines) != 1 {
		t.Fatalf("%d lines say the chain did not answer; want 1, said on the first pass only", len(lines))
	}
	if f := lines[0].ContextMap(); f["check"] != chainNotHeld || f["next"] != "mock-9" || f["series"] != "touch/-/1" {
		t.Fatalf("the line says %v; want check %s, next mock-9, series touch/-/1", f, chainNotHeld)
	}
	if lc.picked != "" {
		t.Fatalf("the chain picked %q; want none, its next not held", lc.picked)
	}
}

// In the full round, a once chain's next recording that is the open test's
// own and answered already (spent: another connection's call of the test
// took it) answers nothing more: a line says the chain's next is not held
// (chainNextNotHeld), and the call is answered as any call is, by the
// test's next own recording of it.
func TestInTheFullRoundAChainsSpentNextIsSaid(t *testing.T) {
	h, n, opts := learnOn(t)
	core, logs := observer.New(zap.InfoLevel)
	h.Logger = zap.New(core)
	tr := learn.NewTracker()
	tr.Begin()
	// A deadline, so a learn step that never stops reading the call (one
	// that takes the spent recording again and again) fails the row rather
	// than hanging it.
	dl, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx := learn.WithTracker(dl, tr)
	first := learnRecorded("mock-1", 0, "GET", "/v1/key", "", nil, `{"version":1}`)
	second := learnRecorded("mock-2", time.Second, "GET", "/v1/key", "", nil, `{"version":2}`)
	db := &learnDb{perTest: []*models.Mock{first, second}, observe: tr.ServedMock}
	if served, miss, err := h.learnMatch(ctx, live("GET", "/v1/key", "", nil), db, n, opts); err != nil || served == nil || served.Name != first.Name {
		t.Fatalf("precondition: the test's first read was answered %v (miss %+v, err %v); want its own %s", served, miss, err, first.Name)
	}
	lc := &learnChain{
		follow: integrations.OnceFollow{Next: first.Name, Series: "touch/-/1"},
		c:      chainCall{series: "touch/-/1", call: "GET /v1/key"},
		strict: h.newStrictNoise(nil, nil, nil),
	}
	var call integrations.CallRound
	served, miss, err := h.learnMatchIn(ctx, live("GET", "/v1/key", "", nil), db, n, opts, integrations.CallRound{}, &call, lc)
	if err != nil || served == nil || served.Name != second.Name {
		t.Fatalf("the chain's call was answered %v (miss %+v, err %v); want the test's next own %s", served, miss, err, second.Name)
	}
	if lc.picked != "" {
		t.Fatalf("the chain picked %q; want none, its next spent", lc.picked)
	}
	lines := logs.FilterMessageSnippet("a once series' chain did not answer a call on its connection").All()
	if len(lines) != 1 {
		t.Fatalf("%d lines say the chain did not answer; want 1", len(lines))
	}
	if f := lines[0].ContextMap(); f["check"] != chainNotHeld || f["next"] != first.Name || f["series"] != "touch/-/1" {
		t.Fatalf("the line says %v; want check %s, next %s, series touch/-/1", f, chainNotHeld, first.Name)
	}
}
