package mapdb

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// Cases sit beside a test's mocks, a test with cases but no mocks gets its own entry, and a second write unions.
func TestUpsertCasesRoundTripsThroughDisk(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, "", format)
			ctx := context.Background()
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "set", map[string][]string{"t1": {"test-1"}, "t2": {"test-2"}}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "set", map[string][]string{"t1": {"test-1", "test-3"}}); err != nil {
				t.Fatal(err)
			}
			cases, err := db.GetCases(ctx, "set")
			if err != nil {
				t.Fatal(err)
			}
			if len(cases["t1"]) != 2 || cases["t1"][1] != "test-3" || len(cases["t2"]) != 1 {
				t.Fatalf("GetCases read %v", cases)
			}

			data, err := yaml.ReadFileF(ctx, zap.NewNop(), dir+"/set", "mappings", format)
			if err != nil {
				t.Fatal(err)
			}
			mapping, err := DecodeMappingF(data, zap.NewNop(), format)
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]models.MappedTestCase{}
			for _, tc := range mapping.TestCases {
				byID[tc.ID] = tc
			}
			if got := byID["t1"]; len(got.Cases) != 2 || got.Cases[0] != "test-1" || got.Cases[1] != "test-3" || len(got.Mocks) != 1 || got.Mocks[0].Name != "mock-0" {
				t.Fatalf("t1 came back as %+v", got)
			}
			if got := byID["t2"]; len(got.Cases) != 1 || got.Cases[0] != "test-2" || len(got.Mocks) != 0 {
				t.Fatalf("t2 came back as %+v", got)
			}

			perTest, _, err := db.Get(ctx, "set")
			if err != nil {
				t.Fatal(err)
			}
			if len(perTest["t1"]) != 1 {
				t.Fatalf("the mocks were disturbed: %+v", perTest)
			}
		})
	}
}
