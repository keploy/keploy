//go:build linux && synhold_integration

// Root-only: it installs nf_tables rules and binds an NFQUEUE, so it lives
// behind the synhold_integration build tag and runs as root in its own CI job
// (.github/workflows/upstream-refusal-linux.yml), never in `go test ./...`.

package synhold

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/nftables"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sys/unix"
)

type plan struct {
	delay time.Duration
	out   Outcome
}

// proxyListeners listens on one port on both loopbacks, as the proxy does.
func proxyListeners(t *testing.T) uint16 {
	t.Helper()
	for i := 0; i < 20; i++ {
		l4, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l4.Addr().(*net.TCPAddr).Port
		l6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		if err != nil {
			_ = l4.Close()
			continue
		}
		for _, l := range []net.Listener{l4, l6} {
			l := l
			t.Cleanup(func() { _ = l.Close() })
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					_ = c.Close()
				}
			}()
		}
		return uint16(port)
	}
	t.Fatal("no port free on both loopbacks")
	return 0
}

func freePort(t *testing.T, host string) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// localPorts gives each connection a test makes a local port that no earlier
// connection in the test had. A test that tells its connections' decisions
// apart by their port needs this, and freePort alone does not give it: the
// kernel offers a port again once its socket is gone, and on the other
// address family at once (127.0.0.1:p and [::1]:p never conflict). Two
// connections would then be counted as one connection decided twice.
type localPorts struct {
	offer func(t *testing.T, host string) int // freePort; a test scripts repeats
	used  map[int]bool
}

func newLocalPorts() *localPorts {
	return &localPorts{offer: freePort, used: map[int]bool{}}
}

func (p *localPorts) next(t *testing.T, host string) int {
	t.Helper()
	for i := 0; i < 100; i++ {
		port := p.offer(t, host)
		if !p.used[port] {
			p.used[port] = true
			return port
		}
	}
	t.Fatalf("no port on %s that this test has not used already", host)
	return 0
}

// TestLocalPortsAreNeverHandedOutTwice: a port the kernel offers again — on
// the other family, or on the same one once its socket is gone — is passed
// over, so no two of a test's connections share a port and have their
// decisions counted together.
func TestLocalPortsAreNeverHandedOutTwice(t *testing.T) {
	offers := []int{40001, 40001, 40003, 40001, 40003, 40005}
	p := &localPorts{used: map[int]bool{}, offer: func(t *testing.T, _ string) int {
		if len(offers) == 0 {
			t.Fatal("asked for more ports than the kernel offered")
		}
		n := offers[0]
		offers = offers[1:]
		return n
	}}
	var got []int
	for _, host := range []string{"127.0.0.1", "::1", "127.0.0.1"} {
		got = append(got, p.next(t, host))
	}
	if want := []int{40001, 40003, 40005}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ports handed out = %v, want %v", got, want)
	}
}

func classify(err error) string {
	for _, e := range []struct {
		n string
		e syscall.Errno
	}{{"ECONNREFUSED", syscall.ECONNREFUSED}, {"EHOSTUNREACH", syscall.EHOSTUNREACH}, {"ENETUNREACH", syscall.ENETUNREACH}} {
		if errors.Is(err, e.e) {
			return e.n
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "TIMEOUT"
	}
	if err == nil {
		return "ok"
	}
	return "other: " + err.Error()
}

func requireRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root (nf_tables and NFQUEUE); run the test binary with sudo")
	}
}

// TestHeldHandshakesAreAnsweredAsDecided: the application's connect returns
// what the decision says — after the decision, not before — on both address
// families, and a SYN retransmitted while its handshake is held is not
// decided again.
func TestHeldHandshakesAreAnsweredAsDecided(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)

	var mu sync.Mutex
	plans := map[uint16]plan{}
	decided := map[uint16]int{}
	h, err := Start(context.Background(), zaptest.NewLogger(t), port,
		func(_ context.Context, client, proxy netip.AddrPort) Decision {
			mu.Lock()
			p := plans[client.Port()]
			decided[client.Port()]++
			mu.Unlock()
			if proxy.Port() != port {
				t.Errorf("decided a handshake to %s, not to the proxy port %d", proxy, port)
			}
			time.Sleep(p.delay)
			return Decision{Outcome: p.out}
		})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()

	// The decisions are counted by the connection's port, so each connection
	// has a port of its own.
	ports := newLocalPorts()
	for _, host := range []string{"127.0.0.1", "::1"} {
		for _, tc := range []struct {
			plan plan
			want string
		}{
			{plan{300 * time.Millisecond, Accept}, "ok"},
			// Longer than the client's first SYN retransmit (1 s).
			{plan{1500 * time.Millisecond, Accept}, "ok"},
			{plan{200 * time.Millisecond, Refuse}, "ECONNREFUSED"},
			{plan{200 * time.Millisecond, HostUnreachable}, "EHOSTUNREACH"},
			{plan{200 * time.Millisecond, NetUnreachable}, "ENETUNREACH"},
			{plan{0, Drop}, "TIMEOUT"},
		} {
			lp := ports.next(t, host)
			mu.Lock()
			plans[uint16(lp)] = tc.plan
			mu.Unlock()
			d := net.Dialer{Timeout: 2500 * time.Millisecond, LocalAddr: &net.TCPAddr{IP: net.ParseIP(host), Port: lp}}
			start := time.Now()
			c, err := d.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
			took := time.Since(start)
			if c != nil {
				_ = c.Close()
			}
			got := classify(err)
			if got != tc.want {
				t.Errorf("%s %v: connect = %s (%v), want %s", host, tc.plan.out, got, err, tc.want)
				continue
			}
			if tc.want != "TIMEOUT" && took < tc.plan.delay {
				t.Errorf("%s %v: connect returned after %v, before the decision (%v)", host, tc.plan.out, took, tc.plan.delay)
			}
			mu.Lock()
			n := decided[uint16(lp)]
			mu.Unlock()
			if tc.plan.out != Drop && n != 1 {
				t.Errorf("%s %v: decided %d times, want once", host, tc.plan.out, n)
			}
		}
	}
}

// TestARetransmittedSYNGetsTheAnswerAlreadyGiven: a copy of a SYN that comes
// after its handshake was answered gets the same answer. It is not decided
// again, which would dial the destination again. Drop leaves the client
// retransmitting the SYN (first after 1 s) until its connect times out, on
// both address families.
func TestARetransmittedSYNGetsTheAnswerAlreadyGiven(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)
	var decided atomic.Int32
	h, err := Start(context.Background(), zaptest.NewLogger(t), port,
		func(context.Context, netip.AddrPort, netip.AddrPort) Decision {
			decided.Add(1)
			return Decision{Outcome: Drop}
		})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, host := range []string{"127.0.0.1", "::1"} {
		before := decided.Load()
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 2500*time.Millisecond)
		if c != nil {
			_ = c.Close()
		}
		if got := classify(err); got != "TIMEOUT" {
			t.Fatalf("%s: connect = %s (%v), want it to time out unanswered", host, got, err)
		}
		if n := decided.Load() - before; n != 1 {
			t.Errorf("%s: decided %d times, want once: a retransmission of an answered SYN was decided again", host, n)
		}
	}
}

func tableExists(t *testing.T, name string) bool {
	t.Helper()
	c, err := nftables.New()
	if err != nil {
		t.Fatal(err)
	}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		if tb.Name == name {
			return true
		}
	}
	return false
}

func dialNow(t *testing.T, port uint16) error {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), 2*time.Second)
	if c != nil {
		_ = c.Close()
	}
	return err
}

func refuseAll(context.Context, netip.AddrPort, netip.AddrPort) Decision {
	return Decision{Outcome: Refuse}
}

// TestCloseLeavesNothingBehind: once the proxy stops, its port's handshakes
// complete at once again and no rule of its remains.
func TestCloseLeavesNothingBehind(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)
	h, err := Start(context.Background(), zaptest.NewLogger(t), port, refuseAll)
	if err != nil {
		t.Fatal(err)
	}
	if err := dialNow(t, port); classify(err) != "ECONNREFUSED" {
		t.Fatalf("while held: %v, want refused", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if tableExists(t, h.table) {
		t.Fatalf("table %s left behind", h.table)
	}
	if err := dialNow(t, port); err != nil {
		t.Fatalf("after Close: %v, want the handshake to complete", err)
	}
}

// TestAnAgentThatDiedLetsHandshakesThrough: an agent that exits without
// removing its rules leaves a queue nobody reads; its SYNs must complete
// (the rule's bypass flag), not hang until the application times out. The
// next agent on the port replaces the table.
func TestAnAgentThatDiedLetsHandshakesThrough(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)
	h, err := Start(context.Background(), zaptest.NewLogger(t), port, refuseAll)
	if err != nil {
		t.Fatal(err)
	}
	// Die: the queue goes, the rules stay.
	h.cancelDecide()
	h.cancelQueue()
	_ = h.nf.Close()
	if !tableExists(t, h.table) {
		t.Fatal("the simulated crash removed the rules; nothing is tested")
	}
	start := time.Now()
	if err := dialNow(t, port); err != nil {
		t.Fatalf("with nobody reading the queue: %v, want the handshake to complete", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("handshake took %v with nobody reading the queue", took)
	}

	h2, err := Start(context.Background(), zaptest.NewLogger(t), port, refuseAll)
	if err != nil {
		t.Fatalf("Start over a stale table: %v", err)
	}
	defer h2.Close()
	if err := dialNow(t, port); classify(err) != "ECONNREFUSED" {
		t.Fatalf("after replacing the stale table: %v, want refused", err)
	}
}

// TestANewConnectionOnAReusedPortIsNotHeldBehindTheAbandonedOne: an
// application that gave up on a connection to a destination that never
// answers, and connects again from the same port, gets its new connection
// decided at once — not dropped as a retransmission of the old SYN for as
// long as the old dial runs. The old decision is cancelled and what it
// produced discarded.
func TestANewConnectionOnAReusedPortIsNotHeldBehindTheAbandonedOne(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)
	var calls atomic.Int32
	oldCancelled := make(chan struct{})
	discarded := make(chan struct{}, 1)
	h, err := Start(context.Background(), zaptest.NewLogger(t), port,
		func(ctx context.Context, _, _ netip.AddrPort) Decision {
			if calls.Add(1) == 1 {
				<-ctx.Done() // the destination never answers
				close(oldCancelled)
				return Decision{Outcome: Accept, Discard: func() { discarded <- struct{}{} }}
			}
			return Decision{Outcome: Accept}
		})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	lp := freePort(t, "127.0.0.1")
	dial := func(timeout time.Duration) error {
		d := net.Dialer{Timeout: timeout, LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: lp}}
		c, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		if c != nil {
			_ = c.Close()
		}
		return err
	}
	if err := dial(500 * time.Millisecond); classify(err) != "TIMEOUT" {
		t.Fatalf("first connection: %v, want it to time out while held", err)
	}
	start := time.Now()
	if err := dial(3 * time.Second); err != nil {
		t.Fatalf("second connection from the same port: %v, want it decided and accepted", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("second connection took %v: it waited behind the abandoned one", took)
	}
	select {
	case <-oldCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned decision was not cancelled")
	}
	select {
	case <-discarded:
	case <-time.After(2 * time.Second):
		t.Fatal("what the abandoned decision produced was not discarded")
	}
}

// TestShutdownAnswersTheHeldHandshakes: when the agent stops, its context
// ends before Close runs (the proxy drains its connections first). A SYN held
// then must be answered at once — its decision cancelled, which completes it
// — not dropped with an unbound queue and left for the application's next
// SYN retransmission.
func TestShutdownAnswersTheHeldHandshakes(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)
	held := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	h, err := Start(ctx, zaptest.NewLogger(t), port,
		func(ctx context.Context, _, _ netip.AddrPort) Decision {
			close(held)
			<-ctx.Done()
			// A cancelled dial takes a moment to unwind; the queue must
			// still be bound when its answer is given.
			time.Sleep(200 * time.Millisecond)
			return Decision{Outcome: OutcomeFor(ctx.Err())}
		})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dialNow(t, port) }()
	<-held
	start := time.Now()
	cancel() // the agent is stopping; Close comes later
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("held connect after shutdown began: %v, want it to complete", err)
		}
	case <-time.After(900 * time.Millisecond):
		t.Fatal("the held connect was not answered when shutdown began: it waits for a SYN retransmit")
	}
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("held connect completed %v after shutdown began", took)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStartRemovesOnlyAbandonedTables: a table whose queue nobody has bound
// (its agent was killed) is removed when another agent starts; a live
// agent's table on another port is not.
func TestStartRemovesOnlyAbandonedTables(t *testing.T) {
	requireRoot(t)
	live, err := Start(context.Background(), zaptest.NewLogger(t), proxyListeners(t), refuseAll)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	dead, err := Start(context.Background(), zaptest.NewLogger(t), proxyListeners(t), refuseAll)
	if err != nil {
		t.Fatal(err)
	}
	dead.cancelDecide()
	dead.cancelQueue()
	_ = dead.nf.Close() // killed: its queue is gone, its table stays
	if !tableExists(t, dead.table) {
		t.Fatal("the simulated kill removed the table; nothing is tested")
	}

	third, err := Start(context.Background(), zaptest.NewLogger(t), proxyListeners(t), refuseAll)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if tableExists(t, dead.table) {
		t.Fatalf("abandoned table %s was not removed", dead.table)
	}
	if !tableExists(t, live.table) {
		t.Fatalf("a live agent's table %s was removed", live.table)
	}
}

// TestStartWaitsForAStoppingAgentsQueue: an agent starting on the port of one
// that is still stopping finds the queue bound (the kernel says EPERM) and
// waits for it, rather than running unheld for its whole session.
func TestStartWaitsForAStoppingAgentsQueue(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)
	old, err := Start(context.Background(), zaptest.NewLogger(t), port, refuseAll)
	if err != nil {
		t.Fatal(err)
	}
	oldClosed := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = old.Close()
		close(oldClosed)
	}()
	h, err := Start(context.Background(), zaptest.NewLogger(t), port, refuseAll)
	if err != nil {
		t.Fatalf("Start while the previous agent's queue was still bound: %v", err)
	}
	defer h.Close()
	// Once the old agent has finished stopping, the new one's table must
	// still be there: the old one removes its table before it frees the
	// queue, so it cannot remove the new one's.
	<-oldClosed
	if !tableExists(t, h.table) {
		t.Fatal("the stopping agent removed the new agent's table")
	}
	if err := dialNow(t, port); classify(err) != "ECONNREFUSED" {
		t.Fatalf("the new agent does not hold: %v, want refused", err)
	}
}

// TestStartHoldsThroughSignals: Start holds the port although the thread it
// runs on is sent SIGURG every few tens of microseconds, so that its probe
// connections are signalled while they wait for their answer; twenty Starts
// in a row must each hold.
func TestStartHoldsThroughSignals(t *testing.T) {
	requireRoot(t)
	// Start runs on this thread, and every signal goes to it.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tid := unix.Gettid()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		// Shorter than a probe connection waits for its answer, so a probe
		// is signalled while it waits; a raw sleep, because time.Sleep
		// makes this pause about a millisecond.
		pause := unix.NsecToTimespec((20 * time.Microsecond).Nanoseconds())
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = unix.Tgkill(os.Getpid(), tid, unix.SIGURG)
			_ = unix.Nanosleep(&pause, nil)
		}
	}()
	defer func() { close(stop); <-stopped }()

	for i := 0; i < 20; i++ {
		port := proxyListeners(t)
		h, err := Start(context.Background(), zaptest.NewLogger(t), port, refuseAll)
		if err != nil {
			t.Fatalf("Start %d, signalled: %v", i, err)
		}
		if err := dialNow(t, port); classify(err) != "ECONNREFUSED" {
			_ = h.Close()
			t.Fatalf("Start %d, signalled, does not hold: %v, want refused", i, err)
		}
		if err := h.Close(); err != nil {
			t.Fatalf("Close %d, signalled: %v", i, err)
		}
	}
}

// TestCloseRemovesItsTableWhileTheQueueIsStillBound pins Close's order: the
// table goes while the queue is still bound, so an agent starting on the same
// port (which can bind only once the queue is free) cannot lose its own table
// to the stopping one.
func TestCloseRemovesItsTableWhileTheQueueIsStillBound(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)
	h, err := Start(context.Background(), zaptest.NewLogger(t), port, refuseAll)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	h.beforeUnbind = func() {
		checked = true
		if tableExists(t, h.table) {
			t.Error("the queue is about to be unbound while the table is still installed")
		}
		if !queueBound(port) {
			t.Error("the queue was unbound before the table was removed")
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("Close never reached the unbind")
	}
}

// TestStartRefusesToHoldWhereARejectDoesNotTakeEffect: when the probe
// connection is not refused (a kernel where the reject answer does not reach
// the application), Start fails, leaving no rules behind, and the proxy
// accepts handshakes as before.
func TestStartRefusesToHoldWhereARejectDoesNotTakeEffect(t *testing.T) {
	requireRoot(t)
	prev := selfTestAnswer
	selfTestAnswer = Accept
	t.Cleanup(func() { selfTestAnswer = prev })
	port := proxyListeners(t)
	if _, err := Start(context.Background(), zaptest.NewLogger(t), port, refuseAll); err == nil {
		t.Fatal("Start held handshakes although a reject did not take effect")
	}
	if tableExists(t, fmt.Sprintf("%s%d", tablePrefix, port)) {
		t.Fatal("a failed Start left its table behind")
	}
	if err := dialNow(t, port); err != nil {
		t.Fatalf("after a failed Start: %v, want the handshake to complete", err)
	}
}

// TestCloseAnswersAHeldHandshake: Close, with a decision still in flight,
// ends that decision and answers its SYN before removing the table (removing
// it would flush the SYN and leave the application to retransmit).
func TestCloseAnswersAHeldHandshake(t *testing.T) {
	requireRoot(t)
	port := proxyListeners(t)
	held := make(chan struct{})
	h, err := Start(context.Background(), zaptest.NewLogger(t), port,
		func(ctx context.Context, _, _ netip.AddrPort) Decision {
			close(held)
			<-ctx.Done()
			return Decision{Outcome: OutcomeFor(ctx.Err())}
		})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dialNow(t, port) }()
	<-held
	start := time.Now()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("held connect after Close: %v, want it to complete", err)
		}
	case <-time.After(900 * time.Millisecond):
		t.Fatal("the held connect was not answered by Close: it waits for a SYN retransmit")
	}
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("held connect completed %v after Close began", took)
	}
}
