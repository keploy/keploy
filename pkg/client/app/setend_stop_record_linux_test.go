package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// runNativeAs is runNative for an app set up in mode: it runs script, stops
// it once it has written started (the run's ctx cancelled, as keploy record's
// stop cancels it), and returns how the run ended.
func runNativeAs(t *testing.T, mode models.Mode, script string) models.AppError {
	t.Helper()
	dir := t.TempDir()
	a := NewApp(zap.NewNop(), "cd "+dir+" && "+script, nil, models.SetupOptions{CommandType: string(utils.Native), Mode: mode})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan models.AppError, 1)
	go func() { done <- a.run(ctx) }()
	for {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		select {
		case appErr := <-done:
			t.Fatalf("the app ended before it started: %+v", appErr)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case appErr := <-done:
		return appErr
	case <-time.After(2 * time.Minute):
		t.Fatal("the stop did not end")
	}
	return models.AppError{}
}

// keploy record's stop of the app, under the proven switch, says when it had
// to kill it, with the grace it gave it: what the app would have done as it
// stopped is then not in the recording (P101-b-k). An app that exits on the
// interrupt is no kill; with the switch off, or in a replay's teardown, the
// stop says nothing.
func TestTheRecordingsStopSaysWhenItKilledTheApp(t *testing.T) {
	ignores := `trap "" INT; touch started; exec sleep 600`
	t.Setenv(provenswitch.Env, "1")
	// The stop under the switch waits on the exit event, and kills on the
	// stall rule: a short one here.
	budget := utils.StopStallBudget
	utils.StopStallBudget = 300 * time.Millisecond
	t.Cleanup(func() { utils.StopStallBudget = budget })
	if appErr := runNativeAs(t, models.MODE_RECORD, ignores); appErr.AppErrorType != models.ErrCtxCanceled ||
		!strings.Contains(appErr.StopKilled, "keploy stopped the application at the recording's end and a process of it made no progress on its way out for 300ms of keploy's runnable time") {
		t.Fatalf("an app that ignores the interrupt: run ended %+v; want the recording's kill named with the stall", appErr)
	}
	if appErr := runNativeAs(t, models.MODE_RECORD, `touch started; exec sleep 600`); appErr.StopKilled != "" {
		t.Fatalf("an app that exits on the interrupt: run says %q", appErr.StopKilled)
	}
	if appErr := runNativeAs(t, models.MODE_TEST, ignores); appErr.StopKilled != "" {
		t.Fatalf("a replay's teardown: run says %q", appErr.StopKilled)
	}
	t.Setenv(provenswitch.Env, "0")
	if appErr := runNativeAs(t, models.MODE_RECORD, ignores); appErr.StopKilled != "" {
		t.Fatalf("with the switch off: run says %q", appErr.StopKilled)
	}
}

// A stop's kill is said for the stop at a set's end and for a recording's,
// each with its own words, and for no other stop; a stop that did not kill
// says nothing.
func TestAStopsKillIsSaidForTheSetsEndAndTheRecordingsOnly(t *testing.T) {
	const why = "its container had not stopped 5s later (the grace keploy's stop gives a docker run app), so keploy killed it"
	for _, tc := range []struct {
		name              string
		setEnd, recordEnd bool
		killed, want      string
	}{
		{"the set's end", true, false, why, "keploy interrupted the application at the set's end and " + why},
		{"the recording's end", false, true, why, "keploy stopped the application at the recording's end and " + why},
		{"another stop", false, false, why, ""},
		{"no kill", true, false, "", ""},
	} {
		a := NewApp(zap.NewNop(), "true", nil, models.SetupOptions{})
		a.noteStopKill(tc.setEnd, tc.recordEnd, tc.killed)
		if got := a.takeSetEndKill(); got != tc.want {
			t.Errorf("%s: said %q, want %q", tc.name, got, tc.want)
		}
	}
}
