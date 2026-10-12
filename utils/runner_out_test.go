//go:build linux || darwin

package utils

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Copying the runner's stdout to RunnerOut must not tie the runner's exit to
// every process that inherited that stdout: a runner that leaves a background
// child holding it (a server started with `&`, an app its TestMain never reaped)
// still finishes as soon as it exits, and RunnerOut still gets what it printed.
func TestRunnerOutDoesNotWaitForBackgroundChildren(t *testing.T) {
	out := &lockedBuf{}
	RunnerOut = out
	t.Cleanup(func() { RunnerOut = nil })

	noopCancel := func(_ *exec.Cmd) func() error { return func() error { return nil } }
	start := time.Now()
	cmdErr := ExecuteCommand(context.Background(), zap.NewNop(),
		`sleep 5 & echo "--- FAIL: TestX (0.00s)"`, Empty, noopCancel, 2*time.Second, nil, nil)
	took := time.Since(start)

	if cmdErr.Err != nil {
		t.Fatalf("the runner exited 0, want no error, got %v", cmdErr.Err)
	}
	// The bug waits out the 2s WaitDelay; the fix waits at most the 500ms
	// drain while the background child holds the pipe. 1.5s tells them apart
	// with room for a busy machine.
	if took > 1500*time.Millisecond {
		t.Fatalf("the runner exited at once but ExecuteCommand took %s", took)
	}
	if !strings.Contains(out.String(), "--- FAIL: TestX") {
		t.Fatalf("RunnerOut did not get the runner's output: %q", out.String())
	}
}
