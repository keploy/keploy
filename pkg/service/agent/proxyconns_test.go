package agent

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/acceptcount"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// connHooks count the connections completed to the proxy, as the Linux hooks
// do.
type connHooks struct {
	loadedHooks
	established uint64
	err         error
	targets     []netip.AddrPort
}

func (h *connHooks) ProxyConnsEstablished(context.Context) (uint64, error) {
	return h.established, h.err
}
func (h *connHooks) ProxyConnTargets() []netip.AddrPort { return h.targets }

// acceptingProxy counts its accepts, as the proxy does.
type acceptingProxy struct {
	coreAgent.Proxy
	acceptcount.Counter
}

func (p *acceptingProxy) AcceptedTo(t []netip.AddrPort) uint64 { return p.To(t) }
func (p *acceptingProxy) WaitAcceptedTo(ctx context.Context, t []netip.AddrPort, n uint64) error {
	return p.WaitTo(ctx, t, n)
}

func tcpAt(s string) net.Addr { return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s)) }

// The agent answers the kernel's count and the proxy's accepts at the same
// addresses, the hooks' targets. Either side missing is "unknown", never 0:
// a 0 would let a test's end pass with connections still in the accept queue.
func TestProxyConnsAreTheHooksCountAndTheProxysAcceptsAtTheSameAddresses(t *testing.T) {
	ctx := context.Background()
	targets := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:16789"), netip.MustParseAddrPort("[::1]:16789")}
	proxy := &acceptingProxy{}
	proxy.Add(tcpAt("127.0.0.1:16789"))
	proxy.Add(tcpAt("[::1]:16789"))
	proxy.Add(tcpAt("127.0.0.2:16789")) // an address the hooks do not redirect to
	a := &Agent{logger: zap.NewNop(), Hooks: &connHooks{established: 5, targets: targets}, Proxy: proxy}
	got, err := a.ProxyConns(ctx)
	if err != nil || got != (models.ProxyConnCounts{Established: 5, Accepted: 2}) {
		t.Fatalf("ProxyConns = %+v, %v; want Established 5, Accepted 2 (only the accepts at the hooks' targets)", got, err)
	}
	if got.Waiting() != 3 {
		t.Fatalf("Waiting() = %d, want 3", got.Waiting())
	}

	for _, tc := range []struct {
		name string
		a    *Agent
	}{
		{"hooks that do not count", &Agent{logger: zap.NewNop(), Hooks: loadedHooks{}, Proxy: proxy}},
		{"a proxy that does not count", &Agent{logger: zap.NewNop(), Hooks: &connHooks{targets: targets}, Proxy: gracefulProxy{}}},
	} {
		if _, err := tc.a.ProxyConns(ctx); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("%s: ProxyConns error %v, want ErrProxyConnsUnsupported", tc.name, err)
		}
		if err := tc.a.WaitProxyAccepted(ctx, 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("%s: WaitProxyAccepted error %v, want ErrProxyConnsUnsupported", tc.name, err)
		}
	}

	// Hooks that count but were never told where the proxy is (not loaded):
	// an error, and not "unsupported", which callers may treat differently.
	notLoaded := &Agent{logger: zap.NewNop(), Hooks: &connHooks{}, Proxy: proxy}
	if _, err := notLoaded.ProxyConns(ctx); err == nil || errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("hooks with no targets: ProxyConns error %v, want a not-loaded error", err)
	}
	readFailed := errors.New("read proxy_conns_established: bad file descriptor")
	broken := &Agent{logger: zap.NewNop(), Hooks: &connHooks{err: readFailed, targets: targets}, Proxy: proxy}
	if _, err := broken.ProxyConns(ctx); !errors.Is(err, readFailed) {
		t.Fatalf("a failed read: ProxyConns error %v, want it", err)
	}
}

// WaitProxyAccepted returns at the accept that reaches the count, and not
// before, whatever else the listener accepts; and with ctx's error when ctx
// ends first.
func TestWaitProxyAcceptedWaitsForTheCountAtTheTargets(t *testing.T) {
	targets := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:16789")}
	proxy := &acceptingProxy{}
	a := &Agent{logger: zap.NewNop(), Hooks: &connHooks{established: 2, targets: targets}, Proxy: proxy}
	done := make(chan error, 1)
	go func() { done <- a.WaitProxyAccepted(context.Background(), 2) }()
	proxy.Add(tcpAt("127.0.0.1:16789"))
	proxy.Add(tcpAt("127.0.0.2:16789"))
	select {
	case err := <-done:
		t.Fatalf("WaitProxyAccepted returned %v with 1 of 2 accepted at the targets", err)
	default:
	}
	proxy.Add(tcpAt("127.0.0.1:16789"))
	if err := <-done; err != nil {
		t.Fatalf("WaitProxyAccepted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- a.WaitProxyAccepted(ctx, 10) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitProxyAccepted after cancel: %v, want context.Canceled", err)
	}
}
