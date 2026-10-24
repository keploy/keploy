//go:build unix

package openapidb

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

const schemaDoc = "openapi: 3.0.0\ninfo:\n  title: test-1\n  version: \"1\"\npaths: {}\n"

// noSearch makes dir a folder that can be listed but not searched (mode
// 0600), so a stat of anything in it fails with EACCES. A process that can
// still search it (root) skips: the symlink-loop cases cover the same path.
func noSearch(t *testing.T, dir, inside string) {
	t.Helper()
	if err := os.Chmod(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.Stat(inside); err == nil {
		t.Skipf("this process (euid %d) can still search a mode-0600 folder, so the permission error cannot be made here; the symlink-loop case covers the same path", os.Geteuid())
	}
}

// A test set's schema folder that is there but cannot be looked at is an
// error, not a set with no schemas: keploy contract validate used to check
// such a set against nothing.
func TestGetTestCasesSchema_AnUnreadableSetFolderIsAnError(t *testing.T) {
	t.Run("permission denied", func(t *testing.T) {
		root := t.TempDir()
		set := filepath.Join(root, "test-set-0")
		if err := os.MkdirAll(set, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(set, "test-1.yaml"), []byte(schemaDoc), 0o644); err != nil {
			t.Fatal(err)
		}
		noSearch(t, root, set)
		got, err := New(zap.NewNop(), root).GetTestCasesSchema(context.Background(), "test-set-0", "")
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("GetTestCasesSchema = %d schemas, %v; want the permission error", len(got), err)
		}
	})
	t.Run("symlink loop", func(t *testing.T) {
		root := t.TempDir()
		set := filepath.Join(root, "test-set-0")
		if err := os.Symlink(set, set); err != nil {
			t.Fatal(err)
		}
		got, err := New(zap.NewNop(), root).GetTestCasesSchema(context.Background(), "test-set-0", "")
		if !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("GetTestCasesSchema = %d schemas, %v; want ELOOP", len(got), err)
		}
	})
}

// A set with no schema folder still has no schemas, and no error; one that
// has them is read.
func TestGetTestCasesSchema_AMissingSetFolderIsEmpty(t *testing.T) {
	root := t.TempDir()
	db := New(zap.NewNop(), root)
	got, err := db.GetTestCasesSchema(context.Background(), "test-set-0", "")
	if err != nil || len(got) != 0 {
		t.Fatalf("GetTestCasesSchema of a missing set = %d schemas, %v; want none, nil", len(got), err)
	}
	set := filepath.Join(root, "test-set-1")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(set, "test-1.yaml"), []byte(schemaDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetTestCasesSchema(context.Background(), "test-set-1", "")
	if err != nil || len(got) != 1 || got[0].Info.Title != "test-1" {
		t.Fatalf("GetTestCasesSchema = %v, %v; want the one schema", got, err)
	}
}

// A mock set's schema file that is there but cannot be looked at is an error,
// not a mock set with no schemas.
func TestGetMocksSchemas_AnUnreadableSchemaFileIsAnError(t *testing.T) {
	t.Run("permission denied", func(t *testing.T) {
		root := t.TempDir()
		set := filepath.Join(root, "test-set-0")
		if err := os.MkdirAll(set, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(set, "schema.yaml")
		if err := os.WriteFile(file, []byte(schemaDoc), 0o644); err != nil {
			t.Fatal(err)
		}
		noSearch(t, set, file)
		got, err := New(zap.NewNop(), root).GetMocksSchemas(context.Background(), "test-set-0", root, "schema")
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("GetMocksSchemas = %d schemas, %v; want the permission error", len(got), err)
		}
	})
	t.Run("symlink loop", func(t *testing.T) {
		root := t.TempDir()
		set := filepath.Join(root, "test-set-0")
		if err := os.MkdirAll(set, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(set, "schema.yaml")
		if err := os.Symlink(file, file); err != nil {
			t.Fatal(err)
		}
		got, err := New(zap.NewNop(), root).GetMocksSchemas(context.Background(), "test-set-0", root, "schema")
		if !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("GetMocksSchemas = %d schemas, %v; want ELOOP", len(got), err)
		}
	})
}

// A mock set without a schema file still has no schemas, and no error; one
// that has it is read.
func TestGetMocksSchemas_AMissingSchemaFileIsEmpty(t *testing.T) {
	root := t.TempDir()
	db := New(zap.NewNop(), root)
	if err := os.MkdirAll(filepath.Join(root, "test-set-0"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, set := range []string{"test-set-0", "test-set-9"} {
		got, err := db.GetMocksSchemas(context.Background(), set, root, "schema")
		if err != nil || len(got) != 0 {
			t.Fatalf("GetMocksSchemas of %s = %d schemas, %v; want none, nil", set, len(got), err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "test-set-0", "schema.yaml"), []byte(schemaDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMocksSchemas(context.Background(), "test-set-0", root, "schema")
	if err != nil || len(got) != 1 || got[0].Info.Title != "test-1" {
		t.Fatalf("GetMocksSchemas = %v, %v; want the one schema", got, err)
	}
}
