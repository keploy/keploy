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

// bodyDiffer is an Adapter whose Diff is JSONDiffer's, saying so
// (JSONBodyDiffer) as MySQL's adapter does, so the engine compares through
// its DiffJSONBodies; calls counts the comparisons that reached it.
type bodyDiffer struct {
	Adapter
	calls *int
}

func (b bodyDiffer) DiffJSONBodies(_ *models.Mock, recorded, live *JSONBody, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, valIsNoise func(string) bool) (map[string][]string, bool) {
	*b.calls++
	return DetectJSONBodyDrift(recorded, live, known, typeStrict, valueChangesOnly, valIsNoise)
}

// forEachStrictBranch runs row once for each way the engine compares a
// recording under strict enforcement: through the adapter's Diff, and
// through DiffJSONBodies for the same adapter as a JSONBodyDiffer (bodyDiffer),
// so a strict row holds on both. newEngine is New over the adapter row gives
// it, as that branch takes it; on the second, a row whose comparisons never
// reach DiffJSONBodies fails.
func forEachStrictBranch(t *testing.T, row func(t *testing.T, newEngine func(a Adapter, detection, strict bool) *Engine)) {
	t.Helper()
	t.Run("Diff", func(t *testing.T) {
		row(t, func(a Adapter, detection, strict bool) *Engine {
			if _, isBodies := a.(JSONBodyDiffer); isBodies {
				t.Fatalf("%T compares through DiffJSONBodies, not its Diff", a)
			}
			return New(a, detection, strict)
		})
	})
	t.Run("DiffJSONBodies", func(t *testing.T) {
		calls := 0
		row(t, func(a Adapter, detection, strict bool) *Engine {
			return New(bodyDiffer{a, &calls}, detection, strict)
		})
		if calls == 0 {
			t.Errorf("no comparison reached DiffJSONBodies")
		}
	})
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
// so the candidates read it once, and gives each the verdict the same
// adapter gives through its Diff: with learned noise stored (type-strict),
// user noise and a recorded value that is noise, over every recorded body
// of jsonBodyCases. An adapter with a Diff of its own keeps it, whatever it
// embeds.
func TestStrictEnforcementReadsALiveJSONBodyOnceThroughItsAdapter(t *testing.T) {
	noise := func(*models.Mock) func(string) bool {
		return func(v string) bool { return v == "b" }
	}
	adapter := func() *fakeAdapter {
		return &fakeAdapter{stored: map[string][]string{"body.id": {}}, isNoiseFn: noise}
	}
	user := map[string][]string{"tags": {}}
	calls := 0
	bodies := New(bodyDiffer{adapter(), &calls}, false, true)
	diff := New(adapter(), false, true)
	live := NewJSONBody(jsonBodyCases[0])
	verdicts := map[bool]int{}
	for i, rec := range jsonBodyCases {
		gotAllowed, gotDrift := bodies.StrictRejectRecordedBody(&models.Mock{}, rec, true, live, user)
		wantAllowed, wantDrift := diff.StrictRejectRecorded(&models.Mock{}, rec, true, live.Raw(), user)
		if gotAllowed != wantAllowed || !reflect.DeepEqual(gotDrift, wantDrift) {
			t.Errorf("recorded %d (%s): through DiffJSONBodies %v, %v; through Diff %v, %v", i, rec, gotAllowed, gotDrift, wantAllowed, wantDrift)
		}
		verdicts[wantAllowed]++
	}
	if calls != len(jsonBodyCases) || verdicts[true] == 0 || verdicts[false] == 0 {
		t.Errorf("%d of %d comparisons reached DiffJSONBodies, %d allowed and %d rejected; want every one, and both verdicts", calls, len(jsonBodyCases), verdicts[true], verdicts[false])
	}
	if live.reads != 2 {
		t.Errorf("the live body was read %d times over %d candidates, want 2 (validated once, decoded once)", live.reads, len(jsonBodyCases))
	}

	own := New(&ownDiffAdapter{}, false, true)
	allowed, drift := own.StrictRejectRecordedBody(&models.Mock{}, jsonBodyCases[0], true, NewJSONBody(jsonBodyCases[0]), nil)
	if allowed || !reflect.DeepEqual(drift, map[string][]string{"body.own": {}}) {
		t.Errorf("an adapter with its own Diff: allowed %v, drift %v; want its Diff's body.own", allowed, drift)
	}
}

// TestStrictEnforcementLeavesOutARecordedValueThatIsNoise: under strict
// enforcement, through both of the engine's branches, a field whose recorded
// value the mock's value noise covers (RecordedValueIsNoise: an obfuscated
// secret) is not compared, whatever its live value; another field that
// drifted beside it is rejected, named alone; and with no value noise the
// same secret's change is rejected.
func TestStrictEnforcementLeavesOutARecordedValueThatIsNoise(t *testing.T) {
	forEachStrictBranch(t, func(t *testing.T, newEngine func(a Adapter, detection, strict bool) *Engine) {
		recorded := []byte(`{"token":"obfuscated-1","a":1}`)
		secret := func(*models.Mock) func(string) bool {
			return func(v string) bool { return v == "obfuscated-1" }
		}
		e := newEngine(&fakeAdapter{body: recorded, hasBody: true, isNoiseFn: secret}, false, true)
		if allowed, drift := e.StrictReject(&models.Mock{}, []byte(`{"token":"live-1","a":1}`), nil); !allowed {
			t.Errorf("the secret changed: rejected, drift %v; want allowed", drift)
		}
		allowed, drift := e.StrictReject(&models.Mock{}, []byte(`{"token":"live-1","a":2}`), nil)
		if allowed || !reflect.DeepEqual(drift, map[string][]string{"body.a": {}}) {
			t.Errorf("the secret and a changed: allowed %v, drift %v; want rejected at body.a alone", allowed, drift)
		}
		plain := newEngine(&fakeAdapter{body: recorded, hasBody: true}, false, true)
		allowed, drift = plain.StrictReject(&models.Mock{}, []byte(`{"token":"live-1","a":1}`), nil)
		if allowed || !reflect.DeepEqual(drift, map[string][]string{"body.token": {}}) {
			t.Errorf("no value noise, the token changed: allowed %v, drift %v; want rejected at body.token", allowed, drift)
		}
	})
}
