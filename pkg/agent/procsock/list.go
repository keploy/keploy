package procsock

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Proc is one process as proc_info's short BSD record shows it: its parent,
// its process group and whether it has exited and waits to be reaped.
type Proc struct {
	PID, PPID, PGID int
	// Zombie is a process that has exited and waits for its parent to
	// reap it (XNU's SZOMB).
	Zombie bool
}

// How proc_info lists processes and lays out their short records, from
// <sys/proc_info.h> and bsd/sys/proc.h. They are read on macOS only; the
// reading itself is here, apart from the system call, so that it is checked
// on every platform.
const (
	shortBSDInfoSize   = 64 // sizeof(struct proc_bsdshortinfo)
	offShortPPID       = 4  // pbsi_ppid
	offShortPGID       = 8  // pbsi_pgid
	offShortStatus     = 12 // pbsi_status
	sZomb              = 5  // SZOMB
	pidSize            = 4  // a pid_t in the list
	maxListPIDsRetries = 4
	listPIDsHeadroom   = 64
)

// readPIDs lists processes through list, one of proc_info's process lists
// (PROC_INFO_CALL_LISTPIDS): given no buffer it answers the list's size in
// bytes, given one it fills it and answers how many bytes it wrote. A list
// that fills the whole buffer may have been cut short, so it is read again
// into one twice that size, a few times at most. Only pids above 0 are
// processes keploy can read or signal: pid 0 is the kernel.
func readPIDs(list func(buf []byte) (int, error)) ([]int, error) {
	size, err := list(nil)
	if err != nil {
		return nil, err
	}
	for try := 0; try < maxListPIDsRetries; try++ {
		buf := make([]byte, size+listPIDsHeadroom*pidSize)
		n, err := list(buf)
		if err != nil {
			return nil, err
		}
		if n >= len(buf) {
			// The table may have grown past the buffer: list it again.
			size = n * 2
			continue
		}
		var pids []int
		for off := 0; off+pidSize <= n; off += pidSize {
			if pid := int(int32(binary.LittleEndian.Uint32(buf[off:]))); pid > 0 {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	return nil, errors.New("the process table kept growing while it was listed")
}

// shortRecord reads process pid's short BSD record (struct
// proc_bsdshortinfo) from the n bytes proc_info wrote to rec.
func shortRecord(pid int, rec []byte, n int) (Proc, error) {
	if n < shortBSDInfoSize {
		return Proc{}, fmt.Errorf("process %d's record is %d bytes, not %d", pid, n, shortBSDInfoSize)
	}
	return Proc{
		PID:    pid,
		PPID:   int(binary.LittleEndian.Uint32(rec[offShortPPID:])),
		PGID:   int(binary.LittleEndian.Uint32(rec[offShortPGID:])),
		Zombie: binary.LittleEndian.Uint32(rec[offShortStatus:]) == sZomb,
	}, nil
}
