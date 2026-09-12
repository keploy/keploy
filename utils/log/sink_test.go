package log

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Every logger-rebuilding helper builds its core from the package's shared
// console sink. RedirectToStderr moves that sink, so a rebuild that happens
// afterwards — --disable-ansi (ChangeColorEncoding), --debug (ChangeLogLevel),
// agent mode (AddMode) — must stay on stderr.
//
// The regression: each helper used to hardcode os.Stdout, so
// `keploy report --format junit --json --disable-ansi` printed INFO lines
// above the XML because --disable-ansi ran after the redirect.
func TestRedirectToStderrSurvivesEveryLoggerRebuild(t *testing.T) {
	orig := PrimarySink()
	t.Cleanup(func() { setPrimarySink(orig) })

	setPrimarySink(os.Stdout)
	LogCfg = zap.Config{}

	if got := PrimarySink(); got != os.Stdout {
		t.Fatalf("precondition: sink is %v, want stdout", got)
	}
	if _, err := RedirectToStderr(); err != nil {
		t.Fatalf("RedirectToStderr: %v", err)
	}
	if got := PrimarySink(); got != os.Stderr {
		t.Fatalf("RedirectToStderr did not move the sink: %v", got)
	}

	rebuilds := []struct {
		name string
		fn   func() (*zap.Logger, error)
	}{
		{name: "ChangeColorEncoding (--disable-ansi)", fn: ChangeColorEncoding},
		{name: "ChangeLogLevel (--debug)", fn: func() (*zap.Logger, error) { return ChangeLogLevel(zapcore.DebugLevel) }},
		{name: "AddMode (agent)", fn: func() (*zap.Logger, error) { return AddMode("agent") }},
	}
	for _, rb := range rebuilds {
		t.Run(rb.name, func(t *testing.T) {
			logger, err := rb.fn()
			if err != nil {
				t.Fatalf("%s: %v", rb.name, err)
			}
			if logger == nil {
				t.Fatalf("%s returned a nil logger", rb.name)
			}
			if got := PrimarySink(); got != os.Stderr {
				t.Errorf("%s put the logger back on %v; machine-readable stdout is now corrupted", rb.name, got)
			}
		})
	}
}

// The rebuild helpers must be safe before New() has run: they build their core
// from LogCfg.Level, and the zero zap.AtomicLevel carries a nil *atomic.Int32
// that panics on the logger's first write — at the caller, not here.
func TestLoggerRebuildsAreSafeWithAnUninitialisedConfig(t *testing.T) {
	orig := PrimarySink()
	origCfg := LogCfg
	t.Cleanup(func() { setPrimarySink(orig); LogCfg = origCfg })

	rebuilds := map[string]func() (*zap.Logger, error){
		"RedirectToStderr":    RedirectToStderr,
		"ChangeColorEncoding": ChangeColorEncoding,
		"ChangeLogLevel":      func() (*zap.Logger, error) { return ChangeLogLevel(zapcore.InfoLevel) },
		"AddMode":             func() (*zap.Logger, error) { return AddMode("agent") },
	}
	for name, fn := range rebuilds {
		t.Run(name, func(t *testing.T) {
			LogCfg = zap.Config{}
			setPrimarySink(os.Stderr)

			logger, err := fn()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// The panic was on the first write, not on construction.
			logger.Debug("probe")
			logger.Info("probe")
		})
	}
}

// The sink defaults to os.Stdout resolved AT CALL TIME, not to the descriptor
// this package saw at init. A test (and the CLI's own stdout-cleanliness test)
// swaps os.Stdout for a pipe; capturing it at init would send the logo and the
// version line to the real terminal and make the capture assert on nothing.
func TestPrimarySinkResolvesStdoutLazily(t *testing.T) {
	// TestOnlyResetSink, not a captured sink: restoring an EXPLICIT os.Stdout would
	// quietly undo the lazy default this test exists to pin.
	t.Cleanup(TestOnlyResetSink)

	setPrimarySink(nil)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	realStdout := os.Stdout
	os.Stdout = w
	got := PrimarySink()
	os.Stdout = realStdout

	if got != w {
		t.Fatalf("PrimarySink() returned %v, want the swapped os.Stdout (%v)", got, w)
	}

	// And an explicit sink still wins over os.Stdout.
	setPrimarySink(os.Stderr)
	if got := PrimarySink(); got != os.Stderr {
		t.Fatalf("an explicit sink was ignored: %v", got)
	}

	// TestOnlyResetSink puts it back to the lazy default.
	TestOnlyResetSink()
	if got := PrimarySink(); got != os.Stdout {
		t.Fatalf("TestOnlyResetSink did not restore the stdout default: %v", got)
	}
}

// TestOnlyResetSink is an exported symbol on a package every keploy binary
// imports, and its whole contract is "undo global state a test dirtied". It
// cannot live in an export_test.go because cli/provider's tests need it across
// the package boundary, so the guard is this: nothing outside a _test.go file
// may call it. A production caller would detach the debug file sink from the
// next logger rebuild and silently undo any --json / --format json redirect.
func TestNoProductionCallerResetsTheSink(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// A CALL is `TestOnlyResetSink(`; the one declaration is
		// `func TestOnlyResetSink(`. Matching on the call syntax rather than
		// on the bare name means a caller inside logger.go itself is caught
		// too, and prose mentions in doc comments are not.
		for _, line := range strings.Split(string(body), "\n") {
			idx := strings.Index(line, "TestOnlyResetSink(")
			if idx < 0 {
				continue
			}
			if strings.HasSuffix(line[:idx], "func ") {
				continue // the declaration itself
			}
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel+": "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("log.TestOnlyResetSink is referenced from non-test files %v. "+
			"It resets package-global logger state and is for tests only: a production caller "+
			"detaches the debug file sink from the next logger rebuild and undoes the "+
			"machine-output stderr redirect.", offenders)
	}
}
