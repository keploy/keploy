package replay

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A run's report holds each test's calls whole, so keploy/.gitignore's
// "/reports/" keeps it local; where a later line of the user's un-ignores
// the reports ("!reports/"), git would commit them, and the run says so at
// WARN, once, asking git itself (git check-ignore). With the entry alone it
// says nothing.
func TestARunSaysOnceWhenGitWouldTrackItsReports(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test needs git, to read what git ignores: %v", err)
	}
	for name, after := range map[string]string{
		"with the entry alone":     "",
		"un-ignoring the reports":  "!reports/\n",
		"un-ignoring every report": "!reports/**\n!reports/\n",
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			if out, err := exec.Command(git, "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			keployDir := filepath.Join(repo, "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "reports", "test-run-0"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := utils.AddToGitIgnore(zap.NewNop(), keployDir, "/reports/"); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(keployDir, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(after)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zapcore.WarnLevel)
			cfg := &config.Config{}
			cfg.Path = keployDir
			r := &Replayer{logger: zap.New(core), config: cfg}
			r.sayReportsTracked("test-run-0")
			r.sayReportsTracked("test-run-0")
			said := logs.FilterMessageSnippet("git does not ignore this run's reports").Len()
			if want := map[bool]int{true: 0, false: 1}[after == ""]; said != want {
				t.Fatalf("the run said %d times that git would track its reports, want %d: %+v", said, want, logs.All())
			}
		})
	}
}
