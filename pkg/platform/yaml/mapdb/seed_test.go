package mapdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, the seed a test's browser drew from is written
// beside its mocks and read back. A later record that ran the test replaces
// it, none included (seeding off): the calls it pairs with were drawn from
// the new one, so a re-record draws new numbers. A write for a test that did
// not run in it (no begin), and a write of the test's mocks alone, keep it.
func TestUpsertCasesKeepsEachTestsSeed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	const s1, s2 = "1111111111111111", "2222222222222222"
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "", format)
			ctx := context.Background()
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {At: t0, Seed: s1}, "t2": {At: t0, Seed: s1}, "t3": {At: t0, Seed: s1},
			}, nil, nil))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {Dir: "pkg"}, "t2": {At: t0.Add(time.Hour), Seed: s2}, "t3": {At: t0.Add(time.Hour)},
			}, nil, nil))
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-1"}}}))
			mapping, err := db.GetMapping(ctx, "set")
			require.NoError(t, err)
			byID := map[string]models.MappedTestCase{}
			for _, tc := range mapping.TestCases {
				byID[tc.ID] = tc
			}
			require.Equal(t, s1, byID["t1"].Seed, "a write without the test's run keeps its seed")
			require.Equal(t, "mock-1", byID["t1"].Mocks[len(byID["t1"].Mocks)-1].Name)
			require.Equal(t, s2, byID["t2"].Seed, "a re-record draws new numbers")
			require.Empty(t, byID["t3"].Seed, "a later record with seeding off replaces it")
		})
	}
}

// A write of the tests' mocks that rebuilds their entries keeps each entry's
// seed, the one written over the one on disk (entryOver).
func TestEntryOverKeepsTheSeed(t *testing.T) {
	o := models.MappedTestCase{ID: "t1", Seed: "1111111111111111"}
	require.Equal(t, "1111111111111111", entryOver(o, models.MappedTestCase{ID: "t1", Dir: "pkg"}).Seed)
	require.Equal(t, "2222222222222222", entryOver(o, models.MappedTestCase{ID: "t1", Seed: "2222222222222222"}).Seed)
}
