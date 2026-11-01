package mockdb

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"sort"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// dropTestMock is pruneHTTPMock i with the metadata md (its type tag and
// stated lifetime).
func dropTestMock(i int, md map[string]string) *models.Mock {
	m := pruneHTTPMock(i)
	m.Spec.Metadata = md
	return m
}

// DropMocks deletes a named per-test mock wherever it was recorded (no time
// rule: mock-5 lies in the startup window UpdateMocks would keep) and returns
// exactly the names it deleted. It keeps a mock it is not handed, and a named
// one that is a config mock or reusable: stated session, or an untagged HTTP
// mock the kind fallback makes session. A name the file does not hold is no
// error.
func TestDropMocks_DeletesOnlyTheNamedPerTestMocks(t *testing.T) {
	perTest := map[string]string{"type": "mocks", models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	fixture := []*models.Mock{
		dropTestMock(0, perTest),                             // mock-0: named, per-test: dropped
		dropTestMock(1, perTest),                             // mock-1: not named: kept
		dropTestMock(2, map[string]string{"type": "config"}), // mock-2: named, config: kept
		dropTestMock(3, map[string]string{"type": "mocks", models.MetadataLifetimeKey: "session"}), // mock-3: named, stated session: kept
		dropTestMock(4, map[string]string{}), // mock-4: named, untagged HTTP (session by kind): kept
		dropTestMock(5, perTest),             // mock-5: named, per-test, startup window: dropped
	}
	named := []string{"mock-0", "mock-2", "mock-3", "mock-4", "mock-5", "mock-9"}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, fixture, "")
			ys := New(zap.NewNop(), dir, "mocks")
			dropped, err := ys.DropMocks(context.Background(), pruneSet, named)
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(dropped)
			if !reflect.DeepEqual(dropped, []string{"mock-0", "mock-5"}) {
				t.Errorf("DropMocks reported %v deleted, want [mock-0 mock-5]", dropped)
			}
			if got := depsNamesOf(readDepsSet(t, dir)); !reflect.DeepEqual(got, []string{"mock-1", "mock-2", "mock-3", "mock-4"}) {
				t.Errorf("the file holds %v, want [mock-1 mock-2 mock-3 mock-4]", got)
			}
		})
	}
}

// No names writes nothing: the file is the same, byte for byte; a set with no
// mock file stays without one.
func TestDropMocks_NoNamesWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := writePruneFixture(t, dir, "yaml", []*models.Mock{pruneHTTPMock(0), pruneHTTPMock(1)}, "")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ys := New(zap.NewNop(), dir, "mocks")
	for _, names := range [][]string{nil, {}} {
		if dropped, err := ys.DropMocks(context.Background(), pruneSet, names); err != nil || dropped != nil {
			t.Fatalf("DropMocks(%v) = %v, %v; want nothing", names, dropped, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the mock file changed with no names (err %v)", err)
	}
	if dropped, err := ys.DropMocks(context.Background(), "no-such-set", []string{"mock-0"}); err != nil || dropped != nil {
		t.Fatalf("a set with no mock file: %v, %v; want nothing", dropped, err)
	}
}
