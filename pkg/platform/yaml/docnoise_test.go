package yaml

import (
	"encoding/json"
	"reflect"
	"testing"

	yamlLib "gopkg.in/yaml.v3"
)

// TestDocNoise_UnmarshalYAML covers the backward-compatible YAML decoding: the
// new mapping shape ({req, value}) and the legacy bare string list (-> value).
func TestDocNoise_UnmarshalYAML(t *testing.T) {
	t.Run("new mapping shape", func(t *testing.T) {
		var n DocNoise
		in := "req:\n  - body.tier_type\n  - body.user.id\nvalue:\n  - \"^tok-.*$\"\n"
		if err := yamlLib.Unmarshal([]byte(in), &n); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !reflect.DeepEqual(n.Req, []string{"body.tier_type", "body.user.id"}) {
			t.Fatalf("req mismatch: %v", n.Req)
		}
		if !reflect.DeepEqual(n.Value, []string{"^tok-.*$"}) {
			t.Fatalf("value mismatch: %v", n.Value)
		}
	})

	t.Run("legacy list shape folds into value", func(t *testing.T) {
		var n DocNoise
		in := "- \"^tok-.*$\"\n- \"^sess-.*$\"\n"
		if err := yamlLib.Unmarshal([]byte(in), &n); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(n.Req) != 0 {
			t.Fatalf("legacy list must not populate req: %v", n.Req)
		}
		if !reflect.DeepEqual(n.Value, []string{"^tok-.*$", "^sess-.*$"}) {
			t.Fatalf("legacy list must fold into value: %v", n.Value)
		}
	})
}

// TestDocNoise_UnmarshalJSON mirrors the YAML test for the JSON path.
func TestDocNoise_UnmarshalJSON(t *testing.T) {
	t.Run("new object shape", func(t *testing.T) {
		var n DocNoise
		if err := json.Unmarshal([]byte(`{"req":["body.a"],"value":["^x.*$"]}`), &n); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !reflect.DeepEqual(n.Req, []string{"body.a"}) || !reflect.DeepEqual(n.Value, []string{"^x.*$"}) {
			t.Fatalf("decoded: %#v", n)
		}
	})

	t.Run("legacy array shape folds into value", func(t *testing.T) {
		var n DocNoise
		if err := json.Unmarshal([]byte(`["^x.*$"]`), &n); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(n.Req) != 0 || !reflect.DeepEqual(n.Value, []string{"^x.*$"}) {
			t.Fatalf("legacy array must fold into value: %#v", n)
		}
	})
}

// TestNewDocNoise_DropsRegexValuesAndSorts verifies the encode helper persists
// only the field PATHS of the schema-noise map (regex values dropped), sorts them
// deterministically, and returns nil when there is nothing to write.
func TestNewDocNoise_DropsRegexValuesAndSorts(t *testing.T) {
	got := NewDocNoise([]string{"^tok-.*$"}, map[string][]string{
		"body.z": {"^ignored.*$"}, // regex value must be dropped
		"body.a": {},
	})
	if got == nil {
		t.Fatal("expected non-nil DocNoise")
	}
	if !reflect.DeepEqual(got.Req, []string{"body.a", "body.z"}) {
		t.Fatalf("req must be sorted paths with no regex values, got %v", got.Req)
	}
	if !reflect.DeepEqual(got.Value, []string{"^tok-.*$"}) {
		t.Fatalf("value mismatch: %v", got.Value)
	}

	if NewDocNoise(nil, nil) != nil {
		t.Fatal("empty inputs must yield nil so omitempty drops the noise key")
	}
}

// TestResolveReqBodyNoise checks decode resolution: request-body schema noise is
// taken from the unified noise.req list, mapping each path to an empty regex list
// (path-only representation). An absent noise block resolves to nil.
func TestResolveReqBodyNoise(t *testing.T) {
	// noise.req present -> its paths become the noise map.
	got := ResolveReqBodyNoise(&DocNoise{Req: []string{"body.a"}})
	if v, ok := got["body.a"]; !ok || len(got) != 1 || len(v) != 0 {
		t.Fatalf("must resolve noise.req to a path-only map, got %v", got)
	}

	// No noise.req -> nil, so omitempty drops the key.
	if ResolveReqBodyNoise(nil) != nil {
		t.Fatal("absent noise block must resolve to nil")
	}
	if ResolveReqBodyNoise(&DocNoise{}) != nil {
		t.Fatal("empty noise.req must resolve to nil")
	}
}
