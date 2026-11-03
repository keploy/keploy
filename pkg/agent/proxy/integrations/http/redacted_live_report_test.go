package http

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
		{name: "query-in-url-only", url: "/x?assertion=REDACTED_abc", params: map[string]string{}, liveURL: "/x?assertion=" + liveSecret, ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value},
		// A test variable's place on the path makes the report name the
		// path escaped, as a place counts it.
		{name: "path-segment-route-variable", url: "/x/REDACTED_abc/y", liveURL: "/x/" + liveSecret + "/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value, vars: models.MockVariables{"v": {Recorded: "y", Bind: "url.path[2]"}}},
		{name: "path-segment-escaped-route-variable", url: "/x/REDACTED_abc/y", liveURL: "/x/" + liveSecret + "%2B/y", ctype: jsonCT, rec: `{"x":"alpha1"}`, live: `{"x":"bravo2"}`, noise: value, vars: models.MockVariables{"v": {Recorded: "y", Bind: "url.path[2]"}}},
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
// redacted. Switch off and on.
func TestMissReportRedactsTheLiveValueAtARedactedPlace(t *testing.T) {
	for _, c := range redactedLiveCases(t) {
		for _, on := range []bool{false, true} {
			mode := "off"
			if on {
				mode = "on"
			}
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				m := c.mock()
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
				all, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range r.FieldDiffs {
					t.Logf("diff %s %s exp=%q act=%q", d.Path, d.Kind, d.Expected, d.Actual)
				}
				if strings.Contains(string(all), liveSecret) {
					t.Errorf("the report carries the live value at a place the recording redacted:\nDiff: %s\nReceivedReq:\n%s\nreport: %s", r.Diff, r.ReceivedReq, all)
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
				// A query or a form renders the marker query-escaped.
				if !strings.Contains(r.ReceivedReq, valueRedacted) && !strings.Contains(r.ReceivedReq, url.QueryEscape(valueRedacted)) {
					t.Errorf("ReceivedReq does not show the redacted place redacted:\n%s", r.ReceivedReq)
				}
			})
		}
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
