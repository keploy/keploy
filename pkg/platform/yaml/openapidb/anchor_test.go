package openapidb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// The folder schemas are read in is itself held to the project: a keploy
// folder, a schema folder or a Download folder that a cloned repository
// carries as a link out of the folder it is in is refused, with why, before
// any schema in it is read, even one that stays inside the project but leaves
// keploy. A keploy folder that links to a folder inside the project is read.
func TestASchemaFolderThatLinksOutIsRefused(t *testing.T) {
	ctx := context.Background()
	const secret = "openapi: 3.0.0\ninfo:\n  title: OUTSIDE-SECRET\n  version: \"1\"\n"
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(secret), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, target, at string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, at); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
	}
	for name, plant := range map[string]func(t *testing.T, base string){
		// keploy/schema -> ../../outside
		"schema out of the project": func(t *testing.T, base string) {
			write(t, filepath.Join(base, "outside", "tests", "test-set-0", "test-1.yaml"))
			link(t, filepath.Join("..", "..", "outside"), filepath.Join(base, "project", "keploy", "schema"))
		},
		// keploy/schema -> ../elsewhere, inside the project but out of keploy
		"schema out of keploy": func(t *testing.T, base string) {
			write(t, filepath.Join(base, "project", "elsewhere", "tests", "test-set-0", "test-1.yaml"))
			link(t, filepath.Join("..", "elsewhere"), filepath.Join(base, "project", "keploy", "schema"))
		},
		// keploy -> ../outside
		"keploy out of the project": func(t *testing.T, base string) {
			write(t, filepath.Join(base, "outside", "schema", "tests", "test-set-0", "test-1.yaml"))
			link(t, filepath.Join("..", "outside"), filepath.Join(base, "project", "keploy"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			plant(t, base)
			t.Chdir(filepath.Join(base, "project"))
			if b, err := os.ReadFile(filepath.Join("keploy", "schema", "tests", "test-set-0", "test-1.yaml")); err != nil || string(b) != secret {
				t.Fatalf("the link does not reach the schema: %v", err)
			}
			db := New(zap.NewNop(), filepath.Join("keploy", "schema"))
			db.ChangePath(filepath.Join("./keploy", "schema", "tests"))
			got, err := db.GetTestCasesSchema(ctx, "test-set-0", "")
			if err == nil || !strings.Contains(err.Error(), "leads out of it") {
				t.Fatalf("GetTestCasesSchema read %d schemas, %v; want the folder refused as leading out", len(got), err)
			}
		})
	}

	t.Run("a downloaded service folder out of the project", func(t *testing.T) {
		base := t.TempDir()
		write(t, filepath.Join(base, "outside", "test-set-0", "schema.yaml"))
		link(t, filepath.Join("..", "..", "..", "outside"), filepath.Join(base, "project", "Download", "Mocks", "svc"))
		t.Chdir(filepath.Join(base, "project"))
		got, err := New(zap.NewNop(), "").GetMocksSchemas(ctx, "test-set-0", filepath.Join("./Download", "Mocks", "svc"), "schema")
		if err == nil || !strings.Contains(err.Error(), "leads out of it") {
			t.Fatalf("GetMocksSchemas read %d schemas, %v; want the folder refused as leading out", len(got), err)
		}
	})

	t.Run("a schema folder that leaves keploy for the project", func(t *testing.T) {
		base := t.TempDir()
		write(t, filepath.Join(base, "project", "shared", "tests", "test-set-0", "test-1.yaml"))
		link(t, filepath.Join("..", "shared"), filepath.Join(base, "project", "keploy", "schema"))
		t.Chdir(filepath.Join(base, "project"))
		db := New(zap.NewNop(), filepath.Join("keploy", "schema"))
		db.ChangePath(filepath.Join("./keploy", "schema", "tests"))
		_, err := db.GetTestCasesSchema(ctx, "test-set-0", "")
		// keploy/schema leaves keploy, so it is refused even inside the
		// project: a schema folder is a folder of the recording.
		if err == nil || !strings.Contains(err.Error(), "leads out of it") {
			t.Fatalf("GetTestCasesSchema: %v; want a schema folder that leaves keploy refused", err)
		}
	})

	t.Run("a keploy folder that links inside the project", func(t *testing.T) {
		base := t.TempDir()
		write(t, filepath.Join(base, "project", "recordings", "schema", "tests", "test-set-0", "test-1.yaml"))
		link(t, "recordings", filepath.Join(base, "project", "keploy"))
		t.Chdir(filepath.Join(base, "project"))
		db := New(zap.NewNop(), filepath.Join("keploy", "schema"))
		db.ChangePath(filepath.Join("./keploy", "schema", "tests"))
		got, err := db.GetTestCasesSchema(ctx, "test-set-0", "")
		if err != nil || len(got) != 1 || got[0].Info.Title != "OUTSIDE-SECRET" {
			t.Fatalf("GetTestCasesSchema through a keploy link inside the project: %d, %v; want the schema", len(got), err)
		}
	})
}
