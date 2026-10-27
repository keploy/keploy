//go:build linux

package routes

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

// envClaimFrom makes this test binary, run again as a child, claim against
// the server at its value and exit.
const envClaimFrom = "KEPLOY_TEST_CLAIM_FROM"

func TestMain(m *testing.M) {
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
