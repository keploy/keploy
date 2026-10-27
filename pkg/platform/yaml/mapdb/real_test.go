package mapdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, the mock.real destinations a test used are written
// beside its mocks and read back, kept by a later write that carries none,
// and replaced by one that carries others.
func TestUpsertCasesKeepsEachTestsRealDestinations(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "", format)
			ctx := context.Background()
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {Real: []string{"localhost:5432"}}, "t2": {Dir: "pkg", Real: []string{"auth.example.com:443"}},
			}, nil, nil))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {Dir: "pkg"}, "t2": {Real: []string{"container:redis"}},
			}, nil, nil))
			mapping, err := db.GetMapping(ctx, "set")
			require.NoError(t, err)
			byID := map[string]models.MappedTestCase{}
			for _, tc := range mapping.TestCases {
				byID[tc.ID] = tc
			}
			require.Equal(t, []string{"localhost:5432"}, byID["t1"].Real)
			require.Len(t, byID["t1"].Mocks, 1)
			require.Equal(t, []string{"container:redis"}, byID["t2"].Real)
			require.Equal(t, "pkg", byID["t2"].Dir)
		})
	}
}
