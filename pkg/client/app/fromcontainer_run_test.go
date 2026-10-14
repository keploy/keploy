package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// fakeDocker answers only the calls runFromContainer is allowed to make. The
// embedded interface is nil, so anything else panics loudly rather than
// returning a zero value that quietly changes the meaning of the test.
type fakeDocker struct {
	docker.Client

	mu            sync.Mutex
	waitCondition container.WaitCondition
	started       bool

	exitCode  int64
	waitErr   error
	logsErr   error
	logBody   string
	stopped   bool
	removed   bool
	removeErr error
	// holdLogs blocks the log stream until released, standing in for a driver
	// that accepts the request and then never produces anything.
	holdLogs chan struct{}
	// blockExit keeps the container "running" so a cancellation test is not
	// racing an exit that has already been delivered.
	blockExit chan struct{}
}

func (f *fakeDocker) ContainerWait(ctx context.Context, _ string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	f.mu.Lock()
	f.waitCondition = cond
	f.mu.Unlock()

	res := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		if f.waitErr != nil {
			errCh <- f.waitErr
			return
		}
		// The daemon answers when the container exits. A container that has not
		// been started has not exited — modelling that is the whole point,
		// because WaitConditionNotRunning is ALREADY MET before a start and the
		// real daemon answers instantly with exit 0.
		for {
			f.mu.Lock()
			started := f.started
			f.mu.Unlock()
			if started {
				break
			}
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
		if f.blockExit != nil {
			select {
			case <-f.blockExit:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
		// The real client binds the wait request to ctx, so a cancelled session
		// surfaces on the error channel rather than as a container exit.
		select {
		case <-ctx.Done():
			errCh <- ctx.Err()
		case res <- container.WaitResponse{StatusCode: f.exitCode}:
		}
	}()
	return res, errCh
}

func (f *fakeDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	f.mu.Lock()
	f.started = true
	f.mu.Unlock()
	return nil
}

func (f *fakeDocker) ContainerLogs(ctx context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	if f.holdLogs != nil {
		select {
		case <-f.holdLogs:
		case <-ctx.Done():
		}
		// Released means the session is tearing the stream down, not that it is
		// about to start producing output.
		return nil, errors.New("log stream torn down")
	}
	return io.NopCloser(strings.NewReader(f.logBody)), nil
}

func (f *fakeDocker) ContainerStop(context.Context, string, container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *fakeDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Stopping first is what gives the app its own StopSignal and StopTimeout;
	// a bare force-remove is an immediate SIGKILL that ignores both.
	if !f.stopped {
		f.removeErr = errors.New("removed without stopping first")
	}
	f.removed = true
	return nil
}

func (f *fakeDocker) ContainerLogsCalledWith() container.WaitCondition {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitCondition
}

func newRunnableApp(d *fakeDocker) *App {
	return &App{
		logger:        zap.NewNop(),
		docker:        d,
		kind:          utils.FromContainer,
		replacementID: "replacement-id",
		container:     "app-keploy",
	}
}

// THE regression guard for this file. A created-but-unstarted container is
// already "not running", so WaitConditionNotRunning is satisfied before
// ContainerStart is even issued: the daemon answers immediately with exit code
// 0 and EVERY run reports success, including one whose test suite failed.
//
// Reproduced against a real daemon before the fix: a container exiting 7 made
// keploy exit 0 and log "test command finished successfully", without waiting
// for the app at all.
func TestRunFromContainerWaitsForTheNextExitNotForNotRunning(t *testing.T) {
	d := &fakeDocker{exitCode: 0}
	if got := newRunnableApp(d).runFromContainer(context.Background()); got.AppErrorType != models.ErrAppStopped {
		t.Fatalf("clean run reported %q, want %q", got.AppErrorType, models.ErrAppStopped)
	}
	if got := d.ContainerLogsCalledWith(); got != container.WaitConditionNextExit {
		t.Fatalf("subscribed with %q, want %q — anything else is already satisfied before the start and answers instantly with exit 0",
			got, container.WaitConditionNextExit)
	}
}

// The exit code is the contract every CI job depends on: a wrapped suite that
// fails has to fail keploy.
func TestRunFromContainerPropagatesTheExitCode(t *testing.T) {
	got := newRunnableApp(&fakeDocker{exitCode: 7}).runFromContainer(context.Background())
	if got.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", got.ExitCode)
	}
	if got.AppErrorType != models.ErrUnExpected {
		t.Errorf("AppErrorType = %q, want %q so propagateExit mirrors it", got.AppErrorType, models.ErrUnExpected)
	}
}

// The log stream is a convenience for the user; the app's lifetime must not
// depend on it. A log driver the daemon refuses to read — none, syslog,
// fluentd, gelf, awslogs, splunk — makes ContainerLogs fail outright, and
// gating on it there would end the session milliseconds after the app started
// and report a clean exit 0.
func TestRunFromContainerSurvivesAnUnreadableLogDriver(t *testing.T) {
	d := &fakeDocker{exitCode: 3, logsErr: errors.New("configured logging driver does not support reading")}
	got := newRunnableApp(d).runFromContainer(context.Background())
	if got.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3: the run ended on the log stream rather than on the app", got.ExitCode)
	}
}

// And the mirror: a log stream that never produces anything must not hold
// teardown open either.
func TestRunFromContainerDoesNotBlockOnAStalledLogStream(t *testing.T) {
	d := &fakeDocker{exitCode: 0, holdLogs: make(chan struct{})}
	defer close(d.holdLogs)

	done := make(chan models.AppError, 1)
	go func() { done <- newRunnableApp(d).runFromContainer(context.Background()) }()

	select {
	case got := <-done:
		if got.AppErrorType != models.ErrAppStopped {
			t.Fatalf("got %q, want a clean stop", got.AppErrorType)
		}
	case <-time.After(logFlushGrace + 5*time.Second):
		t.Fatal("runFromContainer never returned: the app's lifetime is gated on its log stream")
	}
}

// A cancelled session is a user interrupt, not an app failure.
func TestRunFromContainerReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &fakeDocker{exitCode: 0, holdLogs: make(chan struct{}), blockExit: make(chan struct{})}
	defer close(d.holdLogs)

	done := make(chan models.AppError, 1)
	go func() { done <- newRunnableApp(d).runFromContainer(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if got.AppErrorType != models.ErrCtxCanceled {
			t.Fatalf("got %q, want %q", got.AppErrorType, models.ErrCtxCanceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runFromContainer did not return after cancellation")
	}
}

// A TTY container's log stream is RAW; every other container's is multiplexed
// with an 8-byte frame header. StdCopy only understands the second kind - it
// has no TTY detection and reads the first byte as a stream id, so on a raw
// stream it fails on the app's first line of output, and for a byte in
// 0x00-0x03 it sizes an allocation from app-controlled bytes.
func TestRunFromContainerReadsATTYStreamRaw(t *testing.T) {
	// Plain text with no frame header - what a TTY container actually emits.
	d := &fakeDocker{exitCode: 0, logBody: "Listening on :8080\n"}
	app := newRunnableApp(d)
	app.sourceTTY = true

	var out bytes.Buffer
	restore := captureStdout(t, &out)
	got := app.runFromContainer(context.Background())
	restore()

	if got.AppErrorType != models.ErrAppStopped {
		t.Fatalf("got %q, want a clean stop", got.AppErrorType)
	}
	if !strings.Contains(out.String(), "Listening on :8080") {
		t.Errorf("the app's output never reached the terminal; got %q", out.String())
	}
}

// And the mirror: a multiplexed stream must still be demuxed, or the frame
// headers are printed as garbage among the app's own output.
func TestRunFromContainerDemuxesANonTTYStream(t *testing.T) {
	var framed bytes.Buffer
	payload := "hello from stdout\n"
	framed.Write([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(payload))})
	framed.WriteString(payload)

	d := &fakeDocker{exitCode: 0, logBody: framed.String()}
	app := newRunnableApp(d) // sourceTTY stays false

	var out bytes.Buffer
	restore := captureStdout(t, &out)
	app.runFromContainer(context.Background())
	restore()

	if out.String() != payload {
		t.Errorf("got %q, want the demuxed payload %q", out.String(), payload)
	}
}

// The replacement is keploy's disposable copy, and teardown has to end it the
// way its own configuration asks rather than by SIGKILL - replicaConfig goes to
// the trouble of carrying the source's StopSignal and StopTimeout onto it.
func TestRemoveReplacementStopsBeforeRemoving(t *testing.T) {
	d := &fakeDocker{}
	app := newRunnableApp(d)

	app.RemoveReplacement()
	if d.removeErr != nil {
		t.Fatalf("%v", d.removeErr)
	}
	if !d.removed {
		t.Fatal("the replacement was never removed")
	}

	// Idempotent: it is called from App.run's defer AND from the agent client's
	// teardown, and both can run for the same session.
	d.removed = false
	app.RemoveReplacement()
	if d.removed {
		t.Error("removed twice; the second call must be a no-op")
	}
}

// captureStdout redirects os.Stdout for the duration of a test, so the log
// streaming can be asserted on rather than assumed.
func captureStdout(t *testing.T, into *bytes.Buffer) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w

	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(into, r)
	}()
	return func() {
		os.Stdout = saved
		_ = w.Close()
		<-copied
		_ = r.Close()
	}
}
