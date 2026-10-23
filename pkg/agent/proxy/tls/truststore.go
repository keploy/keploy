package tls

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"go.uber.org/zap"
)

// The machine-wide trust-store lock, the one lock every keploy process on a
// host takes while it changes the host's trust state in a way that writes a
// JDK keystore: every keytool edit of one (editJavaKeystore), and every
// rebuild of the system trust store (updateCaStore, refreshCaStore), whose
// Debian hook rewrites /etc/ssl/certs/java/cacerts. Linux's is in
// truststore_linux.go; elsewhere it is a no-op (truststore_other.go).
//
// The lock is the one draft keploy#4755 builds, lifted from it: the same file
// (/run/keploy/ca-trust.lock), the same flock(2), the same names
// (lockTrustStore, storeLockKey, msgTrustStoreBusy, storeCommand,
// runStoreCommand). Two things are added: work under a context that holds the
// lock does not take it again (holdsTrustStoreLock, withTrustStoreLock), so
// #4755's hold around the whole of an install's and a teardown's change of
// the store composes with the holds taken here into one lock; and a process
// waiting for it sees each step of the holder's change, and the progress of
// a command the holder runs under it (Linux: the holder's record in the lock
// file, writeHolderRecord, noteTrustStoreProgress, noteCommandProgress).

// storeLockKey is the context key under which lockTrustStore puts the open
// lock file.
type storeLockKey struct{}

// holdsTrustStoreLock reports whether ctx is one lockTrustStore returned, so
// that its caller holds the trust-store lock. The lock is not reentrant (a
// second take waits for the first), so work done under such a context must not
// take it again.
func holdsTrustStoreLock(ctx context.Context) bool { return ctx.Value(storeLockKey{}) != nil }

// msgTrustStoreBusy is what a keploy process logs when it has to queue for the
// trust-store lock. It is a constant because tests wait for exactly this
// message to know that a caller has reached the lock and is blocked on it.
const msgTrustStoreBusy = "another keploy agent on this machine is changing the system trust store; waiting for it to finish"

// withTrustStoreLock runs change under the trust-store lock: ctx's own when it
// holds it already, else one taken now, the wait for it ended by ctx or by a
// holder that makes no progress (lockTrustStore). change gets the context the
// lock is held under (which carries it, and is not cancelled with ctx), and
// must take a step of its change (noteTrustStoreProgress, or a command run
// through runStoreCommand or combinedOutputNoting) at least every
// trustStoreStallBeats beats, or the processes queued behind it give up.
func withTrustStoreLock(ctx context.Context, logger *zap.Logger, change func(locked context.Context) error) error {
	if holdsTrustStoreLock(ctx) {
		return change(ctx)
	}
	locked, unlock, err := lockTrustStore(ctx, logger)
	if err != nil {
		return err
	}
	defer unlock()
	return change(locked)
}

// storeCommand is exec.CommandContext for a command that changes the trust
// state under the lock: a trust-store rebuild, a keytool edit.
//
// Started under a context lockTrustStore returned, the command inherits the
// trust-store lock (an extra open file descriptor of the same open lock). The
// kernel releases a flock only when every descriptor of it is closed, so the
// lock is then held until this process and the command have both exited.
// Without that, an agent killed in the middle of a rebuild would release the
// lock while its tool ran on (the Debian hook's JVM rewriting the JDK
// keystore), and the next agent would edit beside it.
//
// The command is killed when ctx ends; a context from lockTrustStore never
// does, so a rebuild started under one runs to its end.
func storeCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return lockedCommand(ctx, ctx, name, args...)
}

// lockedCommand is storeCommand for a command killed when ctx ends but given
// the lock locked holds: a keytool run on keploy's own copy of a keystore,
// which a stop may abandon. A command killed (ctx's end) whose descendants
// still hold its output open is waited for no longer than storeCommandWaitDelay
// after: a wrapper that forks its tool must not keep a ceiling from ending the
// wait.
func lockedCommand(ctx, locked context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = storeCommandWaitDelay
	if f, ok := locked.Value(storeLockKey{}).(*os.File); ok && f != nil {
		cmd.ExtraFiles = []*os.File{f}
	}
	return cmd
}

// storeCommandWaitDelay is how long the wait for a killed store command waits
// for its output's pipes to close (exec.Cmd.WaitDelay).
const storeCommandWaitDelay = 10 * time.Second

// runStoreCommand runs cmd, a store command, and returns what it wrote to its
// stdout and stderr, like CombinedOutput.
//
// The output goes to an unlinked temporary file, not to a pipe back to this
// process. If this process dies while the command runs, a pipe breaks, and the
// command dies at its next write (SIGPIPE) — update-ca-certificates, for one,
// reports after it has replaced the bundle and before it runs its hooks — half
// way through the change to the store that the inherited lock is there to let
// it finish.
func runStoreCommand(cmd *exec.Cmd) ([]byte, error) { return runStoreCommandWithin(cmd, 0) }

// runStoreCommandWithin is runStoreCommand for a command waited for no longer
// than ceiling (0: for as long as it runs). Past the ceiling the command is
// left running, never killed: it writes the live trust store (Fedora's
// update-ca-trust empties /etc/pki/tls/certs before it fills it again, so one
// killed half-way leaves it empty), and it keeps the trust-store lock it
// inherited until it ends, so no other agent changes the store beside it.
// The wait then ends with an error naming it, and its progress is noted no
// more (the agents queued behind it judge it by the stall rule:
// lockTrustStore; so does a later wait of this process, for one beat:
// abandonedStoreCommand). What it wrote so far is returned.
func runStoreCommandWithin(cmd *exec.Cmd, ceiling time.Duration) ([]byte, error) {
	f, err := os.CreateTemp("", "keploy-store-command-*.log")
	if err != nil {
		return cmd.CombinedOutput()
	}
	if runtime.GOOS != "windows" {
		// Nothing to clean up afterwards, whatever happens to this process.
		_ = os.Remove(f.Name())
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stop := noteCommandProgress(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }() // reaps it, however long it takes
	var limit <-chan time.Time
	if ceiling > 0 {
		c, stopTimer := storeCommandCeilingTimer(ceiling)
		defer stopTimer()
		limit = c
	}
	var runErr error
	abandoned := false
	select {
	case runErr = <-done:
	case <-limit:
		select {
		case runErr = <-done: // it ended as the ceiling came
		default:
			abandoned = true
			runErr = fmt.Errorf("%s has run for %v and is left running (pid %d): it holds the trust-store lock until it ends", cmd.Path, ceiling, cmd.Process.Pid)
		}
	}
	stop()
	if abandoned {
		abandonedStoreCommand(cmd)
	}
	out, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<20))
	if err != nil && runErr == nil {
		return out, err
	}
	return out, runErr
}

// storeCommandCeilingTimer starts the timer of a store command's ceiling
// (runStoreCommandWithin) and returns its channel and the function that stops
// it. A variable so tests can end the wait themselves.
var storeCommandCeilingTimer = func(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// combinedOutputNoting is cmd.CombinedOutput, with the progress of cmd, a
// command started under the trust-store lock, noted for the processes queued
// for it (runNotingProgress). For a keytool run: one that outlives this
// process (a JVM ignores the broken pipe) keeps the lock it inherited until it
// exits, and what it was writing is only keploy's copy of a keystore.
func combinedOutputNoting(cmd *exec.Cmd) ([]byte, error) {
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := runNotingProgress(cmd)
	return b.Bytes(), err
}

// runNotingProgress runs cmd, and while it runs marks a step of the change it
// is part of for the processes queued for the trust-store lock each time it
// makes progress (noteCommandProgress), so that a long one (Debian's
// update-ca-certificates and the JVM of its hook) does not look to them like
// a holder that has stopped.
func runNotingProgress(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	stop := noteCommandProgress(cmd)
	err := cmd.Wait()
	stop()
	return err
}
