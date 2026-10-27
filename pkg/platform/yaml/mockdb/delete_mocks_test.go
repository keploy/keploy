package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// DeleteMocks takes the named mocks out of the set's mock file in the format
// it is stored in, keeps every other mock, keeps a named one a kept mock
// requires, returns only what it took out, and leaves a file it takes nothing
// out of unwritten.
func TestDeleteMocksTakesOutOnlyWhatNoKeptMockRequires(t *testing.T) {
	prev := models.RegisterMockDeps(models.HTTP, depsByMetadata)
	t.Cleanup(func() { models.RegisterMockDeps(models.HTTP, prev) })
	fixture := []*models.Mock{
		linkedHTTPMock(0, "lease", ""), // mock-0: named, but mock-1 requires it
		linkedHTTPMock(1, "", "lease"), // mock-1: kept, requires lease
		linkedHTTPMock(2, "", ""),      // mock-2: named
		linkedHTTPMock(3, "", ""),      // mock-3: kept
		linkedHTTPMock(4, "", ""),      // mock-4: named
	}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			path := writePruneFixture(t, dir, format, fixture, "")
			ys := New(zap.NewNop(), dir, "mocks")
			ctx := context.Background()
			got, err := ys.DeleteMocks(ctx, pruneSet, []string{"mock-4", "mock-0", "mock-2", "mock-9", "mock-2"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, []string{"mock-2", "mock-4"}) {
				t.Fatalf("took out %v, want [mock-2 mock-4]", got)
			}
			if names := depsNamesOf(readDepsSet(t, dir)); !reflect.DeepEqual(names, []string{"mock-0", "mock-1", "mock-3"}) {
				t.Fatalf("the file holds %v, want [mock-0 mock-1 mock-3]", names)
			}
			if filepath.Ext(path) != "."+format {
				t.Fatalf("fixture at %s, want a .%s file", path, format)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// Nothing named is in the file (mock-0 is still required): it is
			// not written.
			got, err = ys.DeleteMocks(ctx, pruneSet, []string{"mock-2", "mock-0"})
			if err != nil || got != nil {
				t.Fatalf("again: took out %v, err %v; want nothing", got, err)
			}
			if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
				t.Fatalf("taking nothing out rewrote the file (err %v)", err)
			}
		})
	}
	// A set with no mock file has nothing to take out.
	ys := New(zap.NewNop(), t.TempDir(), "mocks")
	if got, err := ys.DeleteMocks(context.Background(), pruneSet, []string{"mock-0"}); err != nil || got != nil {
		t.Fatalf("no mock file: %v, %v; want nothing, no error", got, err)
	}
}
