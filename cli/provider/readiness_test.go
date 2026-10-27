package provider

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func healthPollCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().Duration("health-poll-timeout", config.DefaultHealthPollTimeout, "")
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// Under the switch --health-poll-timeout is refused (U18); without it, or
// with the switch off, nothing changes.
func TestUnderTheSwitchTheHealthPollTimeoutFlagIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	err := refuseHealthPollTimeout(healthPollCmd(t, "--health-poll-timeout", "60s"))
	if err == nil || !strings.Contains(err.Error(), "--health-poll-timeout is not used with "+provenswitch.Env+" (U18)") || !strings.Contains(err.Error(), "readiness event") {
		t.Fatalf("refuseHealthPollTimeout = %v, want the U18 refusal", err)
	}
	if !errors.Is(err, utils.ErrUsage) {
		t.Fatalf("refusal %v is no usage error (exit 8)", err)
	}
	if err := refuseHealthPollTimeout(healthPollCmd(t)); err != nil {
		t.Fatalf("refuseHealthPollTimeout without the flag = %v", err)
	}
	t.Setenv(provenswitch.Env, "")
	if err := refuseHealthPollTimeout(healthPollCmd(t, "--health-poll-timeout", "60s")); err != nil {
		t.Fatalf("refuseHealthPollTimeout with the switch off = %v", err)
	}
}

// Under the switch keploy.yml's test.healthPollTimeout is ignored, with a
// notice when it is not the default keploy writes; with the switch off it
// stays.
func TestUnderTheSwitchTheHealthPollTimeoutKeyIsIgnored(t *testing.T) {
	for _, c := range []struct {
		set    time.Duration
		notice bool
	}{{90 * time.Second, true}, {config.DefaultHealthPollTimeout, false}, {0, false}} {
		t.Setenv(provenswitch.Env, "1")
		core, logs := observer.New(zap.WarnLevel)
		cfg := &config.Config{}
		cfg.Test.HealthPollTimeout = c.set
		dropHealthPollTimeout(zap.New(core), cfg)
		if cfg.Test.HealthPollTimeout != 0 {
			t.Errorf("%v: healthPollTimeout %v under the switch, want ignored (0)", c.set, cfg.Test.HealthPollTimeout)
		}
		if got := logs.FilterMessageSnippet("test.healthPollTimeout is ignored").Len() == 1; got != c.notice {
			t.Errorf("%v: notice %v, want %v", c.set, got, c.notice)
		}
	}
	t.Setenv(provenswitch.Env, "")
	cfg := &config.Config{}
	cfg.Test.HealthPollTimeout = 90 * time.Second
	dropHealthPollTimeout(zap.NewNop(), cfg)
	if cfg.Test.HealthPollTimeout != 90*time.Second {
		t.Fatalf("switch off: healthPollTimeout %v, want it kept", cfg.Test.HealthPollTimeout)
	}
}
