package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// With the switch off compose's abort stops the stack on the app's exit, and
// its up returns only once every service has stopped, each within its own
// grace: keploy took the app for running until then, began re-sends and
// tests against it, and lost the exit to the set's own end. The app's die is
// told to the run's caller at once, with its exit code; the die of keploy's
// own stop (the run's ctx ended) is not.
func TestAComposeAppsExitIsToldAtItsDieWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "0")
	d := newComposeDocker(true, true)
	a := composeApp(d)
	told := make(chan models.AppError, 2)
	ctx, cancel := context.WithCancel(models.WithAppExit(context.Background(), func(e models.AppError) { told <- e }))
	defer cancel()
	stop := a.watchComposeExit(ctx)
	defer stop()
	stream := <-d.streams

	d.mu.Lock()
	d.exitCode = 3
	d.stop()
	d.mu.Unlock()
	stream <- die("3")
	select {
	case e := <-told:
		if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 3 {
			t.Fatalf("told %+v; want the app's unexpected exit, code 3", e)
		}
	case <-time.After(time.Minute):
		t.Fatal("the app's exit was not told at its die")
	}
}

func TestTheDieOfKeploysOwnStopIsNotToldWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "0")
	d := newComposeDocker(true, true)
	a := composeApp(d)
	told := make(chan models.AppError, 2)
	ctx, cancel := context.WithCancel(models.WithAppExit(context.Background(), func(e models.AppError) { told <- e }))
	stop := a.watchComposeExit(ctx)
	stream := <-d.streams
	// keploy's stop: the run's ctx ends, then the app goes.
	cancel()
	d.mu.Lock()
	d.stop()
	d.mu.Unlock()
	select {
	case stream <- die("130"):
	case <-time.After(time.Second):
	}
	stop()
	select {
	case e := <-told:
		t.Fatalf("keploy's own stop was told as the app's exit: %+v", e)
	default:
	}
}

// The run, with the switch off, watches the app's container for its caller
// when the caller listens (models.WithAppExit): the app's die is told while
// compose's up still waits out the rest of the stack, and the run returns
// only once the up does.
func TestAComposeRunWithTheSwitchOffTellsTheAppsDieBeforeItsUpReturns(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	argvLog := stubDockerCLI(t, "", 0)
	upEnds := filepath.Join(t.TempDir(), "up-returns")
	t.Setenv("KEPLOY_STUB_UP_WAIT_FILE", upEnds)
	rec := &inspectRecorder{events: make(chan events.Message, 1)}
	a := &App{
		logger:          zap.NewNop(),
		docker:          rec,
		kind:            utils.DockerCompose,
		cmd:             "docker compose -f /tmp/keploy-compose.yaml up app",
		composeFile:     "/tmp/keploy-compose.yaml",
		keployContainer: "keploy-v3",
		container:       "app",
		composeServices: []string{"app", "keploy-agent"},
	}
	told := make(chan models.AppError, 2)
	ctx, cancel := context.WithCancel(models.WithAppExit(context.Background(), func(e models.AppError) { told <- e }))
	defer cancel()
	ran := make(chan models.AppError, 1)
	go func() { ran <- a.run(ctx) }()
	for !strings.Contains(recordedLog(t, argvLog), "ARG up") {
		time.Sleep(10 * time.Millisecond)
	}

	rec.events <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie,
		Actor: events.Actor{ID: "app", Attributes: map[string]string{"exitCode": "3"}}}
	select {
	case e := <-told:
		if e.AppErrorType != models.ErrUnExpected || e.ExitCode != 3 {
			t.Fatalf("told %+v; want the app's unexpected exit, code 3", e)
		}
	case e := <-ran:
		t.Fatalf("the run returned (%+v) before the up did, and told nothing", e)
	case <-time.After(time.Minute):
		t.Fatal("the app's die was not told while the up still ran")
	}
	select {
	case e := <-ran:
		t.Fatalf("the run returned (%+v) while the up still ran", e)
	default:
	}
	if err := os.WriteFile(upEnds, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-ran
}

// The halt is told only when it is kept and its run's caller listens: a
// halt under the switch is ended for runs that do not listen (keploy
// record's), and a listener is told nothing until a halt is kept.
func TestTheComposeHaltIsToldOnlyWhenKeptAndListenedFor(t *testing.T) {
	a := composeApp(newComposeDocker(true, true))
	a.composeApp.halted.Store(&composeHalt{exitCode: 3})
	a.tellComposeHalt(context.Background())

	var told []models.AppError
	ctx := models.WithAppExit(context.Background(), func(e models.AppError) { told = append(told, e) })
	a.composeApp.halted.Store(nil)
	a.tellComposeHalt(ctx)
	if len(told) != 0 {
		t.Fatalf("told %+v with no halt kept", told)
	}
	a.composeApp.halted.Store(&composeHalt{exitCode: 3})
	a.tellComposeHalt(ctx)
	if len(told) != 1 || told[0].AppErrorType != models.ErrUnExpected || told[0].ExitCode != 3 {
		t.Fatalf("told %+v; want the kept halt once, code 3", told)
	}
}
