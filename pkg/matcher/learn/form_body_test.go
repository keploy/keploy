package learn

import (
	"maps"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A URL-encoded form is read by its declared media type: its parameters
// after it, in any case, but not one whose parameters do not parse.
func TestAFormIsReadByItsDeclaredMediaType(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/x-www-form-urlencoded":                true,
		"Application/X-WWW-Form-Urlencoded; charset=utf-8": true,
		"application/x-www-form-urlencoded; =utf-8":        false,
		"application/json":                                 false,
		"multipart/form-data; boundary=x":                  false,
		"":                                                 false,
	} {
		if got := FormMedia(ct); got != want {
			t.Errorf("FormMedia(%q) = %v; want %v", ct, got, want)
		}
	}
}

// A form keploy keeps decoded is split at each "&" and the first "=" of each
// field; an empty field is none, a field sent more than once has no one
// value and is left out, and one with no "=" holds "".
func TestAFormsFieldsAreTheOnesItSendsOnce(t *testing.T) {
	for body, want := range map[string]map[string]string{
		"a=1&b=x=y":         {"a": "1", "b": "x=y"},
		"a=1&&b=2&":         {"a": "1", "b": "2"},
		"a=1&a=2&b=3":       {"b": "3"},
		"a=1&a=2&a=3":       {},
		"flag&c=https://x/": {"flag": "", "c": "https://x/"},
		"":                  {},
	} {
		if got := FormFields(body); !maps.Equal(got, want) {
			t.Errorf("FormFields(%q) = %v; want %v", body, got, want)
		}
	}
}

// A test case's URL-encoded form body is read as its fields, "body.<name>",
// ahead of the body whole, as a JSON body's leaves are: each as recorded and
// as sent, a field the request as sent does not declare a form "" there, and
// a name with a "." or a bracket Unnamed.
func TestATestCasesFormIsReadAsItsFields(t *testing.T) {
	req := func(sentCT string) *models.TestRequest {
		return &models.TestRequest{Test: "post-oauth-token-8", Parts: []models.TestRequestPart{
			{Name: "header.Content-Type", Recorded: "application/x-www-form-urlencoded", Sent: sentCT},
			{Name: "body", Recorded: "a.b=1&code=r1&grant_type=authorization_code", Sent: "a.b=2&code=f1&grant_type=authorization_code"},
		}}
	}
	type seen struct {
		rec, live string
		unnamed   bool
	}
	read := func(e Earlier) map[string]seen {
		out := map[string]seen{}
		for _, s := range e.Seen {
			out[s.Leaf.Path] = seen{s.Leaf.Value, s.Live, s.Leaf.Unnamed}
		}
		return out
	}
	got := read(GivenRequest(req("application/x-www-form-urlencoded; charset=utf-8")))
	want := map[string]seen{
		"body.a.b": {"1", "2", true}, "body.code": {"r1", "f1", false}, "body.grant_type": {"authorization_code", "authorization_code", false},
		"body": {"a.b=1&code=r1&grant_type=authorization_code", "a.b=2&code=f1&grant_type=authorization_code", false},
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("a form sent as a form: %s = %+v; want %+v", p, got[p], w)
		}
	}
	got = read(GivenRequest(req("text/plain")))
	if got["body.code"] != (seen{"r1", "", false}) || got["body"].live == "" {
		t.Errorf("a form sent as text: body.code = %+v, body = %+v; want the field recorded only, the body whole as sent", got["body.code"], got["body"])
	}
	// A JSON body declared JSON is read as JSON, never as a form.
	js := GivenRequest(&models.TestRequest{Test: "post-oauth-register-4", Parts: []models.TestRequestPart{
		{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/json"},
		{Name: "body", Recorded: `{"a":"x=1&b"}`, Sent: `{"a":"x=2&b"}`},
	}})
	if got := read(js); got["body.a"] != (seen{"x=1&b", "x=2&b", false}) || len(got) != 3 {
		t.Errorf("a JSON body: %+v; want body.a, its keys and the body whole", got)
	}
}
