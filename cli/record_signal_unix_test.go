//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// recordSignalEnv runs TestRecordSignalHelper as the `keploy record` process
// the signal is sent to, and says when it lands. A real signal is
// process-wide, so it goes to a process of its own.
const recordSignalEnv = "KEPLOY_RECORD_SIGNAL_HELPER"

// A signal that lands once the application has ended the recording -- CI's
// stop step a moment behind an application that crashed -- lands on a run that
// is over: Keploy is only tearing down, and the application's exit code
// stands. It was erased: an application that exited 7 and a SIGINT a few
// hundredths of a second later exited 0, and the CI job went green. A signal
// that ends the recording is still a stop, and exits 0 whatever the
// application it killed exited with.
func TestRecordKeepsTheApplicationsCodeThroughALateSignal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lands string
		want  string
	}{
		{"the signal lands after the application ended the recording", "teardown", "exit=7 appExitCode=7"},
		{"the signal ends the recording", "live", "exit=0 appExitCode=none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecordSignalHelper$", "-test.v")
			// No drain window: the handler would hold the signal for it first.
			cmd.Env = append(os.Environ(), recordSignalEnv+"="+tc.lands, "KEPLOY_SIDECAR_DRAIN_SECONDS=")
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

// TestRecordSignalHelper runs `keploy record` under utils.NewCtx's real
// handler, and sends the process a SIGINT while it records or once the
// application has ended the recording. It skips anywhere else.
func TestRecordSignalHelper(t *testing.T) {
	lands := os.Getenv(recordSignalEnv)
	if lands == "" {
		t.Skip("the signalled process of TestRecordKeepsTheApplicationsCodeThroughALateSignal")
	}
	ctx := utils.NewCtx()
	// The handler runs the pre-cancel hooks once it has taken the signal, and
	// before it cancels.
	handled := make(chan struct{})
	var once sync.Once
	utils.RegisterPreCancelHook(func() { once.Do(func() { close(handled) }) })
	sendSIGINT := func() {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatalf("sending SIGINT: %v", err)
		}
		select {
		case <-handled:
		case <-time.After(10 * time.Second):
			t.Fatal("utils.NewCtx's handler never took the signal")
		}
	}
	core, logs := observer.New(zapcore.ErrorLevel)
	logger := zap.New(core)
	code := runKeployLogging(t, ctx, logger, recordThat(func(context.Context) error {
		if lands == "live" {
			sendSIGINT()
			// The application the same signal killed.
			return appExit(models.ErrUnExpected, 130)
		}
		// record.Start once the application exited 7: the stop defer
		// stops Keploy, and the signal lands while it tears down.
		if err := utils.Stop(logger, "user application terminated unexpectedly hence stopping keploy"); err != nil {
			t.Fatalf("stopping keploy: %v", err)
		}
		sendSIGINT()
		return startStoppedBy{models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 7"), ExitCode: 7}}
	}), "record")
	appCode := "none"
	for _, l := range logs.FilterMessage("failed to record").All() {
		if v, ok := l.ContextMap()["appExitCode"]; ok {
			appCode = fmt.Sprint(v)
		}
	}
	t.Logf("exit=%d appExitCode=%s", code, appCode)
}
