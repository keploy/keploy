package templates

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

func newRun(t *testing.T, template map[string]interface{}) *Run {
	t.Helper()
	return NewRun(nil, template, nil)
}

// Rule 2 in a request: each placeholder sends this run's value, escaped for
// where it sits.
func TestRequest_SendsTheRunsValueEscapedForWhereItSits(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr 1/a", "token": "T1", "orderId": "7654321", "q": "a&b=c", "sid": "S1"})
	req := r.Request(models.HTTPReq{
		URL:       "http://localhost:8080/users/{{string .id}}/orders/%7B%7Bstring%20.orderId%7D%7D?filter={{string .q}}&x=1#{{string .id}}",
		URLParams: map[string]string{"filter": "{{string .q}}", "x": "1"},
		Header: map[string]string{
			"Authorization": "Bearer {{string .token}}",
			"Cookie":        "theme=dark; sid={{string .sid}}",
			"Content-Type":  "application/json",
		},
		Body: `{"order_id":"{{int .orderId}}","note":"for {{string .id}}","id":"{{string .id}}"}`,
		Form: []models.FormData{{Key: "order", Values: []string{"{{string .orderId}}"}}},
	})
	assert.Equal(t, "http://localhost:8080/users/usr%201%2Fa/orders/7654321?filter=a%26b%3Dc&x=1#usr 1/a", req.URL)
	assert.Equal(t, map[string]string{"filter": "a&b=c", "x": "1"}, req.URLParams)
	assert.Equal(t, "Bearer T1", req.Header["Authorization"])
	assert.Equal(t, "theme=dark; sid=S1", req.Header["Cookie"])
	assert.Equal(t, `{"order_id":7654321,"note":"for usr 1/a","id":"usr 1/a"}`, req.Body)
	assert.Equal(t, []string{"7654321"}, req.Form[0].Values)
}

// NUMBERS OF ONE MILLION AND UP (the bug: a learned 7654321 was stored as
// "7.654321e+06", so {{int .k}} sent 0 and a path the exponent). A value is
// its exact text, so every place sends the digits.
func TestRequest_ALargeNumberIsSentAsItsDigits(t *testing.T) {
	for _, live := range []string{"7654321", "1000000", "12345678901234567890"} {
		r := newRun(t, map[string]interface{}{"orderId": "1"})
		r.Expected(models.HTTPResp{Body: `{"order_id":"{{capture int .orderId}}"}`}, models.HTTPResp{Body: `{"order_id":` + live + `}`}, ExpectOptions{})
		req := r.Request(models.HTTPReq{
			URL:    "http://h/orders/{{string .orderId}}",
			Header: map[string]string{"Content-Type": "application/json"},
			Body:   `{"order_id":"{{int .orderId}}","bare":{{int .orderId}}}`,
		})
		assert.Equal(t, "http://h/orders/"+live, req.URL)
		assert.Equal(t, `{"order_id":`+live+`,"bare":`+live+`}`, req.Body)
	}
}

// ESCAPED FOR THE BODY'S OWN JSON (the bug: values were escaped once, for the
// marshalled test case, and RemoveDoubleQuotes stripped every " to hide what
// that broke). A quote, a backslash and a newline all arrive as written.
func TestRequest_ATemplateValueKeepsAJSONBodyValid(t *testing.T) {
	for _, value := range []string{`say "hi"`, `C:\tmp\new`, "line1\nline2", `<a&b>`, `W/"x1-abc"`} {
		r := newRun(t, map[string]interface{}{"q": value})
		req := r.Request(models.HTTPReq{Body: `{"q":"{{string .q}}","in":"x {{string .q}} y"}`})
		var got struct{ Q, In string }
		require.NoError(t, json.Unmarshal([]byte(req.Body), &got), "the app was sent invalid JSON: %s", req.Body)
		assert.Equal(t, value, got.Q)
		assert.Equal(t, "x "+value+" y", got.In)
	}
}

// A BODY THAT IS ONE PLACEHOLDER ALONE is text: it is sent as the value
// itself, never as a JSON string in quotes, as main's renderer sent it; and
// an expected body or a stream frame's field written so reads the same way.
func TestRequest_ABodyThatIsOnePlaceholderIsTheValueAsWritten(t *testing.T) {
	r := newRun(t, map[string]interface{}{"tok": "tok_LIVE1", "n": "42"})
	assert.Equal(t, "tok_LIVE1", r.Request(models.HTTPReq{Header: map[string]string{"Content-Type": "text/plain"}, Body: "{{string .tok}}"}).Body)
	assert.Equal(t, "42", r.Request(models.HTTPReq{Body: "{{int .n}}"}).Body)
	exp := models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "text/plain"}, Body: "{{string .tok}}"}
	res := r.Expected(exp, models.HTTPResp{StatusCode: 200, Body: "tok_LIVE1"}, ExpectOptions{})
	assert.Empty(t, res.Failures)
	assert.Equal(t, "tok_LIVE1", res.Resp.Body)
	assert.NotEmpty(t, r.Expected(exp, models.HTTPResp{StatusCode: 200, Body: "tok_OTHER"}, ExpectOptions{}).Failures)
}

func TestRequest_AFormBodyIsQueryEscaped(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "a b&c"})
	req := r.Request(models.HTTPReq{
		Header: map[string]string{"content-type": "application/x-www-form-urlencoded"},
		Body:   "order={{string .id}}&x=1",
	})
	assert.Equal(t, "order=a+b%26c&x=1", req.Body)
}

// A SECRET IS SENT AS RECORDED. keploy sanitize writes {{string .secret.K}}
// over the whole scalar that held a secret and keeps that scalar as recorded
// in secret.yaml, in the syntax of its place: a URL with its query escaped, a
// form body. It is spliced verbatim there, as main sent it: escaping it again
// sent http:%2F%2F… and token%3D…%26user%3D… (a set sanitize wrote broke on
// upgrade). Inside a JSON string, where sanitize stores the decoded value, it
// is JSON-escaped like any value; beside a variable in one URL or form body,
// only the variable is escaped.
func TestRequest_ASecretIsSentAsRecorded(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"id": "a b"}, map[string]interface{}{
		"Url":   "http://localhost:8080/v1/charges?api_key=sk_live_abc%2Bdef%2Fxyz%3D&amount=5",
		"Tok":   "x%2By",
		"Body":  "token=xoxb-1%2B2&user=alice",
		"Quote": `p"w\d`,
	})
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	assert.Equal(t, "http://localhost:8080/v1/charges?api_key=sk_live_abc%2Bdef%2Fxyz%3D&amount=5",
		r.Request(models.HTTPReq{URL: "{{string .secret.Url }}"}).URL, "a whole URL sanitize redacted is sent as recorded")
	assert.Equal(t, "http://h/u/a%20b?tok=x%2By&id=a+b",
		r.Request(models.HTTPReq{URL: "http://h/u/{{string .id}}?tok={{string .secret.Tok}}&id={{string .id}}"}).URL,
		"a secret in a query is never escaped again; a variable beside it is")
	assert.Equal(t, "http://h/x%2By", r.Request(models.HTTPReq{URL: "http://h/{{string .secret.Tok}}"}).URL, "nor in a path")
	assert.Equal(t, "token=xoxb-1%2B2&user=alice", r.Request(models.HTTPReq{Header: form, Body: "{{string .secret.Body }}"}).Body, "a form body sanitize redacted is sent as recorded")
	assert.Equal(t, "t=x%2By&id=a+b", r.Request(models.HTTPReq{Header: form, Body: "t={{string .secret.Tok}}&id={{string .id}}"}).Body)
	assert.Equal(t, `{"pw":"p\"w\\d"}`, r.Request(models.HTTPReq{Header: map[string]string{"Content-Type": "application/json"}, Body: `{"pw":"{{string .secret.Quote}}"}`}).Body, "inside a JSON string a secret is JSON-escaped")
}

// Rule 6: a key config.yaml does not define is literal text.
func TestRequest_AnUndefinedKeyStaysLiteral(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "1"})
	req := r.Request(models.HTTPReq{URL: "http://h/{{string .idd}}", Body: `{"tpl":"Hello {{ .Subscriber.Name }}"}`})
	assert.Equal(t, "http://h/{{string .idd}}", req.URL)
	assert.Equal(t, `{"tpl":"Hello {{ .Subscriber.Name }}"}`, req.Body)
}

// A value that is not a number of its placeholder's type is not sent as 0:
// the placeholder stays as written, and the app sees what went wrong.
func TestRequest_ANonNumberForAnIntPlaceholderStaysAsWritten(t *testing.T) {
	r := newRun(t, map[string]interface{}{"n": "abc"})
	req := r.Request(models.HTTPReq{Body: `{"n":"{{int .n}}"}`})
	assert.Equal(t, `{"n":"{{int .n}}"}`, req.Body)
}

// What makes a re-send safe: the request is a copy. The test case is left
// as written, and a set with no templates is sent byte for byte.
func TestRequest_IsACopyAndLeavesTheTestCaseAsWritten(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "LIVE"})
	orig := models.HTTPReq{
		URL:    "http://h/{{string .id}}",
		Header: map[string]string{"X-Id": "{{string .id}}"},
		Form:   []models.FormData{{Key: "k", Values: []string{"{{string .id}}"}}},
	}
	got := r.Request(orig)
	got.Header["X-Id"] = "changed by whoever sends it"
	got.Form[0].Values[0] = "changed"
	assert.Equal(t, "{{string .id}}", orig.Header["X-Id"])
	assert.Equal(t, "{{string .id}}", orig.Form[0].Values[0])
	assert.Equal(t, "http://h/{{string .id}}", orig.URL)

	empty := newRun(t, nil)
	asWritten := models.HTTPReq{URL: "http://h/a%7Bb?c=%7B%22d%22%7D", Body: `{"x":"{{string .id}}","n":1.0}`, Header: map[string]string{"A": "{{.x}}"}}
	assert.Equal(t, asWritten, empty.Request(asWritten))
}

func TestGRPC_UsePlaceholdersRenderOnACopy(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "7"})
	req := models.GrpcReq{
		Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/svc/Get"}, OrdinaryHeaders: map[string]string{"x-id": "{{string .id}}"}},
		Body:    models.GrpcLengthPrefixedMessage{DecodedData: `1: {{int .id}}`},
	}
	got := r.GRPCRequest(req)
	assert.Equal(t, "7", got.Headers.OrdinaryHeaders["x-id"])
	assert.Equal(t, "1: 7", got.Body.DecodedData)
	assert.Equal(t, "{{string .id}}", req.Headers.OrdinaryHeaders["x-id"])
	exp := r.GRPCExpected(models.GrpcResp{Body: models.GrpcLengthPrefixedMessage{DecodedData: `1: "{{string .id}}"`}})
	assert.Equal(t, `1: "7"`, exp.Body.DecodedData)
}

// A SECRET IN A NAME (a header's, a form field's, a gRPC trailer's) is the
// text the recording held there, rendered as main rendered every name. A
// variable in a name is refused when the set loads (Check), so it never
// runs; here it stays as written.
func TestRequest_ASecretInANameIsTheRecordedText(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"id": "7"}, map[string]interface{}{"K": "Api-Key"})
	req := r.Request(models.HTTPReq{
		Header: map[string]string{"X-{{string .secret.K}}": "1", "X-{{string .id}}": "2"},
		Form:   []models.FormData{{Key: "{{string .secret.K}}", Values: []string{"v"}}},
	})
	assert.Equal(t, map[string]string{"X-Api-Key": "1", "X-{{string .id}}": "2"}, req.Header)
	assert.Equal(t, "Api-Key", req.Form[0].Key)

	exp := r.GRPCExpected(models.GrpcResp{Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-{{string .secret.K}}": "{{string .secret.K}}"}}})
	assert.Equal(t, map[string]string{"x-Api-Key": "Api-Key"}, exp.Trailers.OrdinaryHeaders)
	res := r.Expected(models.HTTPResp{Header: map[string]string{"X-{{string .secret.K}}": "1"}}, models.HTTPResp{Header: map[string]string{"X-Api-Key": "1"}}, ExpectOptions{})
	assert.Equal(t, map[string]string{"X-Api-Key": "1"}, res.Resp.Header)
}
