package http

import (
	"encoding/base64"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A client test case's request is read at a variable's bind place as a
// call's request is: the recorded request must hold the variable's recorded
// value there (a number by its value), and the live one a value that is not
// empty; else why it gives none.
func TestAClientTestCasesRequestIsReadAtItsVariablesPlace(t *testing.T) {
	rec := &models.HTTPReq{Method: "POST", URL: "http://api/apps?team=t-1", Header: map[string]string{"X-Name": "app-1"}, Body: `{"name":"app-1","n":1.50}`}
	live := func(url, header, body string) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: url, Header: map[string]string{"x-name": header}, Body: body}
	}
	sent := live("http://api/apps?team=t-2", "app-2", `{"name":"app-2","n":2}`)
	// api-server's TestSlowStepFailsOnlyItself: an agent's response for a
	// job, its response_time a JSON number (test case
	// post-atg-response-by-id-by-id-1), as recorded and as a run sends it.
	slowURL := "http://localhost:8083/atg/response/b3c4cfaa-7655-400b-b5de-4e6e97abf480/a20b6921-24c6-4a03-bcf9-b409842a88d0"
	slowBody := func(date, responseTime string) string {
		return `{"req_id":"a20b6921-24c6-4a03-bcf9-b409842a88d0","status":200,"headers":{"Content-Length":"2","Content-Type":"application/json","Date":"` + date + `"},"body":"[]","response_time":` + responseTime + `}`
	}
	slowRec := &models.HTTPReq{Method: "POST", URL: slowURL, Header: map[string]string{"Content-Type": "application/json", "Content-Length": "205"}, Body: slowBody("Tue, 10 Nov 2026 18:22:03 GMT", "42597640")}
	slowLive := &models.HTTPReq{Method: "POST", URL: slowURL, Header: map[string]string{"Content-Type": "application/json", "Content-Length": "205"}, Body: slowBody("Tue, 10 Nov 2026 18:29:52 GMT", "51234567")}
	// api-server's TestReplayVerdictReachesAnAppAdminWithoutClusterView: a
	// cluster's proxy sends the token the application minted at its login,
	// the cluster's id (made anew each run) in its cluster_id claim; the
	// learn step binds the id at the claim (test case post-cluster-status-3).
	enc := base64.RawURLEncoding
	bearer := func(payload, sig string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString([]byte(sig))}
	}
	beat := func(header map[string]string) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/cluster/status", Header: header, Body: `{"status":"active"}`}
	}
	beatRec := beat(bearer(`{"cid":"e2e-free-company","cluster_id":"6af47add8a9351dfdb7c457a","iat":1794407071}`, "recorded-signature"))
	beatLive := beat(bearer(`{"cid":"e2e-free-company","cluster_id":"6af47b1e0c22d4e8a91b03f6","iat":1794407138}`, "this-run-signature"))
	for _, tc := range []struct {
		name            string
		rec, live       *models.HTTPReq
		place, recorded string
		value, why      string
	}{
		{name: "body leaf", rec: rec, live: sent, place: "body.name", recorded: "app-1", value: "app-2"},
		{name: "header, any case", rec: rec, live: sent, place: "header.X-Name", recorded: "app-1", value: "app-2"},
		{name: "query", rec: rec, live: sent, place: "url.query.team", recorded: "t-1", value: "t-2"},
		{name: "a number by its value", rec: rec, live: sent, place: "body.n", recorded: "1.5", value: "2"},
		{name: "no copy", rec: nil, live: sent, place: "body.name", recorded: "app-1", why: "keploy holds no copy of it"},
		{name: "no live copy", rec: rec, live: nil, place: "body.name", recorded: "app-1", why: "keploy holds no copy of it"},
		{name: "a place that does not read", rec: rec, live: sent, place: "nowhere.name", recorded: "app-1", why: "its place nowhere.name does not read"},
		{name: "a URL that does not parse", rec: rec, live: live("http://api/%zz", "app-2", `{"name":"app-2"}`), place: "body.name", recorded: "app-1", why: "its URL does not parse"},
		{name: "another recorded value", rec: rec, live: sent, place: "body.name", recorded: "app-9", why: `its recording does not hold the variable's recorded value "app-9" at body.name`},
		{name: "recorded nowhere, empty", rec: rec, live: sent, place: "body.gone", recorded: "", why: "its recording does not hold"},
		{name: "a recorded URL that does not parse", rec: &models.HTTPReq{Method: "POST", URL: "http://api/%zz", Body: `{"name":"app-1"}`}, live: sent, place: "body.name", recorded: "app-1", why: "its URL does not parse"},
		{name: "not sent", rec: rec, live: live("http://api/apps", "", `{}`), place: "body.name", recorded: "app-1", why: "it holds no value at body.name this run"},
		{name: "sent empty", rec: rec, live: live("http://api/apps", "", `{"name":""}`), place: "body.name", recorded: "app-1", why: "it holds no value at body.name this run"},
		{name: "a word of a string", rec: rec, live: sent, place: "body.name#1", recorded: "1", value: "2"},
		{name: "a word past the recorded text's words", rec: rec, live: sent, place: "body.name#2", recorded: "1", why: `its recording does not hold the variable's recorded value "1" at body.name#2`},
		{name: "a word past the live text's words", rec: rec, live: live("http://api/apps", "", `{"name":"app"}`), place: "body.name#1", recorded: "1", why: "it holds no value at body.name#1 this run"},
		// A number is read whole, as the learn step binds one (a word of
		// it is no place: the replay reads a word of a string only).
		{name: "a client test's number, whole", rec: slowRec, live: slowLive, place: "body.response_time", recorded: "42597640", value: "51234567"},
		{name: "a word of a number", rec: slowRec, live: slowLive, place: "body.response_time#0", recorded: "42597640", why: `its recording does not hold the variable's recorded value "42597640" at body.response_time#0`},
		{name: "a JWT claim's word", rec: beatRec, live: beatLive, place: "header.Authorization|jwt.cluster_id#0", recorded: "6af47add8a9351dfdb7c457a", value: "6af47b1e0c22d4e8a91b03f6"},
		{name: "a JWT claim recorded with another value", rec: beatRec, live: beatLive, place: "header.Authorization|jwt.cluster_id#0", recorded: "6af47b1e0c22d4e8a91b03f6", why: `its recording does not hold the variable's recorded value "6af47b1e0c22d4e8a91b03f6" at header.Authorization|jwt.cluster_id#0`},
		{name: "a JWT claim the live token does not hold", rec: beatRec, live: beat(bearer(`{"cid":"e2e-free-company","iat":1794407138}`, "this-run-signature")), place: "header.Authorization|jwt.cluster_id#0", recorded: "6af47add8a9351dfdb7c457a", why: "it holds no value at header.Authorization|jwt.cluster_id#0 this run"},
		{name: "a live header that is no JWT", rec: beatRec, live: beat(map[string]string{"Authorization": "Bearer opaque-token"}), place: "header.Authorization|jwt.cluster_id#0", recorded: "6af47add8a9351dfdb7c457a", why: "it holds no value at header.Authorization|jwt.cluster_id#0 this run"},
		{name: "a number the live request does not send", rec: slowRec, live: live(slowRec.URL, "", `{"req_id":"a20b6921-24c6-4a03-bcf9-b409842a88d0","status":200}`), place: "body.response_time", recorded: "42597640", why: "it holds no value at body.response_time this run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, why := RequestPlace(tc.rec, tc.live, tc.place, tc.recorded)
			if value != tc.value || (tc.why == "") != (why == "") || !strings.Contains(why, tc.why) {
				t.Fatalf("read %q (%q); want %q (%q)", value, why, tc.value, tc.why)
			}
		})
	}
}
