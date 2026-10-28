//go:build linux

package containerkey

import (
	"bufio"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// ownCgroup is this process's cgroup v2 path (/proc/self/cgroup's "0::"
// line); the test is skipped on a host of cgroup v1 alone.
func ownCgroup(t *testing.T) string {
	t.Helper()
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		t.Skipf("no /proc/self/cgroup: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return p
		}
	}
	t.Skip("this host has no cgroup v2 hierarchy")
	return ""
}

// TestTheKernelGivesTheCgroupOfTheAppsEndOfAConnection: the kernel's
// sock_diag gives the cgroup of the process that made a connection's
// socket, asked by the app's end (its own address, then the proxy's), and
// that cgroup's directory is found under this process's cgroup2 mounts:
// the path /proc/self/cgroup names. A socket the kernel holds none of is
// gone, and said so.
func TestTheKernelGivesTheCgroupOfTheAppsEndOfAConnection(t *testing.T) {
	want := ownCgroup(t)
	// The app's end at another address than the proxy's: a query of the
	// proxy's ports at the app's address finds no socket.
	for _, c := range []struct{ network, from string }{{"127.0.0.1:0", "127.0.0.2:0"}, {"[::1]:0", ""}} {
		network := c.network
		ln, err := net.Listen("tcp", network)
		if err != nil {
			if strings.HasPrefix(network, "[") {
				t.Logf("no IPv6 loopback: %v", err)
				continue
			}
			t.Fatal(err)
		}
		defer ln.Close()
		accepted := make(chan net.Conn, 1)
		go func() {
			c, _ := ln.Accept()
			accepted <- c
		}()
		var d net.Dialer
		if c.from != "" {
			d.LocalAddr = net.TCPAddrFromAddrPort(netip.MustParseAddrPort(c.from))
		}
		app, err := d.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		proxySide := <-accepted
		if proxySide == nil {
			t.Fatal("no connection accepted")
		}
		defer proxySide.Close()
		// The proxy asks by its accepted end: the remote is the app's.
		appEnd := proxySide.RemoteAddr().(*net.TCPAddr).AddrPort()
		proxyEnd := proxySide.LocalAddr().(*net.TCPAddr).AddrPort()
		cg, err := socketCgroup(appEnd, proxyEnd)
		if err != nil {
			t.Fatalf("%s: the cgroup of the app's end: %v", network, err)
		}
		var st unix.Stat_t
		path, found, err := findCgroupDir(cg)
		if err != nil || !found {
			t.Fatalf("%s: cgroup %d's directory: %q, %v, %v; want %s", network, cg, path, found, err, want)
		}
		if path != want {
			t.Errorf("%s: the app end's cgroup %d is %s; want this process's, %s", network, cg, path, want)
		}
		if err := unix.Stat(filepath.Join("/sys/fs/cgroup", want), &st); err == nil && st.Ino != cg {
			t.Errorf("%s: the app end's cgroup %d is not this process's (%d)", network, cg, st.Ino)
		}
	}
	gone := netip.MustParseAddrPort("127.0.0.1:1")
	if _, err := socketCgroup(gone, netip.MustParseAddrPort("127.0.0.1:2")); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Errorf("a socket the kernel holds none of: %v; want it said gone", err)
	}
}

// TestACgroupsDirectoryIsFoundByItsID: a cgroup's ID is its directory's
// inode number; its path is the mount's root in the hierarchy and the
// directory's under the mount. A directory none holds is not found, and no
// error; a file of the ID is no cgroup.
func TestACgroupsDirectoryIsFoundByItsID(t *testing.T) {
	at := t.TempDir()
	scope := filepath.Join(at, "system.slice", "docker-"+cid("web")+".scope")
	if err := os.MkdirAll(filepath.Join(scope, "workers"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(scope, "cgroup.procs")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ino := func(p string) uint64 {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			t.Fatal(err)
		}
		return st.Ino
	}
	for _, c := range []struct {
		root, dir, want string
	}{
		{"/", scope, "/system.slice/docker-" + cid("web") + ".scope"},
		{"/", filepath.Join(scope, "workers"), "/system.slice/docker-" + cid("web") + ".scope/workers"},
		{"/kubepods", scope, "/kubepods/system.slice/docker-" + cid("web") + ".scope"},
	} {
		got, found, err := findCgroupDirIn([]cgroup2Mount{{at: at, root: c.root}}, ino(c.dir))
		if err != nil || !found || got != c.want {
			t.Errorf("the directory of %s under a mount of root %s: %q, %v, %v; want %q", c.dir, c.root, got, found, err, c.want)
		}
	}
	if got, found, err := findCgroupDirIn([]cgroup2Mount{{at: at, root: "/"}}, ino(file)); found || err != nil {
		t.Errorf("a file's inode was taken for a cgroup: %q, %v, %v", got, found, err)
	}
	if got, found, err := findCgroupDirIn([]cgroup2Mount{{at: filepath.Join(at, "none"), root: "/"}}, ino(scope)); found || err == nil {
		t.Errorf("a mount keploy cannot read: %q, %v, %v; want an error, never kept as none", got, found, err)
	}
}

// TestCgroup2MountsAreReadFromMountinfo: the cgroup2 mounts of a
// mountinfo, with their roots, their octal escapes undone; other file
// systems are none.
func TestCgroup2MountsAreReadFromMountinfo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mountinfo")
	lines := "" +
		"30 25 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:4 - cgroup2 cgroup2 rw,nsdelegate\n" +
		"31 25 0:27 / /sys/fs/bpf rw,nosuid shared:5 - bpf bpf rw\n" +
		"40 30 0:26 /system.slice /host\\040cgroup rw - cgroup2 cgroup2 rw\n" +
		"41 25 0:28 / /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n"
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	ms, err := cgroup2Mounts(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []cgroup2Mount{{at: "/sys/fs/cgroup", root: "/"}, {at: "/host cgroup", root: "/system.slice"}}
	if len(ms) != len(want) || ms[0] != want[0] || ms[1] != want[1] {
		t.Fatalf("cgroup2 mounts = %+v; want %+v", ms, want)
	}
}
