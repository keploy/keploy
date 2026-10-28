package utils

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

// sentrySink is a Sentry intake on a local port: the DSN it returns points
// at it, and it counts the reports that reach it.
func sentrySink(t *testing.T) (dsn string, reports *atomic.Int32) {
	t.Helper()
	reports = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/1/") {
			reports.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return "http://public@" + strings.TrimPrefix(srv.URL, "http://") + "/1", reports
}

// resetCrashReports leaves the process as a test found it: no client bound,
// nothing armed.
func resetCrashReports(t *testing.T) {
	t.Helper()
	sentry.CurrentHub().BindClient(nil)
	ArmCrashReports("")
	t.Cleanup(func() {
		sentry.CurrentHub().BindClient(nil)
		ArmCrashReports("")
	})
}

// A recovered panic reports through HandleRecovery, which attaches this run's
// whole log. With telemetry off nothing may be sent anywhere, so no client may
// exist; with it on the report reaches the intake (the control, which shows
// this row sees reports at all).
func TestACrashSendsNothingWithTelemetryOff(t *testing.T) {
	for _, disableTele := range []bool{true, false} {
		t.Run(map[bool]string{true: "telemetry off", false: "telemetry on"}[disableTele], func(t *testing.T) {
			resetCrashReports(t)
			dsn, reports := sentrySink(t)
			ArmCrashReports(dsn)
			if sentry.CurrentHub().Client() != nil {
				t.Fatal("arming started a crash-report client before the command resolved telemetry")
			}
			ResolveCrashReports(zap.NewNop(), disableTele)
			HandleRecovery(zap.NewNop(), "boom", "Recovered from panic")
			sentry.Flush(5 * time.Second)
			got := reports.Load()
			if disableTele {
				if c := sentry.CurrentHub().Client(); c != nil {
					t.Fatal("a crash-report client runs with telemetry off")
				}
				if got != 0 {
					t.Fatalf("a crash with telemetry off sent %d reports, want 0", got)
				}
				return
			}
			if got == 0 {
				t.Fatal("a crash with telemetry on sent no report, so the telemetry-off case proves nothing")
			}
		})
	}
}

// Telemetry turned off after a client started (whoever started it) stops it:
// nothing more is sent.
func TestTelemetryOffStopsARunningCrashClient(t *testing.T) {
	resetCrashReports(t)
	dsn, reports := sentrySink(t)
	ArmCrashReports(dsn)
	ResolveCrashReports(zap.NewNop(), false)
	if sentry.CurrentHub().Client() == nil {
		t.Fatal("no crash-report client with telemetry on and a DSN armed")
	}
	ResolveCrashReports(zap.NewNop(), true)
	HandleRecovery(zap.NewNop(), "boom", "Recovered from panic")
	sentry.Flush(5 * time.Second)
	if n := reports.Load(); n != 0 {
		t.Fatalf("a crash after telemetry was turned off sent %d reports, want 0", n)
	}
}

// Left empty, the SDK names the release with `git describe` in the working
// directory, the user's repository; the client names keploy's own.
func TestTheCrashClientNamesKeploysRelease(t *testing.T) {
	resetCrashReports(t)
	prev := Version
	t.Cleanup(func() { Version = prev })
	for _, tc := range []struct{ version, want string }{{"3.4.5", "3.4.5"}, {"", DevVersion}} {
		sentry.CurrentHub().BindClient(nil)
		Version = tc.version
		dsn, _ := sentrySink(t)
		ArmCrashReports(dsn)
		ResolveCrashReports(zap.NewNop(), false)
		c := sentry.CurrentHub().Client()
		if c == nil {
			t.Fatal("no crash-report client with telemetry on and a DSN armed")
		}
		if got := c.Options().Release; got != tc.want {
			t.Fatalf("Version %q: the crash client's release = %q, want %q", tc.version, got, tc.want)
		}
	}
}

// No DSN (a build without one): no client, telemetry on or not.
func TestNoDSNNoCrashClient(t *testing.T) {
	resetCrashReports(t)
	ResolveCrashReports(zap.NewNop(), false)
	if sentry.CurrentHub().Client() != nil {
		t.Fatal("a crash-report client started with no DSN armed")
	}
}
