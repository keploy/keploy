//go:build linux

package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
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
	"go.uber.org/zap/zapcore"
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

// twoPairingHooks pair the first connection the proxy accepts after its
// canary with first, and every later one with later: what the kernel keeps
// in each connection's own socket.
type twoPairingHooks struct {
	*connHooks
	seq          atomic.Uint64
	first, later *coreAgent.NetworkAddress
}

func (h *twoPairingHooks) ProxyConnPair(syscall.RawConn) (coreAgent.ProxyConnPairing, error) {
	n := h.seq.Add(1)
	switch n {
	case 1: // the canary
		return coreAgent.ProxyConnPairing{Seq: n}, nil
	case 2:
		return coreAgent.ProxyConnPairing{Seq: n, AppSeq: n - 1, Dst: h.first}, nil
	}
	return coreAgent.ProxyConnPairing{Seq: n, AppSeq: n - 1, Dst: h.later}, nil
}

// portDest is the by-port map as the connect hook keeps it: one entry per
// source port, the last connect's, until the proxy deletes it.
type portDest struct {
	mu sync.Mutex
	m  map[uint16]*coreAgent.NetworkAddress
}

func (d *portDest) set(port int, a *coreAgent.NetworkAddress) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[uint16(port)] = a
}

func (d *portDest) Get(_ context.Context, port uint16) (*coreAgent.NetworkAddress, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.m[port]; ok {
		return a, nil
	}
	return nil, errors.New("no destination for the port")
}

func (d *portDest) Delete(_ context.Context, port uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.m, port)
	return nil
}

// handlerOrder is a log core that orders the proxy's connection handlers by
// what they say: the first to start waits, before it takes its destination,
// until the second has started, which the accept loop starts only once it
// has read the second connection's pairing. It notes the destination port
// each one routed to, and each one's end, its last word.
type handlerOrder struct {
	mu      sync.Mutex
	started int
	ports   []uint32
	held    chan struct{} // the first handler waits
	second  chan struct{} // the second has started
	free    sync.Once
	ended   chan struct{}
}

func newHandlerOrder() *handlerOrder {
	return &handlerOrder{held: make(chan struct{}), second: make(chan struct{}), ended: make(chan struct{}, 8)}
}

func (o *handlerOrder) release() { o.free.Do(func() { close(o.second) }) }

func (o *handlerOrder) Enabled(zapcore.Level) bool        { return true }
func (o *handlerOrder) With([]zapcore.Field) zapcore.Core { return o }
func (o *handlerOrder) Sync() error                       { return nil }
func (o *handlerOrder) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(e, o)
}

func (o *handlerOrder) Write(e zapcore.Entry, fields []zapcore.Field) error {
	switch e.Message {
	case "Inside handleConnection of proxyServer":
		o.mu.Lock()
		o.started++
		n := o.started
		o.mu.Unlock()
		if n == 1 {
			close(o.held)
			<-o.second
		} else {
			o.release()
		}
	case "time taken by proxy to execute the flow":
		o.ended <- struct{}{}
	}
	for _, f := range fields {
		if f.Key == "DestPort" {
			o.mu.Lock()
			o.ports = append(o.ports, uint32(f.Integer))
			o.mu.Unlock()
		}
	}
	return nil
}

// reuseDialer dials from port on 127.0.0.1, as a process does whose source
// port is the one another, reset, connection had.
func reuseDialer(port int) *net.Dialer {
	return &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			}); err != nil {
				return err
			}
			return serr
		},
	}
}

// Two connections to the proxy with the same two addresses, both waiting for
// it, each go where the kernel's pairing in their own socket says: X, reset
// by its application before the proxy got to it, and Y, which the
// application then made from X's source port. The accept loop reads X's
// pairing, then Y's, before X's handler takes X's destination (the order is
// held, not raced). Keyed by the two addresses, Y's pairing replaced X's, so
// X went to Y's destination, and Y, finding none, fell back to its source
// port, whose entry X's handler had deleted: Y was closed, the bug the
// pairing was meant to fix. With the switch off and on.
func TestTwoConnectionsWithOneFourTupleEachGoWhereTheirOwnPairingSays(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("proven="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			xDst, yDst := namedServer(t, "x"), namedServer(t, "y")
			h := &twoPairingHooks{connHooks: &connHooks{}, first: xDst, later: yDst}
			pm := &portDest{m: map[uint16]*coreAgent.NetworkAddress{}}
			order := newHandlerOrder()
			cfg := config.New()
			cfg.Agent.Mode = models.MODE_RECORD // with no session yet: a call passes through
			cfg.ProxyPort = 0
			p := proxyPkg.New(zap.New(order), pm, cfg)
			require.NoError(t, p.InitIntegrations(context.Background()))
			a := &Agent{logger: zap.NewNop(), Proxy: p, config: cfg, Hooks: h}
			a.markProxyListener()
			ctx, cancel := context.WithCancel(context.Background())
			ready, done := make(chan error, 1), make(chan error, 1)
			go func() { done <- p.Serve(ctx, ready) }()
			t.Cleanup(func() { cancel(); <-done })
			t.Cleanup(order.release) // first: a failed row leaves no handler held
			require.NoError(t, <-ready)
			a.latchCounting()
			proxyAddr := p.Listener.Addr().String()

			free, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			port := free.Addr().(*net.TCPAddr).Port
			require.NoError(t, free.Close())
			call := "GET / HTTP/1.1\r\nHost: dependency\r\nConnection: close\r\n\r\n"

			pm.set(port, xDst) // X's connect
			x, err := reuseDialer(port).Dial("tcp", proxyAddr)
			require.NoError(t, err)
			_, err = io.WriteString(x, call)
			require.NoError(t, err)
			select {
			case <-order.held: // X's handler is held before it routes
			case <-order.ended:
				t.Fatal("X's handler ended without being held: the row cannot order the handlers")
			}
			require.NoError(t, x.(*net.TCPConn).SetLinger(0))
			require.NoError(t, x.Close()) // X is reset
			pm.set(port, yDst)            // Y's connect, from X's source port
			y, err := reuseDialer(port).Dial("tcp", proxyAddr)
			require.NoError(t, err)
			require.Equal(t, x.LocalAddr().String(), y.LocalAddr().String(), "X and Y share their two addresses")
			_, err = io.WriteString(y, call)
			require.NoError(t, err)
			body := ""
			if resp, err := http.ReadResponse(bufio.NewReader(y), nil); err == nil { // no deadline but -timeout
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				body = string(b)
			}
			_ = y.Close() // Y's handler passes it through until Y ends
			<-order.ended
			<-order.ended
			order.mu.Lock()
			ports := append([]uint32(nil), order.ports...)
			order.mu.Unlock()
			require.Equal(t, "y", body, "Y did not reach the destination its own pairing names")
			require.ElementsMatch(t, []uint32{xDst.Port, yDst.Port}, ports,
				"X and Y were not each routed to their own destination (x %d, y %d)", xDst.Port, yDst.Port)
		})
	}
}
