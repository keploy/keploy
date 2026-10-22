//go:build linux

package starts

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/utils"
)

type sysProc struct{}

var (
	bootOnce sync.Once
	bootTime time.Time
)

const ticks = 100

func boot() time.Time {
	bootOnce.Do(func() {
		if b, err := os.ReadFile("/proc/uptime"); err == nil {
			if f := strings.Fields(string(b)); len(f) > 0 {
				if up, err := strconv.ParseFloat(f[0], 64); err == nil {
					bootTime = time.Now().Add(-time.Duration(up * float64(time.Second)))
					return
				}
			}
		}
		f, err := os.Open("/proc/stat")
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					bootTime = time.Unix(n, 0)
				}
				return
			}
		}
	})
	return bootTime
}

func (sysProc) Parent(pid uint32) (uint32, bool) {
	st, ok := utils.ReadProcStat(int(pid))
	if !ok {
		return 0, false
	}
	return uint32(st.PPID), true
}

func (sysProc) Birth(pid uint32) (time.Time, bool) {
	st, ok := utils.ReadProcStat(int(pid))
	if !ok || boot().IsZero() {
		return time.Time{}, false
	}
	return boot().Add(time.Duration(st.StartTime) * time.Second / ticks), true
}

func (sysProc) Program(pid uint32) string {
	if p, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return strings.TrimSuffix(p, " (deleted)")
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Command reads process pid's comm (/proc/<pid>/comm) and command line
// (/proc/<pid>/cmdline, NUL-separated).
func (sysProc) Command(pid uint32) (comm string, argv []string, ok bool) {
	c, cerr := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if cerr == nil {
		comm = strings.TrimSpace(string(c))
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		argv = strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(argv) == 1 && argv[0] == "" {
			argv = nil
		}
	}
	return comm, argv, cerr == nil || len(argv) > 0
}

var Default = New(sysProc{}, 20*time.Millisecond)
