package models

import (
	"encoding/json"
	"reflect"
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
		if tc.ID != "t1" || len(tc.Mocks) == 0 || len(tc.Cases) != 0 || tc.CaseMocks != nil || tc.CaseSteps != nil {
			t.Fatalf("%q came back as %+v", doc, tc)
		}
	}
}

func TestMappedTestCaseCaseMocksAndStepsRoundTrip(t *testing.T) {
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set",
		TestCases: []MappedTestCase{{
			ID:        "orders/e2e.TestA",
			Mocks:     []MockEntry{{Name: "mock-1"}, {Name: "mock-2"}, {Name: "mock-3"}},
			Cases:     []string{"test-1", "test-2"},
			CaseMocks: map[string][]string{"test-1": {"mock-1", "mock-2"}},
			CaseSteps: map[string]string{"test-1": "create", "test-2": ""},
		}},
		Startup: []MockEntry{{Name: "mock-0"}},
	}
	y, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"case_mocks:\n", "case_steps:\n", `test-2: ""`, "startup:\n"} {
		if !strings.Contains(string(y), want) {
			t.Fatalf("%q missing from:\n%s", want, y)
		}
	}
	var fromYAML Mapping
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON Mapping
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	for _, got := range []Mapping{fromYAML, fromJSON} {
		if !reflect.DeepEqual(in, got) {
			t.Fatalf("came back as %+v", got)
		}
	}

	var old struct {
		Tests []struct {
			ID          string      `yaml:"id"`
			MockEntries []MockEntry `yaml:"mock_entries"`
		} `yaml:"tests"`
	}
	if err := yaml.Unmarshal(y, &old); err != nil || len(old.Tests) != 1 || len(old.Tests[0].MockEntries) != 3 {
		t.Fatalf("an older reader read %+v, %v", old, err)
	}
}

func TestMappedTestCaseKeepsMocksBesideCases(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		json      bool
		mocks     []string
	}{
		{"json structured mocks", `{"id":"t","mocks":[{"name":"m1","kind":"Http"}],"cases":["c1"]}`, true, []string{"m1"}},
		{"yaml structured mocks", "id: t\nmocks:\n  - name: m1\n    kind: Http\ncases: [c1]\n", false, []string{"m1"}},
		{"yaml legacy string", "id: t\nmocks: \"m1,m2\"\ncases: [c1]\n", false, []string{"m1", "m2"}},
		{"json legacy string", `{"id":"t","mocks":"m1,m2","cases":["c1"]}`, true, []string{"m1", "m2"}},
		{"yaml persisted", "id: t\nmock_entries:\n  - name: m1\ncases: [c1]\n", false, []string{"m1"}},
		{"yaml cases only", "id: t\ncases: [c1]\n", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got MappedTestCase
			var err error
			if tc.json {
				err = json.Unmarshal([]byte(tc.doc), &got)
			} else {
				err = yaml.Unmarshal([]byte(tc.doc), &got)
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			if len(got.Mocks) > 0 {
				names = got.MockNames()
			}
			if got.ID != "t" || !reflect.DeepEqual(names, tc.mocks) || !reflect.DeepEqual(got.Cases, []string{"c1"}) {
				t.Fatalf("came back as %+v", got)
			}
		})
	}
}

func TestMappedTestCaseKeepsMocksWrittenBesideAFolder(t *testing.T) {
	var fromJSON MappedTestCase
	if err := json.Unmarshal([]byte(`{"id":"T","mocks":[{"name":"mock-1"}],"dir":"pkg/x","starts":2}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	var fromYAML MappedTestCase
	if err := yaml.Unmarshal([]byte("id: T\nmocks:\n  - name: mock-1\ndir: pkg/x\nstarts: 2\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []MappedTestCase{fromJSON, fromYAML} {
		if len(tc.Mocks) != 1 || tc.Mocks[0].Name != "mock-1" || tc.Dir != "pkg/x" || tc.Starts != 2 {
			t.Fatalf("mocks, folder and starts must all be read: %+v", tc)
		}
	}
}
