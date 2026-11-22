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

func TestDeleteEvictsLiveCacheAndHandlesNeverCreatedSet(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, "", format)
			ctx := context.Background()

			// Deleting a set whose directory was never created must succeed.
			if err := db.Delete(ctx, "never-created"); err != nil {
				t.Fatalf("Delete(never-created): %v", err)
			}

			// Seed initial mappings across two flushes (matching a live recording)
			// so both disk and the liveMappings cache hold the set.
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
				"stale-test-1": {{Name: "mock-0"}},
			}); err != nil {
				t.Fatalf("first UpsertBatch: %v", err)
			}
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
				"stale-test-2": {{Name: "mock-1"}},
			}); err != nil {
				t.Fatalf("second UpsertBatch: %v", err)
			}
			if db.live.p.Load() == nil {
				t.Fatalf("expected liveMappings cache to be populated after UpsertBatch")
			}

			if err := db.Delete(ctx, "set"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if db.live.p.Load() != nil {
				t.Fatalf("expected Delete to evict liveMappings cache, still populated")
			}

			// Re-recording new mappings on the same DB instance must not retain
			// stale entries from before Delete.
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
				"fresh-test": {{Name: "mock-2"}},
			}); err != nil {
				t.Fatalf("post-Delete UpsertBatch: %v", err)
			}

			got, ok, err := db.Get(ctx, "set")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !ok {
				t.Fatalf("Get: expected mappings file to exist after re-upsert")
			}
			if _, staleExists := got["stale-test-1"]; staleExists {
				t.Fatalf("stale-test-1 leaked back after Delete + UpsertBatch: %v", got)
			}
			if _, staleExists := got["stale-test-2"]; staleExists {
				t.Fatalf("stale-test-2 leaked back after Delete + UpsertBatch: %v", got)
			}
			if len(got["fresh-test"]) != 1 || got["fresh-test"][0].Name != "mock-2" {
				t.Fatalf("fresh-test = %v, want [mock-2]", got["fresh-test"])
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
