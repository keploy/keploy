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
// tests) whose stamps the app read at the start of its handler, before the
// calls it made to load the service account: in the recording those calls
// took half a millisecond, and the PUT's own window holds the stamp's
// second; in record's full round they ran across a second boundary, so the
// PUT's window opens in the second after the stamp's, and only the window
// since the test began holds it. keploy.yml's clock rules declare createdAt
// and updatedAt (no rule at expiresAt), as api-server's e2e does.
//
// Its expiresAt is the stamp plus the policy's ttl. Where the test's name
// was read in the stamp's second in both runs, the offset rule refuses it
// as built on the name ("8760h0m0s after the moment test case ... holds");
// where round F read the name in the second before the stamp, no rule
// explains it ("no clock read by the app matched it"). Both runs hold it as
// a read of the clock plus the ttl since the test began, so under the time
// default it is a time all the same, which never blocks a test (2026-11-11):
// learned as that read since its test began, and checked by its form. With
// keploy.yml's times strict the refusal stands, as before the default.
func TestLearnMatchHoldsAnExpiryStampedBeforeItsCallsWindow(t *testing.T) {
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
	// The record's id as recorded, so that only the stamps changed: what
	// the account's load gave the application keploy does not see here.
	const recID = "5b39677d-8406-4d49-bfd1-853a0bf33fb8"
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	stamps := []models.DeclaredSource{
		{Rule: "pat-created-at", Kind: models.DeclaredSourceClock, Field: "body.data.createdAt", Clock: plain},
		{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: plain},
	}
	// Recorded: the test began at recBegin, read the clock for the name 100
	// ms on, the app stamped the record 120 ms on, loaded the service
	// account at 125 ms and PUT the record at 130 ms. Round F: began some
	// ten minutes on, and loaded the account and PUT the record a second
	// boundary after the stamp (shape).
	recBegin := learnAt.Add(time.Second)
	// (Seven seconds past the minute: two runs' stamps a whole number of
	// minutes apart leave the cut keploy checks one at untold.)
	began := learnAt.Add(10*time.Minute + 7*time.Second)
	type shape struct {
		nameAt, stampAt, loadAt, putAt time.Duration
		// liveExpiry: round F's expiry, where it is not the recording's.
		liveExpiry time.Duration
		// recLate: the recording too loaded the account and PUT the record
		// a second boundary after the stamp (950, 1010 and 1020 ms on).
		recLate bool
	}
	run := func(timeForm bool, expiry time.Duration, s shape, sources []models.DeclaredSource) (*models.Mock, *learnMiss, *learnDb) {
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
		tr.SetCases([]learn.ClientCase{{Name: "post-query-80", Req: recReq}})
		liveReq := *recReq
		liveReq.Body, liveReq.Timestamp = name(began.Add(s.nameAt)), began.Add(s.nameAt)
		if !tr.Incoming(&liveReq, "", began.Add(s.nameAt)) {
			t.Fatal("the run's request paired with none of the test's recorded requests")
		}
		// The service account's load, the call before the PUT in each run.
		recStamp, recLoad, recPut := 120*time.Millisecond, 125*time.Millisecond, 130*time.Millisecond
		if s.recLate {
			recStamp, recLoad, recPut = 950*time.Millisecond, 1010*time.Millisecond, 1020*time.Millisecond
		}
		load := learnRecorded("mock-load", time.Second+recLoad, "GET", "/v1/service-accounts/6af52ec3", "", nil, `{"kind":"custom"}`)
		tr.ServedMockAt(load, began.Add(s.loadAt))
		tr.ServedMockAsRecorded(load)
		put := learnRecorded("mock-0", time.Second+recPut, "PUT", path(recID), body(recID, recBegin.Add(recStamp), expiry), nil, `{"data":{"version":1}}`)
		db := &learnDb{perTest: []*models.Mock{put}, staged: []*models.Mock{load}}
		liveExpiry := expiry
		if s.liveExpiry != 0 {
			liveExpiry = s.liveExpiry
		}
		in := live("PUT", path(recID), body(recID, began.Add(s.stampAt), liveExpiry), nil)
		in.arrived = began.Add(s.putAt)
		var store integrations.MockMemDb = db
		if timeForm {
			store = timeFormDb{db}
		}
		served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, store, n, opts)
		return served, miss, db
	}
	const (
		year = 365 * 24 * time.Hour
		day  = 24 * time.Hour
	)
	for _, c := range []struct {
		what   string
		expiry time.Duration
		s      shape
		// line: the refusal without the time default.
		line string
		// refused: the PUT stays refused under the time default too.
		refused bool
		// rule: a clock rule at expiresAt too, of the expiry's range.
		rule bool
	}{
		{"a year on, the name read in the stamp's second", year, shape{nameAt: 100 * time.Millisecond, stampAt: 950 * time.Millisecond, loadAt: 1010 * time.Millisecond, putAt: 1020 * time.Millisecond},
			"body.data.expiresAt changed, and its moment, to the second, stands 8760h0m0s after the moment test case post-query-80 at body.variables.input.name holds", false, false},
		{"a day on, round F's name read in the second before the stamp", day, shape{nameAt: 100 * time.Millisecond, stampAt: 1950 * time.Millisecond, loadAt: 2010 * time.Millisecond, putAt: 2020 * time.Millisecond},
			"body.data.expiresAt changed, and no clock read by the app matched it", false, false},
		// A clock rule at expiresAt leaves the read learning found as it is
		// (DeclaredClockLearned decides only the offset rule's refusal in
		// its place), so under the time default it is answered as without
		// the rule; with times strict the refusal of no clock read stands,
		// which the rule does not settle, as before.
		{"a day on, round F's name read in the second before the stamp, a clock rule at expiresAt", day, shape{nameAt: 100 * time.Millisecond, stampAt: 1950 * time.Millisecond, loadAt: 2010 * time.Millisecond, putAt: 2020 * time.Millisecond},
			"body.data.expiresAt changed, and no clock read by the app matched it", false, true},
		// Neither run's own window holds the stamp: nothing shows it a read
		// made for the call, and it stays refused.
		{"a year on, both runs' PUTs a second boundary after the stamp", year, shape{nameAt: 100 * time.Millisecond, stampAt: 950 * time.Millisecond, loadAt: 1010 * time.Millisecond, putAt: 1020 * time.Millisecond, recLate: true},
			"body.data.expiresAt changed", true, false},
		// Round F's expiry stands a day further on than recorded: no one
		// offset from the clock holds both runs, and it stays refused.
		{"round F's expiry a day longer", year, shape{nameAt: 100 * time.Millisecond, stampAt: 1950 * time.Millisecond, loadAt: 2010 * time.Millisecond, putAt: 2020 * time.Millisecond, liveExpiry: year + day},
			"body.data.expiresAt changed", true, false},
	} {
		for _, timeForm := range []bool{true, false} {
			sources := stamps
			if c.rule {
				sources = append(append([]models.DeclaredSource(nil), stamps...), models.DeclaredSource{Rule: "pat-expires-at", Kind: models.DeclaredSourceClock,
					Field: "body.data.expiresAt", Clock: &models.ClockOffset{Min: c.expiry, Max: c.expiry + time.Second, Res: time.Second}})
			}
			served, miss, db := run(timeForm, c.expiry, c.s, sources)
			if !timeForm || c.refused {
				if served != nil || miss == nil || !strings.Contains(miss.res.Why, c.line) {
					t.Fatalf("%s, time default %v: served %v, miss %+v; want the PUT refused (%q)", c.what, timeForm, served != nil, miss, c.line)
				}
				continue
			}
			if served == nil || miss != nil || len(db.consumed) != 1 {
				t.Fatalf("%s, time default: served %v, miss %+v; want the PUT answered, expiresAt a time", c.what, served != nil, miss)
			}
			o, ok := db.consumed[0].Spec.Clocks["body.data.expiresAt"]
			if !ok || o.Since != models.SinceTestBegin || o.Min > c.expiry || o.Max < c.expiry || o.Layout != "epoch-s" {
				t.Errorf("%s, time default: expiresAt learned as %+v; want the clock plus %v in epoch seconds, checked from its test's begin", c.what, o, c.expiry)
			}
		}
	}
}
