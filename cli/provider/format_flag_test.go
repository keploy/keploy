package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// reportCmd builds a fully-flagged `keploy report` command the way the CLI
// does, so --format goes through the real registration and the real
// ValidateFlags arm.
func reportCmd(t *testing.T, cfg *config.Config, args ...string) (*CmdConfigurator, *cobra.Command) {
	t.Helper()
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	cmd := &cobra.Command{Use: "report"}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	return c, cmd
}

func TestReportFormatFlag_Validation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		// cfgFormat is what PreProcessFlags resolved from keploy.yml /
		// KEPLOY_REPORT_FORMAT before ValidateFlags runs.
		cfgFormat  string
		wantFormat string
		wantErr    []string
		// wantWarn are substrings the single config-fallback WARN must carry.
		wantWarn []string
	}{
		{name: "default is text", args: nil, wantFormat: "text"},
		{name: "text", args: []string{"--format", "text"}, wantFormat: "text"},
		{name: "junit", args: []string{"--format", "junit"}, wantFormat: "junit"},
		{name: "json is accepted", args: []string{"--format", "json"}, wantFormat: "json"},
		{name: "case and padding are normalised", args: []string{"--format", "  JSON "}, wantFormat: "json"},
		{name: "explicitly empty falls back to text", args: []string{"--format", ""}, wantFormat: "text"},
		{
			name:    "unknown value is rejected listing every valid one",
			args:    []string{"--format", "xml"},
			wantErr: []string{`invalid --format value "xml"`, "'text'", "'junit'", "'json'"},
		},
		{
			name:    "a near-miss is rejected too",
			args:    []string{"--format", "ndjson"},
			wantErr: []string{`invalid --format value "ndjson"`, "'json'"},
		},
		{
			// `report: {format: json}` in keploy.yml. This used to be
			// clobbered: ValidateFlags read the raw flag (still "text",
			// unchanged) and overwrote the resolved config value, so the
			// yaml/mapstructure key existed and did nothing.
			name: "the config value survives when no flag is passed",
			args: nil, cfgFormat: "json", wantFormat: "json",
		},
		{
			name: "a config value is normalised like a flag value",
			args: nil, cfgFormat: "  JUnit ", wantFormat: "junit",
		},
		{
			name: "an explicit flag beats the config value",
			args: []string{"--format", "text"}, cfgFormat: "json", wantFormat: "text",
		},
		{
			name: "an explicit flag beats the config value in the other direction too",
			args: []string{"--format", "json"}, cfgFormat: "text", wantFormat: "json",
		},
		{
			// SOURCE-DEPENDENT POLICY. `report.format` ships in every generated
			// keploy.yml and was DEAD until this release, so a user who set it
			// wrong got no feedback. Turning that into a hard exit 1 on upgrade
			// — with an error naming a flag they never passed — punishes
			// exactly the population most likely to have a stale value. Warn,
			// name the file, fall back to text, keep exiting 0.
			name: "an invalid CONFIG value warns and falls back to text",
			args: nil, cfgFormat: "yaml",
			wantFormat: "text",
			wantWarn:   []string{"report.format", "falling back to text", "yaml"},
		},
		{
			// ...but a value the user typed is a mistake worth stopping for:
			// they asked for something keploy cannot produce.
			name:    "an invalid FLAG value is still a hard error",
			args:    []string{"--format", "xml"},
			wantErr: []string{`invalid --format value "xml"`, "'json'"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ValidateFlags moves the package-level logger sink for the
			// machine formats; leave it as we found it.
			t.Cleanup(log.TestOnlyResetSink)

			cfg := config.New()
			cfg.Report.Format = tc.cfgFormat
			core, logs := observer.New(zapcore.WarnLevel)
			c, cmd := reportCmd(t, cfg, tc.args...)
			*c.logger = *zap.New(core)

			err := c.ValidateFlags(context.Background(), cmd)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("expected an error, got nil (format = %q)", cfg.Report.Format)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			if cfg.Report.Format != tc.wantFormat {
				t.Errorf("cfg.Report.Format = %q, want %q", cfg.Report.Format, tc.wantFormat)
			}

			warnings := logs.All()
			if len(tc.wantWarn) == 0 {
				if len(warnings) != 0 {
					t.Errorf("a valid invocation emitted %d warning(s): %v", len(warnings), warnings)
				}
				return
			}
			if len(warnings) != 1 {
				t.Fatalf("expected exactly one WARN, got %d: %v", len(warnings), warnings)
			}
			rendered := warnings[0].Message
			for _, f := range warnings[0].Context {
				rendered += " " + f.Key + "=" + f.String
			}
			for _, want := range tc.wantWarn {
				if !strings.Contains(rendered, want) {
					t.Errorf("warning %q does not mention %q", rendered, want)
				}
			}
		})
	}
}

func TestReportFormatFlagHelpListsJSON(t *testing.T) {
	_, cmd := reportCmd(t, config.New())
	flag := cmd.Flags().Lookup("format")
	if flag == nil {
		t.Fatal("format flag is not registered on `keploy report`")
	}
	// The knob's real contract, all of which a user has to be told: the values,
	// that stdout becomes a machine document for two of them, that keploy.yml
	// can set it, and which source wins.
	for _, want := range []string{"text", "junit", "json", "NDJSON", "stderr", "report.format", "this flag wins"} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("help text %q does not mention %q", flag.Usage, want)
		}
	}
}

// The dependency-assertion knob must exist, default to false, and say so.
func TestAssertDependenciesFlag(t *testing.T) {
	cfg := config.New()
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	cmd := &cobra.Command{Use: "test"}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}

	flag := cmd.Flags().Lookup("assert-dependencies")
	if flag == nil {
		t.Fatal("assert-dependencies flag is not registered on `keploy test`")
	}
	if flag.DefValue != "false" {
		t.Errorf("assert-dependencies default = %q, want \"false\" (backward compatibility)", flag.DefValue)
	}
	// The default is the whole point of the flag's contract; the help text has
	// to state it, and has to distinguish itself from --strict-failure.
	//
	// The eligibility caveat is pinned for a product reason, not a cosmetic
	// one: under default settings NO tag value makes an HTTP / Postgres /
	// MySQL / Generic mock per-test tier (DeriveLifetime's kind fallback and
	// its lax-mode promotion between them cover every tag), so for the majority
	// of recordings this flag cannot fail anything and every test reports
	// dependencies_checked=false. A user who turns it on in CI expecting a gate
	// and is never told that is the support thread this line exists to prevent.
	for _, want := range []string{
		"Default false",
		"--strict-failure",
		"per-test-tier",
		"dependencies_checked=false",
		"NOT CHECKED",
	} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("help text %q does not mention %q", flag.Usage, want)
		}
	}
	// ...and it must stay readable at a terminal. cobra does not wrap flag
	// usage, so an unbounded string pushes every later flag off screen. This
	// grew to 2087 characters once, which rendered as one line in a 94-line
	// help output. The cap is a budget, not a style rule: the full contract
	// lives in the config.Test.AssertDependencies doc comment and in the
	// runtime WARN, both of which a user reaches when it matters.
	if len(flag.Usage) > 1000 {
		t.Errorf("assert-dependencies help is %d characters; keep it under 1000 and put the "+
			"reasoning in the config doc comment instead", len(flag.Usage))
	}
	if cfg.Test.AssertDependencies {
		t.Error("config default for Test.AssertDependencies must be false")
	}
}
