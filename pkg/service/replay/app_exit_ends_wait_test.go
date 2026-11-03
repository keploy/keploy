package replay

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// marksHooks records whether each test request's context says the
// application's exit ends it (pkg.AppExitEnds).
type marksHooks struct {
	prHooks
	mu    sync.Mutex
	marks []bool
	rooms []bool
}

func (h *marksHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.mu.Lock()
	h.marks = append(h.marks, pkg.AppExitEnds(ctx))
	// The room for the started application's pid, which a wait names the
	// application's ports from, is on the request's context too.
	utils.NoteAppPID(ctx, 7)
	h.rooms = append(h.rooms, utils.AppPID(ctx) == 7)
	h.mu.Unlock()
	return h.prHooks.SimulateRequest(ctx, tc, set)
}

// The test requests of an application keploy started go out on a context its
// exit ends, so that a refused one waits for its address to accept; those of
// an application keploy did not start (no command) do not, so that a refused
// one keeps the bounded re-send: nothing would end its wait.
func TestTestRequestsWaitOnARefusalOnlyForAnAppKeployStarted(t *testing.T) {
	for _, started := range []bool{true, false} {
		h := newPartialRunHarness(t, 2, 0)
		h.replayer.instrument = started
		hooks := &marksHooks{}
		h.replayer.hookImpl = hooks
		h.run(t)
		if len(hooks.marks) != 2 {
			t.Fatalf("started=%v: %d test requests sent, want 2", started, len(hooks.marks))
		}
		for i, marked := range hooks.marks {
			if marked != started {
				t.Errorf("started=%v: request %d's context says the app's exit ends it: %v", started, i+1, marked)
			}
		}
		for i, room := range hooks.rooms {
			if room != started {
				t.Errorf("started=%v: request %d's context has a room for the application's pid: %v", started, i+1, room)
			}
		}
	}
}

// An application keploy started that exits before it ever accepts a
// connection ends the set's wait for it, and the set fails as the exit does
// (the application halted), saying so: not as a stop by the user.
func TestAnAppThatExitsBeforeItAcceptsFailsItsSetSayingSo(t *testing.T) {
	refusing := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
	h := newPartialRunHarness(t, 2, 0)
	for _, tc := range h.cases {
		tc.HTTPReq.URL = "http://" + refusing + "/x"
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		close(h.instr.appStopped)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
	if status != models.TestSetStatusAppHalted {
		t.Errorf("status %q, want %q: the application exited before it accepted a connection", status, models.TestSetStatusAppHalted)
	}
	if err == nil || !strings.Contains(err.Error(), "exited before it accepted a connection") {
		t.Errorf("error %v, want one saying the application exited before it accepted a connection", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("error %v reads as the user's stop", err)
	}
	if n := len(h.report.results); n != 0 {
		t.Errorf("%d tests were sent, want none", n)
	}
}

// A wait for the application that ends with no exit of its own is the
// user's stop; one that ends with its exit fails the set as that exit does.
func TestNeverReadyStatusIsTheExitsOrTheUsersStop(t *testing.T) {
	for _, c := range []struct {
		err    models.AppError
		status models.TestSetStatus
		exited bool
	}{
		{models.AppError{}, models.TestSetStatusUserAbort, false},
		{models.AppError{AppErrorType: models.ErrCtxCanceled}, models.TestSetStatusUserAbort, false},
		{models.AppError{AppErrorType: models.ErrAppStopped, ExitCode: 0}, models.TestSetStatusAppHalted, true},
		{models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 1}, models.TestSetStatusFaultUserApp, true},
	} {
		if status, exited := neverReadyStatus(c.err); status != c.status || exited != c.exited {
			t.Errorf("%+v: (%q, %v), want (%q, %v)", c.err, status, exited, c.status, c.exited)
		}
	}
}
