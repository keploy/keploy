package mockdb

import (
	"context"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A mask the learn step learned (a value that drifted only by F's clock,
// founder decision 26) is written onto the recording and read back as it
// was, in every storage format; a leaf learned whole later widens it, and a
// leaf learned whole is never narrowed to a mask.
func TestPersistLearnedWritesAMask(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mask := learn.ClockMask("order 1234 placed at 2026-10-14T10:00:41Z", "order 1234 placed at 2026-10-14T10:01:42Z")
	// Another pairing may mask other reads of the leaf.
	other := `^order 1234 placed at \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`
	if mask == "" || mask == other {
		t.Fatal("no mask")
	}
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			ctx := context.Background()
			set := "test-set-0"
			if err := ys.InsertMock(ctx, noiseTestMock(`{"note":"order 1234 placed at 2026-10-14T10:00:41Z","at":"x"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			read := func() map[string][]string {
				t.Helper()
				mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range append(mocks, filtered...) {
					if m.Name == "mock-0" {
						return m.Spec.ReqBodyNoise
					}
				}
				t.Fatal("mock-0 not read back")
				return nil
			}
			persist := func(noise map[string][]string) []string {
				t.Helper()
				added, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", ReqBodyNoise: noise}})
				if err != nil {
					t.Fatal(err)
				}
				return added["mock-0"]
			}

			if added := persist(map[string][]string{"body.note": {mask}, "body.at": {}}); !reflect.DeepEqual(added, []string{"body.at (noise)", "body.note (noise)"}) {
				t.Fatalf("added %v", added)
			}
			if got := read(); !reflect.DeepEqual(got, map[string][]string{"body.note": {mask}, "body.at": {}}) {
				t.Fatalf("read back %q, want the mask %q as written", got, mask)
			}
			if added := persist(map[string][]string{"body.note": {mask}}); len(added) != 0 {
				t.Fatalf("the same mask again added %v", added)
			}
			// Another mask of the leaf: either excuses it.
			if added := persist(map[string][]string{"body.note": {other}}); !reflect.DeepEqual(added, []string{"body.note (noise)"}) {
				t.Fatalf("another mask added %v", added)
			}
			if got := read(); !reflect.DeepEqual(got["body.note"], []string{mask, other}) {
				t.Fatalf("read back %q", got["body.note"])
			}
			// Learned whole: whole from then on.
			if added := persist(map[string][]string{"body.note": {}}); !reflect.DeepEqual(added, []string{"body.note (noise)"}) {
				t.Fatalf("widening to whole added %v", added)
			}
			if added := persist(map[string][]string{"body.note": {mask}, "body.at": {mask}}); len(added) != 0 {
				t.Fatalf("a mask narrowed a leaf learned whole: %v", added)
			}
			if got := read(); !reflect.DeepEqual(got, map[string][]string{"body.note": {}, "body.at": {}}) {
				t.Fatalf("read back %q", got)
			}
		})
	}
}
