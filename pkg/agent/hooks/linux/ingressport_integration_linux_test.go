//go:build linux && pidmap_integration

// Root-only, like the other pidmap_integration tests: it loads keploy's BPF
// objects (the main one and the ingress lookup) into the running kernel and
// attaches the lookup to this process's network namespace.

package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
)

// ingressHoldAgent loads keploy's main object as the agent does and returns
// the hooks, the hold not started.
func ingressHoldAgent(t *testing.T) *Hooks {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to load BPF programs")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	h := NewHooks(zap.NewNop(), &config.Config{})
	if err := spec.LoadAndAssign(&h.objects, nil); err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	t.Cleanup(func() { _ = h.objects.Close() })
	return h
}

// unusedPort is a loopback port nothing listens on now.
func unusedPort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return uint16(p)
}

func dialPort(port uint16) (net.Conn, error) {
	return net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), 10*time.Second)
}

// movedAndNoted writes what the bind hooks write for an app socket that
// bound port orig on 0.0.0.0 and was moved: app_port_info[orig], and, with
// structs.FlagIngressHold, its note. It returns the moved port.
func movedAndNoted(t *testing.T, h *Hooks, orig uint16, note bool) uint16 {
	t.Helper()
	moved := unusedPort(t)
	if err := h.objects.AppPortInfo.Update(orig, appPortTranslate{ProxyPort: orig, ApplicationPort: moved}, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	if note {
		if err := h.objects.AppPortBinds.Update(appBindKey{Port: orig, Kind: appBindV4}, appBindCount{N: 1}, ebpf.UpdateAny); err != nil {
			t.Fatal(err)
		}
	}
	return moved
}

// holdCgroup is a cgroup of the test's own, where the hold attaches its
// release program as it does on the app's.
func holdCgroup(t *testing.T) string {
	t.Helper()
	cg, err := os.MkdirTemp(cgroupV2Root(t), "keploy-ingress-test-")
	if err != nil {
		t.Fatalf("create a cgroup: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })
	return cg
}

// refusedPort dials port and fails the test unless it is refused.
func refusedPort(t *testing.T, port uint16, what string) {
	t.Helper()
	c, err := dialPort(port)
	if err == nil {
		_ = c.Close()
		t.Fatalf("%s: the call was answered, want it refused", what)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("%s: %v, want refused", what, err)
	}
}

// The agent's hold against keploy's own objects: a port the bind hooks moved
// (app_port_info and the note of the app's bind, written here as they write
// them inside the app's bind) is refused while nothing holds it, as before;
// held, a call to it is accepted on the park with the dialed port as its
// local address, before any forwarder listens; once the forwarder is
// registered, the port's calls are the forwarder's. A moved port with no
// note (no socket of the app's bound it, or the hold's flag was off) is the
// kernel's. The park and the forwarder are plain TCP.
func TestAMovedPortIsHeldByTheParkUntilItsForwarderListens(t *testing.T) {
	h := ingressHoldAgent(t)
	orig := unusedPort(t)
	moved := movedAndNoted(t, h, orig, true)
	unnoted := unusedPort(t)
	movedAndNoted(t, h, unnoted, false)
	if c, err := dialPort(orig); err == nil {
		_ = c.Close()
		t.Fatal("the moved port answered before anything held it")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial before the hold: %v, want refused", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.startIngressHold(ctx, holdCgroup(t))
	if !h.IngressHeld() {
		t.Fatal("the hold did not start on this kernel")
	}
	park := h.IngressParkListener()
	if p := listenerProtocol(t, park); p != unix.IPPROTO_TCP {
		t.Fatalf("the park is protocol %d, want plain TCP", p)
	}
	if got, ok := h.MovedPort(orig); !ok || got != moved {
		t.Fatalf("MovedPort(%d) = %d, %v; want %d", orig, got, ok, moved)
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := park.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	c, err := dialPort(orig)
	if err != nil {
		t.Fatalf("dial the held port: %v, want the park to take it", err)
	}
	defer func() { _ = c.Close() }()
	select {
	case p := <-accepted:
		if got := p.LocalAddr().(*net.TCPAddr).Port; got != int(orig) {
			t.Fatalf("the parked connection's local port is %d, want the dialed %d", got, orig)
		}
		_ = p.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the park did not accept the call")
	}
	if n := h.IngressLookupCounts()["parked"]; n != 1 {
		t.Fatalf("parked %d, want 1", n)
	}
	refusedPort(t, unnoted, "a moved port no socket of the app's is noted for")
	if n := h.IngressLookupCounts()["not_where_the_app_bound"]; n == 0 {
		t.Fatalf("the unnoted port's lookup was not counted: %v", h.IngressLookupCounts())
	}

	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	fwd, err := lc.Listen(context.Background(), "tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(int(orig))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fwd.Close() }()
	h.RegisterIngressForwarder(orig, fwd)
	fwdAccepted := make(chan struct{}, 1)
	go func() {
		c, err := fwd.Accept()
		if err == nil {
			_ = c.Close()
			fwdAccepted <- struct{}{}
		}
	}()
	c2, err := dialPort(orig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	select {
	case <-fwdAccepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the registered forwarder did not get the port's call")
	}
	if n := h.IngressLookupCounts()["parked"]; n != 1 {
		t.Fatalf("parked %d after the forwarder registered, want still 1", n)
	}

	// Recording ends: the hold's link and park close with its context, so
	// the port is steered nowhere and, with nothing listening on it, refused,
	// as before keploy held it.
	_ = fwd.Close()
	cancel()
	for deadline := time.Now().Add(10 * time.Second); h.IngressHeld(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hold did not close with its context")
		}
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		c, err := dialPort(orig)
		if err != nil && errors.Is(err, syscall.ECONNREFUSED) {
			break
		}
		if c != nil {
			_ = c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the hold closed, a call to the port: %v, want it refused (the lookup and park still hold it)", err)
		}
	}
}

func listenerProtocol(t *testing.T, ln net.Listener) int {
	t.Helper()
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var p int
	var gerr error
	if err := rc.Control(func(fd uintptr) { p, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return p
}

// ingressSockHelperEnv runs the test binary as an app process in the hold's
// cgroup (TestMain): it opens a TCP socket, prints its cookie, and closes it
// when told, then says so.
const ingressSockHelperEnv = "KEPLOY_INGRESS_SOCK_HELPER"

func runIngressSockHelper() int {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	fmt.Println("cookie", cookie)
	in := bufio.NewReader(os.Stdin)
	if _, err := in.ReadString('\n'); err != nil {
		return 3
	}
	_ = unix.Close(fd)
	fmt.Println("closed")
	_, _ = io.Copy(io.Discard, in)
	return 0
}

// When an app socket the bind hooks noted goes, the hold's release program,
// which keploy attaches to the app's cgroup, takes its note back: the port
// the app let go is not held, and a call to it is refused, as without
// keploy (app_port_info still names where a bind of it would be moved).
func TestAPortWhoseSocketWentIsNotHeld(t *testing.T) {
	h := ingressHoldAgent(t)
	cg := holdCgroup(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.startIngressHold(ctx, cg)
	if !h.IngressHeld() {
		t.Fatal("the hold did not start on this kernel")
	}
	go func() {
		for {
			c, err := h.IngressParkListener().Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	cgf, err := os.Open(cg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cgf.Close() }()
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), ingressSockHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgf.Fd()), Pdeathsig: syscall.SIGKILL}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	out := bufio.NewReader(stdout)
	line, err := out.ReadString('\n')
	var cookie uint64
	if _, serr := fmt.Sscanf(line, "cookie %d", &cookie); err != nil || serr != nil {
		t.Fatalf("the helper said %q (%v, %v)", line, err, serr)
	}

	// The note the bind hooks' post_bind writes for the helper's socket.
	orig := unusedPort(t)
	movedAndNoted(t, h, orig, true)
	key := appBindKey{Port: orig, Kind: appBindV4}
	if err := h.objects.AppBindSocks.Update(cookie, key, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	c, err := dialPort(orig)
	if err != nil {
		t.Fatalf("dial the noted port: %v, want the park to take it", err)
	}
	_ = c.Close()

	if _, err := io.WriteString(stdin, "close\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := out.ReadString('\n'); err != nil || line != "closed\n" {
		t.Fatalf("the helper said %q (%v), want closed", line, err)
	}
	var k appBindKey
	if err := h.objects.AppBindSocks.Lookup(cookie, &k); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("the gone socket's note is still there (%+v, %v): the release program is not on the app's cgroup", k, err)
	}
	var n appBindCount
	if err := h.objects.AppPortBinds.Lookup(key, &n); err != nil || n.N != 0 {
		t.Fatalf("the port's count after its socket went: %+v (%v), want 0", n, err)
	}
	refusedPort(t, orig, "a port whose noted socket went")
}

// acceptedOn reports whether ln accepts a connection within 10 s (each call
// takes one).
func acceptedOn(ln net.Listener) <-chan bool {
	got := make(chan bool, 1)
	go func() {
		if tl, ok := ln.(*net.TCPListener); ok {
			_ = tl.SetDeadline(time.Now().Add(10 * time.Second))
		}
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
		got <- err == nil
	}()
	return got
}

// dialAccepted dials port and fails the test unless ln accepts the call.
func dialAccepted(t *testing.T, port uint16, ln net.Listener, what string) {
	t.Helper()
	got := acceptedOn(ln)
	c, err := dialPort(port)
	if err != nil {
		t.Fatalf("%s: dial: %v", what, err)
	}
	defer func() { _ = c.Close() }()
	if !<-got {
		t.Fatalf("%s did not get the call", what)
	}
}

// A port the agent yields (its forwarder found another listener, not
// keploy's, on it) is that listener's: the lookup leaves its calls to the
// kernel. A forwarder registered for the port later takes the yield back,
// so once that forwarder goes the park holds the port again. A stopped park
// (its accept loop could not go on) is out of the lookup and closed: a call
// to a moved port no forwarder takes is then refused, as without the hold.
func TestAYieldedPortIsItsListenersAndAStoppedParkHoldsNothing(t *testing.T) {
	h := ingressHoldAgent(t)
	orig := unusedPort(t)
	movedAndNoted(t, h, orig, true)
	other, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(orig))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.startIngressHold(ctx, holdCgroup(t))
	if !h.IngressHeld() {
		t.Fatal("the hold did not start on this kernel")
	}
	park := h.IngressParkListener()
	dialAccepted(t, orig, park, "the park, before the agent yields the port,")

	h.YieldIngressPort(orig)
	dialAccepted(t, orig, other, "the listener that held the port")
	if n := h.IngressLookupCounts()["yielded_to_another_listener"]; n != 1 {
		t.Fatalf("yielded %d, want 1: %v", n, h.IngressLookupCounts())
	}
	_ = other.Close()

	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	fwd, err := lc.Listen(context.Background(), "tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(int(orig))))
	if err != nil {
		t.Fatal(err)
	}
	h.RegisterIngressForwarder(orig, fwd)
	dialAccepted(t, orig, fwd, "the forwarder registered after the yield")
	if n := h.IngressLookupCounts()["to_forwarder"]; n != 1 {
		t.Fatalf("to the forwarder %d, want 1 (the registration takes the yield back): %v", n, h.IngressLookupCounts())
	}
	_ = fwd.Close()
	dialAccepted(t, orig, park, "the park, once the forwarder that took the yield back went,")

	h.StopIngressPark()
	refusedPort(t, orig, "a moved port once the park stopped")
	if n := h.IngressLookupCounts()["no_park"]; n != 1 {
		t.Fatalf("no park %d, want 1: %v", n, h.IngressLookupCounts())
	}
	if tl, ok := park.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(10 * time.Second)) // an open park fails the test, not the run
	}
	if c, err := park.Accept(); err == nil {
		_ = c.Close()
		t.Fatal("the stopped park still accepts")
	} else if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the stopped park's Accept: %v, want it closed", err)
	}
}

// The hold closes while the ingress manager still calls it, as at the end of
// a recording, when StopAll unregisters the forwarders as the hold's context
// ends: every call either runs before the close, on maps still open, or
// finds no hold. Under the race detector (the root job builds these rows
// with -race) a call that used a map after letting go of the hold's lock is
// a race with the close. Each accessor loops in a goroutine of its own, so
// each is the last call a goroutine makes before the close, whichever runs
// then. Once closed, the calls answer as with no hold.
func TestTheHoldClosesOnlyBetweenItsCalls(t *testing.T) {
	h := ingressHoldAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.startIngressHold(ctx, holdCgroup(t))
	if !h.IngressHeld() {
		t.Fatal("the hold did not start")
	}
	orig := unusedPort(t)
	movedAndNoted(t, h, orig, true)
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	fwd, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fwd.Close() })

	calls := []func(){
		func() { h.RegisterIngressForwarder(orig, fwd) },
		func() { h.MovedPort(orig) },
		func() { _, _ = h.AppIPv6Binds(orig) },
		func() { h.UnregisterIngressForwarder(orig) },
		func() { h.YieldIngressPort(orig) },
		func() { h.IngressLookupCounts() },
		func() { h.StopIngressPark() },
	}
	callers := len(calls)
	stop := make(chan struct{})
	started := make(chan struct{}, callers)
	done := make(chan struct{}, callers)
	for _, call := range calls {
		go func() {
			defer func() { done <- struct{}{} }()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				call()
				if n == 50 {
					started <- struct{}{}
				}
			}
		}()
	}
	for i := 0; i < callers; i++ {
		<-started
	}
	h.closeIngressHold()
	close(stop)
	for i := 0; i < callers; i++ {
		<-done
	}
	if h.IngressHeld() || h.IngressParkListener() != nil || h.IngressLookupCounts() != nil {
		t.Fatal("a closed hold still answers as held")
	}
	if _, ok := h.MovedPort(orig); ok {
		t.Fatal("a closed hold still reads the moved port")
	}
	if a, err := h.AppIPv6Binds(orig); a != nil || err != nil {
		t.Fatalf("a closed hold still reads the app's binds: %v, %v", a, err)
	}
}

// The hold reads which IPv6 addresses the app's sockets on a port take
// calls on from the bind hooks' notes, as the lookup matches them: an IPv6
// socket on ::1 takes ::1; one on :: takes ::, with IPV6_V6ONLY or without;
// an IPv4 socket, or an IPv6 one on a v4-mapped address (::ffff:127.0.0.1,
// how Java binds 127.0.0.1), takes no IPv6. A note no socket holds now, or
// another port's, takes nothing.
func TestTheHoldReadsTheIPv6AddressesTheAppsBindsTake(t *testing.T) {
	h := ingressHoldAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.startIngressHold(ctx, holdCgroup(t))
	if !h.IngressHeld() {
		t.Fatal("the hold did not start")
	}
	loop6 := netip.IPv6Loopback().As16()
	loop4 := [16]byte{127, 0, 0, 1}
	note := func(orig uint16, kind uint16, addr [16]byte, n uint32) {
		t.Helper()
		if err := h.objects.AppPortBinds.Update(appBindKey{Port: orig, Kind: kind, Addr: addr}, appBindCount{N: n}, ebpf.UpdateAny); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name  string
		kind  uint16
		addr  [16]byte
		wantA []netip.Addr
	}{
		{"an IPv4 socket on 0.0.0.0", appBindV4, [16]byte{}, nil},
		{"an IPv6 socket on ::ffff:127.0.0.1", appBindV4, loop4, nil},
		{"an IPv6 socket on :: without IPV6_V6ONLY", appBindV6Dual, [16]byte{}, []netip.Addr{netip.IPv6Unspecified()}},
		{"an IPv6 socket on :: with IPV6_V6ONLY", appBindV6, [16]byte{}, []netip.Addr{netip.IPv6Unspecified()}},
		{"an IPv6 socket on ::1", appBindV6, loop6, []netip.Addr{netip.IPv6Loopback()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			orig := unusedPort(t)
			note(orig, c.kind, c.addr, 1)
			note(orig, appBindV6, [16]byte{0xfd, 15: 5}, 0) // a socket on [fd00::5] that went
			note(orig+1, appBindV6, [16]byte{0xfd, 15: 6}, 1)
			got, err := h.AppIPv6Binds(orig)
			if err != nil || !slices.Equal(got, c.wantA) {
				t.Fatalf("the IPv6 addresses taken on port %d: %v, %v; want %v", orig, got, err, c.wantA)
			}
		})
	}
}
