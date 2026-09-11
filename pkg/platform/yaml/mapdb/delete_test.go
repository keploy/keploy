package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A re-record must REPLACE the previous run's mapping, not union with it:
// UpsertBatch merges with what is on disk while the recorder reissues the same
// mock-N names, so a surviving entry would point at a different recording.
func TestDeleteRemovesMappingSoReRecordDoesNotUnion(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "")
	ctx := context.Background()
	file := filepath.Join(dir, "set", "mappings.yaml")

	require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
		"run1-t1": {{Name: "mock-0"}},
		"run1-t2": {{Name: "mock-1"}},
	}))
	require.FileExists(t, file)

	require.NoError(t, db.Delete(ctx, "set"))
	_, err := os.Stat(file)
	require.True(t, os.IsNotExist(err), "mapping file must be gone after Delete")

	require.NoError(t, db.Delete(ctx, "set"), "Delete is idempotent: a missing file is not an error")

	require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
		"run2-t1": {{Name: "mock-0"}},
	}))
	got, _, err := db.Get(ctx, "set")
	require.NoError(t, err)
	require.Equal(t, map[string][]models.MockEntry{"run2-t1": {{Name: "mock-0"}}}, got,
		"only the second run's tests survive")
}

func TestDeleteRejectsUnsafeTestSetID(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "")
	for _, id := range []string{"", ".", "..", "a/b"} {
		require.Error(t, db.Delete(context.Background(), id), "testSetID %q must be rejected", id)
	}
}
