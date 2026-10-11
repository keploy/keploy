package utils

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// statTail is a stat line's fields after pgrp, as Linux 6.12 writes them for a
// sleeping process: session through starttime (77), and the rest.
const statTail = " 600 0 -1 4194560 120 0 0 0 1 2 0 0 20 0 1 0 77 1234 56 18446744073709551615 1 1 0 0 0 0 0 0 65536 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0\n"

// The command name in a stat line is the process's own, and can hold spaces
// and parentheses: the fields after it are counted from the last ')'. A line
// parses only whole up to the start time.
func TestParseProcStat(t *testing.T) {
	sleeping := ProcStat{State: 'S', PPID: 603, PGRP: 600, StartTime: 77}
	for _, tc := range []struct {
		line string
		want ProcStat
		ok   bool
	}{
		{"604 (node) S 603 600" + statTail, sleeping, true},
		{"604 (node (app) )x() S 603 600" + statTail, sleeping, true},
		{"604 (a b) S 603 600" + statTail, sleeping, true},
		{"604 ()) Z 9 9" + statTail, ProcStat{State: 'Z', PPID: 9, PGRP: 9, StartTime: 77}, true},
		{"2 (kthreadd) S 0 0" + statTail, ProcStat{State: 'S', StartTime: 77}, true},
		{"1010 (kworker/u32:19-ext4-rsv-conversion) I 2 0" + statTail, ProcStat{State: 'I', PPID: 2, StartTime: 77}, true},
		// The line need not end in a newline once it reaches the start time.
		{"604 (node) S 603 600" + strings.TrimSuffix(statTail[:strings.Index(statTail, " 77 ")+4], " "), sleeping, true},
		// Torn or short lines.
		{"604 (node) S 603 600" + statTail[:strings.Index(statTail, " 77 ")], ProcStat{}, false},
		{"604 (node) S 603", ProcStat{}, false},
		{"604 (node) S 603 600 600 0 -1 4194560\n", ProcStat{}, false},
		{"604 (torn", ProcStat{}, false},
		{"", ProcStat{}, false},
		// A field ProcStat holds that is not what the kernel writes there.
		{"604 (node) S x 600" + statTail, ProcStat{}, false},
		{"604 (node) S 603 -1" + statTail, ProcStat{}, false},
		// A pid past the kernel's 32-bit pid_t, and the largest within it.
		{"604 (node) S 2147483648 600" + statTail, ProcStat{}, false},
		{"604 (node) S 603 2147483648" + statTail, ProcStat{}, false},
		{"604 (node) S 2147483647 600" + statTail, ProcStat{State: 'S', PPID: 2147483647, PGRP: 600, StartTime: 77}, true},
		{"604 (node) SS 603 600" + statTail, ProcStat{}, false},
		{"604 (node) S 603 600" + strings.Replace(statTail, " 77 ", " -77 ", 1), ProcStat{}, false},
	} {
		got, ok := ParseProcStat([]byte(tc.line))
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseProcStat(%q) = %+v, %v; want %+v, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

// InterruptProcessTree parses every process's stat before its first signal,
// so the parser allocates nothing.
func TestParseProcStatAllocatesNothing(t *testing.T) {
	line := []byte("604 (node (app) )x() S 603 600" + statTail)
	if allocs := testing.AllocsPerRun(100, func() { _, _ = ParseProcStat(line) }); allocs != 0 {
		t.Fatalf("ParseProcStat allocates %v times per line; want 0", allocs)
	}
}

// ReadProcStat reads this process as the kernel reports it on Linux, and
// reports nothing where there is no /proc.
func TestReadProcStatReadsThisProcess(t *testing.T) {
	stat, ok := ReadProcStat(os.Getpid())
	if runtime.GOOS != "linux" {
		if ok {
			t.Fatalf("ReadProcStat on %s = %+v; want nothing, there is no /proc", runtime.GOOS, stat)
		}
		return
	}
	if !ok {
		t.Fatal("ReadProcStat could not read this process")
	}
	// The state is the main thread's, often asleep while another reads; it is
	// some state of a live process. (Z is pinned by the zombie test's isZombie.)
	if stat.PPID != os.Getppid() || !strings.ContainsRune("RSD", rune(stat.State)) {
		t.Fatalf("ReadProcStat(self) = %+v; want parent %d, and running or asleep", stat, os.Getppid())
	}
	if _, ok := ReadProcStat(1 << 30); ok {
		t.Fatal("ReadProcStat read a pid no process has")
	}
	// The start time is in clock ticks after boot: this process started
	// after the system did, and less than the system's uptime ago.
	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Fatal(err)
	}
	up, err := time.ParseDuration(strings.Fields(string(uptime))[0] + "s")
	if err != nil {
		t.Fatal(err)
	}
	if stat.StartTime == 0 || time.Duration(stat.StartTime)*time.Second/100 > up {
		t.Fatalf("this process started %d ticks after boot; the system has been up %s", stat.StartTime, up)
	}
}
