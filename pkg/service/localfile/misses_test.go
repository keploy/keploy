package localfile

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// keploy mock replay and keploy test each write their own misses file: its
// name, its header and its by field say which command wrote it, and a run of
// one, writing its file or removing its last run's, leaves the other's file
// byte for byte. A call whose matcher gave no next step is told the
// writer's; one whose matcher gave one keeps it.
func TestEachCommandWritesItsOwnMissesFile(t *testing.T) {
	keployDir := t.TempDir()
	dir := filepath.Join(keployDir, "set")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	misses := []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n", PathTag: "#000000000001"},
		{Protocol: "HTTP", ActualSummary: "GET /v1/config", NextSteps: "the matcher's own"},
	}
	writers := []Writer{
		{File: MissesFile, Command: "keploy mock replay", Holds: "each call no recorded mock answered in the last replay of this set", NextStep: "record it"},
		{File: TestMissesFile, Command: "keploy test", Holds: "each call made outside every test", NextStep: "record the set again"},
	}
	for i, w := range writers {
		other := filepath.Join(dir, writers[1-i].File)
		// The other command's last run's file, as it left it.
		was := []byte("# the other command's file\nset: set\n")
		if err := os.WriteFile(other, was, 0o600); err != nil {
			t.Fatal(err)
		}
		file := WriteMisses(zap.NewNop(), keployDir, "set", w, misses)
		if file != filepath.Join(dir, w.File) {
			t.Fatalf("%s wrote the misses file at %q, want its own %s", w.Command, file, w.File)
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
		if b, err := os.ReadFile(other); err != nil || !bytes.Equal(b, was) {
			t.Errorf("%s's write left the other command's misses file as %q (%v), want it byte for byte", w.Command, b, err)
		}
		// A run of the set that missed no call removes its own last run's
		// file, and only that.
		if file := WriteMisses(zap.NewNop(), keployDir, "set", w, nil); file != "" {
			t.Fatalf("%s's run with no miss wrote %s", w.Command, file)
		}
		if _, err := os.Lstat(filepath.Join(dir, w.File)); !os.IsNotExist(err) {
			t.Errorf("%s's last run's misses file stands after a run with no miss: %v", w.Command, err)
		}
		if b, err := os.ReadFile(other); err != nil || !bytes.Equal(b, was) {
			t.Errorf("%s's run with no miss left the other command's misses file as %q (%v), want it byte for byte", w.Command, b, err)
		}
		if err := os.Remove(other); err != nil {
			t.Fatal(err)
		}
	}
}

// Each misses file, and the temporary file a write of it goes through (a
// kill before its rename leaves one), holds the calls whole: HoldsCallsWhole
// names each, for anything that sends the keploy folder away to leave them
// out. A receipt's temporary file, which names calls by their shape, is not
// named.
func TestHoldsCallsWholeNamesEachMissesFileAndItsTemporaryFile(t *testing.T) {
	keployDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	var temps []string
	t.Cleanup(SetTempWrittenForTest(func(path string) { temps = append(temps, filepath.Base(path)) }))
	miss := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n"}}
	for _, file := range []string{MissesFile, TestMissesFile} {
		temps = nil
		if got := WriteMisses(zap.NewNop(), keployDir, "set", Writer{File: file, Command: "keploy"}, miss); got == "" {
			t.Fatalf("%s was not written", file)
		}
		if len(temps) != 1 || !HoldsCallsWhole(temps[0]) || !HoldsCallsWhole(file) {
			t.Errorf("HoldsCallsWhole names %s: %v, its temporary files %q: want both named", file, HoldsCallsWhole(file), temps)
		}
	}
	for _, name := range []string{TempPrefix + "2716057188.tmp", "last-replay.yaml", "mocks.yaml"} {
		if HoldsCallsWhole(name) {
			t.Errorf("HoldsCallsWhole(%q) = true for a file that names no call whole", name)
		}
	}
}

// A writer whose file is no misses file writes nothing, and says so: a file
// HoldsCallsWhole does not name would be sent away with the keploy folder.
func TestAMissesWriteToAnotherFileWritesNothing(t *testing.T) {
	keployDir := t.TempDir()
	dir := filepath.Join(keployDir, "set")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zapcore.ErrorLevel)
	miss := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n"}}
	for _, file := range []string{"", "last-misses.yaml"} {
		if got := WriteMisses(zap.New(core), keployDir, "set", Writer{File: file, Command: "keploy test"}, miss); got != "" {
			t.Errorf("a write to %q wrote %s", file, got)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the set's folder holds %v (%v), want nothing written", entries, err)
	}
	if n := logs.FilterMessageSnippet("this is a bug in keploy").Len(); n != 2 {
		t.Fatalf("said %d times, want once per write: %+v", n, logs.All())
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
	w := Writer{File: TestMissesFile, Command: "keploy test", Holds: "each call", Unwritten: "could not write the calls whole; the log names each by its shape"}
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

// A .gitignore line of the user's that un-ignores a command's misses file,
// after keploy's entries, keeps that file from being written: git would not
// ignore the calls it holds whole. What to do names the line. Each command's
// own file is checked, by its own name. The write that fails removes that
// command's last run's file, which names an earlier run's calls, and leaves
// the other command's file byte for byte.
func TestAMissesFileGitDoesNotIgnoreIsNotWritten(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test needs git, to read what git ignores: %v", err)
	}
	for _, file := range []string{MissesFile, TestMissesFile} {
		repo := t.TempDir()
		if out, err := exec.Command(git, "init", "-q", repo).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		keployDir := filepath.Join(repo, "keploy")
		dir := filepath.Join(keployDir, "set")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		other := MissesFile
		if file == MissesFile {
			other = TestMissesFile
		}
		otherWas := []byte("set: set\nby: the other command\n")
		for name, body := range map[string][]byte{file: []byte("set: set\nby: this command's last run\n"), other: otherWas} {
			if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gi := strings.Join(missesIgnore(file), "\n") + "\n!/*/" + file + "\n"
		if err := os.WriteFile(filepath.Join(keployDir, ".gitignore"), []byte(gi), 0o644); err != nil {
			t.Fatal(err)
		}
		core, logs := observer.New(zapcore.WarnLevel)
		w := Writer{File: file, Command: "keploy", Unwritten: "could not write the calls whole"}
		if got := WriteMisses(zap.New(core), keployDir, "set", w, []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n"}}); got != "" {
			t.Fatalf("%s was written at %s though git does not ignore it", file, got)
		}
		said := logs.FilterMessage(w.Unwritten).All()
		if len(said) != 1 || !strings.Contains(fmt.Sprint(said[0].ContextMap()["next_step"]), "un-ignores it") || !strings.Contains(fmt.Sprint(said[0].ContextMap()["error"]), file) {
			t.Errorf("%s: said %+v, want why, naming the file, and the line to remove", file, logs.All())
		}
		if _, err := os.Lstat(filepath.Join(dir, file)); !os.IsNotExist(err) {
			t.Errorf("%s: the last run's file is still there (%v), want it removed: it names an earlier run's calls", file, err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, other)); err != nil || !bytes.Equal(b, otherWas) {
			t.Errorf("%s: the other command's file %s holds %q (%v), want it byte for byte", file, other, b, err)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
			t.Errorf("%s: the set's folder holds %v (%v), want only the other command's file: nothing written", file, entries, err)
		}
	}
}
