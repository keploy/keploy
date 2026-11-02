package mocknoise

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// driftAsBefore is DetectJSONDrift as it was before it read its bodies as
// JSONBody: each body validated, then matcher.ChangedJSONFieldPaths, which
// validates and decodes both again, on every comparison.
func driftAsBefore(recorded, live []byte, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, isRecordedNoise func(string) bool) (map[string][]string, bool) {
	if !json.Valid(recorded) || !json.Valid(live) {
		return nil, false
	}
	paths := matcher.ChangedJSONFieldPaths(string(recorded), string(live), known, typeStrict, valueChangesOnly, isRecordedNoise)
	if len(paths) == 0 {
		return nil, true
	}
	out := make(map[string][]string, len(paths))
	for _, p := range paths {
		out["body."+p] = []string{}
	}
	return out, true
}

// undecodable is JSON that json.Unmarshal does not decode into an
// interface{}: its number is past float64's range.
var undecodable = []byte(`{"id":1e400,"name":"a"}`)

// jsonBodyCases are recorded bodies compared with one live body: equal,
// drifted in value, in type and by a removed field, not JSON, and JSON that
// does not decode.
var jsonBodyCases = [][]byte{
	[]byte(`{"id":7,"name":"a","tags":["x"]}`),
	[]byte(`{"id":8,"name":"a","tags":["x"]}`),
	[]byte(`{"id":"7","name":"a","tags":["x"]}`),
	[]byte(`{"id":7,"name":"a","tags":["x"],"gone":true}`),
	[]byte(`{"id":7,"name":"b","tags":["y"]}`),
	[]byte(`not json`),
	nil,
	undecodable,
}

// TestDetectJSONBodyDriftFindsWhatDetectJSONDriftFoundBefore: one live
// JSONBody compared with each recorded body in turn, as the strict gate
// compares a command's live body with its candidates, finds the drift and
// comparability DetectJSONDrift found when it validated and decoded both
// bodies on every comparison, under every noise setting; and so does
// DetectJSONDrift now. Live bodies that are not JSON, or do not decode, are
// compared too.
func TestDetectJSONBodyDriftFindsWhatDetectJSONDriftFoundBefore(t *testing.T) {
	if _, ok := NewJSONBody(undecodable).decodedValue(); ok || !json.Valid(undecodable) {
		t.Fatalf("%s must be JSON that does not decode", undecodable)
	}
	settings := []struct {
		name             string
		known            map[string][]string
		typeStrict       map[string]struct{}
		valueChangesOnly bool
		isNoise          func(string) bool
	}{
		{name: "none"},
		{name: "known", known: map[string][]string{"name": {}}},
		{name: "typeStrict", typeStrict: map[string]struct{}{"id": {}}},
		{name: "valueChangesOnly", valueChangesOnly: true},
		{name: "recordedNoise", isNoise: func(v string) bool { return v == "a" }},
	}
	lives := [][]byte{jsonBodyCases[0], []byte(`not json`), undecodable}
	for _, s := range settings {
		for _, liveRaw := range lives {
			live := NewJSONBody(liveRaw)
			for i, rec := range jsonBodyCases {
				wantDrift, wantOK := driftAsBefore(rec, liveRaw, s.known, s.typeStrict, s.valueChangesOnly, s.isNoise)
				gotDrift, gotOK := DetectJSONBodyDrift(NewJSONBody(rec), live, s.known, s.typeStrict, s.valueChangesOnly, s.isNoise)
				if gotOK != wantOK || !reflect.DeepEqual(gotDrift, wantDrift) {
					t.Errorf("%s, live %s, recorded %d (%s): DetectJSONBodyDrift = %v, %v; before = %v, %v", s.name, liveRaw, i, rec, gotDrift, gotOK, wantDrift, wantOK)
				}
				gotDrift, gotOK = DetectJSONDrift(rec, liveRaw, s.known, s.typeStrict, s.valueChangesOnly, s.isNoise)
				if gotOK != wantOK || !reflect.DeepEqual(gotDrift, wantDrift) {
					t.Errorf("%s, live %s, recorded %d (%s): DetectJSONDrift = %v, %v; before = %v, %v", s.name, liveRaw, i, rec, gotDrift, gotOK, wantDrift, wantOK)
				}
			}
		}
	}
}

// TestALiveJSONBodyComparedWithManyIsReadOnce: a live body compared with
// three recorded ones is validated once and decoded once; each recorded one,
// compared once, is too. Counted, not timed.
func TestALiveJSONBodyComparedWithManyIsReadOnce(t *testing.T) {
	live := NewJSONBody(jsonBodyCases[0])
	recs := []*JSONBody{NewJSONBody(jsonBodyCases[1]), NewJSONBody(jsonBodyCases[2]), NewJSONBody(jsonBodyCases[4])}
	for _, rec := range recs {
		DetectJSONBodyDrift(rec, live, nil, nil, false, nil)
	}
	if live.reads != 2 {
		t.Errorf("the live body was read %d times over %d comparisons, want 2 (validated once, decoded once)", live.reads, len(recs))
	}
	for i, rec := range recs {
		if rec.reads != 2 {
			t.Errorf("recorded body %d was read %d times, want 2", i, rec.reads)
		}
	}
}

// jsonBodyAdapter is fakeAdapter saying its Diff is JSONDiffer's
// (JSONBodyDiffer).
type jsonBodyAdapter struct{ fakeAdapter }

func (a *jsonBodyAdapter) DiffJSONBodies(_ *models.Mock, recorded, live *JSONBody, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, valIsNoise func(string) bool) (map[string][]string, bool) {
	return DetectJSONBodyDrift(recorded, live, known, typeStrict, valueChangesOnly, valIsNoise)
}

// ownDiffAdapter embeds JSONDiffer but has a Diff of its own, as HTTP's
// adapter does for form bodies: it reports every body as drifted at "own".
type ownDiffAdapter struct{ fakeAdapter }

func (a *ownDiffAdapter) Diff(*models.Mock, []byte, []byte, map[string][]string, map[string]struct{}, bool, func(string) bool) (map[string][]string, bool) {
	return map[string][]string{"body.own": {}}, true
}

// TestStrictEnforcementReadsALiveJSONBodyOnceThroughItsAdapter: under strict
// enforcement, an adapter that says its Diff is JSONDiffer's
// (JSONBodyDiffer) is given the live body as the JSONBody the caller holds,
// so three candidates read it once, with the verdicts StrictRejectRecorded
// gives; an adapter with a Diff of its own keeps it, whatever it embeds.
func TestStrictEnforcementReadsALiveJSONBodyOnceThroughItsAdapter(t *testing.T) {
	e := New(&jsonBodyAdapter{}, false, true)
	live := NewJSONBody(jsonBodyCases[0])
	for i, rec := range [][]byte{jsonBodyCases[0], jsonBodyCases[1], []byte(`not json`)} {
		gotAllowed, gotDrift := e.StrictRejectRecordedBody(&models.Mock{}, rec, true, live, nil)
		wantAllowed, wantDrift := e.StrictRejectRecorded(&models.Mock{}, rec, true, live.Raw(), nil)
		if gotAllowed != wantAllowed || !reflect.DeepEqual(gotDrift, wantDrift) {
			t.Errorf("recorded %d: StrictRejectRecordedBody = %v, %v; StrictRejectRecorded = %v, %v", i, gotAllowed, gotDrift, wantAllowed, wantDrift)
		}
	}
	if live.reads != 2 {
		t.Errorf("the live body was read %d times over three candidates, want 2 (validated once, decoded once)", live.reads)
	}

	own := New(&ownDiffAdapter{}, false, true)
	allowed, drift := own.StrictRejectRecordedBody(&models.Mock{}, jsonBodyCases[0], true, NewJSONBody(jsonBodyCases[0]), nil)
	if allowed || !reflect.DeepEqual(drift, map[string][]string{"body.own": {}}) {
		t.Errorf("an adapter with its own Diff: allowed %v, drift %v; want its Diff's body.own", allowed, drift)
	}
}
