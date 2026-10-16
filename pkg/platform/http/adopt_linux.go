//go:build linux

package http

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// Daemonized members (P146, design v14 §3.1: membership is set at fork, and a
// reparent changes nothing).
//
// For a native app the run's members are the keploy CLI's descendants: the
// agent's eBPF filter walks a process's real parents up to the CLI (the
// client it registered), and the proxy's listener check walks /proc's parent
// pids up to it (descends). A daemon the app starts leaves that tree: pg_ctl
// start and redis-server --daemonize fork a child that starts the daemon in a
// session of its own and exits, so the daemon is orphaned and the kernel
// reparents it to the nearest child subreaper, or to init. From then on its
// calls were neither intercepted nor judged (the set passed with them gone to
// the real dependency), and a call to its listener was mocked as an outside
// dependency's.
//
// Under the proven switch the CLI makes itself the child subreaper of its
// descendants (PR_SET_CHILD_SUBREAPER), so an orphaned descendant is
// reparented to the CLI and stays in the tree both walks read. A subreaper
// must reap what it adopts, or every adopted daemon that exits stays a zombie
// for the rest of the run (a fixture that waits for it to go, kill -0 or
// pgrep, would wait for ever). It must reap only those: the CLI's own children
// are waited for by os/exec, and a child reaped under it makes its Wait fail.
// Go holds a pidfd for every child it starts, from the clone that starts it
// until its Wait has reaped it (os.StartProcess, where pidfds work), so a
// zombie child of the CLI that no pidfd of the CLI's names is one it adopted;
// that is checked once, on a child of its own, before the CLI becomes a
// subreaper, and without it nothing changes and a warning says what is lost.
//
// The alternative weighed, membership by cgroup, needs a cgroup of keploy's
// own for the app (cgroup v2, a writable and delegated hierarchy, which
// containers, CI runners and WSL often lack) and a change to the eBPF filter;
// and the end state, the fork-time instance map (§3.3, the native launcher),
// replaces the parent walk outright. Until then the subreaper keeps the walk
// true across a reparent with a change to the CLI alone.

var adoptOnce sync.Once

// adoptDaemons makes this process, the CLI whose tree is the run's
// membership, the child subreaper of its descendants and reaps the ones it
// adopts, once per process; when it cannot, it says so.
func adoptDaemons(logger *zap.Logger) {
	adoptOnce.Do(func() {
		if _, err := startAdopting(logger); err != nil {
			logger.Warn("keploy cannot keep a daemon the application starts in this run: a process the application starts and leaves behind (pg_ctl start, redis-server --daemonize) is reparented away from keploy, and its calls are neither intercepted nor judged",
				zap.Error(err),
				zap.String("next_step", "start such a dependency before keploy, where it is an outside dependency, recorded and mocked; or run it in the foreground of the command keploy runs (postgres -D <dir> &, redis-server --daemonize no &), where it stays the command's child"))
		}
	})
}

// startAdopting is adoptDaemons' work: it checks that Go holds a pidfd for
// each child it starts, makes this process a child subreaper, and reaps every
// adopted child as it exits (SIGCHLD) until stop is called.
func startAdopting(logger *zap.Logger) (stop func(), err error) {
	if err := childrenHavePidfds(); err != nil {
		return nil, err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("make the CLI a child subreaper: %w", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	done := make(chan struct{})
	go func() {
		defer utils.Recover(logger)
		for {
			// Every adopted child that is a zombie now, and every one a scan
			// missed while the list changed under it: a pass that reaped one
			// is followed by another.
			for {
				if len(reapAdopted(logger)) == 0 {
					break
				}
			}
			select {
			case <-sig:
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(sig)
			close(done)
		})
	}, nil
}

// childrenHavePidfds reports, by starting a child, whether Go holds a pidfd
// for it until it is waited for: what tells this process's own children from
// the ones it adopts.
func childrenHavePidfds() error {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start a child to tell its own children from adopted ones: %w", err)
	}
	held := heldPidfds(nil)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("wait for the child that tells its own children from adopted ones: %w", err)
	}
	if !held[cmd.Process.Pid] {
		return errors.New("Go holds no pidfd for the children it starts here (pidfds need Linux 5.4 or later, and no seccomp filter refusing them), so keploy cannot tell its own children from adopted ones")
	}
	return nil
}

// reapAdopted reaps every zombie child of this process that no pidfd of its
// own names, and returns their pids. Each is reaped through a pidfd opened on
// it, so a pid that was reaped and reused meanwhile is never waited for in
// its place; and the pidfds held are read after those are opened, so a child
// of this process that was a zombie then is named by its own.
func reapAdopted(logger *zap.Logger) []int {
	type opened struct{ pid, fd int }
	var zombies []opened
	for _, pid := range childPids() {
		if st, ok := utils.ReadProcStat(pid); !ok || st.State != 'Z' {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			continue // reaped already
		}
		zombies = append(zombies, opened{pid, fd})
	}
	if len(zombies) == 0 {
		return nil
	}
	ours := make(map[int]bool, len(zombies))
	for _, z := range zombies {
		ours[z.fd] = true
	}
	held := heldPidfds(ours)
	var reaped []int
	for _, z := range zombies {
		if !held[z.pid] {
			var info unix.Siginfo
			if err := unix.Waitid(unix.P_PIDFD, z.fd, &info, unix.WEXITED|unix.WNOHANG, nil); err == nil {
				reaped = append(reaped, z.pid)
				logger.Debug("reaped a daemon the run started that keploy had adopted", zap.Int("pid", z.pid))
			}
		}
		_ = unix.Close(z.fd)
	}
	return reaped
}

// childPids is this process's children: each thread's children list
// (/proc/self/task/<tid>/children), or, where the kernel keeps none
// (CONFIG_PROC_CHILDREN off), every process whose parent it is.
func childPids() []int {
	var pids []int
	listed := false
	if tasks, err := os.ReadDir("/proc/self/task"); err == nil {
		for _, t := range tasks {
			b, err := os.ReadFile("/proc/self/task/" + t.Name() + "/children")
			if err != nil {
				continue
			}
			listed = true
			for _, f := range strings.Fields(string(b)) {
				if pid, err := strconv.Atoi(f); err == nil {
					pids = append(pids, pid)
				}
			}
		}
	}
	if listed {
		return pids
	}
	self := os.Getpid()
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		if st, ok := utils.ReadProcStat(pid); ok && st.PPID == self {
			pids = append(pids, pid)
		}
	}
	return pids
}

// heldPidfds is the set of pids this process holds a pidfd for, but the fds
// in skip: a pidfd's fdinfo names its process ("Pid:"), and no other kind of
// fd has that line.
func heldPidfds(skip map[int]bool) map[int]bool {
	held := map[int]bool{}
	fds, err := os.ReadDir("/proc/self/fdinfo")
	if err != nil {
		return held
	}
	for _, f := range fds {
		fd, err := strconv.Atoi(f.Name())
		if err != nil || skip[fd] {
			continue
		}
		b, err := os.ReadFile("/proc/self/fdinfo/" + f.Name())
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "Pid:"); ok {
				if pid, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && pid > 0 {
					held[pid] = true
				}
				break
			}
		}
	}
	return held
}
