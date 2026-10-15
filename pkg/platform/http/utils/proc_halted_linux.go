//go:build linux

package utils

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// ProcessHalted says why process pid, which has not exited, runs nothing more
// of its own until it does, and "" while it may still run (or is gone, or its
// status cannot be read):
//
//   - killed: SIGKILL is pending for it, or its main thread has exited and
//     waits, a zombie, for the last of its other threads. The kernel holds
//     one of its threads in a system call that a fatal signal does not end
//     (an uninterruptible sleep), and it exits as soon as that call ends. A
//     zombie with no thread left has exited, and is only not reaped yet: not
//     halted, its parent reaps it now.
//   - stopped: stopped by a signal or a tracer. It goes on with what it was
//     doing once it is continued.
//   - frozen: its cgroup is frozen (the cgroup freezer, v2 or v1). It goes on
//     once the cgroup is thawed.
func ProcessHalted(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return ""
	}
	if why := statusHalted(b); why != "" {
		return why
	}
	if cg, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup"); err == nil && cgroupFrozen(cg, "/sys/fs/cgroup") {
		return "its cgroup is frozen: it finishes its shutdown, and exits, once the cgroup is thawed"
	}
	return ""
}

// cgroupFrozen reports whether the cgroup a process's /proc/<pid>/cgroup
// names (cg) is frozen, read under the cgroup filesystem at root: cgroup v2's
// cgroup.events "frozen 1", or the v1 freezer's freezer.state FROZEN.
func cgroupFrozen(cg []byte, root string) bool {
	for _, line := range bytes.Split(cg, []byte("\n")) {
		f := bytes.SplitN(line, []byte(":"), 3)
		if len(f) != 3 {
			continue
		}
		path := string(f[2])
		switch {
		case string(f[0]) == "0" && len(f[1]) == 0: // v2
			b, err := os.ReadFile(root + path + "/cgroup.events")
			if err != nil {
				continue
			}
			for _, ev := range bytes.Split(b, []byte("\n")) {
				if string(bytes.TrimSpace(ev)) == "frozen 1" {
					return true
				}
			}
		case bytes.Contains(f[1], []byte("freezer")): // v1
			b, err := os.ReadFile(root + "/freezer" + path + "/freezer.state")
			if err == nil && string(bytes.TrimSpace(b)) == "FROZEN" {
				return true
			}
		}
	}
	return false
}

// statusHalted is ProcessHalted on the contents of a /proc/<pid>/status file.
func statusHalted(status []byte) string {
	const sigkill = uint64(1) << (uint(syscall.SIGKILL) - 1)
	var state byte
	killed := false
	threads := 0
	for _, line := range bytes.Split(status, []byte("\n")) {
		k, v, found := bytes.Cut(line, []byte(":"))
		if !found {
			continue
		}
		v = bytes.TrimSpace(v)
		switch string(k) {
		case "State":
			if len(v) > 0 {
				state = v[0]
			}
		case "Threads":
			threads, _ = strconv.Atoi(string(v))
		case "SigPnd", "ShdPnd":
			if mask, err := strconv.ParseUint(string(v), 16, 64); err == nil && mask&sigkill != 0 {
				killed = true
			}
		}
	}
	switch {
	case (state == 'Z' || state == 'X') && threads <= 1:
		return "" // exited: reaped at once
	case killed || state == 'Z' || state == 'X':
		return "it has been killed, and the kernel still holds one of its threads in a system call: it exits once that call ends"
	case state == 'T' || state == 't':
		return "it is stopped: it finishes its shutdown, and exits, once it is continued"
	}
	return ""
}

// CommandUnderSudo returns the process id of the command sudo (process
// sudoPid) runs -- the first of its descendants that is not sudo itself (sudo
// may run its command under a monitor process of its own) -- or 0 when there
// is none (yet, or any more), or it cannot be told.
func CommandUnderSudo(sudoPid int) int {
	queue := []int{sudoPid}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range childProcesses(pid) {
			comm, err := os.ReadFile("/proc/" + strconv.Itoa(child) + "/comm")
			if err != nil {
				continue // exited meanwhile
			}
			if string(bytes.TrimSpace(comm)) != "sudo" {
				return child
			}
			queue = append(queue, child)
		}
	}
	return 0
}

// childProcesses returns the processes whose parent is pid: from its threads'
// children files, or, on a kernel without them, by every process's parent.
func childProcesses(pid int) []int {
	dir := "/proc/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var kids []int
	haveFiles := true
	for _, t := range tasks {
		b, err := os.ReadFile(dir + "/" + t.Name() + "/children")
		if err != nil {
			if os.IsNotExist(err) {
				haveFiles = false
				break
			}
			continue
		}
		for _, f := range bytes.Fields(b) {
			if n, err := strconv.Atoi(string(f)); err == nil {
				kids = append(kids, n)
			}
		}
	}
	if haveFiles {
		return kids
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	kids = kids[:0]
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		f := bytes.Fields(b[i+1:])
		if len(f) >= 2 {
			if ppid, err := strconv.Atoi(string(f[1])); err == nil && ppid == pid {
				kids = append(kids, n)
			}
		}
	}
	return kids
}
