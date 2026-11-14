package openapidb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func linkTo(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(path), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, path); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

func refused(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("%s: %v, want it refused as outside the recording", what, err)
	}
}

// A schema file is read only inside its folder of schemas: a test's or a
// mock's schema that links out of it is refused by both readers, with why.
func TestSchemasAreReadOnlyInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "schema")
	out := filepath.Join(base, "outside.yaml")
	if err := os.WriteFile(out, []byte("version: SECRET-OUTSIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkTo(t, out, filepath.Join(root, "set", "test-1.yaml"))
	linkTo(t, out, filepath.Join(root, "mockset", "mocks.yaml"))
	db := New(zap.NewNop(), root)
	for _, testPath := range []string{"", root} {
		tcs, err := db.GetTestCasesSchema(ctx, "set", testPath)
		refused(t, "GetTestCasesSchema", err)
		if !strings.Contains(err.Error(), copyHint) {
			t.Fatalf("GetTestCasesSchema: %v, want it to name the copy", err)
		}
		if len(tcs) != 0 {
			t.Fatalf("GetTestCasesSchema read %d schemas", len(tcs))
		}
	}
	mocks, err := db.GetMocksSchemas(ctx, "mockset", root, "mocks")
	refused(t, "GetMocksSchemas", err)
	if !strings.Contains(err.Error(), copyHint) {
		t.Fatalf("GetMocksSchemas: %v, want it to name the copy", err)
	}
	if len(mocks) != 0 {
		t.Fatalf("GetMocksSchemas read %d schemas", len(mocks))
	}
}

// copyHint is what a schema refused for leading out of the schema folder adds
// to why: the fix, a copy of the set in the folder.
const copyHint = "copy the set into it"

// A schema refused because it leads out of the schema folder says how to have
// it read: copy the set into the folder. A refusal with nothing to copy, or
// for a link that stays inside, does not say so.
func TestASchemaThatLeadsOutSaysToCopyTheSetIn(t *testing.T) {
	ctx := context.Background()
	write := func(t *testing.T, path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hint := func(t *testing.T, what string, err error, want bool) {
		t.Helper()
		refused(t, what, err)
		if got := strings.Contains(err.Error(), copyHint); got != want {
			t.Fatalf("%s: %v: names the copy %v, want %v", what, err, got, want)
		}
	}
	t.Run("a test's schema that links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "schema")
		write(t, filepath.Join(base, "outside.yaml"), "openapi: 3.0.0\n")
		linkTo(t, filepath.Join(base, "outside.yaml"), filepath.Join(root, "set", "test-1.yaml"))
		_, err := New(zap.NewNop(), root).GetTestCasesSchema(ctx, "set", "")
		hint(t, "GetTestCasesSchema", err, true)
	})
	t.Run("a test set folder that links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "schema")
		write(t, filepath.Join(base, "shared", "set", "test-1.yaml"), "openapi: 3.0.0\n")
		linkTo(t, filepath.Join(base, "shared", "set"), filepath.Join(root, "set"))
		_, err := New(zap.NewNop(), root).GetTestCasesSchema(ctx, "set", root)
		hint(t, "GetTestCasesSchema", err, true)
	})
	t.Run("a mock set folder that links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "schema")
		write(t, filepath.Join(base, "shared", "mockset", "mocks.yaml"), "openapi: 3.0.0\n")
		linkTo(t, filepath.Join(base, "shared", "mockset"), filepath.Join(root, "mockset"))
		_, err := New(zap.NewNop(), root).GetMocksSchemas(ctx, "mockset", root, "mocks")
		hint(t, "GetMocksSchemas", err, true)
	})
	t.Run("a link out to nothing", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "schema")
		linkTo(t, filepath.Join(base, "missing.yaml"), filepath.Join(root, "set", "test-1.yaml"))
		_, err := New(zap.NewNop(), root).GetTestCasesSchema(ctx, "set", "")
		hint(t, "GetTestCasesSchema", err, false)
	})
	t.Run("an absolute link to a schema inside", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "schema")
		inside := filepath.Join(root, "other", "test-1.yaml")
		write(t, inside, "openapi: 3.0.0\n")
		if err := os.MkdirAll(filepath.Join(root, "set"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(inside, filepath.Join(root, "set", "test-1.yaml")); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
		_, err := New(zap.NewNop(), root).GetTestCasesSchema(ctx, "set", "")
		hint(t, "GetTestCasesSchema", err, false)
	})
}

// A schema is written only inside the recording: an append onto a schema
// file that links out, and any write under a folder that does, are refused,
// with why, and what they lead to is left as it was. A new schema is written.
func TestSchemasAreWrittenOnlyInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	schema := models.OpenAPI{OpenAPI: "3.0.0"}
	t.Run("an append onto a link out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside.yaml")
		if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "schema", "mocks")
		linkTo(t, out, filepath.Join(dir, "mocks.yaml"))
		db := New(zap.NewNop(), filepath.Join(root, "schema"))
		refused(t, "WriteSchema", db.WriteSchema(ctx, zap.NewNop(), root, dir, "mocks", schema, true))
		if got, err := os.ReadFile(out); err != nil || string(got) != "KEEP ME\n" {
			t.Fatalf("the file outside holds %q (%v)", got, err)
		}
	})
	t.Run("a folder that links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		linkTo(t, out, filepath.Join(root, "schema"))
		db := New(zap.NewNop(), filepath.Join(root, "schema"))
		for _, dir := range []string{filepath.Join(root, "schema"), filepath.Join(root, "schema", "tests", "set")} {
			refused(t, "WriteSchema", db.WriteSchema(ctx, zap.NewNop(), root, dir, "test-1", schema, false))
		}
		if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
			t.Fatalf("WriteSchema left %v (%v) outside the recording", entries, err)
		}
	})
	t.Run("a new schema", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "keploy")
		dir := filepath.Join(root, "schema", "tests", "set")
		db := New(zap.NewNop(), filepath.Join(root, "schema"))
		if err := db.WriteSchema(ctx, zap.NewNop(), root, dir, "test-1", schema, false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "test-1.yaml")); err != nil {
			t.Fatal(err)
		}
	})
}
