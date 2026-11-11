package http

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A CI trust policy's key record (api-server's e2e, the workload identity
// tests): the test names its service account "e2e-ci-oidc-<UnixNano>" and
// asks for a trust policy; the app mints the record's id, stamps createdAt
// with the clock's second and expiresAt with that second plus the policy's
// ttlDays (365 by default), and PUTs the record to the secret store in the
// same second as the name. The learn step finds expiresAt's second stood
// 8760h after the name's moment in both runs, and refuses it: a value built
// by an offset on what the test gave the application moves as a read of the
// clock does. keploy.yml's clock rules declare both stamps reads of the
// clock (expiresAt one plus 365 days); the rule settles which source the
// moment has, so the PUT is answered and both stamps are learned as clocks:
// under the T2 time default checked by their form, with keploy.yml's times
// strict by the rules' ranges. Without the rules it is refused as before,
// and so is a value the rule's range does not hold where times are strict.
func TestLearnMatchHoldsADeclaredExpiryAnEarlierNameMovesBy(t *testing.T) {
	const ttl = 365 * 24 * time.Hour
	path := func(id string) string { return "/v1/secret/data/keploy/personal-access-tokens/keploy.io/" + id }
	body := func(id string, at time.Time, expiry time.Duration) string {
		return `{"data":{"_id":"` + id + `","cid":"keploy.io","createdAt":` + strconv.FormatInt(at.Unix(), 10) +
			`,"expiresAt":` + strconv.FormatInt(at.Add(expiry).Unix(), 10) + `,"keyType":"pat","status":"active","updatedAt":"` +
			at.UTC().Format(time.RFC3339) + `"}}`
	}
	name := func(read time.Time) string {
		return `{"query":"mutation($input: CreateWorkloadIdentityInput!) { createServiceAccountWorkloadIdentity(input: $input) { id } }","variables":{"input":{"name":"e2e-ci-oidc-` +
			strconv.FormatInt(read.UnixNano(), 10) + `"}}}`
	}
	identity := func(r time.Time) (time.Time, bool) { return r, true }
	const recID, liveID = "5b39677d-8406-4d49-bfd1-853a0bf33fb8", "9e0c2f41-77aa-4c1b-9d2e-3f6b8a5c1d07"
	// Recorded: the test began at recBegin, read the clock for the name
	// 100 ms on, and the app stamped the record 120 ms on and PUT it at
	// 130 ms. Round F: the same steps, ten minutes on.
	recBegin := learnAt.Add(time.Second)
	began := learnAt.Add(10 * time.Minute)
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	yearOn := &models.ClockOffset{Min: ttl, Max: ttl + time.Second, Res: time.Second}
	rules := []models.DeclaredSource{
		{Rule: "workload-key-created-at", Kind: models.DeclaredSourceClock, Field: "body.data.createdAt", Clock: plain},
		{Rule: "workload-key-expires-at", Kind: models.DeclaredSourceClock, Field: "body.data.expiresAt", Clock: yearOn},
		{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: plain},
	}
	// unread, where not "", is why keploy could not read the run's request
	// (Tracker.Incoming), and the run PUTs the recorded id.
	run := func(sources []models.DeclaredSource, timeForm bool, expiry time.Duration, unread string) (*models.Mock, *learnMiss, *learnDb) {
		h, n, opts := learnOn(t)
		n.sources = func(mock string) []models.DeclaredSource {
			if mock == "mock-0" {
				return sources
			}
			return nil
		}
		tr := learn.NewTracker()
		tr.SetClientTests(true)
		tr.SetAppClock(identity)
		tr.RecordedBegin(recBegin)
		tr.BeginAt(began)
		jsonType := map[string]string{"Content-Type": "application/json"}
		recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Timestamp: recBegin.Add(100 * time.Millisecond),
			Header: jsonType, Body: name(recBegin.Add(100 * time.Millisecond))}
		tr.SetCases([]learn.ClientCase{{Name: "post-query-83", Req: recReq}})
		liveReq := *recReq
		liveReq.Body, liveReq.Timestamp = name(began.Add(100*time.Millisecond)), began.Add(100*time.Millisecond)
		if !tr.Incoming(&liveReq, unread, began.Add(100*time.Millisecond)) {
			t.Fatal("the run's request paired with none of the test's recorded requests")
		}
		put := learnRecorded("mock-0", 1130*time.Millisecond, "PUT", path(recID), body(recID, recBegin.Add(120*time.Millisecond), expiry), nil, `{"data":{"version":1}}`)
		db := &learnDb{perTest: []*models.Mock{put}}
		id := liveID
		if unread != "" {
			// The record's id as recorded, so that only the stamps changed:
			// an id that changed is refused for the request alone, with no
			// rule at it.
			id = recID
		}
		in := live("PUT", path(id), body(id, began.Add(120*time.Millisecond), expiry), nil)
		in.arrived = began.Add(130 * time.Millisecond)
		var store integrations.MockMemDb = db
		if timeForm {
			store = timeFormDb{db}
		}
		served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, store, n, opts)
		return served, miss, db
	}

	// Without the rules the PUT is refused (createdAt, a pick from the
	// name by the clock as far as keploy can tell); with the stamps' rules
	// but expiresAt's, on expiresAt, as the run refused it.
	stamps := []models.DeclaredSource{rules[0], rules[2]}
	for _, timeForm := range []bool{true, false} {
		if served, miss, _ := run(nil, timeForm, ttl, ""); served != nil || miss == nil {
			t.Fatalf("no rules, time default %v: served %v, miss %+v; want the PUT refused", timeForm, served != nil, miss)
		}
		served, miss, _ := run(stamps, timeForm, ttl, "")
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, "body.data.expiresAt changed, and its moment, to the second, stands 8760h0m0s after the moment test case post-query-83 at body.variables.input.name holds") {
			t.Fatalf("no expiry rule, time default %v: served %v, miss %+v; want the PUT refused on expiresAt built by an offset on the name, as the run refused it", timeForm, served != nil, miss)
		}
	}
	for _, timeForm := range []bool{true, false} {
		served, miss, db := run(rules, timeForm, ttl, "")
		if served == nil || miss != nil || len(db.consumed) != 1 {
			t.Fatalf("both stamps declared clocks, time default %v: served %v, miss %+v; want the PUT answered", timeForm, served != nil, miss)
		}
		clocks := db.consumed[0].Spec.Clocks
		for f, want := range map[string]models.ClockOffset{"body.data.createdAt": {Res: time.Second}, "body.data.expiresAt": {Min: ttl, Max: ttl + time.Second, Res: time.Second}} {
			o, ok := clocks[f]
			if !ok || (o.Form != "") != timeForm || o.Res != want.Res || o.Layout != "epoch-s" || !timeForm && (o.Min != want.Min || o.Max != want.Max) {
				t.Errorf("time default %v: %s learned as %+v (clocks %v); want the rule's range %+v in epoch seconds, checked by its form alone under the default", timeForm, f, o, clocks, want)
			}
		}
	}
	// A policy of one day (ttlDays 1): the 365-day rule's range does not
	// hold its expiresAt. Under the time default the stamp is a time all
	// the same, checked by its form; with times strict the range decides,
	// and the PUT is refused.
	day := 24 * time.Hour
	if served, miss, _ := run(rules, true, day, ""); served == nil || miss != nil {
		t.Fatalf("a one-day expiry, time default: served %v, miss %+v; want the PUT answered", served != nil, miss)
	}
	if served, miss, _ := run(rules, false, day, ""); served != nil || miss == nil {
		t.Fatalf("a one-day expiry, times strict: served %v, miss %+v; want the PUT refused by the rule's range", served != nil, miss)
	}
	// A run whose request keploy could not read: each stamp may be a part
	// of what the test gave the application, or a pick from it by the
	// clock, which keploy cannot tell, and a clock rule says nothing of
	// what keploy could not read. The rules leave the PUT refused, naming
	// the request.
	const unread = "its body was cut short"
	for _, timeForm := range []bool{true, false} {
		served, miss, _ := run(rules, timeForm, ttl, unread)
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, "could not read the test case's own request ("+unread+")") {
			t.Fatalf("request unread, every stamp declared a clock, time default %v: served %v, miss %+v; want the PUT refused for the request keploy could not read", timeForm, served != nil, miss)
		}
	}
}
