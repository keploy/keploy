package agent

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// failingHooks fails the eBPF load the way the kernel failed it.
type failingHooks struct {
	coreAgent.Hooks // embedded: nil, so any other call panics loudly
	err             error
}

func (f failingHooks) Load(context.Context, coreAgent.HookCfg, config.Agent) error { return f.err }

// What the hook load failed on has to reach the agent's exit status: it is the
// only way the CLI that launched the agent learns whether to tell the user to
// grant privileges or to fix the environment. Hook replaced every cause with a
// fresh "failed to hook into the app", so all of them exited alike.
func TestHookKeepsWhatTheLoadFailedOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"privileges", fmt.Errorf("%w: failed to set memlock rlimit: %w", utils.ErrPrivilegeRequired, syscall.EPERM), utils.ExitPrivilegeRequired},
		{"environment", fmt.Errorf("%w: neither debugfs nor tracefs are mounted", utils.ErrEnvironmentUnsupported), utils.ExitEnvironmentUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{logger: zap.NewNop(), Hooks: failingHooks{err: tc.err}, config: &config.Config{}}
			grp, ctx := errgroup.WithContext(context.Background())
			ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, grp))
			defer func() {
				cancel()
				_ = grp.Wait()
			}()

			err := a.Hook(ctx, models.HookOptions{Mode: models.MODE_RECORD})
			if !errors.Is(err, tc.err) {
				t.Fatalf("Hook returned %q; the load's own error is gone", err)
			}
			if code := utils.ExitCodeFor(err); code != tc.want {
				t.Fatalf("an agent failing with %q would exit %d, want %d", err, code, tc.want)
			}
		})
	}
}
