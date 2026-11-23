//go:build windows

package utils

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// What a command prints on Windows, which has no PTY, reaches its tee, all of
// it by the time Wait returns, as well as keploy's console: a Docker agent's
// start reads from it why docker run failed (a port the engine could not
// publish). With no tee nothing is put between the command and keploy's
// console: it prints to keploy's own stdout and stderr, as before.
func TestAWindowsCommandsOutputReachesItsTee(t *testing.T) {
	var tee teeBuffer
	cmd := exec.Command("cmd", "/c", "echo Bind for 127.0.0.1:61234 failed: port is already allocated")
	h, err := StartCommandWithPTYTee(cmd, zap.NewNop(), &tee)
	if err != nil {
		t.Fatalf("StartCommandWithPTYTee: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := tee.String(); !strings.Contains(got, "127.0.0.1:61234 failed: port is already allocated") {
		t.Fatalf("the tee got %q; want what the command printed", got)
	}

	cmd = exec.Command("cmd", "/c", "exit 0")
	h, err = StartCommandWithPTYTee(cmd, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("StartCommandWithPTYTee with no tee: %v", err)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("Wait with no tee: %v", err)
	}
	if cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr {
		t.Fatalf("with no tee the command printed to %v and %v; want keploy's own stdout and stderr", cmd.Stdout, cmd.Stderr)
	}
}
