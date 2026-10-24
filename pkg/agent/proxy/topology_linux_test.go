//go:build linux

package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// rowSink is a TopologySink that keeps what it is told.
type rowSink struct {
	mu     sync.Mutex
	live   []models.LiveRow
	mocked []models.MockedRow
}

func (s *rowSink) NoteLive(r models.LiveRow) {
	s.mu.Lock()
	s.live = append(s.live, r)
	s.mu.Unlock()
}

func (s *rowSink) NoteMocked(r models.MockedRow) {
	s.mu.Lock()
	s.mocked = append(s.mocked, r)
	s.mu.Unlock()
}

func (s *rowSink) rows() ([]models.LiveRow, []models.MockedRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.LiveRow(nil), s.live...), append([]models.MockedRow(nil), s.mocked...)
}

// pingServer listens on loopback and answers each connection's "ping" with
// "pong". This test process owns it.
func pingServer(t *testing.T) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				_, _ = c.Write([]byte("pong"))
			}()
		}
	}()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

// callTree sends "ping" through serveTreeListener to the loopback port as
// the process caller, and reports whether the call was relayed and, if it
// was, the listener's answer, else the mocked-local row it left to list. The
// caller's end closes once it has the answer (the relay ends when both ends
// have), or once serveTreeListener left the call to the mock path.
func callTree(t *testing.T, p *Proxy, port, caller uint32) (bool, string, *models.MockedRow) {
	t.Helper()
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: caller}
	cli, srv := net.Pipe()
	type result struct {
		served  bool
		outside *models.MockedRow
		err     error
	}
	done := make(chan result, 1)
	go func() {
		served, outside, err := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
		_ = srv.Close()
		done <- result{served, outside, err}
	}()
	answer := make(chan string, 1)
	go func() {
		if _, err := cli.Write([]byte("ping")); err != nil {
			answer <- ""
			return
		}
		buf := make([]byte, 4)
		n, _ := io.ReadFull(cli, buf)
		answer <- string(buf[:n])
	}()
	var got string
	var r result
	select {
	case got = <-answer:
		_ = cli.Close()
		r = awaitEvent(t, done, "serveTreeListener returning once the caller closed")
	case r = <-done:
		_ = cli.Close()
		got = <-answer
	}
	if r.err != nil {
		t.Fatalf("serveTreeListener: %v", r.err)
	}
	if !r.served {
		return false, "", r.outside
	}
	if r.outside != nil {
		t.Fatalf("a relayed call left a mocked-local row %+v", r.outside)
	}
	return true, got, nil
}

// hold starts a process that runs until the test ends: a process of the
// run's tree that is not this one.
func hold(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("cat")
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = c.Wait()
	})
	return c
}

func provenTreeProxy(t *testing.T) (*Proxy, *rowSink) {
	t.Helper()
	recorded.reset()
	t.Cleanup(recorded.reset)
	t.Setenv("KEPLOY_PROVEN", "1")
	p := selfCallProxy()
	s := &rowSink{}
	p.SetTopologySink(s)
	return p, s
}

// Under the proven switch a listener of the run's own tree is live whatever
// a recording says: the exception for a port the recordings called as a
// dependency (RecordedPort) is gone (design v14 §3.1).
func TestUnderTheSwitchATreeListenerOnARecordedPortIsLive(t *testing.T) {
	p, _ := provenTreeProxy(t)
	port := pingServer(t)
	recorded.add([]*models.Mock{recordedPlainDep(port)})
	if !RecordedPort(port) {
		t.Fatalf("precondition: port %d should be a recorded outside dependency", port)
	}
	served, got, _ := callTree(t, p, port, uint32(os.Getpid()))
	if !served || got != "pong" {
		t.Fatalf("a listener of the run's tree must be relayed under the switch: served=%v answer=%q", served, got)
	}
}

// Under the proven switch a mock.real destination that a listener of the
// run's own tree serves is reached: the call is relayed to the tree's
// listener, live, and mock.real's account of the run (RealRun) counts it, as
// it counts a relay to the destination itself (serveReal). It was not
// counted, so a run whose recordings list that destination failed as never
// reaching it (mock.real's rule 4, REAL_NOT_REACHED). A tree listener that is
// no mock.real destination is not counted.
func TestUnderTheSwitchAMockRealDestinationTheTreeServesIsReached(t *testing.T) {
	p, _ := provenTreeProxy(t)
	port := pingServer(t)
	other := pingServer(t)
	listed := fmt.Sprintf("127.0.0.1:%d", port)
	if err := p.SetRealDeps(context.Background(), []string{listed}); err != nil {
		t.Fatal(err)
	}
	if served, got, _ := callTree(t, p, other, uint32(os.Getpid())); !served || got != "pong" {
		t.Fatalf("precondition: a call to the tree's listener is relayed: served=%v answer=%q", served, got)
	}
	if run := p.RealRun(); len(run.Reached) != 0 {
		t.Fatalf("a tree listener mock.real does not list was counted reached: %+v", run.Reached)
	}
	if served, got, _ := callTree(t, p, port, uint32(os.Getpid())); !served || got != "pong" {
		t.Fatalf("a call to the tree's listener of a mock.real destination is relayed: served=%v answer=%q", served, got)
	}
	if run := p.RealRun(); len(run.Reached) != 1 || run.Reached[0] != listed {
		t.Fatalf("the mock.real destination the tree served was not counted reached: %+v, want [%s]", run.Reached, listed)
	}
}

// silentServer listens on loopback and reads each connection's four bytes,
// then closes it without answering. This test process owns it.
func silentServer(t *testing.T) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.ReadFull(c, make([]byte, 4))
			}()
		}
	}()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

// sendTree sends "ping" through serveTreeListener to the loopback port as
// the process caller and closes the caller's end at once, waiting for no
// answer, and returns what serveTreeListener did.
func sendTree(ctx context.Context, t *testing.T, p *Proxy, port, caller uint32) (bool, error) {
	t.Helper()
	cli, srv := net.Pipe()
	type result struct {
		served bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: caller}
		served, _, err := p.serveTreeListener(ctx, srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "7")
		_ = srv.Close()
		done <- result{served, err}
	}()
	go func() {
		_, _ = cli.Write([]byte("ping"))
		_ = cli.Close()
	}()
	r := awaitEvent(t, done, "serveTreeListener returning")
	return r.served, r.err
}

// A mock.real destination the tree serves is counted as relayReal counts
// one: the open test's REAL row has each connection that sent it bytes, a
// dial that connects reaches it whether or not the listener ever answers, a
// dial that fails counts in the row and reaches nothing, and a call the live
// handler serves, which dials for itself, reaches it by the listener's
// answer.
func TestUnderTheSwitchATreeServedMockRealDestinationIsCountedAsARelay(t *testing.T) {
	p, _ := provenTreeProxy(t)
	silent := silentServer(t)
	listed := fmt.Sprintf("127.0.0.1:%d", silent)
	live := pingServer(t)
	liveListed := fmt.Sprintf("127.0.0.1:%d", live)
	if err := p.SetRealDeps(context.Background(), []string{listed, liveListed}); err != nil {
		t.Fatal(err)
	}
	tr := p.RealTracker()

	tr.BeginTest()
	if served, err := sendTree(context.Background(), t, p, silent, uint32(os.Getpid())); !served || err != nil {
		t.Fatalf("precondition: a call to the tree's listener is relayed: served=%v err=%v", served, err)
	}
	uses, _ := tr.EndTest()
	if want := []models.RealUse{{Dest: listed, Conns: 1}}; !slices.Equal(uses, want) {
		t.Fatalf("the open test's REAL row: %+v, want %+v", uses, want)
	}
	if run := p.RealRun(); !slices.Equal(run.Reached, []string{listed}) {
		t.Fatalf("a dial that connected to a listener that never answers did not reach %s: %+v", listed, run.Reached)
	}

	tr.BeginTest()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if served, err := sendTree(ctx, t, p, silent, uint32(os.Getpid())); !served || err == nil {
		t.Fatalf("precondition: the dial fails: served=%v err=%v", served, err)
	}
	uses, _ = tr.EndTest()
	if want := []models.RealUse{{Dest: listed, Failed: 1}}; !slices.Equal(uses, want) {
		t.Fatalf("a dial that failed: the open test's REAL row %+v, want %+v", uses, want)
	}
	if run := p.RealRun(); len(run.Reached) != 0 {
		t.Fatalf("a dial that failed reached %+v", run.Reached)
	}

	p.live = func(_ context.Context, conn net.Conn, upstream string, _ uint16) {
		up, err := net.Dial("tcp", upstream)
		if err != nil {
			return
		}
		defer up.Close()
		util.RelayRawPassthrough(conn, up)
	}
	// The listener is this process's; its caller is this process's parent,
	// which it descends from.
	if served, got, _ := callTree(t, p, live, uint32(os.Getppid())); !served || got != "pong" {
		t.Fatalf("precondition: a call to an app the caller started goes to the live handler: served=%v answer=%q", served, got)
	}
	if run := p.RealRun(); !slices.Equal(run.Reached, []string{liveListed}) {
		t.Fatalf("the listener's answer through the live handler did not reach %s: %+v", liveListed, run.Reached)
	}
}

// A mock.real loopback destination nothing listens at is mock.real's:
// serveTreeListener leaves it to serveReal, whose failed dial the open
// test's REAL row counts, where it closed the call uncounted, so the row
// depended on whether the host could hold handshakes (realDialFailed).
func TestUnderTheSwitchAMockRealDestinationNothingListensAtIsCountedFailed(t *testing.T) {
	p, _ := provenTreeProxy(t)
	port := uint32(mustPort(t, testnet.RefusedAddr(t, "tcp")))
	listed := fmt.Sprintf("127.0.0.1:%d", port)
	if err := p.SetRealDeps(context.Background(), []string{listed}); err != nil {
		t.Fatal(err)
	}
	if served, _, row := callTree(t, p, port, uint32(os.Getpid())); served || row != nil {
		t.Fatalf("a mock.real destination nothing listens at must be left to serveReal: served=%v row=%+v", served, row)
	}
	tr := p.RealTracker()
	tr.BeginTest()
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })
	if handled, _, err := p.serveReal(context.Background(), srv, listed, "", 1, nil); !handled || err != nil {
		t.Fatalf("serveReal: handled=%v err=%v", handled, err)
	}
	uses, _ := tr.EndTest()
	if want := []models.RealUse{{Dest: listed, Failed: 1}}; !slices.Equal(uses, want) {
		t.Fatalf("the open test's REAL row: %+v, want %+v", uses, want)
	}
}

// A call the live handler serves to a mock.real destination the tree serves
// is the open test's REAL use, and keeps no use point: keploy mock's request
// capture records that call as a test case itself, whose REAL list
// (UsedDuring, from its request's arrival to its response) must not name the
// destination it is the call to.
func TestUnderTheSwitchALiveHandlersCallNamesNoTestCaseByItsOwnBytes(t *testing.T) {
	p, _ := provenTreeProxy(t)
	port := pingServer(t)
	listed := fmt.Sprintf("127.0.0.1:%d", port)
	if err := p.SetRealDeps(context.Background(), []string{listed}); err != nil {
		t.Fatal(err)
	}
	tr := p.RealTracker()
	tr.KeepUses(true)
	t.Cleanup(func() { tr.KeepUses(false) })
	var from, to time.Time
	// As the request capture reads it: the head, then the rest of the
	// request, the arrival of the head's bytes being the test case's start.
	p.live = func(_ context.Context, conn net.Conn, upstream string, _ uint16) {
		head := make([]byte, 2)
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		from = time.Now()
		body := make([]byte, 2)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		up, err := net.Dial("tcp", upstream)
		if err != nil {
			return
		}
		defer up.Close()
		_, _ = up.Write(append(head, body...))
		answer := make([]byte, 4)
		if _, err := io.ReadFull(up, answer); err != nil {
			return
		}
		_, _ = conn.Write(answer)
		to = time.Now()
	}
	tr.BeginTest()
	if served, got, _ := callTree(t, p, port, uint32(os.Getppid())); !served || got != "pong" {
		t.Fatalf("precondition: a call to an app the caller started goes to the live handler: served=%v answer=%q", served, got)
	}
	uses, _ := tr.EndTest()
	if want := []models.RealUse{{Dest: listed, Conns: 1}}; !slices.Equal(uses, want) {
		t.Fatalf("the open test's REAL row: %+v, want %+v", uses, want)
	}
	if got := tr.UsedDuring(from, to); len(got) != 0 {
		t.Fatalf("the call's own test case was named a user of %v", got)
	}
}

// A tree listener on the other loopback family is reached through
// DialDestination's fallback, at an address an IP-literal mock.real entry
// does not list: the entry is not counted reached by it, as relayReal, which
// dials exactly the app's address, never counts it. Nothing listens at the
// listed address.
func TestUnderTheSwitchAnIPEntryIsNotReachedThroughTheOtherLoopbackFamily(t *testing.T) {
	p, _ := provenTreeProxy(t)
	var l net.Listener
	for range 10 {
		l6, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Skipf("no IPv6 loopback here: %v", err)
		}
		port := l6.Addr().(*net.TCPAddr).Port
		// Nothing may listen at the listed address, 127.0.0.1 at that
		// port, for the test's life: the port is held at every IPv4
		// address, beside the IPv6-only listener.
		if testnet.HoldPort(t, port) {
			l = l6
			break
		}
		_ = l6.Close()
	}
	if l == nil {
		t.Fatal("found no port free at every IPv4 address for a listener at [::1]")
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if _, err := io.ReadFull(c, make([]byte, 4)); err == nil {
					_, _ = c.Write([]byte("pong"))
				}
			}()
		}
	}()
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	listed := fmt.Sprintf("127.0.0.1:%d", port)
	if err := p.SetRealDeps(context.Background(), []string{listed}); err != nil {
		t.Fatal(err)
	}
	if served, _, _ := callTree(t, p, port, uint32(os.Getpid())); !served {
		t.Fatal("precondition: a call to the tree's listener is served")
	}
	if run := p.RealRun(); len(run.Reached) != 0 {
		t.Fatalf("%s was counted reached by a dial that fell back to [::1]: %+v", listed, run.Reached)
	}
	localhost := fmt.Sprintf("localhost:%d", port)
	if err := p.SetRealDeps(context.Background(), []string{localhost}); err != nil {
		t.Fatal(err)
	}
	if served, got, _ := callTree(t, p, port, uint32(os.Getpid())); !served || got != "pong" {
		t.Fatalf("precondition: a call to the tree's listener is relayed: served=%v answer=%q", served, got)
	}
	if run := p.RealRun(); !slices.Equal(run.Reached, []string{localhost}) {
		t.Fatalf("a localhost entry, which lists both families, was not reached: %+v", run.Reached)
	}
}

// Under the proven switch app marking has no effect: a process the starts
// registry calls a dependency (it was not the marked app) still owns a
// listener of the run's tree, so a call to it is live, not recorded. With
// the switch off the same call stays on the mock path.
func TestUnderTheSwitchAppMarkingDoesNotMakeATreeListenerADependency(t *testing.T) {
	starts.Default.Reset()
	t.Cleanup(starts.Default.Reset)
	app := hold(t)
	// This test process is a worker, and another of its children is the
	// marked app, so this process is a dependency by app marking.
	starts.Default.Begin(uint32(os.Getpid()), "TestX", "", false, time.Now())
	if !starts.Default.Mark(uint32(app.Process.Pid), time.Now()) {
		t.Fatal("precondition: the child could not be marked as the app")
	}
	if !starts.Default.Dependency(uint32(os.Getpid())) {
		t.Fatal("precondition: app marking should call this process a dependency")
	}
	port := pingServer(t)

	recorded.reset()
	t.Cleanup(recorded.reset)
	t.Setenv("KEPLOY_PROVEN", "")
	if served, _, _ := callTree(t, selfCallProxy(), port, uint32(os.Getppid())); served {
		t.Fatal("with the switch off a dependency by app marking stays on the mock path")
	}

	p, _ := provenTreeProxy(t)
	served, got, _ := callTree(t, p, port, uint32(os.Getppid()))
	if !served || got != "pong" {
		t.Fatalf("under the switch app marking decides nothing: served=%v answer=%q", served, got)
	}
}

// A call another process of the tree makes to a listener of the tree is
// LIVE, told for each read of the caller's bytes; a process's call to its
// own listener (an in-process test server) is relayed and not listed.
func TestUnderTheSwitchACallToAnotherProcessesListenerIsLive(t *testing.T) {
	p, s := provenTreeProxy(t)
	port := pingServer(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	if served, got, _ := callTree(t, p, port, uint32(os.Getpid())); !served || got != "pong" {
		t.Fatalf("a self-call must be relayed: served=%v answer=%q", served, got)
	}
	if live, mocked := s.rows(); len(live) != 0 || len(mocked) != 0 {
		t.Fatalf("a process's call to its own listener is not listed, got live=%+v mocked=%+v", live, mocked)
	}

	if served, got, _ := callTree(t, p, port, uint32(os.Getppid())); !served || got != "pong" {
		t.Fatalf("a call to another process's listener of the tree must be relayed: served=%v answer=%q", served, got)
	}
	live, mocked := s.rows()
	if len(live) == 0 || len(mocked) != 0 {
		t.Fatalf("want LIVE rows and no mocked row, got live=%+v mocked=%+v", live, mocked)
	}
	for _, r := range live {
		if r != (models.LiveRow{Exe: exe, Port: uint16(port)}) {
			t.Fatalf("a LIVE row names the listener's program and port: got %+v, want exe %q port %d", r, exe, port)
		}
	}
}

// A local listener outside the run's tree is a dependency: the call is left
// to the mock path, with the listener's mocked-local row, which is listed
// once the call is recorded or mocked.
func TestUnderTheSwitchALocalListenerOutsideTheTreeIsMocked(t *testing.T) {
	p, s := provenTreeProxy(t)
	// The run's tree is another process's: this test process, which owns
	// the listener, is outside it.
	p.appPID = uint32(hold(t).Process.Pid)
	port := pingServer(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	served, _, outside := callTree(t, p, port, uint32(os.Getpid()))
	if served {
		t.Fatal("a listener outside the run's tree must stay on the mock path")
	}
	want := models.MockedRow{Addr: fmt.Sprintf("127.0.0.1:%d", port), PID: os.Getpid(), Exe: exe, Port: uint16(port)}
	if outside == nil || *outside != want {
		t.Fatalf("want the mocked-local row %+v, got %+v", want, outside)
	}
	if live, mocked := s.rows(); len(live) != 0 || len(mocked) != 0 {
		t.Fatalf("nothing is listed before the call is known to be mocked, got live=%+v mocked=%+v", live, mocked)
	}
	p.noteMockedLocal(want.Addr, port, outside, "")
	if live, mocked := s.rows(); len(live) != 0 || len(mocked) != 1 || mocked[0] != want {
		t.Fatalf("want one mocked row %+v and no LIVE row, got live=%+v mocked=%+v", want, live, mocked)
	}
}

// A container destination is listed by its container key, whether a
// published port or its own address was dialled; a destination that is
// neither a container nor this machine's is not listed; and only keploy
// mock's runs list anything.
func TestAMockedContainerIsListedByItsKey(t *testing.T) {
	p, s := provenTreeProxy(t)
	const key = "container:postgres:5432@aaaaaaaaaaaa"
	p.noteMockedLocal("127.0.0.1:49153", 49153, &models.MockedRow{Addr: "127.0.0.1:49153", PID: 9, Exe: "/usr/bin/docker-proxy", Port: 49153}, key)
	p.noteMockedLocal("172.17.0.2:5432", 5432, nil, key)
	p.noteMockedLocal("10.0.0.7:443", 443, nil, "")
	_, mocked := s.rows()
	want := []models.MockedRow{
		{Addr: "127.0.0.1:49153", Container: key, Port: 49153},
		{Addr: "172.17.0.2:5432", Container: key, Port: 5432},
	}
	if len(mocked) != len(want) || mocked[0] != want[0] || mocked[1] != want[1] {
		t.Fatalf("want %+v, got %+v", want, mocked)
	}

	other := &rowSink{}
	p.SetTopologySink(other)
	p.mockMode = false
	p.noteMockedLocal("172.17.0.2:5432", 5432, nil, key)
	if _, got := other.rows(); len(got) != 0 {
		t.Fatalf("a run that is not keploy mock's lists nothing, got %+v", got)
	}
}

// A recorded local dependency that is not running (as in CI) is mocked all
// the same, so it is listed, by its address: it may be the app under test,
// which the runner did not start.
func TestUnderTheSwitchARecordedLocalDependencyThatIsNotRunningIsListed(t *testing.T) {
	p, _ := provenTreeProxy(t)
	port := uint32(mustPort(t, testnet.RefusedAddr(t, "tcp")))
	recorded.add([]*models.Mock{recordedPlainDep(port)})
	served, _, outside := callTree(t, p, port, uint32(os.Getpid()))
	if served {
		t.Fatal("a recorded dependency that is not running must stay on the mock path")
	}
	want := models.MockedRow{Addr: fmt.Sprintf("127.0.0.1:%d", port), Port: uint16(port)}
	if outside == nil || *outside != want {
		t.Fatalf("want the mocked-local row %+v, got %+v", want, outside)
	}
}

// A call to an address of this machine that nothing listens at and no
// recording has is neither recorded (its dial fails) nor mocked (it misses):
// it is not listed.
func TestUnderTheSwitchACallNothingAnswersAndNoRecordingHasIsNotListed(t *testing.T) {
	p, _ := provenTreeProxy(t)
	port := uint32(mustPort(t, testnet.RefusedAddr(t, "tcp")))
	// A recording of another port: this one is not recorded.
	recorded.add([]*models.Mock{recordedPlainDep(port + 1)})
	// An address of this machine that is not loopback, so the call is not
	// closed as refused: it goes to the mock path, where it misses.
	var own net.IP
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			own = n.IP
			break
		}
	}
	if own == nil {
		t.Skip("this machine has no IPv4 address but loopback")
	}
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(os.Getpid())}
	_, srv := net.Pipe()
	defer srv.Close()
	served, outside, err := p.serveTreeListener(context.Background(), srv, dest, net.JoinHostPort(own.String(), fmt.Sprint(port)), "1")
	if served || err != nil || outside != nil {
		t.Fatalf("want the mock path and no row, got served=%v outside=%+v err=%v", served, outside, err)
	}
	// The same call, recorded: mocked, so listed.
	recorded.add([]*models.Mock{recordedPlainDep(port)})
	served, outside, err = p.serveTreeListener(context.Background(), srv, dest, net.JoinHostPort(own.String(), fmt.Sprint(port)), "1")
	if served || err != nil || outside == nil || outside.Addr != net.JoinHostPort(own.String(), fmt.Sprint(port)) {
		t.Fatalf("want the mock path and the row, got served=%v outside=%+v err=%v", served, outside, err)
	}
}

// eofConn reads nothing, then end of file.
type eofConn struct{ net.Conn }

func (eofConn) Read([]byte) (int, error) { return 0, io.EOF }

// A read that brings none of the caller's bytes (end of file, as on a
// connection left idle across tests) lists nothing: only the caller's bytes
// make a call LIVE for the test open when they come.
func TestALiveReadWithNoBytesListsNothing(t *testing.T) {
	p, s := provenTreeProxy(t)
	c := p.liveConn(eofConn{}, os.Getpid(), 8080)
	if n, err := c.Read(make([]byte, 8)); n != 0 || err != io.EOF {
		t.Fatalf("read %d, %v", n, err)
	}
	if live, _ := s.rows(); len(live) != 0 {
		t.Fatalf("a read with no bytes listed %+v", live)
	}
}

// tcpPair is both ends of a loopback TCP connection.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a := awaitEvent(t, accepted, "the loopback accept")
	if a == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = c.Close(); _ = a.Close() })
	return c.(*net.TCPConn), a.(*net.TCPConn)
}

// A LIVE caller's connection still half-closes: the relay ends what the
// listener sent with CloseWrite on the caller's end, and the caller reads
// the end while its own side stays open.
func TestALiveConnectionForwardsAHalfClose(t *testing.T) {
	p, _ := provenTreeProxy(t)
	caller, proxySide := tcpPair(t)
	c := p.liveConn(proxySide, os.Getpid(), 8080)
	if _, err := c.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := util.CloseWriteIfPossible(c); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	type read struct {
		got []byte
		err error
	}
	ended := make(chan read, 1)
	go func() {
		got, err := io.ReadAll(caller)
		ended <- read{got, err}
	}()
	r := awaitEvent(t, ended, "the caller reading the end the half-close sent")
	if r.err != nil || string(r.got) != "bye" {
		t.Fatalf("the caller read %q (%v), want the bytes and then the end", r.got, r.err)
	}
	if _, err := caller.Write([]byte("still open")); err != nil {
		t.Fatalf("the caller's own side closed too: %v", err)
	}
	buf := make([]byte, len("still open"))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "still open" {
		t.Fatalf("the proxy's side read %q (%v) after its half-close", buf, err)
	}
}

// The call of a test process to an app it started goes to the live handler
// (the runner's own app, whose requests a record captures as tests), and is
// LIVE there too: the handler gets the caller's connection that lists it.
func TestUnderTheSwitchACallToAnAppTheCallerStartedIsLiveThroughTheLiveHandler(t *testing.T) {
	p, s := provenTreeProxy(t)
	port := pingServer(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	p.live = func(_ context.Context, conn net.Conn, upstream string, lport uint16) {
		defer close(handled)
		up, err := net.Dial("tcp", upstream)
		if err != nil {
			return
		}
		defer up.Close()
		util.RelayRawPassthrough(conn, up)
	}
	// The listener is this process's; its caller is this process's parent,
	// which it descends from.
	if served, got, _ := callTree(t, p, port, uint32(os.Getppid())); !served || got != "pong" {
		t.Fatalf("a call to an app the caller started must go to the live handler: served=%v answer=%q", served, got)
	}
	awaitEvent(t, handled, "the live handler serving the call")
	live, _ := s.rows()
	if len(live) == 0 || live[0] != (models.LiveRow{Exe: exe, Port: uint16(port)}) {
		t.Fatalf("want LIVE rows of %s :%d, got %+v", exe, port, live)
	}
}

// With the switch off nothing is told, whatever the call.
func TestOffTheSwitchNoRowIsTold(t *testing.T) {
	recorded.reset()
	t.Cleanup(recorded.reset)
	t.Setenv("KEPLOY_PROVEN", "")
	s := &rowSink{}
	p := selfCallProxy()
	p.SetTopologySink(s)
	port := pingServer(t)
	if served, got, _ := callTree(t, p, port, uint32(os.Getppid())); !served || got != "pong" {
		t.Fatalf("a self-call must be relayed: served=%v answer=%q", served, got)
	}
	outside := selfCallProxy()
	outside.SetTopologySink(s)
	outside.appPID = uint32(hold(t).Process.Pid)
	if served, _, row := callTree(t, outside, port, uint32(os.Getpid())); served || row != nil {
		t.Fatalf("a listener outside the run's tree must stay on the mock path with no row: served=%v row=%+v", served, row)
	}
	if live, mocked := s.rows(); len(live) != 0 || len(mocked) != 0 {
		t.Fatalf("with the switch off no row is told, got live=%+v mocked=%+v", live, mocked)
	}
}

// chanSink is a TopologySink that hands each mocked row to a channel.
type chanSink struct{ mocked chan models.MockedRow }

func (chanSink) NoteLive(models.LiveRow) {}
func (s chanSink) NoteMocked(r models.MockedRow) {
	select {
	case s.mocked <- r:
	default:
	}
}

// handleConnection lists a local destination as mocked only when the call
// goes on to be recorded or answered from mocks, in a record and a replay
// alike: a session that passes connections through lists nothing. A
// container destination is listed by its container key.
func TestHandleConnectionListsAMockedLocalDestinationOnlyWhenItIsMocked(t *testing.T) {
	for _, tc := range []struct {
		mode        models.Mode
		passthrough bool
		container   bool
		// unread: the container's configuration is not read yet, so it has
		// no key: it is listed as a container all the same, by its label.
		unread bool
	}{
		{models.MODE_TEST, false, false, false}, {models.MODE_TEST, true, false, false}, {models.MODE_TEST, false, true, false},
		{models.MODE_TEST, false, true, true},
		{models.MODE_RECORD, false, false, false}, {models.MODE_RECORD, true, false, false}, {models.MODE_RECORD, false, true, false},
	} {
		t.Run(fmt.Sprintf("%s/passthrough=%v/container=%v/unread=%v", tc.mode, tc.passthrough, tc.container, tc.unread), func(t *testing.T) {
			recorded.reset()
			t.Cleanup(recorded.reset)
			t.Setenv("KEPLOY_PROVEN", "1")
			upstream, _, _ := echoUpstream(t)
			saved := containerkey.Default
			t.Cleanup(func() { containerkey.Default = saved })
			if tc.container {
				c := containerkey.Container{ID: "c1", Image: "postgres:16", Port: 5432, Fingerprint: "aaaaaaaaaaaa"}
				if tc.unread {
					c.Fingerprint = ""
				}
				containerkey.Default = containerkey.NewKeyer(containerAt{addr: upstream, c: c})
			} else {
				containerkey.Default = containerkey.NewKeyer(containerAt{})
			}
			cfg := config.New()
			cfg.Agent.Mode = tc.mode
			cfg.Agent.MockMode = true
			cfg.Agent.GlobalPassthrough = tc.passthrough
			// The run's tree is another process's, so this test process's
			// listener is outside it.
			cfg.Agent.ClientNSPID = uint32(hold(t).Process.Pid)
			p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
			p.nsswitchData = []byte("preseeded-by-test") // see testProxy
			sink := chanSink{mocked: make(chan models.MockedRow, 1)}
			p.SetTopologySink(sink)
			if tc.mode == models.MODE_RECORD {
				if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
			} else if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
				t.Fatal(err)
			}
			app, src := acceptedConn(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(ctx, src) }()
			if _, err := app.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			if tc.passthrough {
				if got := <-readN(app, 4); got != "ping" {
					t.Fatalf("the passthrough carried %q, want the echo %q", got, "ping")
				}
				select {
				case r := <-sink.mocked:
					t.Fatalf("a call passed through was listed mocked: %+v", r)
				default:
				}
			} else {
				r := awaitEvent(t, sink.mocked, "the mocked-local row being listed")
				_, portText, _ := net.SplitHostPort(upstream)
				port, _ := strconv.Atoi(portText)
				want := models.MockedRow{Addr: upstream, Port: uint16(port)}
				switch {
				case tc.unread:
					want.Container = "container:postgres:5432"
				case tc.container:
					want.Container = "container:postgres:5432@aaaaaaaaaaaa"
				default:
					exe, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					want.PID, want.Exe = os.Getpid(), exe
				}
				if r != want {
					t.Fatalf("want the row %+v, got %+v", want, r)
				}
			}
			_ = app.Close()
			cancel()
			awaitEvent(t, done, "handleConnection returning")
		})
	}
}

// A local destination mock.real lists, by its address or as a container:
// entry, is relayed to the real thing and never listed mocked, in a record as
// in a replay: the REAL relay is decided before the mocked-local row, and the
// one container naming gives both its label and its repository (its
// configuration unread here: the repository needs none).
func TestHandleConnectionNeverListsARealDestinationMocked(t *testing.T) {
	for _, tc := range []struct {
		mode      models.Mode
		container bool
	}{{models.MODE_TEST, false}, {models.MODE_TEST, true}, {models.MODE_RECORD, false}, {models.MODE_RECORD, true}} {
		t.Run(fmt.Sprintf("%s/container=%v", tc.mode, tc.container), func(t *testing.T) {
			recorded.reset()
			t.Cleanup(recorded.reset)
			t.Setenv("KEPLOY_PROVEN", "1")
			upstream, _, _ := echoUpstream(t)
			saved := containerkey.Default
			t.Cleanup(func() { containerkey.Default = saved })
			entry := upstream
			if tc.container {
				containerkey.Default = containerkey.NewKeyer(containerAt{addr: upstream,
					c: containerkey.Container{ID: "c1", Image: "postgres:16", Port: 5432}})
				entry = "container:postgres"
			} else {
				containerkey.Default = containerkey.NewKeyer(containerAt{})
			}
			cfg := config.New()
			cfg.Agent.Mode = tc.mode
			cfg.Agent.MockMode = true
			cfg.Agent.ClientNSPID = uint32(hold(t).Process.Pid)
			p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
			p.nsswitchData = []byte("preseeded-by-test") // see testProxy
			sink := chanSink{mocked: make(chan models.MockedRow, 1)}
			p.SetTopologySink(sink)
			if err := p.SetRealDeps(context.Background(), []string{entry}); err != nil {
				t.Fatal(err)
			}
			if tc.mode == models.MODE_RECORD {
				if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
			} else if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
				t.Fatal(err)
			}
			app, src := acceptedConn(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(ctx, src) }()
			if _, err := app.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			if got := <-readN(app, 4); got != "ping" {
				t.Fatalf("the REAL relay carried %q, want the real echo %q", got, "ping")
			}
			_ = app.Close()
			awaitEvent(t, done, "handleConnection returning")
			select {
			case r := <-sink.mocked:
				t.Fatalf("a REAL destination was listed mocked: %+v", r)
			default:
			}
			if got := p.RealRun().Reached; len(got) != 1 {
				t.Fatalf("the REAL destination was not reached: %v", got)
			}
		})
	}
}
