//go:build linux

package agent

import (
	"go.keploy.io/server/v3/utils"
)

// statOf reads process pid's stat line; a test gives one the kernel shows
// only for an instant.
var statOf = utils.ReadProcStat

// procAlive reports whether process pid is the one that started at start
// (clock ticks since boot, /proc/<pid>/stat field 22) and has not exited
// (procStart): a reused pid has another start time.
func procAlive(pid int, start uint64) bool {
	got, ok := procStart(pid)
	return ok && got == start
}

// procStart is process pid's start time, false when it has exited: every
// thread of it, not its main thread alone (utils.Exited). A Go program that
// execs from a thread other than its main one, as a test launcher that
// claims and then execs the test binary does, reads Z from the moment its
// main thread exits until the exec takes that thread's place, and runs
// throughout.
func procStart(pid int) (uint64, bool) {
	st, ok := statOf(pid)
	if !ok || st.State == 'X' || utils.Exited(pid, st) {
		return 0, false
	}
	return st.StartTime, true
}

// procDescends reports whether pid is ancestor or one of its descendants.
func procDescends(pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == ancestor {
			return true
		}
		st, ok := utils.ReadProcStat(pid)
		if !ok {
			return false
		}
		pid = st.PPID
	}
	return pid == ancestor
}
