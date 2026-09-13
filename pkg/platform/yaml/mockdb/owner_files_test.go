// Per-owner mock files: one file per test-id, so replacing one test's
// recording is a whole-file write and never has to edit around the other
// tests that used to share mocks.yaml.
package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"
)

func insertOwned(t *testing.T, ys *MockYaml, set string, owners ...string) {
	t.Helper()
	for _, o := range owners {
		if err := ys.InsertMock(context.Background(), ownerTestMock(o), set); err != nil {
			t.Fatalf("InsertMock(owner=%q): %v", o, err)
		}
	}
}

func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func readAll(t *testing.T, ys *MockYaml, set string) int {
	t.Helper()
	zero, wide := time.Time{}, time.Unix(1<<40, 0)
	f, err := ys.GetFilteredMocks(context.Background(), set, zero, wide, nil, nil)
	if err != nil {
		t.Fatalf("GetFilteredMocks: %v", err)
	}
	u, err := ys.GetUnFilteredMocks(context.Background(), set, zero, wide, nil, nil)
	if err != nil {
		t.Fatalf("GetUnFilteredMocks: %v", err)
	}
	return len(f) + len(u)
}

// Each owner gets its own file, named by the same 12-hex hash that names its
// mocks. Nothing lands in mocks.yaml, because nothing here is unowned.
func TestEachOwnerGetsItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	a, b := "spec.ts > d > test A", "spec.ts > d > test B"
	insertOwned(t, ys, "set-0", a, a, b)

	got := filesIn(t, filepath.Join(dir, "set-0"))
	want := []string{ownerHash(a) + ".yaml", ownerHash(b) + ".yaml"}
	sort.Strings(want)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("files on disk = %v, want %v", got, want)
	}
	if n := readAll(t, ys, "set-0"); n != 3 {
		t.Fatalf("read back %d mocks, want 3", n)
	}
}

// An unowned capture keeps landing in mocks.yaml. This is what makes the
// change additive: a runner that reports no scopes writes exactly the one
// file it always wrote.
func TestUnownedStillLandsInMocksYaml(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	insertOwned(t, ys, "set-0", "")

	if got := filesIn(t, filepath.Join(dir, "set-0")); len(got) != 1 || got[0] != "mocks.yaml" {
		t.Fatalf("files on disk = %v, want [mocks.yaml]", got)
	}
	if n := readAll(t, ys, "set-0"); n != 1 {
		t.Fatalf("read back %d mocks, want 1", n)
	}
}

// Mixed set: owners in their own files, unowned in mocks.yaml, and the read
// path returns the union.
func TestMixedOwnedAndUnownedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	a := "spec.ts > d > owned"
	insertOwned(t, ys, "set-0", a, "", a)

	got := filesIn(t, filepath.Join(dir, "set-0"))
	want := []string{"mocks.yaml", ownerHash(a) + ".yaml"}
	sort.Strings(want)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("files on disk = %v, want %v", got, want)
	}
	if n := readAll(t, ys, "set-0"); n != 3 {
		t.Fatalf("read back %d mocks, want 3", n)
	}
}

// Rewriting one owner's file must not disturb another's. This is the property
// the whole per-owner scheme exists for -- it is why a re-record of one test
// can no longer require wiping the set.
func TestRewritingOneOwnerLeavesOthersByteIdentical(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	a, b := "spec.ts > d > test A", "spec.ts > d > test B"
	insertOwned(t, ys, "set-0", a, b)

	bPath := filepath.Join(dir, "set-0", ownerHash(b)+".yaml")
	before, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatalf("read B before: %v", err)
	}

	insertOwned(t, ys, "set-0", a)

	after, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatalf("read B after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("owner B's file changed when owner A was re-written:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// An explicit mock name still addresses exactly one file: that flag names a
// file, so honouring it is what lets a caller read one owner in isolation.
func TestExplicitMockNameStillWritesOneFile(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	insertOwned(t, ys, "set-0", "spec.ts > d > test A", "spec.ts > d > test B")

	if got := filesIn(t, filepath.Join(dir, "set-0")); len(got) != 1 || got[0] != "mocks.yaml" {
		t.Fatalf("files on disk = %v, want [mocks.yaml]", got)
	}
}

// Clearing a set must clear every owner's file, including owners that the
// next recording will not produce.
func TestDeleteMocksForSetRemovesEveryOwnerFile(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	insertOwned(t, ys, "set-0", "spec.ts > d > A", "spec.ts > d > B", "")

	if err := ys.DeleteMocksForSet(context.Background(), "set-0"); err != nil {
		t.Fatalf("DeleteMocksForSet: %v", err)
	}
	if got := filesIn(t, filepath.Join(dir, "set-0")); len(got) != 0 {
		t.Fatalf("files left after delete = %v, want none", got)
	}
}

// mockFileBases ignores anything that is not a mock file, which is what lets
// per-owner files sit flat beside config.yaml and mappings.yaml.
func TestMockFileBasesIgnoresNonMockFiles(t *testing.T) {
	dir := t.TempDir()
	set := filepath.Join(dir, "set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"config.yaml", "mappings.yaml", "README.md", "0123456789ab.yaml", "mocks.yaml"} {
		if err := os.WriteFile(filepath.Join(set, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bases, err := mockFileBases(set)
	if err != nil {
		t.Fatalf("mockFileBases: %v", err)
	}
	want := []string{"mocks", "0123456789ab"}
	if len(bases) != len(want) || bases[0] != want[0] || bases[1] != want[1] {
		t.Fatalf("bases = %v, want %v (unowned first, then owners sorted)", bases, want)
	}
}
