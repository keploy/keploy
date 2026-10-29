package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keploy record and keploy test hand the agent (the kernel) only the
// port-only bypassRules' ports, the flag's among them, and keep the rules with
// a host or a path for the proxy. One with a host or a path on a mysqlPorts
// port, which no MySQL call can match, stops the command at start, named.
func TestScopedBypassRulesStayOutOfTheKernel(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	t.Cleanup(viper.Reset)

	const scoped = "bypassRules:\n  - host: api.example.com\n    port: 443\n  - port: 7003\n  - path: /v1/health\n    port: 8080\n"
	for _, command := range []string{"record", "test"} {
		for _, tc := range []struct {
			name      string
			yml       string
			args      []string
			wantPorts []uint
			wantRules int
			refused   string
		}{
			{"host and path rules", scoped, nil, []uint{7003}, 3, ""},
			{"host and path rules, and the flag", scoped, []string{"--pass-through-ports", "7002"}, []uint{7003, 7002}, 4, ""},
			{"a host rule on a mysql port", "mysqlPorts: [3307]\n" + scoped + "  - host: db.internal\n    port: 3307\n", nil, nil, 0, `{host: "db.internal", path: "", port: 3307}`},
			{"a port-only rule on a mysql port", "mysqlPorts: [3307]\nbypassRules:\n  - port: 3307\n", nil, []uint{3307}, 1, ""},
		} {
			t.Run(command+" "+tc.name, func(t *testing.T) {
				viper.Reset()
				dir := t.TempDir()
				t.Chdir(dir)
				if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(tc.yml), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(dir, "keploy"), 0o755); err != nil {
					t.Fatal(err)
				}
				cfg := config.New()
				c := NewCmdConfigurator(zap.NewNop(), cfg)
				root := &cobra.Command{Use: "keploy"}
				cmd := &cobra.Command{Use: command}
				root.AddCommand(cmd)
				if err := c.AddFlags(cmd); err != nil {
					t.Fatalf("AddFlags: %v", err)
				}
				args := append([]string{"--config-path", dir, "-c", "python app.py", "-p", dir}, tc.args...)
				if err := cmd.ParseFlags(args); err != nil {
					t.Fatalf("ParseFlags: %v", err)
				}
				if err := c.PreProcessFlags(cmd); err != nil {
					t.Fatalf("PreProcessFlags: %v", err)
				}
				err := c.ValidateFlags(context.Background(), cmd)
				if tc.refused != "" {
					if err == nil || !strings.Contains(err.Error(), tc.refused) || !strings.Contains(err.Error(), "{port: N}") {
						t.Fatalf("ValidateFlags = %v, want a refusal naming %s and the port-only rule", err, tc.refused)
					}
					return
				}
				if err != nil {
					t.Fatalf("ValidateFlags: %v", err)
				}
				if got := config.GetByPassPorts(cfg); !slices.Equal(got, tc.wantPorts) {
					t.Errorf("the kernel's ports = %v, want %v", got, tc.wantPorts)
				}
				if len(cfg.BypassRules) != tc.wantRules {
					t.Errorf("bypassRules = %+v, want %d of them for the proxy", cfg.BypassRules, tc.wantRules)
				}
				if tc.wantRules >= 3 && cfg.BypassRules[0] != (models.BypassRule{Host: "api.example.com", Port: 443}) {
					t.Errorf("the host rule did not reach the proxy as written: %+v", cfg.BypassRules[0])
				}
			})
		}
	}
}
