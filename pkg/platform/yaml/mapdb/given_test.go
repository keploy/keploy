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

// Under the proven switch, the values a test gave are written beside its
// mocks and read back. A later record that ran the test replaces them, none
// included: they pair with that record's calls. A write for a test that did
// not run in it (no begin) keeps them.
func TestUpsertCasesKeepsEachTestsGivens(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	g1 := []models.GivenValue{{Name: "given:app", Value: "app-1", Source: "a_test.go:20"}}
	g2 := []models.GivenValue{{Name: "given:app", Value: "app-2"}, {Name: "given:app#2", Value: "app-3"}}
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "", format)
			ctx := context.Background()
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {At: t0, Given: g1}, "t2": {At: t0, Given: g1}, "t3": {At: t0, Given: g1},
			}, nil, nil))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {Dir: "pkg"}, "t2": {At: t0.Add(time.Hour), Given: g2}, "t3": {At: t0.Add(time.Hour)},
			}, nil, nil))
			mapping, err := db.GetMapping(ctx, "set")
			require.NoError(t, err)
			byID := map[string]models.MappedTestCase{}
			for _, tc := range mapping.TestCases {
				byID[tc.ID] = tc
			}
			require.Equal(t, g1, byID["t1"].Given, "a write without the test's run keeps its givens")
			require.Len(t, byID["t1"].Mocks, 1)
			require.Equal(t, g2, byID["t2"].Given)
			require.Empty(t, byID["t3"].Given, "a later record that gave none replaces them")
		})
	}
}
