//go:build unix

package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// EscalationSignal is what the CLI sends a stopping agent it has judged to
// have stopped making progress, or to have detached everything and not exited
// (pkg/platform/http, stopWatch); and, in place of SIGKILL, one it may not
// signal itself: an agent elevated by sudo runs as root, which the user who
// ran keploy cannot send SIGKILL, while sudo keeps that user's uid and hands
// this signal on to the agent (checked with sudo 1.9.16p2).
const EscalationSignal = syscall.SIGUSR2

// DieOnEscalation has EscalationSignal end this process. Before its stop has
// begun (ctx, the root context, is not done yet) the signal begins it, as
// SIGTERM would (ExecCancel): a stray one -- an operator who meant SIGUSR1,
// the parsers' switch -- stops the agent gracefully instead of killing it
// with its CA still installed. Once the stop has begun, the signal kills the
// process (SIGKILL to itself, or an exit where that cannot kill it: a pid
// namespace's init) as soon as no teardown work is in progress
// (BeginTeardownWork): the CLI judged from a report a moment old, and the
// agent may have begun a uprobe removal, or a CA tool rewriting a trust
// store, since. It is for `keploy agent`.
func DieOnEscalation(ctx context.Context) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, EscalationSignal)
	go func() {
		for {
			<-c
			if ctx.Err() != nil {
				break
			}
			ExecCancel()
		}
		for {
			teardownWorkMu.Lock()
			if teardownWork.Load() == 0 {
				// Held across the kill: no work begins in between.
				if os.Getpid() == 1 {
					// The init of its pid namespace (a container's PID 1),
					// which no signal sent from inside the namespace kills,
					// its own included: exit instead, the same end to
					// anything in flight.
					os.Exit(137)
				}
				_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
				select {} // the kill may land after Kill returns (macOS)
			}
			teardownWorkMu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

// dirSink writes TeardownProgressFileName through a directory opened once
// (openCommandLineSink): every write goes to that directory, whatever its
// path names by then.
type dirSink struct {
	dir  int // the directory's fd, open for as long as its reporter runs
	name string
	n    atomic.Uint64
}

// close lets the directory go, once the reporter writes no more; the sink
// is not used after it.
func (d *dirSink) close() {
	_ = unix.Close(d.dir)
	d.dir = -1
}

// openCommandLineSink opens the directory of a command-line progress path
// and checks it: not a symlink, written to by no one but its owner, owned by
// root, by this user, or by the user sudo ran this agent for (SUDO_UID).
func openCommandLineSink(path string) (progressSink, error) {
	if err := checkCommandLinePath(path); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("the teardown progress file's directory %q: %w", dir, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("the teardown progress file's directory %q: %w", dir, err)
	}
	if st.Mode&0o022 != 0 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("the teardown progress file's directory %q may be written by others (mode %o)", dir, st.Mode&0o777)
	}
	if !ownerMayAsk(st.Uid) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("the teardown progress file's directory %q belongs to uid %d, not to this agent's user", dir, st.Uid)
	}
	return &dirSink{dir: fd, name: filepath.Base(path)}, nil
}

// ownerMayAsk reports whether a directory uid owns may hold this agent's
// report: root's, this user's, or that of the user sudo ran it for.
func ownerMayAsk(uid uint32) bool {
	if uid == 0 || uid == uint32(os.Getuid()) {
		return true
	}
	sudoUID, err := strconv.ParseUint(os.Getenv("SUDO_UID"), 10, 32)
	return err == nil && uint32(sudoUID) == uid
}

func (d *dirSink) clear() error {
	if err := unix.Unlinkat(d.dir, d.name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	return nil
}

// write replaces the report with b: a new file in the directory, renamed
// over it.
func (d *dirSink) write(b []byte) error {
	tmp := fmt.Sprintf(".%s.%d.%d", d.name, os.Getpid(), d.n.Add(1))
	fd, err := unix.Openat(d.dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, werr := f.Write(b)
	if werr == nil {
		// Readable by the CLI that asked for it, whatever this umask says.
		werr = f.Chmod(0o644)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = unix.Renameat(d.dir, tmp, d.dir, d.name)
	}
	if werr != nil {
		_ = unix.Unlinkat(d.dir, tmp, 0)
	}
	return werr
}
