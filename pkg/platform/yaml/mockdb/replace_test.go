package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mockSet writes a minimal multi-document mocks.yaml and returns the db rooted
// at its parent. Kinds are deliberately mixed so the per-test/cross-test
// partition that GetFilteredMocks and GetUnFilteredMocks apply would split
// them — GetAllMocks must ignore that split entirely.
func mockSet(t *testing.T, names ...string) (*MockYaml, string) {
	t.Helper()
	dir := t.TempDir()
	setDir := filepath.Join(dir, "set")
	if err := os.MkdirAll(setDir, 0o755); err != nil {
		t.Fatal(err)
	}

	db := New(zap.NewNop(), dir, "")
	mocks := make([]*models.Mock, 0, len(names))
	for _, n := range names {
		mocks = append(mocks, httpMock(n))
	}
	if err := db.writeMocksAtomically(setDir, "mocks", mocks, db.Format); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db, "set"
}

func namesOf(mocks []*models.Mock) []string {
	out := make([]string, 0, len(mocks))
	for _, m := range mocks {
		out = append(out, m.Name)
	}
	return out
}

func mustGetAll(t *testing.T, db *MockYaml, set string) []*models.Mock {
	t.Helper()
	got, err := db.GetAllMocks(context.Background(), set)
	if err != nil {
		t.Fatalf("GetAllMocks: %v", err)
	}
	return got
}

// GetAllMocks must return every mock in file order. The partition readers
// return only their half and sort by timestamp; that is what made a naive
// read-modify-write lose mocks and reshuffle the file.
func TestGetAllMocks_ReturnsEverythingInFileOrder(t *testing.T) {
	db, set := mockSet(t, "mock-0", "mock-1", "mock-2", "mock-3")

	got := namesOf(mustGetAll(t, db, set))
	want := []string{"mock-0", "mock-1", "mock-2", "mock-3"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestGetAllMocks_MissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "")

	got, err := db.GetAllMocks(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("expected no error for a missing mocks file, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no mocks, got %v", namesOf(got))
	}
}

// The core contract: read -> write back unchanged must not lose or reorder
// anything. This is the regression that motivated the whole primitive.
func TestReplaceMocks_RoundTripIsLossless(t *testing.T) {
	db, set := mockSet(t, "mock-0", "mock-1", "mock-2")
	before, err := os.ReadFile(filepath.Join(db.MockPath, set, "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// Drop one and add it straight back under the same name.
	all := mustGetAll(t, db, set)
	var reinstate *models.Mock
	for _, m := range all {
		if m.Name == "mock-1" {
			reinstate = m
		}
	}
	if reinstate == nil {
		t.Fatal("fixture missing mock-1")
	}

	if err := db.ReplaceMocks(context.Background(), set, []string{"mock-1"}, []*models.Mock{reinstate}); err != nil {
		t.Fatalf("ReplaceMocks: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(db.MockPath, set, "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("round-trip changed the file:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// A replacement must land where the original was, so the diff stays local.
func TestReplaceMocks_InsertsAtDroppedPosition(t *testing.T) {
	db, set := mockSet(t, "mock-0", "mock-1", "mock-2", "mock-3")

	fresh := httpMock("mock-9")
	if err := db.ReplaceMocks(context.Background(), set, []string{"mock-1"}, []*models.Mock{fresh}); err != nil {
		t.Fatalf("ReplaceMocks: %v", err)
	}

	got := namesOf(mustGetAll(t, db, set))
	want := []string{"mock-0", "mock-9", "mock-2", "mock-3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v (replacement should land at the dropped index)", got, want)
	}
}

func TestReplaceMocks_AppendsWhenNothingDropped(t *testing.T) {
	db, set := mockSet(t, "mock-0", "mock-1")

	fresh := httpMock("mock-5")
	if err := db.ReplaceMocks(context.Background(), set, nil, []*models.Mock{fresh}); err != nil {
		t.Fatalf("ReplaceMocks: %v", err)
	}

	got := namesOf(mustGetAll(t, db, set))
	want := []string{"mock-0", "mock-1", "mock-5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Dropping a name that is not there means the caller's view of the file is
// stale. Surfacing it beats silently doing a partial splice.
func TestReplaceMocks_UnknownDropIsAnError(t *testing.T) {
	db, set := mockSet(t, "mock-0", "mock-1")

	err := db.ReplaceMocks(context.Background(), set, []string{"mock-1", "mock-404"}, nil)
	if err == nil {
		t.Fatal("expected an error for a drop name that does not exist")
	}
	if !strings.Contains(err.Error(), "mock-404") {
		t.Errorf("error should name the missing mock, got: %v", err)
	}

	// And nothing may have been written.
	got := namesOf(mustGetAll(t, db, set))
	if strings.Join(got, ",") != "mock-0,mock-1" {
		t.Errorf("file was modified despite the error: %v", got)
	}
}

func TestReplaceMocks_NameCollisionIsAnError(t *testing.T) {
	db, set := mockSet(t, "mock-0", "mock-1")

	dup := httpMock("mock-0")
	err := db.ReplaceMocks(context.Background(), set, []string{"mock-1"}, []*models.Mock{dup})
	if err == nil {
		t.Fatal("expected an error when an added mock collides with a surviving one")
	}
	if !strings.Contains(err.Error(), "mock-0") {
		t.Errorf("error should name the colliding mock, got: %v", err)
	}
}

// Renaming is legitimate: dropping mock-0 frees the name for an incoming mock.
func TestReplaceMocks_ReusingADroppedNameIsAllowed(t *testing.T) {
	db, set := mockSet(t, "mock-0", "mock-1")

	replacement := httpMock("mock-0")
	if err := db.ReplaceMocks(context.Background(), set, []string{"mock-0"}, []*models.Mock{replacement}); err != nil {
		t.Fatalf("reusing a dropped name should be allowed, got %v", err)
	}
	got := namesOf(mustGetAll(t, db, set))
	if strings.Join(got, ",") != "mock-0,mock-1" {
		t.Errorf("got %v", got)
	}
}

func TestReplaceMocks_NoOpDoesNotTouchTheFile(t *testing.T) {
	db, set := mockSet(t, "mock-0")
	target := filepath.Join(db.MockPath, set, "mocks.yaml")

	stat, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.ReplaceMocks(context.Background(), set, nil, nil); err != nil {
		t.Fatalf("no-op should succeed, got %v", err)
	}

	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("no-op removed or replaced the file: %v", err)
	}
	if !after.ModTime().Equal(stat.ModTime()) {
		t.Error("no-op rewrote the file")
	}
}

func TestReplaceMocks_DroppingEverythingRemovesTheFile(t *testing.T) {
	db, set := mockSet(t, "mock-0", "mock-1")

	if err := db.ReplaceMocks(context.Background(), set, []string{"mock-0", "mock-1"}, nil); err != nil {
		t.Fatalf("ReplaceMocks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(db.MockPath, set, "mocks.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected the mocks file to be removed, stat err = %v", err)
	}
}

func TestReplaceMocks_NilMockIsRejected(t *testing.T) {
	db, set := mockSet(t, "mock-0")

	if err := db.ReplaceMocks(context.Background(), set, nil, []*models.Mock{nil}); err == nil {
		t.Fatal("expected an error when adding a nil mock")
	}
}
