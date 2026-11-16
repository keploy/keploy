//go:build linux || darwin

package routes

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// envClaimFrom makes this test binary, run again as a child, claim against
// the server at its value and exit.
const envClaimFrom = "KEPLOY_TEST_CLAIM_FROM"

// envBegin makes that child send a test's begin instead.
const envBegin = "KEPLOY_TEST_CLAIM_BEGIN"

// envHoldConnTo makes this test binary, run again as a child, dial its value,
// print the connection's local address, and hold the connection until its
// stdin closes.
const envHoldConnTo = "KEPLOY_TEST_HOLD_CONN_TO"

// envHoldUntilStdinCloses makes this test binary, run again as a child, keep
// what it inherited (its ExtraFiles) until its stdin closes.
const envHoldUntilStdinCloses = "KEPLOY_TEST_HOLD_UNTIL_STDIN_CLOSES"

// envHalfCloseTo makes this test binary, run again as a child, send a GET to
// its value, close its connection's write side (CloseWrite), say so on its
// stdout, and keep the connection until its stdin closes.
const envHalfCloseTo = "KEPLOY_TEST_HALF_CLOSE_TO"

func TestMain(m *testing.M) {
	if addr := os.Getenv(envHalfCloseTo); addr != "" {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(3)
		}
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: keploy\r\n\r\n"); err != nil {
			os.Exit(4)
		}
		if err := c.(*net.TCPConn).CloseWrite(); err != nil {
			os.Exit(5)
		}
		_, _ = io.WriteString(os.Stdout, "half-closed\n")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = c.Close()
		os.Exit(0)
	}
	if os.Getenv(envHoldUntilStdinCloses) == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if addr := os.Getenv(envHoldConnTo); addr != "" {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(3)
		}
		_, _ = io.WriteString(os.Stdout, c.LocalAddr().String()+"\n")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = c.Close()
		os.Exit(0)
	}
	if url := os.Getenv(envClaimFrom); url != "" {
		body := `{"key":"orders","pid":1}`
		if os.Getenv(envBegin) != "" {
			body = `{"name":"TestA","canSkip":true}`
		}
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			os.Exit(3)
		}
		_ = resp.Body.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// CallerPID names the process that sent a request (this test binary run
// again), not the server's own, so a server other than the agent that answers
// the agent's routes reads callers as the agent does.
func TestCallerPIDNamesTheSendingProcess(t *testing.T) {
	type caller struct {
		pid int
		err error
	}
	read := make(chan caller, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		pid, err := CallerPID(r)
		read <- caller{pid, err}
	}))
	defer srv.Close()
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), envClaimFrom+"="+srv.URL)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("the sending child failed: %v: %s", err, out)
	}
	var got caller
	select {
	case got = <-read:
	default:
		t.Fatal("the server read no request")
	}
	if got.err != nil || got.pid != c.Process.Pid {
		t.Fatalf("CallerPID = %d, %v; want the child %d", got.pid, got.err, c.Process.Pid)
	}
}

// A caller that sent its request and closed its connection's write side
// (CloseWrite) while it keeps the connection still holds the socket, and is
// named: on Linux as on macOS. The server reads it only once it has seen the
// caller's end of input, so the caller's socket is no longer established.
func TestCallerPIDNamesACallerThatClosedItsWriteSide(t *testing.T) {
	type caller struct {
		pid int
		err error
	}
	read := make(chan caller, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // the server read the caller's FIN
		case <-time.After(30 * time.Second):
			read <- caller{0, errors.New("the server never saw the caller's end of input")}
			return
		}
		pid, err := CallerPID(r)
		read <- caller{pid, err}
	}))
	defer srv.Close()
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), envHalfCloseTo+"="+srv.Listener.Addr().String())
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.ProcessState == nil {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			_ = c.Wait()
		}
	})
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "half-closed\n" {
		t.Fatalf("the child did not half-close its connection: %q, %v", line, err)
	}
	var got caller
	select {
	case got = <-read:
	case <-time.After(60 * time.Second):
		t.Fatal("the server read no request")
	}
	if got.err != nil || got.pid != c.Process.Pid {
		t.Fatalf("CallerPID = %d, %v; want the half-closed child %d", got.pid, got.err, c.Process.Pid)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatalf("the child: %v", err)
	}
}

// A request with no connection behind it names no caller: CallerPID says so
// rather than naming a process.
func TestCallerPIDIsAnErrorWithNoConnection(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/agent/instance", strings.NewReader("{}"))
	if pid, err := CallerPID(r); err == nil {
		t.Fatalf("CallerPID = %d with no connection, want an error", pid)
	}
}

// The claim reads the caller from the connection: a claim made by another
// process (this test binary run again) hands the service that process's ID,
// not the server's, and a pid in the body is ignored.
func TestTheClaimReadsTheCallerFromTheConnection(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &wireSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleInstance))
	defer srv.Close()
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), envClaimFrom+"="+srv.URL)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("the claiming child failed: %v: %s", err, out)
	}
	if svc.claimKey != "orders" || svc.claimCaller != c.Process.Pid {
		t.Fatalf("claimed key %q for process %d, want orders for the child %d", svc.claimKey, svc.claimCaller, c.Process.Pid)
	}
}

// A link-local IPv6 caller's address names its zone (fe80::1%eth0), which
// the kernel's socket tables do not: the two are one endpoint. Another
// address or port is not, and IPv4-mapped IPv6 is its IPv4 address.
func TestSameEndpointReadsAnAddressWithoutItsZone(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"[fe80::1%eth0]:5000", "[fe80::1]:5000", true},
		{"[fe80::1]:5000", "[fe80::1%lo]:5000", true},
		{"[::ffff:127.0.0.1]:5000", "127.0.0.1:5000", true},
		{"[fe80::1%eth0]:5000", "[fe80::2]:5000", false},
		{"[fe80::1%eth0]:5000", "[fe80::1]:5001", false},
	} {
		if got := sameEndpoint(netip.MustParseAddrPort(c.a), netip.MustParseAddrPort(c.b)); got != c.same {
			t.Errorf("sameEndpoint(%s, %s) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}
