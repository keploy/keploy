package mapdb

import (
	"context"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the switch a move names the test it took the mock out of as its owner,
// unless the move names one, and carries the move's drift; a worker entry
// placed again keeps its owner and takes the move's drift. Without the switch
// no owner is written (the switch-off golden pins those bytes), and the
// drift is written as given, as the move's reason is.
func TestMoveToWorkerTiesAMoveToItsOwner(t *testing.T) {
	for _, on := range []bool{true, false} {
		for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			name := format.FileExtension() + map[bool]string{true: "/switch on", false: "/switch off"}[on]
			t.Run(name, func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				ctx := context.Background()
				db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
				if err := db.Insert(ctx, goldenMapping()); err != nil {
					t.Fatal(err)
				}
				if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{
					{Name: "mock-6", Drift: true},             // out of post-url-1, drifted
					{Name: "mock-3", Owner: "named-by-judge"}, // out of post-url-0, its owner named
				}); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetMapping(ctx, "test-set-0")
				if err != nil {
					t.Fatal(err)
				}
				byName := map[string]models.MockEntry{}
				for _, e := range got.Worker {
					byName[e.Name] = e
				}
				wantOwner := ""
				if on {
					wantOwner = "post-url-1"
				}
				if e := byName["mock-6"]; e.Owner != wantOwner || !e.Drift {
					t.Errorf("mock-6 moved with owner %q, drift %v; want %q, true", e.Owner, e.Drift, wantOwner)
				}
				if e := byName["mock-3"]; e.Owner != "named-by-judge" || e.Drift {
					t.Errorf("mock-3 moved with owner %q, drift %v; want the move's owner, no drift", e.Owner, e.Drift)
				}
				// Placed again: the owner stays, the drift is the last
				// placement's.
				if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{{Name: "mock-6"}}); err != nil {
					t.Fatal(err)
				}
				got, _ = db.GetMapping(ctx, "test-set-0")
				for _, e := range got.Worker {
					if e.Name == "mock-6" && (e.Owner != wantOwner || e.Drift) {
						t.Errorf("mock-6 placed again has owner %q, drift %v; want %q, false", e.Owner, e.Drift, wantOwner)
					}
				}
			})
		}
	}
}

// Every rewrite of mappings.yaml without the switch keeps the set's
// RecordedWith mark and each entry's owner and drift a run with it wrote:
// the writes keploy record and keploy test make, keploy
// test's whole-file Insert included, and a writer that names a mark replaces
// it.
func TestTheMarkAndAMovesOwnerSurviveARewriteWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			ctx := context.Background()
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
			m := goldenMapping()
			m.RecordedWith = []string{models.RecordedWithS12Routing}
			m.Worker[1].Owner, m.Worker[1].Drift = "post-url-1", true
			if err := db.Insert(ctx, m); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{"post-url-2": {{Name: "mock-9", Kind: "Mongo"}}}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "test-set-0", map[string]models.MappedTestCase{"post-url-1": {Cases: []string{"post-url-1"}}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertBoots(ctx, "test-set-0", []models.BootSpec{{Key: "app", Mocks: []models.MockEntry{}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{{Name: "mock-3"}}); err != nil {
				t.Fatal(err)
			}
			// keploy test's whole-file write: no mark, no worker scope.
			if err := db.Insert(ctx, &models.Mapping{TestSetID: "test-set-0", TestCases: []models.MappedTestCase{{ID: "post-url-0", Mocks: []models.MockEntry{{Name: "mock-3", Kind: "Mongo"}}}}}); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetMapping(ctx, "test-set-0")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.RecordedWith, []string{models.RecordedWithS12Routing}) || !got.WasRecordedWith(models.RecordedWithS12Routing) {
				t.Errorf("the set's mark is %v after the rewrites; want [%s]", got.RecordedWith, models.RecordedWithS12Routing)
			}
			found := false
			for _, e := range got.Worker {
				if e.Name == "mock-5" {
					found = true
					if e.Owner != "post-url-1" || !e.Drift {
						t.Errorf("mock-5's owner and drift are %q, %v after the rewrites; want post-url-1, true", e.Owner, e.Drift)
					}
				}
			}
			if !found {
				t.Fatal("the rewrites lost the worker entry mock-5")
			}
			// The worker scope written whole (keploy record's) keeps the mark.
			if err := db.UpsertWorker(ctx, "test-set-0", nil); err != nil {
				t.Fatal(err)
			}
			if got, _ = db.GetMapping(ctx, "test-set-0"); !got.WasRecordedWith(models.RecordedWithS12Routing) {
				t.Errorf("UpsertWorker dropped the set's mark: %v", got.RecordedWith)
			}
			// A writer that names a mark replaces the file's.
			if err := db.Insert(ctx, &models.Mapping{TestSetID: "test-set-0", RecordedWith: []string{"another"}}); err != nil {
				t.Fatal(err)
			}
			if got, _ = db.GetMapping(ctx, "test-set-0"); !reflect.DeepEqual(got.RecordedWith, []string{"another"}) {
				t.Errorf("a writer's mark gave %v; want [another]", got.RecordedWith)
			}
		})
	}
}

// MarkRecordedWith adds a mark to the set's, once, keeping every other
// section; a set with no mapping file, and an empty mark, are errors.
func TestMarkRecordedWithAddsTheSetsMarkOnce(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			ctx := context.Background()
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
			if err := db.MarkRecordedWith(ctx, "test-set-0", models.RecordedWithS12Routing); err == nil {
				t.Fatal("a set with no mapping file was marked")
			}
			if err := db.Insert(ctx, goldenMapping()); err != nil {
				t.Fatal(err)
			}
			if err := db.MarkRecordedWith(ctx, "test-set-0", ""); err == nil {
				t.Fatal("an empty mark was written")
			}
			for range 2 {
				if err := db.MarkRecordedWith(ctx, "test-set-0", models.RecordedWithS12Routing); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.MarkRecordedWith(ctx, "test-set-0", "another"); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetMapping(ctx, "test-set-0")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.RecordedWith, []string{models.RecordedWithS12Routing, "another"}) {
				t.Fatalf("the set's marks are %v; want [%s another]", got.RecordedWith, models.RecordedWithS12Routing)
			}
			if len(got.TestCases) != 2 || len(got.Worker) != 2 || len(got.Startup) != 1 {
				t.Fatalf("the mark lost a section: %+v", got)
			}
		})
	}
}
