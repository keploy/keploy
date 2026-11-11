package http

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A client test's request whose token the application minted with an id in
// a claim (api-server's cluster proxy sends the cluster's id, made anew each
// run, in its token's cluster_id claim), and a later call of the test that
// carries that id whole: the learn step binds the id at the claim's word of
// the request's header (header.Authorization|jwt.cluster_id#0), so each
// replay binds the id the request sends there, and the call uses it. A
// claim that holds the two runs' ids at two words, or a live token that is
// no JWT, holds no word of the id that a replay can read: refused, as
// before.
func TestLearnMatchBindsAnIdAClientTestsJWTClaimCarries(t *testing.T) {
	const recID, fullID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	enc := base64.RawURLEncoding
	bearer := func(payload, sig string) map[string]string {
		return map[string]string{"Content-Type": "application/json",
			"Authorization": "Bearer " + enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString([]byte(sig))}
	}
	recHeader := bearer(`{"cid":"e2e-free-company","cluster_id":"`+recID+`","iat":1794407071}`, "recorded-signature")
	sent := time.Now()
	run := func(liveHeader map[string]string) (*models.Mock, *learnMiss, *learnDb) {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.SetClientTests(true)
		tr.Begin()
		recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/cluster/status", Timestamp: learnAt.Add(-time.Second),
			Header: recHeader, Body: `{"status":"active"}`}
		tr.SetCases([]learn.ClientCase{{Name: "post-cluster-status-3", Req: recReq}})
		liveReq := *recReq
		liveReq.Header, liveReq.Timestamp = liveHeader, sent
		if !tr.Incoming(&liveReq, "", sent) {
			t.Fatal("the run's request paired with none of the test's recorded requests")
		}
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/clusters/find", `{"filter":{"_id":"`+recID+`"}}`, nil, `{"found":1}`)}}
		in := live("POST", "/v1/clusters/find", `{"filter":{"_id":"`+fullID+`"}}`, nil)
		in.arrived = sent.Add(2 * time.Millisecond)
		served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
		return served, miss, db
	}

	served, miss, db := run(bearer(`{"cid":"e2e-free-company","cluster_id":"`+fullID+`","iat":1794407138}`, "this-run-signature"))
	if served == nil || miss != nil || len(db.consumed) != 1 {
		t.Fatalf("the call carrying the claim's id: served %v, miss %+v; want it served", served != nil, miss)
	}
	binds := db.consumed[0].Spec.LearnedBinds()["test case post-cluster-status-3"]
	vars := db.consumed[0].Spec.Learned()
	if len(binds) != 1 || len(vars) != 1 {
		t.Fatalf("binds %+v, variables %+v; want one variable bound at test case post-cluster-status-3", db.consumed[0].Spec.LearnedBinds(), vars)
	}
	for name, b := range binds {
		v := vars[name]
		if b.Recorded != recID || b.Bind != "header.Authorization|jwt.cluster_id#0" || v.Recorded != recID || v.Bind != "" || len(v.Use) != 1 || v.Use[0] != "body.filter._id" {
			t.Fatalf("bind %s = %+v, use %+v; want %q bound at the test case's header.Authorization|jwt.cluster_id#0, used at body.filter._id", name, b, v, recID)
		}
	}

	// A user's id, a UUID, in the token's sub claim: several words, bound
	// at the claim whole.
	const recSub, fullSub = "a20b6921-24c6-4a03-bcf9-b409842a88d0", "0c9e1f7a-5b2d-4e8c-9a61-3d7f0b2c4e19"
	recHeader = bearer(`{"cid":"e2e-free-company","sub":"`+recSub+`","iat":1794407071}`, "recorded-signature")
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.Begin()
	recReq := &models.HTTPReq{Method: "GET", URL: "http://localhost:8083/me", Timestamp: learnAt.Add(-time.Second), Header: recHeader}
	tr.SetCases([]learn.ClientCase{{Name: "get-me-1", Req: recReq}})
	liveReq := *recReq
	liveReq.Header, liveReq.Timestamp = bearer(`{"cid":"e2e-free-company","sub":"`+fullSub+`","iat":1794407138}`, "this-run-signature"), sent
	if !tr.Incoming(&liveReq, "", sent) {
		t.Fatal("the run's request paired with none of the test's recorded requests")
	}
	subDb := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/users/find", `{"filter":{"id":"`+recSub+`"}}`, nil, `{"found":1}`)}}
	in := live("POST", "/v1/users/find", `{"filter":{"id":"`+fullSub+`"}}`, nil)
	in.arrived = sent.Add(2 * time.Millisecond)
	if served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, subDb, n, opts); served == nil || miss != nil || len(subDb.consumed) != 1 {
		t.Fatalf("the call carrying the sub claim's UUID: served %v, miss %+v; want it served", served != nil, miss)
	}
	for _, b := range subDb.consumed[0].Spec.LearnedBinds()["test case get-me-1"] {
		if b.Recorded != recSub || b.Bind != "header.Authorization|jwt.sub" {
			t.Fatalf("bind %+v; want %q bound at the test case's header.Authorization|jwt.sub", b, recSub)
		}
	}
	if len(subDb.consumed[0].Spec.LearnedBinds()["test case get-me-1"]) != 1 {
		t.Fatalf("binds %+v; want one at test case get-me-1", subDb.consumed[0].Spec.LearnedBinds())
	}
	recHeader = bearer(`{"cid":"e2e-free-company","cluster_id":"`+recID+`","iat":1794407071}`, "recorded-signature")

	for what, liveHeader := range map[string]map[string]string{
		"the same claim, this run's id at another word": bearer(`{"cid":"e2e-free-company","cluster_id":"c `+fullID+`","iat":1794407138}`, "this-run-signature"),
		"a live token that is no JWT":                   {"Content-Type": "application/json", "Authorization": "Bearer " + fullID},
	} {
		served, miss, db := run(liveHeader)
		if served != nil || miss == nil || len(db.consumed) != 0 || !strings.Contains(miss.res.Why, "test case post-cluster-status-3 sent earlier in the test at header.Authorization, inside a longer value or at a place keploy cannot bind it") {
			t.Errorf("%s: served %v, miss %+v; want it refused, no claim bound", what, served != nil, miss)
		}
	}
}
