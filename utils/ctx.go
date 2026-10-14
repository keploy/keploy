// Package utils provides utility functions for the Keploy application.
package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

var cancel context.CancelFunc

// preCancelHooks are callbacks fired by NewCtx's signal handler in the
// instant BEFORE cancel() is called. The handler runs them synchronously,
// in registration order, so subsystems can read live state (e.g. the
// agent's syncMock buffer length) at the exact moment of shutdown —
// before any goroutine has reacted to ctx cancellation.
//
// Use case (added for the mongo teardown-orphan investigation): the
// agent registers a hook that writes the syncMock buffer state to
// stderr via fmt.Fprintf (NOT zap, which is buffered and silently
// drops messages when the process dies before the next flush). This
// gives a definitive answer to "did the agent have unsent mocks at
// the moment of shutdown" without depending on the structured logger.
//
// Hooks MUST be bounded, and well inside the stop grace the process may be
// given before it is killed (10s under docker compose; a pod's grace period
// under Kubernetes) — they run on
// the signal-delivery goroutine and any blocking work delays the
// cancellation and increases the chance of SIGKILL truncation. A hook
// that has to do I/O bounds it itself, as the agent's replay-outcome
// write does (pkg/service/agent, leaveStopOutcome: 3s).
var (
	preCancelMu    sync.Mutex
	preCancelHooks []func()
)

// RegisterPreCancelHook appends fn to the list of callbacks NewCtx's
// signal handler runs synchronously before cancel(). Safe to call
// from any goroutine at any time after NewCtx; hooks registered after
// the signal has fired will not run.
func RegisterPreCancelHook(fn func()) {
	if fn == nil {
		return
	}
	preCancelMu.Lock()
	preCancelHooks = append(preCancelHooks, fn)
	preCancelMu.Unlock()
}

// signalHold, when set, is what the first stop signal waits on before it
// cancels the root context (SetSignalHold).
var signalHold atomic.Pointer[func(stopNow <-chan struct{})]

// SetSignalHold has the first SIGINT, SIGTERM or SIGHUP NewCtx receives wait,
// before it cancels the root context, until hold returns: what the process is
// still doing for someone else finishes first. A second signal while it waits
// closes stopNow, and hold must then return at once; that signal is counted
// too (RepeatInterrupts). nil removes the hold.
//
// The keploy agent holds so: a recording's client (the CLI) owns its
// lifetime, and the same signal often reaches both (`pkill -INT keploy`, a
// container stop). Cancelled at once, the agent closed the streams the client
// was still draining, and what its parsers still held from before the stop
// was lost with it.
func SetSignalHold(hold func(stopNow <-chan struct{})) {
	if hold == nil {
		signalHold.Store(nil)
		return
	}
	signalHold.Store(&hold)
}

// interrupted records that a SIGNAL ended this process, as opposed to the CLI
// cancelling its own root context during ordinary teardown.
//
// The two are the same context. `keploy mock record|replay` calls ExecCancel()
// from a defer once the run is over -- that is how its background goroutines
// are torn down -- so by the time the root command returns, ctx.Err() is
// non-nil on every mock run, successful or not. A caller that read ctx.Err()
// as "a signal ended this" therefore reported every recording the VS Code
// extension made as interrupted, and the panel told the user their recording
// had been cut short over a recording that had just succeeded.
var interrupted atomic.Bool

// interruptSignal is the number of the signal NewCtx's handler stopped this run
// on (syscall.SIGHUP=1, SIGINT=2, SIGTERM=15), or 0 when no signal did. It lets
// a replay exit 128+N on a signal kill, so a run cut short before it finished
// verifying is not read as a pass.
var interruptSignal atomic.Int32

// Interrupted reports whether NewCtx's handler stopped this run on a signal:
// SIGINT, SIGTERM, or SIGHUP where NewCtx listens for it (see notifyHangups
// and DieOnHangup).
func Interrupted() bool { return interrupted.Load() }

// InterruptSignalNumber returns the number of the signal NewCtx's handler
// stopped this run on, or 0 when no signal did.
func InterruptSignalNumber() int { return int(interruptSignal.Load()) }

// InterruptExitCode is the 128+N process exit code for the signal that stopped
// this run (SIGHUP→129, SIGINT→130, SIGTERM→143), or 0 when no signal did. A
// replay uses it so a run killed before it finished verifying exits non-zero,
// the shell's own convention for "terminated by signal N", rather than 0 — which
// a CI pipeline reads as a pass.
func InterruptExitCode() int {
	n := int(interruptSignal.Load())
	if n <= 0 {
		return 0
	}
	return 128 + n
}

// MarkInterrupted records that a signal ended this run. The signal handler
// below calls it; a build that installs its own handler, and a test that
// simulates one, call it instead of cancelling a context and hoping the two
// are read as the same thing.
func MarkInterrupted() { interrupted.Store(true) }

// ClearInterrupted is for tests, which share one process across cases.
func ClearInterrupted() { interrupted.Store(false); interruptSignal.Store(0) }

// interruptLatch is a channel closed once.
type interruptLatch struct {
	once sync.Once
	ch   chan struct{}
}

// stopNow is closed by the interrupt that asks a stop still finishing its work
// (a recording draining what the agent captured) to give that up: the first
// that comes while the stop offers to end at one (OfferStopNow). StopNow
// returns it.
var stopNow atomic.Pointer[interruptLatch]

func init() { ResetInterruptedAgain() }

// OfferStopNow says the stop under way has just told the user that one more
// interrupt ends it at once ("press Ctrl+C again to stop now"): while the
// offer stands, until withdraw is called, the next interrupt does (StopNow).
// An interrupt before it is not the user's answer to it, whatever it was. A
// single stop can arrive twice: a signal sent to both sudo and the keploy it
// runs -- `sudo pkill -f keploy`, as CI stops a recording -- reaches keploy
// once directly and once from sudo, which hands its own copy on, and the copy
// comes as the first does, before keploy has said anything. Taken for a second
// Ctrl+C, it gave a recording's drain up at its start, and what the agent
// still held was lost.
//
// So an offer is made only once the stop's first signal has come (Interrupted):
// before it, the copy of that signal would find the offer standing; and a
// whole notice period after that signal was seen (StopNowNotice), whether the
// signal began the stop or came while a stop that began on its own (the app
// exits, a record timer) waited. The copy comes with the first, not a period
// later -- unless sudo is held from handing it on (starved, or stopped), which
// no time can tell from a press, as Go gives a signal's sender to no one. So
// what a press after an offer does must be safe for a copy too: a drain that
// gives up fails its run loudly, and an agent's stop answers a first offer by
// having the agent die between teardown steps (as at a stall verdict), never
// in one.
//
// next is closed by the first interrupt after this offer: a wait that must
// hear only an interrupt given to it (an agent's stop, after the drains, where
// a press that ended a drain must not kill the agent mid-teardown) selects on
// it, not on StopNow. withdraw is called once the stop no longer waits, so the
// offer does not stand for a later wait it did not tell the user of. With no
// signal yet, nothing is offered: next is nil.
func OfferStopNow() (next <-chan struct{}, withdraw func()) {
	if !Interrupted() {
		return nil, func() {}
	}
	repeatInterrupts.mu.Lock()
	repeatInterrupts.offers++
	next = repeatInterrupts.next
	repeatInterrupts.mu.Unlock()
	var once sync.Once
	return next, func() {
		once.Do(func() {
			repeatInterrupts.mu.Lock()
			repeatInterrupts.offers--
			repeatInterrupts.mu.Unlock()
		})
	}
}

// StopNow returns a channel closed by the first interrupt that arrives while a
// stop offers to end at one (OfferStopNow): the user asking a stop that said
// one would end it at once to do so. Work a stop waits on for as long as it
// makes progress (a recording's drain) selects on it, and offers it once it
// has told the user, so the user can always end it.
func StopNow() <-chan struct{} { return stopNow.Load().ch }

// StopNowNotice paces a stop's offer to end at the user's next interrupt
// (OfferStopNow) to the notices it gives while it waits: the offer is made at
// the first notice that comes a whole notice period (as measured, not as
// scheduled) after the stop's first signal was seen -- as the stop began, or
// at the first notice to find it. The copy of that signal, which sudo hands
// on, comes with it, not a period later; a notice read late (its loop was
// busy) does not bring the offer forward. Not safe for concurrent use: the one
// loop that gives the notices owns it.
type StopNowNotice struct {
	period time.Duration
	// sawAt is when the stop's first signal was first seen to have come
	// (zero: not yet).
	sawAt time.Time
	// next and withdraw are the offer's, once made (OfferStopNow).
	next     <-chan struct{}
	withdraw func()
}

// noticeClock is StopNowNotice's clock (nil: time.Now): a test's to set
// (SetNoticeClockForTest), while notices are given on other goroutines.
var noticeClock atomic.Pointer[func() time.Time]

func noticeNow() time.Time {
	if c := noticeClock.Load(); c != nil {
		return (*c)()
	}
	return time.Now()
}

// SetNoticeClockForTest has StopNowNotice read the time from now, and returns
// what puts its own clock back. Tests only.
func SetNoticeClockForTest(now func() time.Time) (restore func()) {
	old := noticeClock.Swap(&now)
	return func() { noticeClock.Store(old) }
}

// NewStopNowNotice starts the notices, every period, of a stop that begins
// now. Call it before the notices' clock starts.
func NewStopNowNotice(period time.Duration) *StopNowNotice {
	n := &StopNowNotice{period: period}
	if Interrupted() {
		n.sawAt = noticeNow()
	}
	return n
}

// Next is called at each notice. It reports whether the notice says that
// another interrupt stops it now: from this notice on (it makes the offer), or
// since an earlier one.
func (n *StopNowNotice) Next() (offering bool) {
	switch {
	case n.withdraw != nil:
	case n.sawAt.IsZero():
		// The flag before the clock: a signal that comes between the two is
		// then seen at the next notice, never stamped before it came.
		if Interrupted() {
			n.sawAt = noticeNow()
		}
	case noticeNow().Sub(n.sawAt) >= n.period:
		n.next, n.withdraw = OfferStopNow()
	}
	return n.withdraw != nil
}

// Interrupt returns a channel closed by the first interrupt after the offer:
// nil, which never is, until the offer is made.
func (n *StopNowNotice) Interrupt() <-chan struct{} { return n.next }

// Withdraw withdraws the offer, if one was made: the stop no longer waits.
func (n *StopNowNotice) Withdraw() {
	if n.withdraw != nil {
		n.withdraw()
	}
}

// MarkInterruptedAgain is what every interrupt after the first does: it is
// counted (RepeatInterrupts, InterruptedAfter), and, while a stop offers to
// end at one (OfferStopNow), it ends that stop (StopNow). It reports whether
// it did. For tests too.
func MarkInterruptedAgain() (stoppedNow bool) { return noteRepeatInterrupt(nil) }

// noteRepeatInterrupt is MarkInterruptedAgain. say, when set, is told whether
// the interrupt ends a stop, and is called before anything that waits on the
// interrupt is woken: what is said of an interrupt comes before what it does.
func noteRepeatInterrupt(say func(stoppedNow bool)) (stoppedNow bool) {
	repeatInterrupts.mu.Lock()
	defer repeatInterrupts.mu.Unlock()
	stoppedNow = repeatInterrupts.offers > 0
	if say != nil {
		say(stoppedNow)
	}
	repeatInterrupts.n++
	close(repeatInterrupts.next)
	repeatInterrupts.next = make(chan struct{})
	if stoppedNow {
		l := stopNow.Load()
		l.once.Do(func() { close(l.ch) })
	}
	return stoppedNow
}

// repeatInterrupts counts the interrupts after the first
// (MarkInterruptedAgain); next is closed, and replaced, at each.
var repeatInterrupts struct {
	mu   sync.Mutex
	n    uint64
	next chan struct{}
	// offers is how many stops offer to end at the next one (OfferStopNow).
	offers int
}

// RepeatInterrupts returns how many interrupts have arrived after the one
// that stopped the run.
func RepeatInterrupts() uint64 {
	repeatInterrupts.mu.Lock()
	defer repeatInterrupts.mu.Unlock()
	return repeatInterrupts.n
}

// InterruptedAfter returns a channel closed once more than n interrupts have
// arrived after the one that stopped the run (RepeatInterrupts). A stop that
// takes RepeatInterrupts() as it begins selects on it to hear an interrupt
// given while it runs, and not one that came before it -- a single stop
// delivered twice among them: `sudo pkill -f keploy` signals both sudo and
// keploy, and sudo hands its signal on to keploy too.
func InterruptedAfter(n uint64) <-chan struct{} {
	repeatInterrupts.mu.Lock()
	defer repeatInterrupts.mu.Unlock()
	if repeatInterrupts.n > n {
		c := make(chan struct{})
		close(c)
		return c
	}
	return repeatInterrupts.next
}

// ResetInterruptedAgain is for tests, which share one process across cases.
func ResetInterruptedAgain() {
	repeatInterrupts.mu.Lock()
	stopNow.Store(&interruptLatch{ch: make(chan struct{})})
	repeatInterrupts.n = 0
	repeatInterrupts.next = make(chan struct{})
	repeatInterrupts.offers = 0
	repeatInterrupts.mu.Unlock()
}

func NewCtx() context.Context {
	// Create a context that can be canceled
	ctx, cancel := context.WithCancel(context.Background())

	SetCancel(cancel)
	// Set up a channel to listen for signals
	sigs := make(chan os.Signal, 1)
	// os.Interrupt is more portable than syscall.SIGINT
	// there is no equivalent for syscall.SIGTERM in os.Signal
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	hangups := notifyHangups()

	// Start a goroutine that will cancel the context when a signal is received
	go func() {
		var sig os.Signal // this received signal will be inside keploy docker container if running in docker else on the host.
		select {
		case sig = <-sigs:
		case sig = <-hangups:
		}
		MarkInterrupted()
		if ss, ok := sig.(syscall.Signal); ok {
			interruptSignal.Store(int32(ss))
		}
		fmt.Printf("Signal received: %s, canceling context...\n", sig)

		// Where NewCtx listens for SIGHUP (not under nohup: see
		// notifyHangups), every SIGHUP from here on changes nothing, and
		// says so, unless DieOnHangup has given SIGHUP its default action
		// back. One hang-up can deliver two. VS Code's Stop sends one to
		// the process on the task's terminal and then closes the terminal,
		// which has the kernel send another to its session leader: both
		// reach keploy when the shell ran it in its own place, as `zsh -c`
		// does. Closing the window of an interactive shell has the shell
		// forward one to its job and the kernel send the job another as the
		// shell exits. SIGHUP stays registered, so none of them is the
		// default action -- death, half way through the shutdown the first
		// one started.
		if hangups != nil {
			go func() {
				for hup := range hangups {
					fmt.Printf("Signal received: %s, ignored: Keploy is already handling %s\n", hup, sig)
				}
			}()
		}

		// App-managed graceful-shutdown drain (Kubernetes sidecar path).
		// When the injecting webhook runs in app-managed-drain mode it sets
		// KEPLOY_SIDECAR_DRAIN_SECONDS on the agent instead of a native
		// `sleep` lifecycle (SleepAction) preStop hook — that field is GA
		// only in k8s 1.30 and some older apiservers reject it, which would
		// fail pod admission. We honour the drain HERE, before cancel(): the
		// proxy and every parser goroutine are still live (ctx not yet
		// cancelled), so in-flight streams keep completing for the window —
		// exactly what a preStop sleep bought — and only then do we tear
		// down.
		//
		// The drain window is RESPECTED: additional SIGTERM/SIGINT signals
		// that arrive while draining are logged but do NOT cut it short.
		// The kubelet sends exactly one SIGTERM and never a second one — it
		// escalates to SIGKILL at terminationGracePeriodSeconds, which is
		// uncatchable and is the real hard stop (so keep the pod's
		// terminationGracePeriodSeconds >= this drain; the webhook bumps it
		// to 45s for a 15s drain). A repeat `kubectl delete` (the usual
		// source of a second SIGTERM) therefore won't truncate an in-flight
		// drain; an operator who truly wants an immediate kill uses
		// `kubectl delete --grace-period=0 --force`. Unset / zero (the
		// default, and every non-sidecar invocation) skips the wait
		// entirely, preserving the historical immediate-cancel behaviour.
		if d := sidecarDrainDuration(); d > 0 {
			fmt.Printf("Draining in-flight connections for %s before shutdown (KEPLOY_SIDECAR_DRAIN_SECONDS)...\n", d)
			drainTimer := time.NewTimer(d)
			for draining := true; draining; {
				select {
				case <-drainTimer.C:
					draining = false
				case sig2 := <-sigs:
					fmt.Printf("Signal %s received during shutdown drain; ignoring to honour the %s drain window "+
						"(SIGKILL at terminationGracePeriodSeconds is the hard stop; use "+
						"`kubectl delete --grace-period=0 --force` to force an immediate kill).\n", sig2, d)
				}
			}
			drainTimer.Stop()
			fmt.Printf("Drain window elapsed; proceeding with shutdown.\n")
		}

		// What the process still does for someone else (SetSignalHold)
		// finishes before the cancel; a second signal cuts it short.
		if h := signalHold.Load(); h != nil {
			stopNow := make(chan struct{})
			held := make(chan struct{})
			go func() {
				defer close(held)
				(*h)(stopNow)
			}()
			select {
			case <-held:
			case sig2 := <-sigs:
				fmt.Printf("Signal received again: %s, stopping now; what Keploy was still saving is not saved\n", sig2)
				MarkInterruptedAgain()
				close(stopNow)
				<-held
			}
		}

		// After the hold, before the hooks and the cancel: see
		// IgnoreHangupOnceStopping.
		absorbHangupsIfStopping()

		// Run pre-cancel hooks SYNCHRONOUSLY while live state is still
		// readable. fmt.Fprintf to stderr inside hooks is the right
		// choice — it goes straight to the syscall and survives even
		// if the process is SIGKILL'd microseconds later (zap's async
		// logger does not survive that race). See RegisterPreCancelHook
		// doc comment for the investigation context.
		preCancelMu.Lock()
		hooks := append([]func(){}, preCancelHooks...)
		preCancelMu.Unlock()
		for _, fn := range hooks {
			func() {
				defer func() {
					_ = recover()
				}()
				fn()
			}()
		}

		cancel()

		// The stop can take a while: a recording drains what the agent
		// captured before it, for as long as that keeps coming. Once it has
		// told the user that another interrupt gives that up (OfferStopNow),
		// the next one does (StopNow); without this, SIGINT stayed registered
		// and unread, so Ctrl+C did nothing and only SIGKILL, which skips all
		// cleanup, could end the process. One before that ends nothing: it
		// may be the first one again (OfferStopNow).
		// The line claims nothing: what a repeat does, the stop that acts on
		// it says itself (a recording's drain: what the agent still held is
		// not saved; an agent's stop: how it stops the agent), and one before
		// any offer does nothing. It is said before anything waiting on the
		// repeat is woken, so it comes before what the repeat makes happen.
		for sig2 := range sigs {
			noteRepeatInterrupt(func(bool) {
				fmt.Printf("Signal received again: %s\n", sig2)
			})
		}
	}()

	return ctx
}

// notifyHangups has a hang-up stop keploy as SIGTERM does: NewCtx takes the
// same path for it -- marks the run interrupted, runs the pre-cancel hooks and
// cancels the root context -- so the command stops what it started and
// reports the run as it reports one ended by SIGTERM, and main's deferred
// cleanup runs. It returns the channel SIGHUP arrives on, or nil, on which
// nothing ever arrives, when SIGHUP was ignored as this process started:
// `nohup keploy record ...` asked to outlive its terminal, and a Notify would
// undo that.
//
// A hang-up is how the VS Code extension's Stop ends a run: it terminates
// the task, which sends the process on the task's terminal SIGHUP (node-pty's
// kill()). Closing a terminal, or losing an ssh session, sends the same.
// keploy listened for SIGINT and SIGTERM only, so SIGHUP killed it where it
// stood: the test command it had started ran on, and none of the cleanup it
// defers ran.
func notifyHangups() chan os.Signal {
	if signal.Ignored(syscall.SIGHUP) {
		return nil
	}
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	return hangups
}

// DieOnHangup gives SIGHUP back the action it had before NewCtx heard it:
// death, or nothing if it was ignored as this process started. It is for a
// process whose owner counts on a hang-up killing it: `keploy agent`, whose
// terminal goes away with the user's (the CLI's PTY, an ssh session), and
// whose enterprise self-update supervisor hands it the SIGHUPs it gets and
// counts a death by one as a deliberate stop. A SIGHUP that arrives between NewCtx and
// this call takes NewCtx's path, and stops the process gracefully instead.
// Once the process is shutting down, IgnoreHangupOnceStopping takes over.
func DieOnHangup() {
	signal.Reset(syscall.SIGHUP)
}

// IgnoreHangupOnceStopping has a hang-up stop killing the process once its
// shutdown has begun: the process finishes it however its terminal goes away.
// It is for `keploy agent`, whose shutdown removes what it attached to the
// kernel: a hang-up that killed it part way through removing a TLS uprobe
// leaves the breakpoint in every process that maps the probed library ("failed
// to unregister, leaking uprobe"), and loses what it was still saving. The CLI
// that started an agent on a PTY no longer closes that terminal to stop a
// running agent, but its own exit closes it, and so does a closed window or a
// dropped ssh session. Before the shutdown a hang-up still kills (DieOnHangup).
//
// The switch is made by the root context's own cancel -- NewCtx's signal
// handler and ExecCancel (Stop) -- just BEFORE it cancels, so nothing that
// runs on the cancellation can be cut off by a hang-up arriving as it starts.
// In NewCtx that is after the signal hold (SetSignalHold): while the agent
// only waits for its recording client, nothing has been torn down yet, and a
// hang-up still kills it. For a context ctx that ends some other way, the
// switch is made when ctx is done.
//
// SIGHUP is ignored (SIG_IGN), which a program the agent starts from then on
// inherits: the CA-restore tools its shutdown runs (keytool, certutil,
// update-ca-certificates), which share its process group and terminal, finish
// too instead of dying of the same hang-up part way through rewriting a trust
// store. Programs started before the shutdown keep the default action.
func IgnoreHangupOnceStopping(ctx context.Context) {
	stoppingAbsorbsHangups.Store(true)
	go func() {
		<-ctx.Done()
		absorbHangupsIfStopping()
	}()
}

// stoppingAbsorbsHangups is set by IgnoreHangupOnceStopping; hangupsAbsorbed
// once absorbHangupsIfStopping has made the switch.
var (
	stoppingAbsorbsHangups atomic.Bool
	hangupsAbsorbed        atomic.Bool
	absorbHangupsOnce      sync.Once
)

// absorbHangupsIfStopping ignores every SIGHUP from now on, when
// IgnoreHangupOnceStopping asked for it.
func absorbHangupsIfStopping() {
	if !stoppingAbsorbsHangups.Load() {
		return
	}
	absorbHangupsOnce.Do(func() {
		signal.Ignore(syscall.SIGHUP)
		hangupsAbsorbed.Store(true)
	})
}

// Stop requires a reason to stop the server.
// this is to ensure that the server is not stopped accidentally.
// and to trace back the stopper
func Stop(logger *zap.Logger, reason string) error {
	// Stop the server.
	if logger == nil {
		return errors.New("logger is not set")
	}
	if cancel == nil {
		err := errors.New("cancel function is not set")
		LogError(logger, err, "failed stopping keploy")
		return err
	}

	if reason == "" {
		err := errors.New("cannot stop keploy without a reason")
		LogError(logger, err, "failed stopping keploy")
		return err
	}

	logger.Info("stopping Keploy", zap.String("reason", reason))
	ExecCancel()
	return nil
}

func ExecCancel() {
	absorbHangupsIfStopping() // before the cancel: see IgnoreHangupOnceStopping
	cancel()
}

func SetCancel(c context.CancelFunc) {
	cancel = c
}

// sidecarDrainDuration returns the graceful-shutdown drain window the
// Kubernetes injecting webhook sets, in app-managed-drain mode, via the
// KEPLOY_SIDECAR_DRAIN_SECONDS env var. It is the in-process replacement for
// a native `sleep` lifecycle preStop hook on the keploy-agent sidecar.
//
// Returns 0 (no wait) when the var is unset, non-numeric, or non-positive —
// so only an explicit positive value can delay shutdown, and every
// non-sidecar / older-deployment invocation keeps the historical
// cancel-immediately-on-signal behaviour.
func sidecarDrainDuration() time.Duration {
	v := os.Getenv("KEPLOY_SIDECAR_DRAIN_SECONDS")
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
