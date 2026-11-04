//go:build linux

package tls

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// procLocks is the kernel's table of the file locks held on this host, with
// pids as the pid namespace of this /proc's mount sees them; procMountinfo is
// this process's table of mounts. Variables so tests can give their own.
var (
	procLocks     = "/proc/locks"
	procMountinfo = "/proc/self/mountinfo"
)

// storeLockHolder says who holds the lock on f's file, as the kernel's table
// of locks has it: the holder's pid, command and state. The table names a
// lock's file by MAJOR:MINOR:INODE (storeLockFileID), so this is the holder
// of this very file, not of another file at the same path in another mount
// namespace. When the table names no holder of it (the holder is outside this
// /proc's pid namespace, whose table leaves its line out, or it let go after
// the last try), it names no process and says how to find it on the host.
func storeLockHolder(f *os.File) string {
	id, err := storeLockFileID(f)
	if err != nil {
		return storeLockHolderUnknown + " (the lock file's device and inode, which name it in /proc/locks, could not be read: " + err.Error() + ")"
	}
	pid := lockHolderPid(id)
	if pid == "" {
		return storeLockHolderUnknown + " (no process this agent can see: on the host, `" + storeLockHolderGrep(id) +
			"` prints the lock's lines: the holder's is the FLOCK line without ->, and its field before " + id + " is the holder's pid)"
	}
	return "process " + pid + processNameAndState(pid)
}

// storeLockFileID is how the kernel's table of locks names f's file:
// MAJOR:MINOR in hex, then the inode in decimal. The table prints the device
// of the file's superblock, which is the one its mount has in procMountinfo
// (in decimal there), not always fstat's: on btrfs fstat gives a subvolume's
// own device, and on an overlayfs whose layers are not all on one filesystem
// (and without xino) a layer's. Only when f's mount is not found there does it
// take fstat's.
func storeLockFileID(f *os.File) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "", err
	}
	dev := uint64(st.Dev) // uint32 on some 32-bit ports
	major, minor := unix.Major(dev), unix.Minor(dev)
	if mountMajor, mountMinor, ok := mountDevice(f); ok {
		major, minor = mountMajor, mountMinor
	}
	return fmt.Sprintf("%02x:%02x:%d", major, minor, st.Ino), nil
}

// mountDevice is the MAJOR:MINOR procMountinfo gives the mount f is on,
// found by the mnt_id of f's /proc/self/fdinfo.
func mountDevice(f *os.File) (major, minor uint32, ok bool) {
	info, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		return 0, 0, false
	}
	var mnt string
	for _, line := range strings.Split(string(info), "\n") {
		if v, found := strings.CutPrefix(line, "mnt_id:"); found {
			mnt = strings.TrimSpace(v)
			break
		}
	}
	if mnt == "" {
		return 0, 0, false
	}
	mounts, err := os.Open(procMountinfo)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = mounts.Close() }()
	// "36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw"
	s := bufio.NewScanner(mounts)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 3 || fields[0] != mnt {
			continue
		}
		majorS, minorS, found := strings.Cut(fields[2], ":")
		a, errA := strconv.ParseUint(majorS, 10, 32)
		b, errB := strconv.ParseUint(minorS, 10, 32)
		if !found || errA != nil || errB != nil {
			return 0, 0, false
		}
		return uint32(a), uint32(b), true
	}
	return 0, 0, false
}

// storeLockHolderGrep is the shell command that prints the lines of the
// host's table of locks for the file id names: the id is a field of its own,
// a space on each side.
func storeLockHolderGrep(id string) string {
	return "grep ' " + id + " ' /proc/locks"
}

// lockHolderPid is the pid procLocks gives for the flock held on the file id
// names, or "" when it has none. A holder's line has the lock's type as its
// 2nd field (FLOCK: on a local filesystem only an flock blocks an flock), the
// pid as its 5th and the file as its 6th. A waiter's line has "->" as its 2nd
// field, so it is no FLOCK line. The kernel leaves out the line of a holder
// outside this /proc's pid namespace; a pid that is not over 0 is no
// holder's either.
func lockHolderPid(id string) string {
	table, err := os.Open(procLocks)
	if err != nil {
		return ""
	}
	defer func() { _ = table.Close() }()
	// "1: FLOCK  ADVISORY  WRITE 1234 00:1d:2892 0 EOF"
	s := bufio.NewScanner(table)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 6 || f[1] != "FLOCK" || f[5] != id {
			continue
		}
		if n, err := strconv.Atoi(f[4]); err == nil && n > 0 {
			return f[4]
		}
	}
	return ""
}

// processNameAndState is " (command, state S)" of the process pid, from its
// /proc/<pid>/stat (statNameAndState), or "" when that cannot be read.
func processNameAndState(pid string) string {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return ""
	}
	return statNameAndState(string(b))
}

// statNameAndState is " (command, state S)" of a /proc/<pid>/stat line,
// "1234 (the command) S ...", or "" when it has no such parts. The command is
// between the first "(" and the last ")": it may hold spaces and parentheses.
func statNameAndState(stat string) string {
	open, closing := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || closing < open {
		return ""
	}
	rest := strings.Fields(stat[closing+1:])
	if len(rest) == 0 {
		return ""
	}
	return " (" + stat[open+1:closing] + ", state " + rest[0] + ")"
}
