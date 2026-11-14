//go:build linux || darwin

package utils

import (
	"bytes"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// teeBuffer is a buffer a PTY's output copy writes to while a row reads it.
type teeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *teeBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}

func (t *teeBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}

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
