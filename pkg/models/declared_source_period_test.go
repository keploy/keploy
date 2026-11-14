package models

import (
	"encoding/json"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// A period mark travels with its source: mappings.yaml keeps it (YAML) and
// the scope table hands it to the agent (JSON), the period's value and
// whether the call is a repeat both, and a source without one stays as it
// was written before marks existed.
func TestDeclaredSourcePeriodTravels(t *testing.T) {
	form := &MintedForm{Prefix: "ui-app-data-reconcile:", MinLen: 28, MaxLen: 28}
	for name, s := range map[string]DeclaredSource{
		"a period's first claim":    {Rule: "lease-id@0a1b2c3d", Kind: DeclaredSourceMinted, Field: "body.docs.msgs.0._id", Minted: form, Period: &PeriodMark{Value: "ui-app-data-reconcile:497545"}},
		"a later claim of a period": {Rule: "lease-id@0a1b2c3d", Kind: DeclaredSourceMinted, Field: "body.docs.msgs.0._id", Minted: form, Period: &PeriodMark{Value: "ui-app-data-reconcile:497545", Repeat: true}},
		"a source with no period":   {Rule: "lease-id@0a1b2c3d", Kind: DeclaredSourceMinted, Field: "body.docs.msgs.0._id", Minted: form},
	} {
		y, err := yaml.Marshal(s)
		if err != nil {
			t.Fatalf("%s: yaml: %v", name, err)
		}
		var fromYAML DeclaredSource
		if err := yaml.Unmarshal(y, &fromYAML); err != nil || !reflect.DeepEqual(fromYAML, s) {
			t.Errorf("%s: yaml gave back %+v (%v), want %+v", name, fromYAML, err, s)
		}
		j, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("%s: json: %v", name, err)
		}
		var fromJSON DeclaredSource
		if err := json.Unmarshal(j, &fromJSON); err != nil || !reflect.DeepEqual(fromJSON, s) {
			t.Errorf("%s: json gave back %+v (%v), want %+v", name, fromJSON, err, s)
		}
		if s.Period == nil {
			for _, b := range [][]byte{y, j} {
				if containsPeriodKey(b) {
					t.Errorf("%s: written with a period key: %s", name, b)
				}
			}
		}
	}
}

// containsPeriodKey reports whether b, a source written as YAML or JSON,
// names the period key.
func containsPeriodKey(b []byte) bool {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil && yaml.Unmarshal(b, &m) != nil {
		return true
	}
	_, ok := m["period"]
	return ok
}
