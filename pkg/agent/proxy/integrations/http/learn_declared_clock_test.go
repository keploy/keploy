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

// timeFormDb is a learn store whose replay turned the time default on
// (integrations.TimeFormReader), as keploy mock replay's scope table does.
type timeFormDb struct{ *learnDb }

func (timeFormDb) TimeForm() bool { return true }

// The PAT store's last-use touch in record's full round (api-server's e2e,
// one run of the once rule): the test creates an app named
// "[E2E] app-crud-<UnixNano>"; the app authenticates (it reads the token,
// GET .../_id/e2e-go-key) and, in the same second as the name, stamps
// lastUsedAt (an epoch second) and updatedAt (RFC 3339) on the token and PUTs
// it back. The learn step finds that second in the client test's request,
// inside the name, in both runs, and refuses the PUT ("body.data.lastUsedAt
// carries a value test case post-client-v1-apps-1 sent earlier in the test
// at body.name, inside a longer value or at a place keploy cannot bind it"):
// it cannot tell a pick from the name by the clock from a read of the clock.
// keploy.yml's two clock rules declare both fields reads of the clock; with
// the T2 time default on, a declared clock passes by its type and form, so
// the PUT is answered and both fields are learned as clocks checked by
// their form. Without the rules it is refused as before. The application's
// clock is known in the call's span in both runs, as the hooks tell it in
// record's full round.
func TestLearnMatchHoldsADeclaredClockAnEarlierNameEchoes(t *testing.T) {
	const token = "/v1/secret/data/keploy/personal-access-tokens/_id/e2e-go-key"
	const path = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/e2e-go-key"
	body := func(at time.Time) string {
		return `{"data":{"_id":"e2e-go-key","cid":"keploy.io","createdAt":1767225600,"keyType":"pat","lastUsedAt":` +
			strconv.FormatInt(at.Unix(), 10) + `,"name":"e2e-go","status":"active","updatedAt":"` + at.UTC().Format(time.RFC3339) + `"},"options":{"cas":1}}`
	}
	name := func(read time.Time) string {
		return `{"endpoint":"https://example.com","name":"[E2E] app-crud-` + strconv.FormatInt(read.UnixNano(), 10) + `"}`
	}
	identity := func(r time.Time) (time.Time, bool) { return r, true }
	// Recorded: the test began at recBegin, its harness read the clock for
	// the name 100 ms on, the app read the token 300 ms on and sent the
	// touch, stamped 400 ms on, at 500 ms.
	recBegin := learnAt.Add(time.Second)
	read := learnRecorded("mock-0", 1300*time.Millisecond, "GET", token, "", nil, `{"data":{"data":{"_id":"e2e-go-key"}}}`)
	touch := func(answer string) *models.Mock {
		return learnRecorded("mock-1", 1500*time.Millisecond, "PUT", path, body(recBegin.Add(400*time.Millisecond)), nil, answer)
	}
	const version = `{"data":{"version":2}}`
	// echoed is an answer that holds the recorded lastUsedAt: a replay would
	// serve that recorded moment.
	echoed := `{"data":{"version":2,"lastUsedAt":` + strconv.FormatInt(recBegin.Unix(), 10) + `}}`
	// Round F: the same steps, ten minutes on.
	began := learnAt.Add(10 * time.Minute)
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	rules := []models.DeclaredSource{
		{Rule: "pat-last-used-at", Kind: models.DeclaredSourceClock, Field: "body.data.lastUsedAt", Clock: plain},
		{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: plain},
	}
	run := func(sources []models.DeclaredSource, timeForm bool, answer string) (*models.Mock, *learnMiss, *learnDb) {
		h, n, opts := learnOn(t)
		n.sources = func(mock string) []models.DeclaredSource {
			if mock == "mock-1" {
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
		recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/client/v1/apps", Timestamp: recBegin.Add(100 * time.Millisecond),
			Header: jsonType, Body: name(recBegin.Add(100 * time.Millisecond))}
		tr.SetCases([]learn.ClientCase{{Name: "post-client-v1-apps-1", Req: recReq}})
		liveReq := *recReq
		liveReq.Body, liveReq.Timestamp = name(began.Add(100*time.Millisecond)), began.Add(100*time.Millisecond)
		if !tr.Incoming(&liveReq, "", began.Add(100*time.Millisecond)) {
			t.Fatal("the run's request paired with none of the test's recorded requests")
		}
		tr.ServedMockAt(read, began.Add(300*time.Millisecond))
		tr.ServedMockAsRecorded(read)
		db := &learnDb{perTest: []*models.Mock{read, touch(answer)}}
		in := live("PUT", path, body(began.Add(400*time.Millisecond)), nil)
		in.arrived = began.Add(450 * time.Millisecond)
		var store integrations.MockMemDb = db
		if timeForm {
			store = timeFormDb{db}
		}
		served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, store, n, opts)
		return served, miss, db
	}

	served, miss, _ := run(nil, true, version)
	if served != nil || miss == nil || !strings.Contains(miss.res.Why, "body.data.lastUsedAt carries a value test case post-client-v1-apps-1 sent earlier in the test at body.name, inside a longer value or at a place keploy cannot bind it") {
		t.Fatalf("no rules: served %v, miss %+v; want the PUT refused on the name's echo, as the run refused it", served != nil, miss)
	}
	// Under the time default both fields are checked by their form; with
	// keploy.yml's times strict, by the rules' range in the call's span, in
	// the layout both runs wrote.
	for _, timeForm := range []bool{true, false} {
		served, miss, db := run(rules, timeForm, version)
		if served == nil || miss != nil || len(db.consumed) != 1 {
			t.Fatalf("both fields declared clocks, time default %v: served %v, miss %+v; want the PUT answered", timeForm, served != nil, miss)
		}
		clocks := db.consumed[0].Spec.Clocks
		for f, lay := range map[string]string{"body.data.lastUsedAt": "epoch-s", "body.data.updatedAt": "iso-second"} {
			o, ok := clocks[f]
			if !ok || (o.Form != "") != timeForm || o.Res != time.Second || o.Layout != lay {
				t.Errorf("time default %v: %s learned as %+v (clocks %v); want the rule's 1s range in layout %s, checked by its form alone under the default", timeForm, f, o, clocks, lay)
			}
		}
		if len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
			t.Errorf("time default %v: noise %v; want none: a declared clock stays compared", timeForm, db.consumed[0].Spec.ReqBodyNoise)
		}
	}
	// An answer that holds the recorded lastUsedAt: with keploy.yml's times
	// strict the rule leaves the field to learning, which refuses the PUT, as
	// Mongo's learning refuses a clock read its call's answer holds; under
	// the time default it is checked by its form.
	if served, miss, _ := run(rules, false, echoed); served != nil || miss == nil {
		t.Fatalf("times strict, the answer holds lastUsedAt: served %v, miss %+v; want the PUT refused", served != nil, miss)
	}
	if served, miss, _ := run(rules, true, echoed); served == nil || miss != nil {
		t.Fatalf("time default, the answer holds lastUsedAt: served %v, miss %+v; want the PUT answered", served != nil, miss)
	}
}
