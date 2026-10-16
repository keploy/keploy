//go:build linux

package http

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// adoptHelperEnv runs TestDaemonsAreAdoptedAndReaped's body in a process of
// its own: a subreaper is a property of the whole process, which the other
// tests of this package share.
const adoptHelperEnv = "KEPLOY_TEST_ADOPT_HELPER"

// A daemon the run starts (a child that starts it in a session of its own and
// exits, as pg_ctl start does) is reparented to keploy's CLI, not to init, so
// it stays in the tree that is the run's membership; once it exits it is
// reaped, never left a zombie; and the CLI's own children, many at once while
// adopted ones exit, are left to their own Wait, which reads each one's exit
// status.
func TestDaemonsAreAdoptedAndReaped(t *testing.T) {
	if os.Getenv(adoptHelperEnv) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonsAreAdoptedAndReaped$", "-test.v")
		cmd.Env = append(os.Environ(), adoptHelperEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the helper process failed: %v\n%s", err, out)
		}
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()

	dir := t.TempDir()
	const daemons = 8
	type daemon struct {
		pid  int
		fifo string
	}
	var ds []daemon
	for i := 0; i < daemons; i++ {
		fifo := filepath.Join(dir, fmt.Sprintf("fifo-%d", i))
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		// The daemonizer starts cat in a session of its own and exits: cat is
		// orphaned, holding its fifo open for read until it is written.
		out, err := exec.Command("/bin/sh", "-c", `setsid cat "$1" >/dev/null 2>&1 & echo $!`, "sh", fifo).Output()
		if err != nil {
			t.Fatalf("the daemonizer: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatalf("the daemonizer printed %q", out)
		}
		ds = append(ds, daemon{pid, fifo})
	}
	for _, d := range ds {
		// The daemonizer has exited and been waited for: its child is
		// reparented by now.
		st, ok := utils.ReadProcStat(d.pid)
		if !ok || st.PPID != os.Getpid() {
			t.Fatalf("daemon %d: parent %d (found %v), want keploy's CLI, %d", d.pid, st.PPID, ok, os.Getpid())
		}
	}

	// The CLI's own children exit while the daemons do: each Wait reads its
	// child's own status, none reaped from under it.
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(code int) {
			defer wg.Done()
			err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != code {
				errs <- fmt.Errorf("own child exiting %d: Wait = %v", code, err)
			}
		}(1 + i%100)
	}
	for _, d := range ds {
		f, err := os.OpenFile(d.fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close() // cat reads EOF and exits
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Each daemon is reaped once it has exited: gone, never a zombie of the
	// CLI's. The bound only turns a hang into a failure.
	deadline := time.Now().Add(time.Minute)
	for _, d := range ds {
		for {
			st, ok := utils.ReadProcStat(d.pid)
			if !ok || st.PPID != os.Getpid() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("daemon %d is still the CLI's child (state %c) after it exited", d.pid, st.State)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
