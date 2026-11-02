package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// settingRow is one run of a verb: keploy.yml's body, the environment, the
// command line after the verb, and what the run has to leave in the config
// (or the refusal it has to end in). parent, when set, is the command the verb
// runs under (contract), and files are written into the working directory
// first.
type settingRow struct {
	name    string
	parent  string
	yml     string
	env     map[string]string
	files   map[string]string
	args    []string
	check   func(t *testing.T, cfg *config.Config)
	refused string
}

// runVerbWithKeployYml runs `keploy <verb>` as main does: config.New's
// defaults, the real flag set, keploy.yml read from --config-path, then
// PreProcessFlags and ValidateFlags. The temp dir is the working directory,
// and keploy/ and recordings/keploy/ exist in it.
func runVerbWithKeployYml(t *testing.T, verb string, row settingRow) {
	t.Helper()
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	viper.Reset()
	t.Cleanup(viper.Reset)
	for k, v := range row.env {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	for _, d := range []string{"keploy", filepath.Join("recordings", "keploy")} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(row.yml), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range row.files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.New()
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	cmd := &cobra.Command{Use: verb}
	if row.parent != "" {
		(&cobra.Command{Use: row.parent}).AddCommand(cmd)
	}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	if err := cmd.ParseFlags(append([]string{"--config-path", dir}, row.args...)); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if err := c.PreProcessFlags(cmd); err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}
	err := c.ValidateFlags(context.Background(), cmd)
	if row.refused != "" {
		if err == nil || !strings.Contains(err.Error(), row.refused) {
			t.Fatalf("ValidateFlags = %v, want a refusal naming %q", err, row.refused)
		}
		return
	}
	if err != nil {
		t.Fatalf("ValidateFlags: %v", err)
	}
	row.check(t, cfg)
}

func wantBool(name string, get func(*config.Config) bool, want bool) func(*testing.T, *config.Config) {
	return func(t *testing.T, cfg *config.Config) {
		t.Helper()
		if got := get(cfg); got != want {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
}

// keploy.yml's retryPassing, test.disableAutoHeaderNoise and
// record.globalPassthrough count on `keploy test` as their flags do, as do
// KEPLOY_RETRYPASSING and TEST_DISABLEAUTOHEADERNOISE; the flag, when given,
// wins, also set false. Each flag's default was read over them on every run.
func TestKeployTestKeepsKeployYmlSettings(t *testing.T) {
	retry := func(c *config.Config) bool { return c.RetryPassing }
	headers := func(c *config.Config) bool { return c.Test.DisableAutoHeaderNoise }
	global := func(c *config.Config) bool { return c.Record.GlobalPassthrough }
	run := []string{"-c", "python app.py"}
	for _, row := range []settingRow{
		{name: "retryPassing, no flag", yml: "retryPassing: true\n", args: run, check: wantBool("retryPassing", retry, true)},
		{name: "retryPassing, --retry-passing-test=false", yml: "retryPassing: true\n", args: append(run, "--retry-passing-test=false"), check: wantBool("retryPassing", retry, false)},
		{name: "retryPassing false, --retry-passing-test", yml: "retryPassing: false\n", args: append(run, "--retry-passing-test"), check: wantBool("retryPassing", retry, true)},
		{name: "no retryPassing, no flag", yml: "test:\n  delay: 6\n", args: run, check: wantBool("retryPassing", retry, false)},
		// KEPLOY_RETRYPASSING reaches retryPassing through AutomaticEnv, which
		// viper consults only for a key it knows of: in a build without the
		// viper_bind_struct tag (go test's), that takes keploy.yml naming it.
		{name: "KEPLOY_RETRYPASSING over retryPassing false, no flag", yml: "retryPassing: false\n", env: map[string]string{"KEPLOY_RETRYPASSING": "true"}, args: run, check: wantBool("retryPassing", retry, true)},
		{name: "KEPLOY_RETRYPASSING, --retry-passing-test=false", yml: "retryPassing: false\n", env: map[string]string{"KEPLOY_RETRYPASSING": "true"}, args: append(run, "--retry-passing-test=false"), check: wantBool("retryPassing", retry, false)},
		{name: "disableAutoHeaderNoise, no flag", yml: "test:\n  disableAutoHeaderNoise: true\n", args: run, check: wantBool("test.disableAutoHeaderNoise", headers, true)},
		{name: "disableAutoHeaderNoise, flag false", yml: "test:\n  disableAutoHeaderNoise: true\n", args: append(run, "--disableAutoHeaderNoise=false"), check: wantBool("test.disableAutoHeaderNoise", headers, false)},
		{name: "disableAutoHeaderNoise false, flag", yml: "test:\n  disableAutoHeaderNoise: false\n", args: append(run, "--disableAutoHeaderNoise"), check: wantBool("test.disableAutoHeaderNoise", headers, true)},
		{name: "TEST_DISABLEAUTOHEADERNOISE, no flag", yml: "test:\n  delay: 6\n", env: map[string]string{"TEST_DISABLEAUTOHEADERNOISE": "true"}, args: run, check: wantBool("test.disableAutoHeaderNoise", headers, true)},
		{name: "record.globalPassthrough, no flag", yml: "record:\n  globalPassthrough: true\n", args: run, check: wantBool("record.globalPassthrough", global, true)},
		{name: "record.globalPassthrough, flag false", yml: "record:\n  globalPassthrough: true\n", args: append(run, "--global-passthrough=false"), check: wantBool("record.globalPassthrough", global, false)},
		{name: "record.globalPassthrough false, flag", yml: "record:\n  globalPassthrough: false\n", args: append(run, "--global-passthrough"), check: wantBool("record.globalPassthrough", global, true)},
	} {
		t.Run(row.name, func(t *testing.T) { runVerbWithKeployYml(t, "test", row) })
	}
}

// keploy.yml's test.mustPass is the must-pass the replay runs with
// (Test.MustPass deletes the failing test cases), so the checks --must-pass
// gets apply to it too: three flakiness checks, no coverage, and a refusal
// without --test-sets. They used to look at the flag alone, so a committed
// `mustPass: true` deleted failing test cases in every test set, unchecked.
// keploy.yml's own test.selectedTests does not stand in for --test-sets: the
// test sets a must-pass run may delete from are named on the command line.
func TestKeployTestMustPassFromKeployYmlIsChecked(t *testing.T) {
	mustPassRun := func(want bool) func(*testing.T, *config.Config) {
		return func(t *testing.T, cfg *config.Config) {
			t.Helper()
			if cfg.Test.MustPass != want {
				t.Fatalf("test.mustPass = %v, want %v", cfg.Test.MustPass, want)
			}
			wantChecks, wantSkip := uint32(1), false
			if want {
				wantChecks, wantSkip = 3, true
			}
			if cfg.Test.MaxFlakyChecks != wantChecks || cfg.Test.SkipCoverage != wantSkip {
				t.Fatalf("maxFlakyChecks = %d, skipCoverage = %v; want %d, %v",
					cfg.Test.MaxFlakyChecks, cfg.Test.SkipCoverage, wantChecks, wantSkip)
			}
		}
	}
	run := []string{"-c", "python app.py"}
	for _, row := range []settingRow{
		{name: "mustPass, -t", yml: "test:\n  mustPass: true\n", args: append(run, "-t", "test-set-1"), check: mustPassRun(true)},
		{name: "mustPass, no -t", yml: "test:\n  mustPass: true\n", args: run, refused: "--test-sets"},
		{name: "TEST_MUSTPASS, no -t", yml: "test:\n  delay: 6\n", env: map[string]string{"TEST_MUSTPASS": "true"}, args: run, refused: "--test-sets"},
		{name: "mustPass, --must-pass=false", yml: "test:\n  mustPass: true\n", args: append(run, "--must-pass=false"), check: mustPassRun(false)},
		{name: "mustPass false, --must-pass, -t", yml: "test:\n  mustPass: false\n", args: append(run, "--must-pass", "-t", "test-set-1"), check: mustPassRun(true)},
		{name: "mustPass false, --must-pass, no -t", yml: "test:\n  mustPass: false\n", args: append(run, "--must-pass"), refused: "--test-sets"},
		{name: "mustPass and selectedTests, no -t", yml: "test:\n  mustPass: true\n  selectedTests:\n    test-set-1: []\n", args: run, refused: "--test-sets"},
		{name: "mustPass, maxFailAttempts 0", yml: "test:\n  mustPass: true\n", args: append(run, "-t", "test-set-1", "--max-fail-attempts", "0"), refused: "maxFailAttempts"},
		{name: "no mustPass", yml: "test:\n  delay: 6\n", args: run, check: mustPassRun(false)},
		{name: "no mustPass, maxFailAttempts 0", yml: "test:\n  delay: 6\n", args: append(run, "--max-fail-attempts", "0"), check: mustPassRun(false)},
	} {
		t.Run(row.name, func(t *testing.T) { runVerbWithKeployYml(t, "test", row) })
	}
}

// keploy.yml's record.globalPassthrough, opportunisticTlsIntercept, metadata
// and enableSampling count on `keploy record` as their flags do, as does the
// environment; the flag, when given, wins. Each flag's default was read over
// them on every run.
func TestKeployRecordKeepsKeployYmlSettings(t *testing.T) {
	global := func(c *config.Config) bool { return c.Record.GlobalPassthrough }
	otls := func(c *config.Config) bool { return c.Record.OpportunisticTLSIntercept }
	metadata := func(want string) func(*testing.T, *config.Config) {
		return func(t *testing.T, cfg *config.Config) {
			t.Helper()
			if cfg.Record.Metadata != want {
				t.Fatalf("record.metadata = %q, want %q", cfg.Record.Metadata, want)
			}
		}
	}
	sampling := func(want int) func(*testing.T, *config.Config) {
		return func(t *testing.T, cfg *config.Config) {
			t.Helper()
			if cfg.Record.EnableSampling != want {
				t.Fatalf("record.enableSampling = %d, want %d", cfg.Record.EnableSampling, want)
			}
		}
	}
	run := []string{"-c", "python app.py"}
	for _, row := range []settingRow{
		{name: "globalPassthrough, no flag", yml: "record:\n  globalPassthrough: true\n", args: run, check: wantBool("record.globalPassthrough", global, true)},
		{name: "globalPassthrough, flag false", yml: "record:\n  globalPassthrough: true\n", args: append(run, "--global-passthrough=false"), check: wantBool("record.globalPassthrough", global, false)},
		{name: "RECORD_GLOBALPASSTHROUGH, no flag", yml: "record:\n  metadata: \"\"\n", env: map[string]string{"RECORD_GLOBALPASSTHROUGH": "true"}, args: run, check: wantBool("record.globalPassthrough", global, true)},
		{name: "opportunisticTlsIntercept, no flag", yml: "record:\n  opportunisticTlsIntercept: true\n", args: run, check: wantBool("record.opportunisticTlsIntercept", otls, true)},
		{name: "opportunisticTlsIntercept, flag false", yml: "record:\n  opportunisticTlsIntercept: true\n", args: append(run, "--opportunistic-tls-intercept=false"), check: wantBool("record.opportunisticTlsIntercept", otls, false)},
		{name: "opportunisticTlsIntercept false, flag", yml: "record:\n  opportunisticTlsIntercept: false\n", args: append(run, "--opportunistic-tls-intercept"), check: wantBool("record.opportunisticTlsIntercept", otls, true)},
		{name: "RECORD_OPPORTUNISTICTLSINTERCEPT, no flag", yml: "record:\n  metadata: \"\"\n", env: map[string]string{"RECORD_OPPORTUNISTICTLSINTERCEPT": "true"}, args: run, check: wantBool("record.opportunisticTlsIntercept", otls, true)},
		{name: "metadata, no flag", yml: "record:\n  metadata: \"team=orders\"\n", args: run, check: metadata("team=orders")},
		{name: "metadata, flag", yml: "record:\n  metadata: \"team=orders\"\n", args: append(run, "--metadata", "team=billing"), check: metadata("team=billing")},
		{name: "RECORD_METADATA, no flag", yml: "record:\n  enableSampling: 0\n", env: map[string]string{"RECORD_METADATA": "team=orders"}, args: run, check: metadata("team=orders")},
		{name: "enableSampling, no flag", yml: "record:\n  enableSampling: 7\n", args: run, check: sampling(7)},
		{name: "enableSampling, flag", yml: "record:\n  enableSampling: 7\n", args: append(run, "--enable-sampling=3"), check: sampling(3)},
		{name: "enableSampling, bare flag", yml: "record:\n  enableSampling: 7\n", args: append(run, "--enable-sampling"), check: sampling(5)},
		{name: "RECORD_ENABLESAMPLING, no flag", yml: "record:\n  metadata: \"\"\n", env: map[string]string{"RECORD_ENABLESAMPLING": "7"}, args: run, check: sampling(7)},
		{name: "RECORD_ENABLESAMPLING, flag", yml: "record:\n  metadata: \"\"\n", env: map[string]string{"RECORD_ENABLESAMPLING": "7"}, args: append(run, "--enable-sampling=3"), check: sampling(3)},
		{name: "no enableSampling, no flag", yml: "record:\n  metadata: \"\"\n", args: run, check: sampling(0)},
	} {
		t.Run(row.name, func(t *testing.T) { runVerbWithKeployYml(t, "record", row) })
	}
}

// keploy.yml's normalize.allowHighRisk and NORMALIZE_ALLOWHIGHRISK count on
// `keploy normalize` as --allow-high-risk does; the flag, when given, wins,
// also set false.
func TestKeployNormalizeKeepsKeployYmlAllowHighRisk(t *testing.T) {
	risk := func(c *config.Config) bool { return c.Normalize.AllowHighRisk }
	for _, row := range []settingRow{
		{name: "allowHighRisk, no flag", yml: "normalize:\n  allowHighRisk: true\n", check: wantBool("normalize.allowHighRisk", risk, true)},
		{name: "allowHighRisk, flag false", yml: "normalize:\n  allowHighRisk: true\n", args: []string{"--allow-high-risk=false"}, check: wantBool("normalize.allowHighRisk", risk, false)},
		{name: "allowHighRisk false, flag", yml: "normalize:\n  allowHighRisk: false\n", args: []string{"--allow-high-risk"}, check: wantBool("normalize.allowHighRisk", risk, true)},
		{name: "NORMALIZE_ALLOWHIGHRISK, no flag", yml: "path: \".\"\n", env: map[string]string{"NORMALIZE_ALLOWHIGHRISK": "true"}, check: wantBool("normalize.allowHighRisk", risk, true)},
		{name: "NORMALIZE_ALLOWHIGHRISK, flag false", yml: "path: \".\"\n", env: map[string]string{"NORMALIZE_ALLOWHIGHRISK": "true"}, args: []string{"--allow-high-risk=false"}, check: wantBool("normalize.allowHighRisk", risk, false)},
		{name: "no allowHighRisk, no flag", yml: "path: \".\"\n", check: wantBool("normalize.allowHighRisk", risk, false)},
	} {
		t.Run(row.name, func(t *testing.T) { runVerbWithKeployYml(t, "normalize", row) })
	}
}

// keploy.yml's path and KEPLOY_PATH name the folder the recordings are in for
// record and test as --path does; --path, when given, wins.
func TestRecordAndTestKeepKeployYmlPath(t *testing.T) {
	for _, verb := range []string{"record", "test"} {
		for _, tc := range []struct {
			name string
			yml  string
			env  map[string]string
			args []string
			want string
		}{
			{name: "path, no flag", yml: "path: recordings\n", want: "recordings"},
			{name: "KEPLOY_PATH, no flag", yml: "test:\n  delay: 6\n", env: map[string]string{"KEPLOY_PATH": "recordings"}, want: "recordings"},
			{name: "path, -p .", yml: "path: recordings\n", args: []string{"-p", "."}, want: "."},
			{name: "no path, no flag", yml: "test:\n  delay: 6\n", want: "."},
		} {
			row := settingRow{name: tc.name, yml: tc.yml, env: tc.env, args: append([]string{"-c", "python app.py"}, tc.args...),
				check: func(t *testing.T, cfg *config.Config) {
					t.Helper()
					want, err := filepath.Abs(filepath.Join(tc.want, "keploy"))
					if err != nil {
						t.Fatal(err)
					}
					if cfg.Path != want {
						t.Fatalf("path = %q, want %q", cfg.Path, want)
					}
				}}
			t.Run(verb+": "+tc.name, func(t *testing.T) { runVerbWithKeployYml(t, verb, row) })
		}
	}
}

// keploy.yml's path and KEPLOY_PATH name the mock folder for `keploy mock
// record` and `keploy mock replay` as --path does; --path, when given, wins.
// The flag's default, read unguarded, put the mocks under ./keploy whatever
// keploy.yml said.
func TestMockVerbsKeepKeployYmlPath(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	t.Cleanup(viper.Reset)
	for _, verb := range []string{"record", "replay"} {
		for _, tc := range []struct {
			name string
			yml  string
			env  map[string]string
			args []string
			want string
		}{
			{name: "path, no flag", yml: "path: recordings\n", want: "recordings"},
			{name: "KEPLOY_PATH, no flag", yml: "mock:\n  name: orders\n", env: map[string]string{"KEPLOY_PATH": "recordings"}, want: "recordings"},
			{name: "path, -p .", yml: "path: recordings\n", args: []string{"-p", "."}, want: "."},
			{name: "no path, no flag", yml: "mock:\n  name: orders\n", want: "."},
		} {
			t.Run(verb+": "+tc.name, func(t *testing.T) {
				viper.Reset()
				for k, v := range tc.env {
					t.Setenv(k, v)
				}
				dir := t.TempDir()
				t.Chdir(dir)
				if err := os.MkdirAll(filepath.Join(dir, "recordings", "keploy"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(tc.yml), 0o644); err != nil {
					t.Fatal(err)
				}
				cmd, cfg, c := mockVerbCmd(t, dir, verb)
				if err := cmd.ParseFlags(append([]string{"-c", "python app.py"}, tc.args...)); err != nil {
					t.Fatal(err)
				}
				if err := c.PreProcessFlags(cmd); err != nil {
					t.Fatalf("PreProcessFlags: %v", err)
				}
				if err := c.ValidateFlags(context.Background(), cmd); err != nil {
					t.Fatalf("ValidateFlags: %v", err)
				}
				want, err := filepath.Abs(filepath.Join(tc.want, "keploy"))
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Path != want {
					t.Fatalf("path = %q, want %q", cfg.Path, want)
				}
			})
		}
	}
}

// wantFolder checks that the run keeps its recordings in <rel>/keploy, as an
// absolute path.
func wantFolder(name string, get func(*config.Config) string, rel string) func(*testing.T, *config.Config) {
	return func(t *testing.T, cfg *config.Config) {
		t.Helper()
		want, err := filepath.Abs(filepath.Join(rel, "keploy"))
		if err != nil {
			t.Fatal(err)
		}
		if got := get(cfg); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func wantStrings(name string, get func(*config.Config) []string, want ...string) func(*testing.T, *config.Config) {
	return func(t *testing.T, cfg *config.Config) {
		t.Helper()
		got := get(cfg)
		if len(got) != len(want) {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s = %q, want %q", name, got, want)
			}
		}
	}
}

func allOf(checks ...func(*testing.T, *config.Config)) func(*testing.T, *config.Config) {
	return func(t *testing.T, cfg *config.Config) {
		t.Helper()
		for _, check := range checks {
			check(t, cfg)
		}
	}
}

// pathRows are a verb's rows for keploy.yml's path: it names the folder the
// recordings are in as --path does, and so does KEPLOY_PATH; --path, when
// given, wins.
func pathRows(get func(*config.Config) string, args ...string) []settingRow {
	folder := func(rel string) func(*testing.T, *config.Config) { return wantFolder("path", get, rel) }
	with := func(more ...string) []string { return append(append([]string{}, args...), more...) }
	return []settingRow{
		{name: "path, no flag", yml: "path: recordings\n", args: with(), check: folder("recordings")},
		{name: "KEPLOY_PATH, no flag", yml: "inCi: false\n", env: map[string]string{"KEPLOY_PATH": "recordings"}, args: with(), check: folder("recordings")},
		{name: "path, -p .", yml: "path: recordings\n", args: with("-p", "."), check: folder(".")},
		{name: "no path, no flag", yml: "inCi: false\n", args: with(), check: folder(".")},
	}
}

// keploy.yml's path and report settings count on `keploy report` as their
// flags do, as does the environment where a flag binds one; the flag, when
// given, wins. Each flag's default was read over them on every run.
func TestKeployReportKeepsKeployYmlSettings(t *testing.T) {
	path := func(c *config.Config) string { return c.Path }
	summary := func(c *config.Config) bool { return c.Report.Summary }
	full := func(c *config.Config) bool { return c.Report.ShowFullBody }
	testCases := func(want ...string) func(*testing.T, *config.Config) {
		return wantStrings("report.testCaseIDs", func(c *config.Config) []string { return c.Report.TestCaseIDs }, want...)
	}
	testSets := func(want ...string) func(*testing.T, *config.Config) {
		return wantStrings("report.selectedTestSets", func(c *config.Config) []string {
			names := make([]string, 0, len(c.Report.SelectedTestSets))
			for name := range c.Report.SelectedTestSets {
				names = append(names, name)
			}
			return names
		}, want...)
	}
	reportPath := func(rel string) func(*testing.T, *config.Config) {
		return func(t *testing.T, cfg *config.Config) {
			t.Helper()
			want := ""
			if rel != "" {
				var err error
				if want, err = filepath.Abs(rel); err != nil {
					t.Fatal(err)
				}
			}
			if cfg.Report.ReportPath != want {
				t.Fatalf("report.reportPath = %q, want %q", cfg.Report.ReportPath, want)
			}
		}
	}
	runs := map[string]string{"run-1.yaml": "tests: []\n", "run-2.yaml": "tests: []\n"}
	rows := append(pathRows(path),
		settingRow{name: "reportPath, no flag", yml: "report:\n  reportPath: run-1.yaml\n", files: runs, check: reportPath("run-1.yaml")},
		settingRow{name: "reportPath, -r", yml: "report:\n  reportPath: run-1.yaml\n", files: runs, args: []string{"-r", "run-2.yaml"}, check: reportPath("run-2.yaml")},
		settingRow{name: "REPORT_REPORTPATH, no flag", yml: "inCi: false\n", files: runs, env: map[string]string{"REPORT_REPORTPATH": "run-1.yaml"}, check: reportPath("run-1.yaml")},
		settingRow{name: "reportPath and path, no flag", yml: "path: recordings\nreport:\n  reportPath: run-1.yaml\n", files: runs,
			check: allOf(reportPath("run-1.yaml"), wantFolder("path", path, "recordings"))},
		settingRow{name: "reportPath naming a folder", yml: "report:\n  reportPath: keploy\n", refused: "must point to a file"},
		settingRow{name: "summary, no flag", yml: "report:\n  summary: true\n", check: wantBool("report.summary", summary, true)},
		settingRow{name: "summary, --summary=false", yml: "report:\n  summary: true\n", args: []string{"--summary=false"}, check: wantBool("report.summary", summary, false)},
		settingRow{name: "REPORT_SUMMARY, no flag", yml: "inCi: false\n", env: map[string]string{"REPORT_SUMMARY": "true"}, check: wantBool("report.summary", summary, true)},
		settingRow{name: "showFullBody, no flag", yml: "report:\n  showFullBody: true\n", check: wantBool("report.showFullBody", full, true)},
		settingRow{name: "showFullBody, --full=false", yml: "report:\n  showFullBody: true\n", args: []string{"--full=false"}, check: wantBool("report.showFullBody", full, false)},
		settingRow{name: "showFullBody false, --full", yml: "report:\n  showFullBody: false\n", args: []string{"--full"}, check: wantBool("report.showFullBody", full, true)},
		settingRow{name: "testCaseIDs, no flag", yml: "report:\n  testCaseIDs: [test-1, test-2]\n", check: testCases("test-1", "test-2")},
		settingRow{name: "testCaseIDs, --test-case", yml: "report:\n  testCaseIDs: [test-1, test-2]\n", args: []string{"--test-case", "test-3, test-4"}, check: testCases("test-3", "test-4")},
		settingRow{name: "selectedTestSets, no flag", yml: "report:\n  selectedTestSets:\n    test-set-1: []\n", check: testSets("test-set-1")},
		settingRow{name: "selectedTestSets, -t", yml: "report:\n  selectedTestSets:\n    test-set-1: []\n", args: []string{"-t", "test-set-2"}, check: testSets("test-set-2")},
		settingRow{name: "no report settings, no flag", yml: "inCi: false\n", check: allOf(reportPath(""), wantBool("report.summary", summary, false),
			wantBool("report.showFullBody", full, false), testCases(), testSets())},
	)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) { runVerbWithKeployYml(t, "report", row) })
	}
}

// keploy.yml's path and KEPLOY_PATH name the folder `keploy diff` and `keploy
// sanitize` read as --path does; --path, when given, wins.
func TestDiffAndSanitizeKeepKeployYmlPath(t *testing.T) {
	path := func(c *config.Config) string { return c.Path }
	for _, verb := range []string{"diff", "sanitize"} {
		for _, row := range pathRows(path) {
			t.Run(verb+": "+row.name, func(t *testing.T) { runVerbWithKeployYml(t, verb, row) })
		}
	}
}

// keploy.yml's path and contract settings count on `keploy contract generate`,
// `download` and `test` as their flags do; the flag, when given, wins.
func TestContractVerbsKeepKeployYmlSettings(t *testing.T) {
	contractPath := func(c *config.Config) string { return c.Contract.Path }
	services := func(want ...string) func(*testing.T, *config.Config) {
		return wantStrings("contract.services", func(c *config.Config) []string { return c.Contract.Services }, want...)
	}
	tests := func(want ...string) func(*testing.T, *config.Config) {
		return wantStrings("contract.tests", func(c *config.Config) []string { return c.Contract.Tests }, want...)
	}
	driven := func(want string) func(*testing.T, *config.Config) {
		return func(t *testing.T, cfg *config.Config) {
			t.Helper()
			if cfg.Contract.Driven != want {
				t.Fatalf("contract.driven = %q, want %q", cfg.Contract.Driven, want)
			}
		}
	}
	servicesRows := []settingRow{
		{name: "services, no flag", yml: "contract:\n  services: [orders, billing]\n", check: services("orders", "billing")},
		{name: "services, -s", yml: "contract:\n  services: [orders, billing]\n", args: []string{"-s", "email"}, check: services("email")},
		{name: "no services, no flag", yml: "inCi: false\n", check: services()},
	}
	drivenRows := []settingRow{
		{name: "driven, no flag", yml: "contract:\n  driven: provider\n", check: driven("provider")},
		{name: "driven, --driven", yml: "contract:\n  driven: provider\n", args: []string{"--driven", "consumer"}, check: driven("consumer")},
	}
	verbs := map[string][]settingRow{
		"generate": append(append(pathRows(contractPath), servicesRows...),
			settingRow{name: "tests, no flag", yml: "contract:\n  tests: [test-1, test-2]\n", check: tests("test-1", "test-2")},
			settingRow{name: "tests, -t", yml: "contract:\n  tests: [test-1, test-2]\n", args: []string{"-t", "test-3"}, check: tests("test-3")},
			settingRow{name: "no tests, no flag", yml: "inCi: false\n", check: tests()},
		),
		"download": append(append([]settingRow{}, servicesRows...), drivenRows...),
		"test":     append(append(pathRows(contractPath), servicesRows...), drivenRows...),
	}
	for verb, rows := range verbs {
		for _, row := range rows {
			row.parent = "contract"
			t.Run(verb+": "+row.name, func(t *testing.T) { runVerbWithKeployYml(t, verb, row) })
		}
	}
}

// keploy.yml's normalize.selectedTests counts on `keploy normalize` as --tests
// does; --tests, when given, wins. The flag's default, read unguarded, cleared
// it on every run.
func TestKeployNormalizeKeepsKeployYmlSelectedTests(t *testing.T) {
	selected := func(want ...string) func(*testing.T, *config.Config) {
		return wantStrings("normalize.selectedTests", func(c *config.Config) []string {
			var got []string
			for _, s := range c.Normalize.SelectedTests {
				got = append(got, s.TestSet+":"+strings.Join(s.Tests, " "))
			}
			return got
		}, want...)
	}
	yml := "normalize:\n  selectedTests:\n    - testSet: test-set-1\n      tests: [test-1]\n"
	for _, row := range []settingRow{
		{name: "selectedTests, no flag", yml: yml, check: selected("test-set-1:test-1")},
		{name: "selectedTests, --tests", yml: yml, args: []string{"--tests", "test-set-2"}, check: selected("test-set-2:")},
		{name: "no selectedTests, no flag", yml: "inCi: false\n", check: selected()},
	} {
		t.Run(row.name, func(t *testing.T) { runVerbWithKeployYml(t, "normalize", row) })
	}
}
