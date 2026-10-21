//go:build linux

package starvetest

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// heavyExitEnv, set to a count, makes TestHeavyExitHelper map that many
// pages and say how many mappings it has.
const heavyExitEnv = "KEPLOY_TEST_HEAVY_EXIT"

// TestHeavyExitHelper is not a test: with heavyExitEnv set it is a process
// that calls HeavyExit and prints how many mappings it then has.
func TestHeavyExitHelper(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv(heavyExitEnv))
	if err != nil {
		t.Skip("a process of TestHeavyExitMapsEachPageOnItsOwn")
	}
	if err := HeavyExit(n); err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(3)
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		os.Exit(4)
	}
	os.Stdout.WriteString("maps " + strconv.Itoa(bytes.Count(maps, []byte("\n"))) + "\n")
	os.Exit(0)
}

// HeavyExit leaves each page it maps a mapping of its own, which the
// process's exit tears down: none merge.
func TestHeavyExitMapsEachPageOnItsOwn(t *testing.T) {
	const n = 3000
	cmd := exec.Command(os.Args[0], "-test.run=^TestHeavyExitHelper$")
	cmd.Env = append(os.Environ(), heavyExitEnv+"="+strconv.Itoa(n))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the helper failed: %v: %s", err, out)
	}
	var maps int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if v, ok := bytes.CutPrefix(sc.Bytes(), []byte("maps ")); ok {
			maps, _ = strconv.Atoi(string(v))
		}
	}
	if maps < n {
		t.Fatalf("the helper has %d mappings after HeavyExit(%d): its pages merged", maps, n)
	}
}

// Ran is the CPU the threads of a process had: it grows by what the process
// runs, and a process that is not there is not read.
func TestRanIsTheCPUAProcesssThreadsHad(t *testing.T) {
	before, ok := Ran(os.Getpid())
	if !ok {
		t.Skip("this kernel keeps no account of a thread's time on a CPU (schedstat)")
	}
	const spin = 50 * time.Millisecond
	var ts unix.Timespec
	cpu := func() time.Duration {
		_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts)
		return time.Duration(ts.Nano())
	}
	for start := cpu(); cpu()-start < spin; {
	}
	after, _ := Ran(os.Getpid())
	if after-before < spin {
		t.Fatalf("Ran grew by %v across %v this process ran", after-before, spin)
	}
	if _, ok := Ran(1 << 30); ok {
		t.Fatal("a process that is not there was read")
	}
}

// Spin returns once its file is there, and not before.
func TestSpinReturnsOnceReleased(t *testing.T) {
	released := filepath.Join(MemDir(t), "released")
	done := make(chan struct{})
	go func() { Spin(released); close(done) }()
	select {
	case <-done:
		t.Fatal("Spin returned before the file was there")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(released, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	<-done
}
