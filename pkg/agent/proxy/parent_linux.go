//go:build linux

package proxy

import "go.keploy.io/server/v3/utils"

// parentOf is the parent of process pid, read from /proc/<pid>/stat
// (utils.ReadProcStat). It is false when the line cannot be read.
func parentOf(pid uint32) (uint32, bool) {
	st, ok := utils.ReadProcStat(int(pid))
	if !ok {
		return 0, false
	}
	return uint32(st.PPID), true
}
