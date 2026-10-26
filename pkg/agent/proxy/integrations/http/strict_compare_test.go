package http

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
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
	return strictMatchIn(t, context.Background(), input, headerNoise, bodyNoise, urlNoise, mocks...)
}

// strictMatchIn is strictMatch on a connection whose context is ctx.
func strictMatchIn(t *testing.T, ctx context.Context, input *req, headerNoise, bodyNoise map[string][]string, urlNoise []string, mocks ...*models.Mock) (*models.Mock, *matchDiag) {
	t.Helper()
	t.Setenv("KEPLOY_PROVEN", "1")
	hn := models.ProvenFlakyHeaderNoise()
	for k, v := range headerNoise {
		hn[k] = v
	}
	db := &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}
	ok, stub, diag, err := newHTTP().match(ctx, input, db, hn, bodyNoise, urlNoise, true, false, false, true, true)
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

// A form is compared key by key, in any order of its keys and of a repeated
// key's values (as the learn step reads them); a field the mock learned may
// change.
func TestStrictComparesForms(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	rec := recorded("m", "/oauth/token", ct, "grant_type=client_credentials&scope=a&scope=b&nonce=n1")
	if got, _ := strictMatch(t, sent("/oauth/token", ct, "nonce=n1&scope=a&grant_type=client_credentials&scope=b"), nil, nil, nil, rec); got == nil {
		t.Fatal("the same form with its keys in another order was not served")
	}
	if got, diag := strictMatch(t, sent("/oauth/token", ct, "grant_type=client_credentials&scope=b&scope=a&nonce=n1"), nil, nil, nil, rec); got == nil {
		t.Fatalf("a repeated key's values in another order were not served: %v", differs(diag))
	}
	if got, diag := strictMatch(t, sent("/oauth/token", ct, "grant_type=client_credentials&scope=a&nonce=n1"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body.scope") {
		t.Fatalf("a repeated key with a value fewer: served %v, differs %v", got != nil, differs(diag))
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
// what the mock learned may differ; parameters, and a repeated parameter's
// values, in any order (as the learn step reads them).
func TestStrictComparesThePathAndTheQuery(t *testing.T) {
	rec := recorded("m", "/v1/users/42/orders?status=open&tag=a&tag=b", nil, "")
	rec.Spec.HTTPReq.Method = "GET"
	get := func(path string) *req {
		r := sent(path, nil, "")
		r.method = "GET"
		return r
	}
	if got, _ := strictMatch(t, get("/v1/users/42/orders?tag=a&status=open&tag=b"), nil, nil, nil, rec); got == nil {
		t.Fatal("the parameters in another order were not served")
	}
	if got, diag := strictMatch(t, get("/v1/users/42/orders?tag=b&status=open&tag=a"), nil, nil, nil, rec); got == nil {
		t.Fatalf("a repeated parameter's values in another order were not served: %v", differs(diag))
	}
	if got, diag := strictMatch(t, get("/v1/users/42/orders?status=open&tag=a&tag=b&page=2"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "query.page") {
		t.Fatalf("a parameter more: served %v, differs %v", got != nil, differs(diag))
	}
	if got, diag := strictMatch(t, get("/v1/users/42/orders?status=open&tag=a&tag=c"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "query.tag") {
		t.Fatalf("a repeated parameter with another value: served %v, differs %v", got != nil, differs(diag))
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
	// Keys are named by the rule a call's path segments follow
	// (models.KeyShape): a short ID with a digit and a random token in both
	// cases are values the app sent too.
	keys := recorded("m", "/acl", ct, `{"keys":{"kqzx9w":1,"NffrFeUfNVaHib":2},"amount":1}`)
	if got, diag := strictMatch(t, sent("/acl", ct, `{"keys":{"kqzx9w":3,"NffrFeUfNVaHib":4},"amount":1}`), nil, nil, nil, keys); got != nil {
		t.Fatal("served")
	} else if d := differs(diag); strings.Join(d, ",") != "body.keys.{}" {
		t.Fatalf("differs %v; want body.keys.{} alone", d)
	}
}

// What is not the call is never compared: Content-Length (the body is, and a
// body whose excused field changed length still answers), and keploy's own
// headers and query parameters. A header the obfuscator redacted may change
// its value, not go away.
func TestStrictSkipsWhatIsNotTheCall(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json", "Content-Length": "10", "Keploy-Test-Id": "t-1", "X-Api-Key": "REDACTED-7"}
	rec := recorded("m", "/v1/charge?keployTrace=1", ct, `{"id":"a"}`)
	rec.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	rec.Noise = []string{`^REDACTED-\d+$`}
	live := sent("/v1/charge?keployTrace=2", map[string]string{"Content-Type": "application/json", "Content-Length": "15", "Keploy-Test-Id": "t-2", "X-Api-Key": "live-key"}, `{"id":"abcdef"}`)
	if got, diag := strictMatch(t, live, nil, nil, nil, rec); got == nil {
		t.Fatalf("not served: %v", differs(diag))
	}
	live.header.Del("X-Api-Key")
	if got, diag := strictMatch(t, live, nil, nil, nil, rec); got != nil || !hasField(differs(diag), "header.x-api-key") {
		t.Fatalf("a redacted header that went away: served %v, differs %v", got != nil, differs(diag))
	}
}

// A repeated header's lines are compared in their order (RFC 9110 §5.3).
func TestStrictComparesARepeatedHeaderInOrder(t *testing.T) {
	rec := recorded("m", "/x", map[string]string{"Accept-Language": "en,fr"}, "")
	live := func(lines ...string) *req {
		r := sent("/x", nil, "")
		for _, l := range lines {
			r.header.Add("Accept-Language", l)
		}
		return r
	}
	if got, _ := strictMatch(t, live("en", "fr"), nil, nil, nil, rec); got == nil {
		t.Fatal("the same lines were not served")
	}
	if got, diag := strictMatch(t, live("fr", "en"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "header.accept-language") {
		t.Fatalf("the lines in another order: served %v, differs %v", got != nil, differs(diag))
	}
}

// A body with data after its JSON document is not that document.
func TestStrictJSONHasNothingAfterIt(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/x", ct, `{"a":1}`)
	if got, diag := strictMatch(t, sent("/x", ct, `{"a":1} {"b":2}`), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body") {
		t.Fatalf("trailing data: served %v, differs %v", got != nil, differs(diag))
	}
}

// keploy.yml's request-body noise excuses a field whose value it covers on
// both sides: a value the live call sends that the noise does not cover is
// compared.
func TestStrictDeclaredBodyNoiseCoversBothSides(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/x", ct, `{"ts":"2026-10-14T10:00:00Z"}`)
	noise := map[string][]string{"ts": {`^\d{4}-\d{2}-\d{2}T`}}
	if got, diag := strictMatch(t, sent("/x", ct, `{"ts":"2026-10-15T11:00:00Z"}`), nil, noise, nil, rec); got == nil {
		t.Fatalf("a value the noise covers on both sides was compared: %v", differs(diag))
	}
	if got, diag := strictMatch(t, sent("/x", ct, `{"ts":"yesterday"}`), nil, noise, nil, rec); got != nil || !hasField(differs(diag), "body.ts") {
		t.Fatalf("a live value the noise does not cover: served %v, differs %v", got != nil, differs(diag))
	}
}

// A container the mock learned (an object or array at a learned place)
// excuses what it holds, keeping its type.
func TestStrictALearnedContainerExcusesItsMembers(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/x", ct, `{"meta":{"trace":"t1","span":"s1"},"amount":1}`)
	rec.Spec.ReqBodyNoise = map[string][]string{"body.meta": {}}
	if got, diag := strictMatch(t, sent("/x", ct, `{"meta":{"trace":"t2","span":"s2"},"amount":1}`), nil, nil, nil, rec); got == nil {
		t.Fatalf("members of a learned container were compared: %v", differs(diag))
	}
	if got, _ := strictMatch(t, sent("/x", ct, `{"meta":"flat","amount":1}`), nil, nil, nil, rec); got != nil {
		t.Fatal("a learned container changed its type and was served")
	}
	if got, _ := strictMatch(t, sent("/x", ct, `{"meta":{"trace":"t2","span":"s2"},"amount":2}`), nil, nil, nil, rec); got != nil {
		t.Fatal("a field outside the learned container was excused")
	}
}

// A form value the obfuscator redacted (its whole "key=value" segment as
// written) may change.
func TestStrictARedactedFormValueMayChange(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	rec := recorded("m", "/oauth/token", ct, "grant_type=client_credentials&client_secret=REDACTED-3")
	rec.Noise = []string{`^client_secret=REDACTED-\d+$`}
	if got, diag := strictMatch(t, sent("/oauth/token", ct, "grant_type=client_credentials&client_secret=s3cr3t"), nil, nil, nil, rec); got == nil {
		t.Fatalf("a redacted form value was compared: %v", differs(diag))
	}
	if got, _ := strictMatch(t, sent("/oauth/token", ct, "grant_type=password&client_secret=s3cr3t"), nil, nil, nil, rec); got != nil {
		t.Fatal("another form value beside a redacted one was excused")
	}
}

// Only stateful recordings take turns, in their record order: a recording
// replayed as it is (ConsumeReuse) takes none, and the pool's order is not
// the record's.
func TestStrictTheCallsRecordingsTakeTurnsInRecordOrder(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Unix(1_700_000_000, 0)
	cm := func(name string, ts time.Time, consume models.ConsumeMode) *models.Mock {
		m := onHost(name, "/counter", "api.example")
		m.TestModeInfo.Consume = consume
		m.Spec.ReqTimestampMock = ts
		return m
	}
	first := cm("first", base, models.ConsumeCursorSaturate)
	second := cm("second", base.Add(time.Second), models.ConsumeCursorSaturate)
	once := cm("once", base.Add(2*time.Second), models.ConsumeReuse)
	h := newHTTP()
	db := &mockMemDb{}
	pick := func(fits ...*models.Mock) string {
		m, commit, _ := h.strictCursorPick(fits, nil, db, true, nil)
		if commit != nil {
			commit()
		}
		return m.Name
	}
	// The pool lists the later recording first.
	for i, want := range []string{"first", "second", "second"} {
		if got := pick(second, first, once); got != want {
			t.Fatalf("pick %d = %s, want %s (record order, saturating, the once-served recording outside the group)", i+1, got, want)
		}
	}
}

// The closest recording a miss is diffed against is one of the call's route
// (method, host, path) when there is one, even if a recording of another
// route differs in fewer fields.
func TestStrictTheClosestRecordingIsOfTheCallsRoute(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json", "X-A": "1", "X-B": "1"}
	route := recorded("route", "/v1/charge", ct, `{"amount":100}`)
	other := recorded("other", "/v1/refund", map[string]string{"Content-Type": "application/json", "X-A": "2", "X-B": "2"}, `{"amount":250}`)
	live := sent("/v1/refund", map[string]string{"Content-Type": "application/json", "X-A": "2", "X-B": "2"}, `{"amount":250}`)
	live.url.Path = "/v1/charge"
	_, diag := strictMatch(t, live, nil, nil, nil, other, route)
	if diag == nil || diag.closest == nil || diag.closest.Name != "route" {
		t.Fatalf("closest = %+v, want the recording of the call's route", diag)
	}
}

// A miss names only the fields the strict comparison found differ: not a
// body field it excused (a learned place), nor a body it found equal (a
// multipart form under another boundary).
func TestStrictDiffersNamesOnlyWhatDiffers(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json", "X-Tenant": "acme"}
	rec := recorded("m", "/v1/charge", ct, `{"items":[{"at":1},{"at":2}]}`)
	rec.Spec.ReqBodyNoise = map[string][]string{"body.items[1].at": {}}
	live := sent("/v1/charge", map[string]string{"Content-Type": "application/json", "X-Tenant": "globex"}, `{"items":[{"at":1},{"at":9}]}`)
	got, diag := strictMatch(t, live, nil, nil, nil, rec)
	if got != nil {
		t.Fatal("served")
	}
	r := newHTTP().buildHTTPMismatchReport(&http.Request{Method: "POST", URL: &url.URL{Path: "/v1/charge"}, Host: "api.example", Header: live.header}, live.body, &mockMemDb{mocks: []*models.Mock{rec}}, nil, nil, nil, diag)
	if strings.Join(r.Differs, ",") != "header.x-tenant" {
		t.Fatalf("Differs = %v, want only the header", r.Differs)
	}

	body, mct := multipartBody(t, "rec-boundary", map[string]string{"name": "pen", "kind": "x"}, "hello")
	up := recorded("up", "/upload", map[string]string{"Content-Type": mct, "X-Tenant": "acme"}, body)
	same, sameCT := multipartBody(t, "live-boundary-123", map[string]string{"name": "pen", "kind": "x"}, "hello")
	liveUp := sent("/upload", map[string]string{"Content-Type": sameCT, "X-Tenant": "globex"}, same)
	got, diag = strictMatch(t, liveUp, nil, nil, nil, up)
	if got != nil {
		t.Fatal("served")
	}
	r = newHTTP().buildHTTPMismatchReport(&http.Request{Method: "POST", URL: &url.URL{Path: "/upload"}, Host: "api.example", Header: liveUp.header}, liveUp.body, &mockMemDb{mocks: []*models.Mock{up}}, nil, nil, nil, diag)
	if strings.Join(r.Differs, ",") != "header.x-tenant" {
		t.Fatalf("Differs = %v, want only the header", r.Differs)
	}
}

// A container the mock learned excuses the fields it holds in a form too:
// a learned body excuses every value, not a key that went away.
func TestStrictALearnedBodyExcusesAFormsValues(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	rec := recorded("m", "/oauth/token", ct, "grant_type=client_credentials&nonce=n1")
	rec.Spec.ReqBodyNoise = map[string][]string{"body": {}}
	if got, diag := strictMatch(t, sent("/oauth/token", ct, "grant_type=password&nonce=n2"), nil, nil, nil, rec); got == nil {
		t.Fatalf("the values of a learned body were compared: %v", differs(diag))
	}
	if got, diag := strictMatch(t, sent("/oauth/token", ct, "grant_type=password"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body.nonce") {
		t.Fatalf("a key that went away: served %v, differs %v", got != nil, differs(diag))
	}
}

// A path-only recording is read as a request line is: "//v1/charge" is that
// path, not host v1.
func TestStrictReadsAPathOnlyRecordingAsARequestLine(t *testing.T) {
	rec := recorded("m", "//v1/charge", nil, "")
	rec.Spec.HTTPReq.Method = "GET"
	live := sent("/x", nil, "")
	live.method = "GET"
	live.url = &url.URL{Path: "//v1/charge"}
	if got, diag := strictMatch(t, live, nil, nil, nil, rec); got == nil {
		t.Fatalf("the very call was not served: %v", differs(diag))
	}
}

// A recording that kept its query only in URLParams (which joins a repeated
// parameter's values with ", ") is compared value by value.
func TestStrictReadsARecordingsURLParams(t *testing.T) {
	rec := recorded("m", "/search", nil, "")
	rec.Spec.HTTPReq.Method = "GET"
	rec.Spec.HTTPReq.URLParams = map[string]string{"tag": "a, b", "q": "pen"}
	get := func(path string) *req {
		r := sent(path, nil, "")
		r.method = "GET"
		return r
	}
	if got, diag := strictMatch(t, get("/search?q=pen&tag=b&tag=a"), nil, nil, nil, rec); got == nil {
		t.Fatalf("the recorded query was not served: %v", differs(diag))
	}
	if got, diag := strictMatch(t, get("/search?q=pen&tag=a"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "query.tag") {
		t.Fatalf("a value fewer: served %v, differs %v", got != nil, differs(diag))
	}
	if got, diag := strictMatch(t, get("/search?q=ink&tag=a&tag=b"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "query.q") {
		t.Fatalf("another value: served %v, differs %v", got != nil, differs(diag))
	}
}

// A multipart form must hold the same parts: a part more, or an upload where
// a field was, or another file name, is another call.
func TestStrictComparesAMultipartFormsParts(t *testing.T) {
	body, ct := multipartBody(t, "rec", map[string]string{"name": "pen"}, "hello")
	rec := recorded("m", "/upload", map[string]string{"Content-Type": ct}, body)
	more, moreCT := multipartBody(t, "live", map[string]string{"name": "pen", "kind": "x"}, "hello")
	if got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": moreCT}, more), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body") {
		t.Fatalf("a part more: served %v, differs %v", got != nil, differs(diag))
	}
	// A part after all the recorded ones.
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.SetBoundary("live-extra")
	_ = w.WriteField("name", "pen")
	fw, _ := w.CreateFormFile("doc", "a.txt")
	_, _ = fw.Write([]byte("hello"))
	_ = w.WriteField("trailer", "x")
	_ = w.Close()
	if got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": w.FormDataContentType()}, b.String()), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body") {
		t.Fatalf("a part after the recorded ones: served %v, differs %v", got != nil, differs(diag))
	}
	renamed := strings.ReplaceAll(body, `filename="`, `filename="other-`)
	if renamed == body {
		t.Fatal("the fixture names no file")
	}
	if got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": ct}, renamed), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "body") {
		t.Fatalf("another file name: served %v, differs %v", got != nil, differs(diag))
	}
}

// A query value the mock learned may change while the parameter has one
// value; a parameter with several values is compared.
func TestStrictALearnedQueryValueIsOneValue(t *testing.T) {
	rec := recorded("m", "/feed?cursor=c1&tag=a&tag=b", nil, "")
	rec.Spec.HTTPReq.Method = "GET"
	rec.Spec.ReqBodyNoise = map[string][]string{"url.query.cursor": {}, "url.query.tag": {}}
	get := func(path string) *req {
		r := sent(path, nil, "")
		r.method = "GET"
		return r
	}
	if got, diag := strictMatch(t, get("/feed?cursor=c9&tag=a&tag=b"), nil, nil, nil, rec); got == nil {
		t.Fatalf("a learned single value was compared: %v", differs(diag))
	}
	if got, diag := strictMatch(t, get("/feed?cursor=c1&tag=x&tag=y"), nil, nil, nil, rec); got != nil || !hasField(differs(diag), "query.tag") {
		t.Fatalf("a learned parameter with several values: served %v, differs %v", got != nil, differs(diag))
	}
}

// A recording replayed as it is (ConsumeReuse) answers as it is, ahead of
// stateful ones: it takes no turn.
func TestStrictARecordingReplayedAsItIsTakesNoTurn(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Unix(1_700_000_000, 0)
	once := onHost("once", "/counter", "api.example")
	once.TestModeInfo.Consume = models.ConsumeReuse
	once.Spec.ReqTimestampMock = base
	c1 := onHost("c1", "/counter", "api.example")
	c1.TestModeInfo.Consume = models.ConsumeCursorSaturate
	c1.Spec.ReqTimestampMock = base.Add(time.Second)
	c2 := onHost("c2", "/counter", "api.example")
	c2.TestModeInfo.Consume = models.ConsumeCursorSaturate
	c2.Spec.ReqTimestampMock = base.Add(2 * time.Second)
	h := newHTTP()
	db := &mockMemDb{}
	for i := 0; i < 2; i++ {
		m, commit, _ := h.strictCursorPick([]*models.Mock{once, c1, c2}, nil, db, true, nil)
		if commit != nil || m.Name != "once" {
			t.Fatalf("pick %d = %s (commit %v), want once with no turn", i+1, m.Name, commit != nil)
		}
	}
}

// clockConn is a connection's context whose learn tracker reads the
// application's clock as the real one (none: keploy cannot tell it), with a
// data-plane call served at served: a call that reaches keploy after it made
// its clock reads after it.
func clockConn(appClock bool, served time.Time) context.Context {
	tr := learn.NewTracker()
	if appClock {
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	}
	tr.ServedMockAt(httpMock("earlier", "GET", "/v1/account"), served)
	tr.SetLearnEnded(true)
	return learn.WithTracker(context.Background(), tr)
}

// A place the learn step learned as a read of the application's clock
// (MockSpec.Clocks) is still compared at a replay: the call must carry a read
// of the clock made in its own window (since the previous serve), plus the
// learned offset, at every kind of place (a body field, a query value, a
// header). A value moved thirty days, the epoch, the recorded value itself,
// a time to live changed by a minute, a value keploy cannot read, a missing
// field, a place two learnings left Empty, and every value where keploy
// cannot tell the application's clock are refused, the field named; learned
// noise on the same place (a switch-off rewrite can put both on a file)
// excuses none of them. keploy.yml's noise does: the user declared the field
// not compared.
func TestStrictChecksALearnedClockReadInItsCallsWindow(t *testing.T) {
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	recAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339) }
	now := base.Add(time.Second)
	ttl := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Second, Res: time.Second}
	plain := models.ClockOffset{Res: time.Second}
	mock := func(clocks map[string]models.ClockOffset) *models.Mock {
		m := recorded("usage", "/v1/usage?since="+stamp(recAt), map[string]string{"Content-Type": "application/json", "X-Sent-At": stamp(recAt)},
			`{"since":"`+stamp(recAt)+`","expires":"`+stamp(recAt.Add(30*time.Minute))+`","q":"x"}`)
		m.Spec.Clocks = clocks
		m.Spec.ReqBodyNoise = map[string][]string{}
		for p := range clocks {
			m.Spec.ReqBodyNoise[p] = []string{}
		}
		return m
	}
	call := func(since, expires, query, header string) *req {
		body := `{"q":"x","since":"` + since + `","expires":"` + expires + `"}`
		if since == "" {
			body = `{"q":"x","expires":"` + expires + `"}`
		}
		r := sent("/v1/usage?since="+query, map[string]string{"Content-Type": "application/json", "X-Sent-At": header}, body)
		r.arrived = base.Add(2 * time.Second)
		return r
	}
	all := map[string]models.ClockOffset{"body.since": plain, "body.expires": ttl, "url.query.since": plain, "header.x-sent-at": plain}
	own := call(stamp(now), stamp(now.Add(30*time.Minute)), stamp(now), stamp(now))
	for _, c := range []struct {
		name      string
		clocks    map[string]models.ClockOffset
		appClock  bool
		in        *req
		bodyNoise map[string][]string
		differs   string
	}{
		{"its own call's now and now + 30m", all, true, own, nil, ""},
		{"moved thirty days", all, true, call(stamp(now.AddDate(0, 0, -30)), stamp(now.Add(30*time.Minute)), stamp(now), stamp(now)), nil, "body.since"},
		{"the epoch", all, true, call("1970-01-01T00:00:00Z", stamp(now.Add(30*time.Minute)), stamp(now), stamp(now)), nil, "body.since"},
		{"the recorded value", all, true, call(stamp(recAt), stamp(now.Add(30*time.Minute)), stamp(now), stamp(now)), nil, "body.since"},
		{"a time to live of 31m", all, true, call(stamp(now), stamp(now.Add(31*time.Minute)), stamp(now), stamp(now)), nil, "body.expires"},
		{"no instant", all, true, call("soon", stamp(now.Add(30*time.Minute)), stamp(now), stamp(now)), nil, "body.since"},
		{"the field gone", map[string]models.ClockOffset{"body.since": plain}, true, call("", stamp(recAt.Add(30*time.Minute)), stamp(recAt), stamp(recAt)), nil, "body.since"},
		{"a query value moved", all, true, call(stamp(now), stamp(now.Add(30*time.Minute)), stamp(now.AddDate(0, 0, -30)), stamp(now)), nil, "query.since"},
		{"a header moved", all, true, call(stamp(now), stamp(now.Add(30*time.Minute)), stamp(now), stamp(recAt)), nil, "header.x-sent-at"},
		{"two learnings left no one offset", map[string]models.ClockOffset{"body.since": {Min: time.Hour, Max: time.Hour}}, true, own, nil, "body.since"},
		{"keploy cannot tell the app's clock", all, false, own, nil, "body.since"},
		{"keploy.yml's noise reaches it", map[string]models.ClockOffset{"body.since": plain}, false, call(stamp(now), stamp(recAt.Add(30*time.Minute)), stamp(recAt), stamp(recAt)), map[string][]string{"since": {}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, diag := strictMatchIn(t, clockConn(c.appClock, base), c.in, nil, c.bodyNoise, nil, mock(c.clocks))
			if c.differs == "" {
				if got == nil {
					t.Fatalf("refused (%v); want it answered", differs(diag))
				}
				return
			}
			if got != nil || !hasField(differs(diag), c.differs) {
				t.Fatalf("answered %v, differs %v; want refused naming %s", got != nil, differs(diag), c.differs)
			}
			why := ""
			for _, f := range diag.differs {
				if f.shape == c.differs {
					why = f.why
				}
			}
			if !strings.Contains(why, "learned") {
				t.Fatalf("the refusal of %s gives no reason: %q", c.differs, why)
			}
		})
	}
	// A read of the clock needs no noise beside it: the walk lets its value
	// change, and the check decides it.
	bare := mock(all)
	bare.Spec.ReqBodyNoise = nil
	if got, diag := strictMatchIn(t, clockConn(true, base), own, nil, nil, nil, bare); got == nil {
		t.Fatalf("refused (%v) with no noise beside the reads of the clock; want it answered", differs(diag))
	}
}

// A refused read of the application's clock names its field and why in the
// miss report (MockMismatchReport.FieldReasons), not only in the agent's
// log: the field as Differs names it, and the check's why, which names it
// so and no value. An object key that may be a value the app sent is {} in
// both, so two such keys refused for one reason give it once; a field that
// differs for no reason of its own (a changed value) gives none.
func TestARefusedClockReadNamesItsFieldAndWhyInTheMissReport(t *testing.T) {
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	recAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339) }
	moved := stamp(base.AddDate(0, 0, -30))
	rec := recorded("usage", "/v1/usage", map[string]string{"Content-Type": "application/json"},
		`{"since":"`+stamp(recAt)+`","users":{"kqzx9w":{"since":"`+stamp(recAt)+`"},"pq8r7s":{"since":"`+stamp(recAt)+`"}},"q":"x"}`)
	plain := models.ClockOffset{Res: time.Second}
	rec.Spec.Clocks = map[string]models.ClockOffset{"body.since": plain, "body.users.kqzx9w.since": plain, "body.users.pq8r7s.since": plain}
	live := sent("/v1/usage", map[string]string{"Content-Type": "application/json"},
		`{"q":"y","since":"`+moved+`","users":{"kqzx9w":{"since":"`+moved+`"},"pq8r7s":{"since":"`+moved+`"}}}`)
	live.arrived = base.Add(2 * time.Second)
	got, diag := strictMatchIn(t, clockConn(true, base), live, nil, nil, nil, rec)
	if got != nil {
		t.Fatal("served a call whose reads of the clock moved thirty days")
	}
	r := newHTTP().buildHTTPMismatchReport(&http.Request{Method: "POST", URL: &url.URL{Path: "/v1/usage"}, Host: "api.example", Header: live.header}, live.body, &mockMemDb{mocks: []*models.Mock{rec}}, nil, nil, nil, diag)
	for _, f := range []string{"body.q", "body.since", "body.users.{}.since"} {
		if !hasField(r.Differs, f) {
			t.Fatalf("Differs = %v; want %s named", r.Differs, f)
		}
	}
	whys := map[string]string{}
	for _, fr := range r.FieldReasons {
		if _, twice := whys[fr.Field]; twice {
			t.Fatalf("%s is given a reason twice: %+v", fr.Field, r.FieldReasons)
		}
		whys[fr.Field] = fr.Why
	}
	if len(whys) != 2 {
		t.Fatalf("FieldReasons = %+v; want body.since and body.users.{}.since, each once", r.FieldReasons)
	}
	for _, f := range []string{"body.since", "body.users.{}.since"} {
		why := whys[f]
		if !strings.HasPrefix(why, f+" was learned as ") || !strings.HasSuffix(why, "made in this call's window: it changed") {
			t.Fatalf("the reason of %s is %q; want the check's why naming it", f, why)
		}
		for _, v := range []string{"kqzx9w", "pq8r7s", moved, stamp(recAt), "2026"} {
			if strings.Contains(why, v) {
				t.Fatalf("the reason of %s names %q: %q", f, v, why)
			}
		}
	}
}

// Without the proven switch keploy reads no learned read of the clock, and
// compares as before it.
func TestNoClockIsReadWithTheSwitchOff(t *testing.T) {
	m := httpMock("m", "GET", "/v1")
	m.Spec.Clocks = map[string]models.ClockOffset{"body.since": {Res: time.Second}}
	t.Setenv("KEPLOY_PROVEN", "")
	if clocksOf(m) != nil {
		t.Fatal("a clock read with the switch off")
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if clocksOf(m) == nil {
		t.Fatal("no clock read with the switch on")
	}
}

// keploy.yml's noise skips a learned read of the clock wherever it reaches
// the place: a header name, a url pattern, a body path with an array
// position. Elsewhere a value that is not this call's read of the clock is
// refused, the field named as the walk names it, the refusals in a stable
// order; a read in a path segment files the recording under the learned
// request key; learned noise on the place no longer excuses it in the learn
// step, which reads the place again.
func TestAClockReadsRefusalsAndKeployYmlNoise(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	plain := models.ClockOffset{Res: time.Second}
	now, old := base.Add(time.Second).Format(time.RFC3339), time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	mk := func(path, header, body string, clocks ...string) *models.Mock {
		m := recorded("m", path, map[string]string{"Content-Type": "application/json", "X-Sent-At": header}, body)
		m.Spec.Clocks = map[string]models.ClockOffset{}
		for _, c := range clocks {
			m.Spec.Clocks[c] = plain
		}
		return m
	}
	call := func(path, header, body string) *req {
		r := sent(path, map[string]string{"Content-Type": "application/json", "X-Sent-At": header}, body)
		r.arrived = base.Add(2 * time.Second)
		return r
	}
	items := `{"items":[{"at":"` + old + `"}]}`
	for _, c := range []struct {
		name         string
		m            *models.Mock
		in           *req
		header, body map[string][]string
		url          []string
		differs      string
	}{
		{"a header keploy.yml names", mk("/v1/u", old, `{}`, "header.x-sent-at"), call("/v1/u", old, `{}`), map[string][]string{"x-sent-at": {}}, nil, nil, ""},
		{"a header", mk("/v1/u", old, `{}`, "header.x-sent-at"), call("/v1/u", old, `{}`), nil, nil, nil, "header.x-sent-at"},
		{"a query value a url pattern masks", mk("/v1/u?since="+old, now, `{}`, "url.query.since"), call("/v1/u?since="+old, now, `{}`), nil, nil, []string{`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`}, ""},
		{"a path segment", mk("/v1/u/"+old, now, `{}`, "url.path.2"), call("/v1/u/"+old, now, `{}`), nil, nil, nil, "path"},
		{"an array position keploy.yml names", mk("/v1/u", now, items, "body.items[0].at"), call("/v1/u", now, items), nil, map[string][]string{"items[].at": {}}, nil, ""},
		{"an array position", mk("/v1/u", now, items, "body.items[0].at"), call("/v1/u", now, items), nil, nil, nil, "body.items[].at"},
		{"a whole JSON body", mk("/v1/u", now, `"`+old+`"`, "body"), call("/v1/u", now, `"`+old+`"`), nil, nil, nil, "body"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, diag := strictMatchIn(t, clockConn(true, base), c.in, c.header, c.body, c.url, c.m)
			if c.differs == "" {
				if got == nil {
					t.Fatalf("refused (%v); want it answered: keploy.yml's noise reaches the place", differs(diag))
				}
				return
			}
			if got != nil || !hasField(differs(diag), c.differs) {
				t.Fatalf("answered %v, differs %v; want refused naming %s", got != nil, differs(diag), c.differs)
			}
		})
	}
	// The refusals of several places come in one order, run after run.
	body, clocks := `{`, []string{}
	for i, k := range []string{"f", "b", "e", "a", "d", "c"} {
		if i > 0 {
			body += ","
		}
		body += `"` + k + `":"` + old + `"`
		clocks = append(clocks, "body."+k)
	}
	body += `}`
	for run := 0; run < 20; run++ {
		_, diag := strictMatchIn(t, clockConn(true, base), call("/v1/u", now, body), nil, nil, nil, mk("/v1/u", now, body, clocks...))
		var order []string
		for _, f := range diag.differs {
			if f.why != "" {
				order = append(order, f.shape)
			}
		}
		if strings.Join(order, " ") != "body.a body.b body.c body.d body.e body.f" {
			t.Fatalf("run %d: the refusals came as %v; want them sorted", run, order)
		}
	}
	m := mk("/v1/u/"+old, now, `{"since":"`+old+`"}`, "url.path.2", "body.since")
	if keys := recordedRequestKeys(m); len(keys) != 2 || keys[1] != requestKeyLearned("POST", 3) {
		t.Fatalf("keys %v; want the learned key %s: the path segment is a read of the clock", keys, requestKeyLearned("POST", 3))
	}
	m.Spec.ReqBodyNoise = map[string][]string{"body.since": {}}
	leaf := learn.Leaf{Path: "body.since", Noise: "body.since", Type: learn.String, Value: old}
	if ex, _ := (learnNoise{}).known(m).Excused(leaf); ex {
		t.Fatal("learned noise excused a place learned as a read of the clock")
	}
	m.Spec.Clocks = nil
	if ex, _ := (learnNoise{}).known(m).Excused(leaf); !ex {
		t.Fatal("learned noise no longer excuses its own place")
	}
}
