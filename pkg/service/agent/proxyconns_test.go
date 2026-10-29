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
	// markErr is MarkProxyListener's answer: a mark the kernel refused.
	markErr error
	// The application's side: its count, and the numbers resolved.
	app        uint64
	appSettled acceptcount.Counter
	appParked  chan uint64
}

func (h *connHooks) AppConnsEstablished(context.Context) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.app, h.err
}
func (h *connHooks) AppConnsSettled() (uint64, error) { return h.appSettled.Accepted() }
func (h *connHooks) WaitAppConnsSettled(ctx context.Context, n uint64) error {
	if h.appParked != nil {
		h.appParked <- n
	}
	return h.appSettled.WaitAccepted(ctx, n)
}
func (h *connHooks) countApp(n uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.app = n
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
	return h.markErr
}
func (h *connHooks) ProxyConnPair(syscall.RawConn) (coreAgent.ProxyConnPairing, error) {
	return coreAgent.ProxyConnPairing{}, errors.New("not used")
}
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
	// bytesWaited hears each WaitPairedBytes, bytesErr is its answer.
	bytesWaited chan uint64
	bytesErr    error
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

// WaitPairedBytes answers bytesErr, and hears each wait's number on
// bytesWaited when that is set.
func (p *acceptingProxy) WaitPairedBytes(_ context.Context, n uint64) error {
	if p.bytesWaited != nil {
		p.bytesWaited <- n
	}
	return p.bytesErr
}
func (p *acceptingProxy) PairedBytesProgress() uint64 { return 0 }
func (p *acceptingProxy) ListenDrops() (models.ListenDrops, error) {
	if p.unmarked != nil {
		return models.ListenDrops{}, p.unmarked
	}
	return models.ListenDrops{Listener: p.drops, Net: 2, NetKnown: true}, nil
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
	hooks := &connHooks{established: 5, app: 4}
	hooks.appSettled.Add(1)
	hooks.appSettled.Add(2)
	hooks.appSettled.Add(3)
	a := &Agent{logger: zap.NewNop(), Hooks: hooks, Proxy: proxy}
	got, err := a.ProxyConns(ctx)
	if err != nil || got != (models.ProxyConnCounts{Established: 5, Accepted: 2, ListenDrops: models.ListenDrops{Listener: 7, Net: 2, NetKnown: true}, AppConns: 4, AppSettled: 3}) {
		t.Fatalf("ProxyConns = %+v, %v; want Established 5, Accepted 2, ListenDrops 7 (2 in the namespace), AppConns 4, AppSettled 3", got, err)
	}
	lostApp := &connHooks{established: 5, app: 1}
	lostApp.appSettled.Add(1)
	lostApp.appSettled.Add(2)
	if c, err := (&Agent{logger: zap.NewNop(), Hooks: lostApp, Proxy: proxy}).ProxyConns(ctx); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("more application connections settled than counted: ProxyConns = %+v, %v; want a lost count", c, err)
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

// WaitProxySettled waits first for every application connection the read
// counted to be settled, while its proxy side is not complete yet (in neither
// proxy-side count), then for the proxy to accept every connection completed
// to it by then. A lost one settles it too.
func TestWaitProxySettledWaitsForTheApplicationsConnections(t *testing.T) {
	hooks := &connHooks{app: 2, appParked: make(chan uint64, 1)}
	proxy := &acceptingProxy{parked: make(chan uint64, 1)}
	a := &Agent{logger: zap.NewNop(), Hooks: hooks, Proxy: proxy}
	c, err := a.ProxyConns(context.Background())
	if err != nil || c.AppConns != 2 || c.Established != 0 {
		t.Fatalf("ProxyConns = %+v, %v", c, err)
	}
	done := make(chan error, 1)
	go func() { done <- a.WaitProxySettled(context.Background(), c) }()
	if n := <-hooks.appParked; n != 2 {
		t.Fatalf("waits for the application's %d, want 2", n)
	}
	// The first one's proxy side completes and is accepted; the second is
	// lost. Neither proxy-side count saw either when the counts were read.
	hooks.count(1)
	proxy.Add(1)
	hooks.appSettled.Add(1)
	select {
	case err := <-done:
		t.Fatalf("WaitProxySettled returned %v with an application connection unsettled", err)
	case n := <-proxy.parked:
		t.Fatalf("WaitProxySettled went on to the proxy's accepts (%d) with an application connection unsettled", n)
	default:
	}
	hooks.appSettled.Add(2)
	if err := <-done; err != nil {
		t.Fatalf("WaitProxySettled: %v", err)
	}
}

// After the application's side settles, the proxy's side is waited on as far
// as the kernel has counted by then: a paired connection not yet accepted
// holds it, and later application connections do not.
func TestWaitProxySettledThenWaitsForThePairedConnectionsAccept(t *testing.T) {
	hooks := &connHooks{app: 1, appParked: make(chan uint64, 1)}
	proxy := &acceptingProxy{parked: make(chan uint64, 1)}
	a := &Agent{logger: zap.NewNop(), Hooks: hooks, Proxy: proxy}
	c, err := a.ProxyConns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Paired (its proxy side numbered 1) before the wait, not accepted.
	hooks.count(1)
	hooks.appSettled.Add(1)
	// Later application connections, never settled.
	hooks.countApp(50)
	done := make(chan error, 1)
	go func() { done <- a.WaitProxySettled(context.Background(), c) }()
	<-hooks.appParked
	if n := <-proxy.parked; n != 1 {
		t.Fatalf("waits for the proxy's %d, want 1", n)
	}
	proxy.Add(1)
	if err := <-done; err != nil {
		t.Fatalf("WaitProxySettled: %v", err)
	}
}

// Last, WaitProxySettled waits for the bytes on the paired connections the
// proxy holds (a pooled connection's call opens no connection), for the
// application connections the read counted, only once every one of them is
// settled and every paired one accepted; the proxy's answer is the wait's. A
// proxy that cannot wait for the bytes is unknown, never settled.
func TestWaitProxySettledLastWaitsForTheBytesOnThePairedConnections(t *testing.T) {
	hooks := &connHooks{app: 3, appParked: make(chan uint64, 1)}
	proxy := &acceptingProxy{parked: make(chan uint64, 1), bytesWaited: make(chan uint64, 1)}
	a := &Agent{logger: zap.NewNop(), Hooks: hooks, Proxy: proxy}
	c, err := a.ProxyConns(context.Background())
	if err != nil || c.AppConns != 3 {
		t.Fatalf("ProxyConns = %+v, %v", c, err)
	}
	hooks.countApp(9) // later connections: not the read's
	done := make(chan error, 1)
	go func() { done <- a.WaitProxySettled(context.Background(), c) }()
	<-hooks.appParked
	select {
	case n := <-proxy.bytesWaited:
		t.Fatalf("waited for the bytes on the paired connections up to %d with the application's connections unsettled", n)
	case err := <-done:
		t.Fatalf("WaitProxySettled returned %v with the application's connections unsettled", err)
	default:
	}
	hooks.count(1)
	hooks.appSettled.Add(1)
	hooks.appSettled.Add(2)
	hooks.appSettled.Add(3)
	if n := <-proxy.parked; n != 1 {
		t.Fatalf("waits for the proxy's %d, want 1", n)
	}
	select {
	case n := <-proxy.bytesWaited:
		t.Fatalf("waited for the bytes on the paired connections up to %d before the paired one was accepted", n)
	case err := <-done:
		t.Fatalf("WaitProxySettled returned %v before the paired connection was accepted", err)
	default:
	}
	proxy.Add(1)
	if n := <-proxy.bytesWaited; n != 3 {
		t.Fatalf("waited for the bytes on the paired connections up to %d, want the read's 3", n)
	}
	if err := <-done; err != nil {
		t.Fatalf("WaitProxySettled: %v", err)
	}

	// The proxy's answer is the wait's. (No one hears the waits from here.)
	hooks.appParked, proxy.parked, proxy.bytesWaited = nil, nil, nil
	lost := fmt.Errorf("%w: cannot tell", models.ErrProxyConnsUnsupported)
	proxy.bytesErr = lost
	if err := a.WaitProxySettled(context.Background(), c); !errors.Is(err, lost) {
		t.Fatalf("WaitProxySettled with the bytes unknown: %v, want the proxy's error", err)
	}
	// A proxy that cannot wait for them: unknown.
	a.Proxy = &acceptingProxyNoBytes{p: proxy}
	if err := a.WaitProxySettled(context.Background(), c); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("WaitProxySettled with a proxy that cannot wait for the bytes: %v, want unknown", err)
	}
}

// acceptingProxyNoBytes is an acceptingProxy without WaitPairedBytes.
type acceptingProxyNoBytes struct {
	coreAgent.Proxy
	p *acceptingProxy
}

func (p *acceptingProxyNoBytes) Accepted() (uint64, error) { return p.p.Accepted() }
func (p *acceptingProxyNoBytes) WaitAccepted(ctx context.Context, n uint64) error {
	return p.p.WaitAccepted(ctx, n)
}
func (p *acceptingProxyNoBytes) ListenDrops() (models.ListenDrops, error) {
	return p.p.ListenDrops()
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

// The agent has its proxy counted by the hooks, before it starts the proxy,
// under the proven switch or when it was started settling reads, and only
// when the hooks can count: otherwise the proxy is started exactly as before,
// its listener untouched.
func TestHookAsksTheProxyToMarkItsListenerUnderTheSwitchOrWhenSettlingReads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proven  string
		hooks   coreAgent.Hooks
		want    string
		settles bool
	}{
		{"switch off, counting hooks", "", &connHooks{}, "[start]", false},
		{"switch on, counting hooks", "1", &connHooks{}, "[mark start]", false},
		{"switch on, hooks that cannot count", "1", loadedHooks{}, "[start]", false},
		// An agent started for a run that learns noise (--settle-reads).
		{"switch off, settling reads, counting hooks", "", &connHooks{}, "[mark start]", true},
		{"switch off, settling reads, hooks that cannot count", "", loadedHooks{}, "[start]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			var calls []string
			p := &markingProxy{calls: &calls}
			cfg := &config.Config{}
			cfg.Agent.SettleReads = tc.settles
			a := &Agent{logger: zap.NewNop(), Hooks: tc.hooks, Proxy: p, config: cfg}
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

// losingHooks count, and have seen application connections lost before the
// proxy got them.
type losingHooks struct {
	countingHooks
	lost models.CaptureLosses
}

func (h losingHooks) CaptureLosses() models.CaptureLosses { return h.lost }

// An application connection lost before the proxy got it is one of the
// agent's capture losses, with the rest: a test that ends after it is not
// judged as if every call reached keploy.
func TestAnApplicationConnectionLostIsACaptureLoss(t *testing.T) {
	lost := models.CaptureLoss{Kind: models.CaptureLossAppConnLost, Count: 1, What: "the latest on local port 41000"}
	a := &Agent{logger: zap.NewNop(), Hooks: losingHooks{lost: models.CaptureLosses{lost}}, Proxy: gracefulProxy{}}
	got, err := a.CaptureLosses(context.Background())
	if err != nil || len(got) != 1 || got[0] != lost {
		t.Fatalf("CaptureLosses = %+v, %v; want the lost application connection", got, err)
	}
}

// countedMarkingProxy is a markingProxy that says whether the kernel counted
// its calls when it came up (agent.CountingReporter).
type countedMarkingProxy struct {
	markingProxy
	counted bool
}

func (p *countedMarkingProxy) CountedFromStart() bool { return p.counted }
func (p *countedMarkingProxy) CountingLost() (<-chan struct{}, error) {
	return nil, nil
}

// Hook latches, once the proxy is up, whether the kernel counts its calls: an
// agent started settling reads settles them only where its proxy was counted
// from its start, whatever the count says later.
func TestHookLatchesWhetherTheProxysCallsAreCounted(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, counted := range []bool{true, false} {
		var calls []string
		p := &countedMarkingProxy{markingProxy: markingProxy{calls: &calls}, counted: counted}
		cfg := &config.Config{}
		cfg.Agent.SettleReads = true
		a := &Agent{logger: zap.NewNop(), Hooks: &connHooks{}, Proxy: p, config: cfg}
		grp, ctx := errgroup.WithContext(context.Background())
		ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, grp))
		if err := a.Hook(ctx, models.HookOptions{Mode: models.MODE_TEST}); err != nil {
			t.Fatalf("the agent did not start: %v", err)
		}
		p.counted = !counted // what the count says after the start does not matter
		if got := a.settlesReads(); got != counted {
			t.Fatalf("proxy counted from its start %v: settlesReads %v", counted, got)
		}
		cancel()
		_ = grp.Wait()
	}
}
