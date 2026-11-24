package app

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A compose app kill after the stop grace expires must log at INFO once the
// run's tests are already done (utils.WithTestsDone), while keeping WARN when
// tests have not finished.
func TestStopComposeAppKillLogsInfoAfterTestsAndWarnBefore(t *testing.T) {
	stallBudget(t, 100*time.Millisecond)

	for _, tc := range []struct {
		name      string
		testsDone bool
		wantLevel zapcore.Level
	}{
		{"tests done logs INFO", true, zapcore.InfoLevel},
		{"tests not done logs WARN", false, zapcore.WarnLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ctr := appStack()
			// Ignore SIGINT so StopComposeApp hits the stall bound and sends SIGKILL.
			ctr.exitsOn = map[string]int{"SIGKILL": 137}
			a := stackApp(d, ctr.name)

			core, logs := observer.New(zapcore.InfoLevel)
			a.logger = zap.New(core)

			ctx := context.Background()
			if tc.testsDone {
				ctx = utils.WithTestsDone(ctx)
			}
			killed, err := a.StopComposeApp(ctx)
			if err != nil {
				t.Fatalf("StopComposeApp failed: %v", err)
			}
			if killed == "" {
				t.Fatalf("expected StopComposeApp to report kill reason, got empty")
			}
			entries := logs.FilterMessageSnippet("keploy is killing the app it stopped:").All()
			if len(entries) != 1 {
				t.Fatalf("expected 1 kill log line, got %d: %v", len(entries), logs.All())
			}
			if entries[0].Level != tc.wantLevel {
				t.Errorf("kill log level = %s, want %s (line: %q)", entries[0].Level, tc.wantLevel, entries[0].Message)
			}
		})
	}

	// Verify the tests-done signal comes purely from ctx (not sticky on App):
	// a later StopComposeApp without utils.WithTestsDone on the SAME App must
	// still log WARN.
	t.Run("not sticky across stops on the same App", func(t *testing.T) {
		d, ctr := appStack()
		ctr.exitsOn = map[string]int{"SIGKILL": 137}
		a := stackApp(d, ctr.name)

		core, logs := observer.New(zapcore.InfoLevel)
		a.logger = zap.New(core)

		if _, err := a.StopComposeApp(utils.WithTestsDone(context.Background())); err != nil {
			t.Fatalf("first StopComposeApp failed: %v", err)
		}
		if entries := logs.FilterMessageSnippet("keploy is killing the app it stopped:").All(); len(entries) != 1 || entries[0].Level != zapcore.InfoLevel {
			t.Fatalf("first stop got %v, want 1 INFO kill log", entries)
		}

		// Bring container back running without going through StartComposeApp.
		d.mu.Lock()
		ctr.running = true
		d.mu.Unlock()
		logs.TakeAll()

		if _, err := a.StopComposeApp(context.Background()); err != nil {
			t.Fatalf("second StopComposeApp failed: %v", err)
		}
		if entries := logs.FilterMessageSnippet("keploy is killing the app it stopped:").All(); len(entries) != 1 || entries[0].Level != zapcore.WarnLevel {
			t.Fatalf("second stop without WithTestsDone got %v, want 1 WARN kill log", entries)
		}
	})
}
