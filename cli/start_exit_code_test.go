package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A `keploy record` or `keploy test` that could not build its service never
// started: it exits non-zero, with the code the failure is tagged with. It
// exited 0, so a run whose environment could not be prepared (a container
// engine that is not ready, a keploy.yml it refuses) read as a clean one.
func TestRecordAndTestThatCannotStartExitNonZero(t *testing.T) {
	for _, command := range []string{"record", "test"} {
		for _, tc := range []struct {
			name    string
			factory ServiceFactory
			want    int
		}{
			{"no service", agentSvcFactory{err: errors.New("invalid record.testCaseNaming in keploy.yml")}, utils.ExitKeployError},
			{"no service, tagged", agentSvcFactory{err: fmt.Errorf("%w: podman is not ready", utils.ErrEnvironmentUnsupported)}, utils.ExitEnvironmentUnsupported},
			{"not its service", agentSvcFactory{svc: struct{}{}}, utils.ExitKeployError},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if got := runRecordOrTest(t, ctx, command, tc.factory); got != tc.want {
					t.Fatalf("exit code %d, want %d", got, tc.want)
				}
			})
		}
	}
}

// A Ctrl+C while the service was being built is a stop, not a failure.
func TestRecordAndTestStoppedBeforeTheyStartExitZero(t *testing.T) {
	for _, command := range []string{"record", "test"} {
		t.Run(command, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if got := runRecordOrTest(t, ctx, command, agentSvcFactory{err: context.Canceled}); got != 0 {
				t.Fatalf("exit code %d, want 0", got)
			}
		})
	}
}

// runRecordOrTest runs the RunE of `keploy record` or `keploy test` against
// factory and returns the exit code the process would end with.
func runRecordOrTest(t *testing.T, ctx context.Context, command string, factory ServiceFactory) int {
	t.Helper()
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	build := Record
	if command == "test" {
		build = Test
	}
	cmd := build(ctx, zap.NewNop(), &config.Config{}, factory, noFlags{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned %v; the command reports failure through its exit code", err)
	}
	return utils.ErrCode
}
