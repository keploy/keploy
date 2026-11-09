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

// The variables a client test's request binds (models.MappedTestCase.Variables)
// are written on the test's entry by record's full round, beside what it
// wrote before, and read back. A later record of the test, whose calls they
// no longer pair with, drops them; a write of the test's mocks, and one for a
// test that did not run, keeps them.
func TestUpsertCasesKeepsEachTestsCaseVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	name := models.MockVariables{"name": {Recorded: "app-1", Bind: models.CaseBind("post-apps-1", "body.name")}}
	suite := models.MockVariables{"suite": {Recorded: "suite-2", Bind: models.CaseBind("post-suites-2", "body.name")}}
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "", format)
			ctx := context.Background()
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {At: t0}, "t2": {At: t0}}, nil, nil))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {Variables: name}, "t2": {Variables: name}}, nil, nil))
			require.NoError(t, db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {Variables: suite}, "t2": {At: t0.Add(time.Hour)}}, nil, nil))
			require.NoError(t, db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}))
			mapping, err := db.GetMapping(ctx, "set")
			require.NoError(t, err)
			byID := map[string]models.MappedTestCase{}
			for _, tc := range mapping.TestCases {
				byID[tc.ID] = tc
			}
			require.Equal(t, models.MockVariables{"name": name["name"], "suite": suite["suite"]}, byID["t1"].Variables, "the full round's variables, beside each other, kept by a write of the test's mocks")
			require.Len(t, byID["t1"].Mocks, 1)
			require.Empty(t, byID["t2"].Variables, "a later record of the test drops them")

			// A writer that rewrites the set (Insert) sets the variables it
			// writes for a test, over the file's: none there, or others.
			require.NoError(t, db.Insert(ctx, &models.Mapping{TestSetID: "set", TestCases: []models.MappedTestCase{
				{ID: "t1", Mocks: []models.MockEntry{{Name: "mock-0"}}, Variables: name},
				{ID: "t2", Mocks: []models.MockEntry{{Name: "mock-1"}}, Variables: suite},
			}}))
			mapping, err = db.GetMapping(ctx, "set")
			require.NoError(t, err)
			for _, tc := range mapping.TestCases {
				byID[tc.ID] = tc
			}
			require.Equal(t, name, byID["t1"].Variables, "Insert's variables for t1, over the file's")
			require.Equal(t, suite, byID["t2"].Variables, "Insert's variables for t2, where the file held none")
		})
	}
}
