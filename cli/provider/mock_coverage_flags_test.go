package provider

import (
	"math"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// replayCmd registers the replay flags through the real addMockFlags, the way
// the CLI does: while the config is still ZERO, before viper has read
// keploy.yml. The flag defaults are therefore zero, which is exactly the trap
// the guarded reads exist for.
func replayCmd(t *testing.T) (*cobra.Command, *config.Config, *CmdConfigurator) {
	t.Helper()
	cfg := &config.Config{}
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	cmd := &cobra.Command{Use: "replay"}
	if err := c.addMockFlags(cmd); err != nil {
		t.Fatal(err)
	}
	return cmd, cfg, c
}

// A floor committed in keploy.yml is the team's gate. The flag's zero default
// must not switch it off; an explicit flag must still override it.
func TestMockCoverageFlagsRespectKeployYml(t *testing.T) {
	t.Cleanup(viper.Reset)

	nan := math.NaN()
	for _, tc := range []struct {
		name       string
		fileMin    *float64
		fileReport string
		flags      map[string]string
		wantMin    float64
		wantReport string
		wantErr    bool
	}{
		{name: "keploy.yml floor survives the flag default", fileMin: ptr(80), wantMin: 80},
		{name: "an explicit flag overrides keploy.yml", fileMin: ptr(80), flags: map[string]string{"min-coverage": "60"}, wantMin: 60},
		{name: "flag alone", flags: map[string]string{"min-coverage": "70"}, wantMin: 70},
		{name: "report path from keploy.yml", fileReport: "build/cov.xml", wantReport: "build/cov.xml"},
		{name: "report path flag", flags: map[string]string{"coverage-report": "out/c.xml"}, wantReport: "out/c.xml"},
		{name: "a floor above 100 is refused", flags: map[string]string{"min-coverage": "101"}, wantErr: true},
		{name: "a negative floor is refused", fileMin: ptr(-1), wantErr: true},
		// Every comparison with NaN is false: a NaN floor would fail nothing.
		{name: "a NaN floor is refused", flags: map[string]string{"min-coverage": "NaN"}, wantErr: true},
		{name: "a NaN floor from keploy.yml is refused", fileMin: &nan, wantErr: true},
		{name: "an infinite floor is refused", flags: map[string]string{"min-coverage": "+Inf"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			cmd, cfg, c := replayCmd(t)
			// What viper's unmarshal of keploy.yml puts there, after AddFlags.
			if tc.fileMin != nil {
				viper.Set("mock.minCoverage", *tc.fileMin)
				cfg.Mock.MinCoverage = *tc.fileMin
			}
			if tc.fileReport != "" {
				viper.Set("mock.coverageReport", tc.fileReport)
				cfg.Mock.CoverageReport = tc.fileReport
			}
			for k, v := range tc.flags {
				if err := cmd.Flags().Set(k, v); err != nil {
					t.Fatal(err)
				}
			}
			err := c.readMockCoverageFlags(cmd)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted min-coverage %v", cfg.Mock.MinCoverage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mock.MinCoverage != tc.wantMin || cfg.Mock.CoverageReport != tc.wantReport {
				t.Fatalf("got min=%v report=%q, want min=%v report=%q", cfg.Mock.MinCoverage, cfg.Mock.CoverageReport, tc.wantMin, tc.wantReport)
			}
		})
	}
}

// The set is keploy/<name>/. A name that is not one directory either escapes
// keploy/ or nests the set where keploy/.gitignore's /*/ rules cannot reach.
func TestMockSetNameIsOneDirectory(t *testing.T) {
	for name, ok := range map[string]bool{
		"":              true, // → "default"
		"default":       true,
		"orders-v2.1_x": true,
		"team/payments": false,
		"../x":          false,
		"..":            false,
		"a\\b":          false,
		"/abs":          false,
	} {
		t.Run(strconv.Quote(name), func(t *testing.T) {
			cmd, cfg, c := replayCmd(t)
			if name != "" {
				if err := cmd.Flags().Set("name", name); err != nil {
					t.Fatal(err)
				}
			}
			err := c.readMockSetName(cmd)
			if ok && err != nil {
				t.Fatalf("refused %q: %v", name, err)
			}
			if !ok && err == nil {
				t.Fatalf("accepted %q as set %q", name, cfg.Mock.Name)
			}
			if name == "" && cfg.Mock.Name != "default" {
				t.Fatalf("empty name resolved to %q, want default", cfg.Mock.Name)
			}
		})
	}
}

func ptr(f float64) *float64 { return &f }

func TestMockSetNameRespectsKeployYml(t *testing.T) {
	for _, tc := range []struct {
		name, fromYml, flag, want string
	}{
		{name: "default", want: "default"},
		{name: "keploy.yml names the set", fromYml: "orders", want: "orders"},
		{name: "an explicit --name wins", fromYml: "orders", flag: "payments", want: "payments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Mock.Name = "default"
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			cmd := &cobra.Command{Use: "replay"}
			if err := c.addMockFlags(cmd); err != nil {
				t.Fatal(err)
			}
			if tc.fromYml != "" {
				cfg.Mock.Name = tc.fromYml
			}
			if tc.flag != "" {
				if err := cmd.Flags().Set("name", tc.flag); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.readMockSetName(cmd); err != nil {
				t.Fatal(err)
			}
			if cfg.Mock.Name != tc.want {
				t.Fatalf("set name = %q, want %q", cfg.Mock.Name, tc.want)
			}
		})
	}
}

// A run-only list committed in keploy.yml survives the flag's empty default;
// an explicit flag still replaces it.
func TestMockRunOnlyFlagRespectsKeployYml(t *testing.T) {
	t.Cleanup(viper.Reset)
	for _, tc := range []struct {
		name  string
		file  []string
		flags string
		want  []string
	}{
		{name: "keploy.yml list survives the flag default", file: []string{"TestA", "TestB"}, want: []string{"TestA", "TestB"}},
		{name: "an explicit flag overrides keploy.yml", file: []string{"TestA"}, flags: "TestC", want: []string{"TestC"}},
		// A parametrized pytest name carries commas: it is one test.
		{name: "a name with commas is one test", flags: "test_x[a,b]", want: []string{"test_x[a,b]"}},
		{name: "neither runs every test", want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			cmd, cfg, c := replayCmd(t)
			if tc.file != nil {
				viper.Set("mock.runOnly", tc.file)
				cfg.Mock.RunOnly = tc.file
			}
			if tc.flags != "" {
				if err := cmd.Flags().Set("run-only", tc.flags); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.readMockRunOnly(cmd); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Mock.RunOnly) != len(tc.want) {
				t.Fatalf("run-only %v, want %v", cfg.Mock.RunOnly, tc.want)
			}
			for i := range tc.want {
				if cfg.Mock.RunOnly[i] != tc.want[i] {
					t.Fatalf("run-only %v, want %v", cfg.Mock.RunOnly, tc.want)
				}
			}
		})
	}
}
