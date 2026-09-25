//go:build linux

package utils

import (
	"os"
	"strconv"
	"strings"
	"testing"
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
