//go:build darwin

package agent

import (
	"time"

	"golang.org/x/sys/unix"
)

// getProc returns the parent PID and start time for pid via sysctl, or false
// if the process does not exist. It uses golang.org/x/sys/unix's typed struct
// so the field layout is correct on every macOS architecture.
func getProc(pid int) (ppid int, startTime time.Time, alive bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || kp.Proc.P_pid == 0 {
		return 0, time.Time{}, false
	}
	tv := kp.Proc.P_starttime
	return int(kp.Eproc.Ppid), time.Unix(int64(tv.Sec), int64(tv.Usec)*1000), true
}

// startTimeKey packs a start time into a uint64 for comparison, the same way
// the Linux claimer uses clock ticks: we use seconds * 1_000_000 + microseconds.
func startTimeKey(t time.Time) uint64 {
	return uint64(t.Unix())*1_000_000 + uint64(t.Nanosecond()/1000)
}

// procAlive reports whether process pid is the one that started at start and
// has not exited.
func procAlive(pid int, start uint64) bool {
	_, t, ok := getProc(pid)
	if !ok {
		return false
	}
	return startTimeKey(t) == start
}

// procStart is process pid's start time, false when it does not exist.
func procStart(pid int) (uint64, bool) {
	_, t, ok := getProc(pid)
	if !ok {
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
		ppid, _, ok := getProc(pid)
		if !ok {
			return false
		}
		pid = ppid
	}
	return pid == ancestor
}
