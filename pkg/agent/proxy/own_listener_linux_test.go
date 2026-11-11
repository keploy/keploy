//go:build linux

package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The test process plays the app: it holds the listeners. The agent is a
// process apart (agentPID), which no process here descends from.
const notTheAgent = 1<<30 + 7

func standApartFromTheAgent(t *testing.T) {
	t.Helper()
	old := agentPID
	agentPID = func() int { return notTheAgent }
	t.Cleanup(func() { agentPID = old })
}

const ownHealth = `{"status":"ok"}`

// appListener is a listener of the test process (the app) at 127.0.0.1
// answering each HTTP request with ownHealth; it returns its port.
func appListener(t *testing.T) uint32 { return appListenerAt(t, "127.0.0.1:0") }

// appListenerAt is appListener at addr.
func appListenerAt(t *testing.T, addr string) uint32 {
	t.Helper()
	l, err := net.Listen("tcp4", addr)
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
				defer func() { _ = c.Close() }()
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(ownHealth), ownHealth)
			}()
		}
	}()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

// anotherProcess starts a process the test process does not descend from,
// in a process group of its own, killed as the test ends.
func anotherProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// The parse-server lane's case, through handleConnection: keploy test of the
// app's GET /parse/health to its own port, which no mock holds. It was
// matched against the mocks and answered 502 "no matching mock"; it reaches
// the app, as at record, and no mismatch is said.
func TestReplayPassesTheAppsCallToItsOwnPortToTheApp(t *testing.T) {
	// The package's TestMain clears KEPLOY_PROVEN, so each switch is set here.
	for _, on := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			standApartFromTheAgent(t)
			recorded.forget()
			t.Cleanup(recorded.forget)
			port := appListener(t)
			dst := fmt.Sprintf("127.0.0.1:%d", port)
			app, src := acceptedConn(t)
			p := replayProxyTo(t, dst, models.OutgoingOptions{})
			d := ipv4Dest(t, dst)
			d.AgentNsPid, d.KernelPid = uint32(os.Getpid()), uint32(os.Getpid())
			p.DestInfo = fakeDestInfo{dest: d}
			p.IsDocker = true // keploy test of docker compose: no CLI pid here
			core, logs := observer.New(zap.DebugLevel)
			p.logger = zap.New(core)
			p.errChannel = make(chan error, 4)
			p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: httpint.New(zap.NewNop())}
			p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()

			if _, err := fmt.Fprintf(app, "GET /parse/health HTTP/1.1\r\nHost: localhost:%d\r\n\r\n", port); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(app), nil)
			if err != nil {
				t.Fatalf("the app's call to its own port got no answer: %v (logs: %v)", err, logs.All())
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			_ = app.Close()
			_ = awaitEvent(t, done, "handleConnection returning after the app left")
			if resp.StatusCode != http.StatusOK || string(body) != ownHealth {
				t.Fatalf("the app's call to its own port was answered %d %q, want the app's own 200 %q", resp.StatusCode, body, ownHealth)
			}
			if n := len(logs.FilterMessageSnippet("no matching mock").All()); n != 0 {
				t.Fatalf("a call passed through to the app must say no mock miss, said %d", n)
			}
			if len(p.errChannel) != 0 {
				t.Fatalf("a call passed through to the app must keep no miss for a test, kept %d", len(p.errChannel))
			}
		})
	}
}

func TestReplayOwnListenerTellsTheAppsOwnPortsApart(t *testing.T) {
	standApartFromTheAgent(t)
	recorded.forget()
	t.Cleanup(recorded.forget)
	self := os.Getpid()
	port := appListener(t)
	recPort := appListener(t)
	anyPort := appListenerAt(t, "0.0.0.0:0")
	recorded.add([]*models.Mock{recordedPlainDep(recPort)})
	refused := uint32(mustPort(t, testnet.RefusedAddr(t, "tcp")))
	other := anotherProcess(t)
	// The package's TestMain clears KEPLOY_PROVEN, so each switch is set here.
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
			ownListenerRows(t, proven, self, port, recPort, anyPort, refused, other)
		})
	}
}

func ownListenerRows(t *testing.T, proven bool, self int, port, recPort, anyPort, refused uint32, other int) {

	type row struct {
		name   string
		p      *Proxy
		mode   models.Mode
		caller int
		port   uint32
		addr   string // the destination, "" for 127.0.0.1:port
		agent  int    // the agent's pid, 0 for one apart
		holder bool   // keploy cannot read the listener's holder
		mnt    string // the holder's container is another, or keploy cannot read the caller's or holder's
		want   bool
	}
	native := func(cli int) *Proxy { return &Proxy{logger: zap.NewNop(), appPID: uint32(cli)} }
	docker := func() *Proxy { return &Proxy{logger: zap.NewNop(), IsDocker: true} }
	rows := []row{
		{name: "the caller holds the listener it calls", p: native(0), mode: models.MODE_TEST, caller: self, port: port, want: true},
		{name: "the caller holds it, at the unspecified address", p: native(0), mode: models.MODE_TEST, caller: self, port: port, addr: fmt.Sprintf("0.0.0.0:%d", port), want: true},
		{name: "native: a process the CLI started holds it", p: native(os.Getppid()), mode: models.MODE_TEST, port: port, want: true},
		{name: "native: the CLI itself holds it", p: native(self), mode: models.MODE_TEST, port: port, want: false},
		{name: "native: another local service holds it", p: native(other), mode: models.MODE_TEST, port: port, want: false},
		{name: "native: no CLI pid here", p: native(0), mode: models.MODE_TEST, port: port, want: false},
		{name: "docker: a process of the caller's container holds it", p: docker(), mode: models.MODE_TEST, caller: other, port: port, want: true},
		{name: "docker: a process of another container holds it (a pod's sidecar)", p: docker(), mode: models.MODE_TEST, caller: other, port: port, mnt: "another container", want: false},
		{name: "docker: keploy cannot read the caller's container", p: docker(), mode: models.MODE_TEST, caller: other, port: port, mnt: "caller unread", want: false},
		{name: "docker: keploy cannot read the holder's container", p: docker(), mode: models.MODE_TEST, caller: other, port: port, mnt: "holder unread", want: false},
		{name: "docker: a caller keploy cannot name", p: docker(), mode: models.MODE_TEST, port: port, want: false},
		{name: "docker: the agent's own listener", p: docker(), mode: models.MODE_TEST, caller: other, port: port, agent: self, want: false},
		{name: "the agent itself calls a listener it holds", p: docker(), mode: models.MODE_TEST, caller: self, port: port, agent: self, want: false},
		{name: "docker: keploy cannot read who holds it", p: docker(), mode: models.MODE_TEST, caller: other, port: port, holder: true, want: false},
		{name: "keploy record", p: docker(), mode: models.MODE_RECORD, caller: self, port: port, want: false},
		{name: "keploy mock decides its own", p: &Proxy{logger: zap.NewNop(), mockMode: true, IsDocker: true}, mode: models.MODE_TEST, caller: self, port: port, want: false},
		{name: "keploy's proxy port", p: &Proxy{logger: zap.NewNop(), IsDocker: true, Port: port}, mode: models.MODE_TEST, caller: self, port: port, want: false},
		{name: "keploy's DNS port", p: &Proxy{logger: zap.NewNop(), IsDocker: true, DNSPort: port}, mode: models.MODE_TEST, caller: self, port: port, want: false},
		{name: "keploy's ingress proxy port", p: &Proxy{logger: zap.NewNop(), IsDocker: true, IncomingProxyPort: uint16(port)}, mode: models.MODE_TEST, caller: self, port: port, want: false},
		{name: "nothing listens there", p: docker(), mode: models.MODE_TEST, caller: self, port: refused, want: false},
		{name: "an address of another host", p: docker(), mode: models.MODE_TEST, caller: self, port: port, addr: fmt.Sprintf("192.0.2.1:%d", port), want: false},
		{name: "another host, at a port the app listens on at every address", p: docker(), mode: models.MODE_TEST, caller: self, port: anyPort, addr: fmt.Sprintf("192.0.2.1:%d", anyPort), want: false},
		{name: "this host, at a port the app listens on at every address", p: docker(), mode: models.MODE_TEST, caller: self, port: anyPort, want: true},
		{name: "an address that does not parse", p: docker(), mode: models.MODE_TEST, caller: self, port: port, addr: "no-port", want: false},
		{name: "a port the recording reached as a dependency", p: docker(), mode: models.MODE_TEST, caller: other, port: recPort, want: proven},
		{name: "a recorded port the caller holds itself", p: docker(), mode: models.MODE_TEST, caller: self, port: recPort, want: true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if r.agent != 0 {
				old := agentPID
				agentPID = func() int { return r.agent }
				t.Cleanup(func() { agentPID = old })
			}
			if r.holder {
				old := ownerOfListener
				ownerOfListener = func(net.IP, uint32) (int, bool) { return 0, true }
				t.Cleanup(func() { ownerOfListener = old })
			}
			if r.mnt != "" {
				old := mountNSOf
				mountNSOf = func(pid int) (string, error) {
					ns, err := old(pid)
					switch {
					case r.mnt == "another container" && pid == self:
						return "mnt:[another]", err
					case r.mnt == "caller unread" && pid == r.caller, r.mnt == "holder unread" && pid == self:
						// The namespace it would have read is the other's:
						// only the failed read tells them apart.
						return ns, errors.New("permission denied")
					}
					return ns, err
				}
				t.Cleanup(func() { mountNSOf = old })
			}
			addr := r.addr
			if addr == "" {
				addr = fmt.Sprintf("127.0.0.1:%d", r.port)
			}
			d := &agent.NetworkAddress{Version: 4, Port: r.port, AgentNsPid: uint32(r.caller), KernelPid: uint32(r.caller)}
			owner, own := r.p.replayOwnListener(r.mode, d, addr)
			if own != r.want {
				t.Fatalf("replayOwnListener(%s) = %v (owner %d), want %v", addr, own, owner, r.want)
			}
			if own && owner != self {
				t.Fatalf("the listener's holder is %d, want the test process %d", owner, self)
			}
		})
	}
}

// A process's container is its mount namespace, read from /proc; a process
// keploy cannot name (0) is in none.
func TestMountNSOfNamesAProcesssMountNamespace(t *testing.T) {
	ns, err := mountNSOf(os.Getpid())
	if err != nil || !strings.HasPrefix(ns, "mnt:[") {
		t.Fatalf("mountNSOf(this process) = %q, %v, want its mnt:[...] namespace", ns, err)
	}
	if !sameContainer(os.Getpid(), anotherProcess(t)) {
		t.Fatal("a process this one started, in its mount namespace, is not in its container")
	}
	if ns, err := mountNSOf(0); err == nil {
		t.Fatalf("mountNSOf(0) = %q, want an error: 0 names no process", ns)
	}
}

// The first call to the app's own port is said at INFO and the rest at debug,
// each counted for the run's end; a dial that fails ends the call (the app's
// connect fails as it would have) and is never left to the mock path.
func TestReplayOwnListenerSaysTheFirstAndEndsAFailedDial(t *testing.T) {
	standApartFromTheAgent(t)
	recorded.forget()
	t.Cleanup(recorded.forget)
	port := appListener(t)
	dst := fmt.Sprintf("127.0.0.1:%d", port)
	core, logs := observer.New(zap.DebugLevel)
	p := &Proxy{logger: zap.New(core), IsDocker: true}
	d := &agent.NetworkAddress{Version: 4, Port: port, AgentNsPid: uint32(os.Getpid())}
	call := func() (bool, error) {
		cli, srv := net.Pipe()
		go func() {
			// The app reads the answer, then hangs up, which ends the relay.
			defer func() { _ = cli.Close() }()
			if _, err := fmt.Fprintf(cli, "GET /parse/health HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
				return
			}
			if resp, err := http.ReadResponse(bufio.NewReader(cli), nil); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
		return p.serveReplayOwnListener(context.Background(), models.MODE_TEST, srv, d, dst, nil)
	}
	for i := 0; i < 2; i++ {
		if served, err := call(); !served || err != nil {
			t.Fatalf("call %d: served=%v err=%v, want it passed through", i, served, err)
		}
	}
	said := logs.FilterMessageSnippet("passed through to the app").All()
	if len(said) != 2 || said[0].Level != zapcore.InfoLevel || said[1].Level != zapcore.DebugLevel {
		t.Fatalf("want the first call said at INFO and the second at debug, got %v", said)
	}
	if n := p.selfCalls.Load(); n != 2 {
		t.Fatalf("calls counted = %d, want 2", n)
	}

	old := dialOwnListener
	dialOwnListener = func(context.Context, *zap.Logger, string, util.DialTarget) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	t.Cleanup(func() { dialOwnListener = old })
	if served, err := call(); !served || err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a failed dial: served=%v err=%v, want the call ended with the dial's error", served, err)
	}

	other := &agent.NetworkAddress{Version: 4, Port: port}
	_, srv := net.Pipe()
	defer func() { _ = srv.Close() }()
	if served, err := (&Proxy{logger: zap.NewNop()}).serveReplayOwnListener(context.Background(), models.MODE_TEST, srv, other, dst, nil); served || err != nil {
		t.Fatalf("a call keploy cannot tell is the app's: served=%v err=%v, want it left to the mock path", served, err)
	}
}
