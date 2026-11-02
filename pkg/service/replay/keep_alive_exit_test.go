package replay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A native app run with --keep-app-alive that exits mid-set, holding a test
// or between two, halts the set and fails the run naming the app, with the
// proven switch off and on: the set is APP_HALTED, its reason naming the
// exit's code and the test held, if one was; no test is sent after the exit;
// the exit code is 1, Start's error names the app, and keploy's stop does not
// say the replay completed "successfully". Start tells the set the exit at
// its run's return (keptAppExit), as compose's runner does at the app's die.
// It ended the run's errgroup context instead: the set read USER_ABORT, Start
// returned nil, its stop said "replay completed successfully", and an exit
// between two tests exited 0.
func TestAKeptNativeAppsExitMidSetFailsTheRunNamingTheApp(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
		// between: the app exits after test-1's verdict and before test-2
		// begins (prInstr.stopAppAfterNUpdates); otherwise it exits holding
		// test-2.
		between bool
	}{
		{"holding test-2, switch off", false, false},
		{"holding test-2, switch on", true, false},
		{"between test-1 and test-2, switch off", false, true},
		{"between test-1 and test-2, switch on", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, tc.on)
			h := newPartialRunHarness(t, 3, 0)
			h.instr.stopErr = models.ErrUnExpected
			app := &heldRequests{exitAt: "test-2", exit: func() { close(h.instr.appStopped) }, requests: map[string]int{}}
			if tc.between {
				app.exitAt = ""
				h.instr.stopAppAfterNUpdates = 3
			}
			h.replayer.hookImpl = prHooks{onRequest: app.onRequest}
			core, logs := observer.New(zapcore.DebugLevel)
			h.replayer.logger = zap.New(core)
			h.replayer.config.Test.Mocking = true
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.KeepAppAlive = true
			// What config/default.go sets, which the harness's bare config
			// does not: Start runs each test set this many times at most.
			h.replayer.config.Test.MaxFlakyChecks = 1
			saved := utils.ErrCode
			t.Cleanup(func() { utils.ErrCode = saved })
			utils.ErrCode = 0
			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()

			err := h.replayer.Start(ctx)
			t.Cleanup(func() {
				if !t.Failed() {
					return
				}
				t.Logf("Start: %v", err)
				if rep := h.report.report; rep != nil {
					t.Logf("report: status %s, reason %q", rep.Status, rep.FailureReason)
				}
				for _, e := range logs.All() {
					t.Logf("%s %s %v", e.Level, e.Message, e.ContextMap())
				}
			})

			rep := h.report.report
			if rep == nil {
				t.Fatal("the set wrote no report")
			}
			if rep.Status != string(models.TestSetStatusAppHalted) {
				t.Fatalf("status %s, reason %q; want APP_HALTED: the kept app exited mid-set", rep.Status, rep.FailureReason)
			}
			held := strings.Contains(rep.FailureReason, "before it answered test test-2")
			if !strings.Contains(rep.FailureReason, "it exited with code 1") || held == tc.between {
				t.Fatalf("reason %q; want it to name the exit's code, and test-2 only when the app held it", rep.FailureReason)
			}
			if n := app.count("test-1"); n != 1 {
				t.Fatalf("test-1 was sent %d times, want once, before the app exited", n)
			}
			if utils.ErrCode != 1 {
				t.Fatalf("exit code %d, want 1: the kept app exited mid-set", utils.ErrCode)
			}
			if err == nil || !strings.Contains(err.Error(), "user application failed under --keep-app-alive") {
				t.Fatalf("Start returned %v; want it to name the kept app's failure", err)
			}
			for _, e := range logs.FilterMessage("stopping Keploy").All() {
				if reason, _ := e.ContextMap()["reason"].(string); strings.Contains(reason, "successfully") {
					t.Fatalf("keploy's stop says %q for a run whose kept app exited mid-set", reason)
				}
			}
			if want := map[bool]int{false: 1, true: 0}[tc.between]; app.count("test-2") != want {
				t.Fatalf("test-2 was sent %d times, want %d", app.count("test-2"), want)
			}
			if n := app.count("test-3"); n != 0 {
				t.Fatalf("test-3 was sent %d times after the app had exited", n)
			}
		})
	}
}

// A native app that exits while keploy waits for it to be ready, before the
// set's first test, halts the set as an exit in a compose set's setup does
// (setupEnd), whether the run keeps it for every set (--keep-app-alive) or
// the set starts its own, with the proven switch off and on: the set is
// APP_HALTED, "application startup failed" with the exit's code, no test is
// sent, the exit code is 1, and keploy's stop says neither "successfully"
// nor "failed to run test set". The exit's end of the set cut the wait
// short, and the set returned "context canceled" as a setup failure: the run
// stopped on "failed to run test set: context canceled" with the set
// unreported.
func TestANativeAppsExitWhileKeployWaitsForItHaltsTheSet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		on, kept bool
	}{
		{"kept alive, switch off", false, true},
		{"kept alive, switch on", true, true},
		{"started by the set, switch off", false, false},
		{"started by the set, switch on", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, tc.on)
			h := newPartialRunHarness(t, 2, 0)
			h.instr.stopErr = models.ErrUnExpected
			app := &heldRequests{requests: map[string]int{}}
			h.replayer.hookImpl = prHooks{onRequest: app.onRequest}
			// The app exits at keploy's first probe of its health URL, which
			// it holds until keploy gives the probe up.
			var exited sync.Once
			health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				exited.Do(func() { close(h.instr.appStopped) })
				select {
				case <-req.Context().Done():
				case <-time.After(10 * time.Second):
				}
			}))
			t.Cleanup(health.Close)
			h.replayer.config.Test.HealthURL = health.URL + "/health"
			core, logs := observer.New(zapcore.DebugLevel)
			h.replayer.logger = zap.New(core)
			h.replayer.config.Test.Mocking = true
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.KeepAppAlive = tc.kept
			h.replayer.config.Test.MaxFlakyChecks = 1
			saved := utils.ErrCode
			t.Cleanup(func() { utils.ErrCode = saved })
			utils.ErrCode = 0
			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()

			err := h.replayer.Start(ctx)
			t.Cleanup(func() {
				if !t.Failed() {
					return
				}
				t.Logf("Start: %v", err)
				for _, e := range logs.All() {
					t.Logf("%s %s %v", e.Level, e.Message, e.ContextMap())
				}
			})

			rep := h.report.report
			if rep == nil {
				t.Fatal("the set wrote no report")
			}
			if rep.Status != string(models.TestSetStatusAppHalted) ||
				!strings.Contains(rep.FailureReason, "application startup failed: it exited with code 1") {
				t.Fatalf("status %s, reason %q; want APP_HALTED, the app's startup failing with its exit's code", rep.Status, rep.FailureReason)
			}
			if utils.ErrCode != 1 {
				t.Fatalf("exit code %d, want 1", utils.ErrCode)
			}
			for _, e := range logs.FilterMessage("stopping Keploy").All() {
				if reason, _ := e.ContextMap()["reason"].(string); strings.Contains(reason, "successfully") || strings.Contains(reason, "failed to run test set") {
					t.Fatalf("keploy's stop says %q for a run whose app exited before the set's first test", reason)
				}
			}
			for _, name := range []string{"test-1", "test-2"} {
				if n := app.count(name); n != 0 {
					t.Fatalf("%s was sent %d times after the app had exited", name, n)
				}
			}
		})
	}
}
