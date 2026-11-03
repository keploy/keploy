package replay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// An application --keep-app-alive keeps across the run's sets that exits
// before it accepts a connection fails the first set as its exit does,
// saying so, and the run stops naming that exit: never "replay completed
// successfully", and never as a stop by the user.
func TestAKeptAppThatExitsBeforeItAcceptsStopsTheRunNamingItsExit(t *testing.T) {
	refusing := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
	h := newPartialRunHarness(t, 2, 0)
	for _, tc := range h.cases {
		tc.HTTPReq.URL = "http://" + refusing + "/x"
	}
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.KeepAppAlive = true
	h.replayer.config.Test.MaxFlakyChecks = 1
	// The application exits once the first set waits for it to accept a
	// connection.
	var exit sync.Once
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "waiting for the application to accept a connection on "+refusing) {
			exit.Do(func() { close(h.instr.appStopped) })
		}
		return nil
	}))
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	err := h.replayer.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "exited before it accepted a connection") {
		t.Errorf("Start: %v, want an error saying the application exited before it accepted a connection", err)
	}
	if logs.FilterMessageSnippet("the application exited before it accepted a connection").Len() == 0 {
		t.Error("no line says the application exited before it accepted a connection")
	}
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	if !strings.Contains(reason, "exited before it accepted a connection") {
		t.Errorf("the run stops with the reason %q, want one naming the application's exit", reason)
	}
	if n := len(h.report.results); n != 0 {
		t.Errorf("%d tests were sent, want none", n)
	}
	if utils.ErrCode != 1 {
		t.Errorf("exit code %d, want 1", utils.ErrCode)
	}
}

type keptAppKey struct{}

// keptAppExit reads an application's exit only from a run's context that its
// exit ended; a context still running, or one its user stopped, has none.
func TestKeptAppExitIsReadOnlyFromARunItsExitEnded(t *testing.T) {
	exit := models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3, Err: errors.New("exit status 3")}

	running, stop := context.WithCancel(context.Background())
	defer stop()
	if got := keptAppExit(running); got != (models.AppError{}) {
		t.Errorf("a running context: %+v, want none", got)
	}
	stopped, stopIt := context.WithCancel(context.Background())
	stopIt()
	if got := keptAppExit(stopped); got != (models.AppError{}) {
		t.Errorf("a context its user stopped: %+v, want none", got)
	}
	ended, endIt := context.WithCancelCause(context.Background())
	endIt(exit)
	// The set reads it through the contexts the run derives from its own.
	child, cancelChild := context.WithCancel(context.WithValue(ended, keptAppKey{}, 1))
	defer cancelChild()
	if got := keptAppExit(child); got != exit {
		t.Errorf("a run its application's exit ended: %+v, want %+v", got, exit)
	}
	other, endOther := context.WithCancelCause(context.Background())
	endOther(errors.New("an agent call failed"))
	if got := keptAppExit(other); got != (models.AppError{}) {
		t.Errorf("a run another error ended: %+v, want none", got)
	}
}

// A run its context ended as a set ran stops with a reason that names what
// ended it: the set's own error when it says more than the cancellation, else
// the cause; never "replay completed successfully".
func TestCancelledRunReasonNamesWhatEndedTheRun(t *testing.T) {
	exit := models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3, Err: errors.New("exit status 3")}
	setErr := fmt.Errorf("the application exited before it accepted a connection: %w", exit)
	for _, c := range []struct {
		name          string
		setErr, cause error
		want          string
	}{
		{"the set's error", setErr, exit, "failed to run test set: " + setErr.Error()},
		{"a cancelled set, the cause", context.Canceled, exit, "the run stopped: " + exit.Error()},
		{"a wrapped cancellation, the cause", fmt.Errorf("send: %w", context.Canceled), exit, "the run stopped: " + exit.Error()},
		{"no set error, the cause", nil, exit, "the run stopped: " + exit.Error()},
		{"nothing said", context.Canceled, context.Canceled, "the run was cancelled before its test sets ended"},
		{"no cause", context.Canceled, nil, "the run was cancelled before its test sets ended"},
	} {
		if got := cancelledRunReason(c.setErr, c.cause); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
