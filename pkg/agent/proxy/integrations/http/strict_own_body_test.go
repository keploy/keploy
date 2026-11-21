package http

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// strictMisses is the reports of c that say the call was answered by no
// recording because strict noise enforcement refused every candidate.
func (c servedCall) strictMisses() []*models.MockMismatchReport {
	var out []*models.MockMismatchReport
	for _, r := range c.reports {
		if r.Answer == "" && r.MatchPhase == models.MatchPhaseStrict {
			out = append(out, r)
		}
	}
	return out
}

// diffPaths is the paths of r's field diffs.
func diffPaths(r *models.MockMismatchReport) []string {
	var out []string
	for _, d := range r.FieldDiffs {
		out = append(out, d.Path)
	}
	return out
}

// countOf is how many of paths are p.
func countOf(paths []string, p string) int {
	n := 0
	for _, q := range paths {
		if q == p {
			n++
		}
	}
	return n
}

// missHintRequestBodyNoise is the request-body noise keploy reads from a
// keploy.yml holding the "requestbody: {...}" entry a miss's next step
// names, read as keploy reads keploy.yml.
func missHintRequestBodyNoise(t *testing.T, next string) map[string]map[string][]string {
	t.Helper()
	const marker = "requestbody: {"
	i := strings.Index(next, marker)
	if i < 0 {
		t.Fatalf("the miss's next step %q names no keploy.yml requestbody entry", next)
	}
	return keployYmlRequestBodyNoise(t, "test.globalNoise.global.requestbody: "+next[i+len(marker)-1:])
}

// Under strict noise enforcement (test.mockNoiseStrict) a recording is
// served only as the call's own: the call's body equals it outside the
// noise keploy.yml declares and the record learned, compared both ways and
// whole. A call that sends a field no recording holds, or an array with
// another element, is a miss whose report names that field, even when it
// leaves one recording standing. A field only the call sends is a new
// input, and the miss's next step is recording again; declaring it noise in
// keploy.yml still makes the recording the call's own. An array element's
// next step names the keploy.yml entry, and that entry, read as keploy reads
// keploy.yml, makes the recording the call's own, served with no report.
// Before, the strict check compared only the fields a recording holds, and
// an array by its last element, so the one recording left was served as
// the call's own with no report: a call that sent admin=true was answered
// as the call without it.
func TestStrictNoiseServesOnlyTheCallsOwnRecording(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded []string
		call     string
		names    string // the field the miss names
		noise    string // the keploy.yml requestbody entry that makes it none; "": the miss's own
	}{
		{name: "a field only the call has, beside another recording", recorded: []string{`{"q":"ada","x":1}`, `{"q":"alan","x":1}`}, call: `{"q":"ada","x":1,"admin":true}`, names: "body.admin", noise: `{admin: []}`},
		{name: "a field only the call has, the one recording", recorded: []string{`{"name":"ada"}`}, call: `{"name":"ada","role":"admin"}`, names: "body.role", noise: `{role: []}`},
		{name: "a nested field only the call has", recorded: []string{`{"q":"ada","f":{"x":1}}`}, call: `{"q":"ada","f":{"x":1,"y":2}}`, names: "body.f.y", noise: `{f.y: []}`},
		{name: "an array element, not the last", recorded: []string{`{"ids":[3,2]}`}, call: `{"ids":[1,2]}`, names: "body.ids[]"},
	} {
		recordings := func(t *testing.T, h *HTTP) *mockMemDb {
			db := &mockMemDb{updateUnFilteredReturn: true}
			for i, b := range tc.recorded {
				db.mocks = append(db.mocks, recordedAs(t, h, "mock-search-"+strconv.Itoa(i+1), i+1, searchCall(b), jsonAnswer(200, `{"hit":`+strconv.Itoa(i+1)+`}`), nil))
			}
			return db
		}
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			strict := models.OutgoingOptions{MockNoiseStrict: true}
			h := newHTTP()
			got := serveReportedWith(t, h, recordings(t, h), strict, searchCall(tc.call))
			if got.status == 200 || len(got.answers()) != 0 {
				t.Fatalf("%s got %d %q with answer reports %+v under strict noise, want a miss", tc.call, got.status, got.body, got.answers())
			}
			m := got.strictMisses()
			if len(m) != 1 || countOf(diffPaths(m[0]), tc.names) != 1 {
				t.Fatalf("%s under strict noise was reported %+v, want one %q miss naming %s once", tc.call, got.reports, models.MatchPhaseStrict, tc.names)
			}
			opts := strict
			if tc.noise == "" {
				opts.NoiseConfig = missHintRequestBodyNoise(t, m[0].NextSteps)
			} else {
				if !strings.Contains(m[0].NextSteps, "Re-record") {
					t.Errorf("the miss of a field only the call sends gives the next step %q, want recording again", m[0].NextSteps)
				}
				opts.NoiseConfig = keployYmlRequestBodyNoise(t, "test.globalNoise.global.requestbody: "+tc.noise)
			}
			h = newHTTP()
			again := serveReportedWith(t, h, recordings(t, h), opts, searchCall(tc.call))
			if again.status != 200 || again.body != `{"hit":1}` || len(again.reports) != 0 {
				t.Fatalf("with keploy.yml noise %v under strict noise, %s got %d %q with reports %+v, want its own recording and none", opts.NoiseConfig, tc.call, again.status, again.body, again.reports)
			}
		})
		t.Run(tc.name+", proven switch", func(t *testing.T) {
			setProven(t, true)
			h := newHTTP()
			got := serveReportedWith(t, h, recordings(t, h), models.OutgoingOptions{MockNoiseStrict: true}, searchCall(tc.call))
			if len(got.answers()) != 0 || got.status == 200 && strings.HasPrefix(got.body, `{"hit":`) {
				t.Fatalf("%s got %d %q with answer reports %+v under strict noise and the switch, want a miss", tc.call, got.status, got.body, got.answers())
			}
		})
	}
}

// Under strict noise enforcement a call whose body is a recording's but for
// key order and spacing, or a value at a field the record learned is noise,
// is that recording's own, served with no report. A learned field of
// another type is still a difference: a miss that names the field, which
// the diffs of the bodies with the learned noise left out did not.
func TestStrictNoiseServesAKeyOrderOrLearnedNoiseAsTheCallsOwn(t *testing.T) {
	setProven(t, false)
	strict := models.OutgoingOptions{MockNoiseStrict: true}
	for _, tc := range []struct {
		name, call string
		served     bool
	}{
		{"another key order", `{ "ts": 1, "q": "ada" }`, true},
		{"a learned field's other value", `{"q":"ada","ts":7}`, true},
		{"a learned field's other type", `{"q":"ada","ts":"7"}`, false},
	} {
		h := newHTTP()
		db := searchRecordings(t, h, map[string][]string{"body.ts": {}}, `{"q":"ada","ts":1}`)
		got := serveReportedWith(t, h, db, strict, searchCall(tc.call))
		if served := got.status == 200 && got.body == `{"hit":1}` && len(got.reports) == 0; served != tc.served {
			t.Errorf("%s: %s got %d %q with reports %+v under strict noise, served as its own %v, want %v", tc.name, tc.call, got.status, got.body, got.reports, served, tc.served)
		}
		if m := got.strictMisses(); !tc.served && (len(m) != 1 || !slices.Contains(diffPaths(m[0]), "body.ts")) {
			t.Errorf("%s: %s under strict noise was reported %+v, want one %q miss naming body.ts", tc.name, tc.call, got.reports, models.MatchPhaseStrict)
		}
	}
}

// A strict miss names only the fields that are no noise: a field keploy.yml
// declares request-body noise is not named, though its value differs too.
func TestStrictNoiseMissNamesNoFieldItsNoiseCovers(t *testing.T) {
	setProven(t, false)
	h := newHTTP()
	db := searchRecordings(t, h, nil, `{"ids":[3,2],"ts":1}`)
	opts := models.OutgoingOptions{MockNoiseStrict: true, NoiseConfig: keployYmlRequestBodyNoise(t, "test.globalNoise.global.requestbody: {ts: []}")}
	got := serveReportedWith(t, h, db, opts, searchCall(`{"ids":[1,2],"ts":2}`))
	if m := got.strictMisses(); len(m) != 1 || !slices.Equal(diffPaths(m[0]), []string{"body.ids[]"}) {
		t.Fatalf("the call was reported %+v under strict noise, want one %q miss naming body.ids[] alone", got.reports, models.MatchPhaseStrict)
	}
}

// A strict miss names a field only one side holds, which the diffs of the
// bodies with the learned noise left out, or of arrays read as one element,
// do not, as missing on the other side: missing_in_live where
// only the recording holds it, missing_in_mock where only the call does,
// and as changed in value where each holds it in an element of an array
// the other's lacks it in. Before, each was named changed in value.
func TestStrictNoiseMissNamesAFieldOnOneSideByItsSide(t *testing.T) {
	setProven(t, false)
	for _, tc := range []struct {
		name     string
		learned  map[string][]string
		recorded string
		call     string
		path     string
		kind     models.MockFieldDiffKind
	}{
		{name: "a learned field only the call has", learned: map[string][]string{"body.ts": {}}, recorded: `{"q":"ada"}`, call: `{"q":"ada","ts":7}`, path: "body.ts", kind: models.DiffKindMissingInMock},
		{name: "a field only the recording's element before the last has", recorded: `{"items":[{"x":1},{"x":1}]}`, call: `{"items":[{"x":1},{"y":1}]}`, path: "body.items[].x", kind: models.DiffKindMissingInLive},
		{name: "a field each side has in an element the other's lacks it in", recorded: `{"items":[{"x":1,"y":1},{"x":2}]}`, call: `{"items":[{"x":1},{"x":2,"y":1}]}`, path: "body.items[].y", kind: models.DiffKindValueChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTP()
			db := searchRecordings(t, h, tc.learned, tc.recorded)
			got := serveReportedWith(t, h, db, models.OutgoingOptions{MockNoiseStrict: true}, searchCall(tc.call))
			m := got.strictMisses()
			if len(m) != 1 {
				t.Fatalf("%s against %s was reported %+v under strict noise, want one %q miss", tc.call, tc.recorded, got.reports, models.MatchPhaseStrict)
			}
			var named []models.MockFieldDiff
			for _, d := range m[0].FieldDiffs {
				if d.Path == tc.path {
					named = append(named, d)
				}
			}
			if len(named) != 1 || named[0].Kind != tc.kind {
				t.Fatalf("%s against %s: the miss names %s as %+v (all: %+v), want it once as %s", tc.call, tc.recorded, tc.path, named, m[0].FieldDiffs, tc.kind)
			}
		})
	}
}
