package http

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/pkg/models"
)

// noiseEntriesIn is the keploy.yml flow mapping a report's next step names
// after marker ("test.globalNoise.global.url: "), read as YAML reads it: the
// entries a user copies into keploy.yml.
func noiseEntriesIn(t *testing.T, next, marker string) map[string][]string {
	t.Helper()
	i := strings.Index(next, marker)
	if i < 0 {
		t.Fatalf("the next step %q does not name %q", next, marker)
	}
	rest := next[i+len(marker):]
	end := strings.Index(rest, "}")
	if !strings.HasPrefix(rest, "{") || end < 0 {
		t.Fatalf("the next step %q names no YAML mapping after %q", next, marker)
	}
	var entries map[string][]string
	if err := yamlLib.Unmarshal([]byte(rest[:end+1]), &entries); err != nil || len(entries) == 0 {
		t.Fatalf("the next step's %q is no YAML mapping of noise entries: %v", rest[:end+1], err)
	}
	return entries
}

// searchRecordings records each of bodies as POST /search, answered
// {"hit":<i>} for the ith (from 1), named mock-search-<i>.
func searchRecordings(t *testing.T, h *HTTP, learned map[string][]string, bodies ...string) *mockMemDb {
	db := &mockMemDb{updateUnFilteredReturn: true}
	for i, b := range bodies {
		m := recordedAs(t, h, "mock-search-"+strconv.Itoa(i+1), i+1, searchCall(b), jsonAnswer(200, `{"hit":`+strconv.Itoa(i+1)+`}`), nil)
		m.Spec.ReqBodyNoise = learned
		db.mocks = append(db.mocks, m)
	}
	return db
}

// A recording whose body is the call's outside its noise is the call's own,
// in the default noise mode too, and it is served with no report: a field
// keploy.yml declares request-body noise, a field the record learned as
// noise, the same JSON with its keys in another order or other spacing. Of
// several such recordings the call is served the first, with no report, and
// it is served its own over another call's whose body is the closer string. A call
// whose body differs from every recording outside its noise is served the
// closest and reported closest_body, naming the fields it differs at and
// none its noise covers, and
// its next step names both causes with the exact keploy.yml entry; the entry
// it names, read as YAML and given as keploy.yml's noise, makes the
// recording the call's own.
func TestABodyThatIsTheCallsOutsideItsNoiseIsServedAsItsOwn(t *testing.T) {
	ts := map[string]map[string][]string{"requestbody": {"ts": {}}}
	for _, tc := range []struct {
		name     string
		recorded []string
		learned  map[string][]string
		opts     models.OutgoingOptions
		call     string
		want     []string // the hits the call may be served
	}{
		{name: "declared noise, default mode", recorded: []string{`{"q":"ada","ts":1}`, `{"q":"alan","ts":2}`}, opts: models.OutgoingOptions{NoiseConfig: ts}, call: `{"q":"ada","ts":3}`, want: []string{`{"hit":1}`}},
		{name: "declared noise, strict mode", recorded: []string{`{"q":"ada","ts":1}`, `{"q":"alan","ts":2}`}, opts: models.OutgoingOptions{NoiseConfig: ts, MockNoiseStrict: true}, call: `{"q":"ada","ts":3}`, want: []string{`{"hit":1}`}},
		{name: "learned noise", recorded: []string{`{"q":"ada","ts":1}`, `{"q":"alan","ts":2}`}, learned: map[string][]string{"body.ts": {}}, call: `{"q":"ada","ts":3}`, want: []string{`{"hit":1}`}},
		{name: "keys in another order", recorded: []string{`{"q":"ada","page":1}`, `{"q":"alan","page":2}`}, call: `{"page":1,"q":"ada"}`, want: []string{`{"hit":1}`}},
		{name: "other spacing", recorded: []string{`{"q":"ada","page":1}`, `{"q":"alan","page":2}`}, call: `{"q": "ada", "page": 1}`, want: []string{`{"hit":1}`}},
		{name: "several of the call's own", recorded: []string{`{"q":"ada","ts":1}`, `{"q":"ada","ts":2}`, `{"q":"alan","ts":5}`}, opts: models.OutgoingOptions{NoiseConfig: ts}, call: `{"q":"ada","ts":9}`, want: []string{`{"hit":1}`}},
		// The other call's body is the closer string (one letter of q
		// against three digits of ts), so a fuzzy choice over every
		// recording would serve it as the call's own.
		{name: "the call's own over a closer other call", recorded: []string{`{"q":"ada","ts":1700000000}`, `{"q":"adx","ts":1800000099}`}, opts: models.OutgoingOptions{NoiseConfig: ts}, call: `{"q":"ada","ts":1800000099}`, want: []string{`{"hit":1}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			h := newHTTP()
			got := serveReportedWith(t, h, searchRecordings(t, h, tc.learned, tc.recorded...), tc.opts, searchCall(tc.call))
			if got.status != 200 || !slices.Contains(tc.want, got.body) || len(got.answers()) != 0 {
				t.Fatalf("the call got %d %q with answer reports %+v, want one of %q and none", got.status, got.body, got.answers(), tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name, call, closest string
		opts                models.OutgoingOptions
		differs             []string
	}{
		{name: "a value no noise covers", call: `{"q":"ada","ts":3}`, closest: "mock-search-1", differs: []string{"body.ts"}},
		{name: "a real input", call: `{"q":"bob","ts":1}`, closest: "mock-search-1", differs: []string{"body.q"}},
		// ts differs too, and keploy.yml declares it noise: the report
		// names only the field that is a difference.
		{name: "a real input beside declared noise", call: `{"q":"bob","ts":3}`, opts: models.OutgoingOptions{NoiseConfig: ts}, closest: "mock-search-1", differs: []string{"body.q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			h := newHTTP()
			got := serveReportedWith(t, h, searchRecordings(t, h, nil, `{"q":"ada","ts":1}`, `{"q":"alan","ts":2}`), tc.opts, searchCall(tc.call))
			a := got.answers()
			if got.status != 200 || len(a) != 1 || a[0].Answer != models.AnswerClosestBody || a[0].ClosestMock != tc.closest || !slices.Equal(a[0].Differs, tc.differs) {
				t.Fatalf("the call got %d %q with answer reports %+v, want one %q with %s at %q", got.status, got.body, a, models.AnswerClosestBody, tc.closest, tc.differs)
			}
			next := a[0].NextSteps
			if !strings.Contains(next, "If the call is a new input, record it (keploy mock record)") || !strings.Contains(next, "("+strings.Join(tc.differs, ", ")+") change on every run") {
				t.Errorf("the next step %q does not name both causes and the fields", next)
			}
			entries := noiseEntriesIn(t, next, "test.globalNoise.global.requestbody: ")
			// keploy.yml's requestbody noise with the next step's entries
			// added to what it already declares.
			noise := map[string][]string{}
			for _, from := range []map[string][]string{tc.opts.NoiseConfig["requestbody"], entries} {
				for k, v := range from {
					noise[k] = v
				}
			}
			h = newHTTP()
			again := serveReportedWith(t, h, searchRecordings(t, h, nil, `{"q":"ada","ts":1}`, `{"q":"alan","ts":2}`), models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"requestbody": noise}}, searchCall(tc.call))
			if again.status != 200 || again.body != `{"hit":1}` || len(again.answers()) != 0 {
				t.Fatalf("with keploy.yml's requestbody %v, the next step's entries added, the call got %d %q with answer reports %+v, want %s's answer and none", noise, again.status, again.body, again.answers(), tc.closest)
			}
		})
	}
}

// Of several recordings that are all the call's own outside its noise, the
// call is served the first in recorded order on every run, whichever of them
// its noise value of that run is the closer string to: a timestamp that
// changes on every run never changes which recording answers the call. The
// first is served with no report.
func TestOfSeveralOfTheCallsOwnRecordingsTheFirstServesItOnEveryRun(t *testing.T) {
	ts := models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"requestbody": {"ts": {}}}}
	recorded := []string{`{"q":"ada","ts":1000}`, `{"q":"ada","ts":2}`, `{"q":"alan","ts":5}`}
	// The runs' ts: the first is the closer string to the second
	// recording, the second to the first, the third to neither.
	for _, call := range []string{`{"q":"ada","ts":3}`, `{"q":"ada","ts":1001}`, `{"q":"ada","ts":77}`} {
		setProven(t, false)
		h := newHTTP()
		got := serveReportedWith(t, h, searchRecordings(t, h, nil, recorded...), ts, searchCall(call))
		if got.status != 200 || got.body != `{"hit":1}` || len(got.answers()) != 0 {
			t.Fatalf("the call %s got %d %q with answer reports %+v, want the first recording's {\"hit\":1} and none", call, got.status, got.body, got.answers())
		}
	}
}

// A body with no fields to compare (plain text) that no recording has byte
// for byte is served the closest recording and reported closest_body at
// "body", with a next step that only records the call: no noise entry
// could make another body its own.
func TestABodyWithNoFieldsNamesNoNoiseEntry(t *testing.T) {
	setProven(t, false)
	text := func(body string) string {
		return "POST /notes HTTP/1.1\r\nHost: users.local\r\nContent-Type: text/plain\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	h := newHTTP()
	db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
		recordedAs(t, h, "mock-note-1", 1, text("first note"), jsonAnswer(200, `{"note":1}`), nil),
		recordedAs(t, h, "mock-note-2", 2, text("second note"), jsonAnswer(200, `{"note":2}`), nil),
	}}
	got := serveReported(t, h, db, text("first notes"))
	a := got.answers()
	if got.status != 200 || len(a) != 1 || a[0].Answer != models.AnswerClosestBody || !slices.Equal(a[0].Differs, []string{"body"}) {
		t.Fatalf("the call got %d %q with answer reports %+v, want one %q at body", got.status, got.body, a, models.AnswerClosestBody)
	}
	if next := a[0].NextSteps; !strings.Contains(next, "record it (keploy mock record)") || strings.Contains(next, "requestbody") || strings.Contains(next, "change on every run") {
		t.Errorf("the next step %q names a noise entry for a body with no fields", next)
	}
}

// A JSON call of another kind of document than the recording's (an array
// against an object) has no field of either at which they differ:
// bodyFieldsDiffering names none, so no next step names a request-body
// noise entry for it.
func TestAnotherKindOfJSONDocumentNamesNoField(t *testing.T) {
	h := newHTTP()
	x := recordedAs(t, h, "mock-search-1", 1, searchCall(`{"q":"ada"}`), jsonAnswer(200, `{"hit":1}`), nil)
	for _, call := range []string{`["ada"]`, `"ada"`} {
		if got := h.bodyFieldsDiffering(x, []byte(call), nil); len(got) != 0 {
			t.Errorf("bodyFieldsDiffering of %s against {\"q\":\"ada\"} = %q, want none", call, got)
		}
	}
}

// bodyFieldsDiffering names every field that differs, in sorted order, each
// time.
func TestBodyFieldsDifferingAreSorted(t *testing.T) {
	h := newHTTP()
	x := recordedAs(t, h, "mock-search-1", 1, searchCall(`{"a":1,"b":1,"c":1,"d":1,"e":1,"f":1,"same":1}`), jsonAnswer(200, `{}`), nil)
	want := []string{"body.a", "body.b", "body.c", "body.d", "body.e", "body.f"}
	for range 20 {
		if got := h.bodyFieldsDiffering(x, []byte(`{"f":2,"e":2,"d":2,"c":2,"b":2,"a":2,"same":1}`), nil); !slices.Equal(got, want) {
			t.Fatalf("bodyFieldsDiffering = %q, want %q", got, want)
		}
	}
}

// A call of an id the recording lacks is reported other_id with a next step
// that names both causes: a new input, to record; an id the app makes on
// every run, with the exact keploy.yml url-noise entry. That entry, read as
// YAML and given as keploy.yml's noise, makes the recording the call's own:
// the call is served it with no report.
func TestAnotherIDsNextStepNamesBothCausesAndItsNoiseMakesTheRecordingItsOwn(t *testing.T) {
	setProven(t, false)
	users := func(t *testing.T, h *HTTP) *mockMemDb {
		return &mockMemDb{mocks: []*models.Mock{
			recordedAs(t, h, "mock-user-1", 1, getUser("1"), jsonAnswer(200, `{"id":1}`), nil),
			recordedAs(t, h, "mock-user-2", 2, getUser("2"), jsonAnswer(200, `{"id":2}`), nil),
		}, updateUnFilteredReturn: true}
	}
	h := newHTTP()
	a := serveReported(t, h, users(t, h), getUser("3")).answers()
	if len(a) != 1 || a[0].Answer != models.AnswerOtherID {
		t.Fatalf("GET /users/3's answer reports are %+v, want one %q", a, models.AnswerOtherID)
	}
	next := a[0].NextSteps
	if !strings.Contains(next, "If the call is a new input, record it (keploy mock record)") || !strings.Contains(next, "If the app makes a new id there on every run") {
		t.Errorf("the next step %q does not name both causes", next)
	}
	entries := noiseEntriesIn(t, next, "test.globalNoise.global.url: ")
	if _, ok := entries["^/users/[0-9]+$"]; !ok || len(entries) != 1 {
		t.Errorf("the next step's url noise is %v, want the one entry ^/users/[0-9]+$", entries)
	}
	h = newHTTP()
	got := serveReportedWith(t, h, users(t, h), models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": entries}}, getUser("3"))
	if got.status != 200 || got.body != `{"id":1}` || len(got.answers()) != 0 {
		t.Fatalf("with keploy.yml's url %v, as the next step names it, GET /users/3 got %d %q with answer reports %+v, want user 1's recording and none", entries, got.status, got.body, got.answers())
	}
}

// urlNoiseEntry masks the segments that differ, by their shape, and every
// other segment as it is, anchored to the whole path: it matches each path
// that differs from the recorded one only there, and no path that differs
// elsewhere. YAML reads it as the regex it quotes.
func TestURLNoiseEntry(t *testing.T) {
	for _, tc := range []struct {
		recorded string
		differs  []string
		want     string
		same     []string
		other    []string
	}{
		{"/users/1", []string{"url.path.1"}, `^/users/[0-9]+$`, []string{"/users/3", "/users/123456"}, []string{"/users/1/orders", "/admin/users/3", "/users/abc"}},
		{"/orgs/7/users/1", []string{"url.path.1", "url.path.3"}, `^/orgs/[0-9]+/users/[0-9]+$`, []string{"/orgs/8/users/2"}, []string{"/orgs/8/teams/2"}},
		{"/v1.2/files/3f2b8c1e-9a4d-4e6b-8f0a-1c2d3e4f5a6b", []string{"url.path.2"}, `^/v1\.2/files/[^/]+$`, []string{"/v1.2/files/a1b2c3d4-0000-4000-8000-0123456789ab"}, []string{"/v1x2/files/a1b2c3d4-0000-4000-8000-0123456789ab", "/v1.2/files/a/b"}},
	} {
		quoted := urlNoiseEntry(tc.recorded, tc.differs)
		var key map[string][]string
		if err := yamlLib.Unmarshal([]byte("{"+quoted+": []}"), &key); err != nil {
			t.Fatalf("urlNoiseEntry(%q) = %s, which YAML does not read: %v", tc.recorded, quoted, err)
		}
		if _, ok := key[tc.want]; !ok || len(key) != 1 {
			t.Fatalf("urlNoiseEntry(%q, %q) = %s, read as %v, want the key %s", tc.recorded, tc.differs, quoted, key, tc.want)
		}
		re := regexp.MustCompile(tc.want)
		h := newHTTP()
		for _, p := range tc.same {
			if !h.MatchURLPath("http://users.local"+tc.recorded, p, []string{re.String()}, false) {
				t.Errorf("%s does not make %s match %s", tc.want, p, tc.recorded)
			}
		}
		for _, p := range tc.other {
			if h.MatchURLPath("http://users.local"+tc.recorded, p, []string{re.String()}, false) {
				t.Errorf("%s makes %s, another call, match %s", tc.want, p, tc.recorded)
			}
		}
	}
}

// requestBodyNoiseEntries names each field root-relative, quoted, with no
// patterns, as one YAML flow mapping reads them.
func TestRequestBodyNoiseEntries(t *testing.T) {
	got := requestBodyNoiseEntries([]string{"body.a.b", "body.ts"})
	if got != `"a.b": [], "ts": []` {
		t.Fatalf("requestBodyNoiseEntries = %s", got)
	}
	var entries map[string][]string
	if err := yamlLib.Unmarshal([]byte("{"+got+"}"), &entries); err != nil || len(entries) != 2 {
		t.Fatalf("YAML reads %s as %v (%v)", got, entries, err)
	}
}
