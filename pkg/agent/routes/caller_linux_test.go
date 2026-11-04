//go:build linux

package routes

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func TestProcAddrPortReadsBothFamilies(t *testing.T) {
	v4, ok := procAddrPort("0100007F:4762")
	if !ok || v4 != netip.MustParseAddrPort("127.0.0.1:18274") {
		t.Fatalf("v4: %v %v", v4, ok)
	}
	v6, ok := procAddrPort("0000000000000000FFFF00000100007F:4762")
	if !ok || !sameEndpoint(v6, netip.MustParseAddrPort("127.0.0.1:18274")) {
		t.Fatalf("v4-mapped v6: %v %v", v6, ok)
	}
	if _, ok := procAddrPort("zz:1"); ok {
		t.Fatal("garbage read as an address")
	}
}

// tcpOwner matches the socket by both ends, so a listener or another
// connection on the same port is never taken for the caller.
func TestTCPOwnerMatchesBothEnds(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("net/tcp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   0: 0100007F:41ED 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 111 1\n"+
		"   1: 0100007F:C350 0100007F:41ED 01 00000000:00000000 00:00000000 00000000  1000        0 222 1\n"+
		"   2: 0100007F:C351 0100007F:41ED 01 00000000:00000000 00:00000000 00000000  1000        0 333 1\n"+
		"   3: 0100007F:C352 0100007F:41ED 06 00000000:00000000 00:00000000 00000000  1000        0 0 1\n"+
		"   4: 0100007F:C353 0100007F:41ED 01 00000000:00000000 00:00000000 00000000  1000        0 444 1\n"+
		"   5: 0100007F:C354 0100007F:41ED 01 00000000:00000000 00:00000000 00000000  1000        0 555 1\n")
	for pid, inode := range map[string]string{"10": "111", "20": "222", "30": "333", "40": "444", "41": "444", "50": "555", "51": "555"} {
		dir := filepath.Join(root, pid, "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("socket:["+inode+"]", filepath.Join(dir, "3")); err != nil {
			t.Fatal(err)
		}
	}
	// 51 is a child 50 is starting (forked, not yet exec'd): it holds a copy
	// of 50's socket. 40 and 41 are unrelated.
	for pid, ppid := range map[string]string{"50": "1", "51": "50", "40": "1", "41": "1"} {
		write(pid+"/stat", pid+" (go) S "+ppid+" 1 1 0 -1")
	}
	old := procRoot
	procRoot = root
	defer func() { procRoot = old }()

	if got, err := tcpOwner(netip.MustParseAddrPort("127.0.0.1:50004"), netip.MustParseAddrPort("127.0.0.1:16877")); err != nil || got != 50 {
		t.Fatalf("owner %d, %v; want 50, the process its child copied the socket from", got, err)
	}
	got, err := tcpOwner(netip.MustParseAddrPort("127.0.0.1:50001"), netip.MustParseAddrPort("127.0.0.1:16877"))
	if err != nil || got != 30 {
		t.Fatalf("owner %d, %v; want 30", got, err)
	}
	if _, err := tcpOwner(netip.MustParseAddrPort("127.0.0.1:50002"), netip.MustParseAddrPort("127.0.0.1:16877")); err == nil {
		t.Fatal("a closed connection (TIME_WAIT, inode 0) was taken for a caller")
	}
	if _, err := tcpOwner(netip.MustParseAddrPort("127.0.0.1:50003"), netip.MustParseAddrPort("127.0.0.1:16877")); err == nil || !strings.Contains(err.Error(), "held by processes") {
		t.Fatalf("a socket two processes hold names no one caller: %v", err)
	}
}

// A begin's caller, read while it is served (in a run of more than one test
// worker), is the process that sent it: this test binary run again.
func TestABeginsCallerIsReadFromItsConnection(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &wireSvc{readBegin: true, beginResp: models.ScopeBeginResp{V: 1, Status: "ok"}}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleScopeBegin))
	defer srv.Close()
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), envClaimFrom+"="+srv.URL, envBegin+"=1")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("the beginning child failed: %v: %s", err, out)
	}
	if len(svc.begun) != 1 || svc.beginPID != c.Process.Pid {
		t.Fatalf("begins %+v read as process %d, want one from the child %d", svc.begun, svc.beginPID, c.Process.Pid)
	}
}

// A socket's holder is read as the pid_t the kernel numbers its processes
// with. A /proc name past that is no process, so the claim that came on the
// socket is refused: it is never handed to the process the name's low 32
// bits give (4294967301 is 5 as the u32 TGID the instance stamp keys on),
// nor to another holder of the socket, and no worker is stamped.
func TestASocketHolderPastAProcessIDIsNoCaller(t *testing.T) {
	t.Run("names", func(t *testing.T) {
		for name, want := range map[string]int{"1": 1, "4194304": 4194304, "2147483647": 2147483647} {
			if got, err := pidOf(name); err != nil || got != want {
				t.Errorf("pidOf(%q) = %d, %v; want %d", name, got, err, want)
			}
		}
		for _, name := range []string{"0", "-5", "05", "+5", "2147483648", "4294967295", "4294967296", "4294967301", "18446744073709551621", "12a"} {
			if got, err := pidOf(name); err == nil {
				t.Errorf("pidOf(%q) = %d: read as a process", name, got)
			}
		}
	})
	for _, tc := range []struct{ name, holder, child string }{
		{"just past a pid_t", "2147483648", ""},
		{"past a u32", "4294967296", ""},
		{"past a u32, its u32 5", "4294967301", ""},
		{"past a u32, its child holding the socket too", "4294967301", "77"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(rel, body string) {
				p := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("net/tcp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
				"   0: 0100007F:C350 0100007F:41ED 01 00000000:00000000 00:00000000 00000000  1000        0 222 1\n")
			holders := []string{tc.holder}
			if tc.child != "" {
				holders = append(holders, tc.child)
				write(tc.child+"/stat", tc.child+" (go) S "+tc.holder+" 1 1 0 -1")
			}
			for _, pid := range holders {
				dir := filepath.Join(root, pid, "fd")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("socket:[222]", filepath.Join(dir, "3")); err != nil {
					t.Fatal(err)
				}
			}
			old := procRoot
			procRoot = root
			t.Cleanup(func() { procRoot = old })

			self, peer := netip.MustParseAddrPort("127.0.0.1:50000"), netip.MustParseAddrPort("127.0.0.1:16877")
			if got, err := tcpOwner(self, peer); err == nil || got != 0 || !strings.Contains(err.Error(), filepath.Join(root, tc.holder)+", which is not a process ID") {
				t.Fatalf("owner %d, %v; want no owner, the holder %s named", got, err, tc.holder)
			}

			t.Setenv(provenswitch.Env, "1")
			svc := &refusingSvc{}
			a := &Agent{logger: zap.NewNop(), svc: svc}
			req := httptest.NewRequest(http.MethodPost, "/agent/instance", strings.NewReader(`{"key":"o","workers":2}`))
			req.RemoteAddr = self.String()
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, net.TCPAddrFromAddrPort(peer)))
			rec := httptest.NewRecorder()
			a.HandleInstance(rec, req)
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), tc.holder) {
				t.Fatalf("the claim was answered %d %s; want the agent's refusal naming %s", rec.Code, rec.Body.String(), tc.holder)
			}
			if svc.claimKey != "" || svc.claimCaller != 0 {
				t.Fatalf("the run was claimed for process %d", svc.claimCaller)
			}
			if len(svc.refusals) != 1 || svc.refusals[0].Code != models.ScopeErrNoCaller {
				t.Fatalf("refusals %+v; want the one no-caller refusal", svc.refusals)
			}
		})
	}
}
