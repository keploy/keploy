package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/acceptcount"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// connHooks count the connections completed to the proxy's marked listener,
// as the Linux hooks do, and note what they were asked to mark.
type connHooks struct {
	loadedHooks
	mu          sync.Mutex
	established uint64
	err         error
	marked      int
}

func (h *connHooks) ProxyConnsEstablished(context.Context) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.established, h.err
}
func (h *connHooks) MarkProxyListener(syscall.RawConn) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.marked++
	return nil
}
func (h *connHooks) ProxyConnSeq(syscall.RawConn) (uint64, error) { return 0, errors.New("not used") }
func (h *connHooks) count(n uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.established = n
}

// acceptingProxy keeps its accepted connections' numbers, as the proxy does.
// unmarked says its listener is not the marked one; parked hears each wait,
// as it begins; onRead, when set, runs once, after the first Accepted.
type acceptingProxy struct {
	coreAgent.Proxy
	acceptcount.Counter
	unmarked error
	drops    uint64
	parked   chan uint64
	onRead   func()
}

func (p *acceptingProxy) Accepted() (uint64, error) {
	if p.unmarked != nil {
		return 0, p.unmarked
	}
	n, err := p.Counter.Accepted()
	if p.onRead != nil {
		f := p.onRead
		p.onRead = nil
		f()
	}
	return n, err
}
func (p *acceptingProxy) WaitAccepted(ctx context.Context, n uint64) error {
	if p.unmarked != nil {
		return p.unmarked
	}
	if p.parked != nil {
		p.parked <- n
	}
	return p.Counter.WaitAccepted(ctx, n)
}
func (p *acceptingProxy) ListenDrops() (uint64, error) {
	if p.unmarked != nil {
		return 0, p.unmarked
	}
	return p.drops, nil
}

// The agent answers the proxy's accepts and drops and the kernel's count.
// Either side missing, a listener that is not marked, or a kernel count below
// the accepts (a lost count) is "unknown", never 0: a 0 would let a test's end
// pass with connections still in the accept queue.
func TestProxyConnsAreTheHooksCountAndTheProxysAccepts(t *testing.T) {
	ctx := context.Background()
	proxy := &acceptingProxy{drops: 7}
	proxy.Add(1)
	proxy.Add(2)
	a := &Agent{logger: zap.NewNop(), Hooks: &connHooks{established: 5}, Proxy: proxy}
	got, err := a.ProxyConns(ctx)
	if err != nil || got != (models.ProxyConnCounts{Established: 5, Accepted: 2, ListenDrops: 7}) {
		t.Fatalf("ProxyConns = %+v, %v; want Established 5, Accepted 2, ListenDrops 7", got, err)
	}
	if got.Waiting() != 3 {
		t.Fatalf("Waiting() = %d, want 3", got.Waiting())
	}

	unmarked := fmt.Errorf("%w: marking it failed", models.ErrProxyConnsUnsupported)
	for _, tc := range []struct {
		name string
		a    *Agent
	}{
		{"hooks that do not count", &Agent{logger: zap.NewNop(), Hooks: loadedHooks{}, Proxy: proxy}},
		{"a proxy that does not count", &Agent{logger: zap.NewNop(), Hooks: &connHooks{}, Proxy: gracefulProxy{}}},
		{"a listener that is not marked", &Agent{logger: zap.NewNop(), Hooks: &connHooks{established: 5}, Proxy: &acceptingProxy{unmarked: unmarked}}},
		{"a kernel count below the accepts", &Agent{logger: zap.NewNop(), Hooks: &connHooks{established: 1}, Proxy: proxy}},
	} {
		if c, err := tc.a.ProxyConns(ctx); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("%s: ProxyConns = %+v, %v; want ErrProxyConnsUnsupported", tc.name, c, err)
		}
		if err := tc.a.WaitProxyAccepted(ctx, 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("%s: WaitProxyAccepted error %v, want ErrProxyConnsUnsupported", tc.name, err)
		}
	}

	readFailed := errors.New("read proxy_conns_established: bad file descriptor")
	parked := &acceptingProxy{}
	broken := &Agent{logger: zap.NewNop(), Hooks: &connHooks{err: readFailed}, Proxy: parked}
	if _, err := broken.ProxyConns(ctx); !errors.Is(err, readFailed) {
		t.Fatalf("a failed read: ProxyConns error %v, want it", err)
	}
	// A wait on a count that cannot be read does not begin.
	parked.parked = make(chan uint64)
	waited := make(chan error, 1)
	go func() { waited <- broken.WaitProxyAccepted(ctx, 1) }()
	select {
	case <-parked.parked:
		t.Fatal("WaitProxyAccepted waited on a count it could not read")
	case err := <-waited:
		if !errors.Is(err, readFailed) {
			t.Fatalf("a failed read: WaitProxyAccepted error %v, want it", err)
		}
	}
}

// The proxy's count is read before the kernel's: a connection the kernel
// counts and the proxy accepts between the two reads then only raises
// Established. Read the other way round, the accepts would pass the kernel's
// count and read as a lost count.
func TestProxyConnsReadsTheAcceptsFirst(t *testing.T) {
	hooks := &connHooks{established: 1}
	proxy := &acceptingProxy{}
	proxy.Add(1)
	proxy.onRead = func() { // between the two reads: number 2, counted and accepted
		hooks.count(2)
		proxy.Add(2)
	}
	a := &Agent{logger: zap.NewNop(), Hooks: hooks, Proxy: proxy}
	c, err := a.ProxyConns(context.Background())
	if err != nil || c.Accepted != 1 || c.Established != 2 {
		t.Fatalf("ProxyConns = %+v, %v; want Accepted 1, Established 2", c, err)
	}
}

// WaitProxyAccepted waits for exactly the connections a read covered: every
// number up to it. A number accepted ahead of an earlier one cannot end it,
// and a steady stream of later connections cannot hold it back.
func TestWaitProxyAcceptedWaitsForExactlyTheNumbersRead(t *testing.T) {
	hooks := &connHooks{established: 2}
	proxy := &acceptingProxy{parked: make(chan uint64, 1)}
	a := &Agent{logger: zap.NewNop(), Hooks: hooks, Proxy: proxy}
	c, err := a.ProxyConns(context.Background())
	if err != nil || c.Established != 2 {
		t.Fatalf("ProxyConns = %+v, %v", c, err)
	}
	done := make(chan error, 1)
	go func() { done <- a.WaitProxyAccepted(context.Background(), c.Established) }()
	if n := <-proxy.parked; n != 2 {
		t.Fatalf("waits for %d, want 2", n)
	}
	// Number 2 accepted ahead of 1, then a stream of later ones.
	proxy.Add(2)
	for seq := uint64(3); seq <= 1000; seq++ {
		hooks.count(seq)
		proxy.Add(seq)
	}
	select {
	case err := <-done:
		t.Fatalf("WaitProxyAccepted returned %v with number 1 not accepted", err)
	default:
	}
	proxy.Add(1)
	if err := <-done; err != nil {
		t.Fatalf("WaitProxyAccepted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- a.WaitProxyAccepted(ctx, 2000) }()
	<-proxy.parked
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitProxyAccepted after cancel: %v, want context.Canceled", err)
	}

	// The listener closing ends the wait with its error.
	closed := fmt.Errorf("%w: the proxy's listener has closed", models.ErrProxyConnsUnsupported)
	go func() { done <- a.WaitProxyAccepted(context.Background(), 2000) }()
	<-proxy.parked
	proxy.Fail(closed)
	if err := <-done; !errors.Is(err, closed) {
		t.Fatalf("WaitProxyAccepted across the listener's close: %v, want its error", err)
	}
}

// markingProxy can have its listener marked, and notes the order of the calls
// the agent makes.
type markingProxy struct {
	startedProxy
	calls *[]string
	mark  func(syscall.RawConn) error
}

func (p *markingProxy) SetListenerCounter(c coreAgent.ListenerCounter) {
	*p.calls = append(*p.calls, "mark")
	p.mark = c.MarkProxyListener
}
func (p *markingProxy) StartProxy(ctx context.Context, opts coreAgent.ProxyOptions) error {
	*p.calls = append(*p.calls, "start")
	return p.startedProxy.StartProxy(ctx, opts)
}

// The agent has its proxy counted by the hooks, before it
// starts the proxy, under the proven switch only, and only when the hooks can
// count: with the switch off the proxy is started exactly as before, its
// listener untouched.
func TestHookAsksTheProxyToMarkItsListenerUnderTheSwitchOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		proven string
		hooks  coreAgent.Hooks
		want   string
	}{
		{"switch off, counting hooks", "", &connHooks{}, "[start]"},
		{"switch on, counting hooks", "1", &connHooks{}, "[mark start]"},
		{"switch on, hooks that cannot count", "1", loadedHooks{}, "[start]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			var calls []string
			p := &markingProxy{calls: &calls}
			a := &Agent{logger: zap.NewNop(), Hooks: tc.hooks, Proxy: p, config: &config.Config{}}
			grp, ctx := errgroup.WithContext(context.Background())
			ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, grp))
			defer func() {
				cancel()
				_ = grp.Wait()
			}()
			if err := a.Hook(ctx, models.HookOptions{Mode: models.MODE_TEST}); err != nil {
				t.Fatalf("the agent did not start: %v", err)
			}
			if got := fmt.Sprint(calls); got != tc.want {
				t.Fatalf("the agent's calls to its proxy: %s, want %s", got, tc.want)
			}
			if p.mark == nil {
				return
			}
			// It is the hooks' mark the proxy calls.
			if err := p.mark(nil); err != nil {
				t.Fatal(err)
			}
			if h := tc.hooks.(*connHooks); h.marked != 1 {
				t.Fatalf("the proxy's mark reached the hooks %d times, want 1", h.marked)
			}
		})
	}
}
