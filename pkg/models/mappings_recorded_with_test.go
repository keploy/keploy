package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The set's RecordedWith mark and a moved entry's Owner and Drift round-trip
// through YAML and JSON, with the switch on or off (they are read and carried
// whatever it is), and their absence writes nothing (omitempty): a file
// without them is written as before.
func TestRecordedWithAndAMovesOwnerRoundTrip(t *testing.T) {
	for _, on := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", on)
			m := Mapping{Version: "v1", Kind: MappingKind, TestSetID: "test-set-0",
				RecordedWith: []string{RecordedWithS12Routing},
				TestCases:    []MappedTestCase{{ID: "post-url-1", Mocks: []MockEntry{{Name: "mock-7", Kind: "Mongo"}}}},
				Worker:       []MockEntry{{Name: "mock-6", Kind: "Mongo", Owner: "post-url-1", Drift: true}}}
			y, err := yaml.Marshal(&m)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"recordedWith:\n    - s12-routing", "owner: post-url-1", "drift: true"} {
				if !strings.Contains(string(y), want) {
					t.Fatalf("the YAML lacks %q:\n%s", want, y)
				}
			}
			var back Mapping
			if err := yaml.Unmarshal(y, &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back.RecordedWith, m.RecordedWith) || back.Worker[0].Owner != "post-url-1" || !back.Worker[0].Drift {
				t.Fatalf("YAML round trip lost a field: %+v", back)
			}
			j, err := json.Marshal(&m)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{`"recordedWith":["s12-routing"]`, `"owner":"post-url-1"`, `"drift":true`} {
				if !strings.Contains(string(j), want) {
					t.Fatalf("the JSON lacks %q: %s", want, j)
				}
			}
			back = Mapping{}
			if err := json.Unmarshal(j, &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back.RecordedWith, m.RecordedWith) || back.Worker[0].Owner != "post-url-1" || !back.Worker[0].Drift {
				t.Fatalf("JSON round trip lost a field: %+v", back)
			}

			bare := Mapping{Version: "v1", Kind: MappingKind, TestSetID: "test-set-0",
				Worker: []MockEntry{{Name: "mock-6", Kind: "Mongo"}}}
			y, _ = yaml.Marshal(&bare)
			j, _ = json.Marshal(&bare)
			for _, absent := range []string{"recordedWith", "owner", "drift"} {
				if strings.Contains(string(y), absent) || strings.Contains(string(j), absent) {
					t.Fatalf("an absent %s is written:\n%s\n%s", absent, y, j)
				}
			}
		})
	}
}

// WasRecordedWith names a mark among the set's, and a nil mapping has none.
func TestWasRecordedWith(t *testing.T) {
	var none *Mapping
	if none.WasRecordedWith(RecordedWithS12Routing) {
		t.Fatal("a nil mapping carries a mark")
	}
	m := &MappingDoc{RecordedWith: []string{"other", RecordedWithS12Routing}}
	if !m.WasRecordedWith(RecordedWithS12Routing) || m.WasRecordedWith("s12") {
		t.Fatalf("WasRecordedWith on %v is wrong", m.RecordedWith)
	}
}
