//go:build linux || darwin || windows

package routes

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// socketSvc is a service whose hooks say whether the process the kernel's
// tables name for a call sends on it (agent.Agent.CallerSocketWhy).
type socketSvc struct {
	wireSvc
	why            string
	asked          int
	pid            int
	client, server netip.AddrPort
}

func (s *socketSvc) CallerSocketWhy(pid int, client, server netip.AddrPort) string {
	s.asked++
	s.pid, s.client, s.server = pid, client, server
	return s.why
}

// claimOver posts a claim to a over a connection whose client end it returns.
func claimOver(t *testing.T, a *Agent) (*http.Response, string, netip.AddrPort, netip.AddrPort) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(a.HandleInstance))
	t.Cleanup(srv.Close)
	var clientEnd net.Addr
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err == nil {
			clientEnd = c.LocalAddr()
		}
		return c, err
	}}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Post(srv.URL, "application/json", strings.NewReader(`{"key":"orders"}`))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body), netip.MustParseAddrPort(clientEnd.String()), netip.MustParseAddrPort(srv.Listener.Addr().String())
}

// The hooks are asked about the owner the kernel's tables name, with the
// connection's two ends, and a claim they vouch for is made.
func TestAClaimTheHooksVouchForIsMadeWithItsEnds(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &socketSvc{}
	resp, body, client, server := claimOver(t, &Agent{logger: zap.NewNop(), svc: svc})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim answered %d: %s", resp.StatusCode, body)
	}
	if svc.asked != 1 || svc.pid != os.Getpid() || svc.client != client || svc.server != server {
		t.Fatalf("hooks asked %d times about %d over %v -> %v; want once about %d over %v -> %v",
			svc.asked, svc.pid, svc.client, svc.server, os.Getpid(), client, server)
	}
	if svc.claimCaller != os.Getpid() {
		t.Fatalf("claimed for %d, want %d", svc.claimCaller, os.Getpid())
	}
}

// A claim over a socket the hooks say another process may hold is refused,
// saying why, and no claim is made.
func TestAClaimOverASocketHandedToAnotherProcessIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	why := "Windows names process 10, the one that created the socket, but process 20 connected it"
	svc := &socketSvc{why: why}
	resp, body, _, _ := claimOver(t, &Agent{logger: zap.NewNop(), svc: svc})
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, string(models.ScopeErrNoCaller)) ||
		!strings.Contains(body, why) {
		t.Fatalf("claim answered %d: %s; want 500 %s naming why", resp.StatusCode, body, models.ScopeErrNoCaller)
	}
	if svc.claimKey != "" {
		t.Fatalf("a refused caller's claim was made (key %q)", svc.claimKey)
	}
}

// A begin or an end reads its caller the same way, through the reader the
// service is handed (coreAgent.CallerOf).
func TestABeginsCallerIsReadThroughTheHooks(t *testing.T) {
	why := "process 20 connected it"
	for _, tc := range []struct {
		why     string
		wantErr bool
	}{{"", false}, {why, true}} {
		svc := &socketSvc{why: tc.why}
		a := &Agent{logger: zap.NewNop(), svc: svc}
		var pid int
		var err error
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pid, err = coreAgent.CallerOf(a.withCaller(r))()
		}))
		resp, gerr := http.Get(srv.URL)
		if gerr == nil {
			_ = resp.Body.Close()
		}
		srv.Close()
		if tc.wantErr != (err != nil) || (err != nil && err.Error() != why) || (err == nil && pid != os.Getpid()) {
			t.Errorf("hooks say %q: caller = %d, %v", tc.why, pid, err)
		}
	}
}
