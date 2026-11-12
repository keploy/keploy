//go:build linux

package linux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// fakeProc writes a /proc of stat files: pid -> (ppid, utime, stime,
// cutime, cstime), each process started at clock tick pid (its starttime);
// each process's mount namespace (ns/mnt) is the application's container's,
// but those in agentCtr's the agent's, and those in otherCtr's another
// container's.
func fakeProc(t *testing.T, procs map[int][5]int, agentCtr []int, otherCtr ...int) string {
	t.Helper()
	root := t.TempDir()
	for pid, v := range procs {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime priority nice threads itreal starttime ...
		stat := fmt.Sprintf("%d (a (weird) name) S %d 1 1 0 -1 0 0 0 0 0 %d %d %d %d 20 0 1 0 %d 100", pid, v[0], v[1], v[2], v[3], v[4], pid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		mnt := "mnt:[4026532001]"
		if slices.Contains(agentCtr, pid) {
			mnt = "mnt:[4026532000]"
		}
		if slices.Contains(otherCtr, pid) {
			mnt = "mnt:[4026532002]"
		}
		if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(mnt, filepath.Join(dir, "ns", "mnt")); err != nil {
			t.Fatal(err)
		}
		// Every process's unified cgroup the same, the root, as on a cgroup v1
		// host: it tells no container from another.
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil { // not a pid: skipped
		t.Fatal(err)
	}
	return root
}

// A native app's CPU time is its processes', the keploy client's
// descendants, each with its reaped children's; never the client's, the
// agent's (the client may have started it, through sudo), or another
// process's.
func TestTheApplicationsCPUTimeIsTheClientsDescendants(t *testing.T) {
	root := fakeProc(t, map[int][5]int{
		1:   {0, 1000, 1000, 1000, 1000}, // init
		10:  {1, 500, 0, 0, 0},           // the keploy client
		11:  {10, 7, 3, 0, 0},            // sudo, the agent's parent
		12:  {11, 900, 0, 0, 0},          // the agent
		20:  {10, 4, 1, 2, 3},            // the app
		21:  {20, 5, 0, 0, 0},            // its worker
		30:  {1, 800, 0, 0, 0},           // another process
		300: {30, 1, 0, 0, 0},
	}, nil)
	ms, ok := memberCPU(root, 12, false, 10, "")
	if got := ownCPU(ms); !ok || got != (4+1+5)*clockTick || len(ms) != 2 {
		t.Fatalf("memberCPU = %v (%v), %v; want %v of the app and its worker, each its own (not its reaped children's); not sudo, the agent, the client or another process", got, ms, ok, 10*clockTick)
	}
	if _, ok := memberCPU(root, 12, false, 0, ""); ok {
		t.Fatal("memberCPU with no client known: want unknown")
	}
}

// The keploy client's pid is read in the agent's pid namespace: a pid the
// client said in another namespace (a CI step container's) names another
// process here, whose descendants are not the application, so the CPU time
// is unknown; said in the agent's own namespace, it is its descendants'.
func TestTheApplicationsCPUTimeIsNeverAnotherNamespacesClients(t *testing.T) {
	root := fakeProc(t, map[int][5]int{
		1:  {0, 1000, 1000, 1000, 1000},
		10: {1, 500, 0, 0, 0},  // the process at the client's pid
		11: {10, 7, 3, 0, 0},   // sudo, the agent's parent
		12: {11, 900, 0, 0, 0}, // the agent
		20: {10, 4, 1, 2, 3},
		21: {20, 5, 0, 0, 0},
	}, nil)
	for _, f := range []struct{ link, path string }{{"pid:[100]", "self/ns/pid"}, {"pid:[100]", "10/ns/pid"}} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, f.path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.link, filepath.Join(root, f.path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "sys", "kernel", "random"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys", "kernel", "random", "boot_id"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "10", "status"), []byte("NSpid:\t10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ms, ok := memberCPU(root, 12, false, 10, "999@b"); ok {
		t.Fatalf("memberCPU of a client said in another pid namespace = %v; want unknown, not the descendants of the process at its pid here", ms)
	}
	ms, ok := memberCPU(root, 12, false, 10, "100@b")
	if got := ownCPU(ms); !ok || got != (4+1+5)*clockTick || len(ms) != 2 {
		t.Fatalf("memberCPU of a client said in the agent's namespace = %v (%v), %v; want the app and its worker", got, ms, ok)
	}
}

// The client's registration keeps the pid namespace it said beside its pid,
// which the readiness watch reads the registered pid through (as root: the
// registration is a kernel map's).
func TestTheClientsRegistrationKeepsItsPIDNamespace(t *testing.T) {
	m, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 8, ValueSize: uint32(binary.Size(structs.ClientInfo{})), MaxEntries: 1})
	if err != nil {
		t.Skipf("no kernel map to register in (needs root): %v", err)
	}
	defer m.Close()
	h := &Hooks{logger: zap.NewNop(), clientRegistrationMap: m}
	var opts config.Agent
	opts.ClientNSPID, opts.ClientPIDNS = 10, "100@b"
	if err := h.registerClient(structs.ClientInfo{ClientNSPID: opts.ClientNSPID}, opts); err != nil {
		t.Fatal(err)
	}
	h.clientMu.Lock()
	defer h.clientMu.Unlock()
	if h.clientPIDNS != "100@b" {
		t.Fatalf("the registration keeps pid namespace %q, want the client's %q", h.clientPIDNS, "100@b")
	}
}

// In Docker mode the application's processes are each container's main
// process (its oldest whose parent is outside the PID namespace) and its
// descendants, but the agent's container's: never the agent, those above it,
// what runs in its container (keploy-agent's healthcheck, a docker exec into
// it), or what an exec into the application's container starts (its own
// healthcheck), whatever their cgroups.
func TestADockerAppsCPUTimeIsEachContainersMainAndItsDescendants(t *testing.T) {
	root := fakeProc(t, map[int][5]int{
		1:  {0, 50, 0, 0, 0},  // the agent's init (tini)
		7:  {1, 900, 0, 0, 0}, // the agent
		9:  {0, 300, 0, 0, 0}, // its healthcheck, exec'd into its container
		40: {0, 30, 10, 0, 0}, // the app, its container's main, joined into the namespace
		41: {40, 20, 0, 5, 0}, // its child
		45: {0, 400, 0, 0, 0}, // the app container's healthcheck, exec'd into it
		46: {45, 70, 0, 0, 0}, // what that runs
		50: {0, 2, 0, 0, 0},   // another container's main in the namespace (a sidecar)
		35: {1, 99, 0, 0, 0},  // older than the app's main, in its container, its parent the agent's init: no main
		60: {1, 80, 0, 0, 0},  // a daemon the app's reparented to the agent's init
	}, []int{1, 7, 9}, 50)
	ms, ok := memberCPU(root, 7, true, 0, "")
	if got := ownCPU(ms); !ok || got != 62*clockTick {
		t.Fatalf("memberCPU = %v (%v), %v; want %v (the app and its child, and the sidecar; not the agent's container, an exec into the app's, or a daemon)", got, ms, ok, 62*clockTick)
	}
	if _, ok := memberCPU(root, 99, true, 0, ""); ok {
		t.Fatal("an agent its /proc does not show: want unknown")
	}
}

// A listen report names its address as the kernel gave it (network order),
// IPv4 unmapped.
func TestAListenReportNamesItsAddress(t *testing.T) {
	w := &readyWatch{netns: 7}
	v4 := readyListenEvent{TGID: 5, Netns: 7, Port: 8080, Family: 2, Addr: [4]uint32{0, 0, 0xffff0000, 0x0100007f}, Cookie: 99}
	l := w.handle(v4)
	if l.Addr != "127.0.0.1" || !l.Here || l.Port != 8080 || l.TGID != 5 || l.Cookie != 99 || l.Seq != 0 {
		t.Fatalf("handle(%+v) = %+v", v4, l)
	}
	v6 := readyListenEvent{Netns: 8, Port: 443, Family: 10, Addr: [4]uint32{0, 0, 0, 0x01000000}}
	if l := w.handle(v6); l.Addr != "::1" || l.Here || l.Seq != 1 {
		t.Fatalf("handle(%+v) = %+v, want ::1 in another namespace, the second listen told", v6, l)
	}
	if got := len(w.listens); got != 2 {
		t.Fatalf("%d listens kept, want 2", got)
	}
}

// The watch numbers each listen it is told (Seq, from its count) and keeps
// at most listensPerAddress of one socket address, dropping that address's
// oldest; the listens asked for from since on are those numbered since or
// later, and the count is every listen told.
func TestTheWatchKeepsAnAddresssLatestListensNumbered(t *testing.T) {
	w := &readyWatch{netns: 7}
	v4 := func(port uint16) readyListenEvent {
		return readyListenEvent{Netns: 7, Port: port, Family: 2, Addr: [4]uint32{0, 0, 0xffff0000, 0}}
	}
	extra := 6
	for i := 0; i < listensPerAddress+extra; i++ {
		if l := w.handle(v4(8080)); l.Seq != uint64(i) {
			t.Fatalf("listen %d numbered %d", i, l.Seq)
		}
	}
	w.handle(v4(9090))
	told, all, err := w.listensFrom(0)
	if err != nil || told != uint64(listensPerAddress+extra+1) || len(all) != listensPerAddress+1 {
		t.Fatalf("told %d, %d kept (%v); want %d told, %d kept", told, len(all), err, listensPerAddress+extra+1, listensPerAddress+1)
	}
	if all[0].Port != 8080 || all[0].Seq != uint64(extra) || all[len(all)-1].Port != 9090 {
		t.Fatalf("kept %v ... %v; want 8080's from Seq %d on, then 9090's", all[0], all[len(all)-1], extra)
	}
	last := uint64(listensPerAddress + extra - 1) // 8080's latest
	if _, from, _ := w.listensFrom(last); len(from) != 2 || from[0].Seq != last {
		t.Fatalf("from %d: %v, want 8080's latest and 9090's", last, from)
	}
	if n, none, _ := w.listensFrom(told); len(none) != 0 || n != told {
		t.Fatalf("from the count %d: %v (count %d), want none", told, none, n)
	}
	w.fail(errors.New("ring closed"))
	if _, _, err := w.listensFrom(0); err == nil {
		t.Fatal("a watch whose reports stopped answers listens, want its error")
	}
}

// The last restart's program is its name up to the NUL: a shorter name the
// kernel wrote over a longer one leaves the longer one's tail after it.
func TestTheLastRestartIsItsNameUpToTheNUL(t *testing.T) {
	var b [64]byte
	copy(b[:], "/usr/bin/sleep\x00-e2e")
	if got := cString(b[:]); got != "/usr/bin/sleep" {
		t.Fatalf("cString = %q, want /usr/bin/sleep", got)
	}
	full := []byte("0123456789")
	if got := cString(full); got != "0123456789" {
		t.Fatalf("cString with no NUL = %q", got)
	}
}

// With the switch off the readiness watch is never started: its maps load
// at their smallest and the agent says it watches nothing.
func TestTheReadinessWatchStaysOffWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	h := &Hooks{}
	flags, err := h.startProvenReady("/nonexistent", false, models.MODE_TEST)
	if err != nil || flags != 0 {
		t.Fatalf("startProvenReady = %d, %v; want nothing started", flags, err)
	}
	t.Setenv(provenswitch.Env, "1")
	if flags, err := h.startProvenReady("/nonexistent", false, models.MODE_RECORD); err != nil || flags != 0 {
		t.Fatalf("startProvenReady for a recording = %d, %v; want nothing started: only keploy test reads it", flags, err)
	}
	t.Setenv(provenswitch.Env, "0")
	if _, err := h.AppReadiness(0); !errors.Is(err, models.ErrReadinessUnsupported) {
		t.Fatalf("AppReadiness with the switch off: %v, want an agent that cannot watch (models.ErrReadinessUnsupported)", err)
	}
	spec, err := hookSpec()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ready_socks", "ready_mains", "ready_live", "ready_listen_events"} {
		m := spec.Maps[name]
		if m == nil {
			t.Fatalf("the object has no %s", name)
		}
		if want := uint32(1); name != "ready_listen_events" && m.MaxEntries != want {
			t.Errorf("%s: %d entries with the switch off, want %d", name, m.MaxEntries, want)
		}
		if want := uint32(os.Getpagesize()); name == "ready_listen_events" && m.MaxEntries != want {
			t.Errorf("%s: %d bytes with the switch off, want a page", name, m.MaxEntries)
		}
	}
}

// A process in the agent's network namespace from another container, before
// the agent watches, is one whose container the kernel knows no main of:
// found, the lowest such pid; one in the agent's own container, or in
// another network namespace, is none.
func TestAContainerAlreadyInTheAgentsNetworkNamespaceIsFound(t *testing.T) {
	root := t.TempDir()
	proc := func(pid int, net, mnt string) {
		dir := filepath.Join(root, fmt.Sprint(pid), "ns")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, target := range map[string]string{"net": net, "mnt": mnt} {
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	proc(7, "net:[1]", "mnt:[10]")  // the agent
	proc(9, "net:[1]", "mnt:[10]")  // its healthcheck
	proc(30, "net:[2]", "mnt:[11]") // another network namespace's
	if pid, ok, err := strangerInNetns(root, 7); ok || err != nil {
		t.Fatalf("found %d (%v), want none", pid, err)
	}
	proc(41, "net:[1]", "mnt:[12]") // an application container started before the agent
	proc(40, "net:[1]", "mnt:[12]")
	if pid, ok, err := strangerInNetns(root, 7); !ok || pid != 40 || err != nil {
		t.Fatalf("found %d, %v (%v); want 40", pid, ok, err)
	}
	// A process gone since the listing (its directory without its
	// namespaces, as /proc leaves it while it is reaped) is no stranger.
	if err := os.MkdirAll(filepath.Join(root, "60"), 0o755); err != nil {
		t.Fatal(err)
	}
	if pid, ok, err := strangerInNetns(root, 7); !ok || pid != 40 || err != nil {
		t.Fatalf("with a process gone: found %d, %v (%v); want 40", pid, ok, err)
	}
	// What cannot be read is no "none": a process whose namespaces it
	// cannot see (a directory, not a link, here).
	if err := os.MkdirAll(filepath.Join(root, "50", "ns", "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := strangerInNetns(root, 7); err == nil {
		t.Fatal("a process whose namespaces cannot be read: want an error, not none")
	}
	if _, _, err := strangerInNetns(root, 8); err == nil {
		t.Fatal("an agent whose own namespaces cannot be read: want an error")
	}
}

// The answer keeps, of the listens asked for, the latest for each port,
// address, namespace and liveness, in order: however often an application
// listens, the answer is as small as the sockets it listens on.
func TestTheReadinessAnswerKeepsTheLatestListenOfEachSocketAddress(t *testing.T) {
	l := func(seq uint64, port uint16, live bool) models.AppListen {
		return models.AppListen{Port: port, Addr: "0.0.0.0", Here: true, Live: live, Seq: seq}
	}
	got := latestOfEach([]models.AppListen{l(0, 8080, false), l(1, 9090, true), l(2, 8080, false), l(3, 8080, true), l(4, 9090, true)})
	want := []models.AppListen{l(2, 8080, false), l(3, 8080, true), l(4, 9090, true)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("latestOfEach = %v, want %v", got, want)
	}
}

// An agent whose container the kernel could not record (registerSelf failed)
// is one that cannot watch: the first test's own connect, or a health URL,
// decides, never a stall.
func TestAnAgentThatCouldNotRecordItsContainerCannotWatch(t *testing.T) {
	w := &readyWatch{docker: true}
	w.selfOnce.Do(func() {})
	w.selfErr = errors.New("the kernel recorded no mount namespace for the agent's own bind")
	h := &Hooks{ready: w}
	if _, err := h.AppReadiness(0); !errors.Is(err, models.ErrReadinessUnsupported) || !strings.Contains(err.Error(), "recorded no mount namespace") {
		t.Fatalf("AppReadiness = %v, want an agent that cannot watch, saying why", err)
	}
}

// ownCPU sums members' own CPU time.
func ownCPU(ms map[int]procStat) time.Duration {
	var n uint64
	for _, m := range ms {
		n += m.cpu
	}
	return time.Duration(n) * clockTick
}

// The application's CPU time as progress grows by what its processes use,
// each its own: what a process used before it exited stays counted; a
// process a supervisor starts again counts (keploy test stops weighing CPU
// time once the kernel says the start restarted); a daemon whose parent
// exited counts as it grows; a pid used again is another process, whose CPU
// time counts from nothing.
func TestTheApplicationsCPUTimeIsEachProcesssOwn(t *testing.T) {
	var c cpuTally
	app := func(ppid int, cpu, start uint64) procStat {
		return procStat{ppid: ppid, cpu: cpu, start: start}
	}
	at := func(ms map[int]procStat, want uint64) {
		t.Helper()
		if got := c.add(ms); got != time.Duration(want)*clockTick {
			t.Fatalf("CPU %v, want %v", got, time.Duration(want)*clockTick)
		}
	}
	at(map[int]procStat{10: app(1, 5, 1), 20: app(10, 30, 2)}, 35)
	// The app crashes, its supervisor starts it again.
	at(map[int]procStat{10: app(1, 6, 1), 21: app(10, 30, 3)}, 66)
	// A daemon: its master forks it and exits; it runs on, alone.
	at(map[int]procStat{40: app(1, 2, 4), 41: app(40, 3, 5)}, 71)
	at(map[int]procStat{41: app(1, 50, 5)}, 118)
	// A pid used again between two reads: another process, whose 2 ticks
	// count though the one before had 30.
	at(map[int]procStat{41: app(1, 50, 5), 30: app(41, 30, 6)}, 148)
	at(map[int]procStat{41: app(1, 50, 5), 30: app(41, 2, 9)}, 150)
}
