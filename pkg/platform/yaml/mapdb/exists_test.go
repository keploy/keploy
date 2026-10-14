package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// TestExistsSeesTheMappingFileInEitherFormat: Exists is what the replay asks
// before it creates a mappings file from what a run consumed, and every other
// read and write of the store (Get, GetStartup, Insert, UpsertBatch) finds the
// file in either format. A recording made with --storage-format json has
// mappings.json, and an Exists that looked only for mappings.yaml told every
// replay of that set, in either format, that there was no file, so the
// replay's write replaced each recorded test's entries with what it consumed.
func TestExistsSeesTheMappingFileInEitherFormat(t *testing.T) {
	ctx := context.Background()
	for _, written := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		for _, asked := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			t.Run(string(written)+" file, "+string(asked)+" store", func(t *testing.T) {
				dir := t.TempDir()
				recorder := NewWithFormat(zap.NewNop(), dir, "mappings", written)
				if err := recorder.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
					"test-1": {{Name: "mock-1", Kind: "Http"}},
				}); err != nil {
					t.Fatalf("UpsertBatch: %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "test-set-0", "mappings."+written.FileExtension())); err != nil {
					t.Fatalf("precondition: the recording wrote no mappings.%s: %v", written.FileExtension(), err)
				}

				db := NewWithFormat(zap.NewNop(), dir, "mappings", asked)
				exists, err := db.Exists(ctx, "test-set-0")
				if err != nil {
					t.Fatalf("Exists: %v", err)
				}
				if !exists {
					t.Fatalf("Exists = false for a set with mappings.%s, asked by a %s store", written.FileExtension(), asked)
				}
				if _, meaningful, err := db.Get(ctx, "test-set-0"); err != nil || !meaningful {
					t.Fatalf("control: Get finds the file (meaningful=%v, err=%v), so Exists must too", meaningful, err)
				}

				missing, err := db.Exists(ctx, "test-set-1")
				if err != nil || missing {
					t.Fatalf("Exists for a set with no mappings file = %v, %v; want false, nil", missing, err)
				}
			})
		}
	}
}
