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

func stat(pid uint32) ([]string, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, false
	}
	s := string(b)
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+2 >= len(s) {
		return nil, false
	}
	return strings.Fields(s[end+2:]), true
}

func (sysProc) Parent(pid uint32) (uint32, bool) {
	f, ok := stat(pid)
	if !ok || len(f) < 2 {
		return 0, false
	}
	p, err := strconv.ParseUint(f[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(p), true
}

func (sysProc) Birth(pid uint32) (time.Time, bool) {
	f, ok := stat(pid)
	if !ok || len(f) < 20 {
		return time.Time{}, false
	}
	t, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil || boot().IsZero() {
		return time.Time{}, false
	}
	return boot().Add(time.Duration(t) * time.Second / ticks), true
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

var Default = New(sysProc{}, 20*time.Millisecond)
