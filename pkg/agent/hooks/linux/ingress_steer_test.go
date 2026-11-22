//go:build linux

package linux

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
)

// podShm is a recordingStartSource of a pod (PID and IPC namespaces of its
// own), whose /dev/shm is a test directory.
func podShm(t *testing.T, pidNS, init uint64) recordingStartSource {
	t.Helper()
	return recordingStartSource{
		pidNS:     func() (uint64, error) { return pidNS, nil },
		ipcNS:     func() (uint64, error) { return 4026532000, nil },
		initStart: func() (uint64, error) { return init, nil },
		shmDir:    t.TempDir(),
	}
}

// The steer table one agent keeps is read back by the next agent of the same
// pod, and by no other: another PID namespace, or the same inode with another
// init (a namespace the kernel gave the inode of a gone one), reads nothing.
func TestTheSteerTableIsReadBackOnlyByTheSamePod(t *testing.T) {
	src := podShm(t, 4026531999, 4711)
	notes := []appBindKey{{Port: 8080, Kind: appBindV4}, {Port: 8443, Kind: appBindV6Dual}}
	if err := writeSteerTable(src, notes); err != nil {
		t.Fatal(err)
	}
	path, mine, err := steerTablePath(src)
	if err != nil || path == "" {
		t.Fatalf("steerTablePath: %q, %v", path, err)
	}
	tbl, err := readSteerTable(path, mine)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Notes) != 2 || tbl.Notes[0] != notes[0] || tbl.Notes[1] != notes[1] {
		t.Fatalf("read back %+v, want %+v", tbl.Notes, notes)
	}
	for _, other := range []steerTable{{Version: 1, PIDNS: mine.PIDNS + 1, Init: mine.Init}, {Version: 1, PIDNS: mine.PIDNS, Init: mine.Init + 1}} {
		if _, err := readSteerTable(path, other); err == nil {
			t.Fatalf("a table of pidns %d init %d was read by pidns %d init %d", mine.PIDNS, mine.Init, other.PIDNS, other.Init)
		}
	}
}

// The host's PID or IPC namespace keeps no table: its /dev/shm outlives any
// pod.
func TestTheHostsNamespacesKeepNoSteerTable(t *testing.T) {
	for _, src := range []recordingStartSource{podShm(t, procPIDInitInode, 1), func() recordingStartSource {
		s := podShm(t, 4026531999, 1)
		s.ipcNS = func() (uint64, error) { return procIPCInitInode, nil }
		return s
	}()} {
		if err := writeSteerTable(src, []appBindKey{{Port: 1, Kind: appBindV4}}); err != nil {
			t.Fatal(err)
		}
		if ents, _ := os.ReadDir(src.shmDir); len(ents) != 0 {
			t.Fatalf("a table was kept in the host's namespace: %v", ents)
		}
	}
}

// What else may be at the table's path (a link, a FIFO, a damaged file, one
// too large) is not read; a write replaces it without writing through it.
func TestASteerTableThatIsNotOneIsNotRead(t *testing.T) {
	src := podShm(t, 4026531999, 4711)
	path, mine, _ := steerTablePath(src)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readSteerTable(path, mine); err == nil {
		t.Fatal("a table was read through a symbolic link")
	}
	if err := writeSteerTable(src, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "{}" {
		t.Fatalf("the write went through the link: %q", b)
	}
	// A FIFO is not read, even with a table written into it and its writer
	// gone while another reader holds it open (a read then gets the table and
	// an end of file, as from a file).
	_ = os.Remove(path)
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(`{"keployIngressSteer":1,"pidns":4026531999,"init":4711,"notes":[]}`); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if _, err := readSteerTable(path, mine); err == nil {
		t.Fatal("a FIFO was read as a table")
	}
	// A table of another version is not read, though it names this pod.
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(`{"keployIngressSteer":2,"pidns":4026531999,"init":4711,"notes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSteerTable(path, mine); err == nil {
		t.Fatal("a table of another version was read")
	}
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSteerTable(path, mine); err == nil {
		t.Fatal("a damaged table was read")
	}
	if err := os.WriteFile(path, make([]byte, maxSteerTableSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSteerTable(path, mine); err == nil {
		t.Fatal("an oversized table was read")
	}
	if _, err := readSteerTable(filepath.Join(src.shmDir, "none"), mine); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing table: %v, want not exist", err)
	}
}

// A listening socket's note is the one the bind hooks write for its bind
// (ingress_hold_note): IPv4, a v4-mapped IPv6 one as IPv4, :: by its
// IPV6_V6ONLY, any other IPv6 address as itself, without its zone.
func TestAListenersNoteIsTheBindHooksOne(t *testing.T) {
	v4 := appBindKey{Port: 80, Kind: appBindV4}
	copy(v4.Addr[:4], []byte{10, 1, 2, 3})
	for _, row := range []struct {
		name string
		s    util.ListenSocket
		want appBindKey
		ok   bool
	}{
		{"ipv4", util.ListenSocket{Family: unix.AF_INET, Addr: netip.MustParseAddr("10.1.2.3")}, v4, true},
		{"v4-mapped", util.ListenSocket{Family: unix.AF_INET6, Addr: netip.MustParseAddr("10.1.2.3")}, v4, true},
		{"dual", util.ListenSocket{Family: unix.AF_INET6, Addr: netip.IPv6Unspecified(), V6OnlyKnown: true}, appBindKey{Port: 80, Kind: appBindV6Dual}, true},
		{"v6only any", util.ListenSocket{Family: unix.AF_INET6, Addr: netip.IPv6Unspecified(), V6Only: true, V6OnlyKnown: true}, appBindKey{Port: 80, Kind: appBindV6}, true},
		{"v6only unknown", util.ListenSocket{Family: unix.AF_INET6, Addr: netip.IPv6Unspecified()}, appBindKey{}, false},
		{"link-local", util.ListenSocket{Family: unix.AF_INET6, Addr: netip.MustParseAddr("fe80::1%2")}, appBindKey{Port: 80, Kind: appBindV6, Addr: netip.MustParseAddr("fe80::1").As16()}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, ok := listenerNote(80, row.s)
			if ok != row.ok || (ok && got != row.want) {
				t.Fatalf("listenerNote = %+v, %v; want %+v, %v", got, ok, row.want, row.ok)
			}
		})
	}
}

// keploy forwards a steered call over the loopback where the application's
// socket that takes it is bound to any address and the loopback reaches that
// same socket: its TIME_WAIT is then one the kernel reuses, so keploy never
// runs out of source ports. Where the application bound the call's own
// address, or the loopback one too, or nothing that takes it, the call goes
// where it was sent.
func TestASteeredCallIsForwardedOverTheLoopbackWhereTheSameSocketTakesIt(t *testing.T) {
	v4 := func(port uint16, a ...byte) appBindKey {
		k := appBindKey{Port: port, Kind: appBindV4}
		copy(k.Addr[:4], a)
		return k
	}
	v6 := func(port uint16, kind uint16, a string) appBindKey {
		k := appBindKey{Port: port, Kind: kind}
		if a != "" {
			k.Addr = netip.MustParseAddr(a).As16()
		}
		return k
	}
	for _, r := range []struct {
		name  string
		to    string
		notes []appBindKey
		want  string
	}{
		{"ipv4 any", "10.1.2.3:80", []appBindKey{v4(80)}, "127.0.0.1:80"},
		{"dual-stack any, ipv4 call", "10.1.2.3:80", []appBindKey{v6(80, appBindV6Dual, "")}, "127.0.0.1:80"},
		{"dual-stack any, ipv6 call", "[fd00::5]:80", []appBindKey{v6(80, appBindV6Dual, "")}, "[::1]:80"},
		{"ipv6-only any, ipv6 call", "[fd00::5]:80", []appBindKey{v6(80, appBindV6, "")}, "[::1]:80"},
		{"v4-mapped call", "[::ffff:10.1.2.3]:80", []appBindKey{v4(80)}, "127.0.0.1:80"},
		{"the call's own address bound", "10.1.2.3:80", []appBindKey{v4(80), v4(80, 10, 1, 2, 3)}, "10.1.2.3:80"},
		{"the loopback bound too", "10.1.2.3:80", []appBindKey{v4(80), v4(80, 127, 0, 0, 1)}, "10.1.2.3:80"},
		{"ipv6 loopback bound too", "[fd00::5]:80", []appBindKey{v6(80, appBindV6, ""), v6(80, appBindV6, "::1")}, "[fd00::5]:80"},
		{"only its own address", "10.1.2.3:80", []appBindKey{v4(80, 10, 1, 2, 3)}, "10.1.2.3:80"},
		{"ipv4 any, ipv6 call", "[fd00::5]:80", []appBindKey{v4(80)}, "[fd00::5]:80"},
		{"another port's", "10.1.2.3:80", []appBindKey{v4(81)}, "10.1.2.3:80"},
		{"a loopback call", "127.0.0.1:80", []appBindKey{v4(80)}, "127.0.0.1:80"},
	} {
		t.Run(r.name, func(t *testing.T) {
			got := steerDialTarget(netip.MustParseAddrPort(r.to), func(k appBindKey) bool { return slices.Contains(r.notes, k) })
			if got.String() != r.want {
				t.Fatalf("steerDialTarget(%s) = %s, want %s", r.to, got, r.want)
			}
		})
	}
}

// The agent steers only with the hold on and both post_bind hooks attached
// (they note each bind and tell the agent of it), and never while observing
// only: the flag it sends and IngressSteers agree.
func TestTheAgentSteersOnlyWhereThePostBindHooksTellItOfEachBind(t *testing.T) {
	for _, r := range []struct {
		name                  string
		held, postBind, watch bool
		steers                bool
	}{
		{name: "held, both post_bind hooks", held: true, postBind: true, steers: true},
		{name: "held, a post_bind hook refused", held: true},
		{name: "held, observing only", held: true, postBind: true, watch: true},
		{name: "no hold", postBind: true},
	} {
		t.Run(r.name, func(t *testing.T) {
			prev := agent.AgentInfoCustomizer
			t.Cleanup(func() { agent.AgentInfoCustomizer = prev })
			agent.AgentInfoCustomizer = nil
			if r.watch {
				agent.AgentInfoCustomizer = func(info *structs.AgentInfo) { info.Flags |= structs.FlagObserveOnly }
			}
			h := NewHooks(zap.NewNop(), &config.Config{})
			if r.held {
				h.ingress = &ingressHold{}
			}
			h.kernelPortAlloc = r.postBind
			var sent structs.AgentInfo
			if err := h.sendFinalAgentInfo(structs.AgentInfo{}, func(*structs.AgentInfo) error { return nil }, func(i structs.AgentInfo) error { sent = i; return nil }); err != nil {
				t.Fatal(err)
			}
			if got := sent.Flags&structs.FlagIngressSteer != 0; got != r.steers {
				t.Fatalf("sent steer flag %v, want %v", got, r.steers)
			}
			if got := h.IngressSteers(); got != r.steers {
				t.Fatalf("IngressSteers %v, want %v", got, r.steers)
			}
		})
	}
}
