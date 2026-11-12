package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// liveSecret is what the app sends where the recording holds a value the
// obfuscator redacted. It is not credential-shaped and its key is off the
// sensitive-name list, so only the place the recording redacted says it is a
// secret.
const liveSecret = "zq9Plm3Xw7Rt"

// redactedMultipart is a multipart form with the fields in order.
func redactedMultipart(t *testing.T, fields ...[2]string) (string, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.SetBoundary("redacted-live-boundary"); err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String(), w.FormDataContentType()
}

// redactedLiveCase is a recorded call one of whose places the obfuscator
// redacted (Mock.Noise holds its patterns), and a live call that sends a
// secret there and differs from the recording in another field too, so it
// misses: x was "alpha1" when recorded and is "bravo2" live.
type redactedLiveCase struct {
	name             string
	url, liveURL     string
	ctype, liveCtype string
	hdr, liveHdr     map[string]string
	rec, live        string
	noise            []string
	// params, when set, are the recording's URLParams in place of those
	// its URL's query gives (an empty map: none).
	params map[string]string
	// vars are the recording's test variables.
	vars models.MockVariables
	// wholeBody: the live body does not read as its type does, and the
	// recording redacted a field of it, so the rendered body is redacted
	// whole.
	wholeBody bool
	// secrets are what the report must not carry: the live value, and its
	// parts when the call sent it escaped (liveSecret when none).
	secrets []string
}

// persisted is r as a report persists it (its JSON), but for when the call
// missed (At): that is the clock's reading, built from no part of the call
// nor of a recording, and its digits spell a live number by chance (a
// number of 4 digits about one report in 2,000), which no redaction can
// prevent. A row that looks for a live value in a report reads this.
func persisted(t testing.TB, r *models.MockMismatchReport) string {
	t.Helper()
	c := *r
	c.At = time.Time{}
	all, err := json.Marshal(&c)
	if err != nil {
		t.Errorf("the report does not marshal: %v", err)
	}
	return string(all)
}

// carried are the secrets of c the report r carries anywhere it persists.
func (c redactedLiveCase) carried(t *testing.T, r *models.MockMismatchReport) []string {
	t.Helper()
	all := persisted(t, r)
	secrets := c.secrets
	if len(secrets) == 0 {
		secrets = []string{liveSecret}
	}
	var out []string
	for _, s := range secrets {
		if strings.Contains(all, s) {
			out = append(out, s)
		}
	}
	return out
}

func redactedLiveCases(t *testing.T) []redactedLiveCase {
	recMP, recMPCT := redactedMultipart(t, [2]string{"assertion", "REDACTED_abc"}, [2]string{"x", "alpha1"})
	liveMP, liveMPCT := redactedMultipart(t, [2]string{"assertion", liveSecret}, [2]string{"x", "bravo2"})
	const jsonCT, formCT = "application/json", "application/x-www-form-urlencoded"
	value := []string{`^REDACTED_abc$`}
	// A body that cannot show x shows it in a header.
	other, liveOther := map[string]string{"X-Other": "alpha1"}, map[string]string{"X-Other": "bravo2"}
	// A live multipart form cut inside its secret field's value, and one
	// cut inside that field's header.
	cutMP, _ := redactedMultipart(t, [2]string{"x", "bravo2"}, [2]string{"assertion", liveSecret})
	cutInValue := cutMP[:strings.Index(cutMP, liveSecret)+len(liveSecret)]
	cutInHeader := cutMP[:strings.Index(cutMP, `name="assertion"`)] + `name="assertion"; filename="` + liveSecret
	// One whose secret field's header line is malformed, and one cut in a
	// value that writes the close delimiter's text (not at a line's start).
	badHeader := strings.Replace(cutMP, `Content-Disposition: form-data; name="assertion"`, `Content-Disposition form-data name assertion`, 1)
	delimMP, _ := redactedMultipart(t, [2]string{"x", "bravo2"}, [2]string{"assertion", liveSecret + " --redacted-live-boundary-- tail"})
	cutAfterDelim := delimMP[:strings.Index(delimMP, " tail")]
	// A part that holds the redacted value inside its data.
	recDoc, recDocCT := redactedMultipart(t, [2]string{"doc", "<a>REDACTED_abc</a>"}, [2]string{"x", "alpha1"})
	liveDoc, liveDocCT := redactedMultipart(t, [2]string{"doc", "<a>" + liveSecret + "</a>"}, [2]string{"x", "bravo2"})
	// A part moved under another name that ends in the redacted part's.
	liveMoved, liveMovedCT := redactedMultipart(t, [2]string{"data[assertion]", liveSecret}, [2]string{"x", "bravo2"})
	// The obfuscator redacts a number under a key it reads as a secret's
	// (otp, pin) as another number, and anchors a pattern on its literal.
	const liveNumber = "739164"
	number, numberSecret := []string{`^482913$`}, []string{liveNumber}
	// With the switch off the request matcher serves a form or a multipart
	// form whose field changed: those calls go to another path too, so
	// they miss either way.
	return []redactedLiveCase{
		{name: "json", url: "/x", ctype: jsonCT, rec: `{"assertion":"REDACTED_abc","x":"alpha1"}`, live: `{"assertion":"` + liveSecret + `","x":"bravo2"}`, noise: value},
		{name: "json-array", url: "/x", ctype: jsonCT, rec: `{"items":[{"assertion":"REDACTED_abc"}],"x":"alpha1"}`, live: `{"items":[{"assertion":"` + liveSecret + `"}],"x":"bravo2"}`, noise: value},
		{name: "json-type-changed", url: "/x", ctype: jsonCT, rec: `{"assertion":"REDACTED_abc","x":"alpha1"}`, live: `{"assertion":{"v":"` + liveSecret + `"},"x":"bravo2"}`, noise: value},
		// The obfuscator writes a form's redacted field twice: the value
		// alone, and the key=value segment of its key (formKeyNoiseRegex).
		{name: "form", url: "/x", liveURL: "/x/other", ctype: formCT, rec: `assertion=REDACTED_abc&x=alpha1`, live: `assertion=` + liveSecret + `&x=bravo2`, noise: []string{`^REDACTED_abc$`, `^assertion=[^&]+$`}},
		{name: "multipart", url: "/x", liveURL: "/x/other", ctype: recMPCT, liveCtype: liveMPCT, rec: recMP, live: liveMP, noise: value},
		{name: "query", url: "/x?assertion=REDACTED_abc", liveURL: "/x?assertion=" + liveSecret, ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		{name: "header", url: "/x", ctype: jsonCT, hdr: map[string]string{"X-Assertion": "REDACTED_abc"}, liveHdr: map[string]string{"X-Assertion": liveSecret}, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		{name: "path-segment", url: "/x/REDACTED_abc/y", liveURL: "/x/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		{name: "whole-body", url: "/x", liveURL: "/x/other", ctype: "text/plain", hdr: other, liveHdr: liveOther, rec: "REDACTED_abc", live: liveSecret, noise: value},
		{name: "json-root-array", url: "/x", ctype: jsonCT, hdr: other, liveHdr: liveOther, rec: `["REDACTED_abc","alpha1"]`, live: `["` + liveSecret + `","bravo2"]`, noise: value},
		{name: "json-root-string", url: "/x", liveURL: "/x/other", ctype: jsonCT, hdr: other, liveHdr: liveOther, rec: `"REDACTED_abc"`, live: `"` + liveSecret + `"`, noise: value},
		{name: "json-root-array-of-objects", url: "/x", ctype: jsonCT, rec: `[{"assertion":"REDACTED_abc","x":"alpha1"}]`, live: `[{"assertion":"` + liveSecret + `","x":"bravo2"}]`, noise: value},
		{name: "json-string-array", url: "/x", ctype: jsonCT, rec: `{"tags":["REDACTED_abc"],"x":"alpha1"}`, live: `{"tags":["` + liveSecret + `"],"x":"bravo2"}`, noise: value},
		{name: "json-missing-in-live", url: "/x", ctype: jsonCT, hdr: map[string]string{"X-Assertion": "REDACTED_abc"}, liveHdr: map[string]string{"X-Assertion": liveSecret}, rec: `{"assertion":"REDACTED_abc","x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		{name: "form-escaped-value", url: "/x", liveURL: "/x/other", ctype: formCT, rec: `assertion=REDACTED%5Fabc&x=alpha1`, live: `assertion=` + liveSecret + `&x=bravo2`, noise: value},
		{name: "form-key-pattern-only", url: "/x", liveURL: "/x/other", ctype: formCT, rec: `assertion=REDACTED_abc&x=alpha1`, live: `assertion=` + liveSecret + `&x=bravo2`, noise: []string{`^assertion=[^&]+$`}},
		{name: "multipart-cut-in-a-value", url: "/x", liveURL: "/x/other", ctype: recMPCT, liveCtype: liveMPCT, hdr: other, liveHdr: liveOther, rec: recMP, live: cutInValue, noise: value, wholeBody: true},
		{name: "multipart-cut-in-a-header", url: "/x", liveURL: "/x/other", ctype: recMPCT, liveCtype: liveMPCT, hdr: other, liveHdr: liveOther, rec: recMP, live: cutInHeader, noise: value, wholeBody: true},
		{name: "multipart-malformed-header", url: "/x", liveURL: "/x/other", ctype: recMPCT, liveCtype: liveMPCT, hdr: other, liveHdr: liveOther, rec: recMP, live: badHeader, noise: value, wholeBody: true},
		{name: "multipart-cut-after-delimiter-text", url: "/x", liveURL: "/x/other", ctype: recMPCT, liveCtype: liveMPCT, hdr: other, liveHdr: liveOther, rec: recMP, live: cutAfterDelim, noise: value, wholeBody: true},
		{name: "query-in-urlparams-only", url: "/x", params: map[string]string{"assertion": "REDACTED_abc"}, liveURL: "/x?assertion=" + liveSecret, ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		// A repeated parameter whose later recorded value was redacted.
		{name: "query-repeated", url: "/x?assertion=public1&assertion=REDACTED_abc", params: map[string]string{}, liveURL: "/x?assertion=public1&assertion=" + liveSecret, ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		{name: "query-in-url-only", url: "/x?assertion=REDACTED_abc", params: map[string]string{}, liveURL: "/x?assertion=" + liveSecret, ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		// A test variable's place on the path makes the report name the
		// path escaped, as a place counts it.
		{name: "path-segment-route-variable", url: "/x/REDACTED_abc/y", liveURL: "/x/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value, vars: models.MockVariables{"v": {Recorded: "y", Bind: "url.path[2]"}}},
		{name: "path-segment-escaped-route-variable", url: "/x/REDACTED_abc/y", liveURL: "/x/" + liveSecret + "%2B/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value, vars: models.MockVariables{"v": {Recorded: "y", Bind: "url.path[2]"}}},
		// The variable's place is the segment the recording redacted.
		// A %2F in a segment before the redacted one does not shift it,
		// and a %2F in the secret's own segment does not split it: a place
		// counts the escaped path's segments, on both sides.
		{name: "path-segment-after-an-escaped-slash", url: "/a%2Fb/REDACTED_abc/y", liveURL: "/a%2Fb/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		{name: "path-segment-after-an-escaped-slash-route-variable", url: "/a%2Fb/REDACTED_abc/y", liveURL: "/a%2Fb/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value, vars: models.MockVariables{"v": {Recorded: "y", Bind: "url.path[2]"}}},
		{name: "path-segment-holding-an-escaped-slash", url: "/x/REDACTED_abc/y", liveURL: "/x/zq9Plm%2F3Xw7Rt/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value, secrets: []string{"zq9Plm", "3Xw7Rt"}},
		{name: "path-segment-holding-an-escaped-slash-route-variable", url: "/x/REDACTED_abc/y", liveURL: "/x/zq9Plm%2F3Xw7Rt/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value, secrets: []string{"zq9Plm", "3Xw7Rt"}, vars: models.MockVariables{"v": {Recorded: "y", Bind: "url.path[2]"}}},
		// A recorded segment whose pattern matches it decoded.
		// A path-only recording that starts with "//" is a path, as the
		// call's request line is: its segments count from the first "/".
		{name: "path-segment-under-a-double-slash", url: "//v1/REDACTED_abc/y", liveURL: "//v1/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		// The mock's pattern names the segment as the URL holds it, escaped.
		{name: "path-segment-pattern-on-the-escaped-form", url: "/x/REDACTED%2Fabc/y", liveURL: "/x/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: []string{`^REDACTED%2Fabc$`}},
		{name: "path-segment-escaped-in-the-recording", url: "/x/REDACTED%20abc/y", liveURL: "/x/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: []string{`^REDACTED abc$`}},
		// A live body that does not read as the recording's did, whose
		// recording redacted a field of it: JSON that does not parse (a
		// trailing comma, two values), and a form or multipart form sent
		// as another type.
		{name: "json-does-not-parse", url: "/x", ctype: jsonCT, hdr: other, liveHdr: liveOther, rec: `{"assertion":"REDACTED_abc","x":"alpha1"}`, live: `{"assertion":"` + liveSecret + `","x":"bravo2",}`, noise: value, wholeBody: true},
		{name: "json-two-values", url: "/x", ctype: jsonCT, hdr: other, liveHdr: liveOther, rec: `{"assertion":"REDACTED_abc","x":"alpha1"}`, live: `{"assertion":"` + liveSecret + `","x":"bravo2"}` + "\n" + `{"y":1}`, noise: value, wholeBody: true},
		{name: "form-sent-as-another-type", url: "/x", liveURL: "/x/other", ctype: formCT, liveCtype: "text/plain", hdr: other, liveHdr: liveOther, rec: `assertion=REDACTED_abc&x=alpha1`, live: `assertion=` + liveSecret + `&x=bravo2`, noise: value, wholeBody: true},
		{name: "multipart-sent-as-another-type", url: "/x", liveURL: "/x/other", ctype: recMPCT, liveCtype: "text/plain", hdr: other, liveHdr: liveOther, rec: recMP, live: liveMP, noise: value, wholeBody: true},
		// Another type with the multipart boundary as a parameter: still
		// not a multipart form.
		{name: "multipart-sent-as-another-type-with-a-boundary", url: "/x", liveURL: "/x/other", ctype: recMPCT, liveCtype: strings.Replace(liveMPCT, "multipart/form-data", "text/plain", 1), hdr: other, liveHdr: liveOther, rec: recMP, live: liveMP, noise: value, wholeBody: true},
		{name: "path-segment-at-its-route-variable", url: "/x/REDACTED_abc/y", liveURL: "/x/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value, vars: models.MockVariables{"v": {Recorded: "REDACTED_abc", Bind: "url.path[1]"}}},
		// A body not read for fields (XML, SOAP, text): the obfuscator
		// redacts an XML element's text by its name, and a pattern's hit
		// in a text, and anchors the pattern on the value alone. The
		// value's place cannot be found in the call's body: it is redacted
		// whole.
		{name: "xml-element", url: "/x", liveURL: "/x/other", ctype: "application/xml", hdr: other, liveHdr: liveOther, rec: `<req><assertion>REDACTED_abc</assertion><x>alpha1</x></req>`, live: `<req><assertion>` + liveSecret + `</assertion><x>bravo2</x></req>`, noise: value, wholeBody: true},
		{name: "soap-element", url: "/x", liveURL: "/x/other", ctype: "text/xml", hdr: other, liveHdr: liveOther, rec: `<Envelope><Body><assertion>REDACTED_abc</assertion></Body></Envelope>`, live: `<Envelope><Body><assertion>` + liveSecret + `</assertion></Body></Envelope>`, noise: value, wholeBody: true},
		{name: "text-token", url: "/x", liveURL: "/x/other", ctype: "text/plain", hdr: other, liveHdr: liveOther, rec: `assertion REDACTED_abc x alpha1`, live: `assertion ` + liveSecret + ` x bravo2`, noise: value, wholeBody: true},
		// A multipart part's data is not read for fields either.
		{name: "multipart-part-holding-a-redacted-value", url: "/x", liveURL: "/x/other", ctype: recDocCT, liveCtype: liveDocCT, rec: recDoc, live: liveDoc, noise: value},
		// The obfuscator redacts a field by its key's name: the call's
		// field under that key is redacted wherever it sits.
		{name: "json-moved-field", url: "/x", ctype: jsonCT, rec: `{"assertion":"REDACTED_abc","x":"alpha1"}`, live: `{"data":{"assertion":"` + liveSecret + `"},"x":"bravo2"}`, noise: value},
		{name: "json-moved-field-in-an-array", url: "/x", ctype: jsonCT, rec: `{"items":[{"assertion":"REDACTED_abc"}],"x":"alpha1"}`, live: `{"data":[{"assertion":{"v":"` + liveSecret + `"}}],"x":"bravo2"}`, noise: value},
		{name: "form-moved-field", url: "/x", liveURL: "/x/other", ctype: formCT, rec: `assertion=REDACTED_abc&x=alpha1`, live: `data.assertion=` + liveSecret + `&x=bravo2`, noise: value},
		{name: "form-moved-bracket-key", url: "/x", liveURL: "/x/other", ctype: formCT, rec: `user[assertion]=REDACTED_abc&x=alpha1`, live: `data[assertion]=` + liveSecret + `&x=bravo2`, noise: value},
		{name: "json-moved-string-array", url: "/x", ctype: jsonCT, rec: `{"tags":["REDACTED_abc"],"x":"alpha1"}`, live: `{"data":{"tags":["` + liveSecret + `"]},"x":"bravo2"}`, noise: value},
		{name: "multipart-moved-field", url: "/x", liveURL: "/x/other", ctype: recMPCT, liveCtype: liveMovedCT, rec: recMP, live: liveMoved, noise: value},
		{name: "json-number", url: "/x", ctype: jsonCT, rec: `{"otp":482913,"x":"alpha1"}`, live: `{"otp":` + liveNumber + `,"x":"bravo2"}`, noise: number, secrets: numberSecret},
		{name: "json-number-nested", url: "/x", ctype: jsonCT, rec: `{"card":{"pin":482913},"x":"alpha1"}`, live: `{"card":{"pin":` + liveNumber + `},"x":"bravo2"}`, noise: number, secrets: numberSecret},
		{name: "json-number-in-an-array", url: "/x", ctype: jsonCT, rec: `{"codes":[482913],"x":"alpha1"}`, live: `{"codes":[` + liveNumber + `],"x":"bravo2"}`, noise: number, secrets: numberSecret},
		{name: "json-number-moved", url: "/x", ctype: jsonCT, rec: `{"otp":482913,"x":"alpha1"}`, live: `{"x":"bravo2","data":{"otp":` + liveNumber + `}}`, noise: number, secrets: numberSecret},
		// The obfuscator redacts a URL a JSON string holds in the URL's
		// structure (a query value, a path segment), anchoring a pattern
		// on that part alone: the string is redacted whole.
		{name: "json-url-query-value", url: "/x", ctype: jsonCT, rec: `{"callback":"https://h.example/cb?sig=REDACTED_abc","x":"alpha1"}`, live: `{"callback":"https://h.example/cb?sig=` + liveSecret + `","x":"bravo2"}`, noise: value},
		{name: "json-url-path-segment", url: "/x", ctype: jsonCT, rec: `{"callback":"https://h.example/hooks/REDACTED_abc","x":"alpha1"}`, live: `{"callback":"https://h.example/hooks/` + liveSecret + `","x":"bravo2"}`, noise: value},
		{name: "json-url-array-element", url: "/x", ctype: jsonCT, rec: `{"urls":["https://h.example/cb?sig=REDACTED_abc"],"x":"alpha1"}`, live: `{"urls":["https://h.example/cb?sig=` + liveSecret + `"],"x":"bravo2"}`, noise: value},
		{name: "json-url-root-string", url: "/x", liveURL: "/x/other", ctype: jsonCT, hdr: other, liveHdr: liveOther, rec: `"https://h.example/cb?sig=REDACTED_abc"`, live: `"https://h.example/cb?sig=` + liveSecret + `"`, noise: value, wholeBody: true},
		// A JSON body with data after its value does not read as JSON
		// (json.Unmarshal's rule): it is not read for fields.
		{name: "json-with-data-after-the-value", url: "/x", liveURL: "/x/other", ctype: jsonCT, hdr: other, liveHdr: liveOther, rec: `{"x":"alpha1"} REDACTED_abc`, live: `{"x":"bravo2"} ` + liveSecret, noise: value, wholeBody: true},
		// A pattern is read as written (the matcher's rule), not only with
		// its anchors dropped: "^+" is one or more starts of the text, a
		// pattern that names the value it anchors, and "+" alone does not
		// compile.
		{name: "json-pattern-with-a-repeated-anchor", url: "/x", ctype: jsonCT, rec: `{"assertion":"REDACTED_abc","x":"alpha1"}`, live: `{"assertion":"` + liveSecret + `","x":"bravo2"}`, noise: []string{`^+REDACTED_abc$`}},
		// A recorded URL that does not parse, at a mock that redacted
		// something: its places cannot be found, so the call's path and
		// query values are redacted whole.
		{name: "url-does-not-parse", url: "/x/%zz/REDACTED_abc", liveURL: "/x/zz/" + liveSecret + "?q=" + liveSecret, ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
	}
}

func (c redactedLiveCase) mock() *models.Mock {
	hdr := map[string]string{"Content-Type": c.ctype}
	for k, v := range c.hdr {
		hdr[k] = v
	}
	m := recorded("m", c.url, hdr, c.rec)
	m.Noise = c.noise
	if c.params != nil {
		m.Spec.HTTPReq.URLParams = c.params
	}
	m.Spec.Variables = c.vars
	return m
}

func (c redactedLiveCase) sent() (*req, *http.Request) {
	u, ct := c.url, c.ctype
	if c.liveURL != "" {
		u = c.liveURL
	}
	if c.liveCtype != "" {
		ct = c.liveCtype
	}
	hdr := map[string]string{"Content-Type": ct}
	for k, v := range c.liveHdr {
		hdr[k] = v
	}
	in := sent(u, hdr, c.live)
	if strings.HasPrefix(u, "//") {
		// The call's request line: a path, not a host.
		in.url, _ = url.ParseRequestURI(u)
	}
	request, _ := http.NewRequest("POST", "http://api.example"+u, strings.NewReader(c.live))
	request.Header = in.header.Clone()
	return in, request
}

// A miss report never carries the live value at a place the recording
// redacted: not in its field diffs, the Diff line made of them, the rendered
// live request (ReceivedReq), or any other field the report YAML and the
// platform API persist. It goes by the place: the live value matches none of
// the recording's patterns, and neither its key nor its shape says it is a
// secret. The rest of the call is still shown, and the place is shown
// redacted. Switch off and on, each also at a recording that states its
// lifetime (as the recorder writes every HTTP mock), whose report tells how
// to serve the call to every test (sessionCallHint).
func TestMissReportRedactsTheLiveValueAtARedactedPlace(t *testing.T) {
	for _, c := range redactedLiveCases(t) {
		for _, mode := range []string{"off", "off-stated-lifetime", "on", "on-stated-lifetime"} {
			on := strings.HasPrefix(mode, "on")
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				m := c.mock()
				if strings.HasSuffix(mode, "stated-lifetime") {
					m.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
				}
				if mode == "on-stated-lifetime" {
					// The hint reads the switch.
					t.Setenv(provenswitch.Env, "1")
				}
				in, request := c.sent()
				var diag *matchDiag
				if on {
					got, d := strictMatch(t, in, nil, nil, nil, m)
					if got != nil {
						t.Fatal("served a call that differs in x")
					}
					diag = d
				} else {
					t.Setenv(provenswitch.Env, "")
					ok, _, d, err := newHTTP().match(context.Background(), in, &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, nil, nil, nil, true, false, true, true, true)
					if err != nil {
						t.Fatal(err)
					}
					if ok {
						t.Fatal("served a call that differs in x")
					}
					diag = d
				}
				r := newHTTP().buildHTTPMismatchReport(request, []byte(c.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
				if r == nil {
					t.Fatal("no report")
				}
				all := persisted(t, r)
				for _, d := range r.FieldDiffs {
					t.Logf("diff %s %s exp=%q act=%q", d.Path, d.Kind, d.Expected, d.Actual)
				}
				if got := c.carried(t, r); len(got) > 0 {
					t.Errorf("the report carries the live value at a place the recording redacted (%q):\nsummary: %s\nDiff: %s\nReceivedReq:\n%s\nreport: %s", got, r.ActualSummary, r.Diff, r.ReceivedReq, all)
				}
				// The redacted segment is the one redacted, whole, and the
				// segment after it still shows.
				if strings.HasPrefix(c.name, "path-segment") && !strings.HasSuffix(r.ActualSummary, "/"+valueRedacted+"/y") {
					t.Errorf("the summary does not show the redacted segment, and only it, redacted: %s", r.ActualSummary)
				}
				// A redacted place shows valueRedacted on the recording's
				// side too, the path diff's as well.
				if strings.Contains(r.Diff, "REDACTED") {
					t.Errorf("Diff shows the recorded placeholder: %s", r.Diff)
				}
				// The rendered recording shows the place redacted too, so
				// the two sides line up.
				if strings.Contains(r.ClosestMockReq, "REDACTED") {
					t.Errorf("the rendered recording shows the recorded placeholder, not the place redacted:\n%s", r.ClosestMockReq)
				}
				if !strings.HasPrefix(r.ReceivedReq, "POST /") {
					t.Errorf("ReceivedReq does not name the path whole:\n%s", r.ReceivedReq)
				}
				for _, d := range r.FieldDiffs {
					switch {
					case d.Path == "path" && (!strings.HasPrefix(d.Expected, "/") || !strings.HasPrefix(d.Actual, "/")):
						t.Errorf("the path diff does not name the paths whole: %+v", d)
					case d.Path == "path" && strings.HasPrefix(c.name, "path-segment") && (!strings.HasSuffix(d.Expected, "/"+valueRedacted+"/y") || !strings.HasSuffix(d.Actual, "/"+valueRedacted+"/y")):
						t.Errorf("the path diff does not show the redacted segment, and only it, redacted on both sides: %+v", d)
					case strings.Contains(d.Expected, "REDACTED"):
						t.Errorf("a field diff shows the recorded placeholder: %+v", d)
					case d.Kind == models.DiffKindMissingInLive && d.Actual != "":
						t.Errorf("a field the live call does not have shows a live value: %+v", d)
					case d.Kind == models.DiffKindMissingInMock && d.Expected != "":
						t.Errorf("a field the recording does not have shows a recorded value: %+v", d)
					}
				}
				if body := r.ReceivedReq[strings.Index(r.ReceivedReq, "\n\n")+2:]; c.wholeBody && body != valueRedacted {
					t.Errorf("a live body that does not read, at a recording that redacted a field of it, is not redacted whole:\n%s", body)
				}
				if !strings.Contains(r.ReceivedReq, "bravo2") {
					t.Errorf("ReceivedReq lost the rest of the call:\n%s", r.ReceivedReq)
				}
				if mode == "off-stated-lifetime" {
					sessionHintMatchesTheCall(t, r, request)
				}
				if mode == "on-stated-lifetime" {
					sessionHintNamesTheCallExactly(t, r, request, strings.HasPrefix(c.name, "path-segment") || c.name == "url-does-not-parse")
				}
				// A query or a form renders the marker query-escaped.
				if !strings.Contains(r.ReceivedReq, valueRedacted) && !strings.Contains(r.ReceivedReq, url.QueryEscape(valueRedacted)) {
					t.Errorf("ReceivedReq does not show the redacted place redacted:\n%s", r.ReceivedReq)
				}
			})
		}
	}
}

// sessionHintMatchesTheCall fails t unless r's next steps tell how to serve
// the call to every test with a session call whose pathPrefix matches the
// call's path (as models.SessionCallRule matches one): a prefix a user can
// paste, which stops before a segment the recording redacted.
func sessionHintMatchesTheCall(t *testing.T, r *models.MockMismatchReport, request *http.Request) {
	t.Helper()
	_, hint, ok := strings.Cut(r.NextSteps, "pathPrefix: ")
	if !ok {
		t.Fatalf("the report does not tell how to serve the call to every test: %s", r.NextSteps)
	}
	prefix, err := strconv.QuotedPrefix(hint)
	if err != nil {
		t.Fatalf("the hint's pathPrefix is not quoted: %s", r.NextSteps)
	}
	prefix, _ = strconv.Unquote(prefix)
	path := request.URL.Path
	matches := path == prefix || strings.HasPrefix(path, prefix+"/")
	if strings.HasSuffix(prefix, "/") {
		matches = strings.HasPrefix(path, prefix)
	}
	if !matches || strings.Contains(prefix, valueRedacted) {
		t.Errorf("the hint's pathPrefix %q does not match the call's path, or names a redacted segment", prefix)
	}
}

// sessionHintNamesTheCallExactly fails t unless r's next steps, under the
// proven switch, name the sessionCalls entry that names the call exactly
// (models.SessionCallRule.names): its method and its whole path, or, where
// the recording redacted a segment of it, the path up to that segment, said
// to go on with segments keploy does not show; cut says the recording
// redacted one (a URL it could not read redacts the path whole).
func sessionHintNamesTheCallExactly(t *testing.T, r *models.MockMismatchReport, request *http.Request, cut bool) {
	t.Helper()
	if !strings.Contains(r.NextSteps, `method: "POST"`) {
		t.Fatalf("the hint does not name the call's method: %s", r.NextSteps)
	}
	quoted := func(s string) string {
		q, err := strconv.QuotedPrefix(s)
		if err != nil {
			t.Fatalf("the hint's path is not quoted: %s", r.NextSteps)
		}
		v, _ := strconv.Unquote(q)
		return v
	}
	path := request.URL.Path
	if _, rest, ok := strings.Cut(r.NextSteps, "pathPrefix: the call's whole path}], a path that begins "); ok {
		begins := quoted(rest)
		if !strings.HasPrefix(path, begins) || begins == path || strings.Contains(begins, valueRedacted) {
			t.Errorf("the hint says the call's path begins %q and goes on unshown, for the call's path %q", begins, path)
		}
		return
	}
	_, hint, ok := strings.Cut(r.NextSteps, "pathPrefix: ")
	if !ok {
		t.Fatalf("the report does not tell how to serve the call to every test: %s", r.NextSteps)
	}
	if got := quoted(hint); cut || got != path {
		t.Errorf("the hint's pathPrefix %q is not the call's whole path %q, or names a path a recording redacted a segment of (%v)", got, path, cut)
	}
}

// A call whose path is empty is named "/" in the hint that tells how to
// serve it to every test, as the request line names it, with the switch off
// and on: under the switch an entry names it exactly with "/"
// (models.SessionCallRule.names reads an empty path as the root).
func TestSessionHintNamesAnEmptyPathSlash(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			m := recorded("m", "/x/REDACTED_abc", map[string]string{"Content-Type": "application/json"}, `{"x":"alpha1"}`)
			m.Noise = []string{`^REDACTED_abc$`}
			m.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
			request, _ := http.NewRequest("POST", "http://api.example", strings.NewReader(`{"x":"bravo2"}`))
			request.Header.Set("Content-Type", "application/json")
			r := newHTTP().buildHTTPMismatchReport(request, []byte(`{"x":"bravo2"}`), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, nil)
			if r == nil || !strings.Contains(r.NextSteps, `pathPrefix: "/"}`) {
				t.Fatalf("the hint does not name the empty path \"/\": %+v", r)
			}
		})
	}
}

// A field diff that carries no values (a form field, an array element under
// the switch) is not shown as two empty values.
func TestMissReportFieldWithoutValuesIsNotTwoEmptyValues(t *testing.T) {
	m := recorded("m", "/x", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, `x=alpha1`)
	in := sent("/x", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, `x=bravo2`)
	got, diag := strictMatch(t, in, nil, nil, nil, m)
	if got != nil {
		t.Fatal("served a call that differs in x")
	}
	request, _ := http.NewRequest("POST", "http://api.example/x", strings.NewReader(`x=bravo2`))
	request.Header = in.header.Clone()
	r := newHTTP().buildHTTPMismatchReport(request, []byte(`x=bravo2`), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
	if r == nil || !strings.Contains(r.Diff, "body.x") {
		t.Fatalf("the report does not name body.x: %+v", r)
	}
	if strings.Contains(r.Diff, `recorded "", live ""`) {
		t.Errorf("Diff shows a field without values as two empty values: %s", r.Diff)
	}
}

// liveBound is a value the test's variable took from an earlier live call,
// at a place the recording redacted: it is a secret too.
const liveBound = "Kp4wX8rTq2Lm"

// liveNumber is a number the app sends where the recording holds a
// redacted string.
const liveNumber = "73950284611"

// noSecret fails t when the report carries either secret, and when its
// summary does not show the redacted place redacted (want).
func noSecret(t *testing.T, r *models.MockMismatchReport, wantSummary bool) {
	t.Helper()
	if r == nil {
		t.Fatal("no report")
	}
	all := persisted(t, r)
	for _, s := range []string{liveSecret, liveBound, liveNumber} {
		if strings.Contains(all, s) {
			t.Errorf("the report carries %q, a live value at a place the recording redacted:\nsummary: %s\nDiff: %s\nreport: %s", s, r.ActualSummary, r.Diff, all)
		}
	}
	if wantSummary && !strings.Contains(r.ActualSummary, valueRedacted) {
		t.Errorf("the summary does not show the redacted segment redacted: %s", r.ActualSummary)
	}
}

// A stateful walk's miss (each recording of the call has answered) names the
// call's path with the segments its last recording redacted redacted, and so
// does the report of a call paired with a recording: the call sent its secret
// there. Switch off and on; with the switch off the walk's miss at a
// recording that states its lifetime (the request matcher's walk under exact
// serving) also tells how to serve the call to every test, by a path prefix
// that names no redacted segment.
func TestAnsweredAndPairedReportsRedactTheLiveValueAtARedactedSegment(t *testing.T) {
	for _, c := range redactedLiveCases(t) {
		if !strings.HasPrefix(c.name, "path-segment") {
			continue
		}
		for _, mode := range []string{"off", "off-stated-lifetime", "on"} {
			on, env := mode == "on", ""
			if on {
				env = "1"
			}
			t.Run("answered/"+c.name+"/"+mode, func(t *testing.T) {
				t.Setenv(provenswitch.Env, env)
				m := c.mock()
				if mode == "off-stated-lifetime" {
					m.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
				}
				_, request := c.sent()
				i, _, answered := servedWalk(nil, []*models.Mock{m}, integrations.SequenceView{Cursor: &mockMemDb{}})
				if i != -1 || answered == nil || answered.last != m.Name {
					t.Fatalf("the walk answered %d (%+v); want the miss naming %s", i, answered, m.Name)
				}
				diag := &matchDiag{phase: models.MatchPhaseExhausted, candidates: 1, pool: []*models.Mock{m}, answered: answered}
				r := newHTTP().buildHTTPMismatchReport(request, []byte(c.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
				noSecret(t, r, true)
				if got := c.carried(t, r); len(got) > 0 {
					t.Errorf("the report carries %q: summary %s", got, r.ActualSummary)
				}
				if r != nil && !strings.HasSuffix(r.ActualSummary, "/"+valueRedacted+"/y") {
					t.Errorf("the summary does not show the redacted segment, and only it, redacted: %s", r.ActualSummary)
				}
				if r != nil && !strings.HasPrefix(r.ActualSummary, "POST "+c.liveURL[:2]) {
					t.Errorf("the summary does not name the call's path: %s", r.ActualSummary)
				}
				// A route variable's place is named on the miss.
				if on && len(c.vars) > 0 && len(r.VariablePlaces) == 0 {
					t.Errorf("the miss names no route variable's place: %+v", r)
				}
				if mode == "off-stated-lifetime" && r != nil {
					sessionHintMatchesTheCall(t, r, request)
				}
			})
			if mode == "off-stated-lifetime" {
				continue // a paired call's report has no next steps to serve it
			}
			t.Run("paired/"+c.name+"/"+mode, func(t *testing.T) {
				t.Setenv(provenswitch.Env, env)
				m := c.mock()
				in, _ := c.sent()
				var r *models.MockMismatchReport
				ctx := models.WithMockMismatchReporter(context.Background(), func(err error) {
					if e, ok := err.(interface {
						MismatchReport() *models.MockMismatchReport
					}); ok {
						r = e.MismatchReport()
					}
				})
				reportPairing(ctx, in, m, integrations.AnsweredCall{}, false)
				noSecret(t, r, true)
				if got := c.carried(t, r); len(got) > 0 {
					t.Errorf("the report carries %q: summary %s", got, r.ActualSummary)
				}
				if r != nil && !strings.HasSuffix(r.ActualSummary, "/"+valueRedacted+"/y") {
					t.Errorf("the summary does not show the redacted segment, and only it, redacted: %s", r.ActualSummary)
				}
			})
		}
	}
}

// A miss at a test variable whose place the recording redacted shows the
// values there redacted: the value the call sent, and the variable's value
// in the test, which an earlier live call sent. Not in its field diffs, nor
// in the Diff its evidence makes. The variable pass runs under the switch
// only.
func TestVariableMissRedactsTheLiveValueAtARedactedPlace(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const rec = "REDACTED_abc"
	noise := []string{`^REDACTED_(abc|def)$`}
	bind := func() *models.Mock {
		m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"`+rec+`","item":"pen"}`, 201, `{}`,
			models.MockVariables{"orderId": bindV(rec, "body.id")}), 1)
		m.Noise = noise
		return m
	}
	bindLive := liveReq("POST", "/orders", `{"id":"`+liveBound+`","item":"pen"}`)
	type step struct {
		method, path, body string
		hdr                map[string]string
	}
	for _, c := range []struct {
		name  string
		mocks func() []*models.Mock
		first *req
		// give, when set, is a value the test gave before the call.
		give *variables.Binding
		live step
		why  string
		// recorded is what the recording holds at the redacted place
		// ("REDACTED_" when unset): Diff does not quote it either, as a
		// redacted place shows valueRedacted on both sides.
		recorded string
	}{
		{name: "use-at-a-path-segment", why: "this call sent", mocks: func() []*models.Mock {
			m := at(varMock("mock-2", "GET", "http://store/orders/"+rec, "", 200, `{}`, models.MockVariables{"orderId": useV(rec, []string{"url.path[1]"})}), 2)
			m.Noise = noise
			return []*models.Mock{bind(), m}
		}, first: bindLive, live: step{method: "GET", path: "/orders/" + liveSecret}},
		{name: "use-at-a-query-value", why: "this call sent", mocks: func() []*models.Mock {
			m := at(varMock("mock-2", "GET", "http://store/items?tok="+rec, "", 200, `{}`, models.MockVariables{"orderId": useV(rec, []string{"url.query.tok"})}), 2)
			m.Noise = noise
			return []*models.Mock{bind(), m}
		}, first: bindLive, live: step{method: "GET", path: "/items?tok=" + liveSecret}},
		{name: "use-at-a-header", why: "this call sent", mocks: func() []*models.Mock {
			m := at(varMock("mock-2", "GET", "http://store/items", "", 200, `{}`, models.MockVariables{"orderId": useV(rec, []string{"header.X-Tok"})}), 2)
			m.Spec.HTTPReq.Header["X-Tok"] = rec
			m.Noise = noise
			return []*models.Mock{bind(), m}
		}, first: bindLive, live: step{method: "GET", path: "/items", hdr: map[string]string{"X-Tok": liveSecret}}},
		{name: "use-in-an-array", why: "this call sent", mocks: func() []*models.Mock {
			m := at(varMock("mock-2", "POST", "http://store/items", `{"items":[{"tok":"`+rec+`"}]}`, 200, `{}`, models.MockVariables{"orderId": useV(rec, []string{"body.items[0].tok"})}), 2)
			m.Noise = noise
			return []*models.Mock{bind(), m}
		}, first: bindLive, live: step{method: "POST", path: "/items", body: `{"items":[{"tok":"` + liveSecret + `"}]}`}},
		{name: "type-changed", why: "another JSON type", mocks: func() []*models.Mock {
			return []*models.Mock{bind()}
		}, live: step{method: "POST", path: "/orders", body: `{"id":` + liveNumber + `,"item":"pen"}`}},
		{name: "another-variables-value", why: "which is variable orderId's value", mocks: func() []*models.Mock {
			m := at(varMock("mock-2", "POST", "http://store/carts", `{"cart":"REDACTED_def"}`, 201, `{}`, models.MockVariables{"cartId": bindV("REDACTED_def", "body.cart")}), 2)
			m.Noise = noise
			return []*models.Mock{bind(), m}
		}, first: bindLive, live: step{method: "POST", path: "/carts", body: `{"cart":"` + liveBound + `"}`}},
		{name: "a-given-not-passed-on", why: "the application did not pass on the value the test gave", mocks: func() []*models.Mock {
			m := at(varMock("mock-1", "POST", "http://store/apps", `{"name":"`+rec+`"}`, 201, `{}`, models.MockVariables{"given:app": useV(rec, []string{"body.name"})}), 1)
			m.Noise = noise
			return []*models.Mock{m}
		}, give: &variables.Binding{Name: "given:app", Value: liveBound, Recorded: rec, Mock: "test o::TestApps", Place: "apps_test.go:20"},
			live: step{method: "POST", path: "/apps", body: `{"name":"` + liveSecret + `"}`}},
		// One of the mock's patterns matches the whole recorded note.
		{name: "a-worded-text", why: "A word is read where the rest of that text places it", mocks: func() []*models.Mock {
			ms := wordedCreate()
			ms[0].Noise = []string{`appRec7`}
			return ms
		}, live: step{method: "PUT", path: "/secrets/app", body: `{"data":{"note":"made by ` + liveSecret + ` at noon"}}`}, recorded: "appRec7"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHTTP()
			db := &varDb{perTest: c.mocks()}
			if c.give != nil {
				db.table.Start(nil)
				if _, ok := db.table.Give(*c.give); !ok {
					t.Fatal("the test's given did not bind")
				}
			}
			if c.first != nil {
				if ok, _, _ := matchVar(t, h, db, c.first); !ok {
					t.Fatal("the binding call should match")
				}
			}
			in := liveReq(c.live.method, c.live.path, c.live.body)
			for k, v := range c.live.hdr {
				in.header.Set(k, v)
			}
			ok, _, diag := matchVar(t, h, db, in)
			wantProblem(t, ok, diag, c.why)
			request, _ := http.NewRequest(c.live.method, "http://store"+c.live.path, strings.NewReader(c.live.body))
			request.Header = in.header.Clone()
			r := h.buildHTTPMismatchReport(request, []byte(c.live.body), db, nil, nil, nil, diag)
			noSecret(t, r, false)
			if r != nil && (r.MatchPhase != models.MatchPhaseVariable || !strings.Contains(r.Diff, valueRedacted)) {
				t.Errorf("the variable miss does not show the place redacted: phase %s Diff %q", r.MatchPhase, r.Diff)
			}
			recorded := c.recorded
			if recorded == "" {
				recorded = "REDACTED_"
			}
			if r != nil && strings.Contains(r.Diff, recorded) {
				t.Errorf("the variable miss quotes the recording's value %q at the redacted place: Diff %q", recorded, r.Diff)
			}
		})
	}
}

// A variable miss keeps the evidence the recording did not redact: the
// values at a place it kept, and evidence that shows no value.
func TestVariableMissKeepsWhatTheRecordingDidNotRedact(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"ord-live-9","item":"pen"}`)); !ok {
		t.Fatal("the create should match")
	}
	ok, _, diag := matchVar(t, h, db, liveReq("GET", "/orders/ord-other-3", ""))
	p := wantProblem(t, ok, diag, "this call sent")
	r := h.buildHTTPMismatchReport(makeReq("GET", "/orders/ord-other-3"), nil, db, nil, nil, nil, diag)
	if r == nil || r.Diff != p.diff() || !strings.Contains(r.Diff, `"ord-live-9"`) || !strings.Contains(r.Diff, `"ord-other-3"`) {
		t.Fatalf("the report's Diff is not the problem's evidence, its values shown: %+v", r)
	}
	db = &varDb{perTest: createAndReadBack("ord-rec-1")[1:]}
	ok, _, diag = matchVar(t, h, db, liveReq("GET", "/orders/ord-live-9", ""))
	p = wantProblem(t, ok, diag, "before the call that binds it")
	r = h.buildHTTPMismatchReport(makeReq("GET", "/orders/ord-live-9"), nil, db, nil, nil, nil, diag)
	if r == nil || r.Diff != p.diff() {
		t.Fatalf("the report's Diff is not the problem's evidence: %+v", r)
	}
}

// loggedNone fails t when an entry logs carries one of secrets, in its
// message or a field.
func loggedNone(t *testing.T, logs *observer.ObservedLogs, secrets ...string) {
	t.Helper()
	for _, e := range logs.All() {
		for _, s := range secrets {
			if strings.Contains(e.Message, s) {
				t.Errorf("log %q carries %q", e.Message, s)
			}
			for k, v := range e.ContextMap() {
				if strings.Contains(fmt.Sprint(v), s) {
					t.Errorf("log %q field %s carries %q: %v", e.Message, k, s, v)
				}
			}
		}
	}
}

// The miss's error, which the proxy logs at Error with the switch off,
// names the call as its report does, the segment the recording redacted
// redacted, and no log line of the call carries the secret: the debug lines
// made where the recording is at hand show it redacted, the others the
// path's shape. Switch off and on; the secret's segment holds a %2F too.
func TestMissErrorAndLogsNameTheCallRedacted(t *testing.T) {
	for _, live := range []string{liveSecret, "zq9Plm%2F3Xw7Rt"} {
		for _, on := range []bool{false, true} {
			for _, pool := range []string{"", "/with-a-candidate-that-redacted-nothing"} {
				mode, env := "off", ""
				if on {
					mode, env = "on", "1"
				}
				t.Run(live+"/"+mode+pool, func(t *testing.T) {
					t.Setenv(provenswitch.Env, env)
					m := recorded("m", "/x/REDACTED_abc/y", map[string]string{"Content-Type": "application/json"}, `{"x":"alpha1"}`)
					m.Noise = []string{`^REDACTED_abc$`}
					mocks := []*models.Mock{m}
					if pool != "" {
						// Each candidate's debug line names the call redacted
						// by the pool's places, not by the candidate's own.
						mocks = []*models.Mock{recorded("n", "/x/q9P/y", map[string]string{"Content-Type": "application/json"}, `{"x":"alpha1"}`), m}
					}
					app, proxy := tcpPair(t)
					t.Cleanup(func() { _ = app.Close(); _ = proxy.Close() })
					core, logs := observer.New(zap.DebugLevel)
					h := &HTTP{Logger: zap.New(core)}
					var reported *models.MockMismatchReport
					ctx := models.WithMockMismatchReporter(context.Background(), func(err error) {
						if e, ok := err.(interface {
							MismatchReport() *models.MockMismatchReport
						}); ok {
							reported = e.MismatchReport()
						}
					})
					body := `{"x":"bravo2"}`
					raw := "POST /x/" + live + "/y?k=v HTTP/1.1\r\nHost: api.example\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
					done := make(chan error, 1)
					go func() {
						done <- h.decodeHTTP(ctx, []byte(raw), proxy, &models.ConditionalDstCfg{Addr: "127.0.0.1:1"}, &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}, models.OutgoingOptions{})
					}()
					go func() { _, _ = io.Copy(io.Discard, app) }()
					err := awaitDone(t, done, "decodeHTTP returning")
					if reported == nil {
						t.Fatal("the miss was not reported")
					}
					want := "POST /x/" + valueRedacted + "/y"
					if reported.ActualSummary != want {
						t.Errorf("the report names the call %q, want %q", reported.ActualSummary, want)
					}
					// With the switch on the miss is reported, not returned.
					if !on && (err == nil || !strings.Contains(err.Error(), "for "+want)) {
						t.Errorf("decodeHTTP = %v, want the miss's error naming the call %q", err, want)
					}
					if err != nil && (strings.Contains(err.Error(), "zq9Plm") || strings.Contains(err.Error(), "3Xw7Rt")) {
						t.Errorf("the miss's error carries the secret: %v", err)
					}
					loggedNone(t, logs, "zq9Plm", "3Xw7Rt")
					if pool != "" && !on && logs.FilterMessage("The url path of mock and request aren't the same").FilterField(zap.String("mock name", "n")).Len() == 0 {
						t.Error("the row is of a debug line at the candidate that redacted nothing: none was written")
					}
				})
			}
		}
	}
}

// The "variable bound" debug line shows a value bound at a place the
// recording redacted redacted, and one bound elsewhere as it is. A place
// whose name ends in #<digits> ("body.ref#2") is read as the check read it,
// the whole name where the recording holds it, not a word of "body.ref".
func TestVariableBoundLogRedactsAValueAtARedactedPlace(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const rec, recRef, liveRef = "REDACTED_abc", "REDACTED_def", "Rf7Lq2Zx9Wn"
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"`+rec+`","item":"pen","ref#2":"`+recRef+`"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV(rec, "body.id"), "item": bindV("pen", "body.item"), "ref": bindV(recRef, "body.ref#2")}), 1)
	m.Noise = []string{`^REDACTED_(abc|def)$`}
	core, logs := observer.New(zap.DebugLevel)
	h := newHTTP()
	h.Logger = zap.New(core)
	db := &varDb{perTest: []*models.Mock{m}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"`+liveBound+`","item":"pencil","ref#2":"`+liveRef+`"}`)); !ok {
		t.Fatal("the binding call should match")
	}
	values := map[string]string{}
	for _, e := range logs.FilterMessage("variable bound").All() {
		v, _ := e.ContextMap()["variable"].(string)
		values[v], _ = e.ContextMap()["value"].(string)
	}
	if values["orderId"] != valueRedacted || values["ref"] != valueRedacted || values["item"] != "pencil" {
		t.Errorf("logged the bound values as %v, want orderId and ref redacted and item as it is", values)
	}
	loggedNone(t, logs, liveBound, liveRef)
}

// A variable used at a path segment the recording redacted, after a
// segment that holds a %2F: the place counts the escaped path, so the miss
// shows that segment redacted, in its summary and in its Diff.
func TestVariableMissAfterAnEscapedSlashRedactsTheSegment(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const rec = "REDACTED_abc"
	noise := []string{`^REDACTED_(abc|def)$`}
	m1 := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"`+rec+`","item":"pen"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV(rec, "body.id")}), 1)
	m1.Noise = noise
	m2 := at(varMock("mock-2", "GET", "http://store/a%2Fb/"+rec, "", 200, `{}`, models.MockVariables{"orderId": useV(rec, []string{"url.path[1]"})}), 2)
	m2.Noise = noise
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{m1, m2}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"`+liveBound+`","item":"pen"}`)); !ok {
		t.Fatal("the binding call should match")
	}
	in := liveReq("GET", "/a%2Fb/"+liveSecret, "")
	ok, _, diag := matchVar(t, h, db, in)
	if ok || diag == nil || diag.variable == nil {
		t.Fatalf("want a variable miss, got served %v, %+v", ok, diag)
	}
	request, _ := http.NewRequest("GET", "http://store/a%2Fb/"+liveSecret, nil)
	request.Header = in.header.Clone()
	r := h.buildHTTPMismatchReport(request, nil, db, nil, nil, nil, diag)
	noSecret(t, r, true)
	if want := "GET /a%2Fb/" + valueRedacted; r != nil && r.ActualSummary != want {
		t.Errorf("the miss names the call %q, want %q", r.ActualSummary, want)
	}
	if r != nil && !strings.Contains(r.Diff, valueRedacted) {
		t.Errorf("Diff does not show the segment redacted: %s", r.Diff)
	}
}

// A paired call with an empty path (a request line in absolute form with no
// path) is named by the path "/", and an empty segment, which holds no value,
// is not written redacted.
func TestAPairedCallWithAnEmptyPathIsNamedSlash(t *testing.T) {
	var got []*models.MockMismatchReport
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) {
		var withReport interface {
			MismatchReport() *models.MockMismatchReport
		}
		if errors.As(err, &withReport) {
			got = append(got, withReport.MismatchReport())
		}
	})
	m := recorded("m", "/REDACTED_abc", nil, "")
	m.Noise = []string{`^REDACTED_abc$`}
	in := &req{method: "GET", url: &url.URL{}, header: http.Header{"Host": {"api.example"}}}
	reportPairing(ctx, in, m, integrations.AnsweredCall{}, false)
	if len(got) != 1 || got[0].ActualSummary != "GET /" {
		t.Fatalf("reports %+v", got)
	}
}

// A miss report is redacted by the places of every recording of the pool it
// was compared with, not only by its closest recording's: a recording the
// report names as the closest (b, nearer by its path) that redacted nothing
// does not show the secret another recording of the call (a) hid. Switch
// off and on, and switch off at recordings that state their lifetime, whose
// report tells how to serve the call to every test.
func TestAMissIsRedactedByEveryRecordingOfThePool(t *testing.T) {
	const jsonCT = "application/json"
	value := []string{`^REDACTED_abc$`}
	jsonHdr := map[string]string{"Content-Type": jsonCT}
	text := map[string]string{"Content-Type": "text/plain", "X-Other": "alpha1"}
	type pooled struct {
		name               string
		aURL, bURL, live   string
		aHdr, bHdr, inHdr  map[string]string
		aBody, bBody, body string
	}
	with := func(h map[string]string, k, v string) map[string]string {
		out := map[string]string{k: v}
		for hk, hv := range h {
			out[hk] = hv
		}
		return out
	}
	cases := []pooled{
		{name: "segment", aURL: "/x/REDACTED_abc/y", bURL: "/x/q9P/y", live: "/x/" + liveSecret + "/y", aHdr: jsonHdr, bHdr: jsonHdr, inHdr: jsonHdr, aBody: `{"x":"alpha1"}`, bBody: `{"x":"alpha1"}`, body: `{"x":"bravo2"}`},
		{name: "query", aURL: "/x/aa?sig=REDACTED_abc", bURL: "/x", live: "/x?sig=" + liveSecret, aHdr: jsonHdr, bHdr: jsonHdr, inHdr: jsonHdr, aBody: `{"x":"alpha1"}`, bBody: `{"x":"alpha1"}`, body: `{"x":"bravo2"}`},
		{name: "header", aURL: "/x/aa", bURL: "/x", live: "/x", aHdr: with(jsonHdr, "X-Sig", "REDACTED_abc"), bHdr: jsonHdr, inHdr: with(jsonHdr, "X-Sig", liveSecret), aBody: `{"x":"alpha1"}`, bBody: `{"x":"alpha1"}`, body: `{"x":"bravo2"}`},
		{name: "json-field", aURL: "/x/aa", bURL: "/x", live: "/x", aHdr: jsonHdr, bHdr: jsonHdr, inHdr: jsonHdr, aBody: `{"assertion":"REDACTED_abc","x":"alpha1"}`, bBody: `{"x":"alpha1"}`, body: `{"assertion":"` + liveSecret + `","x":"bravo2"}`},
		{name: "json-moved-field", aURL: "/x/aa", bURL: "/x", live: "/x", aHdr: jsonHdr, bHdr: jsonHdr, inHdr: jsonHdr, aBody: `{"assertion":"REDACTED_abc","x":"alpha1"}`, bBody: `{"x":"alpha1"}`, body: `{"data":{"assertion":"` + liveSecret + `"},"x":"bravo2"}`},
		// A root array's elements are a field with no key ("[]"): found by
		// the field's name alone.
		{name: "json-root-array", aURL: "/x/aa", bURL: "/x", live: "/x", aHdr: jsonHdr, bHdr: jsonHdr, inHdr: with(jsonHdr, "X-Other", "bravo2"), aBody: `["REDACTED_abc","alpha1"]`, bBody: `["q9P","alpha1"]`, body: `["` + liveSecret + `","bravo2"]`},
		{name: "text-body", aURL: "/x/aa", bURL: "/x/otherz", live: "/x/other", aHdr: text, bHdr: text, inHdr: map[string]string{"Content-Type": "text/plain", "X-Other": "bravo2"}, aBody: `token REDACTED_abc`, bBody: `hello`, body: `token ` + liveSecret},
		{name: "url-does-not-parse", aURL: "/x/%zz/REDACTED_abc", bURL: "/x/zz/q9P", live: "/x/zz/" + liveSecret, aHdr: jsonHdr, bHdr: jsonHdr, inHdr: jsonHdr, aBody: `{"x":"alpha1"}`, bBody: `{"x":"alpha1"}`, body: `{"x":"bravo2"}`},
	}
	for _, c := range cases {
		for _, mode := range []string{"off", "off-stated-lifetime", "on"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				a := recorded("a", c.aURL, c.aHdr, c.aBody)
				a.Noise = value
				b := recorded("b", c.bURL, c.bHdr, c.bBody)
				// A third recording, far from the call, after the one that
				// redacted: each recording of the pool is read, not the
				// first or the last alone.
				far := recorded("far", "/far/away/from/it", c.bHdr, c.bBody)
				if mode == "off-stated-lifetime" {
					for _, m := range []*models.Mock{a, b, far} {
						m.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
					}
				}
				in := sent(c.live, c.inHdr, c.body)
				request, _ := http.NewRequest("POST", "http://api.example"+c.live, strings.NewReader(c.body))
				request.Header = in.header.Clone()
				var diag *matchDiag
				if mode == "on" {
					got, d := strictMatch(t, in, nil, nil, nil, b, a, far)
					if got != nil {
						t.Fatal("served a call that differs in x")
					}
					diag = d
				} else {
					t.Setenv(provenswitch.Env, "")
					ok, _, d, err := newHTTP().match(context.Background(), in, &mockMemDb{mocks: []*models.Mock{b, a, far}, updateUnFilteredReturn: true}, nil, nil, nil, true, false, true, true, true)
					if err != nil {
						t.Fatal(err)
					}
					if ok {
						t.Fatal("served a call that differs in x")
					}
					diag = d
				}
				r := newHTTP().buildHTTPMismatchReport(request, []byte(c.body), &mockMemDb{mocks: []*models.Mock{b, a, far}}, nil, nil, nil, diag)
				if r == nil {
					t.Fatal("no report")
				}
				// The row is of a report that names the recording that
				// redacted nothing.
				if mode != "on" && r.ClosestMock != "b" {
					t.Fatalf("the report names %q, not the nearer recording b", r.ClosestMock)
				}
				if strings.Contains(persisted(t, r), liveSecret) {
					t.Errorf("the report at closest %q carries the live value at a place recording a redacted:\nsummary: %s\nDiff: %s\nReceivedReq:\n%s\nnext: %s", r.ClosestMock, r.ActualSummary, r.Diff, r.ReceivedReq, r.NextSteps)
				}
				// The rest of the call still shows, and the path diff names
				// the recorded path, but where a recorded URL that does not
				// parse leaves no place of the path to find.
				if !strings.Contains(r.ReceivedReq, "bravo2") {
					t.Errorf("ReceivedReq lost what no recording redacted:\n%s", r.ReceivedReq)
				}
				for _, d := range r.FieldDiffs {
					if d.Path == "path" && c.name != "url-does-not-parse" && !strings.HasPrefix(d.Expected, "/x/") {
						t.Errorf("the path diff does not name the recorded path: %+v", d)
					}
				}
				if mode == "off-stated-lifetime" {
					sessionHintMatchesTheCall(t, r, request)
				}
				if mode == "on-stated-lifetime" {
					sessionHintNamesTheCallExactly(t, r, request, strings.HasPrefix(c.name, "path-segment") || c.name == "url-does-not-parse")
				}
			})
		}
	}
}

// A miss whose stateful walk has answered each recording of the call is
// named by the last, and redacted by every recording of the pool: the last
// (b) redacted nothing, and another recording of the call (a) redacted the
// segment the call sent its secret in, in the pool or, having answered a
// call before, on the set's answered list only.
func TestAnAnsweredMissIsRedactedByEveryRecordingOfThePool(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	const live = "/x/" + liveSecret + "/y"
	for _, mode := range []string{"off", "off-stated-lifetime", "on", "off-read-again-empty", "off-a-answered-before"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[string]string{"on": "1"}[mode])
			a := recorded("a", "/x/REDACTED_abc/y", jsonHdr, `{"x":"alpha1"}`)
			a.Noise = []string{`^REDACTED_abc$`}
			b := recorded("b", "/x/q9P/y", jsonHdr, `{"x":"alpha1"}`)
			if mode == "off-stated-lifetime" {
				for _, m := range []*models.Mock{a, b} {
					m.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
				}
			}
			i, _, answered := servedWalk(nil, []*models.Mock{b}, integrations.SequenceView{Cursor: &mockMemDb{}})
			if i != -1 || answered == nil || answered.last != b.Name {
				t.Fatalf("the walk answered %d (%+v); want the miss naming b", i, answered)
			}
			request, _ := http.NewRequest("POST", "http://api.example"+live, strings.NewReader(`{"x":"bravo2"}`))
			request.Header.Set("Content-Type", "application/json")
			diag := &matchDiag{phase: models.MatchPhaseExhausted, candidates: 2, pool: []*models.Mock{b, a}, answered: answered}
			var db integrations.MockMemDb = &mockMemDb{mocks: []*models.Mock{b, a}}
			switch mode {
			case "off-read-again-empty":
				// The db read again for the report holds none of the pool
				// the walk was compared with (another connection consumed
				// them since): the pool's places still redact it.
				db = &mockMemDb{}
			case "off-a-answered-before":
				// a answered a call before and is in no read: only the
				// set's answered list holds it.
				diag.pool = []*models.Mock{b}
				db = answeredStore{mockMemDb: &mockMemDb{mocks: []*models.Mock{b}}, answered: []*models.Mock{a}, complete: true}
			}
			r := newHTTP().buildHTTPMismatchReport(request, []byte(`{"x":"bravo2"}`), db, nil, nil, nil, diag)
			if r == nil || r.ClosestMock != "b" {
				t.Fatalf("the report %+v does not name the walk's last recording b", r)
			}
			all := persisted(t, r)
			if strings.Contains(all, liveSecret) {
				t.Errorf("the report carries the live value at a place recording a redacted: %s", all)
			}
			if want := "POST /x/" + valueRedacted + "/y"; r.ActualSummary != want {
				t.Errorf("summary %q, want %q", r.ActualSummary, want)
			}
			if mode == "off-stated-lifetime" {
				sessionHintMatchesTheCall(t, r, request)
			}
		})
	}
}

// reusableReadFails is a mock db whose reusable mocks (startup and session
// scoped) cannot be read, while the test's own mocks can.
type reusableReadFails struct{ *mockMemDb }

func (reusableReadFails) GetSessionMocks() ([]*models.Mock, error) {
	return nil, errors.New("read failed")
}

// A miss whose pool cannot be read names the call's path by its shape only,
// in its summary and in the error the proxy logs at ERROR (missedCall):
// keploy cannot tell there which segment holds a secret. A miss with no
// closest recording is named redacted by the places of every recording of
// its pool: a variable's recording that left the pool, and under the proven
// switch a test variable's route place (withRoutePlaces).
func TestAMissWithNoClosestRecordingNamesNoSecret(t *testing.T) {
	const live = "/x/" + liveSecret + "/y"
	newReq := func() *http.Request {
		r, _ := http.NewRequest("POST", "http://api.example"+live+"?q="+liveSecret, strings.NewReader(`{}`))
		return r
	}
	check := func(t *testing.T, r *models.MockMismatchReport, request *http.Request, summary string) {
		t.Helper()
		if r == nil {
			t.Fatal("no report")
		}
		all := persisted(t, r)
		if r.ActualSummary != summary || strings.Contains(all, liveSecret) {
			t.Errorf("summary %q, want %q, and no secret in the report: %s", r.ActualSummary, summary, all)
		}
		if got := missedCall(request, r); got != summary {
			t.Errorf("the error names %q, want %q", got, summary)
		}
	}
	// The recording of the pool that redacted the call's segment 1.
	other := recorded("other", "/x/REDACTED_abc/y", map[string]string{"Content-Type": "application/json"}, `{}`)
	other.Noise = []string{`^REDACTED_abc$`}
	for _, mode := range []string{"off", "on"} {
		t.Run("the-pool-cannot-be-read/"+mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[string]string{"off": "", "on": "1"}[mode])
			request := newReq()
			r := newHTTP().buildHTTPMismatchReport(request, []byte(`{}`), &mockMemDb{err: errors.New("read failed")}, nil, nil, nil, nil)
			want := "POST /{}/{}/{}"
			if mode == "on" && len(r.VariablePlaces) > 0 {
				want = "POST /" + valueRedacted
			}
			check(t, r, request, want)
		})
		// The pool's mocks are read in two parts: the reusable ones too.
		t.Run("the-reusable-mocks-cannot-be-read/"+mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[string]string{"off": "", "on": "1"}[mode])
			request := newReq()
			r := newHTTP().buildHTTPMismatchReport(request, []byte(`{}`), reusableReadFails{&mockMemDb{mocks: []*models.Mock{other}}}, nil, nil, nil, nil)
			if !strings.Contains(r.NextSteps, "Failed to read mock database") {
				t.Fatalf("the row is of a pool that cannot be read: %+v", r)
			}
			check(t, r, request, "POST /{}/{}/{}")
		})
		// A recording that states its mocks' lifetimes is told how to serve
		// the call to every test: the hint names no segment of a pool that
		// cannot be read.
		if mode == "off" {
			t.Run("the-pool-cannot-be-read/off-stated-lifetime", func(t *testing.T) {
				t.Setenv(provenswitch.Env, "")
				request := newReq()
				r := newHTTP().buildHTTPMismatchReport(request, []byte(`{}`), statedDb{MockMemDb: &mockMemDb{err: errors.New("read failed")}, states: true}, nil, nil, nil, nil)
				check(t, r, request, "POST /{}/{}/{}")
				sessionHintMatchesTheCall(t, r, request)
			})
		}
		// The db read again for the report holds none of the pool the
		// matcher compared (another connection consumed them since): the
		// miss names no closest recording, and the pool's places redact it.
		t.Run("the-pool-read-again-is-empty/"+mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[string]string{"off": "", "on": "1"}[mode])
			request := newReq()
			diag := &matchDiag{phase: models.MatchPhaseExhausted, candidates: 1, pool: []*models.Mock{other}}
			r := newHTTP().buildHTTPMismatchReport(request, []byte(`{}`), &mockMemDb{}, nil, nil, nil, diag)
			if r.ClosestMock != "" {
				t.Fatalf("the row is of a miss with no closest recording, not %q", r.ClosestMock)
			}
			check(t, r, request, "POST /x/"+valueRedacted+"/y")
		})
		// The recording that redacted the segment answered a call before
		// and is in no read: the set's answered list (heldPlaces) redacts
		// a miss that names no closest recording, of a pool that redacted
		// nothing.
		t.Run("the-redacting-recording-answered-before/"+mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[string]string{"off": "", "on": "1"}[mode])
			request := newReq()
			kept := recorded("kept", "/x/plain/y", map[string]string{"Content-Type": "application/json"}, `{}`)
			diag := &matchDiag{phase: models.MatchPhaseExhausted, candidates: 1, pool: []*models.Mock{kept}}
			db := answeredStore{mockMemDb: &mockMemDb{}, answered: []*models.Mock{other}, complete: true}
			r := newHTTP().buildHTTPMismatchReport(request, []byte(`{}`), db, nil, nil, nil, diag)
			if r.ClosestMock != "" {
				t.Fatalf("the row is of a miss with no closest recording, not %q", r.ClosestMock)
			}
			check(t, r, request, "POST /x/"+valueRedacted+"/y")
		})
		t.Run("variable-recording-gone/"+mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[string]string{"off": "", "on": "1"}[mode])
			diag := &matchDiag{pool: []*models.Mock{other}, variable: &varProblem{mock: "gone", name: "v", place: "url.path[1]", kind: models.DiffKindVariable, expected: "REDACTED_abc", actual: liveSecret}}
			request := newReq()
			r, _ := variableMismatchReport(request, nil, "api.example", diag, nil, &mockMemDb{})
			check(t, r, request, "POST /x/"+valueRedacted+"/y")
		})
		// The recording that redacted the variable's place answered a
		// call before: only the set's answered list holds it.
		t.Run("variable-recording-answered-before/"+mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[string]string{"off": "", "on": "1"}[mode])
			diag := &matchDiag{variable: &varProblem{mock: "gone", name: "v", place: "url.path[1]", kind: models.DiffKindVariable, expected: "REDACTED_abc", actual: liveSecret}}
			request := newReq()
			r, _ := variableMismatchReport(request, nil, "api.example", diag, nil, answeredStore{mockMemDb: &mockMemDb{}, answered: []*models.Mock{other}, complete: true})
			check(t, r, request, "POST /x/"+valueRedacted+"/y")
		})
	}
	t.Run("route-place-with-no-closest", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		read := recorded("read", "/x/recval/y", map[string]string{"Content-Type": "application/json"}, `{}`)
		read.Spec.Variables = models.MockVariables{"v": {Recorded: "recval", Bind: "url.path[1]"}}
		request := newReq()
		pool := []*models.Mock{read, other}
		r := withRoutePlaces(&models.MockMismatchReport{Protocol: "HTTP", ActualSummary: poolPlaces(nil, pool, request.URL.EscapedPath()).summary(request.Method, request.URL.EscapedPath())}, request, nil, pool, nil, poolPlaces(nil, pool, request.URL.EscapedPath()))
		if len(r.VariablePlaces) == 0 {
			t.Fatal("the row is of a call on the route of a declared path place")
		}
		check(t, r, request, "POST /x/"+valueRedacted+"/y")
	})
}
