package replay

import (
	"testing"
)

// TestBodyNoiseForTestCase covers the schema-addition helper's share of the
// sectioned-noise contract. It had no test before.
func TestBodyNoiseForTestCase(t *testing.T) {
	cases := []struct {
		name     string
		tcNoise  map[string][]string
		global   map[string]map[string][]string
		wantKeys []string
	}{
		{
			name:     "dotted key is scoped to its path",
			tcNoise:  map[string][]string{"body.user.id": {".*"}},
			wantKeys: []string{"user.id"},
		},
		{
			name:     "sectioned key lists paths rather than skipping the body",
			tcNoise:  map[string][]string{"body": {"stock", "Total"}},
			wantKeys: []string{"stock", "total"},
		},
		{
			name:     "both shapes merge",
			tcNoise:  map[string][]string{"body": {"stock"}, "body.created_at": {}},
			wantKeys: []string{"stock", "created_at"},
		},
		{
			name:     "empty sectioned key contributes nothing",
			tcNoise:  map[string][]string{"body": {}},
			wantKeys: nil,
		},
		{
			name:     "header noise does not bleed into body noise",
			tcNoise:  map[string][]string{"header": {"Date"}, "header.Etag": {}},
			wantKeys: nil,
		},
		{
			name:     "global body noise is preserved",
			tcNoise:  map[string][]string{"body": {"stock"}},
			global:   map[string]map[string][]string{"body": {"global_field": {}}},
			wantKeys: []string{"stock", "global_field"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			global := tc.global
			if global == nil {
				global = map[string]map[string][]string{}
			}
			got := bodyNoiseForTestCase(tc.tcNoise, global)
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("bodyNoise = %v, want keys %v", got, tc.wantKeys)
			}
			for _, k := range tc.wantKeys {
				if _, ok := got[k]; !ok {
					t.Errorf("bodyNoise = %v, missing key %q", got, k)
				}
			}
		})
	}
}

// TestBodyNoiseForTestCase_DoesNotMutateGlobal pins that the helper copies the
// global config rather than widening it for the rest of the run.
func TestBodyNoiseForTestCase_DoesNotMutateGlobal(t *testing.T) {
	global := map[string]map[string][]string{"body": {}}
	bodyNoiseForTestCase(map[string][]string{"body": {"stock"}, "body.total": {}}, global)
	if len(global["body"]) != 0 {
		t.Errorf("global noise config was mutated: %v", global["body"])
	}
}
