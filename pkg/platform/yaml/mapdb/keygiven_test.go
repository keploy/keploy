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

// What claim keys' harnesses gave outside every test survives every writer of
// mappings.yaml: UpsertKeyGiven writes it (replacing a key's, keeping the
// others'), and Insert's flatten-and-rebuild carries it, as the startup
// section.
func TestKeyGivenSurvivesTheMappingWriters(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "s"), 0o755))
	ctx := context.Background()
	kg := func(key, v string) models.KeyGiven {
		return models.KeyGiven{Key: key, Given: []models.GivenValue{{Name: "outside-cookie-a-1", Value: v}}}
	}
	require.NoError(t, db.UpsertKeyGiven(ctx, "s", []models.KeyGiven{kg("a", "1"), kg("b", "1")}))
	require.NoError(t, db.UpsertKeyGiven(ctx, "s", []models.KeyGiven{kg("a", "2")}))
	require.NoError(t, db.Insert(ctx, &models.Mapping{TestSetID: "s", TestCases: []models.MappedTestCase{{ID: "T", Mocks: []models.MockEntry{{Name: "mock-0"}}}}}))
	require.NoError(t, db.UpsertBatch(ctx, "s", map[string][]models.MockEntry{"U": {{Name: "mock-1"}}}))
	m, err := db.GetMapping(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, []models.KeyGiven{kg("a", "2"), kg("b", "1")}, m.KeyGiven)
	require.Len(t, m.TestCases, 2)
}
