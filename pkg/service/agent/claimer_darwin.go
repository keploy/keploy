//go:build darwin

package agent

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// kinfoProc is the subset of Darwin's struct kinfo_proc (sys/sysctl.h) that
// the claimer reads: the process state, its parent pid, and its start time.
// The full struct is 648 bytes on arm64; only the offsets of the fields we
// read matter, and those are stable across macOS versions.
type kinfoProc struct {
	_        [16]byte   // kp_proc up to p_stat
	Stat     int8       // kp_proc.p_stat (offset 16)
	_        [239]byte  // rest of kp_proc (256 total for extern_proc)
	Ppid     int32      // kp_eproc.e_ppid (offset 256..259)
	_        [132]byte  // to e_paddr at 392
	_        [240]byte  // rest of kp_eproc through to the end
	_        [16]byte   // padding to ki_start
}

// sysctl CTL_KERN / KERN_PROC / KERN_PROC_PID constants.
const (
	ctlKern        = 1
	kernProc       = 14
	kernProcPID    = 1
	kinfoSize      = 648 // sizeof(struct kinfo_proc) on arm64 and amd64
	szpStatRunning = 2   // SRUN
	szpStatZombie  = 5   // SZOMB
)

// getKinfoProc returns the kinfo_proc for pid via sysctl, or false if the
// process does not exist.
func getKinfoProc(pid int) (ppid int, startSec int64, startUsec int32, alive bool) {
	mib := [4]int32{ctlKern, kernProc, kernProcPID, int32(pid)}
	buf := make([]byte, kinfoSize)
	n := uintptr(kinfoSize)

	_, _, errno := syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		4,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&n)),
		0, 0,
	)
	if errno != 0 || n == 0 {
		return 0, 0, 0, false
	}

	// extern_proc.p_starttime is a struct timeval at offset 136 in extern_proc
	// (which starts at offset 0 of kinfo_proc).
	// timeval: tv_sec (int64 on 64-bit), tv_usec (int32).
	sec := int64(binary.LittleEndian.Uint64(buf[136:144]))
	usec := int32(binary.LittleEndian.Uint32(buf[144:148]))

	// e_ppid is at offset 256 (start of kp_eproc) + 0 = 256.
	pp := int32(binary.LittleEndian.Uint32(buf[256:260]))

	return int(pp), sec, usec, true
}

// startTimeKey packs a start time into a uint64 for comparison, the same way
// the Linux claimer uses clock ticks: we use seconds * 1_000_000 + microseconds.
func startTimeKey(sec int64, usec int32) uint64 {
	return uint64(sec)*1_000_000 + uint64(usec)
}

// procAlive reports whether process pid is the one that started at start and
// has not exited.
func procAlive(pid int, start uint64) bool {
	_, sec, usec, ok := getKinfoProc(pid)
	if !ok {
		return false
	}
	return startTimeKey(sec, usec) == start
}

// procStart is process pid's start time, false when it does not exist.
func procStart(pid int) (uint64, bool) {
	_, sec, usec, ok := getKinfoProc(pid)
	if !ok {
		return 0, false
	}
	return startTimeKey(sec, usec), true
}

// procDescends reports whether pid is ancestor or one of its descendants.
func procDescends(pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == ancestor {
			return true
		}
		ppid, _, _, ok := getKinfoProc(pid)
		if !ok {
			return false
		}
		pid = ppid
	}
	return pid == ancestor
}
