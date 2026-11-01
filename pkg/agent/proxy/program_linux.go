//go:build linux

package proxy

import (
	"fmt"
	"os"
	"strings"
)

// ProgramOf is the program process pid runs: its executable, else its
// command name; "" when neither can be read. A LIVE row and a mocked-local
// row name a listener's program by it (models.LiveRow.Exe,
// models.MockedRow.Exe), so a judge that lists a program off the run's clock
// names it by ProgramOf too, and the two match.
func ProgramOf(pid int) string {
	if p, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return strings.TrimSuffix(p, " (deleted)")
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
