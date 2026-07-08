package schemanoise

import (
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// fakeAdapter is a minimal Adapter for exercising the engine independently of
// any protocol: the recorded body and stored noise are supplied directly, and
// SetLearnedNoise records what the engine wrote back. It embeds JSONDiffer, so
// its payloads are compared as JSON — the common case.
type fakeAdapter struct {
	JSONDiffer
	body      []byte
	hasBody   bool
	stored    map[string][]string
	written   map[string][]string
	isNoiseFn func(*models.Mock) func(string) bool
}

func (f *fakeAdapter) RecordedBody(*models.Mock) ([]byte, bool)     { return f.body, f.hasBody }
func (f *fakeAdapter) StoredNoise(*models.Mock) map[string][]string { return f.stored }
func (f *fakeAdapter) SetLearnedNoise(_ *models.Mock, merged map[string][]string) {
	f.written = merged
}
func (f *fakeAdapter) RecordedValueIsNoise(m *models.Mock) func(string) bool {
	if f.isNoiseFn == nil {
		return nil
	}
	return f.isNoiseFn(m)
}

func TestDetectJSONDrift(t *testing.T) {
	tests := []struct {
		name        string
		recorded    string
		live        string
		known       map[string][]string
		wantDrift   map[string][]string
		wantCompare bool
	}{
		{
			name:        "value drift on one field",
			recorded:    `{"orderId":"EKL-1","updatedAt":1}`,
			live:        `{"orderId":"EKL-1","updatedAt":2}`,
			wantDrift:   map[string][]string{"body.updatedAt": {}},
			wantCompare: true,
		},
		{
			name:        "drift on a known-noise field is excluded",
			recorded:    `{"orderId":"EKL-1","updatedAt":1}`,
			live:        `{"orderId":"EKL-1","updatedAt":2}`,
			known:       map[string][]string{"updatedAt": {}},
			wantDrift:   nil,
			wantCompare: true,
		},
		{
			name:        "identical JSON is comparable but no drift",
			recorded:    `{"a":1}`,
			live:        `{"a":1}`,
			wantDrift:   nil,
			wantCompare: true,
		},
		{
			name:        "non-JSON recorded => not comparable",
			recorded:    "plain-text-v1",
			live:        "plain-text-v2",
			wantDrift:   nil,
			wantCompare: false,
		},
		{
			name:        "one side non-JSON => not comparable",
			recorded:    `{"a":1}`,
			live:        "not-json",
			wantDrift:   nil,
			wantCompare: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			drift, comparable := DetectJSONDrift([]byte(tc.recorded), []byte(tc.live), tc.known, nil)
			if comparable != tc.wantCompare {
				t.Fatalf("comparable: got %v want %v", comparable, tc.wantCompare)
			}
			if !reflect.DeepEqual(drift, tc.wantDrift) {
				t.Fatalf("drift: got %v want %v", drift, tc.wantDrift)
			}
		})
	}
}

// lineDiffer is a reference NON-JSON Diff: it proves a parser with a binary /
// structured (non-JSON) payload can plug into the same Engine by supplying its
// own Diff. It treats the body as newline-separated "key=value" records and
// reports drifting keys as "body.<key>" — no JSON anywhere. This is the
// contract test for "plug-and-play for all parser categories".
type lineDiffer struct{}

func (lineDiffer) Diff(_ *models.Mock, recorded, live []byte, known map[string][]string, _ func(string) bool) (map[string][]string, bool) {
	parse := func(b []byte) map[string]string {
		out := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.IndexByte(line, '='); i >= 0 {
				out[line[:i]] = line[i+1:]
			}
		}
		return out
	}
	rec, liv := parse(recorded), parse(live)
	drift := map[string][]string{}
	for k, rv := range rec {
		if _, isKnown := known[k]; isKnown {
			continue
		}
		if lv, ok := liv[k]; !ok || lv != rv {
			drift["body."+k] = []string{}
		}
	}
	if len(drift) == 0 {
		return nil, true // comparable, no drift
	}
	return drift, true
}

// binaryAdapter is a non-JSON parser adapter built on lineDiffer.
type binaryAdapter struct {
	lineDiffer
	body    []byte
	stored  map[string][]string
	written map[string][]string
}

func (b *binaryAdapter) RecordedBody(*models.Mock) ([]byte, bool)              { return b.body, true }
func (b *binaryAdapter) StoredNoise(*models.Mock) map[string][]string          { return b.stored }
func (b *binaryAdapter) SetLearnedNoise(_ *models.Mock, m map[string][]string) { b.written = m }
func (b *binaryAdapter) RecordedValueIsNoise(*models.Mock) func(string) bool   { return nil }

// TestNonJSONParserPlugsIn proves the engine is parser-generic: a non-JSON
// adapter detects drift, learns it, and enforces strict — all without the
// engine knowing anything about the payload format.
func TestNonJSONParserPlugsIn(t *testing.T) {
	a := &binaryAdapter{body: []byte("orderId=EKL-1\nstatus=DISPATCHED")}
	e := New(a, true, false)

	// status drifts → learnable, even though the payload is not JSON.
	drift, comparable := e.Detect(&models.Mock{}, []byte("orderId=EKL-1\nstatus=CANCELLED"), nil)
	if !comparable {
		t.Fatalf("line format must be comparable")
	}
	if _, ok := drift["body.status"]; !ok {
		t.Fatalf("status drift must be detected for a non-JSON parser: %v", drift)
	}
	if _, ok := drift["body.orderId"]; ok {
		t.Fatalf("stable orderId must not drift: %v", drift)
	}

	// Strict: with status learned as noise, a status drift is allowed; an
	// orderId drift is rejected.
	a.stored = map[string][]string{"body.status": {}}
	if !New(a, false, true).StrictAllows(&models.Mock{}, []byte("orderId=EKL-1\nstatus=CANCELLED"), nil) {
		t.Fatalf("non-JSON strict must allow drift confined to learned noise")
	}
	if New(a, false, true).StrictAllows(&models.Mock{}, []byte("orderId=EKL-2\nstatus=DISPATCHED"), nil) {
		t.Fatalf("non-JSON strict must reject drift on a non-noise field")
	}
}

func TestEngineDetectDisabled(t *testing.T) {
	a := &fakeAdapter{body: []byte(`{"a":1}`), hasBody: true}
	e := New(a, false, false)
	drift, comparable := e.Detect(&models.Mock{}, []byte(`{"a":2}`), nil)
	if drift != nil || comparable {
		t.Fatalf("detection disabled must be a no-op, got drift=%v comparable=%v", drift, comparable)
	}
}

func TestEngineLearnMonotonicAndDeepCopied(t *testing.T) {
	stored := map[string][]string{"body.a": {}}
	a := &fakeAdapter{stored: stored}
	e := New(a, true, false)

	added := e.Learn(&models.Mock{}, map[string][]string{"body.a": {}, "body.b": {}})
	if added != 1 {
		t.Fatalf("only body.b is new, want added=1 got %d", added)
	}
	if _, ok := a.written["body.a"]; !ok {
		t.Fatalf("existing body.a must be retained: %v", a.written)
	}
	if _, ok := a.written["body.b"]; !ok {
		t.Fatalf("new body.b must be learned: %v", a.written)
	}
	// The result must not alias the stored map (deep-copy contract).
	if &a.written == &stored {
		t.Fatalf("learned map must not alias the stored map")
	}
	a.written["body.c"] = []string{}
	if _, ok := stored["body.c"]; ok {
		t.Fatalf("mutating the learned map leaked into the stored map")
	}

	// Re-learning the same paths adds nothing and does not write.
	a.written = nil
	if added := e.Learn(&models.Mock{}, map[string][]string{"body.a": {}}); added != 0 {
		t.Fatalf("re-learning a known path must add nothing, got %d", added)
	}
	if a.written != nil {
		t.Fatalf("no write expected when nothing is new, got %v", a.written)
	}
}

func TestEngineKnownNoiseMergesStoredAndUser(t *testing.T) {
	a := &fakeAdapter{stored: map[string][]string{"body.updatedAt": {}}}
	e := New(a, true, false)
	known := e.KnownNoise(&models.Mock{}, map[string][]string{"sessionId": {}})
	// stored noise is prefix-stripped; user noise is root-relative already.
	if _, ok := known["updatedAt"]; !ok {
		t.Fatalf("stored noise must be stripped to root-relative: %v", known)
	}
	if _, ok := known["sessionId"]; !ok {
		t.Fatalf("user noise must be present: %v", known)
	}
}

func TestEngineStrictAllows(t *testing.T) {
	mock := &models.Mock{}
	// No learned noise, value drift => rejected under strict. Strict now
	// value-checks every candidate, not just ones carrying learned noise.
	noneDrift := New(&fakeAdapter{body: []byte(`{"a":1}`), hasBody: true}, false, true)
	if noneDrift.StrictAllows(mock, []byte(`{"a":2}`), nil) {
		t.Fatalf("value drift on an un-learned, un-noised field must be rejected under strict")
	}

	// No learned noise, identical body => allowed (nothing drifted).
	noneSame := New(&fakeAdapter{body: []byte(`{"a":1}`), hasBody: true}, false, true)
	if !noneSame.StrictAllows(mock, []byte(`{"a":1}`), nil) {
		t.Fatalf("identical body must be allowed under strict even with no learned noise")
	}

	// No learned noise but the drift is on a user-configured noise field => allowed.
	noneUserNoise := New(&fakeAdapter{body: []byte(`{"a":1}`), hasBody: true}, false, true)
	if !noneUserNoise.StrictAllows(mock, []byte(`{"a":2}`), map[string][]string{"a": {}}) {
		t.Fatalf("drift on a user-noised field must be allowed under strict even with no learned noise")
	}

	// Without strict enforcement an un-learned mock is left alone (lenient).
	lenient := New(&fakeAdapter{body: []byte(`{"a":1}`), hasBody: true}, false, false)
	if !lenient.StrictAllows(mock, []byte(`{"a":2}`), nil) {
		t.Fatalf("non-strict engine must keep lenient behaviour for un-learned mocks")
	}

	// Learned noise covers the only drifting field => allowed.
	okAdapter := &fakeAdapter{
		body:    []byte(`{"a":1,"updatedAt":1}`),
		hasBody: true,
		stored:  map[string][]string{"body.updatedAt": {}},
	}
	if !New(okAdapter, false, true).StrictAllows(mock, []byte(`{"a":1,"updatedAt":2}`), nil) {
		t.Fatalf("drift confined to learned noise must be allowed under strict")
	}

	// A field OUTSIDE the learned set drifted => rejected.
	rejAdapter := &fakeAdapter{
		body:    []byte(`{"a":1,"updatedAt":1}`),
		hasBody: true,
		stored:  map[string][]string{"body.updatedAt": {}},
	}
	if New(rejAdapter, false, true).StrictAllows(mock, []byte(`{"a":99,"updatedAt":2}`), nil) {
		t.Fatalf("drift on a non-noise field must be rejected under strict")
	}

	// Non-JSON bodies that differ are a real mismatch (no field structure).
	binAdapter := &fakeAdapter{
		body:    []byte("bin-v1"),
		hasBody: true,
		stored:  map[string][]string{"body.x": {}},
	}
	if New(binAdapter, false, true).StrictAllows(mock, []byte("bin-v2"), nil) {
		t.Fatalf("differing non-JSON bodies must be rejected under strict")
	}
}

// StrictReject must name the offending non-noise field(s) so the rejection can
// be logged with the drifted path (e.g. "body.content") — callers and CI rely
// on that detail, not just the boolean verdict.
func TestEngineStrictReject(t *testing.T) {
	mock := &models.Mock{}

	// Allowed => no drift reported.
	allowAdapter := &fakeAdapter{
		body:    []byte(`{"a":1,"updatedAt":1}`),
		hasBody: true,
		stored:  map[string][]string{"body.updatedAt": {}},
	}
	if allowed, drift := New(allowAdapter, false, true).StrictReject(mock, []byte(`{"a":1,"updatedAt":2}`), nil); !allowed || drift != nil {
		t.Fatalf("allowed mock must report no drift; got allowed=%v drift=%v", allowed, drift)
	}

	// Rejected => the non-noise field that drifted is reported, prefixed.
	rejAdapter := &fakeAdapter{
		body:    []byte(`{"a":1,"updatedAt":1}`),
		hasBody: true,
		stored:  map[string][]string{"body.updatedAt": {}},
	}
	allowed, drift := New(rejAdapter, false, true).StrictReject(mock, []byte(`{"a":99,"updatedAt":2}`), nil)
	if allowed {
		t.Fatalf("non-noise drift must be rejected")
	}
	if _, ok := drift["body.a"]; !ok {
		t.Fatalf("rejection drift must name body.a; got %v", drift)
	}

	// Opaque (non-JSON) byte mismatch => rejected with the "body" sentinel.
	binAdapter := &fakeAdapter{
		body:    []byte("bin-v1"),
		hasBody: true,
		stored:  map[string][]string{"body.x": {}},
	}
	allowed, drift = New(binAdapter, false, true).StrictReject(mock, []byte("bin-v2"), nil)
	if allowed || len(drift) == 0 {
		t.Fatalf("opaque byte mismatch must be rejected with non-empty drift; got allowed=%v drift=%v", allowed, drift)
	}

	// No learned noise: strict still value-compares and names the drifted field.
	noNoiseAdapter := &fakeAdapter{
		body:    []byte(`{"a":1,"tier_type":"STANDARD_LARGE"}`),
		hasBody: true,
	}
	allowed, drift = New(noNoiseAdapter, false, true).StrictReject(mock, []byte(`{"a":1,"tier_type":"regular"}`), nil)
	if allowed {
		t.Fatalf("value drift must be rejected under strict even with no learned noise")
	}
	if _, ok := drift["body.tier_type"]; !ok {
		t.Fatalf("rejection drift must name body.tier_type; got %v", drift)
	}
}
