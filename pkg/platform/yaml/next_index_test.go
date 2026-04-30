package yaml

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("stub"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestNextIndexForPrefix_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := NextIndexForPrefix(dir, "get-users")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 1 {
		t.Fatalf("got=%d want=1", got)
	}
}

func TestNextIndexForPrefix_MissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	got, err := NextIndexForPrefix(dir, "get-users")
	if err != nil {
		t.Fatalf("unexpected err for missing dir: %v", err)
	}
	if got != 1 {
		t.Fatalf("got=%d want=1 for missing dir", got)
	}
}

func TestNextIndexForPrefix_ExistingMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"get-users-1.yaml", "get-users-2.yaml", "get-users-5.yaml"} {
		writeTestFile(t, dir, f)
	}
	got, err := NextIndexForPrefix(dir, "get-users")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 6 {
		t.Fatalf("got=%d want=6", got)
	}
}

func TestNextIndexForPrefix_IgnoresNonMatching(t *testing.T) {
	dir := t.TempDir()
	// non-matching prefix
	writeTestFile(t, dir, "post-users-1.yaml")
	writeTestFile(t, dir, "post-users-99.yaml")
	// same-prefix but non-numeric suffix
	writeTestFile(t, dir, "get-users-abc.yaml")
	// wrong extension
	writeTestFile(t, dir, "get-users-3.json")
	// no separator
	writeTestFile(t, dir, "get-users.yaml")
	// matching
	writeTestFile(t, dir, "get-users-2.yaml")
	// substring collision that must not match
	writeTestFile(t, dir, "get-users-ext-4.yaml")

	got, err := NextIndexForPrefix(dir, "get-users")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 3 {
		t.Fatalf("got=%d want=3 (only get-users-2.yaml should count)", got)
	}
}

func TestNextIndexForPrefix_EmptyPrefix(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "anything-1.yaml")
	got, err := NextIndexForPrefix(dir, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 1 {
		t.Fatalf("got=%d want=1 for empty prefix", got)
	}
}

func TestNextIndexForPrefix_RejectsUnsafePrefix(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"../escape", "foo/bar", `foo\bar`, ".."}
	for _, p := range cases {
		if _, err := NextIndexForPrefix(dir, p); err == nil {
			t.Errorf("expected error for prefix %q, got nil", p)
		}
	}
}

func TestNextIndexForPrefix_RejectsTraversalPath(t *testing.T) {
	if _, err := NextIndexForPrefix("/tmp/../etc", "get-users"); err == nil {
		t.Fatalf("expected error for path with '..'")
	}
}
