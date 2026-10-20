package record

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// countReadClock is the clock a record watch's reads of the agent's count are
// bounded on (countReads): nil, keploy's own runnable time. Tests set one
// they drive.
var countReadClock stall.Clock

// countReadEvery is how often a wait for those reads looks again, and so how
// soon it says it waits (countReads.every). Tests shorten it.
var countReadEvery = drainProgressEvery

// countReads are a record watch's reads of the agent's count of what it could
// not capture (CaptureLossReader): the CA watch's with the proven switch off
// (caRejectWatch), and captureWatch's with it on. Every read but a watch's
// base read is asked for, not waited for, and made on a goroutine of its own,
// one at a time, in the order asked (so an answer is never applied after a
// later one): a test case's as it is saved, and the final one as the stop
// ends the capture. A read that lags counts every test case saved since the
// last together. The reads are not ended by the stop, which ends record's
// context before the drain saves its last test cases and before the final
// read is asked (a compose agent that goes on its own has both run with no
// stop at all); the interrupt that gave the stop's waits up (utils.StopNow)
// ends them. Every read is given up once keploy had stall.HangBudget of its
// own runnable time to run without an answer, whether or not anyone is there
// to press Ctrl+C, so a stop no one presses Ctrl+C at (CI's pkill -INT) ends
// too, and a starved keploy is not taken for a silent agent.
//
// While a drain may still save what the agent held, the reads stand no offer
// of the stop's (utils.OfferStopNow): a press at it would close
// utils.StopNow, which the drains give up at. Once the frame drains are over
// the stop waits for the reads still out before it lets the agent go
// (waitForReads), saying so; it offers nothing and hears no press of its own
// (it ends as the reads do: waitForReads says why). The watch's finish, after
// every drain, waits for the rest, and there offers that one more Ctrl+C
// stops the wait now (wait).
type countReads struct {
	logger *zap.Logger
	reader CaptureLossReader
	ctx    context.Context // record's: each read leaves its cancel out (stopProof)
	// stall bounds each read in time keploy could run (stall.HangBudget), on
	// clock (nil: keploy's own runnable time).
	stall time.Duration
	clock stall.Clock
	// every is how often a wait for the reads looks again (countReadEvery):
	// it says it waits once, and again when its offer is made.
	every time.Duration
	// silent is why a read was given up on its bound.
	silent error
	// waiting is what a wait says the agent is yet to say ("how many ...");
	// ifStopped, what a press at finish's offer leaves unsaid.
	waiting   string
	ifStopped string
	// apply takes each read's answer, mu held: the test cases it counts,
	// whether it was made once the final read had been asked, and the count
	// or why there is none. It is not called once the watch has finished.
	apply func(countRead)

	// mu is the watch's own lock: apply is called with it held, and the
	// watch's finish holds it to read what is left unread.
	mu *sync.Mutex
	// wanted is how many reads were asked for since the read goroutine last
	// took them; reading, whether that goroutine runs; idle is closed once it
	// has ended. finished: the watch's finish has said what it had to;
	// nothing more is read or counted.
	wanted   int
	reading  bool
	idle     chan struct{}
	finished bool
	// finalAsked: the final read has been asked for; covered: a read made
	// since then answered, so every call the app made before the stop is in
	// what the reads counted, whichever test case's they are.
	finalAsked bool
	covered    bool
	// pending is the test cases saved since the last read took them, and
	// inflight those the read out counts.
	pending  []string
	inflight []string
}

// countRead is one read's answer (countReads.apply).
type countRead struct {
	// names are the test cases it counts: those saved since the read before
	// took its own (and those a watch left to it).
	names []string
	// afterFinal: it was made once the final read had been asked.
	afterFinal bool
	losses     models.CaptureLosses
	err        error
}

// newCountReads is the reads of a watch whose state mu guards; apply takes
// each answer (countReads.apply).
func newCountReads(ctx context.Context, logger *zap.Logger, reader CaptureLossReader, mu *sync.Mutex, silent error, waiting, ifStopped string, apply func(countRead)) *countReads {
	return &countReads{
		logger: logger, reader: reader, ctx: ctx,
		stall: stall.HangBudget, clock: countReadClock, every: countReadEvery,
		silent: silent, waiting: waiting, ifStopped: ifStopped, apply: apply, mu: mu,
	}
}

// stopProof is a context the stop does not end, as ctx is: only its own
// cancel, and the interrupt that gives the stop's waits up (utils.StopNow),
// end it.
func stopProof(ctx context.Context) (context.Context, context.CancelFunc) {
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		select {
		case <-utils.StopNow():
			cancelRead()
		case <-readCtx.Done():
		}
	}()
	return readCtx, cancelRead
}

// boundedRead reads the agent's count on ctx, which ends it, given up (silent)
// once keploy had r.stall of its own runnable time to run without an answer:
// a starved keploy is not taken for a silent agent.
func (r *countReads) boundedRead(ctx context.Context) (models.CaptureLosses, error) {
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	type answer struct {
		losses models.CaptureLosses
		err    error
	}
	got := make(chan answer, 1) // a read that outlives its bound does not block
	go func() {
		losses, err := r.reader.GetCaptureLosses(readCtx)
		got <- answer{losses, err}
	}()
	m := stall.NewMeter(r.clock, r.stall)
	defer m.Stop()
	for {
		select {
		case a := <-got:
			return a.losses, a.err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.Wait():
			if m.Expired() {
				return nil, r.silent
			}
		}
	}
}

// readNow makes one read and waits for it, as the stop's wait for the reads
// does (waitForReads): not ended by the stop, given up on its bound, and said
// at Info, with that bound, once it has waited r.every. A watch's base read,
// made before any other is asked.
func (r *countReads) readNow() (models.CaptureLosses, error) {
	type answer struct {
		losses models.CaptureLosses
		err    error
	}
	got := make(chan answer, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		readCtx, cancelRead := stopProof(r.ctx)
		defer cancelRead()
		losses, err := r.boundedRead(readCtx)
		got <- answer{losses, err}
	}()
	r.waitFor(done, false)
	a := <-got
	return a.losses, a.err
}

// askTest asks for a read that counts test case name, just saved (and those
// saved since the last read, if the reads lag). It does not wait for the
// read.
func (r *countReads) askTest(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.pending = append(r.pending, name)
	r.askLocked()
}

// askFinal asks for the final read, as the stop ends the capture, while the
// agent still is up. It does not wait for it.
func (r *countReads) askFinal() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.finalAsked = true
	r.askLocked()
}

// askLocked asks for one more read, starting the read goroutine if it is not
// running. r.mu is held.
func (r *countReads) askLocked() {
	r.wanted++
	if r.reading {
		return
	}
	r.reading = true
	r.idle = make(chan struct{})
	go r.readAll()
}

// readAll makes the reads asked for, one at a time, until none is wanted.
func (r *countReads) readAll() {
	for {
		r.mu.Lock()
		if r.wanted == 0 || r.finished {
			r.reading = false
			close(r.idle)
			r.mu.Unlock()
			return
		}
		r.wanted = 0
		names := r.pending
		r.pending = nil
		r.inflight = names
		afterFinal := r.finalAsked
		r.mu.Unlock()

		readCtx, cancelRead := stopProof(r.ctx)
		losses, err := r.boundedRead(readCtx)
		cancelRead()

		r.mu.Lock()
		r.inflight = nil // finish, if it came first, has taken them
		if !r.finished {
			if err == nil && afterFinal {
				r.covered = true
			}
			r.apply(countRead{names: names, afterFinal: afterFinal, losses: losses, err: err})
		}
		r.mu.Unlock()
	}
}

// unreadLocked is, once the watch has finished (r.mu held), the test cases
// whose reads did not come: a wait the user stopped, or a read asked as
// finish began (a consumer of a record drain that gave up). Those a watch
// left to a later read are among them.
func (r *countReads) unreadLocked() []string {
	return append(append([]string(nil), r.inflight...), r.pending...)
}

// waitForReads waits for the reads asked for so far (and those asked while
// it waits), once the frame drains are over, before the stop lets the agent
// go: once it has looked (every r.every) and found a read still out, it says
// that it waits, and each read's bound. No interrupt ends it. It stands no
// offer of the stop's (a record drain may still select on utils.StopNow),
// and it hears no press of its own: nothing keploy can see tells the user's
// next Ctrl+C from the copy of the stop's own signal that sudo hands on (a
// signal a user process sends to sudo as well as to keploy, as `sudo pkill
// -INT -f keploy` or a kill to the process group does, reaches keploy
// directly and again through sudo, and Go gives no signal's sender to its
// handler), and only time could, which a loaded host may not keep. It ends
// as the reads do: answered, failed, given up on their bound (stall.HangBudget
// of keploy's own runnable time), or ended by a utils.StopNow that a press
// at an earlier drain's offer closed.
func (r *countReads) waitForReads() {
	if r != nil {
		r.wait(false)
	}
}

// wait waits until no read is out, looking every r.every, and says once that
// it waits (and again when its offer is made).
// With offer (finish, after every drain), once it has said so, one more
// Ctrl+C stops it now (utils.StopNowNotice), and ends the watch's reads
// (utils.StopNow), each at once, with the press as its answer, which it
// waits for: what they would have counted is not said (ifStopped); a copy of
// the stop's signal taken for that press costs no more than that, as every
// offer of the stop's must (utils.OfferStopNow). Without offer, it ends as
// the reads do (waitForReads).
// It reports whether it waited for all of them to be answered.
func (r *countReads) wait(offer bool) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	if !r.reading {
		r.mu.Unlock()
		return true
	}
	idle := r.idle
	r.mu.Unlock()
	return r.waitFor(idle, offer)
}

// waitFor waits for done, as wait does.
func (r *countReads) waitFor(done <-chan struct{}, offer bool) bool {
	var notice *utils.StopNowNotice
	if offer {
		notice = utils.NewStopNowNotice(r.every)
		defer notice.Withdraw()
	}
	tick := time.NewTicker(r.every)
	defer tick.Stop()
	said, offering := false, false
	for {
		var interrupt <-chan struct{}
		if notice != nil {
			interrupt = notice.Interrupt()
		}
		select {
		case <-done:
			return true
		case <-interrupt:
			// The press closed utils.StopNow before it woke this
			// (utils.MarkInterruptedAgain), which ends the read out, and
			// every one asked after it, at once: each is taken as the press
			// left it before the wait ends, whichever goroutine runs first.
			<-done
			return false
		case <-tick.C:
		}
		wasOffering := offering
		if notice != nil {
			offering = notice.Next()
		}
		if !said || offering != wasOffering {
			msg := "Still waiting for the keploy agent to say " + r.waiting
			switch {
			case offering:
				msg += "; press Ctrl+C again to stop now: " + r.ifStopped
			case notice == nil:
				msg += fmt.Sprintf("; Ctrl+C does not cut this short: each read is given up once keploy has had %s of its own time to run without an answer", r.stall)
			}
			r.logger.Info(msg)
			said = true
		}
	}
}
