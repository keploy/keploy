//go:build darwin

package proxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// canaryCounter is a darwin hooks' count of the proxy's connections: the
// listener is marked and each accepted connection numbered, or the numbering
// fails with err.
type canaryCounter struct {
	mu   sync.Mutex
	next uint64
	err  error
}

func (c *canaryCounter) MarkProxyListener(syscall.RawConn) error { return nil }

func (c *canaryCounter) ProxyConnPair(syscall.RawConn) (agent.ProxyConnPairing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return agent.ProxyConnPairing{}, c.err
	}
	c.next++
	return agent.ProxyConnPairing{Seq: c.next}, nil
}

// startCanaryProxy serves a proxy on an ephemeral port with counter c and
// returns it once it said it is ready; the test's cleanup stops it.
func startCanaryProxy(t *testing.T, c agent.ListenerCounter) *Proxy {
	t.Helper()
	cfg := config.New()
	cfg.ProxyPort = 0
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	p.SetListenerCounter(c)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() {
		cancel()
		<-done // the test binary's -timeout bounds it
	})
	if err := <-ready; err != nil {
		t.Fatalf("the proxy reported not ready: %v", err)
	}
	return p
}

// On macOS the proxy proves its count before it says it is ready, as on
// Linux: it dials its own listener, and the hooks number that connection, so
// the count reads 1 at the start.
func TestOnMacOSTheCanaryIsCountedBeforeTheProxyIsReady(t *testing.T) {
	c := &canaryCounter{}
	p := startCanaryProxy(t, c)
	if n, err := p.Accepted(); err != nil || n != 1 {
		t.Fatalf("Accepted() after the start = %d, %v; want the canary's 1", n, err)
	}
	if !p.CountedFromStart() {
		t.Fatal("the proxy whose canary was counted is not counted from its start")
	}
}

// A count the hooks cannot give (the canary's connection has no number) is
// unknown from the start on macOS too, never a settled count of none.
func TestOnMacOSABrokenCountIsUnknownFromTheStart(t *testing.T) {
	c := &canaryCounter{err: errors.New("an accepted connection has no sequence number")}
	p := startCanaryProxy(t, c)
	if n, err := p.Accepted(); !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "no sequence number") {
		t.Fatalf("Accepted() with a count the canary found broken = %d, %v; want unknown, naming why", n, err)
	}
	if p.CountedFromStart() {
		t.Fatal("a proxy whose canary found the count broken says it was counted from its start")
	}
}
