//go:build windows

package agent

import (
	"errors"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procAlive reports whether process pid is the one created at start and has
// not exited: an exited process whose object another handle keeps has
// exited, and a reused pid has another creation time.
func procAlive(pid int, start uint64) bool {
	got, ok := procStart(pid)
	return ok && got == start
}

// procStart is process pid's creation time (a FILETIME: 100 ns intervals
// since 1601), false when no process has that pid, it has exited, or this
// user may not open it (another user's process, which the other systems skip
// too). The rights it asks are granted on the user's own processes with no
// elevation.
func procStart(pid int) (uint64, bool) {
	if pid <= 0 || int64(pid) > math.MaxUint32 {
		return 0, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	// A process's handle is signaled once it has exited, while its object,
	// and so its pid, lives on for as long as a handle to it is open, as a
	// zombie does until it is reaped.
	if ev, err := windows.WaitForSingleObject(h, 0); err != nil || ev != uint32(windows.WAIT_TIMEOUT) {
		return 0, false
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, false
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), true
}

// zombieExited is true: procStatStart reads a /proc, which Windows has not,
// so it never asks.
var zombieExited = func(int) bool { return true }

// procDescends reports whether pid is ancestor or one of its descendants,
// from one snapshot of every process's parent and the creation times of the
// chain (descendsByCreation). A process whose parent has exited descends from
// no one, as one reparented to init does on Linux.
func procDescends(pid, ancestor int) bool {
	parents, err := processParents()
	if err != nil {
		return false
	}
	return descendsByCreation(pid, ancestor, func(p int) (int, bool) {
		ppid, ok := parents[p]
		return ppid, ok
	}, procStart)
}

// processParents is each process's parent, from a Toolhelp32 snapshot of the
// system's processes, which a standard user takes with no elevation.
func processParents() (map[int]int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	parents := map[int]int{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		parents[int(e.ProcessID)] = int(e.ParentProcessID)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return parents, nil
}
