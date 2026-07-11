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

// TestExecuteCommand_RunsCloudReplayShapeWithoutShell is the regression guard
// for the cloud-replay shell dependency.
//
// The cloud replay launches the user's app the same way every replay does — via
// ExecuteCommand — with the command `docker compose -f - up ...` and the compose
// document piped in on stdin (that is what `-f -` reads). On a distroless cloud
// image with no /bin/sh the old `sh -c <cmd>` wrapper died with
// `exec: "sh": executable file not found`.
//
// This test drives the *real* ExecuteCommand in an environment where `sh` is not
// resolvable (PATH points at a directory that contains only the target binary),
// using the same shape as cloud replay: a multi-token command plus stdin. It
// asserts the command runs directly (no shell) and actually receives the piped
// stdin. If anyone reintroduces a hard `sh -c` on this path, `sh` won't be found
// and this test fails.
//
// `tee <file>` stands in for the app command: it is a real, shell-free binary
// that echoes its stdin to a file, letting us prove the piped compose document
// reached the launched process.
func TestExecuteCommand_RunsCloudReplayShapeWithoutShell(t *testing.T) {
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

	// Same shape as the cloud replay command: multi-token argv + document on
	// stdin (the `-f -` mechanism).
	cmdStr := "tee " + capturedPath
	noopCancel := func(_ *exec.Cmd) func() error { return func() error { return nil } }

	cmdErr := ExecuteCommand(context.Background(), zap.NewNop(), cmdStr, noopCancel, time.Second, composeDoc)
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
