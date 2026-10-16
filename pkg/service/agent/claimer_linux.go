//go:build linux

package agent

import (
	"go.keploy.io/server/v3/utils"
)

// procAlive reports whether process pid is the one that started at start
// (clock ticks since boot, /proc/<pid>/stat field 22) and has not exited: a
// zombie has exited, and a reused pid has another start time.
func procAlive(pid int, start uint64) bool {
	st, ok := utils.ReadProcStat(pid)
	return ok && st.StartTime == start && st.State != 'Z' && st.State != 'X'
}

// procStart is process pid's start time, false when it has exited.
func procStart(pid int) (uint64, bool) {
	st, ok := utils.ReadProcStat(pid)
	if !ok || st.State == 'Z' || st.State == 'X' {
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
