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

// KeepExtra gives a mapping the top-level fields another one read and this
// keploy does not know: a writer that builds the file anew (a per-test
// commit) keeps what a hand edit added there.
func TestKeepExtra(t *testing.T) {
	var old Mapping
	if err := yaml.Unmarshal([]byte("version: v\nkind: TestMocksMapping\ntest_set_id: s\nreviewed_by: agent\n"), &old); err != nil {
		t.Fatal(err)
	}
	fresh := Mapping{Version: "v", Kind: MappingKind, TestSetID: "s"}
	fresh.KeepExtra(&old)
	out, err := yaml.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "reviewed_by: agent\n") {
		t.Fatalf("lost the field:\n%s", out)
	}
	fresh.KeepExtra(nil)
}

// A kept field whose value is an alias of an anchor on a field keploy knows
// reads the same after a rewrite: keploy re-encodes the known field without
// its anchor, so the kept value is the aliased value itself, not *k, which
// would name nothing and make the file unreadable.
func TestAKeptFieldThatAliasesAKnownOneStillReads(t *testing.T) {
	in := "version: api.keploy.io/v1beta1\nkind: &k TestMocksMapping\ntest_set_id: s\nnote: *k\nmore:\n    of: *k\n"
	var m Mapping
	if err := yaml.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("the rewrite does not read back: %v\n%s", err, out)
	}
	if back["note"] != "TestMocksMapping" || back["more"].(map[string]any)["of"] != "TestMocksMapping" {
		t.Fatalf("kept fields read back as %v\n%s", back, out)
	}
}
