package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

const unboundRecID, unboundFullID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"

// startUpThenFind runs a set, with the proven switch at sw, whose start-up
// call (mock-s, a mock of the pool) the learn step paired, its body sending
// boot of the recorded id and of this run's (the set's start-up sent the
// id, as the application minted it there); then a test whose client test,
// where clientReq is not nil, sent its request (recorded and live by id);
// then the test's find, which carries this run's id whole in a JSON body
// or, with form, in a form field. It returns what the find served and
// missed, and its recordings.
func startUpThenFind(t *testing.T, sw string, boot func(id string) string, clientReq func(id string, at time.Time) *models.HTTPReq, form bool) (*models.Mock, *learnMiss, *learnDb) {
	t.Helper()
	h, n, opts := learnOn(t)
	t.Setenv(provenswitch.Env, sw)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.NewSet()
	tr.SetClientTests(clientReq != nil)
	ctx := learn.WithTracker(context.Background(), tr)
	bootMock := learnRecorded("mock-s", -time.Minute, "POST", "/v1/register", boot(unboundRecID), nil, `{"ok":true}`)
	bootMock.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{bootMock}}, pool: map[time.Time]bool{bootMock.Spec.ReqTimestampMock: true}, observe: tr.ServedMock}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/register", boot(unboundFullID), nil), db, n, opts); err != nil || served == nil || miss != nil {
		t.Fatalf("the drifted start-up call is paired: served %v, miss %+v, err %v", served != nil, miss, err)
	}
	tr.Begin()
	sent := time.Now()
	if clientReq != nil {
		tr.SetCases([]learn.ClientCase{{Name: "post-cluster-status-3", Req: clientReq(unboundRecID, learnAt.Add(500*time.Millisecond))}})
		if !tr.Incoming(clientReq(unboundFullID, sent), "", sent) {
			t.Fatal("the run's request paired with none of the test's recorded requests")
		}
	}
	body, header := func(id string) string { return `{"filter":{"_id":"` + id + `"}}` }, map[string]string(nil)
	if form {
		body, header = func(id string) string { return "_id=" + id + "&limit=1" }, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	}
	find := learnRecorded("mock-1", time.Second, "POST", "/v1/clusters/find", body(unboundRecID), header, `{"found":1}`)
	tdb := &learnDb{perTest: []*models.Mock{find}, pool: map[time.Time]bool{bootMock.Spec.ReqTimestampMock: true}}
	in := live("POST", "/v1/clusters/find", body(unboundFullID), header)
	in.arrived = sent.Add(2 * time.Millisecond)
	served, miss, err := h.learnMatch(ctx, in, tdb, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	return served, miss, tdb
}

// unboundClient is a client test's request that sent the id whole in its
// body or, with claim, in the cluster_id claim of its token.
func unboundClient(claim bool) func(id string, at time.Time) *models.HTTPReq {
	return func(id string, at time.Time) *models.HTTPReq {
		h := map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"}
		body := `{"cluster_id":"` + id + `","status":"active"}`
		if claim {
			h["Authorization"] = callBearer(`{"cid":"e2e-free-company","cluster_id":"`+id+`"}`, "signature-of-"+id)
			body = `{"status":"active"}`
		}
		return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/cluster/status", Timestamp: at, Header: h, Body: body}
	}
}

// TestLearnMatchAStartUpCallNoPlanBindsNeverHidesAClientPlace: the set's
// start-up call sent the id whole, and the test's client test sent it whole
// in its body, or in a claim of its token; the test's find carries it.
// Without the proven switch no plan binds at the start-up's call (only a
// test's own calls carry its variables), so it never hides the client
// test's place: the find is served, its id bound at the test case, where
// its request reads this run's id (RequestPlace), not refused at the call.
// Under the switch the start-up's call binds the run's variable, as before.
func TestLearnMatchAStartUpCallNoPlanBindsNeverHidesAClientPlace(t *testing.T) {
	boot := func(id string) string { return `{"cluster_id":"` + id + `"}` }
	for _, claim := range []bool{false, true} {
		for _, sw := range []string{"1", ""} {
			name, wantAt := "the client body switch "+sw, "body.cluster_id"
			if claim {
				name, wantAt = "the client claim switch "+sw, "header.Authorization|jwt.cluster_id#0"
			}
			t.Run(name, func(t *testing.T) {
				served, miss, tdb := startUpThenFind(t, sw, boot, unboundClient(claim), false)
				if served == nil || miss != nil || len(tdb.consumed) != 1 {
					t.Fatalf("the find: served %v, miss %+v; want it served", served != nil, miss)
				}
				binds := tdb.consumed[0].Spec.LearnedBinds()
				if sw == "1" {
					if len(binds["mock-s"]) != 1 || len(binds["test case post-cluster-status-3"]) != 0 {
						t.Fatalf("the find binds %+v; want the run's variable at mock-s, which binds the id under the switch", binds)
					}
					return
				}
				vs := binds["test case post-cluster-status-3"]
				if len(binds["mock-s"]) != 0 || len(vs) != 1 {
					t.Fatalf("the find binds %+v; want one variable at the test case, none at mock-s", binds)
				}
				for _, v := range vs {
					rec, lv := unboundClient(claim)(unboundRecID, learnAt), unboundClient(claim)(unboundFullID, learnAt)
					if got, why := RequestPlace(rec, lv, v.Bind, v.Recorded); v.Bind != wantAt || v.Recorded != unboundRecID || got != unboundFullID || why != "" {
						t.Fatalf("the find binds %s at the test case's %s, which reads %q (%q); want %s at %s, read as %s", v.Recorded, v.Bind, got, why, unboundRecID, wantAt, unboundFullID)
					}
				}
			})
		}
	}
}

// TestLearnMatchARefusalNamesWhereTheRunSentTheValue: a refused find names
// where the value it carries was sent: in the test set's start-up, or in an
// earlier test of the set, never "earlier in the test" of a place the
// running test did not send. The start-up's call sent the id inside a
// longer value, or in a claim of its token, which no replay can bind; an
// earlier test's own call (the run's under the switch) sent the token.
// Without the switch the start-up's call carries no variable of the test;
// under it, the find's form field is no place a variable follows to.
func TestLearnMatchARefusalNamesWhereTheRunSentTheValue(t *testing.T) {
	for _, sw := range []string{"1", ""} {
		t.Run("a longer value switch "+sw, func(t *testing.T) {
			served, miss, _ := startUpThenFind(t, sw, func(id string) string { return `{"note":"cluster ` + id + ` registered"}` }, nil, false)
			want := "body.filter._id carries a value mock-s sent in the test set's start-up at body.note, inside a longer value"
			if served != nil || miss == nil || !strings.Contains(miss.res.Why, want) {
				t.Fatalf("the find: served %v, miss %+v; want it refused: %s", served != nil, miss, want)
			}
		})
	}
	whole := func(id string) string { return `{"cluster_id":"` + id + `"}` }
	t.Run("a whole value without the switch", func(t *testing.T) {
		served, miss, _ := startUpThenFind(t, "", whole, nil, false)
		want := "body.filter._id carries a value mock-s sent in the test set's start-up, and only a test's own calls carry its variables"
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, want) {
			t.Fatalf("the find: served %v, miss %+v; want it refused: %s", served != nil, miss, want)
		}
	})
	t.Run("a form field under the switch", func(t *testing.T) {
		served, miss, _ := startUpThenFind(t, "1", whole, nil, true)
		want := "carries a value mock-s sent in the test set's start-up, at a place keploy cannot follow a variable to (a form or multipart field)"
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, want) {
			t.Fatalf("the find: served %v, miss %+v; want it refused: %s", served != nil, miss, want)
		}
	})
	t.Run("an earlier test's token", func(t *testing.T) {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.NewSet()
		ctx := learn.WithTracker(context.Background(), tr)
		tok := func(id string) string {
			return callBearer(`{"cid":"e2e-free-company","cluster_id":"`+id+`"}`, "signature-of-"+id)
		}
		tr.Begin()
		a := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "GET", "/v1/auth/verify", "", map[string]string{"Authorization": tok(unboundRecID)}, `{"ok":true}`)}}
		if s, m, _ := h.learnMatch(ctx, live("GET", "/v1/auth/verify", "", map[string]string{"Authorization": tok(unboundFullID)}), a, n, opts); s == nil || m != nil {
			t.Fatalf("the earlier test's auth check: served %v, miss %+v", s != nil, m)
		}
		tr.End()
		tr.Begin()
		b := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/clusters/find", `{"filter":{"_id":"`+unboundRecID+`"}}`, nil, `{"found":1}`)}}
		s, m, _ := h.learnMatch(ctx, live("POST", "/v1/clusters/find", `{"filter":{"_id":"`+unboundFullID+`"}}`, nil), b, n, opts)
		want := "body.filter._id carries a value mock-0 sent in an earlier test of the set in the cluster_id claim of the JWT at header.authorization, a call's request"
		if s != nil || m == nil || !strings.Contains(m.res.Why, want) {
			t.Fatalf("the find: served %v, miss %+v; want it refused: %s", s != nil, m, want)
		}
	})
}

// clientThenOwnCallThenFind runs one test, with the proven switch at sw:
// its client test sent clientBody (by value) to /cluster/status, the
// test's own call (mock-a) then sent ownBody, and the test's find sends
// findBody. Each runs recorded with rec and with full in this run. It
// returns what the find served and missed, and the test's recordings.
func clientThenOwnCallThenFind(t *testing.T, sw, rec, full string, clientBody, ownBody, findBody func(string) string) (*models.Mock, *learnMiss, []models.Mock) {
	t.Helper()
	h, n, opts := learnOn(t)
	t.Setenv(provenswitch.Env, sw)
	tr := learn.NewTracker()
	tr.NewSet()
	tr.SetClientTests(true)
	ctx := learn.WithTracker(context.Background(), tr)
	tr.Begin()
	sent := time.Now()
	recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/cluster/status", Timestamp: learnAt.Add(500 * time.Millisecond),
		Header: map[string]string{"Content-Type": "application/json"}, Body: clientBody(rec)}
	liveReq := *recReq
	liveReq.Body, liveReq.Timestamp = clientBody(full), sent
	tr.SetCases([]learn.ClientCase{{Name: "post-cluster-status-3", Req: recReq}})
	if !tr.Incoming(&liveReq, "", sent) {
		t.Fatal("the run's request paired with none of the test's recorded requests")
	}
	a := &learnDb{perTest: []*models.Mock{learnRecorded("mock-a", 700*time.Millisecond, "POST", "/v1/audit", ownBody(rec), nil, `{"ok":true}`)}}
	in0 := live("POST", "/v1/audit", ownBody(full), nil)
	in0.arrived = sent.Add(time.Millisecond)
	if s, m, err := h.learnMatch(ctx, in0, a, n, opts); err != nil || s == nil || m != nil {
		t.Fatalf("the test's own call: served %v, miss %+v, err %v", s != nil, m, err)
	}
	b := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/v1/clusters/find", findBody(rec), nil, `{"found":1}`)}}
	in1 := live("POST", "/v1/clusters/find", findBody(full), nil)
	in1.arrived = sent.Add(2 * time.Millisecond)
	served, miss, err := h.learnMatch(ctx, in1, b, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	return served, miss, b.consumed
}

// TestLearnMatchATestsOwnWholeCallBindsBeforeAClientTestsWholeBody: the
// client test sent the id whole in its body, and the test's own call then
// sent it whole too; the find carries it. Both places bind it, and the
// test's own call, where the application sent it, is named first (its
// rank is below a client test's whole request), though the client test's
// request came first: the find binds at mock-a's body.id. Under both
// switches.
func TestLearnMatchATestsOwnWholeCallBindsBeforeAClientTestsWholeBody(t *testing.T) {
	body := func(k string) func(string) string {
		return func(id string) string { return `{"` + k + `":"` + id + `"}` }
	}
	for _, sw := range []string{"1", ""} {
		t.Run("switch "+sw, func(t *testing.T) {
			served, miss, consumed := clientThenOwnCallThenFind(t, sw, unboundRecID, unboundFullID, body("cluster_id"), body("id"), func(id string) string { return `{"filter":{"_id":"` + id + `"}}` })
			if served == nil || miss != nil || len(consumed) != 1 {
				t.Fatalf("the find: served %v, miss %+v; want it served", served != nil, miss)
			}
			binds := consumed[0].Spec.LearnedBinds()
			if vs := binds["mock-a"]; len(vs) != 1 || len(binds["test case post-cluster-status-3"]) != 0 {
				t.Fatalf("the find binds %+v; want one variable at mock-a, the test's own call that sent the id whole, none at the test case", binds)
			}
			for _, v := range binds["mock-a"] {
				if v.Bind != "body.id" || v.Recorded != unboundRecID {
					t.Fatalf("the find binds %s at mock-a's %s; want %s at body.id", v.Recorded, v.Bind, unboundRecID)
				}
			}
		})
	}
}
