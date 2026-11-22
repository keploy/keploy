//go:build linux

package starts

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/utils"
)

// sysProc reads processes from a /proc: root, or this process's ("" or
// "/proc").
type sysProc struct{ root string }

// proc is the /proc p reads.
func (p sysProc) proc() string {
	if p.root == "" {
		return "/proc"
	}
	return p.root
}

// file is process pid's entry name in p's /proc.
func (p sysProc) file(pid uint32, name string) string {
	return filepath.Join(p.proc(), strconv.FormatUint(uint64(pid), 10), name)
}

// KernelPidsLocal reports whether a kernel pid names in p's /proc the
// process it names in the kernel (utils.KernelPidsLocalIn): only then does
// the registry name a process its proxy reports by one (KernelPids). This
// process's /proc is read once (ownPidsLocal).
func (p sysProc) KernelPidsLocal() bool {
	if p.proc() == "/proc" {
		return ownPidsLocal()
	}
	return utils.KernelPidsLocalIn(p.proc())
}

// ownPidsLocal is utils.KernelPidsAreLocal: this process's /proc, read once.
var ownPidsLocal = utils.KernelPidsAreLocal

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

func (p sysProc) Parent(pid uint32) (uint32, bool) {
	st, ok := utils.ReadProcStatIn(p.proc(), int(pid))
	if !ok {
		return 0, false
	}
	return uint32(st.PPID), true
}

func (p sysProc) Birth(pid uint32) (time.Time, bool) {
	st, ok := utils.ReadProcStatIn(p.proc(), int(pid))
	if !ok || boot().IsZero() {
		return time.Time{}, false
	}
	return boot().Add(time.Duration(st.StartTime) * time.Second / ticks), true
}

func (p sysProc) Program(pid uint32) string {
	if exe, err := os.Readlink(p.file(pid, "exe")); err == nil {
		return strings.TrimSuffix(exe, " (deleted)")
	}
	b, err := os.ReadFile(p.file(pid, "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Command reads process pid's comm (/proc/<pid>/comm) and command line
// (/proc/<pid>/cmdline, NUL-separated).
func (p sysProc) Command(pid uint32) (comm string, argv []string, ok bool) {
	c, cerr := os.ReadFile(p.file(pid, "comm"))
	if cerr == nil {
		comm = strings.TrimSpace(string(c))
	}
	if b, err := os.ReadFile(p.file(pid, "cmdline")); err == nil {
		argv = strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(argv) == 1 && argv[0] == "" {
			argv = nil
		}
	}
	return comm, argv, cerr == nil || len(argv) > 0
}

var Default = New(sysProc{}, 20*time.Millisecond)
