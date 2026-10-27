package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// setFiles is every file of a set's directory, by name, as bytes.
func setFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// Rows 2, 3 and 7 on disk, in every format: what record's full round
// learns is written with its about and reads back as learned (its sum
// survives the encoding); a person's entries in the same file survive the
// rewrite as written, never stamped, and a learned use place does not join
// their variable; and learning it all again in a later run writes nothing.
func TestLearningWritesWhereItCameFromOnceAndLeavesAPersonsEntries(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	for _, format := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON), mockFormatGob} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			ys := clockStore(dir, format)
			hand := &models.About{By: models.AboutByHand, Why: "ours"}
			person := variablesMock()
			person.Name = "mock-0"
			person.Spec.Variables = models.MockVariables{"orderId": {Recorded: "o1", Bind: "body.id", About: hand}}
			person.Spec.ReqBodyNoise = map[string][]string{"body.item": {}}
			person.Spec.NoiseAbout = map[string]models.About{"body.item": *hand}
			fresh := variablesMock()
			fresh.Spec.Variables = nil
			for _, m := range []*models.Mock{person, fresh} {
				if err := ys.InsertMock(ctx, m, "test-set-0"); err != nil {
					t.Fatal(err)
				}
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			states := func(run string) map[string]models.MockState {
				return learn.StampStates(map[string]models.MockState{
					"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.item": {}},
						Variables: models.MockVariables{"orderId": {Recorded: "o1", Bind: "body.id", Use: []string{"body.ref"}}}},
					"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"body.item": {}},
						Variables: models.MockVariables{"orderId": {Recorded: "o1", Bind: "body.id", Answer: []string{"body.id"},
							Clock: []models.ClockRead{{At: time.Date(2026, 10, 27, 5, 0, 0, 0, time.FixedZone("x", 3600)), Res: time.Second}}}},
						Clocks: map[string]models.ClockOffset{"body.at": {Res: time.Millisecond, Layout: "rfc3339"}}},
				}, learn.Stamp{Round: models.AboutRoundFull, Run: run, Keploy: "3.8.91", Test: map[string]string{"mock-0": "t::A", "mock-1": "t::A"}})
			}
			added, err := clockStore(dir, format).PersistLearned(ctx, "test-set-0", states("2026-10-27T05:00:00Z"))
			if err != nil || len(added["mock-0"]) != 0 || len(added["mock-1"]) != 3 {
				t.Fatalf("persist: %v %v", added, err)
			}
			back := clockStore(dir, format)
			m1 := readBack(t, back, "test-set-0", "mock-1")
			for what, s := range map[string]models.AboutState{
				"noise":    models.NoiseAboutState(&m1.Spec, "body.item"),
				"variable": models.VariableAboutState("orderId", m1.Spec.Variables["orderId"]),
				"clock":    models.ClockAboutState(&m1.Spec, "body.at"),
			} {
				if s != models.AboutLearned {
					t.Errorf("the learned %s reads back as %v", what, s)
				}
			}
			if a := m1.Spec.Variables["orderId"].About; a == nil || a.Test != "t::A" || a.Run != "2026-10-27T05:00:00Z" {
				t.Fatalf("the variable's about: %+v", a)
			}
			m0 := readBack(t, back, "test-set-0", "mock-0")
			if v := m0.Spec.Variables["orderId"]; len(v.Use) != 0 || v.About == nil || v.About.By != hand.By || v.About.Why != hand.Why || v.About.Sum != "" {
				t.Fatalf("a person's variable after the rewrite: %+v %+v", v, v.About)
			}
			if a := m0.Spec.NoiseAbout["body.item"]; a.By != models.AboutByHand || a.Why != "ours" || a.Sum != "" {
				t.Fatalf("a person's noise after the rewrite: %+v", a)
			}
			if err := back.Close(); err != nil {
				t.Fatal(err)
			}
			before := setFiles(t, filepath.Join(dir, "test-set-0"))
			added, err = clockStore(dir, format).PersistLearned(ctx, "test-set-0", states("2026-10-28T09:00:00Z"))
			if err != nil || len(added) != 0 {
				t.Fatalf("re-learning: %v %v", added, err)
			}
			if after := setFiles(t, filepath.Join(dir, "test-set-0")); len(after) != len(before) {
				t.Fatalf("re-learning changed the set's files: %v", after)
			} else {
				for name, b := range before {
					if after[name] != b {
						t.Fatalf("re-learning rewrote %s", name)
					}
				}
			}
		})
	}
}
