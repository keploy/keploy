package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Each move's Rule and Series (the declared rule that placed the entry, and
// the series of one canonical request it answers in) are written on the
// entry it moves or places as given, with or without the proven switch, as
// its Trails is: a move that names none leaves a placed entry none. Every
// later rewrite of mappings.yaml keeps both, and the set's rules digest
// (SetRules) with them.
func TestMoveToWorkerWritesRuleAndSeries(t *testing.T) {
	for _, sw := range []string{"1", ""} {
		for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			t.Run("switch="+sw+"/"+format.FileExtension(), func(t *testing.T) {
				t.Setenv(provenswitch.Env, sw)
				ctx := context.Background()
				db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
				m := goldenMapping()
				m.Worker[0].Rule, m.Worker[0].Series = "r-old@00000000", "r-old/-/0" // mock-2: placed again by a move that names none
				m.Worker[1].Rule, m.Worker[1].Series = "r-bg@0badcafe", "r-bg/-/0"   // mock-5: no move names it
				if err := db.Insert(ctx, m); err != nil {
					t.Fatal(err)
				}
				if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{
					{Name: "mock-7", Place: "post-url-0", Trails: "post-url-0", Rule: "cleanup@3f2a91c0", Series: "cleanup/post-url-0/0", Reason: "trailing from post-url-0"},
					{Name: "mock-2"},
				}); err != nil {
					t.Fatal(err)
				}
				if err := db.SetRules(ctx, "test-set-0", "sha256:abc123"); err != nil {
					t.Fatal(err)
				}
				// The rewrites a record and a judge make after the move.
				if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
					"post-url-1": {{Name: "mock-8", Kind: "Mongo"}},
				}); err != nil {
					t.Fatal(err)
				}
				if err := db.UpsertBoots(ctx, "test-set-0", []models.BootSpec{{Key: "app", Mocks: []models.MockEntry{}}}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{{Name: "mock-3"}}); err != nil {
					t.Fatal(err)
				}
				if err := db.MarkRecordedWith(ctx, "test-set-0", "feature-x"); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetMapping(ctx, "test-set-0")
				if err != nil {
					t.Fatal(err)
				}
				type marks struct{ rule, series string }
				want := map[string]marks{
					"mock-7": {"cleanup@3f2a91c0", "cleanup/post-url-0/0"},
					"mock-2": {},
					"mock-5": {"r-bg@0badcafe", "r-bg/-/0"},
					"mock-3": {},
				}
				seen := map[string]bool{}
				for _, e := range got.Worker {
					seen[e.Name] = true
					w, ok := want[e.Name]
					if !ok {
						continue
					}
					if e.Rule != w.rule || e.Series != w.series {
						t.Errorf("worker entry %s has rule %q series %q after the rewrites; want %q %q", e.Name, e.Rule, e.Series, w.rule, w.series)
					}
				}
				for name := range want {
					if !seen[name] {
						t.Errorf("worker entry %s is missing after the rewrites", name)
					}
				}
				if got.Rules != "sha256:abc123" {
					t.Errorf("the set's rules digest is %q after the rewrites; want sha256:abc123", got.Rules)
				}
			})
		}
	}
}

// SetRules writes the set's digest over the file a person edited, keeping
// their comment, and an empty digest removes the key; a set with no
// mapping file is refused, not created.
func TestSetRulesKeepsHandEditsAndRefusesAMissingSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	dir := t.TempDir()
	db := NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatYAML)
	if err := db.Insert(ctx, goldenMapping()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test-set-0", "mappings.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const note = "# a reader's note on the set"
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), "test_set_id:", note+"\ntest_set_id:", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRules(ctx, "test-set-0", "sha256:feed"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), note) {
		t.Errorf("SetRules dropped the reader's comment:\n%s", data)
	}
	if !strings.Contains(string(data), "rules: sha256:feed") {
		t.Errorf("SetRules wrote no rules: line:\n%s", data)
	}
	if err := db.SetRules(ctx, "test-set-0", ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMapping(ctx, "test-set-0")
	if err != nil {
		t.Fatal(err)
	}
	if got.Rules != "" {
		t.Errorf("an empty digest left rules %q", got.Rules)
	}
	if err := db.SetRules(ctx, "test-set-9", "sha256:feed"); err == nil {
		t.Error("SetRules on a set with no mapping file returned no error")
	}
	if _, err := os.Stat(filepath.Join(dir, "test-set-9")); err == nil {
		t.Error("SetRules created a set that had no mapping file")
	}
}
