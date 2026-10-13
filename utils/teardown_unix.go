//go:build unix

package utils

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// EscalationSignal is what the CLI sends a stopping agent it has to kill but
// may not signal itself: an agent elevated by sudo runs as root, which the
// user who ran keploy cannot send SIGKILL, while sudo keeps that user's uid
// and hands this signal on to the agent (checked with sudo 1.9.16p2).
const EscalationSignal = syscall.SIGUSR2

// DieOnEscalation has EscalationSignal kill this process (SIGKILL to itself,
// or an exit where that cannot kill it: a pid namespace's init). It is for `keploy agent`, whose CLI sends it only when its
// stop has to be forced: a second Ctrl+C, or a teardown that has stopped
// making progress or has detached everything and not exited
// (pkg/platform/http, stopWatch).
func DieOnEscalation() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, EscalationSignal)
	go func() {
		<-c
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
		// Still here: this is the init of its pid namespace (a container's
		// PID 1), which no signal sent from inside the namespace kills, its
		// own included. Exit instead: the same end to anything in flight.
		os.Exit(137)
	}()
}

// dirSink writes TeardownProgressFileName through a directory opened once
// (openCommandLineSink): every write goes to that directory, whatever its
// path names by then.
type dirSink struct {
	dir  int // the directory's fd, open for the life of the process
	name string
	n    atomic.Uint64
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
