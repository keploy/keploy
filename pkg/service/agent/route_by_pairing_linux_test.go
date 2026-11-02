//go:build linux

package agent

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// pairingHooks are connHooks whose kernel pairs every connection the proxy
// accepts after its canary with an application connection going to dst, as
// the connect hook recorded it in that connection's own socket.
type pairingHooks struct {
	*connHooks
	seq atomic.Uint64
	dst *coreAgent.NetworkAddress
}

func (h *pairingHooks) ProxyConnPair(syscall.RawConn) (coreAgent.ProxyConnPairing, error) {
	n := h.seq.Add(1)
	if n == 1 {
		return coreAgent.ProxyConnPairing{Seq: n}, nil
	}
	return coreAgent.ProxyConnPairing{Seq: n, AppSeq: n - 1, Dst: h.dst}, nil
}

// reusedPortDest is the by-port map after another connection reused the
// application's source port: every port names that other connection's
// destination.
type reusedPortDest struct{ dst *coreAgent.NetworkAddress }

func (d reusedPortDest) Get(context.Context, uint16) (*coreAgent.NetworkAddress, error) {
	return d.dst, nil
}
func (reusedPortDest) Delete(context.Context, uint16) error { return nil }

// namedServer answers every HTTP call with its name; its address as the
// hooks write it (host-order IPv4, ToIP4AddressStr).
func namedServer(t *testing.T, name string) *coreAgent.NetworkAddress {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	ap := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://"))
	b := ap.Addr().As4()
	return &coreAgent.NetworkAddress{Version: 4, IPv4Addr: uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), Port: uint32(ap.Port())}
}

// callVia makes one HTTP call through the proxy at addr and returns the
// body, "" when the proxy closed the connection instead.
func callVia(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: dependency\r\nConnection: close\r\n\r\n"); err != nil {
		return ""
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil) // no deadline but -timeout
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// With the proven switch off, and an agent that does not settle reads (any
// keploy record or test run), each connection to the proxy goes where the
// kernel's pairing in its own socket says, never where the by-port map says:
// the application's source port, which the map is keyed by, may have been
// reused by another process's connection while this one waited for the
// proxy, and the map then names that other connection's destination. Before,
// only an agent under the switch or settling reads had its listener marked,
// so every other run routed by source port and sent the call to the other
// connection's dependency. The bytes on the paired connections are not held
// (no read waits for them), so nothing says the agent cannot count them.
func TestAnAgentRoutesEachConnectionByItsOwnPairingWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	h := &pairingHooks{connHooks: &connHooks{}, dst: namedServer(t, "own")}
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD // with no session yet: a call passes through
	cfg.ProxyPort = 0
	core, logs := observer.New(zap.InfoLevel)
	p := proxyPkg.New(zap.New(core), reusedPortDest{dst: namedServer(t, "other")}, cfg)
	require.NoError(t, p.InitIntegrations(context.Background()))
	a := &Agent{logger: zap.New(core), Proxy: p, config: cfg, Hooks: h}
	a.markProxyListener()
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	require.NoError(t, <-ready)
	a.latchCounting() // as Hook does once the proxy is up

	for i := 0; i < 2; i++ {
		require.Equal(t, "own", callVia(t, p.Listener.Addr().String()),
			"call %d went where the by-port map said, not where its own socket's pairing says", i)
	}
	require.Equal(t, 1, h.marked, "the proxy's listener is marked for the kernel's pairing")
	require.False(t, a.settlesReads(), "an agent that does not settle reads still reads its misses at once")
	require.Empty(t, p.CaptureLosses(), "routed calls are no capture loss")
	// The one WARN is the proxy's for a call passed through before any
	// session; routing by the pairing adds none.
	warns := logs.FilterLevelExact(zap.WarnLevel).Filter(func(e observer.LoggedEntry) bool {
		return !strings.Contains(e.Message, "no session yet")
	})
	require.Zero(t, warns.Len(), "routing by the pairing warned: %v", warns.All())
}
