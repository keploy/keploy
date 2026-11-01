//go:build darwin

package proxy

import (
	"bytes"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ProgramOf is the program process pid runs: its executable, as the kernel
// has its vnode's path (libproc's proc_pidpath, what Linux's
// /proc/<pid>/exe gives), else its command name (kinfo_proc's p_comm, what
// Linux's /proc/<pid>/comm gives); "" when neither can be read. A LIVE row
// and a mocked-local row name a listener's program by it
// (models.LiveRow.Exe, models.MockedRow.Exe), so a judge that lists a
// program off the run's clock names it by ProgramOf too, and the two match.
func ProgramOf(pid int) string {
	if pid <= 0 {
		return ""
	}
	if p := pidPath(pid); p != "" {
		return p
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
		return ""
	}
	c := kp.Proc.P_comm[:]
	if i := bytes.IndexByte(c, 0); i >= 0 {
		c = c[:i]
	}
	return string(c)
}

// The proc_info system call's PROC_PIDPATHINFO (libproc's proc_pidpath),
// from <sys/proc_info.h>.
const (
	procInfoCallPIDInfo = 2    // PROC_INFO_CALL_PIDINFO
	procPIDPathInfo     = 11   // PROC_PIDPATHINFO
	procPIDPathMax      = 4096 // PROC_PIDPATHINFO_MAXSIZE: 4 * MAXPATHLEN
)

// pidPath is the path of process pid's executable, "" when the kernel has
// none for it (it has exited, or it is not the caller's to read).
func pidPath(pid int) string {
	buf := make([]byte, procPIDPathMax)
	// The call returns 0, not the path's length (libproc's proc_pidpath
	// takes the length with strlen): the path is NUL-terminated in buf.
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return ""
	}
	i := bytes.IndexByte(buf, 0)
	if i <= 0 {
		return ""
	}
	return string(buf[:i])
}
