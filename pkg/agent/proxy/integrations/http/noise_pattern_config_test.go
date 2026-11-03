package http

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// requestBodyNoise is keploy.yml's request noise with a requestBody bucket of
// entries, as decode builds it from the options (NewRequestNoise).
func requestBodyNoise(entries map[string][]string) RequestNoise {
	return NewRequestNoise(zap.NewNop(), models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"requestbody": entries}})
}

// keploy.yml's requestBody entry keeps its patterns on the way to mock
// matching: a field it guards may vary only within them, under the strict
// comparison, under strict enforcement without the switch, and in the drift a
// form body's recording learns. Before, every requestBody entry reached the
// matchers with its patterns dropped, and excused any value.
func TestKeployYmlRequestBodyPatternHoldsBothValues(t *testing.T) {
	if got := requestBodyNoise(map[string][]string{"ID": {`^1$`}}).Body; len(got["id"]) != 1 || got["id"][0] != `^1$` {
		t.Fatalf("requestBody noise reaches the matchers as %v; want id with its pattern", got)
	}
	ct := map[string]string{"Content-Type": "application/json"}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	mp := map[string]string{"Content-Type": "multipart/form-data; boundary=XB"}
	part := func(v string) string {
		return "--XB\r\nContent-Disposition: form-data; name=\"id\"\r\n\r\n" + v + "\r\n--XB--\r\n"
	}
	for _, c := range []struct {
		name, pattern string
		header        map[string]string
		recBody, body string
		served        bool
		why           string // the strict comparison's reason, where it refuses
		field         string // the field the strict comparison names
	}{
		{"JSON, the live value outside", `^1$`, ct, `{"id":1}`, `{"id":2}`, false, "live value (a number of 1 characters)", "body.id"},
		{"JSON, the recorded value outside", `^2$`, ct, `{"id":1}`, `{"id":2}`, false, "recorded value (a number of 1 characters)", "body.id"},
		{"JSON, both values inside", `^[12]$`, ct, `{"id":1}`, `{"id":2}`, true, "", ""},
		{"a form, the live value outside", `^1$`, form, `id=1`, `id=2`, false, "live value (a string of 1 bytes)", "body.id"},
		{"a form, the recorded value outside", `^2$`, form, `id=1`, `id=2`, false, "recorded value (a string of 1 bytes)", "body.id"},
		{"a form, a repeated key's later value outside", `^1$`, form, `id=1&id=1`, `id=1&id=2`, false, "live value (a string of 1 bytes)", "body.id"},
		{"a form, the call lacks the key", `^1$`, form, `id=1&x=a`, `x=a`, false, "live value (absent)", "body.id"},
		{"a form, both values inside", `^[12]$`, form, `id=1`, `id=2`, true, "", ""},
		{"a form, both encoded values inside", `^a b$|^a\+b$`, form, `id=a+b`, `id=a%2Bb`, true, "", ""},
		{"a form field no entry names", `^1$`, form, `id=1&x=a`, `id=1&x=b`, false, "", "body.x"},
		{"a multipart field, the live value outside", `^1$`, mp, part("1"), part("2"), false, "live value (a string of 1 bytes)", "body.id"},
		{"a multipart field, the recorded value outside", `^2$`, mp, part("1"), part("2"), false, "recorded value (a string of 1 bytes)", "body.id"},
		{"a multipart field, both values inside", `^[12]$`, mp, part("1"), part("2"), true, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			noise := requestBodyNoise(map[string][]string{"id": {c.pattern}})
			got, diag := strictMatch(t, sent("/x", c.header, c.body), nil, noise.Body, noise.URL, recorded("m", "/x", c.header, c.recBody))
			if (got != nil) != c.served {
				t.Fatalf("served %v (differs %v); want %v", got != nil, differs(diag), c.served)
			}
			if c.served {
				return
			}
			if c.why == "" {
				// A field no entry names is compared, and no pattern is named.
				for _, f := range diag.differs {
					if f.diff.Path == c.field {
						if f.why != "" {
							t.Fatalf("reason %q for a field no entry names", f.why)
						}
						return
					}
				}
				t.Fatalf("differs %v; want %s", differs(diag), c.field)
			}
			why := whyOf(diag, c.field)
			if !strings.Contains(why, c.field+` vary only within "`+c.pattern+`"`) || !strings.Contains(why, c.why) {
				t.Fatalf("reason %q; want %s, its pattern and %q", why, c.field, c.why)
			}
		})
	}
}

// Strict enforcement without the switch (the request-body noise engine)
// reads the same entry: the live value outside its pattern turns the
// recording away, both inside serve it.
func TestKeployYmlRequestBodyPatternUnderStrictEnforcement(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	for _, c := range []struct {
		pattern string
		served  bool
	}{
		{`^1$`, false},
		{`^2$`, false},
		{`^[12]$`, true},
	} {
		noise := requestBodyNoise(map[string][]string{"id": {c.pattern}})
		db := &mockMemDb{mocks: []*models.Mock{recorded("m", "/x", ct, `{"id":1,"name":"a"}`)}, updateUnFilteredReturn: true}
		ok, _, _, err := newHTTP().match(context.Background(), sent("/x", ct, `{"id":2,"name":"a"}`), db, noise.Header, noise.Body, noise.URL, true, false, true, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if ok != c.served {
			t.Errorf("under id %q: served %v, want %v", c.pattern, ok, c.served)
		}
	}
}

// The drift a form body's recording is checked and learned with reads
// keploy.yml's pattern on both values: a key whose values both match is
// excused; one with a value outside, or one the call lacks, drifts.
func TestKeployYmlFormPatternDrift(t *testing.T) {
	eng := mocknoise.New(httpNoiseAdapter{}, true, true)
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for _, c := range []struct {
		name    string
		noise   map[string][]string
		rec     string
		live    string
		drifted bool
	}{
		{"both values inside", map[string][]string{"id": {`^[12]$`}}, "id=1&x=a", "id=2&x=a", false},
		{"both decoded values inside", map[string][]string{"id": {`^a b$`}}, "id=a+b&x=a", "id=a%20b&x=a", false},
		{"the live value outside", map[string][]string{"id": {`^1$`}}, "id=1&x=a", "id=2&x=a", true},
		{"the recorded value outside", map[string][]string{"id": {`^2$`}}, "id=1&x=a", "id=2&x=a", true},
		{"a repeated key's later value outside", map[string][]string{"id": {`^1$`}}, "id=1&id=1&x=a", "id=1&id=2&x=a", true},
		{"the call lacks the key", map[string][]string{"id": {`^1$`}}, "id=1&x=a", "x=a", true},
		{"an entry with no patterns", map[string][]string{"id": {}}, "id=1&x=a", "x=a", false},
		{"equal values outside the pattern", map[string][]string{"id": {`^1$`}}, "id=7&x=a", "id=7&x=a", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := recorded("m", "/x", form, c.rec)
			drift, comparable := eng.Detect(m, []byte(c.live), requestBodyNoise(c.noise).Body)
			if !comparable {
				t.Fatal("a form body is comparable")
			}
			if _, got := drift["body.id"]; got != c.drifted {
				t.Fatalf("drift %v; body.id drifted = %v, want %v", drift, got, c.drifted)
			}
			allowed, _ := eng.StrictReject(m, []byte(c.live), requestBodyNoise(c.noise).Body)
			if allowed == c.drifted {
				t.Fatalf("strict enforcement allowed = %v, want %v", allowed, !c.drifted)
			}
		})
	}
}

// keploy.yml's pattern on a header the static flaky list also names holds
// both its values in the learn step, as in the strict comparison: the user's
// entry wins over the list's, which would excuse any value.
func TestAUserHeaderPatternWinsOverTheFlakyList(t *testing.T) {
	h, _, _ := learnOn(t)
	opts := models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{"header": {"X-Goog-Request-Params": {`^name=projects/p\d$`}}}}
	noise := NewRequestNoise(zap.NewNop(), opts)
	n := h.newLearnNoise(opts, noise.Body, noise.URL)
	with := func(v string) map[string]string {
		return map[string]string{"Content-Type": "application/json", "X-Goog-Request-Params": v}
	}
	for _, c := range []struct {
		name, rec, live string
		same            bool
	}{
		{"both values inside", "name=projects/p1", "name=projects/p2", true},
		{"the live value outside", "name=projects/p1", "name=projects/OTHER", false},
		{"the recorded value outside", "name=projects/OTHER", "name=projects/p1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := learnRecorded("m", 0, "POST", "/v1/x", `{}`, with(c.rec), `{}`)
			in := live("POST", "/v1/x", `{}`, with(c.live))
			d := learn.Compare(n.shape(liveParts(context.Background(), in), ""), n.shape(recordedParts(m), ""), n.known(m))
			if same := d.Structural == "" && len(d.Drift) == 0; same != c.same {
				t.Fatalf("learn step: same = %v (%+v), want %v", same, d, c.same)
			}
			got, diag := strictMatch(t, sent("/x", with(c.live), `{}`), noise.Header, noise.Body, noise.URL, recorded("m", "/x", with(c.rec), `{}`))
			if (got != nil) != c.same {
				t.Fatalf("strict: served %v (differs %v), want %v", got != nil, differs(diag), c.same)
			}
		})
	}
	// Without the user's entry the list still excuses any value.
	plain := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, nil)
	m := learnRecorded("m", 0, "POST", "/v1/x", `{}`, with("name=projects/p1"), `{}`)
	in := live("POST", "/v1/x", `{}`, with("name=projects/OTHER"))
	if d := learn.Compare(plain.shape(liveParts(context.Background(), in), ""), plain.shape(recordedParts(m), ""), plain.known(m)); d.Structural != "" || len(d.Drift) > 0 {
		t.Fatalf("the flaky list no longer excuses the header: %+v", d)
	}
}

// A body field one side lacks is a change under keploy.yml's requestBody
// entry with patterns, even when the side that holds it matches one: only an
// entry with no patterns excuses a field one side lacks.
func TestLearnRequestBodyPatternOnAFieldOneSideLacks(t *testing.T) {
	h, _, _ := learnOn(t)
	for _, c := range []struct {
		name, recBody, body string
		noise               map[string][]string
		same                bool
	}{
		{"only the call holds it, a value the pattern matches", `{"id":1}`, `{"id":1,"extra":2}`, map[string][]string{"extra": {`^2$`}}, false},
		{"only the recording holds it, a value the pattern matches", `{"id":1,"extra":2}`, `{"id":1}`, map[string][]string{"extra": {`^2$`}}, false},
		{"only the call holds it, an entry with no patterns", `{"id":1}`, `{"id":1,"extra":2}`, map[string][]string{"extra": {}}, true},
		{"both hold values the pattern matches", `{"id":1,"extra":3}`, `{"id":1,"extra":2}`, map[string][]string{"extra": {`^[23]$`}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			opts := models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{"requestbody": c.noise}}
			noise := NewRequestNoise(zap.NewNop(), opts)
			n := h.newLearnNoise(opts, noise.Body, noise.URL)
			m := learnRecorded("m", 0, "POST", "/v1/x", c.recBody, nil, `{}`)
			in := live("POST", "/v1/x", c.body, nil)
			d := learn.Compare(n.shape(liveParts(context.Background(), in), ""), n.shape(recordedParts(m), ""), n.known(m))
			if same := d.Structural == "" && len(d.Drift) == 0; same != c.same {
				t.Fatalf("same = %v (%+v), want %v", same, d, c.same)
			}
		})
	}
}

// A repeated request header is held to keploy.yml's pattern as a whole: a
// later value outside it is a change, though the first one matches.
func TestStrictRepeatedHeaderUnderAPattern(t *testing.T) {
	noise := NewRequestNoise(zap.NewNop(), models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`}}}})
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/x", map[string]string{"Content-Type": "application/json", "X-Id": "a-1"}, `{}`)
	in := sent("/x", ct, `{}`)
	in.header.Add("X-Id", "a-1")
	in.header.Add("X-Id", "evil")
	got, diag := strictMatch(t, in, noise.Header, noise.Body, noise.URL, rec)
	if got != nil || !strings.Contains(whyOf(diag, "header.x-id"), "live value") {
		t.Fatalf("served %v, reason %q; want the repeated header's live value outside the pattern named", got != nil, whyOf(diag, "header.x-id"))
	}
}

// A scalar replaced by a container is a change under a pattern, even one an
// empty value matches: a container holds no value for the pattern to match.
func TestStrictBodyPatternRetype(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	noise := requestBodyNoise(map[string][]string{"id": {`^(\d+)?$`}})
	for _, c := range []struct{ rec, live string }{
		{`{"id":1}`, `{"id":{"a":1}}`},
		{`{"id":{"a":1}}`, `{"id":1}`},
	} {
		if got, diag := strictMatch(t, sent("/x", ct, c.live), nil, noise.Body, noise.URL, recorded("m", "/x", ct, c.rec)); got != nil || !hasField(differs(diag), "body.id") {
			t.Fatalf("%s -> %s: served %v, differs %v; want body.id named", c.rec, c.live, got != nil, differs(diag))
		}
	}
}
