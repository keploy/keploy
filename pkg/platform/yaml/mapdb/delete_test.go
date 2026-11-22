package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func TestDeleteRemovesTheMappingFile(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, "", format)
			ctx := context.Background()
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t": {{Name: "mock-0"}}}); err != nil {
				t.Fatalf("UpsertBatch: %v", err)
			}
			file := filepath.Join(dir, "set", "mappings."+format.FileExtension())
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("mapping file was not written: %v", err)
			}

			if err := db.Delete(ctx, "set"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("mapping file still there after Delete: %v", err)
			}
			// A second delete finds nothing and is not an error.
			if err := db.Delete(ctx, "set"); err != nil {
				t.Fatalf("Delete of a missing file: %v", err)
			}
		})
	}
}

func TestDeleteRefusesAPathThatLeavesTheSet(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "")
	for _, id := range []string{"", "..", "a/b", "../x"} {
		if err := db.Delete(context.Background(), id); err == nil {
			t.Fatalf("Delete(%q) was allowed", id)
		}
	}
}
