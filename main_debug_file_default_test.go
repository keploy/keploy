//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func runWithDebugFileEnv(t *testing.T, dir, debugPath string, args ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Args = append([]string{"keploy"}, args...)
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", "TMPDIR="+t.TempDir(), "KEPLOY_DEBUG_FILE="+debugPath)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func attachInto(t *testing.T, gitignore string, args ...string) (bool, string, *observer.ObservedLogs) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "keploy-host")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if gitignore != "" {
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KEPLOY_DEBUG_FILE", filepath.Join(dir, "agent-debug.log"))
	t.Cleanup(func() { log.SetDebugFileSink(nil) })
	core, logs := observer.New(zapcore.DebugLevel)
	f, sink := maybeAttachDebugFileSink(zap.New(core), args)
	if sink != nil {
		_ = sink.Flush()
	}
	if f != nil {
		_ = f.Close()
	}
	return f != nil && sink != nil, dir, logs
}

func gitignoreLines(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(l); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// Repro on origin/main: a docker-compose agent started with KEPLOY_DEBUG_FILE
// set and --debug=false must not create agent-debug.log or touch .gitignore;
// one started with --debug=true must add unanchored agent-debug.log to the
// folder's .gitignore (so rotated test-set-N/agent-debug.log is ignored too).
func TestComposeAgentDebugFileSubprocessGateAndGitIgnore(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}

	t.Run("no debug flag creates no debug file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "keploy")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		debugPath := filepath.Join(dir, "agent-debug.log")
		out := runWithDebugFileEnv(t, t.TempDir(), debugPath, "agent", "--debug=false", "--help")
		if _, err := os.Stat(debugPath); err == nil {
			t.Fatalf("agent --debug=false created %s (output:\n%s)", debugPath, out)
		}
		if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err == nil {
			t.Fatalf("agent --debug=false touched .gitignore in %s", dir)
		}
	})

	t.Run("debug flag creates debug file and gitignores it at every depth", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "keploy")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		debugPath := filepath.Join(dir, "agent-debug.log")
		out := runWithDebugFileEnv(t, t.TempDir(), debugPath, "agent", "--debug=true", "--help")
		if _, err := os.Stat(debugPath); err != nil {
			t.Fatalf("agent --debug=true did not create %s: %v (output:\n%s)", debugPath, err, out)
		}
		lines := gitignoreLines(t, dir)
		if !slices.Contains(lines, "agent-debug.log") {
			t.Fatalf("agent --debug=true wrote .gitignore %q; want unanchored agent-debug.log", lines)
		}
	})
}

// A run that did not ask for its debug log writes no agent-debug.log and
// touches no .gitignore, even when KEPLOY_DEBUG_FILE is set in the environment.
func TestTheAgentWritesNoDebugFileUnlessDebugIsAsked(t *testing.T) {
	for _, args := range [][]string{
		{"agent"},
		{"agent", "--debug=false"},
		{"agent", "--debug-file=false"},
		{"agent", "--debug", "--debug=false"},
		{"agent", "--debug-file", "--debug-file=false"},
		{"agent", "--", "--debug"},
		{"agent", "--", "--debug-file"},
		{"agent", "--debug=notabool"},
	} {
		attached, dir, _ := attachInto(t, "", args...)
		if attached {
			t.Errorf("%v: the debug file was attached", args)
		}
		if _, err := os.Stat(filepath.Join(dir, "agent-debug.log")); err == nil {
			t.Errorf("%v: agent-debug.log was written into the keploy folder", args)
		}
		if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err == nil {
			t.Errorf("%v: the keploy folder's .gitignore was touched", args)
		}
		if err := log.RotateDebugFileForTestSet("test-set-0"); err != nil {
			t.Errorf("%v: RotateDebugFileForTestSet: %v", args, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "test-set-0", "agent-debug.log")); err == nil {
			t.Errorf("%v: test-set-0/agent-debug.log was written on a non-debug run", args)
		}
	}
}

// In a real git repository, both keploy/agent-debug.log and the rotated
// keploy/test-set-0/agent-debug.log are ignored by git once --debug attaches
// the debug file.
func TestRotatedAgentDebugFileIsGitIgnoredInTestSetFolder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	keployDir := filepath.Join(repo, "keploy")
	if err := os.Mkdir(keployDir, 0o755); err != nil {
		t.Fatal(err)
	}
	debugPath := filepath.Join(keployDir, "agent-debug.log")
	t.Setenv("KEPLOY_DEBUG_FILE", debugPath)
	t.Cleanup(func() { log.SetDebugFileSink(nil) })

	f, sink := maybeAttachDebugFileSink(zap.NewNop(), []string{"agent", "--debug=true"})
	if f == nil || sink == nil {
		t.Fatal("debug file was not attached with --debug=true")
	}
	t.Cleanup(func() {
		_ = sink.Flush()
		_ = f.Close()
	})
	if err := log.RotateDebugFileForTestSet("test-set-0"); err != nil {
		t.Fatalf("RotateDebugFileForTestSet: %v", err)
	}
	rotatedPath := filepath.Join(keployDir, "test-set-0", "agent-debug.log")
	if _, err := os.Stat(rotatedPath); err != nil {
		t.Fatalf("rotated debug file missing: %v", err)
	}
	for _, p := range []string{debugPath, rotatedPath} {
		ignored, known := utils.GitIgnores(repo, p)
		if !known {
			t.Fatalf("git could not classify %s", p)
		}
		if !ignored {
			t.Errorf("%s is NOT ignored by git", p)
		}
	}
}

// When --debug or --debug-file is asked on the run's command line, the debug
// file is attached and the folder's .gitignore gets an unanchored
// agent-debug.log entry so both keploy/agent-debug.log and rotated
// keploy/test-set-N/agent-debug.log are ignored. Existing .gitignore entries
// are preserved.
func TestTheAgentDebugFileIsGitIgnoredAtEveryDepthWhenDebugIsAsked(t *testing.T) {
	for _, tc := range []struct {
		gitignore string
		args      []string
	}{
		{"", []string{"agent", "--debug=true"}},
		{"", []string{"agent", "--debug"}},
		{"", []string{"agent", "-debug"}},
		{"", []string{"agent", "--debug=false", "--debug"}},
		{"", []string{"agent", "--debug-file"}},
		{"", []string{"agent", "--debug-file=true"}},
		{"/cloud-debug.log\n/agent-debug.log\n", []string{"agent", "--debug=false", "--debug-file"}},
	} {
		attached, dir, _ := attachInto(t, tc.gitignore, tc.args...)
		if !attached {
			t.Fatalf("%v: the debug file was not attached", tc.args)
		}
		lines := gitignoreLines(t, dir)
		if !slices.Contains(lines, "agent-debug.log") {
			t.Errorf("%v: .gitignore is %q; want unanchored agent-debug.log", tc.args, lines)
		}
		if tc.gitignore != "" && !slices.Contains(lines, "/cloud-debug.log") {
			t.Errorf("%v: .gitignore is %q; want existing entries kept", tc.args, lines)
		}
	}
}

// The debug file is strictly per-run, never sticky: after a --debug run, a
// --debug-file run, or a run that left /agent-debug.log in .gitignore, a
// subsequent run without --debug/--debug-file writes no agent-debug.log.
func TestTheDebugFileIsNotStickyAfterADebugRun(t *testing.T) {
	for _, earlier := range []struct {
		name      string
		gitignore string
		args      []string
	}{
		{"after one --debug run", "", []string{"agent", "--debug=true"}},
		{"after one --debug-file run", "", []string{"agent", "--debug-file"}},
		{"after an earlier .gitignore entry", "/cloud-debug.log\n/agent-debug.log\n", nil},
	} {
		t.Run(earlier.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "keploy-host")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if earlier.gitignore != "" {
				if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(earlier.gitignore), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "agent-debug.log")
			t.Setenv("KEPLOY_DEBUG_FILE", path)
			t.Cleanup(func() { log.SetDebugFileSink(nil) })
			run := func(args []string) bool {
				f, sink := maybeAttachDebugFileSink(zap.NewNop(), args)
				if sink != nil {
					_ = sink.Flush()
				}
				if f != nil {
					_ = f.Close()
				}
				log.SetDebugFileSink(nil)
				return f != nil
			}
			if earlier.args != nil && !run(earlier.args) {
				t.Fatalf("the earlier run %v wrote no debug file", earlier.args)
			}
			_ = os.Remove(path)
			for _, later := range [][]string{{"agent"}, {"agent", "--debug=false"}} {
				if run(later) {
					t.Errorf("a later run %v wrote %s", later, path)
				}
				if _, err := os.Stat(path); err == nil {
					t.Errorf("a later run %v left %s", later, path)
					_ = os.Remove(path)
				}
			}
		})
	}
}

// If the folder's .gitignore cannot be written (for example, .gitignore is a
// symlink pointing outside the folder), maybeAttachDebugFileSink refuses to
// open agent-debug.log rather than writing an un-ignored debug log into a
// folder users commit.
func TestTheAgentRefusesDebugFileWhenGitIgnoreCannotBeWritten(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "keploy-host")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside-gitignore")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "outside-gitignore"), filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent-debug.log")
	t.Setenv("KEPLOY_DEBUG_FILE", path)
	t.Cleanup(func() { log.SetDebugFileSink(nil) })
	core, logs := observer.New(zapcore.WarnLevel)
	f, sink := maybeAttachDebugFileSink(zap.New(core), []string{"agent", "--debug=true"})
	if f != nil || sink != nil {
		if sink != nil {
			_ = sink.Flush()
		}
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("debug file attached despite unwritable .gitignore")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("agent-debug.log was created despite .gitignore refusal")
	}
	if logs.FilterMessageSnippet(".gitignore could not be made to ignore it").Len() != 1 {
		t.Errorf("want one warning explaining the .gitignore refusal; got %+v", logs.All())
	}
	if got, _ := os.ReadFile(outside); string(got) != "keep\n" {
		t.Errorf("outside file changed to %q", got)
	}
}
