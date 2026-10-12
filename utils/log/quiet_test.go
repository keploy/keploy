package log

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func quietFiles(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	dir := t.TempDir()
	console, err := os.Create(filepath.Join(dir, "console"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "file"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		quiet.Store(false)
		runFile.Store(nil)
		TestOnlyResetSink()
		_ = console.Close()
		_ = file.Close()
	})
	setPrimarySink(console)
	runFile.Store(file)
	LogCfg = defaultLogCfg()
	return console, file
}

func read(t *testing.T, f *os.File) string {
	t.Helper()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestQuietConsoleShowsOnlyFacts(t *testing.T) {
	console, file := quietFiles(t)
	logger, err := Quiet()
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("internal line", zap.String("k", "v"))
	logger.Info("recording: go test ./...", zap.String("mock-set", "shop"), Show)
	logger.Warn("a warning", zap.Error(errors.New("boom")))
	logger.Warn("an internal warning", Hide)
	logger.Debug("a debug line")

	want := "keploy: recording: go test ./...\nkeploy: a warning: boom\n"
	if got := read(t, console); got != want {
		t.Fatalf("console = %q, want %q", got, want)
	}
	got := read(t, file)
	for _, line := range []string{"internal line", `"k": "v"`, "recording: go test ./...", "a warning", "an internal warning"} {
		if !strings.Contains(got, line) {
			t.Errorf("the log file lacks %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "keploy: ") || strings.Contains(got, "a debug line") {
		t.Errorf("the log file changed format or level:\n%s", got)
	}
}

func TestQuietSurvivesRebuildsAndDebugEndsIt(t *testing.T) {
	console, _ := quietFiles(t)
	if _, err := Quiet(); err != nil {
		t.Fatal(err)
	}
	logger, err := ChangeColorEncoding()
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	if got := read(t, console); got != "" {
		t.Fatalf("a rebuild undid Quiet: %q", got)
	}
	logger, err = ChangeLogLevel(zapcore.DebugLevel)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("shown")
	if Quieted() || !strings.Contains(read(t, console), "INFO") {
		t.Fatalf("--debug did not restore the full console: %q", read(t, console))
	}
}

func TestMarksDoNotChangeALineOutsideQuiet(t *testing.T) {
	console, _ := quietFiles(t)
	logger, err := ChangeColorEncoding()
	if err != nil {
		t.Fatal(err)
	}
	logger.Warn("same", zap.String("k", "v"))
	logger.Warn("same", zap.String("k", "v"), Show)
	logger.Warn("same", zap.String("k", "v"), Hide)
	lines := strings.Split(strings.TrimSpace(read(t, console)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %q", lines)
	}
	strip := func(l string) string { return l[strings.Index(l, "WARN"):] }
	if strip(lines[0]) != strip(lines[1]) || strip(lines[0]) != strip(lines[2]) {
		t.Fatalf("a mark changed the line:\n%s", strings.Join(lines, "\n"))
	}
}

func TestFactsWritesMarkedLinesOnceInTheirOwnFormat(t *testing.T) {
	console, _ := quietFiles(t)
	logger, err := ChangeColorEncoding()
	if err != nil {
		t.Fatal(err)
	}
	logger = Facts(logger)
	logger.Info("internal")
	logger.Info("shifted the clock", Show)
	got := read(t, console)
	if !strings.Contains(got, "INFO\tinternal") || !strings.Contains(got, "\nkeploy: shifted the clock\n") || strings.Count(got, "shifted the clock") != 1 {
		t.Fatalf("console = %q", got)
	}
}

func TestQuietRendersTheNextStepAndSurvivesAddMode(t *testing.T) {
	console, _ := quietFiles(t)
	if _, err := Quiet(); err != nil {
		t.Fatal(err)
	}
	logger, err := AddMode("record")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Error("the agent could not start", zap.Error(errors.New("exit 1")), zap.String("next_step", "mount tracefs"))
	if got, want := read(t, console), "keploy: the agent could not start: exit 1\nkeploy: next step: mount tracefs\n"; got != want {
		t.Fatalf("console = %q, want %q", got, want)
	}
}

func TestNoQuietWithoutALogFile(t *testing.T) {
	console, _ := quietFiles(t)
	runFile.Store(nil)
	logger, err := Quiet()
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("still on the console")
	if Quieted() || !strings.Contains(read(t, console), "still on the console") {
		t.Fatalf("a run with no log file went quiet: %q", read(t, console))
	}
}

func TestQuietWritesTheRunFileItWasGiven(t *testing.T) {
	_, file := quietFiles(t)
	other, err := os.Create(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	SetRunFile(other)
	logger, err := Quiet()
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("this run's line")
	if strings.Contains(read(t, file), "this run's line") || !strings.Contains(read(t, other), "this run's line") {
		t.Fatal("the line did not go to the run file")
	}
}
