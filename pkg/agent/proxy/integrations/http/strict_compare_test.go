package http

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// recorded is a recorded POST to api.example, with headers and a body, as the
// recorder writes it.
func recorded(name, path string, header map[string]string, body string) *models.Mock {
	m := httpMock(name, "POST", path)
	h := map[string]string{"Host": "api.example"}
	for k, v := range header {
		h[k] = v
	}
	m.Spec.HTTPReq.Header = h
	m.Spec.HTTPReq.Body = body
	if u, err := url.Parse(path); err == nil && u.RawQuery != "" {
		m.Spec.HTTPReq.URLParams = map[string]string{}
		for k := range u.Query() {
			m.Spec.HTTPReq.URLParams[k] = u.Query().Get(k)
		}
	}
	m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`}
	return m
}

// sent is the live POST the app sends to api.example.
func sent(path string, header map[string]string, body string) *req {
	u, _ := url.Parse(path)
	h := http.Header{}
	h.Set("Host", "api.example")
	for k, v := range header {
		h.Add(k, v)
	}
	return &req{method: "POST", url: u, header: h, body: []byte(body)}
}

// strictMatch runs the matcher under the switch with the decode-time noise:
// the switched static list plus keploy.yml's header and body noise.
func strictMatch(t *testing.T, input *req, headerNoise, bodyNoise map[string][]string, urlNoise []string, mocks ...*models.Mock) (*models.Mock, *matchDiag) {
	t.Helper()
	t.Setenv("KEPLOY_PROVEN", "1")
	hn := models.ProvenFlakyHeaderNoise()
	for k, v := range headerNoise {
		hn[k] = v
	}
	db := &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}
	ok, stub, diag, err := newHTTP().match(context.Background(), input, db, hn, bodyNoise, urlNoise, true, false, false, true, true)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if !ok {
		return nil, diag
	}
	return stub, nil
}

func differs(diag *matchDiag) []string {
	if diag == nil {
		return nil
	}
	return differsOf(diag.differs)
}

func hasField(fields []string, f string) bool {
	for _, x := range fields {
		if x == f {
			return true
		}
	}
	return false
}

// Every header value is compared, outside keploy.yml's header noise and the
// static list; a header sent on one side only is a difference too.
func TestStrictComparesHeaderValues(t *testing.T) {
	rec := recorded("m", "/v1/charge", map[string]string{"Accept": "application/json", "X-Tenant": "acme", "X-Request-Id": "r-1"}, "")
	for _, tc := range []struct {
		name   string
		header map[string]string
		noise  map[string][]string
		served bool
		field  string
	}{
		{"the recording", map[string]string{"Accept": "application/json", "X-Tenant": "acme", "X-Request-Id": "r-9"}, nil, true, ""},
		{"another value", map[string]string{"Accept": "application/json", "X-Tenant": "globex", "X-Request-Id": "r-1"}, nil, false, "header.x-tenant"},
		{"declared noise", map[string]string{"Accept": "application/json", "X-Tenant": "globex", "X-Request-Id": "r-1"}, map[string][]string{"x-tenant": {}}, true, ""},
		{"a header more", map[string]string{"Accept": "application/json", "X-Tenant": "acme", "X-Request-Id": "r-1", "X-Debug": "1"}, nil, false, "header.x-debug"},
		{"a header less", map[string]string{"Accept": "application/json", "X-Request-Id": "r-1"}, nil, false, "header.x-tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, diag := strictMatch(t, sent("/v1/charge", tc.header, ""), tc.noise, nil, nil, rec)
			if (got != nil) != tc.served {
				t.Fatalf("served %v, want %v (differs %v)", got != nil, tc.served, differs(diag))
			}
			if !tc.served && !hasField(differs(diag), tc.field) {
				t.Fatalf("differs %v does not name %s", differs(diag), tc.field)
			}
		})
	}
}

// Under the switch the credentials leave the static list and are compared;
// without it Authorization is noise, as before.
func TestStrictComparesCredentials(t *testing.T) {
	for _, h := range models.CredentialHeaders {
		rec := recorded("m", "/v1/charge", map[string]string{h: "rec-value"}, "")
		if got, diag := strictMatch(t, sent("/v1/charge", map[string]string{h: "other-value"}, ""), nil, nil, nil, rec); got != nil {
			t.Fatalf("%s: another credential was served", h)
		} else if !hasField(differs(diag), "header."+h) {
			t.Fatalf("%s: differs %v", h, differs(diag))
		}
		if got, _ := strictMatch(t, sent("/v1/charge", map[string]string{h: "rec-value"}, ""), nil, nil, nil, rec); got == nil {
			t.Fatalf("%s: the recorded credential was not served", h)
		}
	}
	noise := models.ProvenFlakyHeaderNoise()
	for _, h := range models.CredentialHeaders {
		if _, ok := noise[h]; ok {
			t.Fatalf("%s is still on the switched static list", h)
		}
	}
	if _, ok := noise["x-amz-date"]; !ok {
		t.Fatal("the switched static list lost a signing timestamp")
	}

	// decode.go builds a request's header noise from flakyHeaderNoise.
	for _, h := range models.CredentialHeaders {
		if _, ok := flakyHeaderNoise()[h]; ok {
			t.Fatalf("under the switch decode still treats %s as noise", h)
		}
	}
	t.Setenv("KEPLOY_PROVEN", "")
	for _, h := range models.CredentialHeaders {
		if _, ok := flakyHeaderNoise()[h]; !ok {
			t.Fatalf("without the switch %s left the static list", h)
		}
	}
}

// The User-Agent is compared by its first product token's name: a version
// bump is the same client, another client is not.
func TestStrictComparesTheUserAgentByItsProduct(t *testing.T) {
	rec := recorded("m", "/v1/charge", map[string]string{"User-Agent": "Go-http-client/1.1"}, "")
	for ua, served := range map[string]bool{
		"Go-http-client/1.1":            true,
		"Go-http-client/2.0":            true,
		"Go-http-client":                true,
		"curl/8.4.0":                    false,
		"aws-sdk-go-v2/1.30.0 os/linux": false,
	} {
		if got, _ := strictMatch(t, sent("/v1/charge", map[string]string{"User-Agent": ua}, ""), nil, nil, nil, rec); (got != nil) != served {
			t.Fatalf("User-Agent %q: served %v, want %v", ua, got != nil, served)
		}
	}
	if productName("aws-sdk-go-v2/1.30.0 os/linux lang/go#1.22") != "aws-sdk-go-v2" || productName("  python-requests/2.31 ") != "python-requests" {
		t.Fatal("productName")
	}
}

// Content-Type is compared by its media types: a charset or a boundary is no
// difference, another media type is.
func TestStrictComparesTheContentTypeByMediaType(t *testing.T) {
	rec := recorded("m", "/v1/charge", map[string]string{"Content-Type": "application/json"}, `{"a":1}`)
	if got, _ := strictMatch(t, sent("/v1/charge", map[string]string{"Content-Type": "application/json; charset=utf-8"}, `{"a":1}`), nil, nil, nil, rec); got == nil {
		t.Fatal("a charset was a difference")
	}
	if got, _ := strictMatch(t, sent("/v1/charge", map[string]string{"Content-Type": "text/plain"}, `{"a":1}`), nil, nil, nil, rec); got != nil {
		t.Fatal("another media type was served")
	}
}

// A JSON body is compared field by field and element by element: a value, a
// type, a member more or less, an array in another order are differences; a
// number written another way is not. The miss names the field, without its
// value.
func TestStrictComparesJSONBodies(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/v1/charge", ct, `{"amount":1.50,"currency":"USD","items":[{"id":1},{"id":2}]}`)
	for _, tc := range []struct {
		name, body string
		served     bool
		field      string
	}{
		{"the recording", `{"currency":"USD","amount":1.5,"items":[{"id":1},{"id":2}]}`, true, ""},
		{"another value", `{"amount":2.50,"currency":"USD","items":[{"id":1},{"id":2}]}`, false, "body.amount"},
		{"another type", `{"amount":"1.50","currency":"USD","items":[{"id":1},{"id":2}]}`, false, "body.amount"},
		{"a member more", `{"amount":1.5,"currency":"USD","items":[{"id":1},{"id":2}],"note":"x"}`, false, "body.note"},
		{"a member less", `{"amount":1.5,"items":[{"id":1},{"id":2}]}`, false, "body.currency"},
		{"another order", `{"amount":1.5,"currency":"USD","items":[{"id":2},{"id":1}]}`, false, "body.items[].id"},
		{"an element more", `{"amount":1.5,"currency":"USD","items":[{"id":1},{"id":2},{"id":3}]}`, false, "body.items"},
		{"not JSON", `amount=1.5`, false, "body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, diag := strictMatch(t, sent("/v1/charge", ct, tc.body), nil, nil, nil, rec)
			if (got != nil) != tc.served {
				t.Fatalf("served %v, want %v (differs %v)", got != nil, tc.served, differs(diag))
			}
			if !tc.served && !hasField(differs(diag), tc.field) {
				t.Fatalf("differs %v does not name %s", differs(diag), tc.field)
			}
		})
	}
}

// Known noise: keploy.yml's request-body noise (its type and presence too),
// what the learn step learned (by exact place) or the request-body learner
// learned before it (by its dialect), keeping the type, and a value the
// obfuscator redacted.
func TestStrictExcusesOnlyKnownBodyNoise(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	body := `{"ts":1700000000,"items":[{"id":"a","at":1},{"id":"b","at":2}],"secret":"REDACTED-1"}`
	live := `{"ts":1700000099,"items":[{"id":"a","at":7},{"id":"b","at":2}],"secret":"live-secret"}`

	plain := recorded("plain", "/v1/charge", ct, body)
	if got, _ := strictMatch(t, sent("/v1/charge", ct, live), nil, nil, nil, plain); got != nil {
		t.Fatal("a body that differs outside known noise was served")
	}

	learned := recorded("learned", "/v1/charge", ct, body)
	learned.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}, "body.items[0].at": {}}
	learned.Noise = []string{`^REDACTED-\d+$`}
	if got, diag := strictMatch(t, sent("/v1/charge", ct, live), nil, nil, nil, learned); got == nil {
		t.Fatalf("learned noise and a redacted value were not excused: %v", differs(diag))
	}
	// The learn step named items[0].at, not items[1].at.
	if got, _ := strictMatch(t, sent("/v1/charge", ct, `{"ts":1,"items":[{"id":"a","at":1},{"id":"b","at":9}],"secret":"x"}`), nil, nil, nil, learned); got != nil {
		t.Fatal("a learned place excused another element")
	}
	// A learned field keeps its type.
	if got, _ := strictMatch(t, sent("/v1/charge", ct, `{"ts":"now","items":[{"id":"a","at":1},{"id":"b","at":2}],"secret":"x"}`), nil, nil, nil, learned); got != nil {
		t.Fatal("a learned field changed its type and was served")
	}

	legacy := recorded("legacy", "/v1/charge", ct, body)
	legacy.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}, "body.items[].at": {}, "body.secret": {}}
	if got, diag := strictMatch(t, sent("/v1/charge", ct, live), nil, nil, nil, legacy); got == nil {
		t.Fatalf("noise in the request-body learner's dialect was not excused: %v", differs(diag))
	}

	declared := map[string][]string{"ts": {}, "items[].at": {}, "secret": {}}
	if got, diag := strictMatch(t, sent("/v1/charge", ct, live), nil, declared, nil, plain); got == nil {
		t.Fatalf("keploy.yml's body noise was not excused: %v", differs(diag))
	}
	if got, _ := strictMatch(t, sent("/v1/charge", ct, `{"items":[{"id":"a","at":7},{"id":"b","at":2}],"secret":"x"}`), nil, declared, nil, plain); got == nil {
		t.Fatal("keploy.yml's body noise did not excuse a field the call no longer sends")
	}
}

// A form is compared key by key, its values in any order; a field the mock
// learned may change.
func TestStrictComparesForms(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	rec := recorded("m", "/oauth/token", ct, "grant_type=client_credentials&scope=a&scope=b&nonce=n1")
	if got, _ := strictMatch(t, sent("/oauth/token", ct, "nonce=n1&scope=b&grant_type=client_credentials&scope=a"), nil, nil, nil, rec); got == nil {
		t.Fatal("the same form in another order was not served")
	}
	got, diag := strictMatch(t, sent("/oauth/token", ct, "grant_type=client_credentials&scope=a&scope=b&nonce=n2"), nil, nil, nil, rec)
	if got != nil || !hasField(differs(diag), "body.nonce") {
		t.Fatalf("a changed form field: served %v, differs %v", got != nil, differs(diag))
	}
	rec.Spec.ReqBodyNoise = map[string][]string{"body.nonce": {}}
	if got, _ := strictMatch(t, sent("/oauth/token", ct, "grant_type=client_credentials&scope=a&scope=b&nonce=n2"), nil, nil, nil, rec); got == nil {
		t.Fatal("a learned form field was not excused")
	}
	// No unix-timestamp guess: a form value that changes is compared.
	ts := recorded("ts", "/sts", ct, "Action=AssumeRole&RoleSessionName=s-1700000000")
	if got, _ := strictMatch(t, sent("/sts", ct, "Action=AssumeRole&RoleSessionName=s-1700000999"), nil, nil, nil, ts); got != nil {
		t.Fatal("a form value was relaxed for embedding a timestamp")
	}
}

func multipartBody(t *testing.T, boundary string, fields map[string]string, file string) (string, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.SetBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"name", "kind"} {
		if v, ok := fields[k]; ok {
			_ = w.WriteField(k, v)
		}
	}
	fw, _ := w.CreateFormFile("doc", "a.txt")
	_, _ = fw.Write([]byte(file))
	_ = w.Close()
	return b.String(), w.FormDataContentType()
}

// A multipart form is read by each side's own boundary, so a boundary minted
// per request is no difference, and compared part by part.
func TestStrictComparesMultipartForms(t *testing.T) {
	body, ct := multipartBody(t, "rec-boundary", map[string]string{"name": "pen", "kind": "x"}, "hello")
	rec := recorded("m", "/upload", map[string]string{"Content-Type": ct}, body)
	same, sameCT := multipartBody(t, "live-boundary-123", map[string]string{"name": "pen", "kind": "x"}, "hello")
	if got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": sameCT}, same), nil, nil, nil, rec); got == nil {
		t.Fatalf("the same form under another boundary was not served: %v", differs(diag))
	}
	field, fieldCT := multipartBody(t, "b2", map[string]string{"name": "ink", "kind": "x"}, "hello")
	if got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": fieldCT}, field), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body.name") {
		t.Fatalf("a changed field: served %v, differs %v", got != nil, differs(diag))
	}
	file, fileCT := multipartBody(t, "b3", map[string]string{"name": "pen", "kind": "x"}, "HELLO")
	if got, _ := strictMatch(t, sent("/upload", map[string]string{"Content-Type": fileCT}, file), nil, nil, nil, rec); got != nil {
		t.Fatal("a changed file was served")
	}
}

// Any other body is compared byte for byte, unless the obfuscator redacted
// the whole recorded body.
func TestStrictComparesOpaqueBodies(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/octet-stream"}
	rec := recorded("m", "/blob", ct, "\x01\x02v1")
	if got, _ := strictMatch(t, sent("/blob", ct, "\x01\x02v2"), nil, nil, nil, rec); got != nil {
		t.Fatal("another opaque body was served")
	}
	rec.Noise = []string{`^\x01\x02v\d$`}
	if got, _ := strictMatch(t, sent("/blob", ct, "\x01\x02v2"), nil, nil, nil, rec); got == nil {
		t.Fatal("a redacted body was compared")
	}
}

// The path and the query are compared by value: keploy.yml's url noise and
// what the mock learned may differ; a repeated parameter's values in any
// order.
func TestStrictComparesThePathAndTheQuery(t *testing.T) {
	rec := recorded("m", "/v1/users/42/orders?status=open&tag=a&tag=b", nil, "")
	rec.Spec.HTTPReq.Method = "GET"
	get := func(path string) *req {
		r := sent(path, nil, "")
		r.method = "GET"
		return r
	}
	if got, _ := strictMatch(t, get("/v1/users/42/orders?tag=b&status=open&tag=a"), nil, nil, nil, rec); got == nil {
		t.Fatal("a repeated parameter in another order was not served")
	}
	if got, diag := strictMatch(t, get("/v1/users/43/orders?status=open&tag=a&tag=b"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "path") {
		t.Fatalf("another id: served %v, differs %v", got != nil, differs(diag))
	}
	if got, _ := strictMatch(t, get("/v1/users/43/orders?status=open&tag=a&tag=b"), nil, nil, []string{`/users/[0-9]+`}, rec); got == nil {
		t.Fatal("keploy.yml's url noise was not excused")
	}
	if got, diag := strictMatch(t, get("/v1/users/42/orders?status=closed&tag=a&tag=b"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "query.status") {
		t.Fatalf("another query value: served %v, differs %v", got != nil, differs(diag))
	}
	rec.Spec.ReqBodyNoise = map[string][]string{"url.path.2": {}, "url.query.status": {}}
	if got, diag := strictMatch(t, get("/v1/users/43/orders?status=closed&tag=a&tag=b"), nil, nil, nil, rec); got == nil {
		t.Fatalf("a learned path segment and query value were not excused: %v", differs(diag))
	}
}

// No closest match: of two recordings neither of which is the call, neither
// answers (main's fuzzy fallback served the nearer one).
func TestStrictHasNoFuzzyFallback(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	a := recorded("a", "/v1/charge", ct, `{"amount":100,"currency":"USD","ref":"x1"}`)
	b := recorded("b", "/v1/charge", ct, `{"amount":900,"currency":"EUR","ref":"x2"}`)
	got, diag := strictMatch(t, sent("/v1/charge", ct, `{"amount":250,"currency":"USD","ref":"x1"}`), nil, nil, nil, a, b)
	if got != nil {
		t.Fatalf("served %s", got.Name)
	}
	if diag.phase != models.MatchPhaseStrict || diag.closest == nil || diag.closest.Name != "a" || !hasField(differs(diag), "body.amount") {
		t.Fatalf("diag = %+v, differs %v; want the strict phase, closest a, body.amount", diag, differs(diag))
	}

	t.Setenv("KEPLOY_PROVEN", "")
	db := &mockMemDb{mocks: []*models.Mock{a, b}, updateUnFilteredReturn: true}
	ok, stub, _, err := newHTTP().match(context.Background(), sent("/v1/charge", ct, `{"amount":250,"currency":"USD","ref":"x1"}`), db, models.FlakyHeaderNoise(), nil, nil, true, false, false, true, true)
	if err != nil || !ok || stub.Name != "a" {
		t.Fatalf("without the switch the fuzzy fallback served %v (ok %v, err %v), want a as on main", stub, ok, err)
	}
}

// The miss report diffs against the strict comparison's closest recording,
// with its field diffs (values redacted where secret) and Differs naming the
// fields without values.
func TestStrictMissReportNamesWhatDiffers(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("charge", "/charge", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer abcdefgh12345678"}, `{"amount":100,"currency":"USD"}`)
	live := sent("/charge", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer zzzzzzzz87654321"}, `{"amount":250,"currency":"USD"}`)
	_ = ct
	got, diag := strictMatch(t, live, nil, nil, nil, rec)
	if got != nil {
		t.Fatal("served")
	}
	r := newHTTP().buildHTTPMismatchReport(&http.Request{Method: "POST", URL: &url.URL{Path: "/charge"}, Host: "api.example", Header: live.header}, live.body, &mockMemDb{mocks: []*models.Mock{rec}}, nil, nil, nil, diag)
	if r.ClosestMock != "charge" || r.MatchPhase != models.MatchPhaseStrict {
		t.Fatalf("report closest %q phase %q", r.ClosestMock, r.MatchPhase)
	}
	if strings.Join(r.Differs, ",") != "header.authorization,body.amount" {
		t.Fatalf("Differs = %v", r.Differs)
	}
	for _, d := range r.FieldDiffs {
		if d.Path == "header.authorization" && (strings.Contains(d.Expected, "abcdefgh") || strings.Contains(d.Actual, "zzzzzzzz")) {
			t.Fatalf("a credential's value reached the report: %+v", d)
		}
	}
}

// The reason names a field without any value: an object key the app sent
// (an email address, even one with dots in it, an ID) is written {}. The
// report keeps the full name, as the noise config is written.
func TestStrictNamesFieldsWithoutValues(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/acl", ct, `{"users":{"alice.smith@example.com":{"role":"admin"}},"orders":{"9876543210":{"total":5}}}`)
	got, diag := strictMatch(t, sent("/acl", ct, `{"users":{"alice.smith@example.com":{"role":"owner"}},"orders":{"9876543210":{"total":6}}}`), nil, nil, nil, rec)
	if got != nil {
		t.Fatal("served")
	}
	if d := differs(diag); strings.Join(d, ",") != "body.orders.{}.total,body.users.{}.role" {
		t.Fatalf("differs %v", d)
	}
	if !hasField(func() []string {
		var out []string
		for _, d := range fieldDiffs(diag.differs) {
			out = append(out, d.Path)
		}
		return out
	}(), "body.users.alice.smith@example.com.role") {
		t.Fatal("the report lost the field's full name")
	}
}
