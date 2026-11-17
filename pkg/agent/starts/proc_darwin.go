//go:build darwin

package starts

import (
	"bytes"
	"time"

	"golang.org/x/sys/unix"
)

type sysProc struct{}

func (sysProc) Parent(pid uint32) (uint32, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil || kp == nil || kp.Proc.P_pid == 0 {
		return 0, false
	}
	return uint32(kp.Eproc.Ppid), true
}

func (sysProc) Birth(pid uint32) (time.Time, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil || kp == nil || kp.Proc.P_pid == 0 {
		return time.Time{}, false
	}
	tv := kp.Proc.P_starttime
	return time.Unix(int64(tv.Sec), int64(tv.Usec)*1000), true
}

func (sysProc) Program(pid uint32) string {
	b, err := unix.SysctlRaw("kern.procargs2", int(pid))
	if err != nil || len(b) < 5 {
		return ""
	}
	rest := b[4:]
	if i := bytes.IndexByte(rest, 0); i > 0 {
		return string(rest[:i])
	}
	return ""
}

// KernelPidsLocal is true: macOS knows no PID namespaces, and its sysctl
// names a process by the number keploy's proxy reports.
func (sysProc) KernelPidsLocal() bool { return true }

// Command reads process pid's comm (kinfo_proc's p_comm) and command line
// (kern.procargs2: argc, the executable's path and its padding, then argv,
// NUL-separated: procArgs).
func (sysProc) Command(pid uint32) (comm string, argv []string, ok bool) {
	if kp, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid)); err == nil && kp != nil && kp.Proc.P_pid != 0 {
		c := kp.Proc.P_comm[:]
		if i := bytes.IndexByte(c, 0); i >= 0 {
			c = c[:i]
		}
		comm = string(c)
	}
	if b, err := unix.SysctlRaw("kern.procargs2", int(pid)); err == nil {
		argv = procArgs(b)
	}
	return comm, argv, comm != "" || len(argv) > 0
}

var Default = New(sysProc{}, 2*time.Millisecond)
