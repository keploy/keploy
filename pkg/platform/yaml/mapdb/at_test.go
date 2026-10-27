package mapdb

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, a test's recorded begin is written beside its
// mocks, read back by GetMapping, kept by a later write that carries none, and replaced by a
// later write that carries another.
func TestUpsertCasesKeepsEachTestsRecordedBegin(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	first := time.Date(2026, 10, 6, 9, 30, 15, 0, time.UTC)
	second := first.Add(time.Hour)
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "", format)
			ctx := context.Background()
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {At: first}, "t2": {Dir: "pkg"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {Dir: "pkg"}, "t2": {At: second}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			third := second.Add(time.Hour)
			if err := db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t2": {At: third}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			mapping, err := db.GetMapping(ctx, "set")
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]models.MappedTestCase{}
			for _, tc := range mapping.TestCases {
				byID[tc.ID] = tc
			}
			if got := byID["t1"]; !got.At.Equal(first) || got.Dir != "pkg" || len(got.Mocks) != 1 {
				t.Fatalf("t1 came back as %+v", got)
			}
			if got := byID["t2"]; !got.At.Equal(third) || got.Dir != "pkg" {
				t.Fatalf("t2 came back as %+v", got)
			}
		})
	}
}
