package containerkey

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// cid is a container ID (64 hex digits) of seed.
func cid(seed string) string {
	return strings.Repeat(fmt.Sprintf("%x", seed), 64)[:64]
}

// fakeCgroups is a cgroup tree of known directories (path by ID; "" for a
// directory none holds), and the sockets in its cgroups (cgroup ID by the
// app end's port), with the finds it was asked.
type fakeCgroups struct {
	dirs  map[uint64]string
	socks map[uint16]uint64
	finds map[uint64]int
}

func (f *fakeCgroups) cgroupOf(local, _ netip.AddrPort) (uint64, error) {
	cg, ok := f.socks[local.Port()]
	if !ok {
		return 0, errors.New("the kernel gives no cgroup of its socket (a test's)")
	}
	return cg, nil
}

func (f *fakeCgroups) find(id uint64) (string, bool, error) {
	f.finds[id]++
	p, ok := f.dirs[id]
	if !ok {
		return "", false, errors.New("keploy cannot read the cgroup directory (a test's)")
	}
	return p, p != "", nil
}

// end is the app end of a connection from port to the proxy.
func end(port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)
}

var proxyEnd = netip.MustParseAddrPort("127.0.0.1:16789")

// TestEachAppContainersConnectionsTakeItsOwnZone: a docker-mode agent's app
// containers share its PID and network namespaces, so neither a connection's
// PID nor its address names the container that made it; its socket's cgroup
// does. Compose's two services in two zones each have their connections
// learned in their own (docker-<id>.scope of systemd's driver, docker/<id>
// of cgroupfs', a process in a cgroup under its container's, the innermost
// container of a cgroup under another's), never the other's. A connection keploy cannot place, or of a container it was given
// no zone of, is unknown, and says why.
func TestEachAppContainersConnectionsTakeItsOwnZone(t *testing.T) {
	kolkata, ny := zoneData(t, "Asia/Kolkata"), zoneData(t, "America/New_York")
	web, worker, db, jvm, unlisted := cid("web"), cid("worker"), cid("db"), cid("jvm"), cid("new")
	p := newPushedIndex()
	p.apply(Snapshot{Source: "s", Seq: 1, Containers: []SnapshotContainer{
		{ID: web, Image: "web:1", Fingerprint: fpDefault, Zone: &Zone{Name: "Asia/Kolkata", Data: kolkata}},
		{ID: worker, Image: "worker:1", Fingerprint: fpDefault, Zone: &Zone{Name: "America/New_York", Data: ny}},
		{ID: db, Image: "postgres:16", Fingerprint: fpDefault},
		{ID: jvm, Image: "jvm:1", Name: "jvm-1", Fingerprint: fpDefault, Zone: &Zone{Err: "a JVM's option sets its zone"}},
	}})
	f := &fakeCgroups{
		dirs: map[uint64]string{
			101: "/system.slice/docker-" + web + ".scope",
			102: "/docker/" + worker,
			103: "/system.slice/docker-" + web + ".scope/workers",
			104: "/system.slice/docker-" + db + ".scope",
			105: "/system.slice/docker-" + jvm + ".scope",
			106: "/system.slice/docker-" + unlisted + ".scope",
			107: "/system.slice/sshd.service",
			108: "",
			109: "/docker/" + worker + "/docker/" + web,
		},
		socks: map[uint16]uint64{1: 101, 2: 102, 3: 103, 4: 104, 5: 105, 6: 106, 7: 107, 8: 108, 10: 110, 11: 109},
		finds: map[uint64]int{},
	}
	dirs := &cgroupDirs{find: f.find}
	at := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		port uint16
		off  int
	}{{1, 19800}, {2, -4 * 3600}, {3, 19800}, {1, 19800}, {11, 19800}} {
		z, err := p.connZone(t.Context(), f.cgroupOf, dirs, end(c.port), proxyEnd)
		if err != nil {
			t.Fatalf("the zone of a connection from port %d: %v", c.port, err)
		}
		if _, off := at.In(z.Loc).Zone(); off != c.off {
			t.Errorf("a connection from port %d is learned in %v (offset %d); want offset %d", c.port, z.Loc, off, c.off)
		}
	}
	if f.finds[101] != 1 {
		t.Errorf("cgroup 101's directory was looked for %d times; want once, then kept", f.finds[101])
	}
	for _, c := range []struct {
		port uint16
		want string
	}{
		{4, "is no app container"},
		{5, "could not read the zone of container jvm-1"},
		{5, "a JVM's option sets its zone"},
		{6, "of no container in the list"},
		{7, "of no container in the list"},
		{8, "under no cgroup2 mount"},
		{9, "the kernel gives no cgroup of its socket"},
		{10, "cannot read the cgroup directory"},
	} {
		z, err := p.connZone(t.Context(), f.cgroupOf, dirs, end(c.port), proxyEnd)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("a connection from port %d is learned in %v (%v); want unknown, saying %q", c.port, z.Loc, err, c.want)
		}
	}
	// A cgroup none holds is kept so; one keploy could not read is
	// looked for again.
	_, _ = p.connZone(t.Context(), f.cgroupOf, dirs, end(8), proxyEnd)
	_, _ = p.connZone(t.Context(), f.cgroupOf, dirs, end(10), proxyEnd)
	if f.finds[108] != 1 || f.finds[110] != 2 {
		t.Errorf("finds: of a cgroup none holds %d, want 1; of one keploy could not read %d, want 2", f.finds[108], f.finds[110])
	}
}

// TestAConnectionOfAContainerStartedAsItWasMadeAsksForAList: a connection
// whose cgroup names a container the list held does not hold (Docker
// started it as the connection was made, before the CLI's list) asks the
// CLI for a list (Settle), and takes that container's zone from it.
func TestAConnectionOfAContainerStartedAsItWasMadeAsksForAList(t *testing.T) {
	kolkata := zoneData(t, "Asia/Kolkata")
	late := cid("late")
	p := newPushedIndex()
	p.apply(Snapshot{Source: "s", Seq: 1, Settles: true, Containers: []SnapshotContainer{}})
	f := &fakeCgroups{dirs: map[uint64]string{201: "/system.slice/docker-" + late + ".scope"}, socks: map[uint16]uint64{1: 201}, finds: map[uint64]int{}}
	asks := make(chan SettleAsk, 1)
	go func() { asks <- p.awaitAsk(t.Context(), p.boot, 0) }()
	type result struct {
		off int
		err error
	}
	got := make(chan result, 1)
	go func() {
		z, err := p.connZone(t.Context(), f.cgroupOf, &cgroupDirs{find: f.find}, end(1), proxyEnd)
		if err != nil {
			got <- result{err: err}
			return
		}
		_, off := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC).In(z.Loc).Zone()
		got <- result{off: off}
	}()
	var ask SettleAsk
	select {
	case ask = <-asks:
	case r := <-got:
		t.Fatalf("the connection was answered (%+v) without asking the CLI for a list", r)
	}
	select {
	case r := <-got:
		t.Fatalf("the connection was answered (%+v) before the CLI answered its ask", r)
	default:
	}
	p.apply(Snapshot{Source: "s", Seq: 2, Settles: true, Settled: ask.Asked, SettledBoot: p.boot, Containers: []SnapshotContainer{
		{ID: late, Image: "web:1", Fingerprint: fpDefault, Zone: &Zone{Name: "Asia/Kolkata", Data: kolkata}},
	}})
	if r := <-got; r.err != nil || r.off != 19800 {
		t.Fatalf("the connection of the container the answer lists: offset %d, %v; want IST's 19800", r.off, r.err)
	}
}

// TestContainerIDsIn: a cgroup path's container IDs are its runs of exactly
// 64 hex digits, outermost first; a longer or shorter run is none.
func TestContainerIDsIn(t *testing.T) {
	a, b := cid("a"), cid("b")
	for _, c := range []struct {
		path string
		want []string
	}{
		{"/system.slice/docker-" + a + ".scope", []string{a}},
		{"/docker/" + a + "/docker/" + b, []string{a, b}},
		{"/" + a, []string{a}},
		{"/docker/" + a + "0", nil},
		{"/docker/" + a[:63], nil},
		{"/user.slice/user-1000.slice", nil},
	} {
		if got := containerIDsIn(c.path); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("containerIDsIn(%q) = %v; want %v", c.path, got, c.want)
		}
	}
}
