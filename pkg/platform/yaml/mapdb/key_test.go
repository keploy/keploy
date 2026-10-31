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

// Under the proven switch, the key of the claim a test ran under is written
// beside its mocks and read back, kept by a later write that carries none
// (a rewrite of the test's list or of the whole mapping), and replaced by one that carries another.
func TestUpsertCasesKeepsEachTestsClaimKey(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "", format)
			ctx := context.Background()
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"a::TestA": {{Name: "mock-0"}}}))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"a::TestA": {Key: "a"}, "b::TestB": {Key: "b"},
			}, nil, nil))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"a::TestA": {Dir: "a"}, "b::TestB": {Key: "c"},
			}, nil, nil))
			keys := func() map[string]string {
				mapping, err := db.GetMapping(ctx, "set")
				require.NoError(t, err)
				out := map[string]string{}
				for _, tc := range mapping.TestCases {
					out[tc.ID] = tc.Key
				}
				return out
			}
			require.Equal(t, map[string]string{"a::TestA": "a", "b::TestB": "c"}, keys())
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"a::TestA": {{Name: "mock-0"}, {Name: "mock-1"}}}))
			require.Equal(t, map[string]string{"a::TestA": "a", "b::TestB": "c"}, keys(), "a rewrite of a test's list keeps every key")
			// A rewrite of the whole mapping (Insert) keeps the file's key
			// for an entry that carries none, and writes the one it carries.
			require.NoError(t, db.Insert(ctx, &models.Mapping{TestSetID: "set", TestCases: []models.MappedTestCase{
				{ID: "a::TestA", Mocks: []models.MockEntry{{Name: "mock-0"}, {Name: "mock-1"}}, Dir: "a"},
				{ID: "b::TestB", Key: "d"},
			}}))
			require.Equal(t, map[string]string{"a::TestA": "a", "b::TestB": "d"}, keys())
		})
	}
}
