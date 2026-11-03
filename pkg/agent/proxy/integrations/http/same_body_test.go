package http

import (
	"bytes"
	"mime/multipart"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestSameRecordedBody holds that SameRecordedBody reads two bodies as the
// strict comparison does: by the recording's Content-Type, JSON as a
// decoded document, a form by its parsed values, a multipart form part by
// part, anything else byte for byte, with learned and redacted places free.
func TestSameRecordedBody(t *testing.T) {
	const jsonCT, formCT = "application/json", "application/x-www-form-urlencoded"
	rec := func(ct, body string, noise ...string) *models.Mock {
		return &models.Mock{Kind: models.HTTP, Noise: noise, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{
			Method: "POST", URL: "http://pay.test/v1/charges", Header: map[string]string{"Content-Type": ct}, Body: body,
		}}}
	}
	mp := func(boundary string, fields ...string) ([]byte, string) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		if err := w.SetBoundary(boundary); err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(fields); i += 2 {
			if err := w.WriteField(fields[i], fields[i+1]); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes(), w.FormDataContentType()
	}
	recMP, recMPCT := mp("recordedboundary", "amount", "100", "currency", "usd")
	liveMP, liveMPCT := mp("liveboundary0000", "amount", "100", "currency", "usd")
	otherMP, _ := mp("liveboundary0000", "amount", "200", "currency", "usd")

	rows := []struct {
		name    string
		rec     *models.Mock
		learned map[string][]string
		body    string
		ct      string
		want    bool
	}{
		{"json, a space after the colon", rec(jsonCT, `{"amount":100}`), nil, `{"amount": 100}`, jsonCT, true},
		{"json, pretty-printed with its keys in another order", rec(jsonCT, `{"amount":100,"currency":"usd"}`), nil, "{\n  \"currency\": \"usd\",\n  \"amount\": 100\n}", jsonCT, true},
		{"json, 1.50 is 1.5", rec(jsonCT, `{"amount":1.50}`), nil, `{"amount":1.5}`, jsonCT, true},
		{"json, another value", rec(jsonCT, `{"amount":100}`), nil, `{"amount": 200}`, jsonCT, false},
		{"json, another key", rec(jsonCT, `{"amount":100}`), nil, `{"amount":100,"capture":true}`, jsonCT, false},
		{"json, an array in another order", rec(jsonCT, `{"ids":[1,2]}`), nil, `{"ids":[2,1]}`, jsonCT, false},
		{"json, a learned field's value differs", rec(jsonCT, `{"amount":100,"ts":1}`), map[string][]string{"body.ts": {}}, `{"amount":100, "ts":2}`, jsonCT, true},
		{"json, a learned field's value differs, unlearned", rec(jsonCT, `{"amount":100,"ts":1}`), nil, `{"amount":100,"ts":2}`, jsonCT, false},
		{"json, every element's learned field", rec(jsonCT, `{"items":[{"id":1},{"id":2}]}`), map[string][]string{"body.items[].id": {}}, `{"items":[{"id":7},{"id":8}]}`, jsonCT, true},
		{"json, an unlearned field next to a learned one", rec(jsonCT, `{"amount":100,"ts":1}`), map[string][]string{"body.ts": {}}, `{"amount":200,"ts":2}`, jsonCT, false},
		{"json, a redacted value", rec(jsonCT, `{"card":"tok_REDACTED"}`, `tok_REDACTED`), nil, `{"card":"4242"}`, jsonCT, true},
		{"form, its keys in another order", rec(formCT, `a=1&b=2`), nil, `b=2&a=1`, formCT, true},
		{"form, a repeated key's values in another order", rec(formCT, `a=1&a=2`), nil, `a=2&a=1`, formCT, true},
		{"form, another value", rec(formCT, `a=1&b=2`), nil, `b=3&a=1`, formCT, false},
		{"form read by the recording's type, the live one unsaid", rec(formCT, `a=1&b=2`), nil, `b=2&a=1`, "", true},
		{"text, the same bytes", rec("text/plain", `hello`), nil, `hello`, "text/plain", true},
		{"text, one more space", rec("text/plain", `hello`), nil, `hello `, "text/plain", false},
		{"multipart, another boundary", rec(recMPCT, string(recMP)), nil, string(liveMP), liveMPCT, true},
		{"multipart, another value", rec(recMPCT, string(recMP)), nil, string(otherMP), liveMPCT, false},
		{"multipart, its live boundary unsaid", rec(recMPCT, string(recMP)), nil, string(liveMP), "", false},
		{"no mock", nil, nil, `{}`, jsonCT, false},
		{"no request", &models.Mock{Kind: models.HTTP}, nil, `{}`, jsonCT, false},
	}
	for _, r := range rows {
		if got := SameRecordedBody(r.rec, r.learned, []byte(r.body), r.ct); got != r.want {
			t.Errorf("%s: SameRecordedBody = %v, want %v", r.name, got, r.want)
		}
	}
}
