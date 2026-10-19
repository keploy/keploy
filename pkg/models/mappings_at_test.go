package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// A test's recorded begin goes out and comes back whole, in YAML and JSON, and
// a test without one writes no field.
func TestMappedTestCaseAtRoundTrip(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := time.Date(2026, 10, 6, 9, 30, 15, 123456789, time.UTC)
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
		{ID: "pkg::TestA", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, At: at},
		{ID: "pkg::TestB"},
	}}

	y, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(y), "at:") != 1 {
		t.Fatalf("want one at: field (the test without a begin writes none):\n%s", y)
	}
	var fromYAML Mapping
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatal(err)
	}
	assertAt(t, fromYAML, at)

	j, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(j), `"at"`) != 1 {
		t.Fatalf("want one at field in JSON: %s", j)
	}
	var fromJSON Mapping
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	assertAt(t, fromJSON, at)
}

func assertAt(t *testing.T, m Mapping, at time.Time) {
	t.Helper()
	if len(m.TestCases) != 2 {
		t.Fatalf("got %d tests", len(m.TestCases))
	}
	if !m.TestCases[0].At.Equal(at) {
		t.Fatalf("pkg::TestA began at %v, want %v", m.TestCases[0].At, at)
	}
	if !m.TestCases[1].At.IsZero() {
		t.Fatalf("pkg::TestB has no recorded begin, got %v", m.TestCases[1].At)
	}
}

// The begin is read in every shape a test's entry has had, including one with
// no mocks at all.
func TestMappedTestCaseAtDecodesInEveryShape(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	want := time.Date(2026, 10, 6, 9, 30, 15, 0, time.UTC)
	for _, doc := range []string{
		"id: t1\nat: 2026-10-06T09:30:15Z\nmock_entries:\n  - name: mock-0\n",
		"id: t1\nat: 2026-10-06T09:30:15Z\nmocks:\n  - name: mock-0\n",
		"id: t1\nat: 2026-10-06T09:30:15Z\nmocks: mock-0,mock-1\n",
		"id: t1\nat: 2026-10-06T09:30:15Z\nmocks: [mock-0, mock-1]\n",
		"id: t1\nat: 2026-10-06T09:30:15Z\n",
	} {
		var tc MappedTestCase
		if err := yaml.Unmarshal([]byte(doc), &tc); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if tc.ID != "t1" || !tc.At.Equal(want) {
			t.Fatalf("%q decoded as %+v", doc, tc)
		}
	}
	for _, doc := range []string{
		`{"id":"t1","at":"2026-10-06T09:30:15Z","mock_entries":[{"name":"mock-0"}]}`,
		`{"id":"t1","at":"2026-10-06T09:30:15Z","mocks":[{"name":"mock-0"}]}`,
		`{"id":"t1","at":"2026-10-06T09:30:15Z","mocks":"mock-0"}`,
		`{"id":"t1","at":"2026-10-06T09:30:15Z","mocks":["mock-0"]}`,
		`{"id":"t1","at":"2026-10-06T09:30:15Z"}`,
	} {
		var tc MappedTestCase
		if err := json.Unmarshal([]byte(doc), &tc); err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if tc.ID != "t1" || !tc.At.Equal(want) {
			t.Fatalf("%s decoded as %+v", doc, tc)
		}
	}
}

// Under the proven switch, a recorded begin that is not a time fails the
// decode: read in another shape, the entry would lose its mocks.
func TestMappedTestCaseRefusesABeginThatIsNotATime(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, doc := range []string{
		"id: t1\nat: yesterday\nmock_entries:\n  - name: mock-0\n",
		"id: t1\nat: 2026-10-06 09:30:15 +0000 UTC\nmock_entries:\n  - name: mock-0\n",
	} {
		var tc MappedTestCase
		if err := yaml.Unmarshal([]byte(doc), &tc); err == nil || !strings.Contains(err.Error(), "not a time") {
			t.Fatalf("%q: want a not-a-time error, got %v (%+v)", doc, err, tc)
		}
	}
	for _, doc := range []string{
		`{"id":"t1","at":"yesterday","mock_entries":[{"name":"mock-0"}]}`,
		`{"id":"t1","at":"2026-10-06T09:30:15","mock_entries":[{"name":"mock-0"}]}`,
	} {
		var tc MappedTestCase
		if err := json.Unmarshal([]byte(doc), &tc); err == nil || !strings.Contains(err.Error(), "not a time") {
			t.Fatalf("%s: want a not-a-time error, got %v (%+v)", doc, err, tc)
		}
	}
}

// With the proven switch off, a test entry reads and writes as it did before
// keploy kept the begin: at: is not read, whatever it holds, so an entry keeps
// its mocks and its shape; and an entry without a begin is written byte for
// byte as the persisted format alone.
func TestMappedTestCaseWithoutTheSwitchIgnoresAt(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	for _, doc := range []string{
		"id: t1\nat: yesterday\nmock_entries:\n  - name: mock-0\n",
		"id: t1\nat: 2026-10-06T09:30:15Z\nmock_entries:\n  - name: mock-0\n",
		"id: t1\nat: [not, a, time]\nmocks: mock-0\n",
	} {
		var tc MappedTestCase
		if err := yaml.Unmarshal([]byte(doc), &tc); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if tc.ID != "t1" || len(tc.Mocks) != 1 || tc.Mocks[0].Name != "mock-0" || !tc.At.IsZero() {
			t.Fatalf("%q decoded as %+v", doc, tc)
		}
	}
	for _, doc := range []string{
		`{"id":"t1","at":"yesterday","mock_entries":[{"name":"mock-0"}]}`,
		`{"id":"t1","at":"2026-10-06T09:30:15Z","mock_entries":[{"name":"mock-0"}]}`,
		`{"id":"t1","at":42,"mocks":"mock-0"}`,
	} {
		var tc MappedTestCase
		if err := json.Unmarshal([]byte(doc), &tc); err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if tc.ID != "t1" || len(tc.Mocks) != 1 || tc.Mocks[0].Name != "mock-0" || !tc.At.IsZero() {
			t.Fatalf("%s decoded as %+v", doc, tc)
		}
	}

	tc := MappedTestCase{ID: "t1", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, Cases: []string{"c1"}, Dir: "pkg", Starts: 2}
	y, err := yaml.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	old, err := yaml.Marshal(tc.persistedFormat())
	if err != nil {
		t.Fatal(err)
	}
	if string(y) != string(old) {
		t.Fatalf("YAML without a begin changed:\n%s\nwas:\n%s", y, old)
	}
	j, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	oldJ, err := json.Marshal(tc.persistedFormat())
	if err != nil {
		t.Fatal(err)
	}
	if string(j) != string(oldJ) {
		t.Fatalf("JSON without a begin changed:\n%s\nwas:\n%s", j, oldJ)
	}
}
