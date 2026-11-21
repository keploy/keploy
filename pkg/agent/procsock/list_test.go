package procsock

import (
	"encoding/binary"
	"errors"
	"slices"
	"testing"
)

// fakeList stands in for one of proc_info's process lists: it answers its
// size for no buffer and, for each buffer in turn, writes fill's pids (as
// many as fit) and answers the bytes written. It records each buffer's size.
type fakeList struct {
	size  int
	fills [][]int32
	bufs  []int
}

func (f *fakeList) list(buf []byte) (int, error) {
	if buf == nil {
		return f.size, nil
	}
	f.bufs = append(f.bufs, len(buf))
	pids := f.fills[min(len(f.bufs), len(f.fills))-1]
	n := 0
	for _, pid := range pids {
		if n+pidSize > len(buf) {
			break
		}
		binary.LittleEndian.PutUint32(buf[n:], uint32(pid))
		n += pidSize
	}
	return n, nil
}

func pidsOf(n int, from int32) []int32 {
	pids := make([]int32, n)
	for i := range pids {
		pids[i] = from + int32(i)
	}
	return pids
}

// A list that fills the whole buffer may have been cut short: it is read
// again into a buffer at least twice as large, and that read is the answer.
func TestReadPIDsListsAgainWhenTheListFillsTheBuffer(t *testing.T) {
	first := 10 + listPIDsHeadroom // fills the first buffer exactly
	f := &fakeList{size: 10 * pidSize, fills: [][]int32{pidsOf(first, 100), pidsOf(first+3, 1000)}}
	pids, err := readPIDs(f.list)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.bufs) != 2 || f.bufs[0] != first*pidSize || f.bufs[1] < 2*f.bufs[0] {
		t.Fatalf("buffers %v; want %d bytes, then one at least twice that", f.bufs, first*pidSize)
	}
	want := make([]int, first+3)
	for i := range want {
		want[i] = 1000 + i
	}
	if !slices.Equal(pids, want) {
		t.Fatalf("readPIDs = %v; want the second read's %d pids from 1000", pids, len(want))
	}
}

// A list that fills every buffer it is given is said, after a bounded
// number of reads, never answered cut short.
func TestReadPIDsSaysATableThatKeepsGrowing(t *testing.T) {
	f := &fakeList{size: pidSize, fills: [][]int32{pidsOf(1<<16, 1)}}
	pids, err := readPIDs(f.list)
	if err == nil || pids != nil {
		t.Fatalf("readPIDs = %v, %v; want an error", pids, err)
	}
	if len(f.bufs) != maxListPIDsRetries {
		t.Fatalf("read %d times; want %d", len(f.bufs), maxListPIDsRetries)
	}
}

// Every entry the list wrote is read, the last one too, and only pids above
// 0 are answered: pid 0 is the kernel, and kill(2) given 0 signals the
// caller's own process group.
func TestReadPIDsAnswersOnlyProcesses(t *testing.T) {
	f := &fakeList{size: 5 * pidSize, fills: [][]int32{{0, 7, -1, 1, 42}}}
	pids, err := readPIDs(f.list)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []int{7, 1, 42}) {
		t.Fatalf("readPIDs = %v; want [7 1 42]", pids)
	}
}

// An error from the list, for its size or for its pids, is answered.
func TestReadPIDsAnswersTheListsError(t *testing.T) {
	bad := errors.New("no list")
	size := func(buf []byte) (int, error) {
		if buf == nil {
			return 0, bad
		}
		return 0, nil
	}
	fill := func(buf []byte) (int, error) {
		if buf == nil {
			return pidSize, nil
		}
		binary.LittleEndian.PutUint32(buf, 9)
		return pidSize, bad
	}
	for name, list := range map[string]func([]byte) (int, error){"size": size, "pids": fill} {
		if pids, err := readPIDs(list); !errors.Is(err, bad) || pids != nil {
			t.Errorf("%s: readPIDs = %v, %v; want %v", name, pids, err, bad)
		}
	}
}

// record lays out a struct proc_bsdshortinfo as <sys/proc_info.h> has it,
// written out here rather than from the reader's own constants: 64 bytes,
// pbsi_pid at 0, pbsi_ppid at 4, pbsi_pgid at 8, pbsi_status at 12.
func record(ppid, pgid, status uint32) []byte {
	rec := make([]byte, 64)
	for i := range rec {
		rec[i] = 0xee
	}
	binary.LittleEndian.PutUint32(rec[0:], 0xffffffff) // pbsi_pid: not read
	binary.LittleEndian.PutUint32(rec[4:], ppid)
	binary.LittleEndian.PutUint32(rec[8:], pgid)
	binary.LittleEndian.PutUint32(rec[12:], status)
	return rec
}

func TestShortRecordReadsParentGroupAndState(t *testing.T) {
	const sRun, sZombie = 2, 5 // SRUN, SZOMB: bsd/sys/proc.h
	for _, c := range []struct {
		status uint32
		want   Proc
	}{
		{sRun, Proc{PID: 50, PPID: 3, PGID: 40}},
		{sZombie, Proc{PID: 50, PPID: 3, PGID: 40, Zombie: true}},
	} {
		got, err := shortRecord(50, record(3, 40, c.status), 64)
		if err != nil || got != c.want {
			t.Errorf("status %d: shortRecord = %+v, %v; want %+v", c.status, got, err, c.want)
		}
	}
	// A record shorter than the struct is not read.
	if got, err := shortRecord(50, record(3, 40, sRun), 63); err == nil {
		t.Errorf("a short record read as %+v; want an error", got)
	}
}
