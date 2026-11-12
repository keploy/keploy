//go:build unix

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The failure this was found by, end to end through the replay request: the
// compose app publishes host port P to its port 8080, the tests were recorded
// on 8080 and are replayed with --port P, and the first one is sent while the
// app is still starting, so the connection is refused. It must be re-sent as
// an app still starting, and get the app's answer once it listens, not fail at
// once as a port the host can never reach.
func TestARefusalAtAHostPortPublishedToTheAppsPortIsReSent(t *testing.T) {
	// The app's port, refused until the app listens on it, and held all along
	// (holdLoopbackPort): released and bound again, it was another's to take
	// in between on a busy host, and the app could not listen on it.
	held := holdLoopbackPort(t)
	hostPort := strconv.Itoa(held.port)
	if l, err := net.Listen("tcp", "127.0.0.1:"+hostPort); err == nil {
		_ = l.Close()
		t.Fatalf("another socket could bind the app's port %s before the app listened: the test does not hold it", hostPort)
	}

	// The app finishes starting while keploy asks the agent where it listens:
	// from then on the host port answers, as docker forwards it to the app.
	var app *http.Server
	var startOnce sync.Once
	t.Cleanup(func() {
		if app != nil {
			_ = app.Close()
		}
	})
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() {
			ln, err := held.listen()
			if err != nil {
				t.Errorf("the app could not listen on %s: %v", hostPort, err)
				return
			}
			if err := held.handedOver(ln); err != nil {
				_ = ln.Close()
				t.Errorf("the app's listener on %s: %v", hostPort, err)
				return
			}
			app = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("pong"))
			})}
			go func() { _ = app.Serve(ln) }()
		})
		// Asked before the app listened: nothing listens on its port yet.
		_ = json.NewEncoder(w).Encode(models.AppListenAddrs{Addrs: nil})
	})
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)

	fake := &countingInspect{resp: agentContainer(nat.PortMap{"8080/tcp": {{HostIP: "0.0.0.0", HostPort: hostPort}}}, true, "bridge")}
	a := dockerAgent(utils.DockerCompose, fake)
	a.conf.Agent.AgentURI = srv.URL + "/agent"

	port, _ := strconv.ParseUint(hostPort, 10, 32)
	tc := &models.TestCase{
		Name: "get-ping-1", Kind: models.HTTP, AppPort: 8080,
		HTTPReq: models.HTTPReq{Method: "GET", URL: "http://127.0.0.1:18057/ping", Header: map[string]string{}},
	}
	resp, err := pkg.SimulateHTTP(context.Background(), tc, "test-set-0", zap.NewNop(), pkg.SimulationConfig{
		APITimeout:          5,
		ConfigHost:          "127.0.0.1",
		ConfigPort:          uint32(port),
		AppPortReachability: a,
	})
	if err != nil {
		t.Fatalf("the test failed instead of waiting for the app: %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.Body != "pong" {
		t.Errorf("got %d %q, want the app's 200 pong", resp.StatusCode, resp.Body)
	}
}

// heldPort is a TCP port on 127.0.0.1, bound and not listening: a
// connection to it is refused, as one to a port nothing listens on is. The
// port stays the test's all along: it is bound without SO_REUSEADDR, so no
// other socket on the host can bind it, not even a listener (Go's set it),
// until the test ends. listen makes that same socket listen and hands it
// over, so the port is never released between the refusals and the answers,
// for anything else on a busy host to take. It needs a bound socket handed
// over as a listener (net.FileListener), which Go offers only on unix
// systems: hence this file's build tag.
type heldPort struct {
	port int
	f    *os.File
	id   syscall.Stat_t // the socket's identity, from when it was bound
}

func holdLoopbackPort(t *testing.T) *heldPort {
	t.Helper()
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "held loopback port")
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	h := &heldPort{port: sa.(*syscall.SockaddrInet4).Port, f: f}
	if err := syscall.Fstat(fd, &h.id); err != nil {
		t.Fatal(err)
	}
	return h
}

// listen makes the held socket listen and returns it as a listener: one on a
// copy of the socket, so f's close at the test's end leaves it listening
// until the app's server closes it.
func (h *heldPort) listen() (net.Listener, error) {
	if err := syscall.Listen(int(h.f.Fd()), syscall.SOMAXCONN); err != nil {
		return nil, os.NewSyscallError("listen", err)
	}
	return net.FileListener(h.f)
}

// handedOver says whether ln is the held socket itself (a copy of its
// descriptor: the socket it was when bound), and not another socket bound
// to its port once the held one was let go, even one given the same
// descriptor number.
func (h *heldPort) handedOver(ln net.Listener) error {
	var got syscall.Stat_t
	sc, ok := ln.(syscall.Conn)
	if !ok {
		return fmt.Errorf("a %T has no socket to compare", ln)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var statErr error
	if err := raw.Control(func(fd uintptr) { statErr = syscall.Fstat(int(fd), &got) }); err != nil {
		return err
	}
	if statErr != nil {
		return statErr
	}
	if got.Dev != h.id.Dev || got.Ino != h.id.Ino {
		return errors.New("it is another socket than the one held: the port was let go before the app listened")
	}
	return nil
}
