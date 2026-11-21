package record

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// fakeDeleter records each DeleteTests call and fails the names in fail; a
// batch of more than one name fails when batchFails is set, after deleting
// those before the first failing one, as a store that stops part way does.
type fakeDeleter struct {
	calls      [][]string
	fail       map[string]error
	batchFails bool
	gone       map[string]bool
}

func (f *fakeDeleter) DeleteTests(_ context.Context, _ string, ids []string) error {
	f.calls = append(f.calls, append([]string(nil), ids...))
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	for i, id := range ids {
		if f.gone[id] {
			return fmt.Errorf("delete %s: %w", id, fs.ErrNotExist)
		}
		if err := f.fail[id]; err != nil || (f.batchFails && len(ids) > 1 && i == len(ids)/2) {
			if err == nil {
				err = errors.New("the store stopped part way")
			}
			return err
		}
		f.gone[id] = true
	}
	return nil
}

func deletedNames(m map[string]struct{}) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// The revoked test cases are deleted in one call: a shared loss can revoke
// thousands, and a store may make each call a round trip.
func TestDeleteRevokedAsksForThemAllInOneCall(t *testing.T) {
	f := &fakeDeleter{}
	got := deleteRevoked(context.Background(), zap.NewNop(), f, "set", []string{"a", "b", "c"})
	if len(f.calls) != 1 || len(f.calls[0]) != 3 {
		t.Fatalf("DeleteTests calls %v, want one with all three names", f.calls)
	}
	if fmt.Sprint(deletedNames(got)) != "[a b c]" {
		t.Fatalf("deleted %v, want [a b c]", deletedNames(got))
	}
}

// When the one call fails, each name is deleted on its own: one that cannot
// be deleted stays in the set and is not counted, the others are, and one
// the failed batch had deleted already (gone on the retry) counts as deleted.
func TestDeleteRevokedFallsBackToOneByOneWhenTheBatchFails(t *testing.T) {
	f := &fakeDeleter{fail: map[string]error{"c": errors.New("permission denied")}}
	got := deleteRevoked(context.Background(), zap.NewNop(), f, "set", []string{"a", "b", "c", "d"})
	if len(f.calls) != 5 {
		t.Fatalf("DeleteTests calls %v, want the batch then one per name", f.calls)
	}
	if fmt.Sprint(deletedNames(got)) != "[a b d]" {
		t.Fatalf("deleted %v, want [a b d]: c could not be deleted, a and b were deleted by the batch that stopped at c", deletedNames(got))
	}
	f = &fakeDeleter{batchFails: true}
	got = deleteRevoked(context.Background(), zap.NewNop(), f, "set", []string{"a", "b", "c", "d"})
	if fmt.Sprint(deletedNames(got)) != "[a b c d]" {
		t.Fatalf("deleted %v after a batch that stopped part way, want all four", deletedNames(got))
	}
}

// The yaml store's delete of a file that is not there (DeleteRecordingFileF,
// which its DeleteTests calls) says so as fs.ErrNotExist, which
// deleteRevoked's retry reads as deleted.
func TestDeleteRecordingFileSaysAMissingFileIsNotThere(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := yaml.DeleteRecordingFileF(zap.NewNop(), dir, dir, "x", yaml.FormatYAML); err != nil {
		t.Fatalf("deleting a file that is there: %v", err)
	}
	if err := yaml.DeleteRecordingFileF(zap.NewNop(), dir, dir, "x", yaml.FormatYAML); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleting a file that is not there: %v, want one that is fs.ErrNotExist", err)
	}
}

// With the yaml store, a batch that stops at a test case it cannot delete has
// deleted those before it; the one-by-one retry deletes the rest, finds the
// earlier ones gone and counts them deleted, and the only failure said is the
// test case that stays: no ERROR claims a deleted test case failed.
func TestDeleteRevokedWithTheYamlStoreSaysOnlyTheTestCaseThatStays(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "set", "tests")
	for _, n := range []string{"a", "b", "d"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n+".yaml"), []byte("kind: Http\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// c cannot be deleted: a non-empty directory stands in its file's place.
	if err := os.MkdirAll(filepath.Join(dir, "c.yaml", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	got := deleteRevoked(context.Background(), logger, testdb.New(logger, root), "set", []string{"a", "b", "c", "d"})
	if fmt.Sprint(deletedNames(got)) != "[a b d]" {
		t.Fatalf("deleted %v, want [a b d]", deletedNames(got))
	}
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		if f, ok := e.ContextMap()["testcase id"]; ok && f != "c" {
			t.Fatalf("ERROR %q names %v, a test case the batch deleted", e.Message, f)
		}
		if f, ok := e.ContextMap()["file"]; ok && f != filepath.Join(dir, "c.yaml") {
			t.Fatalf("ERROR %q names %v, a test case the batch deleted", e.Message, f)
		}
	}
	if logs.FilterMessageSnippet("leaving it in the set").Len() != 1 {
		t.Fatalf("want one WARN for c, the test case that stays: %v", logs.All())
	}
}
