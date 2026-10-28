//go:build linux || darwin

package routes

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// envClaimFrom makes this test binary, run again as a child, claim against
// the server at its value and exit.
const envClaimFrom = "KEPLOY_TEST_CLAIM_FROM"

// envHoldConnTo makes this test binary, run again as a child, dial its value,
// print the connection's local address, and hold the connection until its
// stdin closes.
const envHoldConnTo = "KEPLOY_TEST_HOLD_CONN_TO"

// envHoldUntilStdinCloses makes this test binary, run again as a child, keep
// what it inherited (its ExtraFiles) until its stdin closes.
const envHoldUntilStdinCloses = "KEPLOY_TEST_HOLD_UNTIL_STDIN_CLOSES"

func TestMain(m *testing.M) {
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
		resp, err := http.Post(url, "application/json", strings.NewReader(`{"key":"orders","pid":1}`))
		if err != nil {
			os.Exit(3)
		}
		resp.Body.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
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

// holdConnFrom starts this test binary again as a child that dials addr and
// holds the connection; it returns the child and the connection's local
// address. The child and its process group die in cleanup.
func holdConnFrom(t *testing.T, addr string) (*exec.Cmd, string) {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), envHoldConnTo+"="+addr)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the holding child said nothing: %v", err)
	}
	return c, strings.TrimSpace(line)
}
