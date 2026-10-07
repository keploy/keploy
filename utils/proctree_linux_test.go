//go:build linux

package utils

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// isZombie reports whether pid has exited and is waiting for its parent to
// reap it.
func isZombie(t *testing.T, pid int) bool {
	t.Helper()
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatalf("read process %d: %v", pid, err)
	}
	// "pid (comm) state ...": comm may itself hold spaces and parentheses.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

// countingFS counts the stat files opened through it.
type countingFS struct {
	fs.FS
	stats atomic.Int64
}

func (c *countingFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, "/stat") {
		c.stats.Add(1)
	}
	return c.FS.Open(name)
}

func (c *countingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(c.FS, name)
}

func statLine(pid int, comm string, state byte, ppid, pgid int) *fstest.MapFile {
	// The fields after pgrp, as a real stat carries them.
	return &fstest.MapFile{Data: []byte(fmt.Sprintf("%d (%s) %c %d %d %d 0 -1 4194560 120 0 0 0 1 2 0 0 20 0 1 0 77 1234 56\n",
		pid, comm, state, ppid, pgid, pgid))}
}

// The tree InterruptProcessTree signals is read with one read of each
// process's stat, however deep the tree under the process being interrupted.
//
// The walk it replaces read the whole of /proc again for each process it
// found in the tree, and read each one's group once more, all before the
// first signal: (depth + 1) x the processes on the host. Under memory pressure
// each read of a /proc file waits on reclaim, and the signal waited for all of
// them. Here the tree is the shape keploy runs a native app in -- sh -c, nyc,
// npm, sh -c, node -- with one descendant in a process group of its own, under
// a host's worth of other processes. A walk that reads the table again for
// each process it finds opens six times as many stat files.
func TestTheTreeToInterruptReadsEachProcessOnce(t *testing.T) {
	proc := fstest.MapFS{
		// Not processes: skipped without a read.
		"self/stat":   statLine(1, "init", 'S', 0, 1),
		"uptime":      &fstest.MapFile{Data: []byte("1.0 2.0\n")},
		"sys/kernel":  &fstest.MapFile{Data: []byte("x")},
		"1/stat":      statLine(1, "systemd", 'S', 0, 1),
		"2/stat":      statLine(2, "kthreadd", 'S', 0, 0),
		"500/stat":    statLine(500, "keploy", 'S', 1, 500),
		"600/stat":    statLine(600, "sh", 'S', 500, 600), // the app's leader, Setpgid
		"601/stat":    statLine(601, "nyc", 'S', 600, 600),
		"602/stat":    statLine(602, "npm start", 'S', 601, 600),
		"603/stat":    statLine(603, "sh", 'S', 602, 600),
		"604/stat":    statLine(604, "node (app) )x(", 'S', 603, 600),
		"605/stat":    statLine(605, "worker", 'Z', 604, 605), // a zombie in a group of its own
		"700/stat":    statLine(700, "unrelated", 'S', 1, 700),
		"701/status":  &fstest.MapFile{Data: []byte("Name:\tgone\n")}, // exited before its stat was read
		"702/stat":    &fstest.MapFile{Data: []byte("702 (torn")},     // cannot be parsed
		"703/stat":    statLine(703, "other child of keploy", 'S', 500, 500),
		"1000/stat":   statLine(1000, "postgres", 'S', 1, 1000),
		"1001/stat":   statLine(1001, "postgres", 'S', 1000, 1000),
		"1002/stat":   statLine(1002, "postgres", 'S', 1000, 1000),
		"1003/stat":   statLine(1003, "dockerd", 'S', 1, 1003),
		"1004/stat":   statLine(1004, "containerd", 'S', 1, 1004),
		"1005/stat":   statLine(1005, "sshd", 'S', 1, 1005),
		"1006/stat":   statLine(1006, "bash", 'S', 1005, 1006),
		"1007/stat":   statLine(1007, "go", 'R', 1006, 1007),
		"1008/stat":   statLine(1008, "compile", 'R', 1007, 1007),
		"1009/stat":   statLine(1009, "compile", 'R', 1007, 1007),
		"1010/stat":   statLine(1010, "kworker/0:1-events", 'I', 2, 0),
		"1011/stat":   statLine(1011, "kworker/1:2", 'I', 2, 0),
		"1012/stat":   statLine(1012, "journald", 'S', 1, 1012),
		"1013/stat":   statLine(1013, "cron", 'S', 1, 1013),
		"1014/stat":   statLine(1014, "a b c", 'S', 1, 1014),
		"1015/stat":   statLine(1015, "((", 'S', 1, 1015),
		"1016/stat":   statLine(1016, ")", 'S', 1, 1016),
		"1017/stat":   statLine(1017, "", 'S', 1, 1017),
		"1018/stat":   statLine(1018, "tail", 'S', 1006, 1018),
		"1019/stat":   statLine(1019, "less", 'S', 1006, 1019),
		"1020/stat":   statLine(1020, "vim", 'T', 1006, 1020),
		"1021/stat":   statLine(1021, "top", 'S', 1006, 1021),
		"1022/stat":   statLine(1022, "make", 'S', 1006, 1022),
		"1023/stat":   statLine(1023, "cc", 'S', 1022, 1022),
		"1024/stat":   statLine(1024, "ld", 'S', 1022, 1022),
		"1025/stat":   statLine(1025, "as", 'S', 1022, 1022),
		"1026/stat":   statLine(1026, "dbus-daemon", 'S', 1, 1026),
		"1027/stat":   statLine(1027, "polkitd", 'S', 1, 1027),
		"1028/stat":   statLine(1028, "chronyd", 'S', 1, 1028),
		"1029/stat":   statLine(1029, "agetty", 'S', 1, 1029),
		"1030/stat":   statLine(1030, "rsyslogd", 'S', 1, 1030),
		"1031/status": &fstest.MapFile{Data: []byte("Name:\tgone too\n")},
	}
	processes := 0
	for name := range proc {
		dir, _, _ := strings.Cut(name, "/")
		if _, err := strconv.Atoi(dir); err == nil && strings.Contains(name, "/") {
			processes++
		}
	}
	counted := &countingFS{FS: proc}

	// What InterruptProcessTree reads before its first signal: the tree, and
	// the group of each process in it and of the process itself.
	children, groupOf, err := childPIDsIn(counted, 600)
	if err != nil {
		t.Fatalf("childPIDsIn: %v", err)
	}
	groups, err := uniqueProcessGroups(append(append([]int(nil), children...), 600), groupOf)
	if err != nil {
		t.Fatalf("uniqueProcessGroups: %v", err)
	}
	if got := counted.stats.Load(); got != int64(processes) {
		t.Fatalf("read %d stat files for %d processes before the first signal; want each read once", got, processes)
	}

	sort.Ints(children)
	if fmt.Sprint(children) != "[601 602 603 604 605]" {
		t.Fatalf("the tree under 600 = %v; want [601 602 603 604 605]", children)
	}
	sort.Ints(groups)
	if fmt.Sprint(groups) != "[600 605]" {
		t.Fatalf("the tree's process groups = %v; want [600 605]", groups)
	}
	for pid, want := range map[int]int{600: 600, 601: 600, 604: 600, 605: 605, 500: 500, 703: 500} {
		if got, err := groupOf(pid); err != nil || got != want {
			t.Fatalf("group of %d = %d, %v; want %d", pid, got, err, want)
		}
	}
	for _, pid := range []int{701, 702, 1031} {
		if _, err := groupOf(pid); err == nil {
			t.Fatalf("process %d has no stat that can be read, yet the table holds a group for it", pid)
		}
	}
}

// The table read from this host's /proc holds this process, its parent and
// its group as the kernel reports them.
func TestReadProcTableReadsThisHostsProcfs(t *testing.T) {
	table, err := readProcTable(os.DirFS("/proc"))
	if err != nil {
		t.Fatalf("readProcTable: %v", err)
	}
	if got := table.parents[os.Getpid()]; got != os.Getppid() {
		t.Fatalf("parent of this process = %d; want %d", got, os.Getppid())
	}
	pgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got := table.groups[os.Getpid()]; got != pgid {
		t.Fatalf("group of this process = %d; want %d", got, pgid)
	}
}

// noNestedPidNamespace skips a test that needs a process in a nested pid
// namespace where this host cannot start one, and fails it where
// KEPLOY_TEST_USERNS_REQUIRED=1: go-test.yaml's lane lets unprivileged users
// create namespaces and sets it, so the test cannot pass there by skipping.
func noNestedPidNamespace(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("KEPLOY_TEST_USERNS_REQUIRED") == "1" {
		t.Fatalf("KEPLOY_TEST_USERNS_REQUIRED=1, but "+format, args...)
	}
	t.Skipf(format, args...)
}

// A descendant in a nested pid namespace -- a sandboxed browser under a test
// runner, or anything started under `unshare --pid` -- is grouped by its group
// as seen from here, the one kill(2) takes.
//
// The walk read groups from status's NSpgid line, which holds one group per
// namespace the process is in ("NSpgid:\t<here>\t0"); it read any line with
// more than one as group -1, and signalling the group -(-1) is kill(1, SIGINT).
// Run as root, as `sudo -E keploy` runs it, the interrupt sent SIGINT to init,
// which on a systemd host starts ctrl-alt-del.target, a reboot by default; run
// as a user it was refused, and logged at ERROR.
func TestTheTreeToInterruptGroupsADescendantInANestedPidNamespace(t *testing.T) {
	cmd := exec.Command("unshare", "--user", "--map-root-user", "--pid", "--fork", "sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // as keploy starts a native command
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		noNestedPidNamespace(t, "cannot start a process in a nested pid namespace here: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-exited })

	// The forked child is pid 1 of the new namespace, in unshare's group.
	var child int
	deadline := time.Now().Add(8 * time.Second)
	for child == 0 {
		select {
		case <-exited:
			noNestedPidNamespace(t, "unshare could not create a user and pid namespace here: %s", strings.TrimSpace(stderr.String()))
		default:
		}
		if children, _, err := findChildPIDs(cmd.Process.Pid); err == nil && len(children) > 0 {
			child = children[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unshare's child never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(child) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(status), "NSpgid:\t"+strconv.Itoa(cmd.Process.Pid)+"\t") {
		noNestedPidNamespace(t, "the child is not in a nested pid namespace here: %q", status)
	}

	children, groupOf, err := findChildPIDs(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("findChildPIDs: %v", err)
	}
	groups, err := uniqueProcessGroups(append(children, cmd.Process.Pid), groupOf)
	if err != nil {
		t.Fatalf("uniqueProcessGroups: %v", err)
	}
	if fmt.Sprint(groups) != fmt.Sprint([]int{cmd.Process.Pid}) {
		t.Fatalf("the groups to signal are %v; want only unshare's group %d", groups, cmd.Process.Pid)
	}
}
