package templates

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// A place's leading words, its frame's number and its placeholders are
// keploy's or the test case's own, so they stay whole; every key of its path
// that may be a value the app sent is {}, a key that holds a space included:
// the place was shaped after its last space, which kept "body.users.Ada "
// whole, and wrote a frame's number {}. A key with a dot in it is shaped part
// by part ("com" of an e-mail address is kept), as models.FieldShape states:
// a place does not escape its keys' dots.
func TestPlaceShape(t *testing.T) {
	for _, c := range []struct{ where, want string }{
		{"body.users.Ada Lovelace.id", "body.users.{}.id"},
		{"body.users.Ada Q Lovelace.id", "body.users.{}.id"},
		{"body.users.alice@example.com.token", "body.users.{}.com.token"},
		{"response body.ada@example", "response body.{}"},
		{"body", "body"},
		{"body[4]", "body[4]"},
		{"body.items[0].id", "body.items[0].id"},
		{"body key roles.u_LIVE2", "body key roles.{}"},
		{"body key roles.{{string .id}}", "body key roles.{{string .id}}"},
		{"body key roles.{{string .id}}.Ada Lovelace", "body key roles.{{string .id}}.{}"},
		{"header Set-Cookie.sid", "header Set-Cookie.sid"},
		{"header Authorization", "header Authorization"},
		{"body frame 3", "body frame 3"},
		{"body frame 31 data", "body frame 31 data"},
		{"body frame 3 data.items[0].u_7Hq2Lw9Zx", "body frame 3 data.items[0].{}"},
		{"body frame 0 key u_7Hq2Lw9Zx", "body frame 0 key {}"},
		{"response", "response"},
		// Every lead keploy's checker writes is kept: the request's URL, its
		// params, headers, names, form and body, and gRPC's pseudo headers
		// and trailers. "request url" was {} (a space is in no name).
		{"request url", "request url"},
		{"request url_params.token", "request url_params.token"},
		{"request url_params.u_7Hq2Lw9Zx", "request url_params.{}"},
		{"request header name X-Api-Key", "request header name X-Api-Key"},
		{"request header X-Request-Id", "request header X-Request-Id"},
		{"request form field name email", "request form field name email"},
		{"request form u_7Hq2Lw9Zx", "request form {}"},
		{"request body", "request body"},
		{"request body.users.Ada Lovelace", "request body.users.{}"},
		{"request pseudo header :authority", "request pseudo header :authority"},
		{"response pseudo header :status", "response pseudo header :status"},
		{"response trailer grpc-status", "response trailer grpc-status"},
		{"response header name Set-Cookie", "response header name Set-Cookie"},
		{"response body key orders.u_7Hq2Lw9Zx", "response body key orders.{}"},
	} {
		if got := PlaceShape(c.where); got != c.want {
			t.Errorf("PlaceShape(%q) = %q, want %q", c.where, got, c.want)
		}
	}
}

// The next steps a log line says name every place by its shape and never a
// recorded value: the failing place, the place that captured the key and the
// producer's, and the uncaptured key's advice. They named a place in another
// test case raw and quoted config.yaml's recorded value.
func TestNextStepsLogNoKeyValueAndNoRecordedValue(t *testing.T) {
	const name, recorded = "Ada Lovelace", "u_REC7Hq2Lw9Zx"
	create := models.TestCase{Name: "create", Kind: models.HTTP, HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"users":{"` + name + `":{"id":"{{capture string .id}}"}}}`}}
	stats := models.TestCase{Name: "stats", Kind: models.HTTP, HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"users":{"` + name + `":{"last":"{{string .id}}"}}}`}}
	read := models.TestCase{Name: "read", Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: "http://h/u/{{string .uid}}"}}
	r := newRun(t, map[string]interface{}{"id": recorded, "uid": recorded})
	res := r.Check([]*models.TestCase{&create, &stats, &read}, CheckOptions{})
	steps := []string{res.NextStep("stats", Failure{Where: "body.users." + name + ".last", Placeholder: "{{string .id}}"}, true)}
	for _, u := range res.Uncaptured {
		steps = append(steps, u.UncapturedNextStep())
	}
	if len(steps) < 2 {
		t.Fatalf("no uncaptured key: %+v", res.Uncaptured)
	}
	got := strings.Join(steps, "\n")
	for _, want := range []string{"create/response body.users.{}.id captured", "stats/response body.users.{}.last", "the value config.yaml's template holds for uid"} {
		if !strings.Contains(got, want) {
			t.Errorf("the next steps do not say %q: %s", want, got)
		}
	}
	for what, v := range map[string]string{"the key that names a user": name, "the recorded value": recorded} {
		if found := logleak.Find(t, got, []byte(v)); len(found) > 0 {
			t.Errorf("a next step holds %s: %v\n%s", what, found, got)
		}
	}
}
