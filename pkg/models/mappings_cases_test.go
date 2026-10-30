package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
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

// Under the proven switch an entry with both mock lists is refused, naming
// the test: keploy reads mock_entries only, so a mocks: list beside it (a hand
// edit, or an undo followed to the letter) would be dropped without a word,
// and the next rewrite would erase it. With the switch off it reads as before.
func TestAnEntryWithBothMockListsIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name, doc string
		json      bool
	}{
		{"yaml", "id: post-url-1\nmock_entries:\n  - name: m1\nmocks:\n  - name: mock-6\n", false},
		{"yaml empty mock_entries", "id: post-url-1\nmock_entries: []\nmocks:\n  - name: mock-6\n", false},
		{"json", `{"id":"post-url-1","mock_entries":[{"name":"m1"}],"mocks":[{"name":"mock-6"}]}`, true},
		{"yaml names as a string", "id: post-url-1\nmock_entries:\n  - name: m1\nmocks: \"mock-6,mock-7\"\n", false},
		{"yaml mocks through a merge", "<<: {mocks: \"mock-6,mock-7\"}\nid: post-url-1\nmock_entries:\n  - name: m1\n", false},
		{"json names as a string", `{"id":"post-url-1","mock_entries":[{"name":"m1"}],"mocks":"mock-6,mock-7"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got MappedTestCase
			var err error
			if tc.json {
				err = json.Unmarshal([]byte(tc.doc), &got)
			} else {
				err = yaml.Unmarshal([]byte(tc.doc), &got)
			}
			if err == nil {
				t.Fatalf("read as %+v; want a refusal", got)
			}
			for _, want := range []string{"post-url-1", "`mocks`", "`mock_entries`"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %s", err, want)
				}
			}
		})
	}
	// An empty or null mocks list loses nothing when it is not read: the
	// entry reads as mock_entries.
	for _, doc := range []string{"id: t\nmock_entries:\n  - name: m1\nmocks: []\n", "id: t\nmock_entries:\n  - name: m1\nmocks:\n", "id: t\nmock_entries:\n  - name: m1\nmocks: {}\n"} {
		var got MappedTestCase
		if err := yaml.Unmarshal([]byte(doc), &got); err != nil || len(got.Mocks) != 1 || got.Mocks[0].Name != "m1" {
			t.Fatalf("%q read as %+v, %v", doc, got, err)
		}
	}
	for _, doc := range []string{`{"id":"t","mock_entries":[{"name":"m1"}],"mocks":[]}`, `{"id":"t","mock_entries":[{"name":"m1"}],"mocks":{}}`} {
		var got MappedTestCase
		if err := json.Unmarshal([]byte(doc), &got); err != nil || len(got.Mocks) != 1 {
			t.Fatalf("%s read as %+v, %v", doc, got, err)
		}
	}
}

// With the proven switch off an entry with both mock lists reads as it did
// before keploy refused it: mock_entries alone.
func TestAnEntryWithBothMockListsReadsAsBeforeWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	var got MappedTestCase
	if err := yaml.Unmarshal([]byte("id: t\nmock_entries:\n  - name: m1\nmocks:\n  - name: mock-6\n"), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "t" || !reflect.DeepEqual(got.MockNames(), []string{"m1"}) {
		t.Fatalf("came back as %+v; want t with m1 alone", got)
	}
}

// MockPlaces says where each name is: a test's own list by its id, and each
// set section by its key, in the file's order, once each.
func TestMockPlaces(t *testing.T) {
	m := &Mapping{
		TestCases: []MappedTestCase{{ID: "o::TestA", Mocks: []MockEntry{{Name: "mock-0"}, {Name: "mock-1"}}}, {ID: "o::TestB", Mocks: []MockEntry{{Name: "mock-1"}}}},
		Startup:   []MockEntry{{Name: "mock-2"}},
		Worker:    []MockEntry{{Name: "mock-3"}, {Name: "mock-1"}},
		Shared:    []MockEntry{{Name: "mock-4"}},
		Runner:    []MockEntry{{Name: "mock-5"}},
		Boots:     []BootSpec{{Mocks: []MockEntry{{Name: "mock-6"}}}},
	}
	want := map[string][]string{
		"mock-0": {"test o::TestA"},
		"mock-1": {"test o::TestA", "test o::TestB", "worker:"},
		"mock-2": {"startup:"}, "mock-3": {"worker:"}, "mock-4": {"shared:"}, "mock-5": {"runner:"}, "mock-6": {"boots:"},
	}
	if got := m.MockPlaces(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A test entry whose mock list a merge key (<<) brings reads with it, as
// the both-lists refusal sees it, with the switch on or off: before, it read
// with no mocks, and the next rewrite erased them.
func TestAMergeKeyGivesATestEntryItsMocks(t *testing.T) {
	for _, on := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, on)
		var m Mapping
		doc := "x-a: &a\n    mock_entries:\n        - name: mock-0\n        - name: mock-1\ntests:\n    - <<: *a\n      id: t2\n"
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("switch %q: %v", on, err)
		}
		if len(m.TestCases) != 1 || m.TestCases[0].ID != "t2" || len(m.TestCases[0].Mocks) != 2 {
			t.Fatalf("switch %q: read as %+v; want t2 with mock-0 and mock-1", on, m.TestCases)
		}
	}
}
