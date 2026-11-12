package http

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// A value the test gave keploy (a given) is learned as a template key is: the
// call that carries it whole uses the given's variable, with no bind, and is
// answered with this run's value. Its variable says why, where the test gave
// it and how to undo it.
func TestLearnMatchAValueTheTestGaveIsItsGiven(t *testing.T) {
	h, n, opts := learnOn(t)
	const rec, full = "app-1761609600123456789", "app-1761700000000000001"
	db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/apps", `{"name":"`+rec+`"}`, nil, `{"name":"`+rec+`","id":7}`)}}}
	db.table.Start(nil)
	if _, ok := db.table.Give(variables.Binding{Name: "given:app", Value: full, Recorded: rec, Mock: "test o::TestApps", Place: "apps_test.go:20"}); !ok {
		t.Fatal("the test's given did not bind")
	}
	served, miss, _ := h.learnMatch(context.Background(), live("POST", "/v1/apps", `{"name":"`+full+`"}`, nil), db, n, opts)
	if served == nil || miss != nil || served.Spec.HTTPResp.Body != `{"name":"`+full+`","id":7}` {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	why := learn.GivenWhy("given:app", "apps_test.go:20", rec, full)
	want := models.MockVariables{"given:app": {Recorded: rec, Use: []string{"body.name"}, Answer: []string{"body.name"}, Why: why}}
	if got := db.consumed[0].Spec.Learned(); !reflect.DeepEqual(got, want) {
		t.Fatalf("variables %+v, want %+v", got, want)
	}
	for _, part := range []string{"the test gave given:app (apps_test.go:20)", `"` + rec + `" when recorded`, `"` + full + `" in record's full round`, "delete this given:app block", "at apps_test.go:20 and re-record"} {
		if !strings.Contains(why, part) {
			t.Fatalf("why %q lacks %q", why, part)
		}
	}
}

// A call that carries a value made from one the test gave (the given inside
// a longer value) is refused naming the given and where the test gave it,
// never learned as noise: keploy follows a given only whole.
func TestLearnMatchRefusesAValueMadeFromAGiven(t *testing.T) {
	h, n, opts := learnOn(t)
	const rec, full = "1761609600123456789", "1761700000000000001"
	db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/apps", `{"name":"app-`+rec+`"}`, nil, `{"id":7}`)}}}
	db.table.Start(nil)
	if _, ok := db.table.Give(variables.Binding{Name: "given:nonce", Value: full, Recorded: rec, Mock: "test o::TestApps", Place: "apps_test.go:20", Given: true}); !ok {
		t.Fatal("the test's given did not bind")
	}
	tr := learn.NewTracker()
	tr.Begin()
	tr.GiveValue(learn.TestGiven("test o::TestApps", "given:nonce", rec, full))
	ctx := learn.WithTracker(context.Background(), tr)
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/apps", `{"name":"app-`+full+`"}`, nil), db, n, opts)
	if served != nil || miss == nil || !strings.Contains(miss.res.Why, learn.GivenDerivedWhy("body.name", "given:nonce", "apps_test.go:20")) {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	if len(db.consumed) != 0 {
		t.Fatalf("a refused call consumed %v", db.consumed)
	}
}

// A call that carried the given whole when recorded, but in record's full
// round another value than the test gave then (an answer the application
// kept from before), is refused with its own plain reason, naming the
// given: never the given's use, never noise, never "made from" it.
func TestLearnMatchRefusesAGivenTheAppDidNotPassOn(t *testing.T) {
	h, n, opts := learnOn(t)
	const rec, full = "1761609600123456789", "1761700000000000001"
	db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/apps", `{"name":"`+rec+`"}`, nil, `{"id":7}`)}}}
	db.table.Start(nil)
	if _, ok := db.table.Give(variables.Binding{Name: "given:nonce", Value: full, Recorded: rec, Mock: "test o::TestApps", Place: "apps_test.go:20", Given: true}); !ok {
		t.Fatal("the test's given did not bind")
	}
	tr := learn.NewTracker()
	tr.Begin()
	tr.GiveValue(learn.TestGiven("test o::TestApps", "given:nonce", rec, full))
	ctx := learn.WithTracker(context.Background(), tr)
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/apps", `{"name":"app-cached-0001"}`, nil), db, n, opts)
	if served != nil || miss == nil || !strings.HasPrefix(miss.res.Why, learn.GivenNotPassedOnWhy("body.name", "given:nonce", "apps_test.go:20")) {
		t.Fatalf("served %v miss %+v", served, miss)
	}
	if len(db.consumed) != 0 {
		t.Fatalf("a refused call consumed %v", db.consumed)
	}
}

// A given never makes the application's own clock read in the same call a
// value made from it: the test gives a nonce from its clock
// (UnixNano), the application sends it whole beside a createdAt it stamps
// from its own clock in the same second. The nonce is the given's variable;
// createdAt is learned as with no given (the clock read, MockSpec.Clocks), whether the
// test's clock runs on real time in record's full round or moves with the
// application's.
func TestLearnMatchAGivenLeavesTheApplicationsClockRead(t *testing.T) {
	recAt := learnAt.Add(-time.Hour)
	created := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	body := func(nonce string, at time.Time) string {
		return `{"name":"` + nonce + `","createdAt":"` + created(at) + `"}`
	}
	nonceRec := strconv.FormatInt(recAt.Add(-40*time.Millisecond).UnixNano(), 10)
	for regime, nonceFull := range map[string]string{
		"test clock on real time":         strconv.FormatInt(recAt.Add(26*time.Hour+3*time.Second).UnixNano(), 10),
		"test clock moved with the app's": strconv.FormatInt(recAt.Add(-40*time.Millisecond+learn.FullRoundLead+5*time.Millisecond).UnixNano(), 10),
	} {
		h, n, opts := learnOn(t)
		m := learnRecorded("mock-0", -time.Hour+300*time.Millisecond, "POST", "/v1/apps", body(nonceRec, recAt), nil, `{"id":7}`)
		db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{m}}}
		db.table.Start(nil)
		if _, ok := db.table.Give(variables.Binding{Name: "given:nonce", Value: nonceFull, Recorded: nonceRec, Mock: "test o::TestApps", Place: "apps_test.go:20", Given: true}); !ok {
			t.Fatal("the test's given did not bind")
		}
		tr := learn.NewTracker()
		tr.Begin()
		tr.GiveValue(learn.TestGiven("test o::TestApps", "given:nonce", nonceRec, nonceFull))
		ctx := learn.WithTracker(context.Background(), tr)
		served, miss, err := h.learnMatch(ctx, live("POST", "/v1/apps", body(nonceFull, recAt.Add(learn.FullRoundLead)), nil), db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("%s: served %v miss %+v err %v", regime, served, miss, err)
		}
		got := db.consumed[0].Spec
		if v, ok := got.Learned()["given:nonce"]; !ok || !reflect.DeepEqual(v.Use, []string{"body.name"}) {
			t.Fatalf("%s: variables %+v, want given:nonce used at body.name", regime, got.Learned())
		}
		if _, ok := got.Clocks["body.createdAt"]; !ok || len(got.ReqBodyNoise) != 0 || len(got.Learned()) != 1 {
			t.Fatalf("%s: clocks %v noise %v variables %+v, want body.createdAt learned as the clock read", regime, got.Clocks, got.ReqBodyNoise, got.Learned())
		}
	}
}
