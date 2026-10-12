package mapdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// UpsertBatch and UpsertBatchReplacing differ ONLY in what happens to a test
// already on disk: the first unions its mock list, the second overwrites it.
// Integration record needs the union (its agent emits a test's mocks as a
// delta); mock mode needs the replace (it writes each owner once, from the whole
// run, so a union can only resurrect a previous recording's names).
func TestUpsertBatchReplacingOverwritesATestsMockList(t *testing.T) {
	ctx := context.Background()
	first := map[string][]models.MockEntry{"t1": {{Name: "a-0"}, {Name: "a-1"}}}
	second := map[string][]models.MockEntry{"t1": {{Name: "a-0"}}}

	unioning := New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, unioning.UpsertBatch(ctx, "set", first))
	require.NoError(t, unioning.UpsertBatch(ctx, "set", second))
	got, _, err := unioning.Get(ctx, "set")
	require.NoError(t, err)
	require.Equal(t, []models.MockEntry{{Name: "a-0"}, {Name: "a-1"}}, got["t1"],
		"UpsertBatch must keep unioning -- integration record depends on it")

	replacing := New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, replacing.UpsertBatchReplacing(ctx, "set", first))
	require.NoError(t, replacing.UpsertBatchReplacing(ctx, "set", second))
	got, _, err = replacing.Get(ctx, "set")
	require.NoError(t, err)
	require.Equal(t, []models.MockEntry{{Name: "a-0"}}, got["t1"],
		"the mock this owner no longer records must be gone, not unioned back in")
}

// A test absent from the batch is untouched by either variant: replacement is
// per owner, not a whole-file rewrite.
func TestUpsertBatchReplacingLeavesOtherTestsAlone(t *testing.T) {
	ctx := context.Background()
	db := New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, db.UpsertBatchReplacing(ctx, "set", map[string][]models.MockEntry{
		"t1": {{Name: "a-0"}},
		"t2": {{Name: "b-0"}},
	}))
	require.NoError(t, db.UpsertBatchReplacing(ctx, "set", map[string][]models.MockEntry{
		"t1": {{Name: "a-1"}},
	}))
	got, _, err := db.Get(ctx, "set")
	require.NoError(t, err)
	require.Equal(t, map[string][]models.MockEntry{
		"t1": {{Name: "a-1"}},
		"t2": {{Name: "b-0"}},
	}, got)
}
