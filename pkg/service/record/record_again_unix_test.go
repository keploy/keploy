//go:build unix

package record

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The command a call outside every test names to record its set again
// (utils.RecordSetAgain), run by a POSIX shell, moves that set's folder and
// no other out of the keploy folder, recordings and all, into the run's
// folder of keploy-replaced beside it, hands keploy record the app's command
// as it was given ($ and quotes kept), and keploy record then records under
// the set's own name: the set is recorded again in its place, not into a new
// set beside it that would leave the run red the same way.
func TestTheRecordAgainCommandRecordsTheFailingSetInItsPlace(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell")
	}
	work := t.TempDir()
	root := filepath.Join(work, "my tests", "keploy")
	for _, set := range []string{"test-set-0", "test-set-1", "test-set-10"} {
		if err := os.MkdirAll(filepath.Join(root, set, "tests"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, set, "tests", "test-1.yaml"), []byte(set), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// keploy here writes the words it was given, one per line.
	bin := t.TempDir()
	argsFile := filepath.Join(work, "args")
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + utils.ShellWord(argsFile) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "keploy"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	app := `bash -c "echo $PORT; it's"`
	again := utils.RecordSetAgain(root, filepath.Join(work, "conf"), work, "test-set-1", "test-run-4", app)
	cmdLine := again.Command
	if cmdLine == "" {
		t.Fatal("no command for test-set-1")
	}
	run := exec.Command(sh, "-c", cmdLine)
	run.Dir = work
	run.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "PORT=8080")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", cmdLine, err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "test-set-1")); !os.IsNotExist(err) {
		t.Fatalf("test-set-1 is still there (%v); the command must move the failing set out", err)
	}
	if want := filepath.Join(work, "my tests", "keploy-replaced", "test-set-1-test-run-4"); again.Replaced != want {
		t.Fatalf("the recordings go to %s; want %s", again.Replaced, want)
	}
	if b, err := os.ReadFile(filepath.Join(again.Replaced, "test-set-1", "tests", "test-1.yaml")); err != nil || string(b) != "test-set-1" {
		t.Fatalf("test-set-1's recordings in %s: %q (%v); the command keeps them", again.Replaced, b, err)
	}
	for _, set := range []string{"test-set-0", "test-set-10"} {
		if b, err := os.ReadFile(filepath.Join(root, set, "tests", "test-1.yaml")); err != nil || string(b) != set {
			t.Fatalf("%s's recordings are %q (%v); the command moves the failing set alone", set, b, err)
		}
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	want := []string{"record", "-c", app, "--path", filepath.Join(work, "my tests"), "--config-path", filepath.Join(work, "conf"), "--metadata", "name=test-set-1"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("keploy was given %q; want %q", args, want)
	}
	// keploy record, given that --metadata, names the new recording
	// test-set-1: the set the run failed.
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, testDB: testdb.New(zap.NewNop(), root)}
	r.config.Record.Metadata = args[len(args)-1]
	if id, err := r.GetNextTestSetID(context.Background()); err != nil || id != "test-set-1" {
		t.Fatalf("keploy record records into %q (%v); want test-set-1, the set the run failed", id, err)
	}
	// With the folder left in place, the same record goes to a new set:
	// why the command moves it first.
	if err := os.MkdirAll(filepath.Join(root, "test-set-1", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if id, err := r.GetNextTestSetID(context.Background()); err != nil || id == "test-set-1" {
		t.Fatalf("with test-set-1 in place keploy record records into %q (%v); want another set", id, err)
	}
}
