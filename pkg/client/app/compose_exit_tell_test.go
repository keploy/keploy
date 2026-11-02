package app

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
