package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// busyCheckingHook is an auxiliary hook whose teardown notes whether the
// agent's teardown reported it as work in progress while it ran.
type busyCheckingHook struct {
	sawBusy, ran bool
}

func (h *busyCheckingHook) AfterStart(context.Context, agent.Proxy) error { return nil }

func (h *busyCheckingHook) BeforeTeardown(context.Context) error {
	h.ran = true
	h.sawBusy = utils.TeardownBusy()
	return nil
}

// The auxiliary hook's teardown -- where an enterprise agent removes its TLS
// uprobes -- runs as teardown work in progress (utils.BeginTeardownWork): the
// CLI that stopped the agent takes no check made during it for one without
// progress, and so never kills the agent part way through a removal, which
// would leak the uprobe. The work ends with it.
func TestTheHooksTeardownIsWorkInProgress(t *testing.T) {
	h := &busyCheckingHook{}
	p := &Proxy{logger: zap.NewNop(), auxiliaryHook: h}
	before := utils.TeardownSteps()
	p.detachThenRemoveCA(context.Background())
	if !h.ran {
		t.Fatal("the auxiliary hook's teardown did not run")
	}
	if !h.sawBusy {
		t.Fatal("the auxiliary hook's teardown ran without being reported as work in progress")
	}
	if utils.TeardownBusy() {
		t.Fatal("the teardown is still reported as work in progress after it")
	}
	if steps := utils.TeardownSteps() - before; steps < 3 {
		t.Fatalf("the hook's teardown and the CA's removal recorded %d steps, want the work's start, its end, and one between them", steps)
	}
}

// The proxy's shutdown records its stages as steps of the agent's teardown,
// and the detach and the CA's removal as work in progress, which the CLI that
// stopped the agent reads: a teardown that records none is taken for one that
// has stopped making progress. Driven through start(), as the agent stops it.
func TestTheProxysShutdownRecordsItsSteps(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), skipListener: true}
	ctx, cancel := context.WithCancel(context.Background())
	readyChan := make(chan error, 1)
	startErr := make(chan error, 1)
	go func() { startErr <- p.start(ctx, readyChan) }()
	select {
	case err := <-readyChan:
		if err != nil {
			cancel()
			t.Fatalf("start() reported not-ready: %v", err)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		cancel()
		t.Fatal("start() never signalled readiness")
	}
	before := utils.TeardownSteps()
	cancel()
	select {
	case <-startErr:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("start() did not return after its context was cancelled")
	}
	// The flush's end, then the detach and the CA's removal: begun, between,
	// ended.
	if steps := utils.TeardownSteps() - before; steps < 4 {
		t.Fatalf("the proxy's shutdown recorded %d teardown steps, want at least 4", steps)
	}
	if utils.TeardownBusy() {
		t.Fatal("the proxy's shutdown left teardown work in progress")
	}
}

// A proxy that cannot listen still removes what the agent installed before it
// tried: StartProxy installs this run's CA in the trust store (SetupCAForApp)
// before start() binds its listener, and a port in use -- a previous agent
// still around -- made start() return before its shutdown was set up, so the
// CA's removal (detachThenRemoveCA) never ran and the anchor stayed trusted.
func TestAProxyThatCannotListenStillRemovesWhatItInstalled(t *testing.T) {
	held, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	h := &busyCheckingHook{}
	p := &Proxy{logger: zap.NewNop(), auxiliaryHook: h, Port: uint32(held.Addr().(*net.TCPAddr).Port)}
	readyChan := make(chan error, 1)
	startErr := make(chan error, 1)
	go func() { startErr <- p.start(context.Background(), readyChan) }()
	var err1 error
	select {
	case err1 = <-startErr:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("start() did not return on a port in use")
	}
	if err1 == nil {
		t.Fatal("start() listened on a port in use")
	}
	if ready := <-readyChan; ready == nil {
		t.Fatal("start() reported ready on a port in use")
	}
	if !h.ran {
		t.Fatal("a proxy that could not listen returned without its teardown: the CA it installed stays in the trust store")
	}
	if utils.TeardownBusy() {
		t.Fatal("the failed start left teardown work in progress")
	}
}

// orderHook records the order its start and teardown run in; its start holds
// until release is closed (nil: at once).
type orderHook struct {
	mu      sync.Mutex
	order   []string
	entered chan struct{}
	release chan struct{}
}

func (h *orderHook) note(s string) {
	h.mu.Lock()
	h.order = append(h.order, s)
	h.mu.Unlock()
}

func (h *orderHook) AfterStart(context.Context, agent.Proxy) error {
	h.note("start")
	if h.entered != nil {
		close(h.entered)
	}
	if h.release != nil {
		<-h.release
	}
	h.note("started")
	return nil
}

func (h *orderHook) BeforeTeardown(context.Context) error {
	h.note("teardown")
	return nil
}

// parkedIn reports whether a goroutine is parked waiting for a mutex inside
// fn, by the state and stack of every goroutine.
func parkedIn(fn string) bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range bytes.Split(buf, []byte("\n\n")) {
		header, _, _ := bytes.Cut(g, []byte("\n"))
		if (bytes.Contains(header, []byte("sync.Mutex.Lock")) || bytes.Contains(header, []byte("semacquire"))) &&
			bytes.Contains(g, []byte(fn)) {
			return true
		}
	}
	return false
}

// The auxiliary hook's start never follows its teardown: a stop that reached
// StartProxy before it started the hook (the stop cancels the context the
// DNS servers wait on, but StartProxy went on) had start()'s shutdown run the
// hook's BeforeTeardown first, and the AfterStart after it attached what
// nothing removed before the exit. Now a start after the teardown does not
// run, and a teardown during a start waits for it.
func TestTheAuxiliaryHooksStartNeverFollowsItsTeardown(t *testing.T) {
	t.Run("a start after the teardown does not run", func(t *testing.T) {
		h := &orderHook{}
		p := &Proxy{logger: zap.NewNop(), auxiliaryHook: h}
		p.detachThenRemoveCA(context.Background())
		if err := p.runAuxStart(context.Background()); !errors.Is(err, errAuxTornDown) {
			t.Fatalf("a start after the teardown returned %v, want errAuxTornDown", err)
		}
		if got := strings.Join(h.order, ","); got != "teardown" {
			t.Fatalf("the hook ran %q, want its teardown alone", got)
		}
	})
	t.Run("a teardown during a start waits for it", func(t *testing.T) {
		h := &orderHook{entered: make(chan struct{}), release: make(chan struct{})}
		p := &Proxy{logger: zap.NewNop(), auxiliaryHook: h}
		started := make(chan error, 1)
		go func() { started <- p.runAuxStart(context.Background()) }()
		<-h.entered
		tornDown := make(chan struct{})
		go func() { p.detachThenRemoveCA(context.Background()); close(tornDown) }()
		for !parkedIn("detachThenRemoveCA") { // the teardown waits for the start
			select {
			case <-tornDown:
				t.Fatal("the hook's teardown ran while its start was still attaching")
			default:
			}
			runtime.Gosched()
		}
		// Meanwhile the start is teardown work in progress, where its
		// progress can be seen; the teardown waiting for it has begun no
		// work of its own, which would count as progress throughout the
		// wait where none can be seen.
		if utils.TeardownProgressSeen() != utils.TeardownBusy() {
			t.Fatalf("teardown work is in progress while the teardown waits for the hook's start: %v, want %v (where progress can be seen)",
				utils.TeardownBusy(), utils.TeardownProgressSeen())
		}
		close(h.release)
		if err := <-started; err != nil {
			t.Fatal(err)
		}
		<-tornDown
		if got := strings.Join(h.order, ","); got != "start,started,teardown" {
			t.Fatalf("the hook ran %q, want its start to end before its teardown", got)
		}
	})
}
