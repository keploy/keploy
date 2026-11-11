package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// SetSources writes the declared sources on every entry it names, in any
// list (a test's, worker:), and they survive the rewrites a record and a
// judge make after it, a move to worker: among them. A second call names
// the set's whole sources: an entry it does not name loses its own, and a
// call that changes nothing writes nothing. Sources keploy cannot decide a
// field by, a name no entry has and a set with no mapping file are refused,
// writing nothing. In both formats, whatever the switch.
func TestSetSourcesMarksEntriesAndReplacesTheSetsSources(t *testing.T) {
	clock := models.DeclaredSource{Rule: "at@0a1b2c3d", Kind: models.DeclaredSourceClock, Field: "body.docs.msgs.0.at", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}}
	minted := models.DeclaredSource{Rule: "id@44aa55bb", Kind: models.DeclaredSourceMinted, Field: "body.docs.msgs.0._id"}
	for _, sw := range []string{"1", ""} {
		for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			t.Run("switch="+sw+"/"+format.FileExtension(), func(t *testing.T) {
				t.Setenv(provenswitch.Env, sw)
				ctx := context.Background()
				dir := t.TempDir()
				db := NewWithFormat(zap.NewNop(), dir, "mappings", format)
				if err := db.Insert(ctx, goldenMapping()); err != nil {
					t.Fatal(err)
				}
				sourcesOf := func() map[string][]models.DeclaredSource {
					t.Helper()
					m, err := db.GetMapping(ctx, "test-set-0")
					if err != nil {
						t.Fatal(err)
					}
					out := map[string][]models.DeclaredSource{}
					lists := [][]models.MockEntry{m.Startup, m.Worker}
					for _, tc := range m.TestCases {
						lists = append(lists, tc.Mocks)
					}
					for _, list := range lists {
						for _, e := range list {
							if len(e.Sources) > 0 {
								out[e.Name] = e.Sources
							}
						}
					}
					return out
				}
				want := map[string][]models.DeclaredSource{"mock-3": {clock, minted}, "mock-6": {minted}, "mock-2": {clock}}
				if err := db.SetSources(ctx, "test-set-0", want); err != nil {
					t.Fatal(err)
				}
				if got := sourcesOf(); !reflect.DeepEqual(got, want) {
					t.Fatalf("sources %v, want %v", got, want)
				}
				if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{{Name: "mock-6", Place: "post-url-1", Reason: "moved"}}); err != nil {
					t.Fatal(err)
				}
				if err := db.SetRules(ctx, "test-set-0", "sha256:abc123"); err != nil {
					t.Fatal(err)
				}
				if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{"post-url-1": {{Name: "mock-8", Kind: "Mongo"}}}); err != nil {
					t.Fatal(err)
				}
				if got := sourcesOf(); !reflect.DeepEqual(got, want) {
					t.Fatalf("after a move and the rewrites: sources %v, want %v", got, want)
				}
				only := map[string][]models.DeclaredSource{"mock-3": {minted}}
				if err := db.SetSources(ctx, "test-set-0", only); err != nil {
					t.Fatal(err)
				}
				if got := sourcesOf(); !reflect.DeepEqual(got, only) {
					t.Fatalf("a second call: sources %v, want only %v", got, only)
				}
				path := filepath.Join(dir, "test-set-0", "mappings."+format.FileExtension())
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
					t.Fatal(err)
				}
				if err := db.SetSources(ctx, "test-set-0", only); err != nil {
					t.Fatal(err)
				}
				if st, err := os.Stat(path); err != nil || !st.ModTime().Equal(time.Unix(1, 0)) {
					t.Fatalf("a call that changes nothing rewrote the file (%v, %v)", st, err)
				}
				for name, bad := range map[string]map[string][]models.DeclaredSource{
					"a name no entry has": {"mock-3": {clock}, "mock-404": {minted}},
					"an unknown kind":     {"mock-3": {{Rule: "x@00000000", Kind: "answer", Field: "body.x"}}},
					"two on one field":    {"mock-3": {minted, {Rule: "y@00000000", Kind: models.DeclaredSourceMinted, Field: minted.Field}}},
				} {
					if err := db.SetSources(ctx, "test-set-0", bad); err == nil {
						t.Fatalf("%s: SetSources took %v", name, bad)
					}
					if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
						t.Fatalf("%s: a refused call rewrote the file (%v)", name, err)
					}
				}
				if err := db.SetSources(ctx, "test-set-9", only); err == nil || !strings.Contains(err.Error(), "has no mappings file") {
					t.Fatalf("a set with no mapping file: %v, want it refused as one", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "test-set-9")); !os.IsNotExist(err) {
					t.Fatalf("a refused call made the set (%v)", err)
				}
			})
		}
	}
}
