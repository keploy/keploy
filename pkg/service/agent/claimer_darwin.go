//go:build darwin

package agent

import (
	"time"

	"golang.org/x/sys/unix"
)

// sZomb is XNU's p_stat for a process that has exited and waits for its
// parent to reap it (bsd/sys/proc.h SZOMB).
const sZomb = 5

// getProc returns the parent PID, start time and p_stat for pid via sysctl,
// or false if no process has that pid. It uses golang.org/x/sys/unix's typed
// struct so the field layout is correct on every macOS architecture. A zombie
// is still found: its parent link stays meaningful until it is reaped.
func getProc(pid int) (ppid int, startTime time.Time, stat int8, found bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || kp.Proc.P_pid == 0 {
		return 0, time.Time{}, 0, false
	}
	tv := kp.Proc.P_starttime
	return int(kp.Eproc.Ppid), time.Unix(int64(tv.Sec), int64(tv.Usec)*1000), kp.Proc.P_stat, true
}

// startTimeKey packs a start time into a uint64 for comparison, the same way
// the Linux claimer uses clock ticks: we use seconds * 1_000_000 + microseconds.
func startTimeKey(t time.Time) uint64 {
	return uint64(t.Unix())*1_000_000 + uint64(t.Nanosecond()/1000)
}

// procAlive reports whether process pid is the one that started at start and
// has not exited: a zombie has exited, and a reused pid has another start time.
func procAlive(pid int, start uint64) bool {
	got, ok := procStart(pid)
	return ok && got == start
}

// procStart is process pid's start time, false when it does not exist or has
// exited (a zombie).
func procStart(pid int) (uint64, bool) {
	_, t, stat, ok := getProc(pid)
	if !ok || stat == sZomb {
		return 0, false
	}
	return startTimeKey(t), true
}

// procDescends reports whether pid is ancestor or one of its descendants.
func procDescends(pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == ancestor {
			return true
		}
		ppid, _, _, ok := getProc(pid)
		if !ok {
			return false
		}
		pid = ppid
	}
	return pid == ancestor
}
