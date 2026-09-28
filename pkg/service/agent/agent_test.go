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

// loadedHooks loads the way a kernel that allows it does, and says nothing
// about where the proxy is reached, as hooks built for other platforms do not.
type loadedHooks struct {
	coreAgent.Hooks // embedded: nil, so any other call panics loudly
}

func (loadedHooks) Load(context.Context, coreAgent.HookCfg, config.Agent) error { return nil }

// addressedHooks loads, and then says where the application reaches the
// proxy, as the Linux hooks do. Until it has loaded it says loopback, the Linux
// hooks' starting value, so an agent asking too early would get the wrong
// address for an agent started with --is-docker.
type addressedHooks struct {
	loadedHooks
	ip     string
	loaded bool
}

func (h *addressedHooks) Load(context.Context, coreAgent.HookCfg, config.Agent) error {
	h.loaded = true
	return nil
}

func (h *addressedHooks) ProxyIPv4() string {
	if !h.loaded {
		return "127.0.0.1"
	}
	return h.ip
}

// startedProxy keeps what the proxy was started with.
type startedProxy struct {
	coreAgent.Proxy // embedded: nil, so any other call panics loudly
	opts            *coreAgent.ProxyOptions
}

func (p *startedProxy) StartProxy(_ context.Context, opts coreAgent.ProxyOptions) error {
	p.opts = &opts
	return nil
}

// Hook installs coreAgent.ProxyHook when a build sets one; a no-op here keeps
// this test's subject the address, whatever another test left set.
func (p *startedProxy) SetAuxiliaryHook(coreAgent.AuxiliaryProxyHook) {}

// The proxy's DNS server answers a name it has no recorded answer for with the
// address the application reaches the proxy at, and the hooks are what know it.
// Hook asked the machine for a non-loopback address instead, for every agent,
// and so a native agent — reached over loopback — would not start on a machine
// with none: a laptop with its network off, where `keploy mock replay` exited 6
// instead of replaying.
func TestHookAnswersDNSWithWhereTheHooksReachTheProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hooks    coreAgent.Hooks
		isDocker bool
		want     string // "" = the proxy's loopback default
	}{
		{"hooks that do not say", loadedHooks{}, false, ""},
		{"native Linux hooks", &addressedHooks{ip: "127.0.0.1"}, false, "127.0.0.1"},
		{"hooks of an agent started with --is-docker", &addressedHooks{ip: "192.0.2.10"}, true, "192.0.2.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &startedProxy{}
			a := &Agent{logger: zap.NewNop(), Hooks: tc.hooks, Proxy: p, config: &config.Config{}}
			grp, ctx := errgroup.WithContext(context.Background())
			ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, grp))
			defer func() {
				cancel()
				_ = grp.Wait()
			}()

			if err := a.Hook(ctx, models.HookOptions{Mode: models.MODE_TEST, IsDocker: tc.isDocker}); err != nil {
				t.Fatalf("the agent did not start: %v", err)
			}
			if p.opts == nil {
				t.Fatal("the agent never started its proxy")
			}
			if p.opts.DNSIPv4Addr != tc.want {
				t.Fatalf("the proxy's DNS server answers with %q, want %q", p.opts.DNSIPv4Addr, tc.want)
			}
		})
	}
}
