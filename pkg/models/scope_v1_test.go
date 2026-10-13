package models

import (
	"strings"
	"testing"
)

// A test's ID is [project::](file|key)::name, and a name holding "::" is
// escaped so the ID's last "::" always ends its path.
func TestTestID(t *testing.T) {
	for _, tc := range []struct {
		project, file, key, name, want string
	}{
		{"", "", "orders", "TestA", "orders::TestA"},
		{"svc", "orders/x_test.go", "orders", "TestA", "svc::orders/x_test.go::TestA"},
		{"", "", "", "TestA", "TestA"},
		{"", "", "net", "TestDial/[::1]:80", "net::TestDial/[%3A%3A1]:80"},
		{"", "", "net", "TestDial/50%/[::1]", "net::TestDial/50%25/[%3A%3A1]"},
		{"", "", "net", "TestDial/50%", "net::TestDial/50%"},
		// A name that already reads like an escape is escaped, so it never
		// shares an ID with the name it would decode to.
		{"", "", "k", "TestX/a%3A%3Ab", "k::TestX/a%253A%253Ab"},
		{"", "", "k", "TestX/a::b", "k::TestX/a%3A%3Ab"},
		{"", "", "k", "TestX/50%25", "k::TestX/50%2525"},
		// A leading ":" would move the split to before it.
		{"", "", "k", ":x", "k::%3Ax"},
		{"", "", "k", ":::x", "k::%3A%3A:x"},
		{"", "tests/x.py", "k", "TestCls::test_x", "tests/x.py::TestCls%3A%3Atest_x"},
	} {
		if got := TestID(tc.project, tc.file, tc.key, tc.name); got != tc.want {
			t.Errorf("TestID(%q, %q, %q, %q) = %q, want %q", tc.project, tc.file, tc.key, tc.name, got, tc.want)
		}
	}
}

// Every name up to five characters from the characters escaping touches gets
// an ID of its own, and the ID's last "::" ends its path, for keys IDPart
// allows: what enterprise reads back (umproven.IsUMID) is what the agent
// built.
func TestTestIDsAreDistinctAndSplitAtThePath(t *testing.T) {
	const alphabet = ":%3A25x"
	names := []string{""}
	all := []string{}
	for n := 1; n <= 5; n++ {
		var next []string
		for _, p := range names {
			for _, c := range alphabet {
				next = append(next, p+string(c))
			}
		}
		names = next
		all = append(all, next...)
	}
	for _, key := range []string{"k", "k:x", "a/b"} {
		if !IDPart(key) {
			t.Fatalf("IDPart(%q) = false", key)
		}
		seen := make(map[string]string, len(all))
		for _, name := range all {
			id := TestID("", "", key, name)
			if prev, ok := seen[id]; ok {
				t.Fatalf("key %q: names %q and %q share the ID %q", key, prev, name, id)
			}
			seen[id] = name
			i := strings.LastIndex(id, "::")
			if i < 0 || id[:i] != key || id[i+2:] != escapeTestName(name) {
				t.Fatalf("key %q, name %q: ID %q splits at %d", key, name, id, i)
			}
		}
	}
	for _, bad := range []string{"a::b", ":a", "a:", "::"} {
		if IDPart(bad) {
			t.Errorf("IDPart(%q) = true", bad)
		}
	}
}
