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

// A test's recorded end is written beside its begin and read back by
// GetMapping; a later write that carries no begin keeps both; a later
// record's begin takes its own end with it, none included, so no entry pairs
// one record's begin with another's end.
func TestUpsertCasesKeepsEachTestsEndWithItsBegin(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "", format)
			ctx := context.Background()
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {At: t0, End: t0.Add(2 * time.Second)},
				"t2": {At: t0, End: t0.Add(3 * time.Second)},
				"t3": {At: t0, End: t0.Add(4 * time.Second)},
			}, nil, nil))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {Dir: "pkg"},
				"t2": {At: t0.Add(time.Hour), End: t0.Add(time.Hour + time.Second)},
				"t3": {At: t0.Add(time.Hour)},
			}, nil, nil))
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-1"}}}))
			mapping, err := db.GetMapping(ctx, "set")
			require.NoError(t, err)
			byID := map[string]models.MappedTestCase{}
			for _, tc := range mapping.TestCases {
				byID[tc.ID] = tc
			}
			require.True(t, byID["t1"].At.Equal(t0) && byID["t1"].End.Equal(t0.Add(2*time.Second)), "a write without the test's run keeps its begin and end: %+v", byID["t1"])
			require.Equal(t, "pkg", byID["t1"].Dir)
			require.Equal(t, "mock-1", byID["t1"].Mocks[len(byID["t1"].Mocks)-1].Name)
			require.True(t, byID["t2"].End.Equal(t0.Add(time.Hour+time.Second)), "a re-record's end replaces the earlier one: %+v", byID["t2"])
			require.True(t, byID["t3"].At.Equal(t0.Add(time.Hour)) && byID["t3"].End.IsZero(), "a re-record without an end drops the earlier record's: %+v", byID["t3"])
		})
	}
}

// A write that rebuilds the tests' entries keeps each entry's end with the
// begin it pairs with (entryOver): the one on disk when the write carries no
// begin, the write's (none included) when it carries one.
func TestEntryOverKeepsTheEndWithItsBegin(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	o := models.MappedTestCase{ID: "t1", At: t0, End: t0.Add(time.Second)}
	kept := entryOver(o, models.MappedTestCase{ID: "t1", Dir: "pkg"})
	require.True(t, kept.At.Equal(t0) && kept.End.Equal(t0.Add(time.Second)), "%+v", kept)
	took := entryOver(o, models.MappedTestCase{ID: "t1", At: t0.Add(time.Hour), End: t0.Add(time.Hour + time.Second)})
	require.True(t, took.At.Equal(t0.Add(time.Hour)) && took.End.Equal(t0.Add(time.Hour+time.Second)), "%+v", took)
	dropped := entryOver(o, models.MappedTestCase{ID: "t1", At: t0.Add(time.Hour)})
	require.True(t, dropped.At.Equal(t0.Add(time.Hour)) && dropped.End.IsZero(), "%+v", dropped)
}
