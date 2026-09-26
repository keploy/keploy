//go:build linux

package linux

import (
	"context"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// newTestHooks constructs a Hooks value that is safe to call GetProxyInfo on
// without loading any BPF programs. GetProxyInfo's non-docker branch only
// touches opts/cfg and the IPv4/IPv6 helper routines — it does not dereference
// the BPF maps or eBPF object handles — so an otherwise-zero Hooks is fine.
func newTestHooks(t *testing.T) *Hooks {
	t.Helper()
	return &Hooks{
		logger:       zap.NewNop(),
		proxyIP4:     "127.0.0.1",
		proxyIP6:     [4]uint32{0, 0, 0, 1},
		objectsMutex: sync.RWMutex{},
		m:            sync.Mutex{},
	}
}

// The address the agent hands the proxy's DNS server is where the kernel is
// told to send the application's connections. A native agent shares the
// application's network namespace, so that is loopback, whatever the machine's
// network is — none at all, on a laptop with its network off. An agent started
// with --is-docker is reached at its container's own address.
func TestProxyIPv4IsWhereTheKernelSendsTheApplication(t *testing.T) {
	for _, tc := range []struct {
		name     string
		isDocker bool
	}{
		{"native", false},
		{"--is-docker", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := "127.0.0.1"
			if tc.isDocker {
				ip, err := utils.GetContainerIPv4()
				if err != nil {
					t.Skipf("no non-loopback IPv4 address here for a container's agent to be reached at: %v", err)
				}
				want = ip
			}
			h := NewHooks(zap.NewNop(), &config.Config{})
			// Not the constructor's loopback: the native case has to show that
			// resolveProxyInfo wrote the address, not that nothing did.
			h.proxyIP4 = "192.0.2.99"

			info, err := h.resolveProxyInfo(context.Background(), config.Agent{IsDocker: tc.isDocker}, agent.HookCfg{IsDocker: tc.isDocker})
			if err != nil {
				t.Fatalf("the agent could not tell where its proxy is reached: %v", err)
			}
			if got := h.ProxyIPv4(); got != want {
				t.Fatalf("the proxy is reached at %q, want %q", got, want)
			}
			if redirect, _ := IPv4ToUint32(h.ProxyIPv4()); redirect != info.IP4 {
				t.Fatalf("ProxyIPv4 says %s, but the kernel is told to send connections to %08x", h.ProxyIPv4(), info.IP4)
			}
			if h.proxyIP6 != info.IP6 {
				t.Fatalf("the hooks log the IPv6 redirect target as %08x, but the kernel is told %08x", h.proxyIP6, info.IP6)
			}
		})
	}
}

func TestGetProxyInfo_NonDocker_ReturnsNonZeroIPv6(t *testing.T) {
	h := newTestHooks(t)

	// EnableIPv6Redirect is true by default on Config.New(); simulate that
	// here directly on a SetupOptions value.
	opts := config.Agent{
		SetupOptions: models.SetupOptions{
			IsDocker:           false,
			ProxyPort:          16789,
			EnableIPv6Redirect: true,
		},
	}
	cfg := agent.HookCfg{Pid: 0, IsDocker: false}

	info, err := h.GetProxyInfo(context.Background(), opts, cfg)
	if err != nil {
		t.Fatalf("GetProxyInfo returned error: %v", err)
	}

	zero := [4]uint32{}
	if info.IP6 == zero {
		t.Fatalf("expected non-zero IP6 when EnableIPv6Redirect=true, got %v", info.IP6)
	}

	// Should match ::ffff:127.0.0.1. The v4-mapped layout is:
	//   [0]=0, [1]=0, [2]=0x0000ffff, [3]=0x7f000001
	want, err := ToIPv4MappedIPv6("127.0.0.1")
	if err != nil {
		t.Fatalf("ToIPv4MappedIPv6 helper error: %v", err)
	}
	if info.IP6 != want {
		t.Fatalf("unexpected IP6 value: got %v, want %v", info.IP6, want)
	}

	if info.Port != opts.ProxyPort {
		t.Errorf("Port = %d, want %d", info.Port, opts.ProxyPort)
	}
}

func TestGetProxyInfo_NonDocker_ZeroWhenDisabled(t *testing.T) {
	h := newTestHooks(t)

	opts := config.Agent{
		SetupOptions: models.SetupOptions{
			IsDocker:           false,
			ProxyPort:          16789,
			EnableIPv6Redirect: false,
		},
	}
	// cfg.Port == 0 ensures the "explicit port override" branch does not
	// flip IP6 to non-zero on its own.
	cfg := agent.HookCfg{Pid: 0, IsDocker: false, Port: 0}

	info, err := h.GetProxyInfo(context.Background(), opts, cfg)
	if err != nil {
		t.Fatalf("GetProxyInfo returned error: %v", err)
	}

	zero := [4]uint32{}
	if info.IP6 != zero {
		t.Fatalf("expected zero IP6 when EnableIPv6Redirect=false and cfg.Port==0, got %v", info.IP6)
	}
}

func TestGetProxyInfo_NonDocker_CfgPortOverrideStillForcesV6(t *testing.T) {
	// Regression guard: even with the flag disabled, an explicit cfg.Port
	// override keeps the v4-mapped v6 address so the pre-existing behaviour
	// for per-app port overrides is preserved.
	h := newTestHooks(t)

	opts := config.Agent{
		SetupOptions: models.SetupOptions{
			IsDocker:           false,
			ProxyPort:          16789,
			EnableIPv6Redirect: false,
		},
	}
	cfg := agent.HookCfg{Pid: 0, IsDocker: false, Port: 17000}

	info, err := h.GetProxyInfo(context.Background(), opts, cfg)
	if err != nil {
		t.Fatalf("GetProxyInfo returned error: %v", err)
	}

	zero := [4]uint32{}
	if info.IP6 == zero {
		t.Fatalf("expected non-zero IP6 when cfg.Port override is set, got %v", info.IP6)
	}
	if info.Port != cfg.Port {
		t.Errorf("Port = %d, want %d", info.Port, cfg.Port)
	}
}
