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
// KEPLOY_DEBUG_FILE is a named pipe, whose open waits for a reader. It returns
// the run, its stdout, the directories, the pipe and the log the run created.
func heldRun(t *testing.T) (cmd *exec.Cmd, out *bytes.Buffer, dir, tmp, fifo, logPath string) {
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
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", "TMPDIR="+tmp, "KEPLOY_DEBUG_FILE="+fifo)
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
