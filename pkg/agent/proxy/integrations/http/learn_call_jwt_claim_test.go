package http

import (
	"context"
	"encoding/base64"
	"maps"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

func callBearer(payload, sig string) string {
	enc := base64.RawURLEncoding
	return "Bearer " + enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString([]byte(sig))
}

// TestLearnMatchBindsNoClaimOfACallsToken: the application first sends the
// token to an earlier call (an auth check, a forwarded token), then a call
// that carries the claim's id whole. A JWT claim is a bind place only in a
// client test's request, which a replay reads; a call's request is put back
// at replay with its recorded values, and no replay can write a claim into
// a signed token (TestVariables_AWordKeployCannotFollowIsRefused). So:
//   - with no client test, the find is refused at learn, naming the claim
//     and why it is no place, and nothing is bound at the earlier call;
//   - with a client test that sent the token, the earlier call, which sent
//     the id only inside a longer value, does not hide the test case's
//     claim: the id is bound there, the test case's request reads this
//     run's id at it (RequestPlace), and nothing is bound at the call.
func TestLearnMatchBindsNoClaimOfACallsToken(t *testing.T) {
	const recID, fullID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	recTok := callBearer(`{"cid":"e2e-free-company","cluster_id":"`+recID+`","iat":1794407071}`, "recorded-signature")
	liveTok := callBearer(`{"cid":"e2e-free-company","cluster_id":"`+fullID+`","iat":1794407138}`, "this-run-signature")
	for _, client := range []bool{false, true} {
		name := "no client test"
		if client {
			name = "a client test that sent the token"
		}
		t.Run(name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			sent := time.Now()
			if client {
				tr.SetClientTests(true)
			}
			tr.Begin()
			recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/cluster/status", Timestamp: learnAt.Add(-time.Second),
				Header: map[string]string{"Content-Type": "application/json", "Authorization": recTok}, Body: `{"status":"active"}`}
			liveReq := *recReq
			liveReq.Header = map[string]string{"Content-Type": "application/json", "Authorization": liveTok}
			liveReq.Timestamp = sent
			if client {
				tr.SetCases([]learn.ClientCase{{Name: "post-cluster-status-3", Req: recReq}})
				if !tr.Incoming(&liveReq, "", sent) {
					t.Fatal("the run's request paired with none of the test's recorded requests")
				}
			}
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "GET", "/v1/auth/verify", "", map[string]string{"Authorization": recTok}, `{"ok":true}`),
				learnRecorded("mock-1", time.Millisecond, "POST", "/v1/clusters/find", `{"filter":{"_id":"`+recID+`"}}`, nil, `{"found":1}`),
			}}
			ctx := learn.WithTracker(context.Background(), tr)
			in0 := live("GET", "/v1/auth/verify", "", map[string]string{"Authorization": liveTok})
			in0.arrived = sent.Add(time.Millisecond)
			if s0, m0, _ := h.learnMatch(ctx, in0, db, n, opts); s0 == nil {
				t.Fatalf("the auth check was not served: %+v", m0)
			}
			in1 := live("POST", "/v1/clusters/find", `{"filter":{"_id":"`+fullID+`"}}`, nil)
			in1.arrived = sent.Add(2 * time.Millisecond)
			s1, m1, _ := h.learnMatch(ctx, in1, db, n, opts)
			for _, c := range db.consumed {
				for target, vs := range c.Spec.LearnedBinds() {
					for _, v := range vs {
						if target == "mock-0" && strings.Contains(v.Bind, "|jwt.") {
							t.Fatalf("%s binds at mock-0's %s, a claim of a call's token: no replay of mock-0 can put it back", c.Name, v.Bind)
						}
					}
				}
			}
			if !client {
				want := "body.filter._id carries a value mock-0 sent earlier in the test in the cluster_id claim of the JWT at header.authorization, a call's request: keploy binds a variable at a JWT claim only in a client test's request"
				if s1 != nil || m1 == nil || !strings.Contains(m1.res.Why, want) {
					t.Fatalf("the find: served %v, miss %+v; want it refused at learn: %s", s1 != nil, m1, want)
				}
				return
			}
			if s1 == nil || m1 != nil {
				t.Fatalf("the find: served %v, miss %+v; want it served, bound at the test case's claim", s1 != nil, m1)
			}
			var claimed int
			for _, c := range db.consumed {
				for target, vs := range c.Spec.LearnedBinds() {
					for _, b := range vs {
						if !strings.Contains(b.Bind, "|jwt.") {
							continue
						}
						claimed++
						if target != "test case post-cluster-status-3" || b.Bind != "header.Authorization|jwt.cluster_id#0" || b.Recorded != recID {
							t.Fatalf("%s binds %+v at %s; want %s at the test case's header.Authorization|jwt.cluster_id#0", c.Name, b, target, recID)
						}
						if v, why := RequestPlace(recReq, &liveReq, b.Bind, b.Recorded); v != fullID || why != "" {
							t.Fatalf("the test case's request reads %q (%q) at %s; want this run's %s", v, why, b.Bind, fullID)
						}
					}
				}
			}
			if claimed != 1 {
				t.Fatalf("%d binds at a claim; want the find's id at the test case's claim alone", claimed)
			}
		})
	}
}

// TestLearnMatchBindsAtACallThatSentTheIdWholeBeforeTheClaim: the client
// test sends the token, an earlier call of the test sends the claim's id
// whole (a lookup by id), then the find carries it. A call of the test that
// sent the value whole is named before the test case's claim, as before
// (placeRank): the lookup's id binds at the test case's claim, and the
// find's at the lookup's own place, where a replay reads what the
// application sends; the claim binds the lookup's alone.
func TestLearnMatchBindsAtACallThatSentTheIdWholeBeforeTheClaim(t *testing.T) {
	const recID, fullID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	recTok := callBearer(`{"cid":"e2e-free-company","cluster_id":"`+recID+`","iat":1794407071}`, "recorded-signature")
	liveTok := callBearer(`{"cid":"e2e-free-company","cluster_id":"`+fullID+`","iat":1794407138}`, "this-run-signature")
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	sent := time.Now()
	tr.SetClientTests(true)
	tr.Begin()
	recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/cluster/status", Timestamp: learnAt.Add(-time.Second),
		Header: map[string]string{"Content-Type": "application/json", "Authorization": recTok}, Body: `{"status":"active"}`}
	liveReq := *recReq
	liveReq.Header = map[string]string{"Content-Type": "application/json", "Authorization": liveTok}
	liveReq.Timestamp = sent
	tr.SetCases([]learn.ClientCase{{Name: "post-cluster-status-3", Req: recReq}})
	if !tr.Incoming(&liveReq, "", sent) {
		t.Fatal("the run's request paired with none of the test's recorded requests")
	}
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/clusters/get", `{"id":"`+recID+`"}`, nil, `{"ok":true}`),
		learnRecorded("mock-1", time.Millisecond, "POST", "/v1/clusters/find", `{"filter":{"_id":"`+recID+`"}}`, nil, `{"found":1}`),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	in0 := live("POST", "/v1/clusters/get", `{"id":"`+fullID+`"}`, nil)
	in0.arrived = sent.Add(time.Millisecond)
	if s0, m0, _ := h.learnMatch(ctx, in0, db, n, opts); s0 == nil || m0 != nil {
		t.Fatalf("the lookup: served %v, miss %+v; want it served, bound at the test case's claim", s0 != nil, m0)
	}
	in1 := live("POST", "/v1/clusters/find", `{"filter":{"_id":"`+fullID+`"}}`, nil)
	in1.arrived = sent.Add(2 * time.Millisecond)
	if s1, m1, _ := h.learnMatch(ctx, in1, db, n, opts); s1 == nil || m1 != nil {
		t.Fatalf("the find: served %v, miss %+v; want it served, bound at the lookup's id", s1 != nil, m1)
	}
	want := map[string][2]string{
		"mock-0": {"test case post-cluster-status-3", "header.Authorization|jwt.cluster_id#0"},
		"mock-1": {"mock-0", "body.id"},
	}
	got := map[string][2]string{}
	for _, c := range db.consumed {
		for target, vs := range c.Spec.LearnedBinds() {
			for _, b := range vs {
				if b.Recorded != recID {
					continue
				}
				if _, dup := got[c.Name]; dup {
					t.Fatalf("%s binds the id twice: %+v", c.Name, c.Spec.LearnedBinds())
				}
				got[c.Name] = [2]string{target, b.Bind}
			}
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("the id binds at %v; want %v: the lookup at the test case's claim, the find at the lookup's id, which it sent whole", got, want)
	}
}

// TestLearnMatchBindsAtAWholeClientBodyAnEarlierTokenCarried: in an earlier
// test the application sent a token it minted, which holds a new cluster's
// id in a claim, to an auth check; the next client test sends the id whole
// in its request body, and the find carries it whole. The earlier call sent
// the id only inside a longer value, which binds nothing, and a call's
// token's claim is no bind place: it never hides the test case's body. The
// find is served, its id bound at the test case's body.cluster_id, where the
// test case's request reads this run's id (RequestPlace), and nothing is
// bound at the earlier call.
func TestLearnMatchBindsAtAWholeClientBodyAnEarlierTokenCarried(t *testing.T) {
	const recID, fullID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	recTok := callBearer(`{"cid":"e2e-free-company","cluster_id":"`+recID+`"}`, "recorded-signature")
	liveTok := callBearer(`{"cid":"e2e-free-company","cluster_id":"`+fullID+`"}`, "this-run-signature")
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.NewSet()
	tr.SetClientTests(true)
	ctx := learn.WithTracker(context.Background(), tr)
	sent := time.Now()
	clientTest := func(name, url, recBody, liveBody string, recAt, at time.Time) *models.HTTPReq {
		rec := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083" + url, Timestamp: recAt,
			Header: map[string]string{"Content-Type": "application/json"}, Body: recBody}
		l := *rec
		l.Body, l.Timestamp = liveBody, at
		tr.SetCases([]learn.ClientCase{{Name: name, Req: rec}})
		if !tr.Incoming(&l, "", at) {
			t.Fatalf("the run's request to %s paired with none of the test's recorded requests", url)
		}
		return rec
	}
	tr.Begin()
	clientTest("post-cluster-login-1", "/cluster/login", `{"cluster_name":"e2e-old-proxy"}`, `{"cluster_name":"e2e-old-proxy"}`, learnAt.Add(-time.Second), sent)
	a := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "GET", "/v1/auth/verify", "", map[string]string{"Authorization": recTok}, `{"ok":true}`),
	}}
	in0 := live("GET", "/v1/auth/verify", "", map[string]string{"Authorization": liveTok})
	in0.arrived = sent.Add(time.Millisecond)
	if s0, m0, _ := h.learnMatch(ctx, in0, a, n, opts); s0 == nil || m0 != nil {
		t.Fatalf("the earlier test's auth check: served %v, miss %+v", s0 != nil, m0)
	}
	tr.End()
	tr.Begin()
	body := func(id string) string { return `{"cluster_id":"` + id + `","status":"active"}` }
	recReq := clientTest("post-cluster-status-3", "/cluster/status", body(recID), body(fullID), learnAt.Add(time.Second), sent.Add(time.Second))
	liveReq := *recReq
	liveReq.Body = body(fullID)
	b := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-1", 2*time.Second, "POST", "/v1/clusters/find", `{"filter":{"_id":"`+recID+`"}}`, nil, `{"found":1}`),
	}}
	in1 := live("POST", "/v1/clusters/find", `{"filter":{"_id":"`+fullID+`"}}`, nil)
	in1.arrived = sent.Add(time.Second + 2*time.Millisecond)
	if s1, m1, _ := h.learnMatch(ctx, in1, b, n, opts); s1 == nil || m1 != nil {
		t.Fatalf("the find: served %v, miss %+v; want it served, bound at the test case's body.cluster_id", s1 != nil, m1)
	}
	tr.End()
	var found bool
	for _, c := range append(append([]models.Mock{}, a.consumed...), b.consumed...) {
		for target, vs := range c.Spec.LearnedBinds() {
			for _, v := range vs {
				if target == "mock-0" {
					t.Fatalf("%s binds %+v at mock-0, which sent the id only inside a longer value", c.Name, v)
				}
				if c.Name != "mock-1" || v.Recorded != recID {
					continue
				}
				if target != "test case post-cluster-status-3" || v.Bind != "body.cluster_id" {
					t.Fatalf("the find binds the id at %s %s; want the test case's body.cluster_id, which sent it whole", target, v.Bind)
				}
				if got, why := RequestPlace(recReq, &liveReq, v.Bind, v.Recorded); got != fullID || why != "" {
					t.Fatalf("the test case's request reads %q (%q) at %s; want this run's %s", got, why, v.Bind, fullID)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the find binds no id at the test case: %+v", b.consumed)
	}
}

// TestSentTextIsTheRecordedLeafOfThatMocksRequest: a refusal reads the
// claim in the text the named mock's request was recorded with at the
// path: not another mock's, not an answer's, not another leaf's.
func TestSentTextIsTheRecordedLeafOfThatMocksRequest(t *testing.T) {
	earlier := []learn.Earlier{
		{Mock: "mock-a", Seen: []learn.Seen{{Side: learn.Request, Leaf: learn.Leaf{Path: "header.authorization", Value: "a-request"}}}},
		{Mock: "mock-b", Seen: []learn.Seen{
			{Side: learn.Response, Leaf: learn.Leaf{Path: "header.authorization", Value: "b-answer"}},
			{Side: learn.Request, Leaf: learn.Leaf{Path: "header.x-other", Value: "b-other"}},
			{Side: learn.Request, Leaf: learn.Leaf{Path: "header.authorization", Value: "b-request"}},
		}},
	}
	for _, tc := range []struct{ mock, want string }{{"mock-b", "b-request"}, {"mock-a", "a-request"}, {"mock-c", ""}} {
		if got := sentText(earlier, tc.mock, "header.authorization"); got != tc.want {
			t.Errorf("sentText(%s) = %q; want %q", tc.mock, got, tc.want)
		}
	}
}
