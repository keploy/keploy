package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// optOutFactory records, at each service lookup (where every command pings
// telemetry and resolves its installation id), whether telemetry was off.
type optOutFactory struct {
	cfg  *config.Config
	seen map[string]bool
}

func (f *optOutFactory) GetService(_ context.Context, cmd string) (interface{}, error) {
	f.seen[cmd] = f.cfg.DisableTele
	return nil, errors.New("no services in a unit test")
}

// The four commands that run no Validate: through the real root, with the
// opt-out given as the root's --disable-tele and as KEPLOY_DISABLETELE, the
// lookup sees telemetry off, so it sends no Ping and persists no id.
func TestCommandsWithoutValidateHonorTheTelemetryOptOut(t *testing.T) {
	for _, tc := range []struct {
		name, service string
		args          []string
	}{
		{"config --generate", "config", []string{"config", "--generate", "--path", ""}},
		{"update", "update", []string{"update"}},
		{"export postman", "export", []string{"export", "postman"}},
		{"import postman", "import", []string{"import", "postman"}},
	} {
		for _, how := range []string{"flag", "env", "none"} {
			t.Run(tc.name+"/"+how, func(t *testing.T) {
				t.Setenv("KEPLOY_DISABLETELE", "")
				t.Chdir(t.TempDir())
				cfg := config.New()
				f := &optOutFactory{cfg: cfg, seen: map[string]bool{}}
				root := Root(context.Background(), zap.NewNop(), f, provider.NewCmdConfigurator(zap.NewNop(), cfg))
				root.SetOut(&bytes.Buffer{})
				root.SetErr(&bytes.Buffer{})
				args := append([]string(nil), tc.args...)
				if tc.service == "config" {
					args[3] = t.TempDir()
				}
				switch how {
				case "flag":
					args = append([]string{"--disable-tele"}, args...)
				case "env":
					t.Setenv("KEPLOY_DISABLETELE", "true")
				}
				root.SetArgs(args)
				_ = root.Execute()
				off, looked := f.seen[tc.service]
				if !looked {
					t.Fatalf("%s never looked its service up", tc.name)
				}
				if want := how != "none"; off != want {
					t.Fatalf("%s with opt-out %q: telemetry off at the lookup = %v, want %v", tc.name, how, off, want)
				}
			})
		}
	}
}
