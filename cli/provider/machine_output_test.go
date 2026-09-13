package provider

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// captureStdout swaps os.Stdout for a pipe, runs fn, and returns everything fn
// wrote to it. The pipe is drained on a goroutine so a large logo cannot block
// on the 64KB pipe buffer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	defer func() {
		os.Stdout = orig
		_ = r.Close()
	}()
	fn()
	_ = w.Close()
	return <-done
}

// captureStderr is captureStdout's other half, for the modes that move the
// console sink off stdout.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	defer func() {
		os.Stderr = orig
		_ = r.Close()
	}()
	fn()
	_ = w.Close()
	return <-done
}

func TestIsMachineReadableOutput(t *testing.T) {
	cases := []struct {
		name       string
		cmd        string
		args       []string
		cfgFormat  string // what PreProcessFlags resolved from keploy.yml
		jsonOutput bool
		// extraFormatFlag registers a --format flag on a command that is not
		// `keploy report`, the way an enterprise-only subcommand could.
		extraFormatFlag bool
		want            bool
	}{
		{name: "plain report", cmd: "report", want: false},
		{name: "text format", cmd: "report", args: []string{"--format", "text"}, want: false},
		{name: "json format", cmd: "report", args: []string{"--format", "json"}, want: true},
		{name: "junit format", cmd: "report", args: []string{"--format", "junit"}, want: true},
		{name: "case and padding normalised", cmd: "report", args: []string{"--format", " JUnit "}, want: true},
		{name: "an invalid value is not machine output", cmd: "report", args: []string{"--format", "xml"}, want: false},
		{name: "the global --json flag alone", cmd: "test", jsonOutput: true, want: true},
		{name: "a command with no --format flag at all", cmd: "test", want: false},
		{
			// `report: {format: json}` in keploy.yml, no flag passed. This was
			// silently ignored: the flag value was read raw, so the config key
			// existed but did nothing, and stdout kept the logo.
			name: "the config file alone selects a machine format",
			cmd:  "report", cfgFormat: "json", want: true,
		},
		{name: "config junit too", cmd: "report", cfgFormat: "junit", want: true},
		{name: "config text is not machine output", cmd: "report", cfgFormat: "text", want: false},
		{
			// An explicitly passed flag beats the config file, in both
			// directions.
			name: "an explicit --format text overrides a machine format in the config",
			cmd:  "report", args: []string{"--format", "text"}, cfgFormat: "json", want: false,
		},
		{
			name: "an explicit --format json overrides text in the config",
			cmd:  "report", args: []string{"--format", "json"}, cfgFormat: "text", want: true,
		},
		{
			// `keploy test` never emits a report document on stdout, so a
			// report format in keploy.yml must not suppress its logo.
			name: "a report format in the config does not affect keploy test",
			cmd:  "test", cfgFormat: "json", want: false,
		},
		{
			// cli/provider is shared with the enterprise binary. A command
			// there that grows an unrelated --format flag whose value happens
			// to be "json" must not have its logo suppressed and its logger
			// pushed to stderr; only `keploy report` writes a report document
			// on stdout. extraFormatFlag registers the flag on a non-report
			// command to prove the scoping is by NAME, not merely by flag
			// presence.
			name: "a non-report command with its own --format json is not machine output",
			cmd:  "diff", args: []string{"--format", "json"},
			extraFormatFlag: true, want: false,
		},
		{
			name: "...and its config value is ignored too",
			cmd:  "diff", cfgFormat: "junit",
			extraFormatFlag: true, want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCmdConfigurator(zap.NewNop(), config.New())
			cmd := &cobra.Command{Use: tc.cmd}
			if err := c.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			if tc.extraFormatFlag {
				cmd.Flags().String("format", "text", "an unrelated format flag on another command")
			}
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			if got := isMachineReadableOutput(cmd, tc.cfgFormat, tc.jsonOutput); got != tc.want {
				t.Errorf("isMachineReadableOutput() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The regression this pins: `keploy report --format json 2>/dev/null | jq`
// failed with "Invalid numeric literal" because the ANSI logo, the version
// line and every zap INFO record went to STDOUT ahead of the NDJSON. Both the
// log redirect and the logo suppression were gated on the global --json flag,
// while --format was not read until several hundred lines later.
//
// The test drives the real ValidateFlags, captures the real os.Stdout, writes
// the document the report service would write, and decodes every line.
func TestMachineFormatsKeepStdoutParseable(t *testing.T) {
	// Deliberately NOT calling log.New() (it would create keploy-logs.txt in
	// the working directory): the rebuild helpers must work from an untouched
	// LogCfg, which is what log.ensureLogCfg guarantees.

	cases := []struct {
		name      string
		args      []string
		cfgFormat string
		debug     bool
		rebuild   func(t *testing.T, logger **zap.Logger)
		lines     []any
	}{
		{
			name:  "--format json",
			args:  []string{"--format", "json"},
			lines: []any{map[string]string{"schema_version": "1"}, map[string]string{"schema_version": "1"}},
		},
		{
			name:  "--format junit (same pre-existing bug)",
			args:  []string{"--format", "junit"},
			lines: []any{map[string]string{"schema_version": "1"}},
		},
		{
			// --disable-ansi rebuilds the logger through ChangeColorEncoding,
			// which used to hardcode os.Stdout and silently undo the redirect.
			name: "--format json --disable-ansi",
			args: []string{"--format", "json", "--disable-ansi"},
			rebuild: func(t *testing.T, logger **zap.Logger) {
				t.Helper()
				l, err := log.ChangeColorEncoding()
				if err != nil {
					t.Fatalf("ChangeColorEncoding: %v", err)
				}
				**logger = *l
			},
			lines: []any{map[string]string{"schema_version": "1"}},
		},
		{
			name:  "--format json --debug (ChangeLogLevel rebuild)",
			args:  []string{"--format", "json"},
			debug: true,
			lines: []any{map[string]string{"schema_version": "1"}},
		},
		{
			name: "--format json + agent mode (AddMode rebuild)",
			args: []string{"--format", "json"},
			rebuild: func(t *testing.T, logger **zap.Logger) {
				t.Helper()
				l, err := log.AddMode("agent")
				if err != nil {
					t.Fatalf("AddMode: %v", err)
				}
				**logger = *l
			},
			lines: []any{map[string]string{"schema_version": "1"}},
		},
		{
			// No flag at all: the format comes from keploy.yml. Fixing the
			// config binding without teaching the stdout gate about it would
			// have put the logo, the version line and every zap record back on
			// stdout ahead of the NDJSON for exactly these users.
			name:      "report.format json from keploy.yml, no flag passed",
			cfgFormat: "json",
			lines:     []any{map[string]string{"schema_version": "1"}},
		},
		{
			name:      "report.format junit from keploy.yml, no flag passed",
			cfgFormat: "junit",
			lines:     []any{map[string]string{"schema_version": "1"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ValidateFlags moves the package-level log sink to stderr and, on
			// the --disable-ansi case, rewrites LogCfg. Both outlive the test,
			// so restore them or every later test in this binary inherits them.
			t.Cleanup(log.TestOnlyResetSink)

			cfg := config.New()
			cfg.Debug = tc.debug
			cfg.Report.Format = tc.cfgFormat
			c := NewCmdConfigurator(zap.New(nil), cfg)
			cmd := &cobra.Command{Use: "report"}
			if err := c.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			// Registered on the root command in the real CLI.
			cmd.Flags().Bool("disable-ansi", false, "")
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}

			out := captureStdout(t, func() {
				if err := c.ValidateFlags(t.Context(), cmd); err != nil {
					t.Errorf("ValidateFlags: %v", err)
					return
				}
				if tc.rebuild != nil {
					tc.rebuild(t, &c.logger)
				}
				// Everything keploy logs from here on must land on stderr.
				c.logger.Info("No test sets selected for report generation")
				c.logger.Warn("Color encoding is disabled")

				w := utils.NewJSONWriter(true)
				for _, line := range tc.lines {
					if err := w.Write(line); err != nil {
						t.Errorf("write: %v", err)
					}
				}
			})

			if out == "" {
				t.Fatal("nothing was written to stdout")
			}
			dec := json.NewDecoder(strings.NewReader(out))
			n := 0
			for {
				var v map[string]any
				err := dec.Decode(&v)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("stdout is not parseable JSON (%v).\n--- stdout ---\n%s", err, out)
				}
				n++
			}
			if n != len(tc.lines) {
				t.Fatalf("decoded %d objects, want %d.\n--- stdout ---\n%s", n, len(tc.lines), out)
			}
		})
	}
}

// The text format keeps the logo: this is an interactive CLI, and suppressing
// it for everyone would be a regression in the opposite direction.
func TestTextFormatStillPrintsTheLogo(t *testing.T) {
	t.Cleanup(log.TestOnlyResetSink)

	cfg := config.New()
	c := NewCmdConfigurator(zap.New(nil), cfg)
	cmd := &cobra.Command{Use: "report"}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	if err := cmd.ParseFlags([]string{"--format", "text"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	out := captureStdout(t, func() {
		if err := c.ValidateFlags(t.Context(), cmd); err != nil {
			t.Errorf("ValidateFlags: %v", err)
		}
	})
	// The ANSI logo emits one escape sequence per character, so match on a
	// glyph rather than on a word.
	if !strings.Contains(out, "▓") {
		t.Errorf("the logo disappeared from the default text mode:\n%q", out)
	}
}

// The logo and the version line go through log.PrimarySink(), not a hardcoded
// os.Stdout, so the stdout/stderr split is ONE mechanism rather than a per-
// banner suppression list every future machine-output mode has to remember to
// extend. With the logger redirected, nothing may reach stdout.
func TestPrintLogoFollowsTheLoggerSink(t *testing.T) {
	t.Cleanup(log.TestOnlyResetSink)

	// Default: the sink is stdout, so the logo lands there.
	onStdout := captureStdout(t, func() { PrintLogo(log.PrimarySink(), true) })
	if !strings.Contains(onStdout, "OPEN SOURCE") {
		t.Fatalf("the logo did not reach stdout by default:\n%q", onStdout)
	}

	if _, err := log.RedirectToStderr(); err != nil {
		t.Fatalf("RedirectToStderr: %v", err)
	}
	afterRedirect := captureStdout(t, func() { PrintLogo(log.PrimarySink(), true) })
	if afterRedirect != "" {
		t.Fatalf("the logo still went to stdout after the logger moved to stderr; "+
			"a machine-readable document would be corrupted by it:\n%q", afterRedirect)
	}
}

// The flag-error path is console output like every other banner, so it follows
// the logger's sink too.
//
// It is an error path with a non-zero exit, so no machine consumer was going
// to parse what it corrupts — but color.Red writes to fatih/color's own
// package-level Output, which snapshots os.Stdout at init and therefore cannot
// follow the sink at all. Leaving one writer outside the mechanism turns the
// stdout/stderr split back into a suppression list that every new console
// print has to remember to join, which is how `--format json|junit` came to
// print the logo over its own NDJSON in the first place.
func TestFlagErrorFollowsTheLoggerSink(t *testing.T) {
	t.Cleanup(log.TestOnlyResetSink)

	c := NewCmdConfigurator(zap.NewNop(), config.New())
	cmd := &cobra.Command{Use: "test"}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}

	const flagErr = "unknown flag: --not-a-real-flag"
	var onStdout string
	onStderr := captureStderr(t, func() {
		// Redirected INSIDE the capture so the sink picks up the pipe, the
		// way `--format json` moves it before anything prints.
		if _, err := log.RedirectToStderr(); err != nil {
			t.Fatalf("RedirectToStderr: %v", err)
		}
		onStdout = captureStdout(t, func() {
			_ = cmd.FlagErrorFunc()(cmd, errors.New(flagErr))
		})
	})

	if onStdout != "" {
		t.Errorf("the flag-error path wrote to stdout with the logger on stderr:\n%q", onStdout)
	}
	if !strings.Contains(onStderr, flagErr) {
		t.Errorf("the flag error never reached the logger's sink, so it went to a stdout this "+
			"process had already declared machine-readable:\n%q", onStderr)
	}
}
