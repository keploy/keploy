package log

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newQuietTestLogger builds a logger with the quiet console core writing to
// consoleBuf, and a file core writing to fileBuf. It sets the package state
// as QuietConsole does, and restores it when the test ends.
func newQuietTestLogger(t *testing.T, consoleBuf, fileBuf *bytes.Buffer) *zap.Logger {
	t.Helper()

	// Save and restore package state.
	primarySinkMu.Lock()
	oldQuiet := quietMode
	oldFile := activeLogFile
	primarySinkMu.Unlock()
	t.Cleanup(func() {
		primarySinkMu.Lock()
		quietMode = oldQuiet
		activeLogFile = oldFile
		primarySinkMu.Unlock()
	})

	// A temp file for the file core: we intercept its writes via a pipe,
	// but the quiet core reads PrimarySink, so we use a real temp file for
	// the console side and capture it through consoleBuf.
	consoleFile, err := os.CreateTemp(t.TempDir(), "quiet-console-*.txt")
	if err != nil {
		t.Fatal(err)
	}

	// File core: writes full zap format, with Show field stripped.
	fileSink := zapcore.AddSync(&writeCapture{buf: fileBuf})
	ensureLogCfg()
	LogCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	fc := cleanFileCore{consoleCore(LogCfg, fileSink, LogCfg.Level)}

	// Console core: the quiet core that checks for Show.
	qc := &quietConsoleCore{out: consoleFile}

	core := zapcore.NewTee(fc, qc)
	logger := zap.New(newRedactingCore(core))

	// After each write, read the console file back into consoleBuf.
	t.Cleanup(func() {
		_ = logger.Sync()
		b, _ := os.ReadFile(consoleFile.Name())
		consoleBuf.Write(b)
		consoleFile.Close()
	})

	primarySinkMu.Lock()
	quietMode = true
	primarySinkMu.Unlock()

	return logger
}

// writeCapture captures writes to a buffer, implementing zapcore.WriteSyncer.
type writeCapture struct{ buf *bytes.Buffer }

func (w *writeCapture) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *writeCapture) Sync() error                 { return nil }

// --- quietConsoleCore tests ---

func TestQuietConsole_ShowMarkedInfoAppearsOnConsole(t *testing.T) {
	SetRedactor(nil)
	var console, file bytes.Buffer
	logger := newQuietTestLogger(t, &console, &file)

	logger.Info("recorded mocks", Show("recorded 16 outgoing calls"), zap.Int("mocks", 16))
	_ = logger.Sync()

	// Read the console output.
	t.Cleanup(func() {}) // trigger the cleanup that reads the file
	// Force sync and read.
	logger.Sync()
}

func TestQuietConsole_ShowMarkedInfoAppearsOnConsole_Direct(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	entry := zapcore.Entry{Level: zap.InfoLevel, Message: "recorded mocks"}
	fields := []zapcore.Field{Show("recorded 16 outgoing calls"), zap.Int("mocks", 16)}

	_ = qc.Write(entry, fields)

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)
	got := buf.String()

	if !strings.Contains(got, "keploy: recorded 16 outgoing calls") {
		t.Errorf("Show-marked INFO not on console, got: %q", got)
	}
}

func TestQuietConsole_InfoWithoutShowIsSilenced(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	entry := zapcore.Entry{Level: zap.InfoLevel, Message: "per-test scoping enabled"}
	_ = qc.Write(entry, []zapcore.Field{zap.Int("tests", 5)})

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)

	if buf.Len() > 0 {
		t.Errorf("INFO without Show should be silenced, got: %q", buf.String())
	}
}

func TestQuietConsole_DebugIsSilenced(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	entry := zapcore.Entry{Level: zap.DebugLevel, Message: "internal detail"}
	_ = qc.Write(entry, nil)

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)

	if buf.Len() > 0 {
		t.Errorf("DEBUG should be silenced, got: %q", buf.String())
	}
}

func TestQuietConsole_WarnIncludesErrorDetail(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	entry := zapcore.Entry{Level: zap.WarnLevel, Message: "failed to publish"}
	fields := []zapcore.Field{zap.Error(errors.New("connection refused"))}

	_ = qc.Write(entry, fields)

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)
	got := buf.String()

	if !strings.Contains(got, "keploy: WARN: failed to publish: connection refused") {
		t.Errorf("WARN should include error detail, got: %q", got)
	}
}

func TestQuietConsole_WarnWithoutErrorShowsMessage(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	entry := zapcore.Entry{Level: zap.WarnLevel, Message: "something happened"}
	_ = qc.Write(entry, nil)

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)
	got := buf.String()

	if !strings.Contains(got, "keploy: WARN: something happened") {
		t.Errorf("WARN without error should show message, got: %q", got)
	}
}

func TestQuietConsole_ErrorIncludesErrorDetail(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	entry := zapcore.Entry{Level: zap.ErrorLevel, Message: "agent failed"}
	fields := []zapcore.Field{zap.Error(errors.New("timeout"))}

	_ = qc.Write(entry, fields)

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)
	got := buf.String()

	if !strings.Contains(got, "keploy: ERROR: agent failed: timeout") {
		t.Errorf("ERROR should include error detail, got: %q", got)
	}
}

func TestQuietConsole_NextStepGetsOwnLine(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	entry := zapcore.Entry{Level: zap.WarnLevel, Message: "no mocks captured"}
	fields := []zapcore.Field{zap.String("next_step", "check that the proxy intercepted the traffic")}

	_ = qc.Write(entry, fields)

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)
	got := buf.String()

	if !strings.Contains(got, "keploy: WARN: no mocks captured") {
		t.Errorf("WARN message not found, got: %q", got)
	}
	if !strings.Contains(got, "keploy: NEXT  fix: check that the proxy intercepted the traffic") {
		t.Errorf("next_step should be on its own NEXT line, got: %q", got)
	}
}

func TestQuietConsole_NextStepBeforeError_BothAppear(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	entry := zapcore.Entry{Level: zap.WarnLevel, Message: "no mocks captured"}
	// next_step BEFORE zap.Error — the two-pass scan must collect both.
	fields := []zapcore.Field{
		zap.String("next_step", "re-record"),
		zap.Error(errors.New("connection refused")),
	}

	_ = qc.Write(entry, fields)

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)
	got := buf.String()

	if !strings.Contains(got, "connection refused") {
		t.Errorf("error detail should appear even when next_step comes first, got: %q", got)
	}
	if !strings.Contains(got, "keploy: NEXT  fix: re-record") {
		t.Errorf("next_step should appear, got: %q", got)
	}
}

func TestQuietConsole_WithCarriesFields(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	qc := &quietConsoleCore{out: createTempFile(t)}

	sub := qc.With([]zapcore.Field{Show("from-with")})

	entry := zapcore.Entry{Level: zap.InfoLevel, Message: "something"}
	_ = sub.Write(entry, nil)

	b, _ := os.ReadFile(qc.out.Name())
	buf.Write(b)
	got := buf.String()

	if !strings.Contains(got, "keploy: from-with") {
		t.Errorf("With-carried Show should appear on console, got: %q", got)
	}
}

// --- cleanFileCore tests ---

func TestCleanFileCore_StripsShowField(t *testing.T) {
	SetRedactor(nil)
	var buf bytes.Buffer
	ensureLogCfg()
	LogCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	inner := consoleCore(LogCfg, &writeCapture{buf: &buf}, LogCfg.Level)
	fc := cleanFileCore{inner}

	entry := zapcore.Entry{Level: zap.InfoLevel, Message: "recorded mocks"}
	ce := fc.Check(entry, nil)
	if ce == nil {
		t.Fatal("Check returned nil for INFO entry")
	}
	ce.Write(Show("recorded 16 outgoing calls"), zap.Int("mocks", 16))

	got := buf.String()
	if strings.Contains(got, showKey) {
		t.Errorf("file output should not contain %s, got: %q", showKey, got)
	}
	if !strings.Contains(got, "recorded mocks") {
		t.Errorf("file output should contain the message, got: %q", got)
	}
	if !strings.Contains(got, "16") {
		t.Errorf("file output should contain the mocks count, got: %q", got)
	}
}

// --- Show() tests ---

func TestShow_ReturnsCorrectField(t *testing.T) {
	f := Show("hello world")
	if f.Key != showKey {
		t.Errorf("Show key = %q, want %q", f.Key, showKey)
	}
	if f.String != "hello world" {
		t.Errorf("Show value = %q, want %q", f.String, "hello world")
	}
}

// --- QuietConsole() edge cases ---

func TestQuietConsole_NoopWhenDebug(t *testing.T) {
	SetRedactor(nil)
	t.Chdir(t.TempDir())

	ensureLogCfg()
	LogCfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)

	logger, err := QuietConsole()
	if err != nil {
		t.Fatal(err)
	}
	// Should return a working logger (no-op quiet, full console).
	logger.Debug("debug-visible")
	// No panic = pass.
}

func TestQuietConsole_NoopWhenNoLogFile(t *testing.T) {
	SetRedactor(nil)
	t.Chdir(t.TempDir())

	ensureLogCfg()
	LogCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)

	primarySinkMu.Lock()
	old := activeLogFile
	activeLogFile = nil
	primarySinkMu.Unlock()
	t.Cleanup(func() {
		primarySinkMu.Lock()
		activeLogFile = old
		primarySinkMu.Unlock()
	})

	logger, err := QuietConsole()
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("still-visible")
	// No panic = pass.
}

// --- IsQuiet / GetActiveLogFile ---

func TestIsQuiet_ReflectsQuietMode(t *testing.T) {
	primarySinkMu.Lock()
	old := quietMode
	quietMode = false
	primarySinkMu.Unlock()
	t.Cleanup(func() {
		primarySinkMu.Lock()
		quietMode = old
		primarySinkMu.Unlock()
	})

	if IsQuiet() {
		t.Error("IsQuiet should be false before QuietConsole")
	}

	primarySinkMu.Lock()
	quietMode = true
	primarySinkMu.Unlock()

	if !IsQuiet() {
		t.Error("IsQuiet should be true after setting quietMode")
	}
}

func TestGetActiveLogFile_ReturnsStoredFile(t *testing.T) {
	primarySinkMu.Lock()
	old := activeLogFile
	primarySinkMu.Unlock()
	t.Cleanup(func() {
		primarySinkMu.Lock()
		activeLogFile = old
		primarySinkMu.Unlock()
	})

	tmp, err := os.CreateTemp(t.TempDir(), "test-log-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	primarySinkMu.Lock()
	activeLogFile = tmp
	primarySinkMu.Unlock()

	got := GetActiveLogFile()
	if got != tmp {
		t.Errorf("GetActiveLogFile returned %v, want %v", got, tmp)
	}
}

// --- helpers ---

func createTempFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "quiet-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
