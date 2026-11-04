package tls

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// openStoreLock opens caRunsDir's store lock through a handle of its own, as a
// waiting change has it open.
func openStoreLock(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join(caRunsDir, storeLockName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// useLocksTable makes storeLockHolder read lines in place of /proc/locks for
// the rest of the test, and returns the file it reads.
func useLocksTable(t *testing.T, lines ...string) (table string) {
	t.Helper()
	table = filepath.Join(t.TempDir(), "locks")
	if err := os.WriteFile(table, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := procLocks
	procLocks = table
	t.Cleanup(func() { procLocks = old })
	return table
}

// pidsOf is the pids (the 5th field) of the lock lines out holds that hold a
// lock, and of those that wait for one ("->" lines, their 6th).
func pidsOf(out string) (holders, waiters []string) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch f := strings.Fields(line); {
		case len(f) >= 6 && f[1] == "->":
			waiters = append(waiters, f[5])
		case len(f) >= 5:
			holders = append(holders, f[4])
		}
	}
	return holders, waiters
}

// The holder of the store lock, as the kernel's table of locks has it, is
// named with its pid, command and state: here this process, which holds the
// lock through another handle, as another run holds it, and is not stopped
// (R, S or D, not T).
func TestStoreLockHolderIsNamed(t *testing.T) {
	useRunsDir(t)
	holdStoreLockElsewhere(t)
	comm, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatal(err)
	}
	want := "process " + strconv.Itoa(os.Getpid()) + " (" + strings.TrimSpace(string(comm)) + ", state "
	got := storeLockHolder(openStoreLock(t))
	if state, ok := strings.CutPrefix(got, want); !ok || (state != "R)" && state != "S)" && state != "D)") {
		t.Fatalf("the holder is %q, want %q and its state, R, S or D", got, want+"...")
	}
}

// A holder's command and state come from its /proc/<pid>/stat line: the
// command is what is between the first "(" and the last ")", which may hold
// spaces, parentheses and a state's letter, and the state is the field after
// it. A line without those parts gives neither.
func TestStoreLockHolderStateIsParsed(t *testing.T) {
	if got, want := statNameAndState("4242 (a) T (b) T 1 4242 4242 0 -1 4194560"), " (a) T (b, state T)"; got != want {
		t.Fatalf("statNameAndState gave %q, want %q", got, want)
	}
	if got, want := statNameAndState("17 (flock) S 1 17"), " (flock, state S)"; got != want {
		t.Fatalf("statNameAndState gave %q, want %q", got, want)
	}
	for _, line := range []string{"", "4242", "4242 (a", "4242 a) S", "4242 (a)"} {
		if got := statNameAndState(line); got != "" {
			t.Fatalf("statNameAndState(%q) gave %q, want nothing", line, got)
		}
	}
}

// The store lock's file is named by its mount's device, as the table of
// mounts gives it in decimal, in hex (as /proc/locks prints it), with its
// inode: not by fstat's device, which on btrfs, and on an overlayfs whose
// layers are not all on one filesystem (and without xino), is another.
// Here the table gives the file's mount 259:1048575, which no file's fstat
// gives. With its mount not in the table, it takes fstat's device.
func TestStoreLockFileIDIsTheMounts(t *testing.T) {
	useRunsDir(t)
	holdStoreLockElsewhere(t)
	f := openStoreLock(t)
	info, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		t.Fatal(err)
	}
	mnt := ""
	for _, line := range strings.Split(string(info), "\n") {
		if v, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			mnt = strings.TrimSpace(v)
		}
	}
	if mnt == "" {
		t.Fatalf("no mnt_id in the store lock's fdinfo: %q", info)
	}
	var st os.FileInfo
	if st, err = f.Stat(); err != nil {
		t.Fatal(err)
	}
	ino := strconv.FormatUint(uint64(st.Sys().(*syscall.Stat_t).Ino), 10) // uint32 on some ports
	table := filepath.Join(t.TempDir(), "mountinfo")
	lines := "1 0 259:1048574 / / rw - ext4 /dev/sda1 rw\n" + mnt + " 1 259:1048575 / /run rw - tmpfs tmpfs rw\n"
	if err := os.WriteFile(table, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	old := procMountinfo
	procMountinfo = table
	t.Cleanup(func() { procMountinfo = old })
	if id, err := storeLockFileID(f); err != nil || id != "103:fffff:"+ino {
		t.Fatalf("the store lock's id is %q (%v), want 103:fffff:%s", id, err, ino)
	}

	// A table of mounts without the file's mount: fstat's device.
	if err := os.WriteFile(table, []byte("1 0 259:1048574 / / rw - ext4 /dev/sda1 rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dev := uint64(st.Sys().(*syscall.Stat_t).Dev) // uint32 on some ports
	want := fmt.Sprintf("%02x:%02x:%s", unix.Major(dev), unix.Minor(dev), ino)
	if id, err := storeLockFileID(f); err != nil || id != want {
		t.Fatalf("with its mount not in the table, the store lock's id is %q (%v), want fstat's %s", id, err, want)
	}
}

// The holder is the one of this very file: of the table's lines it takes the
// one whose MAJOR:MINOR:INODE is the file's, not another device's file with
// that inode, nor one whose major ends with the file's, nor an inode that
// only starts or ends with its digits, nor a process that waits for the lock
// (a "->" line), nor an open file description lock, which has no pid (-1),
// nor a pid that is not over 0, nor a POSIX lock's holder (only an flock
// blocks an flock); the last four here before the holder's. With no such
// line (a holder outside this /proc's pid namespace) it names no process,
// and says how to find it on the host. That command prints the
// holder's line and the waiter's of such a table, and no decoy's; run on
// this host's /proc/locks while another handle holds the lock, it prints the
// holder's line, with its pid.
func TestStoreLockHolderOfThisFile(t *testing.T) {
	useRunsDir(t)
	holdStoreLockElsewhere(t)
	f := openStoreLock(t)
	id, err := storeLockFileID(f)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.LastIndexByte(id, ':')
	dev, ino := id[:i], id[i+1:]
	decoys := []string{
		"1: FLOCK  ADVISORY  WRITE 777 ff:fe:" + ino + " 0 EOF",
		"2: FLOCK  ADVISORY  WRITE 778 " + dev + ":" + ino + "1 0 EOF",
		"3: FLOCK  ADVISORY  WRITE 779 " + dev + ":1" + ino + " 0 EOF",
		"4: FLOCK  ADVISORY  WRITE 781 1" + id + " 0 EOF",
		"5: OFDLCK ADVISORY  WRITE -1 " + id + " 0 EOF",
		"6: -> FLOCK  ADVISORY  WRITE 780 " + id + " 0 EOF",
		"7: FLOCK  ADVISORY  WRITE 0 " + id + " 0 EOF",
		"8: POSIX  ADVISORY  WRITE 782 " + id + " 0 EOF",
	}
	table := useLocksTable(t, append(decoys, "9: FLOCK  ADVISORY  WRITE 4242 "+id+" 0 EOF")...)
	if got := storeLockHolder(f); got != "process 4242" && !strings.HasPrefix(got, "process 4242 (") {
		t.Fatalf("with the holder's line among decoys, the holder is %q, want process 4242", got)
	}
	grepTable := strings.Replace(storeLockHolderGrep(id), "/proc/locks", table, 1)
	out, err := exec.Command("sh", "-c", grepTable).Output()
	if err != nil {
		t.Fatalf("%s: %v", grepTable, err)
	}
	if holders, waiters := pidsOf(string(out)); strings.Join(holders, " ") != "-1 0 782 4242" || strings.Join(waiters, " ") != "780" {
		t.Fatalf("%s printed %q: want the lines of this file only (the OFD, pid 0, POSIX and holder's lines, and the waiter's)", grepTable, out)
	}

	useLocksTable(t, decoys...)
	got := storeLockHolder(f)
	grep := storeLockHolderGrep(id)
	if !strings.HasPrefix(got, storeLockHolderUnknown) || !strings.Contains(got, "`"+grep+"`") {
		t.Fatalf("with no holder's line, the holder is %q, want %q and how to find it (%s)", got, storeLockHolderUnknown, grep)
	}
	out, err = exec.Command("sh", "-c", grep).Output()
	if err != nil {
		t.Fatalf("%s: %v", grep, err)
	}
	if holders, _ := pidsOf(string(out)); strings.Join(holders, " ") != strconv.Itoa(os.Getpid()) {
		t.Fatalf("%s printed %q: want the one line of its holder, pid %d", grep, out, os.Getpid())
	}
}

// A native install gives its changes one bound to wait out for the store lock
// (withStoreLockBudget): with another holder keeping the lock, the error it
// fails with says the bound is the install's. (With no JDK here, its store
// update is its one change that takes the lock:
// TestStoreLockTriesOnceAfterTheInstallsBound shows how the budget's changes
// share it.)
func TestNativeInstallGivesItsChangesOneBudget(t *testing.T) {
	useRunsDir(t)
	setStoreLockTimes(t, 200*time.Millisecond, time.Hour)
	recordStoreLockSteps(t)
	holdStoreLockElsewhere(t)
	store, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "keploy-test-update-ca"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing else on PATH: no JDK keystore is touched.
	t.Setenv("PATH", bin)
	t.Setenv("JAVA_HOME", "")
	oldPaths, oldCmds := caStorePath, caStoreUpdateCmd
	caStorePath, caStoreUpdateCmd = []string{store + "/"}, []string{"keploy-test-update-ca"}
	t.Cleanup(func() {
		caStorePath, caStoreUpdateCmd = oldPaths, oldCmds
		recordNativeInstall(nil)
	})
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { TeardownNativeCA(zap.NewNop()) })
	err = setupNativeForApp(installCtx(t), zap.NewNop(), ca, nil, 0, "")
	if !errors.Is(err, ErrStoreLockHeld) || !strings.Contains(err.Error(), "in one CA install") {
		t.Fatalf("the install ended with %v, want ErrStoreLockHeld, its bound the install's", err)
	}
}
