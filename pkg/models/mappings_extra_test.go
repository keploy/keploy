package models

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A mapping keeps the fields it read that this keploy does not know, at the
// top, on a test entry and on a mock entry, and writes them back after its
// own; a mapping without any reads as one built in code.
func TestAMappingKeepsFieldsItDoesNotKnow(t *testing.T) {
	in := "version: v\nkind: TestMocksMapping\ntest_set_id: s\nreviewed_by: agent\ntests:\n    - id: o::TestA\n      ticket: JIRA-1\n      mock_entries:\n        - name: mock-0\n          kind: Http\n          note: keep\n"
	var m Mapping
	if err := yaml.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reviewed_by: agent\n", "ticket: JIRA-1\n", "note: keep\n", "test_set_id: s\n", "- name: mock-0\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("lost %q:\n%s", want, out)
		}
	}

	plain := "version: v\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n          kind: Http\n"
	var p Mapping
	if err := yaml.Unmarshal([]byte(plain), &p); err != nil {
		t.Fatal(err)
	}
	if p.extra != nil || p.TestCases[0].extra != nil || p.TestCases[0].Mocks[0].extra != nil {
		t.Fatalf("a mapping with no unknown field read as having some: %+v", p)
	}
}
