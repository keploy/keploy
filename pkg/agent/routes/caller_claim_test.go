//go:build linux || darwin

package routes

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

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
