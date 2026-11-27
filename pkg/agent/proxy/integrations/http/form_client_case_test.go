package http

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// TestRequestPlaceReadsFormURLEncodedClientRequest verifies the replay bind-read
// side for raw wire application/x-www-form-urlencoded client test requests
// across both exact and parameterized/mixed-case Content-Types, preserving
// encoded '&' (%26), '=' (%3D), '+' (%2B), and '%' (%25) without double-decoding
// or phantom keys, failing closed on invalid escapes, and preserving JSON
// bodies sent under a form Content-Type.
func TestRequestPlaceReadsFormURLEncodedClientRequest(t *testing.T) {
	formReq := func(ct string, vals url.Values) *models.HTTPReq {
		return &models.HTTPReq{
			Method: "POST",
			URL:    "http://localhost:8083/oauth/par",
			Header: map[string]string{"content-type": ct, "Host": "localhost:8083"},
			Body:   vals.Encode(),
		}
	}
	recVals := url.Values{
		"client_id":       {"demo-cli"},
		"redirect_uri":    {"https://app.example/cb?state=abc&nonce=111"},
		"installation_id": {"cli-inst-1794207702156240454"},
		"code_challenge":  {"chalRec123+abc"},
		"note":            {"x&y=z 50% off"},
		"dup":             {"a", "b"},
	}
	liveVals := url.Values{
		"client_id":       {"demo-cli"},
		"redirect_uri":    {"https://app.example/cb?state=abc&nonce=222"},
		"installation_id": {"cli-inst-1794207771159240454"},
		"code_challenge":  {"chalLive456+xyz"},
		"note":            {"x&y=w 75% off"},
		"dup":             {"c", "d"},
	}

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"Application/X-WWW-Form-Urlencoded; charset=UTF-8",
	} {
		rec := formReq(ct, recVals)
		live := formReq(ct, liveVals)
		t.Run(ct, func(t *testing.T) {
			for _, tc := range []struct {
				name            string
				rec, live       *models.HTTPReq
				place, recorded string
				value, why      string
			}{
				{name: "whole form field", rec: rec, live: live, place: "body.installation_id", recorded: "cli-inst-1794207702156240454", value: "cli-inst-1794207771159240454"},
				{name: "word of form field", rec: rec, live: live, place: "body.installation_id#2", recorded: "1794207702156240454", value: "1794207771159240454"},
				{name: "URL-decoded redirect_uri with embedded &", rec: rec, live: live, place: "body.redirect_uri", recorded: "https://app.example/cb?state=abc&nonce=111", value: "https://app.example/cb?state=abc&nonce=222"},
				{name: "no phantom key from embedded &", rec: rec, live: live, place: "body.nonce", recorded: "111", why: "its recording does not hold"},
				{name: "literal plus sign preserved", rec: rec, live: live, place: "body.code_challenge", recorded: "chalRec123+abc", value: "chalLive456+xyz"},
				{name: "literal &, =, and percent preserved", rec: rec, live: live, place: "body.note", recorded: "x&y=z 50% off", value: "x&y=w 75% off"},
				{name: "mismatched recorded value", rec: rec, live: live, place: "body.installation_id", recorded: "wrong", why: `its recording does not hold the variable's recorded value "wrong" at body.installation_id`},
				{name: "repeated key refused", rec: rec, live: live, place: "body.dup", recorded: "a", why: "its recording does not hold"},
				{name: "empty live value refused", rec: rec, live: formReq(ct, url.Values{"installation_id": {""}}), place: "body.installation_id", recorded: "cli-inst-1794207702156240454", why: "it holds no value at body.installation_id this run"},
				{name: "missing live key refused", rec: rec, live: formReq(ct, url.Values{"client_id": {"demo-cli"}}), place: "body.installation_id", recorded: "cli-inst-1794207702156240454", why: "it holds no value at body.installation_id this run"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, why := RequestPlace(tc.rec, tc.live, tc.place, tc.recorded)
					if got != tc.value || (tc.why == "") != (why == "") || !strings.Contains(why, tc.why) {
						t.Fatalf("RequestPlace = (%q, %q); want (%q, containing %q)", got, why, tc.value, tc.why)
					}
				})
			}

			// Invalid escape in live form is refused cleanly (never reads raw "a+b%20c").
			badLive := &models.HTTPReq{
				Method: "POST",
				URL:    "http://localhost:8083/oauth/par",
				Header: map[string]string{"Content-Type": ct},
				Body:   "discount=50%&installation_id=cli-inst-999",
			}
			if _, why := RequestPlace(rec, badLive, "body.installation_id", "cli-inst-1794207702156240454"); !strings.Contains(why, "it holds no value at body.installation_id this run") {
				t.Fatalf("invalid raw escape why = %q; want no value this run", why)
			}

			// Regression guard: a JSON body sent under a form Content-Type
			// (e.g. jQuery $.ajax default) must still read JSON leaves.
			jsonRec := &models.HTTPReq{
				Method: "POST",
				URL:    "http://localhost:8083/orders",
				Header: map[string]string{"Content-Type": ct},
				Body:   `{"userId":"u-rec-1","qty":2}`,
			}
			jsonLive := &models.HTTPReq{
				Method: "POST",
				URL:    "http://localhost:8083/orders",
				Header: map[string]string{"Content-Type": ct},
				Body:   `{"userId":"u-live-2","qty":2}`,
			}
			if got, why := RequestPlace(jsonRec, jsonLive, "body.userId", "u-rec-1"); why != "" || got != "u-live-2" {
				t.Fatalf("RequestPlace(JSON under %s) = (%q, %q); want (\"u-live-2\", \"\")", ct, got, why)
			}
		})
	}

	// Non-form Content-Type is never parsed as form.
	plainRec := formReq("text/plain", url.Values{"installation_id": {"cli-inst-1"}})
	plainLive := formReq("text/plain", url.Values{"installation_id": {"cli-inst-2"}})
	if _, why := RequestPlace(plainRec, plainLive, "body.installation_id", "cli-inst-1"); !strings.Contains(why, "its recording does not hold") {
		t.Fatalf("non-form Content-Type why = %q; want refused", why)
	}
}

// TestLearnMatchAndRequestPlaceRoundTripFormURLEncodedClientRequest exercises
// learning a variable from a client test's application/x-www-form-urlencoded
// request and reading it back via RequestPlace on replay.
func TestLearnMatchAndRequestPlaceRoundTripFormURLEncodedClientRequest(t *testing.T) {
	const recInst, fullInst = "cli-inst-1794207702156240454", "cli-inst-1794207771159240454"
	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=utf-8",
	} {
		t.Run(ct, func(t *testing.T) {
			sent := time.Now()
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.SetClientTests(true)
			tr.Begin()
			mkBody := func(inst string) string {
				return url.Values{"client_id": {"demo-cli"}, "installation_id": {inst}, "note": {"x&y=z 50% off"}}.Encode()
			}
			recReq := &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/oauth/par",
				Timestamp: learnAt.Add(-time.Second),
				Header:    map[string]string{"Content-Type": ct},
				Body:      mkBody(recInst),
			}
			tr.SetCases([]learn.ClientCase{{Name: "post-oauth-par-2", Req: recReq}})
			liveReq := *recReq
			liveReq.Body = mkBody(fullInst)
			liveReq.Timestamp = sent
			if !tr.Incoming(&liveReq, "", sent) {
				t.Fatal("incoming form request did not pair")
			}

			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/rate-limit", `{"installation_id":"`+recInst+`"}`, nil, `{"ok":true}`),
			}}
			in := live("POST", "/v1/rate-limit", `{"installation_id":"`+fullInst+`"}`, nil)
			in.arrived = sent.Add(2 * time.Millisecond)

			served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
			if served == nil || miss != nil || len(db.consumed) != 1 {
				t.Fatalf("learnMatch: served=%v miss=%+v; want served", served != nil, miss)
			}
			binds := db.consumed[0].Spec.LearnedBinds()["test case post-oauth-par-2"]
			if len(binds) != 1 {
				t.Fatalf("binds = %+v; want 1 bind at test case post-oauth-par-2", db.consumed[0].Spec.LearnedBinds())
			}
			for _, b := range binds {
				if b.Bind != "body.installation_id" || b.Recorded != recInst {
					t.Fatalf("bind = %+v; want body.installation_id recorded %q", b, recInst)
				}
				got, why := RequestPlace(recReq, &liveReq, b.Bind, b.Recorded)
				if why != "" || got != fullInst {
					t.Fatalf("RequestPlace(%s) = (%q, %q); want (%q, \"\")", b.Bind, got, why, fullInst)
				}
			}
		})
	}
}

func TestReqViewPutBackAndFitVariablesFormURLEncoded(t *testing.T) {
	req := &models.HTTPReq{
		Method: "POST",
		URL:    "http://localhost:8083/oauth/par",
		Header: map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"},
		Body:   url.Values{"client_id": {"demo-cli"}, "installation_id": {"cli-inst-222"}}.Encode(),
	}
	v, ok := recordedView(req)
	if !ok {
		t.Fatal("recordedView failed")
	}
	p, err := variables.Parse("body.installation_id")
	if err != nil {
		t.Fatal(err)
	}
	out, ok := v.putBack(p, held{value: "cli-inst-111"})
	if !ok {
		t.Fatal("putBack whole form field failed")
	}
	if got, ok := out.leaf(p); !ok || got.value != "cli-inst-111" {
		t.Fatalf("leaf after putBack = (%+v, %v); want cli-inst-111", got, ok)
	}
	addr, err := variables.ParseAddress("body.installation_id#2")
	if err != nil {
		t.Fatal(err)
	}
	outW, ok := v.putBackAt(varPlace{raw: "body.installation_id#2", place: addr.Place, at: addr}, held{value: "111"})
	if !ok {
		t.Fatal("putBackAt word in form field failed")
	}
	if got, ok := outW.leaf(p); !ok || got.value != "cli-inst-111" {
		t.Fatalf("leaf after putBackAt = (%+v, %v); want cli-inst-111", got, ok)
	}

	// Outgoing HTTP mock with raw form-urlencoded body using fitVariables.
	h, _, _ := learnOn(t)
	formCT := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	m := learnRecorded("mock-form-var", 0, "POST", "/oauth/token",
		url.Values{"grant_type": {"authorization_code"}, "code": {"code+rec1&a=b"}}.Encode(), formCT, `{"ok":true}`)
	m.Spec.Variables = models.MockVariables{
		"code": {Recorded: "code+rec1&a=b", Bind: "body.code"},
	}
	in := live("POST", "/oauth/token",
		url.Values{"grant_type": {"authorization_code"}, "code": {"code+live2&a=b"}}.Encode(), formCT)
	tbl := &variables.Table{}
	hit, prob := h.fitVariables(context.Background(), in, liveView(in), m, tbl, declaredRules{}, nil, nil, nil)
	if prob != nil || hit == nil || len(hit.binds) != 1 || hit.binds[0].Value != "code+live2&a=b" {
		t.Fatalf("fitVariables on form mock: hit=%+v prob=%+v; want bind code=code+live2&a=b", hit, prob)
	}

	// Regression guard: JSON body sent under a form Content-Type still works
	// through fitVariables (whole bind, word putBack, and sibling JSON noise
	// bypassing the byte-equal shortcut) for both CT spellings.
	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=UTF-8",
	} {
		t.Run("fitVariables JSON body under "+ct, func(t *testing.T) {
			hdr := map[string]string{"Content-Type": ct}
			jm := learnRecorded("mock-json-under-form", 0, "POST", "/orders", `{"userId":"usr-rec-1","traceId":"t-rec","qty":2}`, hdr, `{"ok":true}`)
			jm.Spec.Variables = models.MockVariables{
				"uid": {Recorded: "rec", Bind: "body.userId#1"},
			}
			jin := live("POST", "/orders", `{"userId":"usr-live-1","traceId":"t-live","qty":2}`, hdr)
			jtbl := &variables.Table{}
			jhit, jprob := h.fitVariables(context.Background(), jin, liveView(jin), jm, jtbl, declaredRules{}, nil, map[string][]string{"traceId": nil}, nil)
			if jprob != nil || jhit == nil || len(jhit.binds) != 1 || jhit.binds[0].Value != "live" {
				t.Fatalf("fitVariables(JSON under %s): hit=%+v prob=%+v; want bind uid=live", ct, jhit, jprob)
			}
		})
	}
}
