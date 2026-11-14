package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// A test's recorded end goes out and comes back whole beside its begin, in
// YAML and JSON, whatever the switch; a test without one writes no field.
func TestMappedTestCaseEndRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 13, 9, 30, 15, 123456789, time.UTC)
	end := at.Add(1500 * time.Millisecond)
	for _, sw := range []string{"1", ""} {
		t.Setenv(provenswitch.Env, sw)
		in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
			{ID: "pkg::TestA", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, At: at, End: end},
			{ID: "pkg::TestB", At: at},
		}}
		y, err := yaml.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(y), "end:") != 1 {
			t.Fatalf("switch=%q: want one end: field (the test without an end writes none):\n%s", sw, y)
		}
		var fromYAML Mapping
		if err := yaml.Unmarshal(y, &fromYAML); err != nil {
			t.Fatal(err)
		}
		assertEnd(t, sw, fromYAML, at, end)

		j, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(j), `"end"`) != 1 {
			t.Fatalf("switch=%q: want one end field in JSON: %s", sw, j)
		}
		var fromJSON Mapping
		if err := json.Unmarshal(j, &fromJSON); err != nil {
			t.Fatal(err)
		}
		assertEnd(t, sw, fromJSON, at, end)
	}
}

func assertEnd(t *testing.T, sw string, m Mapping, at, end time.Time) {
	t.Helper()
	if len(m.TestCases) != 2 {
		t.Fatalf("switch=%q: got %d tests", sw, len(m.TestCases))
	}
	if a := m.TestCases[0]; !a.At.Equal(at) || !a.End.Equal(end) || len(a.Mocks) != 1 {
		t.Fatalf("switch=%q: pkg::TestA came back as %+v, want at %v end %v", sw, a, at, end)
	}
	if b := m.TestCases[1]; !b.At.Equal(at) || !b.End.IsZero() {
		t.Fatalf("switch=%q: pkg::TestB has no recorded end, got %+v", sw, b)
	}
}

// The end is read in every shape a test's entry has had, and an end: that is
// not a time fails the decode whatever the switch: read in another shape, the
// entry would lose its mocks.
func TestMappedTestCaseEndDecodesInEveryShapeAndRefusesANonTime(t *testing.T) {
	want := time.Date(2026, 11, 13, 9, 30, 16, 0, time.UTC)
	for _, sw := range []string{"1", ""} {
		t.Setenv(provenswitch.Env, sw)
		for _, doc := range []string{
			"id: t1\nend: 2026-11-13T09:30:16Z\nmock_entries:\n  - name: mock-0\n",
			"id: t1\nend: 2026-11-13T09:30:16Z\nmocks:\n  - name: mock-0\n",
			"id: t1\nend: 2026-11-13T09:30:16Z\nmocks: mock-0,mock-1\n",
			"id: t1\nend: 2026-11-13T09:30:16Z\nmocks: [mock-0, mock-1]\n",
			"id: t1\nend: 2026-11-13T09:30:16Z\n",
		} {
			var tc MappedTestCase
			if err := yaml.Unmarshal([]byte(doc), &tc); err != nil || tc.ID != "t1" || !tc.End.Equal(want) || !tc.At.IsZero() {
				t.Fatalf("switch=%q %q decoded as %+v (%v)", sw, doc, tc, err)
			}
		}
		for _, doc := range []string{
			`{"id":"t1","end":"2026-11-13T09:30:16Z","mock_entries":[{"name":"mock-0"}]}`,
			`{"id":"t1","end":"2026-11-13T09:30:16Z","mocks":[{"name":"mock-0"}]}`,
			`{"id":"t1","end":"2026-11-13T09:30:16Z","mocks":"mock-0"}`,
			`{"id":"t1","end":"2026-11-13T09:30:16Z","mocks":["mock-0"]}`,
			`{"id":"t1","end":"2026-11-13T09:30:16Z"}`,
		} {
			var tc MappedTestCase
			if err := json.Unmarshal([]byte(doc), &tc); err != nil || tc.ID != "t1" || !tc.End.Equal(want) || !tc.At.IsZero() {
				t.Fatalf("switch=%q %s decoded as %+v (%v)", sw, doc, tc, err)
			}
		}
		for _, doc := range []string{
			"id: t1\nend: tomorrow\nmock_entries:\n  - name: mock-0\n",
			"id: t1\nend: [not, a, time]\nmocks: mock-0\n",
		} {
			var tc MappedTestCase
			if err := yaml.Unmarshal([]byte(doc), &tc); err == nil || !strings.Contains(err.Error(), "recorded end (end) is not a time") {
				t.Fatalf("switch=%q %q: want a not-a-time error, got %v (%+v)", sw, doc, err, tc)
			}
		}
		for _, doc := range []string{
			`{"id":"t1","end":"tomorrow","mock_entries":[{"name":"mock-0"}]}`,
			`{"id":"t1","end":42,"mocks":"mock-0"}`,
		} {
			var tc MappedTestCase
			if err := json.Unmarshal([]byte(doc), &tc); err == nil || !strings.Contains(err.Error(), "recorded end (end) is not a time") {
				t.Fatalf("switch=%q %s: want a not-a-time error, got %v (%+v)", sw, doc, err, tc)
			}
		}
		// A key named end inside a mock entry is that entry's, not the test's.
		var nested MappedTestCase
		if err := yaml.Unmarshal([]byte("id: t1\nmock_entries:\n  - name: mock-0\n    end: tomorrow\n"), &nested); err != nil || !nested.End.IsZero() {
			t.Fatalf("switch=%q: a nested end: was read as the test's: %+v (%v)", sw, nested, err)
		}
	}
}
