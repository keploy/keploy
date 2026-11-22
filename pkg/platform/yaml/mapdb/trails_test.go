package mapdb

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Each move's Trails (the earlier test whose call the entry trails) is
// written on the entry it moves or places as given, with or without the
// proven switch: a move that names none leaves a placed entry none, so a
// trail is only ever the last proof's. An entry no move names keeps its
// own.
func TestMoveToWorkerWritesEachMovesTrail(t *testing.T) {
	for _, sw := range []string{"1", ""} {
		for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			t.Run("switch="+sw+"/"+format.FileExtension(), func(t *testing.T) {
				t.Setenv(provenswitch.Env, sw)
				ctx := context.Background()
				db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
				m := goldenMapping()
				m.Worker[0].Trails = "post-url-0" // mock-2: placed again by a move that names no trail
				m.Worker[1].Trails = "post-url-0" // mock-5: no move names it
				if err := db.Insert(ctx, m); err != nil {
					t.Fatal(err)
				}
				if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{
					{Name: "mock-7", Trails: "post-url-0", Reason: "trailing from post-url-0"}, // a test's entry moves, with its trail
					{Name: "mock-2"}, // a worker entry is placed again, with none
				}); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetMapping(ctx, "test-set-0")
				if err != nil {
					t.Fatal(err)
				}
				trails := map[string]string{}
				for _, e := range got.Worker {
					trails[e.Name] = e.Trails
				}
				if trails["mock-7"] != "post-url-0" {
					t.Errorf("mock-7 moved with the trail post-url-0, and has %q", trails["mock-7"])
				}
				if v, ok := trails["mock-2"]; !ok || v != "" {
					t.Errorf("mock-2, placed again by a move naming no trail, has %q (listed %v); want none", v, ok)
				}
				if trails["mock-5"] != "post-url-0" {
					t.Errorf("mock-5, which no move names, has the trail %q; want its own", trails["mock-5"])
				}
			})
		}
	}
}

// A rewrite of mappings.yaml without the proven switch keeps the trail a
// run with it wrote, as it keeps every field of an entry: each write
// decodes the file whole and encodes it back.
func TestATrailSurvivesARewriteWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			ctx := context.Background()
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
			m := goldenMapping()
			m.Worker[1].Trails = "post-url-0"
			if err := db.Insert(ctx, m); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
				"post-url-1": {{Name: "mock-8", Kind: "Mongo"}},
				"post-url-2": {{Name: "mock-9", Kind: "Mongo"}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertBoots(ctx, "test-set-0", []models.BootSpec{{Key: "app", Mocks: []models.MockEntry{}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{{Name: "mock-3"}}); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetMapping(ctx, "test-set-0")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range got.Worker {
				want := ""
				if e.Name == m.Worker[1].Name {
					want = "post-url-0"
				}
				if e.Trails != want {
					t.Errorf("worker entry %s has the trail %q after the rewrites; want %q", e.Name, e.Trails, want)
				}
			}
		})
	}
}
