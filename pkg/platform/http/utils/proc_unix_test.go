//go:build linux || darwin

package utils

import (
	"os/exec"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// What a command started under a PTY prints reaches its tee, all of it by the
// time Wait returns, as well as keploy's console: a Docker agent's start reads
// from it why docker run failed (a port the engine could not publish).
func TestAPTYCommandsOutputReachesItsTee(t *testing.T) {
	var tee teeBuffer
	cmd := exec.Command("sh", "-c", "printf 'Bind for 127.0.0.1:61234 failed: port is already allocated\\n'")
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
}
