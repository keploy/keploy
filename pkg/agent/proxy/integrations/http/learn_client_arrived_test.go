package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// The HTTP adapter's learn step (learnMatch) reads a client test's request
// that reached the application before the call (learn.Tracker.Incoming) as
// a place of the call, from the call's own arrival: a name the request sent,
// which the call carries whole, binds at the request's test case. A request
// that reached the application after the call, or a call whose arrival is
// not known, gives the call nothing there.
func TestLearnMatchBindsAValueAtTheClientTestsRequestThatSentIt(t *testing.T) {
	const rec, full = "[E2E] app-crud-1794207702156240454", "[E2E] app-crud-1794207771159240454"
	jsonType := map[string]string{"Content-Type": "application/json"}
	sent := time.Now()
	run := func(header map[string]string, arrived time.Time) (*models.Mock, *learnMiss, *learnDb) {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.SetClientTests(true)
		tr.Begin()
		recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/client/v1/apps", Timestamp: learnAt.Add(-time.Second),
			Header: header, Body: `{"name":"` + rec + `"}`}
		tr.SetCases([]learn.ClientCase{{Name: "post-apps-1", Req: recReq}})
		liveReq := *recReq
		liveReq.Body, liveReq.Timestamp = `{"name":"`+full+`"}`, sent
		if !tr.Incoming(&liveReq, "", sent) {
			t.Fatal("the run's request paired with none of the test's recorded requests")
		}
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/search", `{"filter":{"name":"`+rec+`"}}`, nil, `{"hits":1}`)}}
		in := live("POST", "/v1/search", `{"filter":{"name":"`+full+`"}}`, nil)
		in.arrived = arrived
		served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
		return served, miss, db
	}

	served, miss, db := run(jsonType, sent.Add(2*time.Millisecond))
	if served == nil || miss != nil || len(db.consumed) != 1 {
		t.Fatalf("the call after the client's request: served %v miss %+v; want it served", served, miss)
	}
	binds := db.consumed[0].Spec.LearnedBinds()["test case post-apps-1"]
	vars := db.consumed[0].Spec.Learned()
	if len(binds) != 1 || len(vars) != 1 {
		t.Fatalf("binds %+v, variables %+v; want one variable bound at test case post-apps-1", db.consumed[0].Spec.LearnedBinds(), vars)
	}
	for name, b := range binds {
		v := vars[name]
		if b.Recorded != rec || b.Bind != "body.name" || v.Recorded != rec || v.Bind != "" || len(v.Use) != 1 || v.Use[0] != "body.filter.name" {
			t.Fatalf("bind %s = %+v, use %+v; want %q bound at the test case's body.name, used at body.filter.name", name, b, v, rec)
		}
	}

	// No place of the call: the value is read as on main, an instant keploy
	// saw no read of the application's clock for, and refused.
	for what, arrived := range map[string]time.Time{
		"a call that reached keploy before the client's request": sent.Add(-time.Millisecond),
		"a call whose arrival is not known":                      {},
	} {
		served, miss, db := run(jsonType, arrived)
		if served != nil || miss == nil || len(db.consumed) != 0 || strings.Contains(miss.res.Why, "test case post-apps-1") || !strings.Contains(miss.res.Why, "saw no read of the app's clock") {
			t.Errorf("%s: served %v, miss %+v; want it refused as an instant of no clock read, the client's request no place of it", what, served != nil, miss)
		}
	}
	// A request that declares no media type: its body's leaves are read as
	// text by their bytes, which no place names, so nothing binds there.
	served, miss, db = run(nil, sent.Add(2*time.Millisecond))
	if served != nil || miss == nil || len(db.consumed) != 0 || !strings.Contains(miss.res.Why, "test case post-apps-1 sent earlier in the test at body.name, inside a longer value or at a place keploy cannot bind it") {
		t.Errorf("a client request of no media type: served %v, miss %+v; want it refused, no place to bind", served != nil, miss)
	}
}

// A value an earlier call of the test sent in a form field, where no
// variable binds (learn.VarPlace reads a body's leaves only in JSON), is
// refused when a later call carries it whole, never bound there: only a
// client test's request has its leaves named by their paths alone.
func TestLearnMatchRefusesAValueAnEarlierCallSentInAFormField(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/orders", "order=o-1", form, `{}`),
		learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1", "", nil, `{"id":"o-1"}`),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", "order=o-9", form), db, n, opts); served == nil || miss != nil {
		t.Fatalf("the form's call is paired: %+v", miss)
	}
	served, miss, _ := h.learnMatch(ctx, live("GET", "/v1/orders/o-9", "", nil), db, n, opts)
	if served != nil || miss == nil || !strings.Contains(miss.res.Why, "carries a value mock-0 sent earlier in the test at body.order, inside a longer value or at a place keploy cannot bind it") {
		t.Fatalf("served %v miss %+v; want it refused, the form field no place to bind", served != nil, miss)
	}
}
