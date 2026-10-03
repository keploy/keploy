package pkg

import (
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestLogSendFailure_TransportResetLogsWarn pins the fix: a transport-level
// reset (IsTransportConnReset — dominated under loaded CI by docker's
// userland proxy resetting a freshly-accepted connection before the app
// processes it) must log at WARN, not ERROR, because the caller
// (Replayer.retryResetOnce) safely re-sends this exact error class and
// usually recovers it. Logging ERROR here — before the caller has any chance
// to retry — made a routine, self-healing retry indistinguishable from a
// fatal crash to any downstream tooling (several CI lanes) that scans logs
// for "ERROR". If this test starts failing, someone reverted the fix.
func TestLogSendFailure_TransportResetLogsWarn(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	logSendFailure(logger, connResetErr("http://127.0.0.1:8080/health"))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d", len(entries))
	}
	if entries[0].Level != zapcore.WarnLevel {
		t.Fatalf("expected a transport reset to log at WARN, got %s: %q", entries[0].Level, entries[0].Message)
	}
}

// TestLogSendFailure_NonResetErrorStaysError confirms the fix is scoped
// correctly: an error that is NOT a transport reset (e.g. a genuinely fatal
// failure, or an ECONNREFUSED that already exhausted its own bounded retries
// in doRequestWithConnRefusedRetry) is never retried by anything upstream, so
// it must keep logging at ERROR -- no signal lost for a real failure.
func TestLogSendFailure_NonResetErrorStaysError(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	logSendFailure(logger, errors.New("boom: dns lookup failed"))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d", len(entries))
	}
	if entries[0].Level != zapcore.ErrorLevel {
		t.Fatalf("expected a non-reset error to log at ERROR, got %s: %q", entries[0].Level, entries[0].Message)
	}
}
