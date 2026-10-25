//go:build linux

package utils

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const fixtureBoot = "0b5e2f2c-7d0e-4c4e-9a43-9d3a3c2b1a00"

// fakePIDNSProc is a /proc whose reader runs in pid namespace pid:[ownIno] on
// fixtureBoot (boot "" leaves the boot id out), and whose procs map each pid
// to its NSpid line and its ns/pid inode (0: no ns link).
type fakeProcess struct {
	nspid string // the NSpid line's pids; "" for a status that cannot be read
	ino   uint64
}

func fakePIDNSProc(t *testing.T, ownIno uint64, boot string, procs map[uint32]fakeProcess) string {
	t.Helper()
	root := t.TempDir()
	mk := func(dir string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk(filepath.Join(root, "self", "ns"))
	// The agent, pid 4242 of its own namespace: one namespace deep.
	if err := os.WriteFile(filepath.Join(root, "self", "status"), []byte("Name:\tagent\nNSpid:\t4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ownIno != 0 {
		if err := os.Symlink("pid:["+strconv.FormatUint(ownIno, 10)+"]", filepath.Join(root, "self", "ns", "pid")); err != nil {
			t.Fatal(err)
		}
	}
	if boot != "" {
		mk(filepath.Join(root, "sys", "kernel", "random"))
		if err := os.WriteFile(filepath.Join(root, "sys", "kernel", "random", "boot_id"), []byte(boot+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for pid, p := range procs {
		dir := filepath.Join(root, strconv.FormatUint(uint64(pid), 10))
		mk(filepath.Join(dir, "ns"))
		if p.nspid != "" {
			if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tx\nNSpid:\t"+p.nspid+"\nPPid:\t1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if p.ino != 0 {
			if err := os.Symlink("pid:["+strconv.FormatUint(p.ino, 10)+"]", filepath.Join(dir, "ns", "pid")); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// The CLI's pid is taken only where it is the CLI's in the agent's own pid
// namespace; a CLI in another namespace (a CI step container's, beside a
// docker agent's own) names another process here, which is never taken. An
// inode other than the agent's is another namespace on any kernel, so it is
// not taken even where the kernel is not known.
func TestClientPIDHereTakesTheCLIsPidOnlyInItsNamespace(t *testing.T) {
	same := "100@" + fixtureBoot
	procs := map[uint32]fakeProcess{
		7:  {nspid: "7", ino: 100},     // the CLI, in the agent's namespace
		8:  {nspid: "8 1", ino: 200},   // a process of a child namespace
		9:  {nspid: "9", ino: 300},     // its NSpid says 9, its ns another
		10: {nspid: "", ino: 100},      // status unreadable
		11: {nspid: "11 3", ino: 0},    // a child namespace's, no ns link
		13: {nspid: "13 13", ino: 0},   // a child namespace's at the same pid there, no ns link
		14: {nspid: "", ino: 300},      // status unreadable, its ns another
		15: {nspid: "15", ino: 0},      // the CLI, its ns/pid unreadable (no ptrace access)
		16: {nspid: "16 16", ino: 200}, // a child namespace's at the same pid there
	}
	proc := fakePIDNSProc(t, 100, fixtureBoot, procs)
	for _, c := range []struct {
		name   string
		pid    uint32
		ns     string
		want   uint32
		kind   ClientPIDKind
		whyHas string // "" = Why must be ""
	}{
		{"no pid said", 0, same, 0, ClientPIDTaken, ""},
		{"the CLI in the agent's namespace", 7, same, 7, ClientPIDTaken, ""},
		{"the CLI, its ns/pid unreadable", 15, same, 15, ClientPIDTaken, ""},
		{"another namespace on this kernel: a CI step container's", 7, "999@" + fixtureBoot, 0, ClientPIDElsewhere, "another pid namespace"},
		{"another namespace's CLI at no process here", 12, "999@" + fixtureBoot, 0, ClientPIDElsewhere, "another pid namespace"},
		{"the same inode on another kernel", 7, "100@another-boot", 0, ClientPIDElsewhere, "on another kernel"},
		{"another namespace that said no boot id", 7, "200", 0, ClientPIDElsewhere, "another pid namespace"},
		{"a CLI in no Linux namespace", 7, ClientPIDNSNone, 0, ClientPIDElsewhere, "no Linux pid namespace"},
		{"same namespace, the pid a child namespace's process", 8, same, 0, ClientPIDAnother, "has pid 1 in its own pid namespace"},
		{"same namespace, the pid's process in another namespace", 9, same, 0, ClientPIDAnother, "it is another process"},
		{"same namespace, a child namespace's at the same pid, no ns link", 13, same, 0, ClientPIDAnother, "2 pid namespaces deep"},
		{"same namespace, a child namespace's at the same pid", 16, same, 0, ClientPIDAnother, "another pid namespace (200)"},
		{"same namespace, no process at the pid", 12, same, 0, ClientPIDGone, "no process has the pid 12"},
		{"same namespace, status unreadable: the number, said", 10, same, 10, ClientPIDByNumber, "status of pid 10 cannot be read"},
		{"same namespace, status unreadable, its ns another", 14, same, 0, ClientPIDAnother, "another pid namespace (300)"},
		{"no namespace said (an older CLI): the number", 7, "", 7, ClientPIDByNumber, "did not say its pid namespace"},
		{"no namespace said, the pid cannot be the CLI's", 11, "", 0, ClientPIDNotTheCLI, "has pid 3 in its own pid namespace"},
		{"no namespace said, no process", 12, "", 0, ClientPIDGone, "no process has the pid 12"},
		{"no namespace said, status unreadable: the number", 10, "", 10, ClientPIDByNumber, "status of pid 10 cannot be read"},
		{"a value that does not parse", 7, "pid:[100]", 7, ClientPIDByNumber, "does not parse"},
		{"a value that does not parse, the pid cannot be the CLI's", 8, "pid:[100]", 0, ClientPIDNotTheCLI, "has pid 1 in its own pid namespace"},
		{"no boot id said", 7, "100", 7, ClientPIDByNumber, "did not say its kernel's boot id"},
		{"no boot id said, the pid's process in another namespace", 9, "100", 0, ClientPIDNotTheCLI, "it is another process"},
		{"no boot id said, a child namespace's at the same pid, no ns link", 13, "100", 0, ClientPIDNotTheCLI, "2 pid namespaces deep"},
	} {
		got := ClientPIDIn(proc, c.pid, c.ns)
		if got.PID != c.want || got.Kind != c.kind {
			t.Errorf("%s: pid %d, kind %d, want %d, %d (why %q)", c.name, got.PID, got.Kind, c.want, c.kind, got.Why)
		}
		if c.whyHas == "" && got.Why != "" || c.whyHas != "" && !strings.Contains(got.Why, c.whyHas) {
			t.Errorf("%s: why %q, want it to say %q", c.name, got.Why, c.whyHas)
		}
	}

	// A process whose status cannot be read and that exits before the agent
	// stats it again is no live process.
	clientStatusUnread = func(dir string) {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { clientStatusUnread = func(string) {} })
	if got := ClientPIDIn(proc, 10, same); got.PID != 0 || got.Kind != ClientPIDGone {
		t.Errorf("a process that exited after its status could not be read: %+v, want none", got)
	}
}

// What the agent cannot read of its own namespace or kernel leaves the
// number alone (as before keploy said the namespace), never a pid of another
// namespace, and never a process that cannot be the CLI.
func TestClientPIDHereKeepsTheNumberWhereItCannotCompare(t *testing.T) {
	procs := map[uint32]fakeProcess{
		7:  {nspid: "7", ino: 100},
		8:  {nspid: "8 1", ino: 200},
		9:  {nspid: "9", ino: 300},
		13: {nspid: "13 13", ino: 0},
	}
	for _, c := range []struct {
		name   string
		ownIno uint64
		boot   string
		pid    uint32
		ns     string
		want   uint32
		kind   ClientPIDKind
		whyHas string
	}{
		{"own namespace unreadable", 0, fixtureBoot, 7, "100@" + fixtureBoot, 7, ClientPIDByNumber, "cannot read its own pid namespace"},
		{"own boot id unreadable", 100, "", 7, "100@" + fixtureBoot, 7, ClientPIDByNumber, "cannot read its kernel's boot id"},
		{"own namespace unreadable, a child namespace's process", 0, fixtureBoot, 8, "100@" + fixtureBoot, 0, ClientPIDNotTheCLI, "has pid 1 in its own pid namespace"},
		{"own namespace unreadable, the pid's process in another namespace", 0, fixtureBoot, 9, "100@" + fixtureBoot, 0, ClientPIDNotTheCLI, "it is another process"},
		{"own namespace unreadable, the CLI on another kernel", 0, fixtureBoot, 7, "100@another-boot", 0, ClientPIDElsewhere, "on another kernel"},
		{"own boot id unreadable, another namespace", 100, "", 7, "200@" + fixtureBoot, 0, ClientPIDElsewhere, "another pid namespace"},
		{"own boot id unreadable, a child namespace's at the same pid", 100, "", 13, "100@" + fixtureBoot, 0, ClientPIDNotTheCLI, "2 pid namespaces deep"},
	} {
		got := ClientPIDIn(fakePIDNSProc(t, c.ownIno, c.boot, procs), c.pid, c.ns)
		if got.PID != c.want || got.Kind != c.kind || !strings.Contains(got.Why, c.whyHas) {
			t.Errorf("%s: %+v, want pid %d, kind %d, saying %q", c.name, got, c.want, c.kind, c.whyHas)
		}
	}
}

// The value a CLI says is the agent's own reading of its namespace: a CLI
// and an agent of one namespace compare equal, and this process is taken.
func TestOwnPIDNamespaceIsWhatTheAgentComparesWith(t *testing.T) {
	ns := OwnPIDNamespace()
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Skipf("no /proc/self/ns/pid: %v", err)
	}
	ino, _ := pidNSLinkInode(link)
	if !strings.HasPrefix(ns, strconv.FormatUint(ino, 10)+"@") {
		t.Fatalf("OwnPIDNamespace %q, want %d@<boot_id>", ns, ino)
	}
	if got := ClientPIDHere(uint32(os.Getpid()), ns); got.PID != uint32(os.Getpid()) || got.Why != "" {
		t.Fatalf("this process said by its own namespace: %+v", got)
	}
}
