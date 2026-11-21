package testdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// A recording's keploy folder that a cloned repository carries as a link out
// of the project, by ".." or by an absolute target, is refused by every read
// and write of the store: nothing is written where it leads, and no test case
// there is read. A keploy folder that links to a folder inside the project
// keeps working.
func TestAKeployFolderThatLinksOutOfTheProjectIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, absolute := range []bool{false, true} {
		base := t.TempDir()
		outside := filepath.Join(base, "outside")
		// A test case outside the project, as a store writes it.
		if err := New(zap.NewNop(), outside).InsertTestCase(ctx, simpleCase("test-1"), "set", false); err != nil {
			t.Fatal(err)
		}
		project := filepath.Join(base, "project")
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatal(err)
		}
		keploy := filepath.Join(project, "keploy")
		if absolute {
			if err := os.Symlink(outside, keploy); err != nil {
				t.Skipf("cannot make a symbolic link here: %v", err)
			}
		} else {
			linkTo(t, outside, keploy)
		}
		db := New(zap.NewNop(), keploy)

		requireWriteRefused(t, "a record through the keploy link", db.InsertTestCase(ctx, simpleCase("test-2"), "new", false))
		if _, err := os.Lstat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
			t.Fatalf("absolute=%v: a record created a set outside the project: %v", absolute, err)
		}
		tcs, err := db.GetTestCases(ctx, "set")
		if err == nil || !strings.Contains(err.Error(), "only inside") {
			t.Fatalf("absolute=%v: the test cases outside were read: %d, %v; want them refused", absolute, len(tcs), err)
		}
	}

	// Inside the project, a keploy folder that is a link keeps working.
	base := t.TempDir()
	project := filepath.Join(base, "project")
	real := filepath.Join(project, "recordings")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	linkTo(t, real, filepath.Join(project, "keploy"))
	db := New(zap.NewNop(), filepath.Join(project, "keploy"))
	if err := db.InsertTestCase(ctx, simpleCase("test-1"), "set", false); err != nil {
		t.Fatalf("a record through a keploy link inside the project: %v", err)
	}
	if tcs, err := db.GetTestCases(ctx, "set"); err != nil || len(tcs) != 1 {
		t.Fatalf("read through a keploy link inside the project: %d, %v; want 1", len(tcs), err)
	}
	if _, err := os.Stat(filepath.Join(real, "set", "tests", "test-1.yaml")); err != nil {
		t.Fatalf("the test case is not where the link leads: %v", err)
	}
}
