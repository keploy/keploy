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

// Regression test for keploy#4488: a re-record deletes mocks.yaml for the set
// but previously left mappings.yaml in place, so UpsertBatch merged the fresh
// per-test entries into stale ones from the previous recording and tests could
// map to mocks that no longer exist. DeleteForSet must remove every mapping
// file variant so the next UpsertBatch starts from an empty slate.
func TestDeleteForSetClearsStaleMappings(t *testing.T) {
	logger := zap.NewNop()
	dir := t.TempDir()
	setID := "myset"

	db := New(logger, dir, "")

	ctx := context.Background()

	// First recording: two tests mapped to their mocks.
	first := map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1"}, {Name: "mock-2"}},
		"test-2": {{Name: "mock-3"}},
	}
	if err := db.UpsertBatch(ctx, setID, first); err != nil {
		t.Fatalf("first upsert failed: %v", err)
	}

	got, present, err := db.Get(ctx, setID)
	if err != nil || !present {
		t.Fatalf("mappings should exist after first record: got=%v present=%v err=%v", got, present, err)
	}
	if len(got["test-1"]) != 2 {
		t.Fatalf("expected 2 mocks for test-1 after first record, got %d", len(got["test-1"]))
	}

	// Re-record: clear mappings exactly as the mock-record flow now does.
	if err := db.DeleteForSet(ctx, setID); err != nil {
		t.Fatalf("delete for set failed: %v", err)
	}

	if _, present, _ := db.Get(ctx, setID); present {
		t.Fatal("mappings must not survive DeleteForSet")
	}

	// Second recording: only one test with one mock. The stale test-2 entry
	// (and test-1's second mock) must NOT reappear via merge.
	second := map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1"}},
	}
	if err := db.UpsertBatch(ctx, setID, second); err != nil {
		t.Fatalf("second upsert failed: %v", err)
	}

	got, _, err = db.Get(ctx, setID)
	if err != nil {
		t.Fatalf("get after second record failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("stale tests survived re-record: got %d tests (%v), want 1", len(got), got)
	}
	if len(got["test-1"]) != 1 || got["test-1"][0].Name != "mock-1" {
		t.Fatalf("stale mocks survived re-record for test-1: %v", got["test-1"])
	}
}

// DeleteForSet must tolerate a missing mappings file (first-ever record calls
// it before anything exists).
func TestDeleteForSetMissingFileIsNoOp(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "")
	if err := db.DeleteForSet(context.Background(), "fresh"); err != nil {
		t.Fatalf("delete on missing file should be a no-op, got: %v", err)
	}
}

// The test-set ID reaches os.Remove here, so the pathsafe guard must reject
// traversal attempts exactly like the mock-side delete does.
func TestDeleteForSetRejectsUnsafeSetIDs(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "")
	for _, bad := range []string{"../escape", "a/b", "..", "."} {
		if err := db.DeleteForSet(context.Background(), bad); err == nil {
			t.Fatalf("expected rejection for %q", bad)
		}
	}
}

// JSON-format mappings must be cleared too — a yaml refresh must not leave a
// stale json mapping file behind that FileExistsAny would then prefer.
func TestDeleteForSetClearsJSONVariant(t *testing.T) {
	logger := zap.NewNop()
	dir := t.TempDir()
	setID := "jsonset"

	db := NewWithFormat(logger, dir, "", yaml.FormatJSON)
	ctx := context.Background()
	if err := db.UpsertBatch(ctx, setID, map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1"}},
	}); err != nil {
		t.Fatalf("json upsert failed: %v", err)
	}
	jsonPath := filepath.Join(dir, setID, "mappings.json")
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("expected json mappings at %s: %v", jsonPath, err)
	}

	ydb := New(logger, dir, "")
	if err := ydb.DeleteForSet(ctx, setID); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Fatalf("json mappings survived delete: %v", err)
	}
}
