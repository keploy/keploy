package mapdb

import (
	"context"
	"os"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMappingDb_DeleteMappingsForSet(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "mapdb-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	logger := zap.NewNop()
	db := New(logger, tempDir, "mappings")

	ctx := context.Background()
	testSetID := "mock-set-1"

	// 1. Insert a batch of mappings
	entries := map[string][]models.MockEntry{
		"test-1": {
			{Name: "mock-1"},
			{Name: "mock-2"},
		},
	}
	err = db.UpsertBatch(ctx, testSetID, entries)
	if err != nil {
		t.Fatalf("failed to upsert batch: %v", err)
	}

	// Verify file exists
	exists, err := db.Exists(ctx, testSetID)
	if err != nil {
		t.Fatalf("unexpected error checking existence: %v", err)
	}
	if !exists {
		t.Fatalf("expected mapping file to exist")
	}

	// 2. Delete mappings for the mock set
	err = db.DeleteMappingsForSet(ctx, testSetID)
	if err != nil {
		t.Fatalf("failed to delete mappings: %v", err)
	}

	// 3. Verify file no longer exists
	existsAfter, err := db.Exists(ctx, testSetID)
	if err != nil {
		t.Fatalf("unexpected error checking existence after delete: %v", err)
	}
	if existsAfter {
		t.Fatalf("expected mapping file to be deleted")
	}

	// 4. Calling delete on non-existent set should not return error
	err = db.DeleteMappingsForSet(ctx, "non-existent-set")
	if err != nil {
		t.Fatalf("DeleteMappingsForSet returned error for missing file: %v", err)
	}
}
