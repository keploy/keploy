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
// under the time default checked by their form, with keploy.yml's times
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
		{Rule: "pat-created-at", Kind: models.DeclaredSourceClock, Field: "body.data.createdAt", Clock: plain},
		{Rule: "pat-expires-at", Kind: models.DeclaredSourceClock, Field: "body.data.expiresAt", Clock: yearOn},
		{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: plain},
	}
	// shift moves round F's name and stamps off the run's clock: a test
	// that sends an instant it did not read now, and an app that builds
	// both stamps on it (0: the run's shape).
	var shift time.Duration
	// recShift moves the recording's name and stamps off its clock, and
	// nameShift round F's name alone: F's stamps stay reads of its clock.
	var recShift, nameShift time.Duration
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
			Header: jsonType, Body: name(recBegin.Add(100*time.Millisecond + recShift))}
		tr.SetCases([]learn.ClientCase{{Name: "post-query-83", Req: recReq}})
		liveReq := *recReq
		liveReq.Body, liveReq.Timestamp = name(began.Add(100*time.Millisecond+shift+nameShift)), began.Add(100*time.Millisecond)
		if !tr.Incoming(&liveReq, unread, began.Add(100*time.Millisecond)) {
			t.Fatal("the run's request paired with none of the test's recorded requests")
		}
		put := learnRecorded("mock-0", 1130*time.Millisecond, "PUT", path(recID), body(recID, recBegin.Add(120*time.Millisecond+recShift), expiry), nil, `{"data":{"version":1}}`)
		db := &learnDb{perTest: []*models.Mock{put}}
		id := liveID
		if unread != "" {
			// The record's id as recorded, so that only the stamps changed:
			// an id that changed is refused for the request alone, with no
			// rule at it.
			id = recID
		}
		in := live("PUT", path(id), body(id, began.Add(120*time.Millisecond+shift), expiry), nil)
		in.arrived = began.Add(130 * time.Millisecond)
		var store integrations.MockMemDb = db
		if timeForm {
			store = timeFormDb{db}
		}
		served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, store, n, opts)
		return served, miss, db
	}

	// Without the rules the PUT is refused (createdAt, a pick from the
	// name by the clock as far as keploy can tell). With the stamps' rules
	// but expiresAt's, the run refused it on expiresAt, built by the offset
	// on the name; its call's window holds it as a read of the clock plus
	// 365 days in both runs, so it is a time all the same, learned as that
	// read since its test began, which a replay holds to its test's span.
	// Built on a name an hour off the run's clock, it is no such read in
	// round F, and stays refused on the offset.
	stamps := []models.DeclaredSource{rules[0], rules[2]}
	const offsetLine = "body.data.expiresAt changed, and its moment, to the second, stands 8760h0m0s after the moment test case post-query-83 at body.variables.input.name holds"
	for _, timeForm := range []bool{true, false} {
		if served, miss, _ := run(nil, timeForm, ttl, ""); served != nil || miss == nil {
			t.Fatalf("no rules, time default %v: served %v, miss %+v; want the PUT refused", timeForm, served != nil, miss)
		}
		shift = -time.Hour
		served, miss, _ := run(stamps, timeForm, ttl, "")
		shift = 0
		if served != nil || miss == nil || timeForm && !strings.Contains(miss.res.Why, offsetLine) {
			t.Fatalf("no expiry rule, stamps an hour off round F's clock, time default %v: served %v, miss %+v; want the PUT refused (on the offset under the time default)", timeForm, served != nil, miss)
		}
		served, miss, db := run(stamps, timeForm, ttl, "")
		if !timeForm {
			// mock.times: strict checks a time by its range and
			// provenance: the read since the test began keeps the range
			// alone, so the offset rule's refusal stands, as before the
			// time default was (learn.OffsetReadUnder).
			if served != nil || miss == nil || !strings.Contains(miss.res.Why, offsetLine) {
				t.Fatalf("no expiry rule, mock.times: strict: served %v, miss %+v; want the PUT refused on expiresAt built by an offset on the name, as before the time default", served != nil, miss)
			}
			continue
		}
		if served == nil || miss != nil || len(db.consumed) != 1 {
			t.Fatalf("no expiry rule, time default %v: served %v, miss %+v; want the PUT answered, expiresAt a read of the clock plus 365 days", timeForm, served != nil, miss)
		}
		o, ok := db.consumed[0].Spec.Clocks["body.data.expiresAt"]
		if !ok || o.Since != models.SinceTestBegin || o.Min > ttl || o.Max < ttl || o.Min <= ttl-time.Second || o.Layout != "epoch-s" || o.Form != "" {
			t.Errorf("no expiry rule, time default %v: expiresAt learned as %+v; want the clock plus 365 days in epoch seconds, checked from its test's begin", timeForm, o)
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

	// A recording whose app built its stamps on a name an hour off its
	// clock, and a round F that stamps them with its clock while its name
	// is an hour off: createdAt's recorded moment stands in the recorded
	// name, and F's name gives F's by no rule. It may be a pick from the
	// request by the clock, which a clock rule does not settle: the rule
	// says how a read is checked, not that the value is no part of the
	// request. The PUT stays refused, naming the name.
	const holdsLine = "body.data.createdAt changed, and the moment it was recorded with stands in test case post-query-83 at body.variables.input.name"
	for _, timeForm := range []bool{true, false} {
		recShift, nameShift = -time.Hour, -time.Hour
		served, miss, _ := run(rules, timeForm, ttl, "")
		recShift, nameShift = 0, 0
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, holdsLine) {
			t.Fatalf("createdAt's recorded moment in the recorded name alone, every stamp declared a clock, time default %v: served %v, miss %+v; want the PUT refused on createdAt, naming the name", timeForm, served != nil, miss)
		}
	}
}

// A keploy test's call sends a time that moved three days, no lead and no
// read of the clock, which another test's answer F served holds: the value
// may be a pick from that answer by the clock, which keploy cannot tell from
// a read of the clock. A clock rule at the field settles what learning could not tell
// of a time's source, never a refusal that names a place the value may have
// come from: the call stays refused, naming the answer, under the time
// default and with times strict. Without that answer the rule decides: under
// the time default the moved time is checked by its form, and the call is
// answered.
func TestLearnMatchKeepsAnotherTestsAnswerARuledTimeMayCopy(t *testing.T) {
	rule := models.DeclaredSource{Rule: "beat-at", Kind: models.DeclaredSourceClock, Field: "body.at",
		Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}}
	const rec, moved = "2026-10-14T10:00:00Z", "2026-10-17T10:00:00Z"
	run := func(withAnswer, timeForm bool) (*models.Mock, *learnMiss) {
		h, n, opts := learnOn(t)
		n.sources = func(mock string) []models.DeclaredSource {
			if mock == "mock-0" {
				return []models.DeclaredSource{rule}
			}
			return nil
		}
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 10*time.Millisecond, "POST", "/v1/beat", `{"at":"`+rec+`"}`, nil, `{}`)}}
		tr := learn.NewTracker()
		tr.Begin()
		if withAnswer {
			seen := learnRecorded("mock-1", 5*time.Millisecond, "GET", "/v1/seen", "", nil, `{"last":"`+moved+`"}`)
			tr.ServedMock(seen)
			tr.ServedMockAsRecorded(seen)
		}
		tr.Begin()
		var store integrations.MockMemDb = db
		if timeForm {
			store = timeFormDb{db}
		}
		served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/beat", `{"at":"`+moved+`"}`, nil), store, n, opts)
		if err != nil {
			t.Fatal(err)
		}
		return served, miss
	}
	if served, miss := run(false, true); served == nil || miss != nil {
		t.Fatalf("no answer holds the time, time default: served %v, miss %+v; want the call answered, the time checked by its form", served != nil, miss)
	}
	for _, timeForm := range []bool{true, false} {
		served, miss := run(true, timeForm)
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, "body.at changed, and the moment record's full round saw in it stands in mock-1 at spec.resp.body") {
			t.Fatalf("another test's answer holds the time, time default %v: served %v, miss %+v; want the call refused at body.at, naming mock-1", timeForm, served != nil, miss)
		}
	}
}
