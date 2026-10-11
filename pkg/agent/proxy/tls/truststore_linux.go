//go:build linux

package tls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// Several keploy agents can run on one machine at the same time — parallel CI
// jobs on a shared runner, two services of a monorepo recorded side by side —
// and on Linux each of them installs its run CA into the same system trust
// store. Two things keep them out of each other's way, and both are flock(2)
// locks on files under /run/keploy, which the kernel drops the instant every
// process holding them is gone, however it went:
//
//   - the trust-store lock (lockTrustStore): one file every agent locks while
//     it changes the store, so that only one of them at a time adds or removes
//     an anchor and regenerates the trust outputs;
//   - the run claim (claimRun / runIsLive): one file per run, which its agent
//     keeps locked for as long as it lives, so another agent can tell the
//     anchor of a run that is still going from one a crashed run left behind.
//
// Neither needs a pid, a start time or a clock: there is no pid to be reused
// and no age to guess at. A lock that can be taken is, by that fact alone, a
// lock nobody holds.
//
// /run/keploy is root-owned, as the agent is, and both kinds of file are 0600
// (the claims in a 0700 directory), so no unprivileged process can open them
// and so hold up, or fool, an agent. /run is a tmpfs: nothing outlives a
// reboot, which is right, because no run does either.

// trustStateDir holds the trust-store lock and the run claims. A variable so
// tests can point it at a directory of their own.
var trustStateDir = "/run/keploy"

// trustStoreLockPath is the file every keploy agent on this machine locks
// while it changes the system trust store.
func trustStoreLockPath() string { return filepath.Join(trustStateDir, "ca-trust.lock") }

// runClaimsDir holds one claim file per running agent, named by its run id.
func runClaimsDir() string { return filepath.Join(trustStateDir, "ca-runs") }

// msgTrustStoreBusy is what an agent logs when it has to queue for the
// trust-store lock. It is a constant because tests wait for exactly this
// message to know that a caller has reached the lock and is blocked on it.
const msgTrustStoreBusy = "another keploy agent on this machine is changing the system trust store; waiting for it to finish"

// lockTrustStore blocks until this process holds the machine-wide trust-store
// lock. It returns the context to change the store under and the function that
// releases the lock.
//
// The distro tools that rebuild the trust outputs are not safe to run two at a
// time, and before this lock every agent that started or stopped ran one:
//
//   - Debian's update-ca-certificates assembles the new bundle in one fixed
//     file, /etc/ssl/certs/ca-certificates.crt.new, which every run appends
//     to, moves over the bundle and deletes on exit. Two runs at once fail
//     ("mv: cannot stat '...ca-certificates.crt.new'"), or succeed and leave a
//     bundle with certificates missing — the other agent's anchor, or the
//     machine's own roots, down to an empty bundle, which breaks TLS for every
//     process on the machine.
//   - Fedora's update-ca-trust deletes the certificate directory's hash links
//     and recreates them with a plain "ln -s", so two runs fail each other
//     ("ln: failed to create symbolic link ...: File exists"). keploy no longer
//     runs it (see swapP11KitTrust), but the swap that replaces it is one
//     agent's work at a time too.
//
// Holding this lock around every change makes each regeneration run alone, and
// the last one to run the one that saw every anchor.
//
// The context returned carries the lock to every command started from it with
// storeCommand, so that a command still running when this process dies keeps
// the lock until it exits too, and the next agent never regenerates beside it.
// It is also not cancelled with ctx: a regeneration that has started runs to
// its end, because one that is killed half-way leaves the store half-written
// for every process on the machine.
//
// The wait has no time limit. A holder either finishes or dies, and the kernel
// releases a dead holder's lock, so the queue always moves; ctx is how a
// caller that no longer wants the lock stops waiting for it. A lock that is
// free is taken even when ctx is already done: ctx bounds the wait, not the
// work. A lock that is busy is not waited for once ctx is done, and a wait
// whose ctx ends before its turn comes gives the lock straight back when it
// does come.
//
// The lock is not reentrant: a caller that already holds it and asks again
// waits for itself. Take it once, around the whole change.
func lockTrustStore(ctx context.Context, logger *zap.Logger) (context.Context, func(), error) {
	path := trustStoreLockPath()
	// Explicit modes: keploy runs with a process-wide umask of 0.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to create the directory of the trust-store lock %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open the trust-store lock %s: %w", path, err)
	}
	held := func() (context.Context, func(), error) {
		// Name the holder for whoever queues behind it. It is only ever read
		// for a log line, so a failure to write it changes nothing.
		if err := f.Truncate(0); err == nil {
			_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
		}
		work := context.WithValue(context.WithoutCancel(ctx), storeLockKey{}, f)
		return work, func() { _ = f.Close() }, nil
	}

	ok, err := tryFlock(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("failed to lock %s: %w", path, err)
	}
	if ok {
		return held()
	}

	if ctx.Err() != nil {
		// The caller has already given up: there is nothing to wait for.
		_ = f.Close()
		return nil, nil, fmt.Errorf("did not wait for the trust-store lock %s: %w", path, context.Cause(ctx))
	}
	logger.Info(msgTrustStoreBusy, zap.String("held_by_pid", trustStoreLockHolder()), zap.String("lock", path))
	start := time.Now()

	// flock(2) cannot be interrupted from here, so the blocking call gets a
	// goroutine of its own. It owns f until it returns.
	granted := make(chan error, 1)
	go func() { granted <- waitFlock(f) }()

	select {
	case err := <-granted:
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("failed to lock %s: %w", path, err)
		}
		if ctx.Err() == nil {
			logger.Debug("took the trust-store lock after waiting for another keploy agent", zap.Duration("waited", time.Since(start)))
			return held()
		}
		// The turn came, but after the caller had given up: give it back
		// below, like a turn that is still to come.
		granted <- nil
	case <-ctx.Done():
	}
	// The blocked call gets the lock when its turn comes. Give it straight
	// back, so an abandoned wait never holds up the agents queued behind it.
	go func() {
		<-granted
		_ = f.Close()
		if hook := abandonedWaitReturned.Load(); hook != nil {
			(*hook)()
		}
	}()
	return nil, nil, fmt.Errorf("stopped waiting for the trust-store lock %s: %w", path, context.Cause(ctx))
}

// abandonedWaitReturned, when set, is called once a wait for the trust-store
// lock that its caller gave up on has been granted the lock and given it back.
// It is for tests, which cannot otherwise know when that has happened.
var abandonedWaitReturned atomic.Pointer[func()]

// trustStoreLockHolder returns the pid the current holder of the trust-store
// lock wrote into it, for the log line of an agent that has to wait.
func trustStoreLockHolder() string {
	b, err := os.ReadFile(trustStoreLockPath())
	if pid := string(bytes.TrimSpace(b)); err == nil && pid != "" {
		return pid
	}
	return "unknown"
}

// claimRun marks the run with this id as live for as long as this process
// holds the claim it returns: a file in runClaimsDir, created and exclusively
// locked here. Another agent's sweep leaves the anchors of a live run alone
// (runIsLive). The claim lasts until release, or until this process is gone —
// a crash and a SIGKILL included, because it is the kernel that drops it.
//
// The caller holds the trust-store lock, so no sweep can run between the
// claim's creation and its lock.
func claimRun(runID string) (*runClaim, error) {
	if !validRunID(runID) {
		return nil, fmt.Errorf("cannot claim run %q: not a run id", runID)
	}
	dir := runClaimsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create the run claims directory %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone. Only root (and the
	// agent is root) can have made it, but whoever can open a claim can hold
	// it, so make sure nobody else can.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to make the run claims directory %s private: %w", dir, err)
	}
	path := filepath.Join(dir, runID)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create the claim of run %s: %w", runID, err)
	}
	ok, err := tryFlock(f)
	if err == nil && !ok {
		err = errors.New("locked by another process")
	}
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("failed to lock the claim of run %s: %w", runID, err)
	}
	// For a person looking at /run/keploy: nothing reads it.
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
	return &runClaim{path: path, f: f}, nil
}

// runIsLive reports whether a process still holds the claim of the run with
// this id: whether the agent that installed that run's anchors is still
// running, so that applications of its run are trusting leaves its CA signs
// right now.
//
// A run with no claim file is not live: its agent is gone and its claim was
// already cleared, or it was installed by a keploy from before claims, or the
// machine has rebooted since (/run is a tmpfs). A claim file nobody holds is
// not live either — its process is gone — and is removed on the way. An error
// means "unknown", and an anchor is only removed on proof.
//
// The caller holds the trust-store lock, so no claim is being made meanwhile.
func runIsLive(runID string) (bool, error) {
	if !validRunID(runID) {
		// No agent ever claims such a name, so no agent is using it.
		return false, nil
	}
	path := filepath.Join(runClaimsDir(), runID)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	ok, err := tryFlock(f)
	if err != nil {
		return false, fmt.Errorf("failed to test the claim of run %s: %w", runID, err)
	}
	if !ok {
		return true, nil
	}
	// The lock is ours, so nobody else's: the run's agent is gone.
	_ = os.Remove(path)
	return false, nil
}

// sweepRunClaims removes the claims of runs whose agents are gone and whose
// anchors, if they had any left, have already been swept.
func sweepRunClaims(logger *zap.Logger) {
	entries, err := os.ReadDir(runClaimsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if live, err := runIsLive(e.Name()); err != nil {
			logger.Debug("left a run claim in place: could not tell whether its run is still alive", zap.String("run", e.Name()), zap.Error(err))
		} else if !live {
			logger.Debug("cleared the claim of a run that is gone", zap.String("run", e.Name()))
		}
	}
}

// writeAnchor writes a run CA's certificate to path, an anchor file in the
// system trust store, replacing any same-named leftover. A file it could not
// write whole is removed.
func writeAnchor(path string, certPEM []byte) error {
	// Explicit 0644, not os.Create's 0666: under keploy's umask of 0 that left
	// a world-WRITABLE anchor any local user could swap. O_NOFOLLOW: the store
	// directories are root's, and so is what keploy writes in them.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	// The file may predate this call (a same-named leftover): make its mode
	// ours too.
	err = f.Chmod(0o644)
	if err == nil {
		_, err = f.Write(certPEM)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// tryFlock takes an exclusive flock on f without waiting and reports whether it
// got it. false with a nil error means another open file description — in this
// process or any other — holds the lock.
func tryFlock(f *os.File) (bool, error) {
	defer runtime.KeepAlive(f)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			return false, err
		}
	}
}

// waitFlock takes an exclusive flock on f, blocking until it is free.
func waitFlock(f *os.File) error {
	defer runtime.KeepAlive(f)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
