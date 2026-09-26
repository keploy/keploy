//go:build linux || darwin

package utils

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestExecuteCommand_RunsPipedStdinShapeWithoutShell is the regression guard for
// the shell dependency on the command-execution path.
//
// A compose document keploy generates ITSELF no longer comes through here at
// all — that path drives the compose library in-process (see
// App.runComposeInProcess), which is what removed the `docker` binary
// requirement. What still comes through here is the USER's own command, and on
// a distroless image with no /bin/sh the old `sh -c <cmd>` wrapper died with
// `exec: "sh": executable file not found`.
//
// This test drives the *real* ExecuteCommand in an environment where `sh` is not
// resolvable (PATH points at a directory that contains only the target binary),
// with a multi-token command plus piped stdin. It asserts the command runs
// directly (no shell) and actually receives the stdin. If anyone reintroduces a
// hard `sh -c` on this path, `sh` won't be found and this test fails.
//
// `tee <file>` stands in for the app command: it is a real, shell-free binary
// that echoes its stdin to a file, letting us prove the piped compose document
// reached the launched process.
func TestExecuteCommand_RunsPipedStdinShapeWithoutShell(t *testing.T) {
	teePath, err := exec.LookPath("tee")
	if err != nil {
		t.Skipf("tee not available: %v", err)
	}

	// A bin dir holding ONLY the target binary — deliberately no sh/bash/dash,
	// so keploy's exec.LookPath("sh") fails exactly as it would on a distroless
	// runtime image.
	binDir := t.TempDir()
	if err := os.Symlink(teePath, filepath.Join(binDir, "tee")); err != nil {
		t.Fatalf("link tee into shell-less bin dir: %v", err)
	}
	t.Setenv("PATH", binDir)

	// Sanity: the environment really has no shell.
	if p, err := exec.LookPath("sh"); err == nil {
		t.Fatalf("test setup is wrong: sh is still resolvable at %s", p)
	}

	capturedPath := filepath.Join(t.TempDir(), "captured-compose.yaml")
	composeDoc := []byte("services:\n  app:\n    image: demo:latest\n")

	// Multi-token argv + a document on stdin, the shape a user command that
	// reads from stdin takes.
	cmdStr := "tee " + capturedPath
	noopCancel := func(_ *exec.Cmd) func() error { return func() error { return nil } }

	cmdErr := ExecuteCommand(context.Background(), zap.NewNop(), cmdStr, Empty, noopCancel, time.Second, composeDoc, nil)
	if cmdErr.Err != nil {
		t.Fatalf("ExecuteCommand failed in a shell-less env (a hard `sh -c` regression?): type=%s err=%v",
			cmdErr.Type, cmdErr.Err)
	}

	got, err := os.ReadFile(capturedPath)
	if err != nil {
		t.Fatalf("command did not run (nothing captured): %v", err)
	}
	if !bytes.Equal(got, composeDoc) {
		t.Fatalf("stdin was not piped to the launched command: got %q want %q", got, composeDoc)
	}
}

// TestExecuteCommand_GivesExtraEnvToThatCommandAlone: extraEnv is how the
// docker compose command that starts the agent gets the control-plane token
// its compose file names without a value. It must reach that command — and only
// that command: exporting it to keploy's own environment would hand it to every
// process keploy starts afterwards.
func TestExecuteCommand_GivesExtraEnvToThatCommandAlone(t *testing.T) {
	const name = "KEPLOY_TEST_EXTRA_ENV"
	t.Setenv(name, "") // restored afterwards
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	noopCancel := func(_ *exec.Cmd) func() error { return func() error { return nil } }

	cmdErr := ExecuteCommand(context.Background(), zap.NewNop(), `printf %s "$`+name+`" > `+out, Empty, noopCancel, time.Second, nil,
		[]string{name + "=only-for-this-command"})
	if cmdErr.Err != nil {
		t.Fatalf("ExecuteCommand: %v", cmdErr.Err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "only-for-this-command" {
		t.Fatalf("the command saw %q, not the variable it was given", got)
	}
	if _, exported := os.LookupEnv(name); exported {
		t.Fatalf("%s was exported to keploy's own environment", name)
	}
}
