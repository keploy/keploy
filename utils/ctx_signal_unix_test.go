//go:build unix

package utils

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// newCtxSignalEnv runs TestNewCtxSignalHelper as the process the signal is
// sent to, and says when it lands. A real signal is process-wide: sent to this
// test binary, it would also reach the handler of every earlier test that
// called NewCtx.
const newCtxSignalEnv = "KEPLOY_NEWCTX_SIGNAL_HELPER"

// Interrupted says a signal ENDED the run; that is what SetFailureExitCode
// reads it for. A signal that lands once the CLI has cancelled its own root
// context ended nothing: the run's outcome was decided before it, and Keploy
// was only tearing down. Marked, it erased that outcome -- `keploy record` over
// an application that had just exited 7 got CI's stop signal a few hundredths
// of a second later, and exited 0.
func TestNewCtxMarksOnlyASignalThatEndsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lands is when the signal lands: "live", or "teardown" once the CLI
		// has cancelled the root context itself.
		lands string
		want  string
	}{
		{"a signal while the run is live", "live", "interrupted=true"},
		{"a signal once the CLI is tearing down", "teardown", "interrupted=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNewCtxSignalHelper$", "-test.v")
			// No drain window: the handler would hold the signal for it first.
			cmd.Env = append(os.Environ(), newCtxSignalEnv+"="+tc.lands, "KEPLOY_SIDECAR_DRAIN_SECONDS=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the signalled process failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("want %s:\n%s", tc.want, out)
			}
		})
	}
}

// TestNewCtxSignalHelper is the process TestNewCtxMarksOnlyASignalThatEndsTheRun
// signals, through NewCtx's real handler. It skips anywhere else.
func TestNewCtxSignalHelper(t *testing.T) {
	lands := os.Getenv(newCtxSignalEnv)
	if lands == "" {
		t.Skip("the signalled process of TestNewCtxMarksOnlyASignalThatEndsTheRun")
	}
	ctx := NewCtx()
	// The handler runs the pre-cancel hooks once it has taken the signal, and
	// before it cancels.
	handled := make(chan struct{})
	var once sync.Once
	RegisterPreCancelHook(func() { once.Do(func() { close(handled) }) })
	if lands == "teardown" {
		// What utils.Stop does: record.Start's stop defer, once the
		// application has ended the recording.
		ExecCancel()
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("sending SIGINT: %v", err)
	}
	waitFor(t, handled, "NewCtx's handler never took the signal")
	waitFor(t, ctx.Done(), "the root context was never cancelled")
	t.Logf("interrupted=%t", Interrupted())
}

func waitFor(t *testing.T, done <-chan struct{}, never string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal(never)
	}
}
