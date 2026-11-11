package http

import (
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, why := RequestPlace(tc.rec, tc.live, tc.place, tc.recorded)
			if value != tc.value || (tc.why == "") != (why == "") || !strings.Contains(why, tc.why) {
				t.Fatalf("read %q (%q); want %q (%q)", value, why, tc.value, tc.why)
			}
		})
	}
}
