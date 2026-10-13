package tls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"go.keploy.io/server/v3/utils"
)

// The pieces of the system trust-store install that every platform shares.
// The Linux mechanisms they serve — the machine-wide trust-store lock and the
// per-run claims — are in truststore_linux.go; off Linux those are no-ops
// (truststore_other.go).

// storeLockKey is the context key under which lockTrustStore puts the open
// lock file, for storeCommand to hand to the commands it starts.
type storeLockKey struct{}

// storeCommand is exec.CommandContext for a command that changes the system
// trust store: a bundle regeneration, a p11-kit extraction, a keytool import or
// delete.
//
// Started from the context lockTrustStore returned, the command inherits the
// trust-store lock (as an extra open file descriptor of the same open lock).
// The kernel releases a flock only when every descriptor of it is closed, so
// the lock is then held until this process and the command have both exited.
// Without that, an agent killed in the middle of a regeneration would release
// the lock while its tool ran on, and the next agent would regenerate beside
// that tool — the very overlap the lock exists to prevent.
func storeCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if f, ok := ctx.Value(storeLockKey{}).(*os.File); ok && f != nil {
		cmd.ExtraFiles = []*os.File{f}
	}
	return cmd
}

// runStoreCommand runs cmd, a store command, and returns what it wrote to its
// stdout and stderr, like CombinedOutput.
//
// The output goes to an unlinked temporary file, not to a pipe back to this
// process. If this process dies while the command runs, a pipe breaks, and the
// command dies at its next write (SIGPIPE) — update-ca-certificates, for one,
// reports after it has replaced the bundle and before it runs its hooks — half
// way through the change to the store that the inherited lock is there to let
// it finish.
func runStoreCommand(cmd *exec.Cmd) ([]byte, error) {
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
	runErr := cmd.Run()
	out, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<20))
	if err != nil && runErr == nil {
		return out, err
	}
	return out, runErr
}

// runClaim is what marks this process's run CA as in use, for as long as it is
// held (Linux: see claimRun). It is nil-safe, and holds no file off Linux.
type runClaim struct {
	path string
	f    *os.File
}

// release gives up the claim. Its run's anchors must be gone by then, or the
// next sweep removes them.
func (c *runClaim) release() {
	if c == nil || c.f == nil {
		return
	}
	_ = os.Remove(c.path)
	_ = c.f.Close()
	c.f = nil
}

// validRunID reports whether id can be a run id: what generateRunCA makes (16
// lowercase hex digits) and so what names a run's claim file and its anchors,
// keploy-mitm-<id>.crt.
func validRunID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// errInterruptedAgain is why a stopping agent gave up its place in the queue
// for the trust store when a second interrupt reached it.
var errInterruptedAgain = errors.New("keploy was interrupted again while it was stopping")

// errSignalledWhileStopping is why it gave up its place when a signal reached
// it during a stop that no signal started: the CLI stops an agent over its API
// (POST /agent/stop), so the first signal the agent sees comes in the middle of
// that stop.
var errSignalledWhileStopping = errors.New("keploy was signalled while it was stopping")

// stopSignals returns a channel that receives every SIGINT and SIGTERM sent to
// this process from now on, and the function that stops that. A variable so
// that tests can stand in for the signals.
var stopSignals = func() (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return c, func() { signal.Stop(c) }
}

// untilToldToStopNow returns a context that is done when keploy, already
// stopping, is told to stop now — and the function that releases it. Told to
// stop now is either of:
//
//   - a second interrupt (utils.InterruptedAgain): the stop began with a signal
//     and another one came;
//   - any SIGINT or SIGTERM that reaches this process while the context lives:
//     the stop began some other way — the CLI asked for it over the agent's
//     API — and this is the first signal, sent to say "now".
//
// One that has already happened when the context is made makes it done before
// it is returned, with nothing in between to race.
func untilToldToStopNow() (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(context.Background())
	sigs, stopSigs := stopSignals()
	again := utils.InterruptedAgain()
	select {
	case <-again:
		cancel(errInterruptedAgain)
	case sig := <-sigs:
		cancel(fmt.Errorf("%w (%s)", errSignalledWhileStopping, sig))
	default:
		go func() {
			select {
			case <-again:
				cancel(errInterruptedAgain)
			case sig := <-sigs:
				cancel(fmt.Errorf("%w (%s)", errSignalledWhileStopping, sig))
			case <-ctx.Done():
			}
		}()
	}
	return ctx, func() {
		stopSigs()
		cancel(nil)
	}
}
