package provider

import (
	"fmt"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A command keploy cannot parse used to erase the container name the user
// supplied, because the assignment was unconditional. Everything downstream -
// the name-conflict pre-clean, teardown, app-log capture - then operated on ""
// and silently did nothing.
func TestResolveDockerNames(t *testing.T) {
	cases := []struct {
		name          string
		given         config.Config
		cont          string
		net           string
		wantContainer string
		wantNetwork   string
	}{
		{
			name:          "a parse that found nothing leaves the user's flags alone",
			given:         config.Config{ContainerName: "my-app", NetworkName: "my-net"},
			wantContainer: "my-app",
			wantNetwork:   "my-net",
		},
		{
			name:          "a successful parse wins, because docker names the container from the command",
			given:         config.Config{ContainerName: "my-app", NetworkName: "my-net"},
			cont:          "parsed-app",
			net:           "parsed-net",
			wantContainer: "parsed-app",
			wantNetwork:   "parsed-net",
		},
		{
			name:          "a parse fills in what the user did not supply",
			cont:          "parsed-app",
			net:           "parsed-net",
			wantContainer: "parsed-app",
			wantNetwork:   "parsed-net",
		},
		{
			name:          "each field is decided on its own",
			given:         config.Config{ContainerName: "my-app", NetworkName: "my-net"},
			net:           "parsed-net",
			wantContainer: "my-app",
			wantNetwork:   "parsed-net",
		},
		{
			name: "nothing given and nothing parsed stays empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.given
			resolveDockerNames(zap.NewNop(), &cfg, tc.cont, tc.net, nil)

			if cfg.ContainerName != tc.wantContainer {
				t.Errorf("ContainerName = %q, want %q", cfg.ContainerName, tc.wantContainer)
			}
			if cfg.NetworkName != tc.wantNetwork {
				t.Errorf("NetworkName = %q, want %q", cfg.NetworkName, tc.wantNetwork)
			}
		})
	}
}

// Several e2e lanes fail the build on any ERROR line, so the severity of a
// recoverable parse failure is a contract, not a detail. ParseDockerCmd
// reports a missing --network as an error even when the container parsed -
// the ordinary default-bridge shape - and that must not reach ERROR.
func TestResolveDockerNamesLogSeverity(t *testing.T) {
	parseErr := fmt.Errorf("failed to parse network name")

	cases := []struct {
		name      string
		given     config.Config
		cont      string
		net       string
		err       error
		wantError bool
	}{
		{
			name:      "container parsed, only the network missing",
			cont:      "my-app",
			err:       parseErr,
			wantError: false,
		},
		{
			name:      "command unparseable but the user supplied a container",
			given:     config.Config{ContainerName: "my-app"},
			err:       fmt.Errorf("failed to parse container name"),
			wantError: false,
		},
		{
			name:      "nothing parsed and nothing supplied",
			err:       fmt.Errorf("failed to parse container name"),
			wantError: true,
		},
		{
			name: "a clean parse says nothing at all",
			cont: "my-app",
			net:  "my-net",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := tc.given
			resolveDockerNames(zap.New(core), &cfg, tc.cont, tc.net, tc.err)

			errors := logs.FilterLevelExact(zapcore.ErrorLevel).Len()
			if (errors > 0) != tc.wantError {
				t.Fatalf("error-level entries = %d, wantError %v (all: %v)", errors, tc.wantError, logs.All())
			}
		})
	}
}
