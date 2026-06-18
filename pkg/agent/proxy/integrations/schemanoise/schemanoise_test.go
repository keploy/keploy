package schemanoise

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// fakeAdapter is a minimal Adapter for exercising the engine independently of
// any protocol: the recorded body and stored noise are supplied directly, and
// SetLearnedNoise records what the engine wrote back.
type fakeAdapter struct {
	body      []byte
	hasBody   bool
	stored    map[string][]string
	written   map[string][]string
	isNoiseFn func(*models.Mock) func(string) bool
}

func (f *fakeAdapter) RecordedBody(*models.Mock) ([]byte, bool) { return f.body, f.hasBody }
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
		name         string
		recorded     string
		live         string
		known        map[string][]string
		wantDrift    map[string][]string
		wantCompare  bool
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
	// No learned noise => always allowed (strict never tightens un-learned mocks).
	none := New(&fakeAdapter{body: []byte(`{"a":1}`), hasBody: true}, false, true)
	if !none.StrictAllows(mock, []byte(`{"a":2}`), nil) {
		t.Fatalf("mock with no learned noise must be allowed under strict")
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
