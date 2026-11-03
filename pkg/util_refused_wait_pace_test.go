package pkg

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// pacedDials refuses the client's first n dials as the kernel does (all of
// them when n < 0), then dials for real, and keeps when each dial began.
type pacedDials struct {
	mu    sync.Mutex
	n     int
	at    []time.Time
	dials chan struct{}
}

func newPacedDials(n int) *pacedDials {
	return &pacedDials{n: n, dials: make(chan struct{}, 1024)}
}

func (d *pacedDials) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.at = append(d.at, time.Now())
	refuse := d.n != 0
	if d.n > 0 {
		d.n--
	}
	d.mu.Unlock()
	d.dials <- struct{}{}
	if refuse {
		return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, addr)
}

// gaps is the time between each dial and the one before it.
func (d *pacedDials) gaps() []time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	var gaps []time.Duration
	for i := 1; i < len(d.at); i++ {
		gaps = append(gaps, d.at[i].Sub(d.at[i-1]))
	}
	return gaps
}

// countingListener counts the connections it accepts.
type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

// serveCounting answers 200 on addr and counts the connections made to it.
func serveCounting(t *testing.T, addr string) *countingListener {
	ln := &countingListener{Listener: testnet.ListenShared(t, "tcp", addr)}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln
}

// A request refused again right after its address accepted a connection
// (the address accepts, the request's own connection is refused: a listener
// that comes and goes as the application starts) is sent again only after a
// pause of acceptPollInterval each time, never at once: the wait cannot spin.
// It says once that it waits.
func TestARequestRefusedAgainAfterItsAddressAcceptedIsSentAgainAtAPause(t *testing.T) {
	addr := refusingAddr(t)
	serve(t, addr)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithCancel(EndsWithApp(context.Background()))
	defer cancel()
	dials := newPacedDials(-1)
	client := &http.Client{Transport: &http.Transport{DialContext: dials.dial}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/a", strings.NewReader("body"))
	done := make(chan sendResult, 1)
	go func() {
		resp, err := doRequestWithConnRefusedRetry(ctx, zap.New(core), client, req, nil, 0)
		done <- sendResult{resp, err}
	}()
	const want = 4
	for i := 0; i < want; i++ {
		select {
		case <-dials.dials:
		case r := <-done:
			t.Fatalf("the request ended (%v, %v) after %d dials while the application ran", r.resp, r.err, i)
		case <-time.After(30 * time.Second):
			t.Fatalf("the request was sent %d times in 30 s, want %d", i, want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not end at its context's end")
	}
	// The first wait accepts at once (the address accepts); every later one
	// begins with the pause.
	for i, gap := range dials.gaps()[1:] {
		if gap < acceptPollInterval {
			t.Errorf("re-send %d came %s after the one before, under the %s pause", i+2, gap, acceptPollInterval)
		}
	}
	if n := waitingLines(logs); n != 1 {
		t.Errorf("said %d times that the request waits, want once", n)
	}
}

// Where the application's address may not be probed (WithoutAddressProbe:
// --disable-app-ready-probe), a refused request is sent again every
// acceptPollInterval while the application keploy runs is up, and no
// connection is made to its address but the request's own: no connection
// opened and closed having sent nothing.
func TestARefusedRequestIsSentAgainWithNoProbeWhereTheAddressMayNotBeProbed(t *testing.T) {
	addr := refusingAddr(t)
	ln := serveCounting(t, addr)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithCancel(WithoutAddressProbe(EndsWithApp(context.Background())))
	defer cancel()
	refusals := maxConnRefusedRetries + 2
	dials := newPacedDials(refusals)
	client := &http.Client{Transport: &http.Transport{DialContext: dials.dial}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/a", strings.NewReader("body"))
	done := make(chan sendResult, 1)
	go func() {
		resp, err := doRequestWithConnRefusedRetry(ctx, zap.New(core), client, req, nil, 0)
		done <- sendResult{resp, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.resp.StatusCode != http.StatusOK {
			t.Fatalf("want the application's 200 after %d refusals, got (%v, %v)", refusals, r.resp, r.err)
		}
		_ = r.resp.Body.Close()
	case <-time.After(30 * time.Second):
		t.Fatal("the request was not answered although its address accepts")
	}
	// The listener accepted the request's connection first and only: the
	// connections it accepted before it are taken in the order they came.
	if n := ln.accepted.Load(); n != 1 {
		t.Errorf("the application's address took %d connections, want only the request's own", n)
	}
	gaps := dials.gaps()
	if len(gaps) != refusals {
		t.Fatalf("the request was sent %d times, want %d", len(gaps)+1, refusals+1)
	}
	for i, gap := range gaps {
		if gap < acceptPollInterval {
			t.Errorf("re-send %d came %s after the one before, under the %s pause", i+1, gap, acceptPollInterval)
		}
	}
	lines := logs.FilterMessageSnippet("probing the application's address is disabled by configuration").All()
	if len(lines) != 1 || !strings.Contains(lines[0].Message, addr) {
		t.Errorf("said %v, want one line naming %s that says it is sent again with no probe", lines, addr)
	}
	if n := waitingLines(logs); n != 0 {
		t.Errorf("said %d times that it waits for the address to accept a connection, which it may not probe", n)
	}
}

// AddressProbeAllowed is true unless WithoutAddressProbe marked the context.
func TestAddressProbeAllowedUnlessMarked(t *testing.T) {
	if !AddressProbeAllowed(context.Background()) {
		t.Error("an unmarked context may not probe")
	}
	if AddressProbeAllowed(WithoutAddressProbe(EndsWithApp(context.Background()))) {
		t.Error("a context WithoutAddressProbe marked may probe")
	}
}

// A refused request's wait, with or without probing its address, says on a
// timer that it still waits, naming the address, from its first wait until
// the request is answered, and not after.
func TestARefusedRequestSaysItStillWaitsUntilItIsAnswered(t *testing.T) {
	every := stillWaitingEvery
	stillWaitingEvery = 50 * time.Millisecond
	t.Cleanup(func() { stillWaitingEvery = every })
	for _, probe := range []bool{true, false} {
		t.Run(map[bool]string{true: "probing", false: "no probe"}[probe], func(t *testing.T) {
			addr := refusingAddr(t)
			core, logs := observer.New(zapcore.InfoLevel)
			ctx, cancel := context.WithCancel(EndsWithApp(context.Background()))
			defer cancel()
			if !probe {
				ctx = WithoutAddressProbe(ctx)
			}
			done := sendInBackground(ctx, zap.New(core), addr)
			still := func() int {
				return logs.FilterMessageSnippet("still waiting for the application to accept a connection on " + addr).Len()
			}
			deadline := time.After(30 * time.Second)
			for still() < 2 {
				select {
				case r := <-done:
					t.Fatalf("the request ended (%v, %v) while its address refused", r.resp, r.err)
				case <-deadline:
					t.Fatalf("said %d times in 30 s that the request still waits on %s, every %s", still(), addr, stillWaitingEvery)
				case <-time.After(10 * time.Millisecond):
				}
			}
			serve(t, addr)
			select {
			case r := <-done:
				if r.err != nil || r.resp.StatusCode != http.StatusOK {
					t.Fatalf("want the application's 200 once it accepted, got (%v, %v)", r.resp, r.err)
				}
				_ = r.resp.Body.Close()
			case <-time.After(30 * time.Second):
				t.Fatal("the request was not answered once its address accepted")
			}
			n := still()
			time.Sleep(10 * stillWaitingEvery)
			if after := still(); after != n {
				t.Errorf("said %d more times that the request waits after it was answered", after-n)
			}
		})
	}
}
