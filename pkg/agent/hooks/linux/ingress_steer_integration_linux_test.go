//go:build linux && pidmap_integration

package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// steeredApp is the app's own listener on 127.0.0.1:port, noted as the bind
// hooks note a bind they steer (app_bind_socks by its cookie, counted in
// app_port_binds): what keploy's agent has after the app bound its port.
func steeredApp(t *testing.T, h *Hooks, port uint16) (net.Listener, appBindKey) {
	t.Helper()
	ln := listenShared(t, "tcp4", "127.0.0.1", port)
	return ln, noteListener(t, h, ln, port)
}

// noteListener notes ln, on 127.0.0.1:port, as the bind hooks note a bind
// they steer.
func noteListener(t *testing.T, h *Hooks, ln net.Listener, port uint16) appBindKey {
	t.Helper()
	return noteListenerAt(t, h, ln, [4]byte{127, 0, 0, 1}, port)
}

// noteListenerAt is noteListener for ln on addr:port.
func noteListenerAt(t *testing.T, h *Hooks, ln net.Listener, addr [4]byte, port uint16) appBindKey {
	t.Helper()
	k := appBindKey{Port: port, Kind: appBindV4}
	copy(k.Addr[:4], addr[:])
	if err := h.objects.AppBindSocks.Update(listenerCookie(t, ln), k, ebpf.UpdateNoExist); err != nil {
		t.Fatal(err)
	}
	if err := h.objects.AppPortBinds.Update(k, appBindCount{N: 1}, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	return k
}

func listenerCookie(t *testing.T, ln net.Listener) uint64 {
	t.Helper()
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ck uint64
	var gerr error
	if err := rc.Control(func(fd uintptr) { ck, gerr = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_COOKIE) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return ck
}

// inodeOf is the inode of the socket c (a listener or a connection) holds.
func inodeOf(t *testing.T, c syscall.Conn) uint32 {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ino uint64
	var serr error
	if err := rc.Control(func(fd uintptr) {
		var st unix.Stat_t
		serr = unix.Fstat(int(fd), &st)
		ino = st.Ino
	}); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatal(serr)
	}
	return uint32(ino)
}

// joinCgroup moves this process into cg, where the hold's programs are, for
// the rest of the test: keploy's own process is in the cgroup the hold
// attaches to (its own connections are noted there), and so is the test,
// whose dials stand for keploy's.
func joinCgroup(t *testing.T, cg string) {
	t.Helper()
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	var orig string
	for _, l := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			orig = filepath.Join(cgroupV2Root(t), p)
		}
	}
	if orig == "" {
		t.Fatal("this process is in no cgroup v2")
	}
	pid := []byte(strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(filepath.Join(cg, "cgroup.procs"), pid, 0); err != nil {
		t.Fatalf("join the cgroup %s: %v", cg, err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(orig, "cgroup.procs"), pid, 0); err != nil {
			t.Errorf("go back to the cgroup %s: %v", orig, err)
		}
	})
}

// takenBy dials 127.0.0.1:port with d and wants ln to accept the connection,
// from the dialer's own address.
func takenBy(t *testing.T, d *net.Dialer, port uint16, ln net.Listener, what string) {
	t.Helper()
	takenAt(t, d, "127.0.0.1", port, ln, what)
}

// takenAt is takenBy for host:port.
func takenAt(t *testing.T, d *net.Dialer, host string, port uint16, ln net.Listener, what string) {
	t.Helper()
	c, err := d.Dial("tcp4", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		t.Fatalf("%s: dial: %v", what, err)
	}
	defer func() { _ = c.Close() }()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	a, err := ln.Accept()
	if err != nil {
		t.Fatalf("%s: the listener accepted nothing: %v", what, err)
	}
	defer func() { _ = a.Close() }()
	if a.RemoteAddr().String() != c.LocalAddr().String() {
		t.Fatalf("%s: the listener accepted a connection from %s, want the dial's from %s", what, a.RemoteAddr(), c.LocalAddr())
	}
	if p := a.LocalAddr().(*net.TCPAddr).Port; p != int(port) {
		t.Fatalf("%s: accepted on port %d, want %d, the one dialed", what, p, port)
	}
}

// ownTakenBy is takenBy for keploy's own dial (OwnDialControl), which the
// kernel must have noted as keploy's own (OwnConnNoted).
func ownTakenBy(t *testing.T, h *Hooks, port uint16, ln net.Listener, what string) {
	t.Helper()
	d := net.Dialer{Control: func(network, address string, c syscall.RawConn) error {
		return h.OwnDialControl(network, address, c)
	}}
	c, err := d.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		t.Fatalf("%s: dial: %v", what, err)
	}
	if err := h.OwnConnNoted(c); err != nil {
		_ = c.Close()
		t.Fatalf("%s: %v", what, err)
	}
	_ = c.Close()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	a, err := ln.Accept()
	if err != nil {
		t.Fatalf("%s: the listener accepted nothing: %v", what, err)
	}
	defer func() { _ = a.Close() }()
	if a.RemoteAddr().String() != c.LocalAddr().String() {
		t.Fatalf("%s: the listener accepted a connection from %s, want keploy's own from %s", what, a.RemoteAddr(), c.LocalAddr())
	}
}

// startSteeringAgent starts h's hold on cg (the lookup on the test's network
// namespace), steering, as load does once its flags are sent.
func startSteeringAgent(t *testing.T, h *Hooks, cg string) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.startIngressHold(ctx, cg)
	h.steering.Store(true) // as sendFinalAgentInfo sets it with the hold on and both post_bind hooks attached
	if !h.IngressSteers() {
		cancel()
		t.Fatal("the hold did not start, so keploy does not steer")
	}
	t.Cleanup(cancel)
	return cancel
}

// sendSteeringFlags sends h's agent info as load does, with both post_bind
// hooks attached: the flags say the hooks steer, and the restarted agent's
// steering again (startSteering) runs.
func sendSteeringFlags(t *testing.T, h *Hooks) {
	t.Helper()
	h.kernelPortAlloc = true
	h.agentRegistrationMap = h.objects.KeployAgentRegistrationMap // as load sets it
	if err := h.sendFinalAgentInfo(structs.AgentInfo{RecordingStartTime: 1}, func(*structs.AgentInfo) error { return nil }, h.SendAgentInfo); err != nil {
		t.Fatal(err)
	}
	if !h.IngressSteers() {
		t.Fatal("the flags sent do not steer")
	}
}

// The application keeps its port while keploy steers it, so the agent's death
// leaves the port to the application, and an agent restarted in the pod steers
// it again from the table the first one kept:
//   - with the first agent, a call to the app's port is the app's until the
//     agent's park is armed (its ingress accepts on it), then the park's, and
//     the agent's own dial (OwnDialControl) is the app's;
//   - the first agent keeps the port in the pod's steer table as its ingress
//     starts on it (KeepSteerTable);
//   - the first agent dies (its hold and objects closed): the same call is
//     the app's own socket's, at once;
//   - a second agent, loading fresh objects, steers it again from the table
//     as its flags are sent (sendFinalAgentInfo), and its bind events give
//     the port first, as a bind of it would: the call is the app's until its
//     park is armed, then its park's, and the app's socket is noted again by
//     its cookie, for its release to take back.
func TestARestartedAgentSteersTheAppsPortAgainAndTheOutageReachesTheApp(t *testing.T) {
	cg := holdCgroup(t)
	joinCgroup(t, cg)
	src := podShm(t, 4026531999, 4711)
	port := sharedPort(t)

	first := ingressHoldAgent(t)
	first.shmSrc = &src
	stopFirst := startSteeringAgent(t, first, cg)
	app, note := steeredApp(t, first, port)
	first.KeepSteerTable()
	var plain net.Dialer
	takenBy(t, &plain, port, app, "the app's port before the first agent's park is armed")
	armPark(t, first)
	takenBy(t, &plain, port, first.IngressParkListener(), "the first agent steers the app's port")
	ownTakenBy(t, first, port, app, "the first agent's own dial")

	// The agent dies: its sockets, programs and maps go.
	stopFirst()
	first.closeIngressHold()
	_ = first.objects.Close()
	takenBy(t, &plain, port, app, "the app's port while no agent runs")

	second := ingressHoldAgent(t)
	second.shmSrc = &src
	appIno := inodeOf(t, app.(*net.TCPListener))
	second.appSocketsFn = func() (map[uint32]bool, error) { return map[uint32]bool{appIno: true}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	second.startIngressHold(ctx, cg)
	sendSteeringFlags(t, second)
	second.BindEvents = second.objects.BindEvents // as load sets it
	events, err := second.WatchBindEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-events:
		if e.OrigAppPort != port || e.NewAppPort != port {
			t.Fatalf("the restarted agent's first bind event is %d->%d, want %d->%d: the port it steers again", e.OrigAppPort, e.NewAppPort, port, port)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the restarted agent gave no bind event of the port it steers again")
	}
	// Until its recorder connects (its park armed as the ingress accepts on
	// it), the restarted agent leaves the port to the app: nothing would
	// serve a call steered to it.
	takenBy(t, &plain, port, app, "the app's port before the restarted agent's park is armed")
	armPark(t, second)
	takenBy(t, &plain, port, second.IngressParkListener(), "the restarted agent steers the app's port again")
	ownTakenBy(t, second, port, app, "the restarted agent's own dial")
	// A connection keploy did not name as its own is not taken for one.
	pc, err := plain.Dial("tcp4", app.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.OwnConnNoted(pc); err == nil {
		t.Fatal("a connection keploy did not name was taken as noted as its own")
	}
	_ = pc.Close()
	var got appBindKey
	if err := second.objects.AppBindSocks.Lookup(listenerCookie(t, app), &got); err != nil || got != note {
		t.Fatalf("the app's socket is noted as %+v (%v), want %+v", got, err, note)
	}
	var c appBindCount
	if err := second.objects.AppPortBinds.Lookup(note, &c); err != nil || c.N != 1 {
		t.Fatalf("the app's port is counted %d (%v), want 1", c.N, err)
	}
	// Seeding again notes nothing twice.
	if _, _, err := second.seedSteering(src); err != nil {
		t.Fatal(err)
	}
	if err := second.objects.AppPortBinds.Lookup(note, &c); err != nil || c.N != 1 {
		t.Fatalf("after a second seeding the app's port is counted %d (%v), want 1", c.N, err)
	}
}

// A restarted agent steers only what the table names, and only a socket the
// application holds: a socket that listens on the app's port at an address
// the table does not name, or one another container of the pod holds where
// the table says (every container can write /dev/shm), is the kernel's.
func TestARestartedAgentSteersOnlyWhatTheTableNamesOnTheAppsSockets(t *testing.T) {
	cg := holdCgroup(t)
	src := podShm(t, 4026531999, 4711)

	port := sharedPort(t)
	other := appBindKey{Port: port, Kind: appBindV4}
	copy(other.Addr[:4], []byte{127, 0, 0, 2})
	if err := writeSteerTable(src, []appBindKey{other}); err != nil {
		t.Fatal(err)
	}
	app := listenShared(t, "tcp4", "127.0.0.1", port)
	h := ingressHoldAgent(t)
	appIno := inodeOf(t, app.(*net.TCPListener))
	h.appSocketsFn = func() (map[uint32]bool, error) { return map[uint32]bool{appIno: true}, nil }
	startSteeringAgent(t, h, cg)
	seeded, found, err := h.seedSteering(src)
	if err != nil || !found || len(seeded) != 0 {
		t.Fatalf("seeded %v (table found %v, %v), want nothing", seeded, found, err)
	}
	var plain net.Dialer
	takenBy(t, &plain, port, app, "a port the table does not name")

	sibling := sharedPort(t)
	mine := appBindKey{Port: sibling, Kind: appBindV4}
	copy(mine.Addr[:4], []byte{127, 0, 0, 1})
	if err := writeSteerTable(src, []appBindKey{mine}); err != nil {
		t.Fatal(err)
	}
	foreign := listenShared(t, "tcp4", "127.0.0.1", sibling) // not the app's
	seeded, _, err = h.seedSteering(src)
	if err != nil || len(seeded) != 0 {
		t.Fatalf("seeded %v (%v), want nothing: the socket is not the application's", seeded, err)
	}
	var k appBindKey
	if err := h.objects.AppBindSocks.Lookup(listenerCookie(t, foreign), &k); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("another container's socket is noted as %+v (%v), want it not noted", k, err)
	}

	// A socket that went between the read of the socket table and its note
	// had no release to take the note back: the seeding takes it back.
	gone := listenShared(t, "tcp4", "127.0.0.1", sibling)
	ck := listenerCookie(t, gone)
	if err := h.objects.AppBindSocks.Update(ck, mine, ebpf.UpdateNoExist); err != nil {
		t.Fatal(err)
	}
	if err := h.objects.AppPortBinds.Update(mine, appBindCount{N: 1}, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	_ = gone.Close()
	_ = h.objects.AppBindSocks.Update(ck, mine, ebpf.UpdateAny) // as if noted after it went
	h.ingressMu.RLock()
	n := unseedGone(h.ingress, sibling, []seededSock{{cookie: ck, note: mine}})
	h.ingressMu.RUnlock()
	var c appBindCount
	if err := h.objects.AppPortBinds.Lookup(mine, &c); n != 1 || err != nil || c.N != 0 {
		t.Fatalf("took back %d note(s); the port is counted %d (%v), want 1 and 0", n, c.N, err)
	}
	if err := h.objects.AppBindSocks.Lookup(ck, &k); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("the gone socket is still noted as %+v (%v)", k, err)
	}

	// A connection keploy named as its own that the kernel did not note
	// (this test's process is outside the hold's cgroup, where the sock_ops
	// program is) is not taken as noted: it may have been steered back.
	d := net.Dialer{Control: h.OwnDialControl}
	c2, err := d.Dial("tcp4", app.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if err := h.OwnConnNoted(c2); err == nil {
		t.Fatal("a named connection the kernel did not note was taken as noted")
	}
}

// A socket the application binds on a port keploy steers already (here on
// another address of the port; an IPv6 listener, or an admin listener on one
// address, are alike) is kept in the pod's steer table at its own bind event,
// as the port's first socket was at the port's, by the ingress manager that
// takes the events; so an agent restarted in the pod steers it again too.
func TestASocketBoundLaterOnASteeredPortIsSteeredAgainAfterARestart(t *testing.T) {
	cg := holdCgroup(t)
	joinCgroup(t, cg)
	src := podShm(t, 4026531999, 4711)
	port := sharedPort(t)

	first := ingressHoldAgent(t)
	first.shmSrc = &src
	stopFirst := startSteeringAgent(t, first, cg)
	pm := incoming.New(zap.NewNop(), first, &config.Config{})
	app, _ := steeredApp(t, first, port)
	pm.StartIngressProxy(context.Background(), port, port) // the port's first bind event
	later := listenShared(t, "tcp4", "127.0.0.2", port)
	laterNote := noteListenerAt(t, first, later, [4]byte{127, 0, 0, 2}, port)
	pm.StartIngressProxy(context.Background(), port, port) // the later socket's bind event
	pm.StopAll()
	stopFirst()
	first.closeIngressHold()
	_ = first.objects.Close()

	second := ingressHoldAgent(t)
	second.shmSrc = &src
	inodes := map[uint32]bool{inodeOf(t, app.(*net.TCPListener)): true, inodeOf(t, later.(*net.TCPListener)): true}
	second.appSocketsFn = func() (map[uint32]bool, error) { return inodes, nil }
	startSteeringAgent(t, second, cg)
	seeded, found, err := second.seedSteering(src)
	if err != nil || !found || len(seeded) != 1 || seeded[0] != port {
		t.Fatalf("seeded %v (table found %v, %v), want the port %d", seeded, found, err, port)
	}
	var got appBindKey
	if err := second.objects.AppBindSocks.Lookup(listenerCookie(t, later), &got); err != nil || got != laterNote {
		t.Fatalf("the later socket is noted as %+v (%v), want %+v: the table did not keep it", got, err, laterNote)
	}
	armPark(t, second)
	var plain net.Dialer
	takenAt(t, &plain, "127.0.0.2", port, second.IngressParkListener(), "the restarted agent steers the later socket's address again")
	takenBy(t, &plain, port, second.IngressParkListener(), "the restarted agent steers the first socket's address again")
}

// What the application opened while no agent ran is not seen by the agent
// after it: a caller's keep-alive connection to the app's port is not steered
// when its next request comes (sk_lookup sees SYNs only), and the app's
// pooled connection to a dependency was never redirected. keploy never ends
// the app's connections, so the restarted agent counts them as it records
// again (its park armed), and charges each read at which a connection to a
// dependency carried data (CaptureLossUnseenConnection), which leaves the
// test it fell in without a verdict; the calls on a caller's connection are
// counted for the log. A connection that closes between two reads is charged
// by what it carried up to its close (its end, as the kernel sends it), so a
// call on a connection the dependency closes is never lost uncharged, and an
// idle one that closes is not charged; it is watched no more. The sockets are
// the kernel's own, read through sock_diag.
func TestConnectionsOpenedWhileNoAgentRanAreCountedAndTheirDataCharged(t *testing.T) {
	cg := holdCgroup(t)
	port := sharedPort(t)

	first := ingressHoldAgent(t)
	stopFirst := startSteeringAgent(t, first, cg)
	app, _ := steeredApp(t, first, port)
	armPark(t, first)
	stopFirst()
	first.closeIngressHold()
	_ = first.objects.Close()

	// The outage: a caller connects to the app, and the app to a dependency.
	caller, err := net.Dial("tcp4", app.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = caller.Close() }()
	_ = app.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	inbound, err := app.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbound.Close() }()
	dep, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dep.Close() }()
	pooled, err := net.Dial("tcp4", dep.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pooled.Close() }()
	depEnd, err := dep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = depEnd.Close() }()
	idle, err := net.Dial("tcp4", dep.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idle.Close() }()
	idleEnd, err := dep.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idleEnd.Close() }()

	second := ingressHoldAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	second.startIngressHold(ctx, cg)
	second.steering.Store(true)
	noteListener(t, second, app, port)
	appInodes := map[uint32]bool{
		inodeOf(t, app.(*net.TCPListener)): true,
		inodeOf(t, inbound.(*net.TCPConn)): true,
		inodeOf(t, pooled.(*net.TCPConn)):  true,
		inodeOf(t, idle.(*net.TCPConn)):    true,
	}
	second.unseen.testSource = &unseenSource{
		sockets: util.TCPSockets,
		ends:    watchTCPEnds,
		app:     func() (map[uint32]bool, error) { return appInodes, nil },
		own: func() (map[uint32]bool, error) {
			return map[uint32]bool{inodeOf(t, second.IngressParkListener().(*net.TCPListener)): true}, nil
		},
	}
	armPark(t, second)
	second.unseen.mu.Lock()
	nIn, nOut := len(second.unseen.inbound), len(second.unseen.egress)
	second.unseen.mu.Unlock()
	if nIn != 1 || nOut != 2 {
		t.Fatalf("the census found %d caller connection(s) and %d dependency connection(s), want 1 and 2", nIn, nOut)
	}
	if l := second.unseenLosses(); len(l) != 0 {
		t.Fatalf("losses %+v before any data, want none", l)
	}

	// The caller's next request on its connection: the app's, not steered,
	// so not recorded; counted for the log.
	if _, err := io.WriteString(caller, "GET /after-rearm HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = inbound.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := inbound.Read(make([]byte, 64)); err != nil {
		t.Fatalf("the app did not get the caller's request on its own connection: %v", err)
	}
	// The app calls its dependency over the pooled connection.
	if _, err := io.WriteString(pooled, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	_ = depEnd.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := depEnd.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	var l models.CaptureLosses
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		// The bytes are counted once acknowledged.
		if l = second.unseenLosses(); len(l) == 1 {
			break
		}
	}
	if len(l) != 1 || l[0].Kind != models.CaptureLossUnseenConnection || l[0].Count != 1 || !strings.Contains(l[0].What, dep.Addr().String()) {
		t.Fatalf("losses %+v, want one %s naming the dependency %s", l, models.CaptureLossUnseenConnection, dep.Addr())
	}
	second.unseen.mu.Lock()
	in := second.unseen.callsIn
	second.unseen.mu.Unlock()
	if in != 1 {
		t.Fatalf("calls on callers' connections counted %d, want 1", in)
	}
	if l := second.unseenLosses(); l[0].Count != 1 {
		t.Fatalf("losses %+v after a read with no data, want the count unchanged", l)
	}

	// The dependency answers the next call on the pooled connection and
	// closes it (Connection: close), and the test's end read comes only
	// after the connection has gone from the socket table: what it carried
	// up to its close is charged.
	pooledCookie, idleCookie := cookieOf(t, pooled.(*net.TCPConn)), cookieOf(t, idle.(*net.TCPConn))
	if _, err := io.WriteString(pooled, "SELECT 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(depEnd, make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(depEnd, "1 row"); err != nil {
		t.Fatal(err)
	}
	_ = depEnd.Close()
	_ = pooled.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(pooled, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	_ = pooled.Close()
	goneFromTable(t, pooledCookie)
	if l := second.unseenLosses(); len(l) != 1 || l[0].Count != 2 || !strings.Contains(l[0].What, dep.Addr().String()) || !strings.Contains(l[0].What, "closed since") {
		t.Fatalf("losses %+v after a call on a connection that closed before the read, want the count 2 naming the closed connection to %s", l, dep.Addr())
	}

	// An idle pooled connection that closes carried nothing since: not
	// charged, and watched no more. The ends are still watched: the
	// caller's connection still is.
	_ = idleEnd.Close()
	_ = idle.Close()
	goneFromTable(t, idleCookie)
	if l := second.unseenLosses(); len(l) != 1 || l[0].Count != 2 {
		t.Fatalf("losses %+v after an idle connection closed, want the count unchanged at 2", l)
	}
	second.unseen.mu.Lock()
	n, ends := len(second.unseen.egress), second.unseen.ends
	second.unseen.mu.Unlock()
	if n != 0 || ends == nil {
		t.Fatalf("%d closed dependency connection(s) still watched, the ends watched: %v; want none, and the ends watched for the caller's", n, ends != nil)
	}

	// The caller's connection closes with nothing more on it: not counted
	// again, and with nothing left to watch the ends are not watched.
	callerCookie := cookieOf(t, inbound.(*net.TCPConn))
	_ = caller.Close()
	_ = inbound.Close()
	goneFromTable(t, callerCookie)
	second.unseenLosses()
	second.unseen.mu.Lock()
	in, n, ends = second.unseen.callsIn, len(second.unseen.inbound), second.unseen.ends
	second.unseen.mu.Unlock()
	if in != 1 || n != 0 || ends != nil {
		t.Fatalf("after the caller's connection closed: calls counted %d, %d still watched, ends watched %v; want 1, none, not", in, n, ends != nil)
	}
}

// goneFromTable waits until the socket of cookie is in none of the states a
// read of the losses finds it in (unseenLiveStates).
func goneFromTable(t *testing.T, cookie uint64) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		socks, err := util.TCPSockets(unseenLiveStates)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range socks {
			found = found || s.Cookie == cookie
		}
		if !found {
			return
		}
	}
	t.Fatalf("the socket %d is still in the table 10s after its close", cookie)
}

// keploy's own connections to the application never run out of source ports
// where the application's own callers would not: nothing is bound before the
// connect, so the kernel picks each one's port as for any connect() and
// reuses one a closed connection left in TIME_WAIT (net.ipv4.tcp_tw_reuse on
// the loopback). In a 64-port range, two rounds of 64 of keploy's own
// connections, 2.5 s apart, each closed by keploy first, all connect to the
// application's socket. Binding each to its source before the connect kept
// every port to itself through its TIME_WAIT: 66 of these 128 failed with
// "address already in use".
func TestKeploysOwnDialsDoNotRunOutOfSourcePorts(t *testing.T) {
	if !inOwnPIDAndNetNamespaces(t) {
		return
	}
	reuse, _ := os.ReadFile("/proc/sys/net/ipv4/tcp_tw_reuse")
	cg := holdCgroup(t)
	joinCgroup(t, cg)
	h := ingressHoldAgent(t)
	startSteeringAgent(t, h, cg)
	const port = 8080
	app, _ := steeredApp(t, h, port)
	armPark(t, h)
	// The range is set once the park listens (its port is outside it): the
	// 64 ports are the dials' alone.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_local_port_range", []byte("40000 40063"), 0); err != nil {
		t.Fatal(err)
	}
	var parked atomic.Int32
	go func() {
		for {
			c, err := h.IngressParkListener().Accept()
			if err != nil {
				return
			}
			parked.Add(1)
			_ = c.Close()
		}
	}()
	go func() {
		for {
			c, err := app.Accept()
			if err != nil {
				return
			}
			go func() { // the app closes once keploy has
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	}()
	d := net.Dialer{Control: h.OwnDialControl}
	ok, failed := 0, 0
	var first error
	for round := 0; round < 2; round++ {
		if round > 0 {
			time.Sleep(2500 * time.Millisecond)
		}
		for i := 0; i < 64; i++ {
			c, err := d.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				err = h.OwnConnNoted(c)
			}
			if err != nil {
				failed++
				if first == nil {
					first = err
				}
				if c != nil {
					_ = c.Close()
				}
				continue
			}
			ok++
			_ = c.Close()
		}
	}
	t.Logf("tcp_tw_reuse=%s, 64-port range, 2 rounds of 64 of keploy's own dials 2.5 s apart, keploy closing first: %d ok, %d failed (%v), %d steered back to the park", strings.TrimSpace(string(reuse)), ok, failed, first, parked.Load())
	if failed != 0 || parked.Load() != 0 {
		t.Fatalf("keploy's own dials failed %d of 128 (first: %v), and %d came back to its park", failed, first, parked.Load())
	}
}

// steerAgentEnv names, in a helper agent process, the app's port it steers;
// steerAgentCgroupEnv the cgroup its hold attaches to.
const (
	steerAgentEnv       = "KEPLOY_STEER_AGENT_PORT"
	steerAgentCgroupEnv = "KEPLOY_STEER_AGENT_CGROUP"
	// steerAgentHeapEnv: the MiB of memory the helper agent holds, touched,
	// as a running agent does (its tear-down at its death takes time that
	// grows with it).
	steerAgentHeapEnv = "KEPLOY_STEER_AGENT_HEAP_MIB"
	// steerAgentOnlyEnv: "kill", the helper agent detaches its exit program
	// once it is ready, so only its SIGKILL program can mark it exiting;
	// "exit", its SIGKILL program, so only its exit program can.
	steerAgentOnlyEnv = "KEPLOY_STEER_AGENT_ONLY"
)

// steerAgentHeap is the helper agent's memory (steerAgentHeapEnv), kept for
// its life.
var steerAgentHeap []byte

// TestSteerAgentHelper is the agent process of the kill row
// (TestAKilledAgentProcessLeavesTheAppsPortToTheApp), run alone by it: it
// steers the app's port, as keploy's agent does, forwarding each steered call
// to the app from a connection of its own, and says "ready" once its park is
// armed. It runs until it is killed.
func TestSteerAgentHelper(t *testing.T) {
	p := os.Getenv(steerAgentEnv)
	if p == "" {
		t.Skip("the kill row's agent process")
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	if mib, _ := strconv.Atoi(os.Getenv(steerAgentHeapEnv)); mib > 0 {
		steerAgentHeap = make([]byte, mib<<20)
		for i := 0; i < len(steerAgentHeap); i += os.Getpagesize() {
			steerAgentHeap[i] = 1
		}
	}
	h := ingressHoldAgent(t)
	startSteeringAgent(t, h, os.Getenv(steerAgentCgroupEnv))
	socks, err := util.TCPListenSockets(uint16(port))
	if err != nil || len(socks) != 1 {
		t.Fatalf("the app's listener on %d: %v, %v", port, socks, err)
	}
	k := appBindKey{Port: uint16(port), Kind: appBindV4}
	copy(k.Addr[:4], []byte{127, 0, 0, 1})
	if err := h.objects.AppBindSocks.Update(socks[0].Cookie, k, ebpf.UpdateNoExist); err != nil {
		t.Fatal(err)
	}
	if err := h.objects.AppPortBinds.Update(k, appBindCount{N: 1}, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	armPark(t, h)
	var who ingressAgent
	h.ingressMu.RLock()
	err = h.ingress.coll.Maps["ingress_agent"].Lookup(uint32(0), &who)
	h.ingressMu.RUnlock()
	if err != nil || who.KernelTgid == 0 || who.Exiting != 0 {
		t.Fatalf("the agent is named to its programs as %+v (%v), want its kernel TGID learnt and it not exiting", who, err)
	}
	if only := os.Getenv(steerAgentOnlyEnv); only != "" {
		h.ingressMu.RLock()
		l := h.ingress.agentExit
		if only == "exit" {
			l = h.ingress.agentKill
		}
		err := l.Close()
		h.ingressMu.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("ready")
	d := net.Dialer{Control: h.OwnDialControl}
	for {
		c, err := h.IngressParkListener().Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			a, err := d.Dial("tcp4", c.LocalAddr().String())
			if err != nil {
				return
			}
			defer func() { _ = a.Close() }()
			if h.OwnConnNoted(a) != nil {
				return
			}
			go func() { _, _ = io.Copy(a, c); _ = a.(*net.TCPConn).CloseWrite() }()
			_, _ = io.Copy(c, a)
		}()
	}
}

// The agent is a process of its own, holding 1 GiB of memory as a running
// agent holds hundreds of MiB: SIGKILLed (what the OOM killer sends) while
// callers call the app, both as fast as 8 callers can, each waiting for its
// answer, and at a fixed rate whatever the answers (one call begun every
// 500 µs), it leaves the app's port to the app from the kill on, not only
// from its end: the agent's end comes only after its memory is torn down, a
// time that grows with its memory, and its park listens until then. No call
// is refused. A call lost is one in flight through the agent at the kill: none
// begun more than 1 ms after the signal was sent, and at most one of each
// waiting caller's. Every call begun after that, and every one after the
// agent's end, is the app's own socket's directly. Before the kill, calls go
// through the agent. Moving the bind, the agent's death left nothing on the
// port: every call refused until the app was restarted.
func TestAKilledAgentProcessLeavesTheAppsPortToTheApp(t *testing.T) {
	if !inOwnPIDAndNetNamespaces(t) {
		return
	}
	killedAgentRow(t, "")
}

// The SIGKILL alone marks the agent exiting as it is sent, before any of its
// threads runs: the kill row with the agent's exit program detached once it is
// ready (its kernel TGID learnt), so that nothing else can. An agent whose
// threads cannot run at once at its kill (a CPU-throttled container) could
// otherwise be steered to until the first of them did.
func TestAKilledAgentIsSteeredToNoMoreFromItsSIGKILLsSend(t *testing.T) {
	if !inOwnPIDAndNetNamespaces(t) {
		return
	}
	killedAgentRow(t, "kill")
}

// Without the SIGKILL program, the agent is marked exiting as the first of
// its threads begins its thread group's exit (as on any other fatal signal,
// or its own exit): the kill row with the agent's SIGKILL program detached
// once it is ready, so that nothing else can.
func TestAKilledAgentIsSteeredToNoMoreFromItsFirstThreadsExit(t *testing.T) {
	if !inOwnPIDAndNetNamespaces(t) {
		return
	}
	killedAgentRow(t, "exit")
}

// killedAgentRow is the kill row; with only "kill" or "exit", the agent's
// other program detached once it is ready.
func killedAgentRow(t *testing.T, only string) {
	sig := syscall.SIGKILL
	cg := holdCgroup(t)
	port := sharedPort(t)
	app := listenShared(t, "tcp4", "127.0.0.1", port)
	// The app answers each call with the port it came from.
	go func() {
		for {
			c, err := app.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
					return
				}
				_, _ = fmt.Fprintf(c, "%d\n", c.RemoteAddr().(*net.TCPAddr).Port)
			}()
		}
	}()

	cgf, err := os.Open(cg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cgf.Close() }()
	agent := exec.Command("/proc/self/exe", "-test.run=^TestSteerAgentHelper$", "-test.v")
	agent.Env = append(os.Environ(), steerAgentEnv+"="+strconv.Itoa(int(port)), steerAgentCgroupEnv+"="+cg, steerAgentHeapEnv+"=1024")
	if only != "" {
		agent.Env = append(agent.Env, steerAgentOnlyEnv+"="+only)
	}
	agent.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgf.Fd()), Pdeathsig: syscall.SIGKILL}
	agent.Stderr = os.Stderr
	out, err := agent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = agent.Wait(); close(waited) }()
	t.Cleanup(func() { _ = agent.Process.Kill(); <-waited })
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == "ready" {
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-waited:
		t.Fatal("the agent process ended before it was ready")
	case <-time.After(60 * time.Second):
		t.Fatal("the agent process was not ready in 60 s")
	}

	// call is one call: "direct" (the app's socket took the caller's own
	// connection), "agent" (the agent forwarded it), "refused" or "lost".
	call := func() string {
		c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				return "refused"
			}
			return "lost"
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, "call\n"); err != nil {
			return "lost"
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			return "lost"
		}
		if strings.TrimSpace(line) == strconv.Itoa(c.LocalAddr().(*net.TCPAddr).Port) {
			return "direct"
		}
		return "agent"
	}
	if got := call(); got != "agent" {
		t.Fatalf("a call before the kill was %s, want it forwarded by the agent", got)
	}

	// Each call, by when it began: its caller (-1 for the fixed-rate
	// ones), its start and its result.
	type callAt struct {
		caller int
		begun  time.Time
		r      string
	}
	var (
		mu    sync.Mutex
		calls []callAt
		stop  = make(chan struct{})
		wg    sync.WaitGroup
	)
	record := func(caller int, begun time.Time, r string) {
		mu.Lock()
		calls = append(calls, callAt{caller, begun, r})
		mu.Unlock()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				begun := time.Now()
				record(i, begun, call())
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for next := time.Now(); ; next = next.Add(500 * time.Microsecond) {
			select {
			case <-stop:
				return
			case <-time.After(time.Until(next)):
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				begun := time.Now()
				record(-1, begun, call())
			}()
		}
	}()
	time.Sleep(500 * time.Millisecond)
	if err := agent.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	killed := time.Now() // the signal sent
	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		close(stop)
		wg.Wait()
		t.Fatalf("the agent did not end within 30 s of its %v", sig)
	}
	ended := time.Now()
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()

	var (
		before, dying, after = map[string]int{}, map[string]int{}, map[string]int{}
		lostOf               = map[int]int{}
		late                 []string
		firstDirect          time.Duration = -1
		lostAt               []time.Duration
	)
	for _, c := range calls {
		at := c.begun.Sub(killed)
		switch {
		case c.begun.Before(killed):
			before[c.r]++
		case c.begun.Before(ended):
			dying[c.r]++
		default:
			after[c.r]++
		}
		if c.r == "lost" {
			lostAt = append(lostAt, at)
			if c.caller >= 0 {
				lostOf[c.caller]++
			}
		}
		if at > time.Millisecond && c.r != "direct" {
			late = append(late, fmt.Sprintf("%s begun %v after the kill", c.r, at))
		}
		if c.r == "direct" && at >= 0 && (firstDirect < 0 || at < firstDirect) {
			firstDirect = at
		}
	}
	t.Logf("the agent ended %v after its %v was sent; calls before the kill %v, between it and the agent's end %v, after it (2 s) %v; lost calls begun at %v from the kill; the first direct call begun %v after it",
		ended.Sub(killed), sig, before, dying, after, lostAt, firstDirect)
	if before["refused"]+dying["refused"]+after["refused"] != 0 {
		t.Fatal("a call to the app's port was refused")
	}
	if len(late) > 0 {
		t.Fatalf("calls begun more than 1 ms after the agent's %v was sent did not reach the app's own socket: %v", sig, late)
	}
	for caller, n := range lostOf {
		if n > 1 {
			t.Fatalf("caller %d lost %d calls, want at most the one it had in flight through the agent", caller, n)
		}
	}
	if after["lost"] != 0 || after["agent"] != 0 || after["direct"] == 0 {
		t.Fatalf("calls after the agent's end %v, want every one the app's own socket's, answered", after)
	}
}
