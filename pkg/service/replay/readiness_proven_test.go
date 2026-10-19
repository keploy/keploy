package replay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/reportdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// manualWake replaces readyWake: each send on the returned channel is one
// wake-up; woken is signalled after the wait has weighed each check.
func manualWake(t *testing.T) chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	was := readyWake
	readyWake = func(ctx context.Context) (<-chan struct{}, func()) { return ch, func() {} }
	t.Cleanup(func() { readyWake = was })
	return ch
}

func stallLimit(t *testing.T, n int) {
	t.Helper()
	was := readyStallLimit
	readyStallLimit = n
	t.Cleanup(func() { readyStallLimit = was })
}

// fakeAgent answers AppReadiness from a script, one answer per call (the last
// repeats), counting the calls.
type fakeAgent struct {
	mu      sync.Mutex
	answers []models.AppReadiness
	errs    []error
	calls   int
	// moving adds a millisecond of CPU time at each call: a start going on.
	moving bool
	// since is what the last call asked from.
	since uint64
	// block, when set, holds every call until the caller gives up on it: an
	// agent that never answers.
	block bool
}

func (f *fakeAgent) AppReadiness(ctx context.Context, since uint64) (models.AppReadiness, error) {
	f.mu.Lock()
	if f.block {
		f.calls++
		f.mu.Unlock()
		<-ctx.Done()
		return models.AppReadiness{}, ctx.Err()
	}
	defer f.mu.Unlock()
	f.since = since
	i := min(f.calls, len(f.answers)-1)
	f.calls++
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	a := f.answers[i]
	if f.moving {
		a.CPU, a.CPUKnown = time.Duration(f.calls)*time.Millisecond, true
	}
	// The count of listens told, as an agent answers it: one past the
	// latest it holds, unless the answer says.
	for _, l := range a.Listens {
		if a.ListenCount <= l.Seq {
			a.ListenCount = l.Seq + 1
		}
	}
	return a, err
}

func (f *fakeAgent) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type waitResult struct {
	ready bool
	why   string
}

// tallies are the in-flight read counts of the waits a test started, by the
// channel their result comes on (readsWeighed).
var tallies sync.Map

// tallied registers tally as the reads in flight of the wait whose result
// comes on out.
func tallied(out chan waitResult, tally *readTally) chan waitResult {
	tallies.Store(out, tally)
	return out
}

func startWait(p provenReadiness) chan waitResult {
	if p.tally == nil {
		p.tally = new(readTally)
	}
	out := tallied(make(chan waitResult, 1), p.tally)
	go func() {
		ready, why := p.wait(context.Background())
		out <- waitResult{ready, why}
	}()
	return out
}

func testCfg() *config.Config {
	return &config.Config{}
}

// readsWeighed waits until the wait has started its read for wake-up woken
// (one at its start, one at each wake-up) and taken every read it started,
// or ended: a wake-up sent before is one the watchdog counts with the read's
// answer not yet in.
func readsWeighed(t *testing.T, res chan waitResult, woken int) {
	t.Helper()
	v, ok := tallies.Load(res)
	if !ok {
		return
	}
	tally := v.(*readTally)
	weighed := func() bool {
		started := tally.started.Load()
		return started >= int64(woken)+1 && tally.taken.Load() == started
	}
	for deadline := time.Now().Add(30 * time.Second); !weighed() && len(res) == 0; time.Sleep(100 * time.Microsecond) {
		if time.Now().After(deadline) {
			t.Fatal("a read of the agent never answered")
		}
	}
}

// wakeUntil sends wake-ups until the wait ends, at most n; it fails if it has
// not ended by then.
func wakeUntil(t *testing.T, wake chan struct{}, res chan waitResult, n int) waitResult {
	t.Helper()
	for i := 0; i <= n; i++ {
		readsWeighed(t, res, i)
		select {
		case r := <-res:
			return r
		case wake <- struct{}{}:
		case <-time.After(30 * time.Second):
			t.Fatalf("the wait neither ended nor took wake-up %d", i)
		}
	}
	select {
	case r := <-res:
		return r
	case <-time.After(30 * time.Second):
		t.Fatalf("the wait did not end within %d wake-ups", n)
		return waitResult{}
	}
}

// The application is ready once it listens, in the agent's namespace, on the
// port the first test dials; never before, however long that takes.
func TestUnderTheSwitchTheAppIsReadyWhenItListensWhereTheFirstTestDials(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	cpu := func(n int) models.AppReadiness {
		return models.AppReadiness{CPU: time.Duration(n) * time.Millisecond, CPUKnown: true}
	}
	elsewhere := cpu(40)
	elsewhere.Listens = []models.AppListen{{Port: 9090, Here: true, Live: true}, {Port: 8080, Here: false, Live: true, Seq: 1}}
	ready := cpu(50)
	ready.Listens = append(append([]models.AppListen(nil), elsewhere.Listens...), models.AppListen{Port: 8080, Here: true, Live: true, Seq: 2})
	a := &fakeAgent{answers: []models.AppReadiness{cpu(10), cpu(20), cpu(30), elsewhere, ready}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	r := wakeUntil(t, wake, res, 10)
	if !r.ready || r.why != "" {
		t.Fatalf("wait = %+v, want ready", r)
	}
	if a.count() != 5 {
		t.Fatalf("ready after %d reads, want 5: before its listen on :8080 here (another port, another namespace) it is not ready", a.count())
	}
}

// A start that makes no progress at all across the watchdog's count is not
// judged, naming the ports the application listens on.
func TestUnderTheSwitchAStartWithNoProgressIsNotJudgedNamingWhereItListens(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	idle := models.AppReadiness{CPU: 5 * time.Second, CPUKnown: true, Progress: models.ReadyProgress{Listens: 1, Connects: 2},
		Listens: []models.AppListen{{Port: 9090, Here: true, Live: true}, {Port: 7070, Here: false, Seq: 1}}}
	a := &fakeAgent{answers: []models.AppReadiness{idle}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	r := wakeUntil(t, wake, res, 10)
	if r.ready || !strings.Contains(r.why, "never listened on :8080") || !strings.Contains(r.why, "it listens on :9090") ||
		!strings.Contains(r.why, ":7070 in another network namespace") || !strings.Contains(r.why, "across 3 wake-ups") {
		t.Fatalf("wait = %+v, want not ready, naming :8080, :9090 and :7070", r)
	}
	if a.count() != 4 { // the first look, and one weighing each of three wake-ups
		t.Fatalf("stalled after %d reads, want 4", a.count())
	}
}

// A start that only computes (a JIT warming up, a migration run in-process),
// however starved: its CPU time moving is progress, so it never stalls.
func TestUnderTheSwitchAStarvedCPUOnlyBootIsNoStall(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	var answers []models.AppReadiness
	for i := 0; i < 30; i++ {
		// 10 ms of CPU every other wake-up: a process at a sliver of a CPU.
		answers = append(answers, models.AppReadiness{CPU: time.Duration(i/2) * 10 * time.Millisecond, CPUKnown: true})
	}
	done := answers[len(answers)-1]
	done.CPU += 10 * time.Millisecond
	done.Listens = []models.AppListen{{Port: 8080, Here: true, Live: true}}
	answers = append(answers, done)
	a := &fakeAgent{answers: answers}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	r := wakeUntil(t, wake, res, 40)
	if !r.ready {
		t.Fatalf("wait = %+v, want ready: a boot whose CPU time moves is going on", r)
	}
}

// Each of a listen, a connect, a fork and an exec is progress.
func TestUnderTheSwitchEachKindOfProgressKeepsTheWaitGoing(t *testing.T) {
	progress := func(p models.ReadyProgress) func(uint64) models.AppReadiness {
		return func(n uint64) models.AppReadiness {
			q := p
			q.Connects, q.Forks, q.Execs = q.Connects*n, q.Forks*n, q.Execs*n
			return models.AppReadiness{Progress: q}
		}
	}
	for _, c := range []struct {
		name string
		p    func(n uint64) models.AppReadiness
	}{
		// A listen on a socket address not listened on before in the set
		// (here closed again, a probe).
		{"listen", func(n uint64) models.AppReadiness {
			st := models.AppReadiness{Progress: models.ReadyProgress{Listens: n}}
			for k := uint64(0); k < n; k++ {
				st.Listens = append(st.Listens, models.AppListen{Port: uint16(9000 + k), Here: true, Seq: k})
			}
			return st
		}},
		{"connect", progress(models.ReadyProgress{Connects: 1})},
		{"fork", progress(models.ReadyProgress{Forks: 1})},
		{"exec", progress(models.ReadyProgress{Execs: 1})},
	} {
		t.Run(c.name, func(t *testing.T) {
			wake := manualWake(t)
			stallLimit(t, 2)
			var answers []models.AppReadiness
			for i := 0; i < 12; i++ {
				answers = append(answers, c.p(uint64(i/2))) // moves every other wake-up
			}
			last := c.p(6)
			last.Listens = append(last.Listens, models.AppListen{Port: 80, Here: true, Live: true, Seq: 100})
			answers = append(answers, last)
			res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: answers}, host: "localhost", port: "80", listen: 80})
			if r := wakeUntil(t, wake, res, 20); !r.ready {
				t.Fatalf("wait = %+v, want ready", r)
			}
		})
	}
}

// A read of the agent that fails (a transient failure, the agent replaced
// mid-wait) is a wake-up with no progress seen, never a readiness: a wait
// that only ever fails is a stall, naming the error.
func TestUnderTheSwitchAnAgentReadErrorIsNoProgressAndNamedAtTheStall(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	ok := models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}
	a := &fakeAgent{answers: []models.AppReadiness{{}, ok}, errs: []error{errors.New("boom")}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	if r := wakeUntil(t, wake, res, 10); !r.ready {
		t.Fatalf("wait = %+v, want ready after one failed read", r)
	}
	a = &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom")}}
	res = startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	r := wakeUntil(t, wake, res, 10)
	if r.ready || !strings.Contains(r.why, "could not tell whether the application is ready at the last read: boom") {
		t.Fatalf("wait = %+v, want not judged, naming the agent's error", r)
	}
}

// An agent that cannot watch at all (another platform's, an older one): the
// first test is sent as it is, and its own connect decides.
func TestUnderTheSwitchAnAgentThatCannotWatchLeavesItToTheFirstTestsAddress(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there yet
	host, port, _ := net.SplitHostPort(addr)
	a := &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{fmt.Errorf("%w: 404", models.ErrReadinessUnsupported)}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: host, port: port, listen: 1})
	for i := 0; i < 3; i++ {
		select {
		case r := <-res:
			t.Fatalf("wait = %+v before the address took a connect", r)
		case wake <- struct{}{}:
			time.Sleep(10 * time.Millisecond) // a dial refused in wall time
		}
	}
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("the port was taken meanwhile: %v", err)
	}
	defer ln.Close()
	var r waitResult
	for done := false; !done; {
		select {
		case r = <-res:
			done = true
		case wake <- struct{}{}:
			time.Sleep(10 * time.Millisecond)
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end")
		}
	}
	if !r.ready || a.count() != 1 {
		t.Fatalf("wait = %+v after %d reads, want ready once the address takes a connect, the agent asked once", r, a.count())
	}
}

// Once the application listens, its CPU time is no progress: up and idle on
// another port (a JVM's timers and GC use some now and then), it stalls,
// naming the port.
func TestUnderTheSwitchCPUTimeIsNoProgressOnceTheAppListens(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	idle := models.AppReadiness{Progress: models.ReadyProgress{Listens: 1, Restarts: 2}, Listens: []models.AppListen{{Port: 9090, Here: true, Live: true}}}
	a := &fakeAgent{answers: []models.AppReadiness{idle}, moving: true}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	r := wakeUntil(t, wake, res, 10)
	if r.ready || !strings.Contains(r.why, "never listened on :8080") || !strings.Contains(r.why, "it listens on :9090") {
		t.Fatalf("wait = %+v, want not judged, naming :9090, though its CPU time moved", r)
	}
	// Its restarts are said, not as "without listening": it listens.
	if !strings.Contains(r.why, "it restarted 2 time(s)") || strings.Contains(r.why, "without listening") {
		t.Fatalf("wait = %+v, want its two restarts said, not without listening", r)
	}
}

// In Docker mode a listen on loopback only is not where the published port
// reaches the application: not ready, and the stall says why.
func TestUnderTheSwitchADockerAppsLoopbackListenIsNoReadiness(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	loop := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "127.0.0.1", Here: true, Live: true}}}
	a := &fakeAgent{answers: []models.AppReadiness{loop}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8098", listen: 3000, docker: true})
	r := wakeUntil(t, wake, res, 10)
	if r.ready || !strings.Contains(r.why, "127.0.0.1:3000 only, which no published port reaches") {
		t.Fatalf("wait = %+v, want not judged, naming the loopback listen", r)
	}
	all := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "127.0.0.1", Here: true, Live: true}, {Port: 3000, Addr: "0.0.0.0", Here: true, Live: true, Seq: 1}}}
	a = &fakeAgent{answers: []models.AppReadiness{loop, all}, moving: true}
	res = startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8098", listen: 3000, docker: true})
	if r := wakeUntil(t, wake, res, 10); !r.ready {
		t.Fatalf("wait = %+v, want ready on the 0.0.0.0 listen", r)
	}
}

// A listen the kernel had no room to report is one keploy cannot vouch for
// (whose socket on the port is it?): no readiness, and the stall says so.
func TestUnderTheSwitchAListenTheKernelCouldNotReportIsNoReadiness(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	a := &fakeAgent{answers: []models.AppReadiness{{ListensLost: 3}}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080, since: readyMark{lost: 1}})
	if r := wakeUntil(t, wake, res, 10); r.ready || !strings.Contains(r.why, "the kernel had no room to report 2 listen(s)") {
		t.Fatalf("wait = %+v, want not judged, naming the 2 listens lost since the set began", r)
	}
}

// The run stopping ends the wait as a stop, not as a stall.
func TestUnderTheSwitchAStoppedRunEndsTheWaitAsAStop(t *testing.T) {
	_ = manualWake(t)
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeAgent{answers: []models.AppReadiness{{}}}
	out := make(chan waitResult, 1)
	go func() {
		ready, why := provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080}.wait(ctx)
		out <- waitResult{ready, why}
	}()
	for a.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if r := <-out; r.ready || r.why != "" {
		t.Fatalf("wait = %+v, want stopped", r)
	}
}

// --health-url: ready on its first 2xx, a 503 while starting is not; with no
// ceiling on a slow answer.
func TestUnderTheSwitchAHealthURLIsReadyOnItsFirst2xx(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		if k < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		time.Sleep(1500 * time.Millisecond) // slower than any old probe timeout
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := testCfg()
	cfg.Test.HealthURL = srv.URL + "/healthz"
	a := &fakeAgent{answers: []models.AppReadiness{{Listens: []models.AppListen{{Port: 8080, Here: true}}}}, moving: true}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: a, host: "localhost", port: "8080", listen: 8080})
	var r waitResult
	for done := false; !done; {
		select {
		case r = <-res:
			done = true
		case wake <- struct{}{}:
			time.Sleep(10 * time.Millisecond) // the server answers in wall time
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end")
		}
	}
	if !r.ready {
		t.Fatalf("wait = %+v, want ready on the 2xx (the listen alone is not the event with a health URL)", r)
	}
	if n < 3 {
		t.Fatalf("%d health requests, want 3: ready before the 2xx", n)
	}
}

// --health-path: any HTTP answer will do.
func TestUnderTheSwitchAHealthPathIsReadyOnAnyAnswer(t *testing.T) {
	wake := manualWake(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/up" {
			t.Errorf("path %q, want /up", r.URL.Path)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	cfg := testCfg()
	cfg.Test.HealthPath = "up"
	a := &fakeAgent{answers: []models.AppReadiness{{}}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: a, host: host, port: port, listen: 1})
	if r := wakeUntil(t, wake, res, 100); !r.ready {
		t.Fatalf("wait = %+v, want ready on the 503", r)
	}
}

// An application keploy did not start (--base-path, no command): its first
// test's own connect is the event, so nothing is waited for, but an explicit
// --health-url's first 2xx. So is a set whose first test says nowhere it
// dials.
func TestUnderTheSwitchAnAppKeployDidNotStartIsWaitedForOnlyByItsHealthURL(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	wake := manualWake(t)
	stallLimit(t, 1000)
	r := &Replayer{logger: zap.NewNop(), config: testCfg()}
	if ready, why := r.appReady(context.Background(), nil, "test-set-0", false, false); !ready || why != "" {
		t.Fatalf("appReady(not started) = %v %q, want ready at once", ready, why)
	}
	if ready, why := r.appReady(context.Background(), nil, "test-set-0", true, false); !ready || why != "" {
		t.Fatalf("appReady(no test says where it dials) = %v %q, want ready at once", ready, why)
	}
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if n++; n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	r.config.Test.HealthURL = srv.URL + "/up"
	out := make(chan waitResult, 1)
	go func() {
		ready, why := r.appReady(context.Background(), nil, "test-set-0", false, false)
		out <- waitResult{ready, why}
	}()
	var res waitResult
	for done := false; !done; {
		select {
		case res = <-out:
			done = true
		case wake <- struct{}{}:
			time.Sleep(10 * time.Millisecond)
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end")
		}
	}
	if !res.ready || n < 3 {
		t.Fatalf("appReady(not started, --health-url) = %+v after %d requests, want ready on the third's 2xx", res, n)
	}
}

// --health-url follows redirects to its 2xx, as the health poll always did.
func TestUnderTheSwitchAHealthURLFollowsRedirects(t *testing.T) {
	wake := manualWake(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/health/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/health/", func(w http.ResponseWriter, _ *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := testCfg()
	cfg.Test.HealthURL = srv.URL + "/health"
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: &fakeAgent{answers: []models.AppReadiness{{}}, moving: true}, host: "localhost", port: "1", listen: 1})
	if r := wakeUntil(t, wake, res, 100); !r.ready {
		t.Fatalf("wait = %+v, want ready on the redirect's 200", r)
	}
}

// --health-path is asked on the scheme the tests dial: the probe speaks TLS
// to a TLS-only port (here one that ends every connection after its
// handshake, and answers no plain-HTTP request at all).
func TestUnderTheSwitchAHealthPathIsAskedOnTheTestsScheme(t *testing.T) {
	wake := manualWake(t)
	certs := httptest.NewTLSServer(http.NotFoundHandler())
	defer certs.Close()
	var hello atomic.Bool
	cfg := &tls.Config{Certificates: certs.TLS.Certificates, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		hello.Store(true)
		return nil, nil
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := tls.Server(c, cfg)
				_ = tc.Handshake()
				_ = tc.Close()
			}()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	tcfg := testCfg()
	tcfg.Test.HealthPath = "/health"
	stallLimit(t, 1000)
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: tcfg, agent: &fakeAgent{answers: []models.AppReadiness{{}}, moving: true}, scheme: "https", host: host, port: port, listen: 1})
	if r := wakeUntil(t, wake, res, 100); !r.ready || !hello.Load() {
		t.Fatalf("wait = %+v, a TLS hello %v; want ready on the HTTPS handshake", r, hello.Load())
	}
}

// --delay is added after the event.
func TestUnderTheSwitchTheDelayComesAfterTheEvent(t *testing.T) {
	_ = manualWake(t)
	cfg := testCfg()
	cfg.Test.Delay = 1
	a := &fakeAgent{answers: []models.AppReadiness{{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}}}
	start := time.Now()
	if ready, why := (provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: a, host: "localhost", port: "8080", listen: 8080}).wait(context.Background()); !ready {
		t.Fatalf("wait = %v %q", ready, why)
	}
	if d := time.Since(start); d < time.Second {
		t.Fatalf("ready after %v, want --delay (1s) after the listen", d)
	}
}

// fakePublished answers PublishedContainerPort from a map.
type fakePublished map[uint16]uint16

func (f fakePublished) PublishedContainerPort(_ context.Context, h uint16) (uint16, bool) {
	c, ok := f[h]
	return c, ok
}

// The port the application listens on for the first test's: the container
// port the keploy agent container maps it to, else a docker run's or a
// compose service's published container port, else the port itself.
func TestTheListenPortIsTheContainerPortTheTestsHostPortPublishes(t *testing.T) {
	ctx := context.Background()
	if got := listenPortFor(ctx, fakePublished{8098: 3000}, "docker compose up", "", 8098); got != 3000 {
		t.Errorf("from the agent container: %d, want 3000", got)
	}
	for _, c := range []struct {
		cmd  string
		host uint16
		want uint16
	}{
		{"docker run -p 8080:3000 app", 8080, 3000},
		{"docker run -p 127.0.0.1:9000:9001/tcp -p 8080:3000 app", 9000, 9001},
		{"docker run --publish=8081:80 app", 8081, 80},
		{"docker run -p 3000 app", 3000, 3000}, // a random host port: nothing maps 3000
		{"./app", 8080, 8080},
	} {
		if got := listenPortFor(ctx, fakePublished{}, c.cmd, "", c.host); got != c.want {
			t.Errorf("listenPortFor(%q, %d) = %d, want %d", c.cmd, c.host, got, c.want)
		}
	}
	dir := t.TempDir()
	compose := filepath.Join(dir, "docker-compose.yml")
	override := filepath.Join(dir, "override.yml")
	if err := os.WriteFile(compose, []byte(`services:
  db:
    container_name: db
    ports: ["5433:5432"]
  app:
    ports:
      - "${APP_PORT:-8098}:3000"
      - published: 9443
        target: 443
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("services:\n  app:\n    ports: [\"7070:7000\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := "docker compose -f " + compose + " -f " + override + " up"
	for _, c := range []struct {
		host, want uint16
	}{{8098, 3000}, {9443, 443}, {7070, 7000}, {5433, 5433}} {
		if got := listenPortFor(ctx, nil, cmd, "app", c.host); got != c.want {
			t.Errorf("compose, the service by its name: listenPortFor(%d) = %d, want %d", c.host, got, c.want)
		}
	}
	t.Setenv("APP_PORT", "8099")
	if got := listenPortFor(ctx, nil, cmd, "app", 8099); got != 3000 {
		t.Errorf("compose with APP_PORT=8099: listenPortFor(8099) = %d, want 3000", got)
	}
}

// With the switch off, an application keploy did not start is not waited
// for, and one it started goes through waitForAppReady as always.
func TestWithTheSwitchOffReadinessIsAsBefore(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "0")
	r := &Replayer{logger: zap.NewNop(), config: testCfg()}
	if ready, why := r.appReady(context.Background(), nil, "test-set-0", false, false); !ready || why != "" {
		t.Fatalf("appReady(not started) = %v %q, want ready at once", ready, why)
	}
}

// A listen whose socket has since closed is no readiness: a start that
// probes a port by listening on it and closing it, or that died and is being
// restarted, is ready when the port is listened on again.
func TestUnderTheSwitchAListenSinceClosedIsNoReadiness(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	probed := models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true}}}
	again := models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true}, {Port: 8080, Here: true, Live: true, Seq: 1}}}
	a := &fakeAgent{answers: []models.AppReadiness{probed, probed, probed, again}, moving: true}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	if r := wakeUntil(t, wake, res, 10); !r.ready {
		t.Fatalf("wait = %+v, want ready once 8080 is listened on again", r)
	}
	if a.count() != 4 {
		t.Fatalf("ready after %d reads, want 4: a listen since closed is not ready", a.count())
	}
	stallLimit(t, 2)
	a = &fakeAgent{answers: []models.AppReadiness{probed}}
	res = startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	if r := wakeUntil(t, wake, res, 10); r.ready || !strings.Contains(r.why, "it listened on :8080 and closed it") {
		t.Fatalf("wait = %+v, want not judged, saying it listened on :8080 and closed it", r)
	}
}

// A set whose application keploy started anew looks only at the listens
// after the mark read before it started it: an earlier set's application
// still listening on the port (one that outlived its set) is not this one,
// and its listen does not stop this one's CPU time counting.
func TestUnderTheSwitchARestartedSetLooksOnlyAtItsOwnListens(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	old := models.AppListen{Port: 8080, Here: true, Live: true, Seq: 0}
	booting := models.AppReadiness{Listens: []models.AppListen{old}}
	up := models.AppReadiness{Listens: []models.AppListen{old, {Port: 8080, Here: true, Live: true, Seq: 1}}}
	a := &fakeAgent{answers: []models.AppReadiness{booting, booting, booting, booting, booting, up}, moving: true}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080, since: readyMark{listens: 1}})
	if r := wakeUntil(t, wake, res, 20); !r.ready {
		t.Fatalf("wait = %+v, want ready on this set's listen, the CPU time of its boot counted", r)
	}
	if a.count() != 6 {
		t.Fatalf("ready after %d reads, want 6: the earlier set's listen is not this one's", a.count())
	}
}

// A port probe (listening on a port and closing it) is no listen: the CPU
// time of the start that goes on after it still counts.
func TestUnderTheSwitchAClosedProbeDoesNotStopTheCPUCount(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	probe := models.AppListen{Port: 9999, Here: true, Seq: 0}
	booting := models.AppReadiness{Listens: []models.AppListen{probe}}
	up := models.AppReadiness{Listens: []models.AppListen{probe, {Port: 8080, Here: true, Live: true, Seq: 1}}}
	a := &fakeAgent{answers: []models.AppReadiness{booting, booting, booting, booting, booting, up}, moving: true}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	if r := wakeUntil(t, wake, res, 20); !r.ready {
		t.Fatalf("wait = %+v, want ready: a closed probe does not end the boot's CPU count", r)
	}
}

// An application kept alive that stalled for an earlier set, read again with
// nothing moved, is the same stall: not judged at the first read, not after
// another watchdog's count. Anything moved, or a count lower (another
// agent), and the wait counts afresh; a stall says where it ended.
func TestUnderTheSwitchAKeptAliveAppsSameStallIsNotJudgedAtOnce(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	at := progressMark{p: models.ReadyProgress{Forks: 1, Execs: 1}}
	same := models.AppReadiness{Progress: at.p}
	a := &fakeAgent{answers: []models.AppReadiness{same}}
	var got *progressMark
	p := provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080, stalledAt: &at,
		onStall: func(m progressMark) { got = &m }}
	if r := wakeUntil(t, wake, startWait(p), 10); r.ready || !strings.Contains(r.why, "never listened on :8080") {
		t.Fatalf("wait = %+v, want not judged", r)
	}
	if a.count() != 1 || got == nil || *got != at {
		t.Fatalf("not judged after %d reads, stall at %+v; want at the first read, at %+v", a.count(), got, at)
	}
	for _, c := range []struct {
		name string
		p    models.ReadyProgress
	}{{"moved", models.ReadyProgress{Forks: 2, Execs: 1}}, {"another agent's count", models.ReadyProgress{}}} {
		a := &fakeAgent{answers: []models.AppReadiness{{Progress: c.p}}}
		p.agent = a
		if r := wakeUntil(t, wake, startWait(p), 10); r.ready || a.count() != 4 {
			t.Fatalf("%s: wait = %+v after %d reads, want not judged after the first look and three wake-ups, counted afresh", c.name, r, a.count())
		}
	}
}

// readyInstr is an instrumentation whose agent answers AppReadiness (the
// embedded nil Instrumentation makes any other call panic).
type readyInstr struct {
	Instrumentation
	*fakeAgent
}

func httpCase(url string) []*models.TestCase {
	return []*models.TestCase{{Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: url}}}
}

// Across sets of an application kept alive: a set stalls, the next is not
// judged at its first read (nothing moved), and once the application listens
// the next is ready and the stall forgotten.
func TestUnderTheSwitchAKeptAliveAppsLaterSetsCarryItsStall(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	wake := manualWake(t)
	stallLimit(t, 2)
	idle := models.AppReadiness{Progress: models.ReadyProgress{Execs: 1}}
	a := &fakeAgent{answers: []models.AppReadiness{idle}}
	r := &Replayer{logger: zap.NewNop(), config: testCfg(), instrumentation: readyInstr{fakeAgent: a}, readyTally: new(readTally)}
	tcs := httpCase("http://localhost:8080/")
	run := func(warm bool) waitResult {
		out := tallied(make(chan waitResult, 1), r.readyTally)
		go func() {
			ready, why := r.appReady(context.Background(), tcs, "test-set-0", true, warm)
			out <- waitResult{ready, why}
		}()
		return wakeUntil(t, wake, out, 10)
	}
	if res := run(false); res.ready || a.count() != 3 || r.readyStalledAt == nil {
		t.Fatalf("set 0: %+v after %d reads, stall %v; want not judged after the count", res, a.count(), r.readyStalledAt)
	}
	if res := run(true); res.ready || a.count() != 4 {
		t.Fatalf("set 1: %+v after %d reads, want not judged at its first read", res, a.count())
	}
	a.mu.Lock()
	a.answers = []models.AppReadiness{{Progress: models.ReadyProgress{Execs: 1, Listens: 1}, Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}}
	a.mu.Unlock()
	if res := run(true); !res.ready || r.readyStalledAt != nil {
		t.Fatalf("set 2: %+v, stall %v; want ready, the stall forgotten", res, r.readyStalledAt)
	}
}

// The mark before a set's application starts anew is the agent's count of
// listens (and of those lost); an agent that cannot watch marks nothing, and
// one that cannot be read leaves the set unable to tell its application's
// listens from an earlier one's: not judged, without a wait.
func TestUnderTheSwitchARestartedSetsMarkIsTheAgentsListenCount(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	_ = manualWake(t)
	a := &fakeAgent{answers: []models.AppReadiness{{Listens: []models.AppListen{{Port: 1}, {Port: 2, Seq: 1}}, ListensLost: 3}}}
	r := &Replayer{logger: zap.NewNop(), config: testCfg(), instrumentation: readyInstr{fakeAgent: a}}
	r.markReadiness(context.Background())
	if r.readySince == nil || *r.readySince != (readyMark{listens: 2, lost: 3}) || r.readySinceErr != "" {
		t.Fatalf("mark %+v (%q), want {2 3}", r.readySince, r.readySinceErr)
	}
	a = &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{fmt.Errorf("%w: 404", models.ErrReadinessUnsupported)}}
	r.instrumentation = readyInstr{fakeAgent: a}
	if r.markReadiness(context.Background()); r.readySince != nil || r.readySinceErr != "" {
		t.Fatalf("an agent that cannot watch: mark %+v (%q), want none", r.readySince, r.readySinceErr)
	}
	a = &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{errors.New("boom")}}
	r.instrumentation = readyInstr{fakeAgent: a}
	r.markReadiness(context.Background())
	if r.readySince != nil || r.readySinceErr != "boom" {
		t.Fatalf("a failed read: mark %+v (%q), want the error", r.readySince, r.readySinceErr)
	}
	if ready, why := r.appReady(context.Background(), httpCase("http://localhost:8080/"), "test-set-0", true, false); ready || !strings.Contains(why, "boom") || a.count() != 1 {
		t.Fatalf("appReady = %v %q after %d reads, want not judged at once, naming the error", ready, why, a.count())
	}
}

// A set whose first test says nowhere it dials is not waited for: the agent
// is never asked.
func TestUnderTheSwitchASetWithNoDialTargetAsksTheAgentNothing(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	_ = manualWake(t) // nothing wakes a wait: one would never end
	a := &fakeAgent{answers: []models.AppReadiness{{}}}
	r := &Replayer{logger: zap.NewNop(), config: testCfg(), instrumentation: readyInstr{fakeAgent: a}}
	out := make(chan waitResult, 1)
	go func() {
		ready, why := r.appReady(context.Background(), nil, "test-set-0", true, false)
		out <- waitResult{ready, why}
	}()
	select {
	case res := <-out:
		if !res.ready || res.why != "" || a.count() != 0 {
			t.Fatalf("appReady = %+v after %d reads, want ready at once, the agent not asked", res, a.count())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("appReady is waiting (%d reads of the agent): want ready at once", a.count())
	}
}

// Every set waits for its application under the switch, a kept-alive one's
// later sets too; with it off, as before, not those.
func TestUnderTheSwitchEverySetWaitsForItsApplication(t *testing.T) {
	for _, on := range []bool{false, true} {
		if on {
			t.Setenv("KEPLOY_PROVEN", "1")
		} else {
			t.Setenv("KEPLOY_PROVEN", "0")
		}
		for _, c := range []struct{ serve, first bool }{{false, true}, {false, false}, {true, true}, {true, false}} {
			want := !c.serve || c.first || on
			if got := waitsForApp(c.serve, c.first); got != want {
				t.Errorf("switch %v, kept alive %v, first set %v: waits %v, want %v", on, c.serve, c.first, got, want)
			}
		}
	}
}

// An invalid --health-url is said once a run, however many sets.
func TestUnderTheSwitchAnInvalidHealthURLIsSaidOnce(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	core, logs := observer.New(zapcore.ErrorLevel)
	cfg := testCfg()
	cfg.Test.HealthURL = "localhost:8080/health"
	r := &Replayer{logger: zap.New(core), config: cfg}
	for i := 0; i < 2; i++ {
		if ready, _ := r.appReady(context.Background(), nil, fmt.Sprintf("test-set-%d", i), false, false); !ready {
			t.Fatalf("set %d: not ready, want the first test's own connect to decide", i)
		}
	}
	if n := logs.FilterMessageSnippet("invalid --health-url").Len(); n != 1 {
		t.Fatalf("said %d times, want once", n)
	}
}

// Compose's variables expand as compose expands them: from the environment,
// then the project's .env file.
func TestComposeVariablesExpandAsComposeDoes(t *testing.T) {
	env := envLookup(func(k string) (string, bool) {
		v, ok := map[string]string{"SET": "8098", "EMPTY": ""}[k]
		return v, ok
	})
	for in, want := range map[string]string{
		"${SET}:3000": "8098:3000", "$SET:3000": "8098:3000", "${UNSET:-9000}:3000": "9000:3000",
		"${EMPTY:-9000}": "9000", "${EMPTY-9000}": "", "${UNSET-9000}": "9000", "${SET:?need it}": "8098",
		"${SET:+1}": "1", "${EMPTY:+1}": "", "${EMPTY+1}": "1", "$$SET": "$SET",
	} {
		if got := interpolate(in, env); got != want {
			t.Errorf("interpolate(%q) = %q, want %q", in, got, want)
		}
	}
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  app:\n    ports: [\"${APP_PORT}:3000\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# the port\nexport APP_PORT=\"8097\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listenPortFor(context.Background(), nil, "docker compose -f "+compose+" up", "app", 8097); got != 3000 {
		t.Errorf("from the project's .env: listenPortFor(8097) = %d, want 3000", got)
	}
	other := filepath.Join(t.TempDir(), "ci.env")
	if err := os.WriteFile(other, []byte("APP_PORT=8096\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listenPortFor(context.Background(), nil, "docker compose --env-file "+other+" -f "+compose+" up", "app", 8096); got != 3000 {
		t.Errorf("from --env-file: listenPortFor(8096) = %d, want 3000", got)
	}
	t.Setenv("APP_PORT", "8095")
	if got := listenPortFor(context.Background(), nil, "docker compose -f "+compose+" up", "app", 8095); got != 3000 {
		t.Errorf("the environment over .env: listenPortFor(8095) = %d, want 3000", got)
	}
}

// markedInstr is the RunTestSet harness's instrumentation with an agent that
// answers AppReadiness: before the application runs, an earlier start's
// listen still live on the port; once it runs (released), its own too. It
// notes the order of its reads, the run, and the first test sent.
type markedInstr struct {
	*prInstr
	port    uint16
	release chan struct{}
	mu      sync.Mutex
	events  []string
	ran     bool
}

func (m *markedInstr) note(e string) {
	m.mu.Lock()
	m.events = append(m.events, e)
	m.mu.Unlock()
}

func (m *markedInstr) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.events...)
}

func (m *markedInstr) AppReadiness(_ context.Context, since uint64) (models.AppReadiness, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "read")
	all := []models.AppListen{{Port: m.port, Here: true, Live: true, Seq: 0}}
	if m.ran {
		all = append(all, models.AppListen{Port: m.port, Here: true, Live: true, Seq: 1})
	}
	st := models.AppReadiness{ListenCount: uint64(len(all))}
	for _, l := range all {
		if l.Seq >= since {
			st.Listens = append(st.Listens, l)
		}
	}
	return st, nil
}

func (m *markedInstr) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	m.note("run")
	<-m.release
	m.mu.Lock()
	m.ran = true
	m.mu.Unlock()
	return m.prInstr.Run(ctx, opts)
}

type markedHooks struct {
	prHooks
	m *markedInstr
}

func (h markedHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, id string) (interface{}, error) {
	h.m.note("sent")
	return h.prHooks.SimulateRequest(ctx, tc, id)
}

// Under the switch a set whose application keploy starts anew reads the
// agent's listen count before it starts it: the set is ready on its own
// start's listen, never on the earlier one's still live on the port.
func TestUnderTheSwitchARestartedSetIsMarkedBeforeItsApplicationStarts(t *testing.T) {
	setSwitch(t, true)
	wake := manualWake(t)
	stallLimit(t, 1<<30)
	h := newPartialRunHarness(t, 1, 0)
	u, err := url.Parse(h.cases[0].HTTPReq.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		t.Fatalf("the harness's app port %q: %v", u.Port(), err)
	}
	m := &markedInstr{prInstr: h.instr, port: uint16(port), release: make(chan struct{})}
	h.replayer.instrumentation = m
	h.replayer.hookImpl = markedHooks{m: m}
	done := make(chan models.TestSetStatus, 1)
	go func() { done <- h.run(t) }()
	// The application starts (blocked) and the wait has read once: two reads
	// (the mark's and the wait's) and no test sent; one that took the earlier
	// start's listen for this one's would have sent it.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
		ev := m.seen()
		if slices.Contains(ev, "sent") {
			t.Fatalf("events %v: a test was sent before the application ran, on the earlier start's listen", ev)
		}
		reads := 0
		for _, e := range ev {
			if e == "read" {
				reads++
			}
		}
		if slices.Contains(ev, "run") && reads >= 2 {
			if ev[0] != "read" {
				t.Fatalf("events %v, want the agent's listens read before the application runs", ev)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events %v: the application never ran, or the wait never read", ev)
		}
	}
	close(m.release)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
		m.mu.Lock()
		ran := m.ran
		m.mu.Unlock()
		if ran {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the released application never ran")
		}
	}
	for {
		select {
		case status := <-done:
			if status != models.TestSetStatusPassed {
				t.Fatalf("status %q, events %v", status, m.seen())
			}
			return
		case wake <- struct{}{}:
		case <-time.After(60 * time.Second):
			t.Fatalf("the set never ended; events %v", m.seen())
		}
	}
}

// Once the application listens (anywhere, this set's, still), only a new
// listen is progress: its pool reconnects, heartbeats and children are no
// start going on, so one that listens where the set's first test does not
// dial is not judged however busy it is.
func TestUnderTheSwitchABusyAppListeningElsewhereIsNotJudged(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	var answers []models.AppReadiness
	for i := 0; i < 20; i++ {
		answers = append(answers, models.AppReadiness{
			Listens:  []models.AppListen{{Port: 9090, Here: true, Live: true}},
			Progress: models.ReadyProgress{Listens: 1, Connects: uint64(i), Forks: uint64(i), Execs: uint64(i)},
		})
	}
	a := &fakeAgent{answers: answers, moving: true}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	r := wakeUntil(t, wake, res, 10)
	if r.ready || !strings.Contains(r.why, "never listened on :8080") || !strings.Contains(r.why, "it listens on :9090") || a.count() != 4 {
		t.Fatalf("wait = %+v after %d reads, want not judged after the first look and three wake-ups, its connects, forks and execs no progress", r, a.count())
	}
}

// An agent that cannot watch leaves a health URL to decide alone: its 2xx is
// the event, not the first test's connect.
func TestUnderTheSwitchAnAgentThatCannotWatchLeavesAHealthURLToDecide(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	cfg := testCfg()
	cfg.Test.HealthURL = srv.URL + "/up"
	a := &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{fmt.Errorf("%w: 501", models.ErrReadinessUnsupported)}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: a, host: "localhost", port: "8080", listen: 8080})
	var r waitResult
	for done := false; !done; {
		select {
		case r = <-res:
			done = true
		case wake <- struct{}{}:
			time.Sleep(10 * time.Millisecond) // the server answers in wall time
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end")
		}
	}
	if !r.ready || n.Load() < 3 || a.count() != 1 {
		t.Fatalf("wait = %+v after %d health requests and %d reads, want ready on the third's 2xx, the agent asked once", r, n.Load(), a.count())
	}
}

// A keploy that cannot ask its agent: a health URL decides alone, and with
// none the first test's own connect does, after --delay as ever.
func TestUnderTheSwitchAnUnaskableAgentLeavesItToTheHealthURLOrTheDelay(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	wake := manualWake(t)
	stallLimit(t, 1000)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	cfg := testCfg()
	cfg.Test.HealthURL = srv.URL + "/up"
	r := &Replayer{logger: zap.NewNop(), config: cfg, instrumentation: struct{ Instrumentation }{}}
	out := make(chan waitResult, 1)
	go func() {
		ready, why := r.appReady(context.Background(), httpCase("http://localhost:8080/"), "test-set-0", true, false)
		out <- waitResult{ready, why}
	}()
	var res waitResult
	for done := false; !done; {
		select {
		case res = <-out:
			done = true
		case wake <- struct{}{}:
			time.Sleep(10 * time.Millisecond)
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end")
		}
	}
	if !res.ready || n.Load() < 3 {
		t.Fatalf("appReady = %+v after %d health requests, want ready on the third's 2xx", res, n.Load())
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg = testCfg()
	cfg.Test.Delay = 1
	r = &Replayer{logger: zap.NewNop(), config: cfg, instrumentation: struct{ Instrumentation }{}}
	start := time.Now()
	if ready, why := r.appReady(context.Background(), httpCase("http://"+ln.Addr().String()+"/"), "test-set-0", true, false); !ready || why != "" || time.Since(start) < time.Second {
		t.Fatalf("appReady = %v %q after %v, want ready once its address takes a connect, after --delay (1s)", ready, why, time.Since(start))
	}
	// Kept alive (warm), ready at the first look: no --delay.
	start = time.Now()
	if ready, why := r.appReady(context.Background(), httpCase("http://"+ln.Addr().String()+"/"), "test-set-1", true, true); !ready || why != "" || time.Since(start) >= time.Second {
		t.Fatalf("warm appReady = %v %q after %v, want ready at once, no --delay", ready, why, time.Since(start))
	}
}

// A kept-alive application ready at the first look pays no --delay (there was
// no start); one that was not, after its event, does.
func TestUnderTheSwitchAWarmAppReadyAtOncePaysNoDelay(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	cfg := testCfg()
	cfg.Test.Delay = 1
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}
	start := time.Now()
	if ready, _ := (provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: &fakeAgent{answers: []models.AppReadiness{up}}, host: "localhost", port: "8080", listen: 8080, warm: true}).wait(context.Background()); !ready || time.Since(start) >= time.Second {
		t.Fatalf("ready %v after %v, want ready at once, no --delay", ready, time.Since(start))
	}
	a := &fakeAgent{answers: []models.AppReadiness{{}, up}, moving: true}
	start = time.Now()
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: a, host: "localhost", port: "8080", listen: 8080, warm: true})
	if r := wakeUntil(t, wake, res, 10); !r.ready || time.Since(start) < time.Second {
		t.Fatalf("wait = %+v after %v, want ready after its listen and --delay (1s)", r, time.Since(start))
	}
}

// An agent that never answers is no wait for ever: the wake-ups go on being
// counted with its read in flight, and the set is not judged, saying so.
func TestUnderTheSwitchAnAgentThatNeverAnswersIsAStallSaidSo(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	a := &fakeAgent{answers: []models.AppReadiness{{}}, block: true}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	for deadline := time.Now().Add(30 * time.Second); a.count() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the wait never read the agent")
		}
	}
	var r waitResult
	for done := false; !done; {
		select {
		case r = <-res:
			done = true
		case wake <- struct{}{}:
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end: a wedged agent hangs it")
		}
	}
	if r.ready || !strings.Contains(r.why, "has not answered its last read, in flight across 4 wake-ups") || a.count() != 1 {
		t.Fatalf("wait = %+v after %d reads, want not judged at the fourth wake-up (three weighed with no answer), the one read still in flight", r, a.count())
	}
}

// A wait that goes on says, every readyStatusEvery wake-ups, what it waits on
// and what moved: an endless one is told, not silent.
func TestUnderTheSwitchALongWaitSaysWhatItWaitsOn(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	was := readyStatusEvery
	readyStatusEvery = 2
	t.Cleanup(func() { readyStatusEvery = was })
	core, logs := observer.New(zapcore.InfoLevel)
	var answers []models.AppReadiness
	for i := 0; i < 6; i++ {
		answers = append(answers, models.AppReadiness{Progress: models.ReadyProgress{Execs: uint64(i)}})
	}
	answers = append(answers, models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}})
	res := startWait(provenReadiness{logger: zap.New(core), cfg: testCfg(), agent: &fakeAgent{answers: answers}, host: "localhost", port: "8080", listen: 8080})
	if r := wakeUntil(t, wake, res, 20); !r.ready {
		t.Fatalf("wait = %+v, want ready", r)
	}
	said := logs.FilterMessageSnippet("still waiting for the application").All()
	if len(said) < 2 {
		t.Fatalf("said it was waiting %d times, want every 2 wake-ups", len(said))
	}
	f := said[0].ContextMap()
	if f["waitingFor"] != "a listen on :8080, where the set's first test dials localhost:8080" || f["newExecs"] != uint64(2) {
		t.Fatalf("said %v, want what it waits on and the two execs since the first look", f)
	}
}

// A wait whose reads all fail ends not judged without a stall mark: the next
// kept-alive set waits afresh, nothing being known of where this one stalled.
func TestUnderTheSwitchAStallSeenThroughFailedReadsMarksNothing(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	a := &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom")}}
	var got *progressMark
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080, onStall: func(m progressMark) { got = &m }})
	if r := wakeUntil(t, wake, res, 10); r.ready || got != nil {
		t.Fatalf("wait = %+v, stall mark %v; want not judged, no mark", r, got)
	}
}

// A listen in another network namespace does not end the start's CPU count:
// the application has not listened where keploy reaches it.
func TestUnderTheSwitchAListenElsewhereDoesNotEndTheCPUCount(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	elsewhere := models.AppListen{Port: 9090, Here: false, Live: true}
	booting := models.AppReadiness{Listens: []models.AppListen{elsewhere}}
	up := models.AppReadiness{Listens: []models.AppListen{elsewhere, {Port: 8080, Here: true, Live: true, Seq: 1}}}
	a := &fakeAgent{answers: []models.AppReadiness{booting, booting, booting, booting, booting, up}, moving: true}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080})
	if r := wakeUntil(t, wake, res, 20); !r.ready {
		t.Fatalf("wait = %+v, want ready: another namespace's listen does not end the boot's CPU count", r)
	}
}

// A port closed and listened on again reads as listening, not as closed.
func TestUnderTheSwitchAPortListenedOnAgainIsSaidListening(t *testing.T) {
	p := provenReadiness{}
	st := models.AppReadiness{Listens: []models.AppListen{{Port: 9090, Addr: "0.0.0.0", Here: true}, {Port: 9090, Addr: "0.0.0.0", Here: true, Live: true, Seq: 1}}}
	if got := p.listeningOn(st); got != "it listens on :9090" {
		t.Fatalf("listeningOn = %q, want it listens on :9090", got)
	}
}

// A stall whose CPU time keploy could not read says so, not that none was
// used.
func TestUnderTheSwitchAStallWithItsCPUTimeUnknownSaysSo(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: []models.AppReadiness{{}}}, host: "localhost", port: "8080", listen: 8080})
	if r := wakeUntil(t, wake, res, 10); r.ready || !strings.Contains(r.why, "its CPU time keploy could not read") {
		t.Fatalf("wait = %+v, want not judged, saying its CPU time was unknown", r)
	}
}

// The run stopping while the mark is read is a stop, not a set unable to
// tell its listens apart.
func TestUnderTheSwitchAStopDuringTheMarkIsAStop(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{context.Canceled}}
	r := &Replayer{logger: zap.NewNop(), config: testCfg(), instrumentation: readyInstr{fakeAgent: a}}
	if r.markReadiness(ctx); r.readySinceErr != "" || r.readySince != nil {
		t.Fatalf("mark %+v (%q), want none: the run stopped", r.readySince, r.readySinceErr)
	}
}

// A set under the switch whose application never gets ready sends none of
// its tests, and is not judged, through RunTestSet.
func TestUnderTheSwitchASetWhoseAppNeverGetsReadySendsNothing(t *testing.T) {
	setSwitch(t, true)
	wake := manualWake(t)
	stallLimit(t, 3)
	h := newPartialRunHarness(t, 2, 0)
	m := &markedInstr{prInstr: h.instr, release: make(chan struct{})}
	close(m.release)
	m.port = 1 // it never listens where the tests dial
	h.replayer.instrumentation = m
	h.replayer.hookImpl = markedHooks{m: m}
	done := make(chan models.TestSetStatus, 1)
	go func() { done <- h.run(t) }()
	for {
		select {
		case status := <-done:
			if status != models.TestSetStatusInternalErr || slices.Contains(m.seen(), "sent") {
				t.Fatalf("status %q, events %v; want not judged, no test sent", status, m.seen())
			}
			return
		case wake <- struct{}{}:
		case <-time.After(60 * time.Second):
			t.Fatalf("the set never ended; events %v", m.seen())
		}
	}
}

// healthCounter is a health URL that answers 503 twice, then 200, counting.
func healthCounter(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/up", &n
}

// An application keploy did not start (no command), under the switch, is
// waited for by its --health-url before the first test, through RunTestSet.
func TestUnderTheSwitchAnAppWithNoCommandIsWaitedForByItsHealthURL(t *testing.T) {
	setSwitch(t, true)
	stopWakes := autoWake(t)
	defer stopWakes()
	h := newPartialRunHarness(t, 1, 0)
	h.replayer.instrument = false
	url, n := healthCounter(t)
	h.replayer.config.Test.HealthURL = url
	if status := h.run(t); status != models.TestSetStatusPassed || n.Load() < 3 {
		t.Fatalf("status %q after %d health requests, want PASSED after the third's 2xx", status, n.Load())
	}
}

// So is an application at a --base-path.
func TestUnderTheSwitchABasePathAppIsWaitedForByItsHealthURL(t *testing.T) {
	setSwitch(t, true)
	stopWakes := autoWake(t)
	defer stopWakes()
	app := newBPApp(t)
	run := newBasePathRun(t, app.URL, "test-1")
	url, n := healthCounter(t)
	run.replayer.config.Test.HealthURL = url
	if _, err := run.start(t); err != nil || n.Load() < 3 || len(app.asked()) != 1 {
		t.Fatalf("Start: %v after %d health requests, %d tests sent; want the test sent after the third's 2xx", err, n.Load(), len(app.asked()))
	}
}

// autoWake drives the watchdog by itself, a wake-up every millisecond, for
// a test that only needs it to move.
func autoWake(t *testing.T) func() {
	t.Helper()
	wake := manualWake(t)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case wake <- struct{}{}:
				time.Sleep(time.Millisecond)
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

// A start that loops (a supervisor re-exec'ing a crashing application, the
// same command line each time) is no progress after its first round: it is
// not judged, saying how often it restarted and what. A start chain of
// distinct execs (a script, then its migration, then its server) is.
func TestUnderTheSwitchARestartLoopStallsAndAStartChainDoesNot(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	var loop []models.AppReadiness
	for i := 0; i < 20; i++ {
		// The kernel counts the first round's fork and exec, then each
		// restart; the agent leaves a restart's CPU time out.
		loop = append(loop, models.AppReadiness{Progress: models.ReadyProgress{Forks: 1, Execs: 2, Restarts: uint64(i)}, LastRestart: "/usr/bin/app", CPU: time.Second, CPUKnown: true})
	}
	// Its supervisor's own CPU time, a little at each restart (moving), is
	// a loop going round, not a start going on.
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: loop, moving: true}, host: "localhost", port: "8080", listen: 8080})
	r := wakeUntil(t, wake, res, 10)
	if r.ready || !strings.Contains(r.why, "it restarted 3 time(s) without listening (the last: /usr/bin/app)") {
		t.Fatalf("wait = %+v, want not judged, naming its restarts and the program", r)
	}
	var chain []models.AppReadiness
	for i := 0; i < 8; i++ {
		chain = append(chain, models.AppReadiness{Progress: models.ReadyProgress{Execs: uint64(i)}})
	}
	chain = append(chain, models.AppReadiness{Progress: models.ReadyProgress{Execs: 8, Listens: 1}, Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}})
	res = startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: chain}, host: "localhost", port: "8080", listen: 8080})
	if r := wakeUntil(t, wake, res, 20); !r.ready {
		t.Fatalf("wait = %+v, want ready: each new exec of the chain is progress", r)
	}
}

// A kept-alive application whose health URL answers at the first look pays
// no --delay either.
func TestUnderTheSwitchAWarmAppsHealthURLAtOncePaysNoDelay(t *testing.T) {
	_ = manualWake(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	cfg := testCfg()
	cfg.Test.Delay = 2
	cfg.Test.HealthURL = srv.URL
	// The agent answers at once, with nothing: the health URL is the event.
	start := time.Now()
	if ready, _ := (provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: &fakeAgent{answers: []models.AppReadiness{{}}}, host: "localhost", port: "8080", listen: 8080, warm: true}).wait(context.Background()); !ready || time.Since(start) >= 2*time.Second {
		t.Fatalf("ready %v after %v, want ready at once, no --delay", ready, time.Since(start))
	}
}

// The port the first test's listen is looked for on comes from docker: until
// it answers no listen matches, and one that never answers is a stall that
// says so.
func TestUnderTheSwitchTheListenPortWaitsForDockersAnswer(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Here: true, Live: true}}}
	never := func(ctx context.Context) uint16 { <-ctx.Done(); return 0 }
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: []models.AppReadiness{up}}, host: "localhost", port: "8098", resolve: never})
	if r := wakeUntil(t, wake, res, 10); r.ready || !strings.Contains(r.why, "docker has not answered keploy's look at the ports the agent container publishes") ||
		!strings.Contains(r.why, "never listened where the set's first test dials localhost:8098") || strings.Contains(r.why, ":0") {
		t.Fatalf("wait = %+v, want not judged, naming docker, no port it never said", r)
	}
	answer := make(chan uint16)
	later := func(ctx context.Context) uint16 { return <-answer }
	tally := new(readTally)
	res = startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: []models.AppReadiness{up}, moving: true}, host: "localhost", port: "8098", resolve: later, tally: tally})
	wake <- struct{}{}
	answer <- 3000
	// The answer taken before the next wake-up: what the wait weighs from
	// here on, it weighs with the port docker gave.
	until(t, func() bool { return tally.ports.Load() == 1 || len(res) > 0 })
	if r := wakeUntil(t, wake, res, 10); !r.ready {
		t.Fatalf("wait = %+v, want ready once docker says 8098 is the container's 3000", r)
	}
}

// When the agent cannot watch and a health URL decides, the stall says so,
// not that the application listened nowhere and made no progress.
func TestUnderTheSwitchAnUnwatchedStallSaysTheAgentCannotTell(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	cfg := testCfg()
	cfg.Test.HealthURL = srv.URL
	a := &fakeAgent{answers: []models.AppReadiness{{}}, errs: []error{fmt.Errorf("%w: 501", models.ErrReadinessUnsupported)}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: cfg, agent: a, host: "localhost", port: "8080", listen: 8080})
	var r waitResult
	for done := false; !done; {
		select {
		case r = <-res:
			done = true
		case wake <- struct{}{}:
			time.Sleep(10 * time.Millisecond)
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end")
		}
	}
	if r.ready || !strings.Contains(r.why, "keploy's agent cannot tell when the application listens") || strings.Contains(r.why, "it listens on") {
		t.Fatalf("wait = %+v, want not judged, saying the agent cannot tell", r)
	}
}

// A stall decided while a read is in flight marks nothing (its answer is not
// known), and no status line is said at the stall itself.
func TestUnderTheSwitchAStallWithAReadInFlightMarksNothingAndSaysOnce(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	was := readyStatusEvery
	readyStatusEvery = 2
	t.Cleanup(func() { readyStatusEvery = was })
	core, logs := observer.New(zapcore.InfoLevel)
	a := &fakeAgent{answers: []models.AppReadiness{{}}, block: true}
	var got *progressMark
	res := startWait(provenReadiness{logger: zap.New(core), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080, onStall: func(m progressMark) { got = &m }})
	for deadline := time.Now().Add(30 * time.Second); a.count() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the wait never read the agent")
		}
	}
	var r waitResult
	for done := false; !done; {
		select {
		case r = <-res:
			done = true
		case wake <- struct{}{}:
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end")
		}
	}
	if r.ready || got != nil || logs.FilterMessageSnippet("still waiting").Len() != 0 {
		t.Fatalf("wait = %+v, stall mark %v, %d status lines; want not judged, no mark, none at the stall", r, got, logs.FilterMessageSnippet("still waiting").Len())
	}
}

// In Docker mode a listen on loopback only still ends the start's CPU count
// (the application listens), though no published port reaches it: a busy
// one that listens nowhere else is not judged.
func TestUnderTheSwitchADockerAppOnLoopbackOnlyStallsHoweverBusy(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	loop := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "127.0.0.1", Here: true, Live: true}}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: []models.AppReadiness{loop}, moving: true}, host: "localhost", port: "8098", listen: 3000, docker: true})
	if r := wakeUntil(t, wake, res, 10); r.ready || !strings.Contains(r.why, "127.0.0.1:3000 only") {
		t.Fatalf("wait = %+v, want not judged, its CPU time no progress once it listens", r)
	}
}

// An agent whose count of listens is below the set's mark is another agent:
// it cannot tell this set's listens from an earlier one's, said at the stall.
func TestUnderTheSwitchAnAgentWhoseCountWentBackCannotTell(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	a := &fakeAgent{answers: []models.AppReadiness{{ListenCount: 1, Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}}}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080, since: readyMark{listens: 5}})
	if r := wakeUntil(t, wake, res, 10); r.ready || !strings.Contains(r.why, "fewer than the 5 it had when this set's application started") {
		t.Fatalf("wait = %+v, want not judged, naming the count that went back", r)
	}
}

// markerInstr answers the agent's mark.
type markerInstr struct {
	readyInstr
	marked models.AppReadiness
}

func (m markerInstr) MarkReadiness(context.Context) (models.AppReadiness, error) {
	return m.marked, nil
}

// The mark is the agent's count of listens told, from its mark (which
// forgets what the last set's processes did), however many listens it
// answers (none, from that count on).
func TestUnderTheSwitchTheMarkIsTheAgentsCount(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	a := &fakeAgent{answers: []models.AppReadiness{{}}}
	r := &Replayer{logger: zap.NewNop(), config: testCfg(), instrumentation: markerInstr{readyInstr: readyInstr{fakeAgent: a}, marked: models.AppReadiness{ListenCount: 5, ListensLost: 2}}}
	r.markReadiness(context.Background())
	if r.readySince == nil || *r.readySince != (readyMark{listens: 5, lost: 2}) || a.count() != 0 {
		t.Fatalf("mark %+v after %d reads, want {5 2} from the agent's mark", r.readySince, a.count())
	}
}

// A set not judged because its application never got ready says so once:
// no ERROR about the results it has none of. The run keeps its results in
// the YAML report store, which keploy test uses and which fails the read of
// a set with none.
func TestUnderTheSwitchANotJudgedSetSaysWhyOnce(t *testing.T) {
	setSwitch(t, true)
	stopWakes := autoWake(t)
	defer stopWakes()
	stallLimit(t, 2)
	app := newBPApp(t)
	run := newBasePathRun(t, app.URL, "test-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	run.replayer.config.Test.HealthURL = srv.URL
	run.replayer.reportDB = reportdb.New(zap.NewNop(), t.TempDir())
	if _, err := run.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}
	errs := run.errorLogs()
	notJudged := 0
	for _, e := range errs {
		if strings.Contains(e, "failed to get testcase results") {
			t.Fatalf("ERROR lines %v: the not-judged set's missing results were said too", errs)
		}
		if strings.Contains(e, "test-set is not judged") {
			notJudged++
		}
	}
	if notJudged != 1 || len(app.asked()) != 0 {
		t.Fatalf("ERROR lines %v, %d tests sent; want the set said not judged once, no test sent", errs, len(app.asked()))
	}
}

// until waits for cond, spinning the scheduler, failing after 30 s.
func until(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
	}
}

// gatedAgent is a fakeAgent whose read number at (from 0) waits for gate.
type gatedAgent struct {
	*fakeAgent
	at   int64
	gate chan struct{}
	n    atomic.Int64
}

func (g *gatedAgent) AppReadiness(ctx context.Context, since uint64) (models.AppReadiness, error) {
	if g.n.Add(1)-1 == g.at {
		select {
		case <-g.gate:
		case <-ctx.Done():
		}
	}
	return g.fakeAgent.AppReadiness(ctx, since)
}

// docker's answer, once in, is taken before a read's answer that came with
// it is weighed: a port docker gave never counts against the application.
// Here the wait is held (its status line) while a read's answer and docker's
// both come in; the read's, weighed first, would be the stall.
func TestUnderTheSwitchDockersAnswerIsTakenBeforeWhatCameWithIt(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 2)
	was := readyStatusEvery
	readyStatusEvery = 1
	t.Cleanup(func() { readyStatusEvery = was })
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "0.0.0.0", Here: true, Live: true}}}
	for i := 0; i < 50; i++ {
		blocked, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		core, _ := observer.New(zap.InfoLevel)
		logger := zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
			if strings.HasPrefix(e.Message, "still waiting") {
				once.Do(func() { close(blocked); <-release })
			}
			return nil
		}))
		answer := make(chan uint16)
		agent := &gatedAgent{fakeAgent: &fakeAgent{answers: []models.AppReadiness{up}}, at: 1, gate: make(chan struct{})}
		tally := new(readTally)
		res := startWait(provenReadiness{logger: logger, cfg: testCfg(), agent: agent, host: "localhost", port: "8098", docker: true, tally: tally,
			resolve: func(context.Context) uint16 { return <-answer }})
		readsWeighed(t, res, 0) // the first read, docker not in
		wake <- struct{}{}      // its read held in the agent
		wake <- struct{}{}      // weighed with that read in flight: the status line, held
		<-blocked
		close(agent.gate) // the held read answers
		answer <- 3000    // and docker answers
		until(t, func() bool { return tally.answered.Load() == 2 && tally.portsSent.Load() == 1 })
		close(release)
		var r waitResult
		select {
		case r = <-res:
		case <-time.After(30 * time.Second):
			t.Fatal("the wait did not end")
		}
		if !r.ready {
			t.Fatalf("round %d: wait = %+v, want ready on :3000, docker's answer taken first", i, r)
		}
	}
}

// stuckDocker is an agent client whose look at docker never answers.
type stuckDocker struct{ readyInstr }

func (stuckDocker) PublishedContainerPort(ctx context.Context, _ uint16) (uint16, bool) {
	<-ctx.Done()
	return 0, false
}

// A native application listens on the port its first test dials: no look at
// docker, however docker answers, before its listen there is its readiness.
func TestUnderTheSwitchANativeAppsPortNeedsNoLookAtDocker(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	wake := manualWake(t)
	stallLimit(t, 2)
	a := &fakeAgent{answers: []models.AppReadiness{{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}}}
	r := &Replayer{logger: zap.NewNop(), config: testCfg(), instrumentation: stuckDocker{readyInstr{fakeAgent: a}}, readyTally: new(readTally)}
	out := tallied(make(chan waitResult, 1), r.readyTally)
	go func() {
		ready, why := r.appReady(context.Background(), httpCase("http://localhost:8080/"), "test-set-0", true, false)
		out <- waitResult{ready, why}
	}()
	if res := wakeUntil(t, wake, out, 10); !res.ready || a.count() != 1 {
		t.Fatalf("%+v after %d reads, want ready at the first", res, a.count())
	}
}

// A kept-alive application's set ready once docker answers, after the
// agent's first read, and before any wake-up, pays no --delay: it was ready
// at the first look.
func TestUnderTheSwitchAWarmDockerSetReadyOnDockersAnswerPaysNoDelay(t *testing.T) {
	_ = manualWake(t)
	cfg := testCfg()
	cfg.Test.Delay = 2
	core, logs := observer.New(zap.InfoLevel)
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "0.0.0.0", Here: true, Live: true}}}
	answer := make(chan uint16)
	res := startWait(provenReadiness{logger: zap.New(core), cfg: cfg, agent: &fakeAgent{answers: []models.AppReadiness{up}}, host: "localhost", port: "8098",
		docker: true, warm: true, resolve: func(context.Context) uint16 { return <-answer }})
	readsWeighed(t, res, 0)
	answer <- 3000
	select {
	case r := <-res:
		if !r.ready {
			t.Fatalf("wait = %+v, want ready", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not end")
	}
	ready := logs.FilterMessage("the application is ready; firing the first test").All()
	if len(ready) != 1 || ready[0].ContextMap()["then --delay"] != time.Duration(0) {
		t.Fatalf("ready said %v, want once, then no --delay", ready)
	}
}

// A set's restarts begin at the agent's mark before keploy started its
// application, not at the wait's first read: three restarts before it are
// three, said at the stall and in each status line, and its CPU time no
// progress since; and a start with two restarts before the mark, none
// since, has its CPU time counted, and goes on.
func TestUnderTheSwitchASetsRestartsBeginAtItsMark(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	wake := manualWake(t)
	stallLimit(t, 3)
	was := readyStatusEvery
	readyStatusEvery = 1
	t.Cleanup(func() { readyStatusEvery = was })
	run := func(marked uint64, answers []models.AppReadiness) (waitResult, *observer.ObservedLogs, *fakeAgent) {
		core, logs := observer.New(zap.InfoLevel)
		a := &fakeAgent{answers: answers, moving: true}
		r := &Replayer{logger: zap.New(core), config: testCfg(), readyTally: new(readTally),
			instrumentation: markerInstr{readyInstr: readyInstr{fakeAgent: a}, marked: models.AppReadiness{Progress: models.ReadyProgress{Restarts: marked}}}}
		r.markReadiness(context.Background())
		out := tallied(make(chan waitResult, 1), r.readyTally)
		go func() {
			ready, why := r.appReady(context.Background(), httpCase("http://localhost:8080/"), "test-set-0", true, false)
			out <- waitResult{ready, why}
		}()
		return wakeUntil(t, wake, out, 20), logs, a
	}
	looped := models.AppReadiness{Progress: models.ReadyProgress{Restarts: 3}, LastRestart: "/usr/bin/app"}
	res, logs, _ := run(0, []models.AppReadiness{looped})
	if res.ready || !strings.Contains(res.why, "it restarted 3 time(s) without listening (the last: /usr/bin/app)") {
		t.Fatalf("wait = %+v, want not judged, its three restarts since the mark said", res)
	}
	lines := logs.FilterMessage("still waiting for the application before the set's first test").All()
	if len(lines) == 0 {
		t.Fatal("no status line")
	}
	for _, l := range lines {
		if l.ContextMap()["restarts"] != uint64(3) || l.ContextMap()["lastRestart"] != "/usr/bin/app" {
			t.Fatalf("status line %v, want three restarts, the last /usr/bin/app", l.ContextMap())
		}
	}
	var going []models.AppReadiness
	for i := 0; i < 8; i++ {
		going = append(going, models.AppReadiness{Progress: models.ReadyProgress{Restarts: 2}})
	}
	going = append(going, models.AppReadiness{Progress: models.ReadyProgress{Restarts: 2}, Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}})
	if res, _, a := run(2, going); !res.ready || a.count() != len(going) {
		t.Fatalf("wait = %+v after %d reads, want ready at read %d: its CPU time counted, no restart since the mark", res, a.count(), len(going))
	}
}

// A kept-alive application whose start loops: its set stalls, and the next
// set, the loop gone round again since (more restarts, more CPU time), is
// not judged at its first read: the same thing again is no progress.
func TestUnderTheSwitchAKeptAliveRestartLoopsStallIsCarried(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	wake := manualWake(t)
	stallLimit(t, 2)
	a := &fakeAgent{answers: []models.AppReadiness{{Progress: models.ReadyProgress{Execs: 1, Restarts: 4}}}, moving: true}
	// No mark, as a --keep-app-alive run takes none (its application is
	// started once for the run): its restarts begin at 0.
	r := &Replayer{logger: zap.NewNop(), config: testCfg(), readyTally: new(readTally),
		instrumentation: markerInstr{readyInstr: readyInstr{fakeAgent: a}}}
	tcs := httpCase("http://localhost:8080/")
	run := func(warm bool) waitResult {
		out := tallied(make(chan waitResult, 1), r.readyTally)
		go func() {
			ready, why := r.appReady(context.Background(), tcs, "test-set-0", true, warm)
			out <- waitResult{ready, why}
		}()
		return wakeUntil(t, wake, out, 10)
	}
	if res := run(false); res.ready || r.readyStalledAt == nil {
		t.Fatalf("set 0: %+v, stall %v; want not judged after the count", res, r.readyStalledAt)
	}
	before := a.count()
	a.mu.Lock()
	a.answers = []models.AppReadiness{{Progress: models.ReadyProgress{Execs: 1, Restarts: 6}}}
	a.mu.Unlock()
	if res := run(true); res.ready || a.count() != before+1 || !strings.Contains(res.why, "it restarted 6 time(s)") {
		t.Fatalf("set 1: %+v after %d reads (%d before), want not judged at its first read, its six restarts said", res, a.count(), before)
	}
}

// A start that loops, listening on a side port (a debug port) before each
// crash, listens there again at each round: no progress, so it is not
// judged, saying how often it restarted.
func TestUnderTheSwitchACrashLoopListeningOnASidePortStalls(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	var answers []models.AppReadiness
	for i := uint64(0); i < 30; i++ {
		answers = append(answers, models.AppReadiness{
			Progress:    models.ReadyProgress{Listens: i + 1, Execs: 1, Restarts: i},
			LastRestart: "/usr/bin/java",
			Listens:     []models.AppListen{{Port: 5005, Addr: "0.0.0.0", Here: true, Seq: i}},
		})
	}
	res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: answers}, host: "localhost", port: "8080", listen: 8080})
	r := wakeUntil(t, wake, res, 20)
	if r.ready || !strings.Contains(r.why, "without listening (the last: /usr/bin/java)") {
		t.Fatalf("wait = %+v, want not judged, its restarts said", r)
	}
}

// A set's results that could not be read are said for any set but one
// whose application never got ready (it said why, and ran no test) and one
// with no tests to run: one not judged for another reason too.
func TestOnlyANeverReadySetsUnreadResultsGoUnsaid(t *testing.T) {
	r := &Replayer{}
	r.markNotJudged("other")
	r.markNotJudged("idle")
	r.markNeverReady("idle")
	if !r.saysResultsUnread(models.TestSetStatusInternalErr, "other") || !r.saysResultsUnread(models.TestSetStatusFailed, "plain") {
		t.Fatal("a set not judged for another reason, or judged, has its unread results unsaid")
	}
	if r.saysResultsUnread(models.TestSetStatusInternalErr, "idle") || r.saysResultsUnread(models.TestSetStatusNoTestsToRun, "plain") {
		t.Fatal("a never-ready set's, or one with no tests to run, missing results are said")
	}
	r.forgetNotJudged("idle")
	if !r.saysResultsUnread(models.TestSetStatusInternalErr, "idle") {
		t.Fatal("a set run again is still taken as never ready")
	}
}

// With no mark (an application kept alive for the run, or one that came up
// with its agent), a set's restarts begin at 0: three before the wait's
// first read are three, said, and its CPU time no progress.
func TestUnderTheSwitchWithNoMarkASetsRestartsBeginAtNone(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	wake := manualWake(t)
	stallLimit(t, 3)
	looped := models.AppReadiness{Progress: models.ReadyProgress{Restarts: 3}, LastRestart: "/usr/bin/app"}
	a := &fakeAgent{answers: []models.AppReadiness{looped}, moving: true}
	r := &Replayer{logger: zap.NewNop(), config: testCfg(), readyTally: new(readTally),
		instrumentation: markerInstr{readyInstr: readyInstr{fakeAgent: a}}}
	out := tallied(make(chan waitResult, 1), r.readyTally)
	go func() {
		ready, why := r.appReady(context.Background(), httpCase("http://localhost:8080/"), "test-set-0", true, false)
		out <- waitResult{ready, why}
	}()
	if res := wakeUntil(t, wake, out, 20); res.ready || !strings.Contains(res.why, "it restarted 3 time(s) without listening (the last: /usr/bin/app)") {
		t.Fatalf("wait = %+v, want not judged, its three restarts said", res)
	}
}

// docker's answer, taken first, is judged against the latest read: a read's
// answer that came with it, saying the application's listen on that port
// closed since (a probe), is weighed, not the read before it. Here the wait
// is held while both come in; it is not ready on the stale read.
func TestUnderTheSwitchDockersAnswerIsJudgedOnTheLatestRead(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	was := readyStatusEvery
	readyStatusEvery = 1
	t.Cleanup(func() { readyStatusEvery = was })
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "0.0.0.0", Here: true, Live: true}}}
	down := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "0.0.0.0", Here: true}}}
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	core, _ := observer.New(zap.InfoLevel)
	logger := zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if strings.HasPrefix(e.Message, "still waiting") {
			once.Do(func() { close(blocked); <-release })
		}
		return nil
	}))
	answer := make(chan uint16)
	agent := &gatedAgent{fakeAgent: &fakeAgent{answers: []models.AppReadiness{up, down}}, at: 1, gate: make(chan struct{})}
	tally := new(readTally)
	res := startWait(provenReadiness{logger: logger, cfg: testCfg(), agent: agent, host: "localhost", port: "8098", docker: true, tally: tally,
		resolve: func(context.Context) uint16 { return <-answer }})
	readsWeighed(t, res, 0)
	wake <- struct{}{}
	wake <- struct{}{}
	<-blocked
	close(agent.gate) // the held read answers: :3000 closed
	answer <- 3000    // and docker answers
	until(t, func() bool { return tally.answered.Load() == 2 && tally.portsSent.Load() == 1 })
	close(release)
	if r := wakeUntil(t, wake, res, 10); r.ready || !strings.Contains(r.why, "never listened on :3000") {
		t.Fatalf("wait = %+v, want not judged: the latest read says :3000 closed", r)
	}
}

// The status line before docker answers names no port docker never gave.
func TestUnderTheSwitchTheStatusLineBeforeDockerAnswersNamesNoPort(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 3)
	was := readyStatusEvery
	readyStatusEvery = 1
	t.Cleanup(func() { readyStatusEvery = was })
	core, logs := observer.New(zap.InfoLevel)
	never := func(ctx context.Context) uint16 { <-ctx.Done(); return 0 }
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "0.0.0.0", Here: true, Live: true}}}
	res := startWait(provenReadiness{logger: zap.New(core), cfg: testCfg(), agent: &fakeAgent{answers: []models.AppReadiness{up}}, host: "localhost", port: "8098", docker: true, resolve: never})
	_ = wakeUntil(t, wake, res, 10)
	lines := logs.FilterMessage("still waiting for the application before the set's first test").All()
	if len(lines) == 0 {
		t.Fatal("no status line")
	}
	for _, l := range lines {
		if s, _ := l.ContextMap()["waitingFor"].(string); strings.Contains(s, ":0,") || !strings.Contains(s, "docker has not said") {
			t.Fatalf("status line waits for %q, want no port docker never gave, and why", s)
		}
	}
}

// A listen is progress the first time its socket address is listened on:
// once the application listens, a crash loop re-listening on its live side
// port is none (it stalls); before, a listen the kernel had no room to
// report is one, a new address on the same port is one, and the same port
// and address in another network namespace is one.
func TestUnderTheSwitchAListenIsProgressByItsAddress(t *testing.T) {
	t.Run("a live side port again", func(t *testing.T) {
		wake := manualWake(t)
		stallLimit(t, 3)
		var answers []models.AppReadiness
		for i := uint64(0); i < 30; i++ {
			answers = append(answers, models.AppReadiness{
				Progress: models.ReadyProgress{Listens: i + 1, Execs: 1, Restarts: i},
				Listens:  []models.AppListen{{Port: 5005, Addr: "0.0.0.0", Here: true, Live: true, Seq: i}},
			})
		}
		res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: answers}, host: "localhost", port: "8080", listen: 8080})
		if r := wakeUntil(t, wake, res, 20); r.ready || !strings.Contains(r.why, "it listens on :5005") {
			t.Fatalf("wait = %+v, want not judged: listening on :5005 again is no progress", r)
		}
	})
	progressEveryOther := func(t *testing.T, at func(i int) models.AppReadiness) {
		t.Helper()
		wake := manualWake(t)
		stallLimit(t, 2)
		var answers []models.AppReadiness
		for i := 0; i < 12; i++ {
			answers = append(answers, at(i/2)) // moves every other wake-up
		}
		last := at(6)
		last.Listens = append(last.Listens, models.AppListen{Port: 80, Here: true, Live: true, Seq: 100})
		answers = append(answers, last)
		res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: answers}, host: "localhost", port: "80", listen: 80})
		if r := wakeUntil(t, wake, res, 20); !r.ready {
			t.Fatalf("wait = %+v, want ready: each new listen was progress", r)
		}
	}
	t.Run("a lost listen", func(t *testing.T) {
		progressEveryOther(t, func(i int) models.AppReadiness { return models.AppReadiness{ListensLost: uint64(i)} })
	})
	t.Run("a new address on one port", func(t *testing.T) {
		progressEveryOther(t, func(i int) models.AppReadiness {
			var st models.AppReadiness
			for k := 0; k <= i; k++ {
				st.Listens = append(st.Listens, models.AppListen{Port: 9000, Addr: fmt.Sprintf("10.0.0.%d", k+1), Here: true, Seq: uint64(k)})
			}
			return st
		})
	})
	t.Run("the same address in another namespace", func(t *testing.T) {
		wake := manualWake(t)
		stallLimit(t, 2)
		here := models.AppListen{Port: 9000, Addr: "0.0.0.0", Here: true}
		there := models.AppListen{Port: 9000, Addr: "0.0.0.0", Seq: 1}
		live := models.AppListen{Port: 80, Here: true, Live: true, Seq: 2}
		answers := []models.AppReadiness{{}, {Listens: []models.AppListen{here}}, {Listens: []models.AppListen{here}},
			{Listens: []models.AppListen{here, there}}, {Listens: []models.AppListen{here, there}}, {Listens: []models.AppListen{here, there, live}}}
		res := startWait(provenReadiness{logger: zap.NewNop(), cfg: testCfg(), agent: &fakeAgent{answers: answers}, host: "localhost", port: "80", listen: 80})
		if r := wakeUntil(t, wake, res, 10); !r.ready {
			t.Fatalf("wait = %+v, want ready: :9000 in another namespace was a new listen", r)
		}
	})
}

// bufferedWake is manualWake with room for one wake-up: one sent while the
// wait is held is in when it goes on, beside whatever else came in.
func bufferedWake(t *testing.T) chan struct{} {
	t.Helper()
	ch := make(chan struct{}, 1)
	was := readyWake
	readyWake = func(ctx context.Context) (<-chan struct{}, func()) { return ch, func() {} }
	t.Cleanup(func() { readyWake = was })
	return ch
}

// heldWait starts a wait whose first status line holds it until release:
// the second wake-up is weighed with the first wake-up's read (gated in the
// agent) in flight, and while it is held that read answers.
func heldWait(t *testing.T, p provenReadiness, agent *gatedAgent) (res chan waitResult, blocked, release chan struct{}) {
	t.Helper()
	blocked, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	core, _ := observer.New(zap.InfoLevel)
	p.logger = zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if strings.HasPrefix(e.Message, "still waiting") {
			once.Do(func() { close(blocked); <-release })
		}
		return nil
	}))
	p.agent = agent
	return startWait(p), blocked, release
}

// A read's answer already in is weighed before a wake-up that came with it:
// the wake-up before is weighed by that answer, not as one whose read never
// answered. Here the answer says the application listens: it is ready, never
// a stall weighed with its answer in (a starved keploy, the next wake-up due
// at once).
func TestUnderTheSwitchAReadsAnswerIsWeighedBeforeAWakeUpThatCameWithIt(t *testing.T) {
	wake := bufferedWake(t)
	stallLimit(t, 2)
	was := readyStatusEvery
	readyStatusEvery = 1
	t.Cleanup(func() { readyStatusEvery = was })
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "0.0.0.0", Here: true, Live: true}}}
	for i := 0; i < 50; i++ {
		tally := new(readTally)
		agent := &gatedAgent{fakeAgent: &fakeAgent{answers: []models.AppReadiness{{}, up}}, at: 1, gate: make(chan struct{})}
		res, blocked, release := heldWait(t, provenReadiness{cfg: testCfg(), host: "localhost", port: "3000", listen: 3000, tally: tally}, agent)
		readsWeighed(t, res, 0)
		wake <- struct{}{} // its read held in the agent
		wake <- struct{}{} // weighed with that read in flight: the status line, held
		<-blocked
		close(agent.gate) // the read answers: :3000 listens
		until(t, func() bool { return tally.answered.Load() == 2 })
		wake <- struct{}{} // the next wake-up, due at once: in before the wait goes on
		close(release)
		r := <-res
		select { // a wake-up the wait never took is none of the next round's
		case <-wake:
		default:
		}
		if !r.ready {
			t.Fatalf("round %d: wait = %+v, want ready on :3000, the read's answer weighed first", i, r)
		}
	}
}

// docker's answer, a read's and the next wake-up all at once (a starved
// keploy, on the stall limit's last wake-up): the read's answer is weighed
// with the port before the wake-up, so an application listening on the port
// docker gives is ready.
func TestUnderTheSwitchDockersAnswerAReadsAndAWakeUpAtOnce(t *testing.T) {
	wake := bufferedWake(t)
	stallLimit(t, 2)
	was := readyStatusEvery
	readyStatusEvery = 1
	t.Cleanup(func() { readyStatusEvery = was })
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "0.0.0.0", Here: true, Live: true}}}
	for i := 0; i < 50; i++ {
		answer := make(chan uint16)
		tally := new(readTally)
		agent := &gatedAgent{fakeAgent: &fakeAgent{answers: []models.AppReadiness{up}}, at: 1, gate: make(chan struct{})}
		res, blocked, release := heldWait(t, provenReadiness{cfg: testCfg(), host: "localhost", port: "8098", docker: true, tally: tally,
			resolve: func(context.Context) uint16 { return <-answer }}, agent)
		readsWeighed(t, res, 0)
		wake <- struct{}{}
		wake <- struct{}{}
		<-blocked
		close(agent.gate)
		answer <- 3000
		until(t, func() bool { return tally.answered.Load() == 2 && tally.portsSent.Load() == 1 })
		wake <- struct{}{} // the next wake-up, due at once: in before the wait goes on
		close(release)
		r := <-res
		select { // a wake-up the wait never took is none of the next round's
		case <-wake:
		default:
		}
		if !r.ready {
			t.Fatalf("round %d: wait = %+v, want ready on :3000 with docker's answer in", i, r)
		}
	}
}
