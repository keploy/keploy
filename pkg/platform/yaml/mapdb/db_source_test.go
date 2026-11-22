package mapdb

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A mappings.yaml says which keploy command wrote its test entries when it is
// not keploy record (models.Mapping.Source): keploy test's write names it,
// and a writer that names none keeps the file's, record's included.
func TestMappingSourceIsKeptAcrossWrites(t *testing.T) {
	ctx := context.Background()
	db := New(zap.NewNop(), t.TempDir(), "")
	source := func() string {
		t.Helper()
		s, err := db.GetSource(ctx, "set")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := source(); s != "" {
		t.Fatalf("no file: source %q", s)
	}
	if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t1": {{Name: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if s := source(); s != "" {
		t.Fatalf("keploy record's file: source %q", s)
	}
	if err := db.Insert(ctx, &models.Mapping{TestSetID: "set", Source: models.MappingSourceKeployTest,
		TestCases: []models.MappedTestCase{{ID: "t1", Mocks: []models.MockEntry{{Name: "a"}}}}}); err != nil {
		t.Fatal(err)
	}
	if s := source(); s != models.MappingSourceKeployTest {
		t.Fatalf("after keploy test's write: source %q", s)
	}
	// A startup-only write and record's writes keep it.
	if err := db.Insert(ctx, &models.Mapping{TestSetID: "set", Startup: []models.MockEntry{{Name: "boot"}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t2": {{Name: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if s := source(); s != models.MappingSourceKeployTest {
		t.Fatalf("after writes that name none: source %q", s)
	}
}
