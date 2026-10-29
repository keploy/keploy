//go:build !windows

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/log"
)

// heldRun starts `keploy --help` in a scratch working directory with a temp
// directory of its own, held in start() after its log exists: its
// KEPLOY_DEBUG_FILE is a named pipe, whose open waits for a reader. env is
// added to its environment. It returns the run, its stdout, the directories,
// the pipe and the log the run created.
func heldRun(t *testing.T, env ...string) (cmd *exec.Cmd, out *bytes.Buffer, dir, tmp, fifo, logPath string) {
	t.Helper()
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}
	dir, tmp = t.TempDir(), t.TempDir()
	fifo = filepath.Join(t.TempDir(), "debug.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot make a named pipe here: %v", err)
	}
	cmd = exec.Command(os.Args[0])
	cmd.Args = []string{"keploy", "--help"}
	cmd.Env = append(append(os.Environ(), subprocessEnv+"=1", "TMPDIR="+tmp, "KEPLOY_DEBUG_FILE="+fifo), env...)
	cmd.Dir = dir
	out = &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// The log is created before the debug file's open waits: wait for it.
	deadline := time.Now().Add(60 * time.Second)
	for {
		logs, _ := filepath.Glob(filepath.Join(tmp, log.LogFilePattern))
		if len(logs) == 1 {
			return cmd, out, dir, tmp, fifo, logs[0]
		}
		if len(logs) > 1 {
			t.Fatalf("one run created %d logs: %v", len(logs), logs)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run created no %s in its temp directory %s; it said:\n%s", log.LogFilePattern, tmp, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func requireEmpty(t *testing.T, dir, what string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("%s left %s behind in %s", what, e.Name(), dir)
	}
}

// A run killed outright -- SIGKILL, no deferred cleanup -- leaves nothing in
// its working directory: it used to leave keploy-logs.txt in the user's
// project for git to commit. Its log is in the temp directory.
func TestKilledRunLeavesNothingInTheWorkingDirectory(t *testing.T) {
	cmd, _, dir, _, _, logPath := heldRun(t)
	requireEmpty(t, dir, "a live run")
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	requireEmpty(t, dir, "a killed run")
	if _, err := os.Lstat(logPath); err != nil {
		t.Errorf("the killed run's log is not where it was created: %v", err)
	}
}

// The run says where its log is, beside its version line, and that file is
// its log while it runs; a normal end removes it, and the run leaves nothing
// in its working directory.
func TestRunNamesItsLogAndRemovesItAtTheEnd(t *testing.T) {
	cmd, out, dir, tmp, fifo, logPath := heldRun(t)
	go func() {
		// The reader the debug file's open waits for; it reads to the end.
		if r, err := os.Open(fifo); err == nil {
			_, _ = io.Copy(io.Discard, r)
			_ = r.Close()
		}
	}()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("keploy --help: %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "logs: "+logPath+"\n") {
		t.Errorf("the run did not say its log is %s beside its version:\n%s", logPath, out)
	}
	if _, err := os.Lstat(logPath); !os.IsNotExist(err) {
		t.Errorf("the run's log outlived its normal end (lstat: %v)", err)
	}
	requireEmpty(t, dir, "a run")
	requireEmpty(t, tmp, "a run")
}

// release lets a held run (heldRun) go on: it opens the named pipe its
// debug file waits on and reads it to the end.
func release(fifo string) {
	go func() {
		if r, err := os.Open(fifo); err == nil {
			_, _ = io.Copy(io.Discard, r)
			_ = r.Close()
		}
	}()
}

// One native run is one file. The agent the CLI starts (`keploy agent
// --client-log-file=<the CLI's log>`) appends its lines to the CLI's log,
// creates none of its own, and leaves the CLI's in place at its end; the CLI
// goes on appending after the agent's lines, not over them, and removes the
// file at its own end.
func TestNativeAgentAppendsToItsCLIsLog(t *testing.T) {
	// A GOMEMLIMIT_MB that is no number: the CLI logs that once the hold lets
	// it go, after the agent's line.
	cli, cliOut, dir, tmp, fifo, logPath := heldRun(t, "GOMEMLIMIT_MB=not-a-number", "SUDO_UID=")
	held, err := os.Open(logPath) // the file, to read once the CLI has removed its name
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	// The agent logs a warning: its KEPLOY_DEBUG_FILE cannot be opened.
	agentTmp := t.TempDir()
	agent := exec.Command(os.Args[0])
	agent.Args = []string{"keploy", "agent", "--" + log.ClientLogFileFlag + "=" + logPath, "--help"}
	agent.Env = append(os.Environ(), subprocessEnv+"=1", "TMPDIR="+agentTmp, "SUDO_UID=",
		"KEPLOY_DEBUG_FILE="+filepath.Join(t.TempDir(), "missing", "debug.log"))
	agent.Dir = dir
	agentOut, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("keploy agent --help: %v\n%s", err, agentOut)
	}
	if strings.Contains(string(agentOut), "a log of its own") {
		t.Errorf("the agent did not take its CLI's log:\n%s", agentOut)
	}
	requireEmpty(t, agentTmp, "an agent given its CLI's log")
	if _, err := os.Lstat(logPath); err != nil {
		t.Fatalf("the agent's end removed its CLI's log: %v", err)
	}

	release(fifo)
	if err := cli.Wait(); err != nil {
		t.Fatalf("keploy --help: %v\n%s", err, cliOut)
	}
	b, err := io.ReadAll(held)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	agentAt := strings.Index(got, "KEPLOY_DEBUG_FILE set but could not be opened")
	cliAt := strings.Index(got, "invalid GOMEMLIMIT_MB value")
	if agentAt < 0 || cliAt < agentAt {
		t.Errorf("the run's one log does not hold the agent's line and then the CLI's (at %d, %d); it holds:\n%s", agentAt, cliAt, got)
	}
	if _, err := os.Lstat(logPath); !os.IsNotExist(err) {
		t.Errorf("the CLI's end left its log (lstat: %v)", err)
	}
	requireEmpty(t, tmp, "a run")
	requireEmpty(t, dir, "a run")
}

// A run whose stdout is a document -- --json, `report --format json|junit`,
// or report.format json in keploy.yml -- creates no log file at all, not one
// it removes once its flags are checked: held right after its logger is
// built, before cobra parses anything, it has none, so a kill there leaves
// nothing in the temp directory. A text run held at the same point has its
// log, which shows the hold is past the point the log is created.
func TestMachineOutputRunCreatesNoLogEvenBeforeItsFlagsAreChecked(t *testing.T) {
	if code := runCLI(t, "--help"); code != 0 {
		t.Skipf("keploy --help exited %d; CLI startup is unusable in this environment", code)
	}
	for _, c := range []struct {
		name      string
		args      []string
		keployYml string
		keeps     bool
	}{
		{name: "report --format json", args: []string{"report", "--format", "json"}},
		{name: "report --format=junit", args: []string{"report", "--format=junit"}},
		{name: "--json", args: []string{"--json", "report"}},
		{name: "report.format json in keploy.yml", args: []string{"report"}, keployYml: "report:\n  format: \"json\"\n"},
		{name: "report --format text", args: []string{"report", "--format", "text"}, keployYml: "report:\n  format: \"json\"\n", keeps: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, tmp := t.TempDir(), t.TempDir()
			if c.keployYml != "" {
				if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(c.keployYml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fifo := filepath.Join(t.TempDir(), "hold.fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("cannot make a named pipe here: %v", err)
			}
			cmd := exec.Command(os.Args[0])
			cmd.Args = append([]string{"keploy"}, c.args...)
			cmd.Env = append(os.Environ(), subprocessEnv+"=1", "TMPDIR="+tmp, holdAfterLoggerEnv+"="+fifo)
			cmd.Dir = dir
			out := &bytes.Buffer{}
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var waitErr error
			exited := make(chan struct{})
			go func() { waitErr = cmd.Wait(); close(exited) }()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-exited
			})

			// The open returns once the run is at its hold.
			opened := make(chan *os.File, 1)
			go func() {
				w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
				if err != nil {
					w = nil
				}
				opened <- w
			}()
			var w *os.File
			select {
			case w = <-opened:
			case <-exited:
				// Lets the open above return, so its goroutine ends.
				if r, e := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0); e == nil {
					_ = r.Close()
				}
				t.Fatalf("keploy %s ended (%v) before its hold:\n%s", strings.Join(c.args, " "), waitErr, out)
			case <-time.After(60 * time.Second):
				t.Fatalf("keploy %s never reached its hold", strings.Join(c.args, " "))
			}
			if w == nil {
				t.Fatal("cannot open the hold's pipe")
			}
			held, _ := filepath.Glob(filepath.Join(tmp, log.LogFilePattern))
			if c.keeps && len(held) != 1 {
				t.Errorf("a text run held after its logger has %d logs %v, want its one", len(held), held)
			}
			if !c.keeps && len(held) != 0 {
				t.Errorf("a run with machine-readable output, held before its flags are checked, has created %v; want no log file", held)
			}
			_ = w.Close()
			select {
			case <-exited:
			case <-time.After(120 * time.Second):
				t.Fatalf("keploy %s did not end once let go:\n%s", strings.Join(c.args, " "), out)
			}
			requireEmpty(t, tmp, "keploy "+strings.Join(c.args, " "))
		})
	}
}
