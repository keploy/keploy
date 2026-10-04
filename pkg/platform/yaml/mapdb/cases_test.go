package mapdb

import (
	"context"
	"testing"
	"time"

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
			if err := db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {Cases: []string{"test-1"}}, "t2": {Cases: []string{"test-2"}}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {Cases: []string{"test-1", "test-3"}}}, nil, nil); err != nil {
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

func TestUpsertCasesWritesCaseMocksStepsAndStartup(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, "", format)
			ctx := context.Background()
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-1"}, {Name: "mock-2"}}}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {Cases: []string{"test-1"}, CaseMocks: map[string][]string{"test-1": {"mock-1"}}, CaseSteps: map[string]string{"test-1": "create"}},
			}, []models.MockEntry{{Name: "mock-0"}}, nil); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
				"t1": {Cases: []string{"test-2"}, CaseSteps: map[string]string{"test-2": ""}},
			}, nil, nil); err != nil {
				t.Fatal(err)
			}
			data, err := yaml.ReadFileF(ctx, zap.NewNop(), dir+"/set", "mappings", format)
			if err != nil {
				t.Fatal(err)
			}
			mapping, err := DecodeMappingF(data, zap.NewNop(), format)
			if err != nil {
				t.Fatal(err)
			}
			got := mapping.TestCases[0]
			if len(got.Cases) != 2 || len(got.Mocks) != 2 || len(got.CaseMocks) != 1 || got.CaseMocks["test-1"][0] != "mock-1" ||
				got.CaseSteps["test-1"] != "create" || got.CaseSteps["test-2"] != "" || len(got.CaseSteps) != 2 {
				t.Fatalf("t1 came back as %+v", got)
			}
			if names := mapping.StartupMockNames(); len(names) != 1 || names[0] != "mock-0" {
				t.Fatalf("startup came back as %v", names)
			}
		})
	}
}

func TestAMappingWrittenBeforeCaseMocksStillDecodes(t *testing.T) {
	old := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n  - id: t1\n    mock_entries:\n      - name: mock-0\n        kind: Http\n    cases:\n      - test-1\n  - id: t2\n    mocks: mock-1,mock-2\n"
	mapping, err := DecodeMapping([]byte(old), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping.TestCases) != 2 || mapping.TestCases[0].Cases[0] != "test-1" || mapping.TestCases[0].CaseMocks != nil || mapping.TestCases[0].CaseSteps != nil || len(mapping.TestCases[1].Mocks) != 2 || mapping.Startup != nil {
		t.Fatalf("decoded as %+v", mapping)
	}
}

func TestUpsertCasesKeepsEachTestsFolder(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, "", format)
			ctx := context.Background()
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}); err != nil {
				t.Fatal(err)
			}
			in := map[string]models.MappedTestCase{
				"t1": {Cases: []string{"test-1"}, Dir: "/repo/e2e/orders"},
				"t2": {Dir: "/repo/e2e/payments"},
			}
			if err := db.UpsertCases(ctx, "set", in, nil, nil); err != nil {
				t.Fatal(err)
			}
			data, err := yaml.ReadFileF(ctx, zap.NewNop(), dir+"/set", "mappings", format)
			if err != nil {
				t.Fatal(err)
			}
			mapping, err := DecodeMappingF(data, zap.NewNop(), format)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, tc := range mapping.TestCases {
				got[tc.ID] = tc.Dir
			}
			if got["t1"] != "/repo/e2e/orders" || got["t2"] != "/repo/e2e/payments" {
				t.Fatalf("folders came back as %v", got)
			}
		})
	}
}

func TestUpsertCasesKeepsTheSuites(t *testing.T) {
	dir := t.TempDir()
	db := NewWithFormat(zap.NewNop(), dir, "", yaml.FormatYAML)
	ctx := context.Background()
	at := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	suites := []models.SuiteSpan{{Dir: "/repo/e2e/orders", Start: at, End: at.Add(3 * time.Second)}}
	if err := db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {Dir: "/repo/e2e/orders"}}, nil, suites); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.ReadFileF(ctx, zap.NewNop(), dir+"/set", "mappings", yaml.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := DecodeMappingF(data, zap.NewNop(), yaml.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping.Suites) != 1 || mapping.Suites[0].Dir != "/repo/e2e/orders" || mapping.Suites[0].End.Sub(mapping.Suites[0].Start) != 3*time.Second {
		t.Fatalf("suites came back as %+v", mapping.Suites)
	}
}
