package models

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMappedTestCaseCasesRoundTrip(t *testing.T) {
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
		{ID: "t1", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, Cases: []string{"test-1", "test-3"}},
		{ID: "t2", Cases: []string{"test-2"}},
	}}

	y, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(y), "cases:") {
		t.Fatalf("cases were not written:\n%s", y)
	}
	var fromYAML Mapping
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatal(err)
	}
	assertCases(t, fromYAML)

	j, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON Mapping
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	assertCases(t, fromJSON)
}

func assertCases(t *testing.T, m Mapping) {
	t.Helper()
	if len(m.TestCases) != 2 {
		t.Fatalf("got %d tests", len(m.TestCases))
	}
	t1, t2 := m.TestCases[0], m.TestCases[1]
	if len(t1.Cases) != 2 || t1.Cases[0] != "test-1" || t1.Cases[1] != "test-3" || len(t1.Mocks) != 1 || t1.Mocks[0].Name != "mock-0" {
		t.Fatalf("t1 came back as %+v", t1)
	}
	if len(t2.Cases) != 1 || t2.Cases[0] != "test-2" || len(t2.Mocks) != 0 {
		t.Fatalf("t2 came back as %+v", t2)
	}
}

// A mapping written before the field existed reads back with no cases, in every format it has had.
func TestMappedTestCaseWithoutCasesStillDecodes(t *testing.T) {
	for _, doc := range []string{
		"id: t1\nmock_entries:\n  - name: mock-0\n",
		"id: t1\nmocks:\n  - name: mock-0\n",
		"id: t1\nmocks: mock-0,mock-1\n",
	} {
		var tc MappedTestCase
		if err := yaml.Unmarshal([]byte(doc), &tc); err != nil {
			t.Fatal(err)
		}
		if tc.ID != "t1" || len(tc.Mocks) == 0 || len(tc.Cases) != 0 {
			t.Fatalf("%q came back as %+v", doc, tc)
		}
	}
}
