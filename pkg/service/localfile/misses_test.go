package localfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// keploy mock replay and keploy test write one misses file: its header and
// its by field say which command wrote it, for a reader of one command's
// misses to read only that command's. A call whose matcher gave no next step
// is told the writer's; one whose matcher gave one keeps it.
func TestTheMissesFileSaysWhichCommandWroteIt(t *testing.T) {
	keployDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	misses := []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n", PathTag: "#000000000001"},
		{Protocol: "HTTP", ActualSummary: "GET /v1/config", NextSteps: "the matcher's own"},
	}
	for _, w := range []Writer{
		{Command: "keploy mock replay", Holds: "each call no recorded mock answered in the last replay of this set", NextStep: "record it"},
		{Command: "keploy test", Holds: "each call made outside every test", NextStep: "record the set again"},
	} {
		file := WriteMisses(zap.NewNop(), keployDir, "set", w, misses)
		if file != filepath.Join(keployDir, "set", MissesFile) {
			t.Fatalf("%s wrote the misses file at %q", w.Command, file)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		head := "# Written by `" + w.Command + "`: " + w.Holds + ", whole, beside the path tag keploy's log names it by. Local and git-ignored.\n"
		if !strings.HasPrefix(string(raw), head) {
			t.Errorf("%s's misses file begins\n%s", w.Command, strings.SplitN(string(raw), "\n", 2)[0])
		}
		var got MissesReport
		if err := yaml.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.By != w.Command || got.Set != "set" || len(got.Misses) != 2 {
			t.Fatalf("%s's misses file holds %+v", w.Command, got)
		}
		if got.Misses[0].Call != "GET /v1/reset/t0k3n" || got.Misses[0].NextStep != w.NextStep {
			t.Errorf("%s names the first call %+v, want it whole with the writer's next step", w.Command, got.Misses[0])
		}
		if got.Misses[1].NextStep != "the matcher's own" {
			t.Errorf("%s names the second call %+v, want its matcher's next step", w.Command, got.Misses[1])
		}
	}
}

// A misses file that cannot be written is said in the writer's words, with
// why and what to do, and nothing is left in the set's folder.
func TestAnUnwrittenMissesFileIsSaidInTheWritersWords(t *testing.T) {
	keployDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetAddGitIgnoreForTest(func(*zap.Logger, string, string) error { return os.ErrPermission }))
	core, logs := observer.New(zapcore.WarnLevel)
	w := Writer{Command: "keploy test", Holds: "each call", Unwritten: "could not write the calls whole; the log names each by its shape"}
	if file := WriteMisses(zap.New(core), keployDir, "set", w, []models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A db.example."}}); file != "" {
		t.Fatalf("the misses file was written at %s with keploy/.gitignore read-only", file)
	}
	said := logs.FilterMessage(w.Unwritten).All()
	if len(said) != 1 || !strings.Contains(said[0].ContextMap()["next_step"].(string), "writable") {
		t.Fatalf("the writer said %+v, want its own line with what to do", logs.All())
	}
	if entries, err := os.ReadDir(filepath.Join(keployDir, "set")); err != nil || len(entries) != 0 {
		t.Fatalf("the set's folder holds %v (%v), want nothing written", entries, err)
	}
}
