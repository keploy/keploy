package replay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// P101-c: under the proven switch a set whose end keploy read with its
// application still running says so in its report (AppNotStopped): a call the
// application made after that read is not judged. A set keploy stopped its
// app for says nothing; with the switch off nothing is said.
func TestASetReadWithItsAppRunningSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		on      bool
		serving bool
		compose bool
		want    string
	}{
		{"its app kept across sets", true, true, false, "--keep-app-alive"},
		{"its app stopped first", true, false, false, ""},
		{"its compose stack kept across sets", true, true, true, "docker compose stack"},
		{"switch off", false, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, tc.on)
			h := newPartialRunHarness(t, 2, 0)
			if tc.compose {
				h.replayer.config.CommandType = string(utils.DockerCompose)
			}
			var status models.TestSetStatus
			if tc.serving {
				status = h.runServing(t)
			} else {
				status = h.run(t)
			}
			got := h.report.report.AppNotStopped
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("status %s, app_not_stopped %q; want %q", status, got, tc.want)
			}
			if tc.want != "" && status != models.TestSetStatusPassed {
				t.Fatalf("status %s; the set's verdict is what its read found, PASSED", status)
			}
		})
	}
}

// The final attempt of a run's last set stops the app --keep-app-alive keeps
// across sets before its end's read, and says nothing more; an earlier
// attempt, or another set, leaves it running and says so. A stop that had to
// kill it leaves the set not judged, as for a set's own app.
func TestTheLastSetStopsTheAppKeptAcrossSets(t *testing.T) {
	provenOn(t, true)
	for _, tc := range []struct {
		name        string
		last, final bool
		killed      string
		stops       int
		disclosed   bool
		status      models.TestSetStatus
	}{
		{"the last set's final attempt", true, true, "", 1, false, models.TestSetStatusPassed},
		{"the last set's final attempt, its stop killing", true, true, "keploy interrupted the application at the set's end and a process of it had not exited 3s later (the grace keploy's stop gives each of its process groups), so keploy killed it", 1, false, models.TestSetStatusInternalErr},
		{"an earlier attempt of the last set", true, false, "", 0, true, models.TestSetStatusPassed},
		{"another set", false, true, "", 0, true, models.TestSetStatusPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			stops := 0
			h.replayer.isLastTestSet, h.replayer.finalAttempt = tc.last, tc.final
			h.replayer.keepAliveStop = func() string { stops++; return tc.killed }
			status := h.runServing(t)
			if stops != tc.stops || status != tc.status || (h.report.report.AppNotStopped != "") != tc.disclosed {
				t.Fatalf("stops %d, status %s, app_not_stopped %q; want %d stops, %s, disclosed %v",
					stops, status, h.report.report.AppNotStopped, tc.stops, tc.status, tc.disclosed)
			}
		})
	}
}

// Each kind of run says why it read its set's end with the app running.
func TestWhyAppNotStopped(t *testing.T) {
	if got := whyAppNotStopped(utils.DockerCompose, true); !strings.Contains(got, "keeps the application's docker compose stack running across test-sets") {
		t.Errorf("compose, kept: %q", got)
	}
	if got := whyAppNotStopped(utils.DockerCompose, false); strings.Contains(got, "across test-sets") || !strings.Contains(got, "takes keploy's agent down") {
		t.Errorf("compose, per set: %q", got)
	}
	if got := whyAppNotStopped(utils.Native, true); !strings.Contains(got, "--keep-app-alive") {
		t.Errorf("keep-alive: %q", got)
	}
	if got := whyAppNotStopped(utils.Empty, false); !strings.Contains(got, "did not start the application") {
		t.Errorf("no app: %q", got)
	}
}

// The app --keep-app-alive keeps across sets runs, under the proven switch, on
// a context of its own: its stop cancels it with the set-end cause and waits
// until its run has ended, saying why that run's stop killed it. Off the
// switch, and under docker compose, there is no such stop.
func TestTheKeptAppsStopWaitsForItsRun(t *testing.T) {
	provenOn(t, true)
	ctx := context.Background()
	appCtx, stop, ran := keepAliveStopper(ctx, utils.Native)
	if stop == nil {
		t.Fatal("no stop for an app kept across sets, under the switch")
	}
	ended := make(chan struct{})
	go func() {
		<-appCtx.Done()
		if !errors.Is(context.Cause(appCtx), models.ErrAppStoppedForSetEnd) {
			t.Errorf("the app's run was ended with %v, want the set-end cause", context.Cause(appCtx))
		}
		ran(models.AppError{AppErrorType: models.ErrCtxCanceled, StopKilled: "killed it"})
		close(ended)
	}()
	if why := stop(); why != "killed it" {
		t.Fatalf("the stop said %q, want its run's kill", why)
	}
	<-ended

	runCtx, cancel := context.WithCancel(ctx)
	_, stop, _ = keepAliveStopper(runCtx, utils.Native)
	cancel()
	if why := stop(); why != "" {
		t.Fatalf("a run that ended first: the stop said %q", why)
	}

	if appCtx, stop, _ := keepAliveStopper(ctx, utils.DockerCompose); stop != nil || appCtx != ctx {
		t.Fatal("docker compose: a stop of the app, which would stop keploy's agent")
	}
	provenOn(t, false)
	if appCtx, stop, _ := keepAliveStopper(ctx, utils.Native); stop != nil || appCtx != ctx {
		t.Fatal("switch off: a stop of the app")
	}
}

// A set's attempt is the last it can be given only on the last flaky check,
// and never under --must-pass, which runs the set again once its failing
// tests are taken out: the app kept across sets is needed then.
func TestNoAttemptFollowsOnlyTheLastFlakyCheckWithoutMustPass(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		test    config.Test
		want    bool
	}{
		{1, config.Test{MaxFlakyChecks: 1}, true},
		{1, config.Test{MaxFlakyChecks: 3}, false},
		{3, config.Test{MaxFlakyChecks: 3}, true},
		{1, config.Test{MaxFlakyChecks: 1, MustPass: true}, false},
		{3, config.Test{MaxFlakyChecks: 3, MustPass: true}, false},
	} {
		if got := noAttemptFollows(tc.attempt, tc.test); got != tc.want {
			t.Errorf("attempt %d of %+v: %v, want %v", tc.attempt, tc.test, got, tc.want)
		}
	}
}
