package record

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// caDocs is where keploy says how a client that pins its own CA trusts
// keploy's, natively and in Docker.
const caDocs = "https://keploy.io/docs/running-keploy/docker-tls/#a-client-that-pins-its-own-ca"

// errCountSilent is why a read of the agent's count was given up: keploy had
// the whole of caRejectWatch.stall to run, and the agent did not answer.
var errCountSilent = errors.New("the keploy agent did not answer its count of failed TLS handshakes")

// caReadClock is the clock the watch's reads are bounded on: nil, keploy's
// own runnable time. Tests set one they drive.
var caReadClock stall.Clock

// caRejectWatch is, with the proven switch off, the one thing keploy record
// says of the TLS handshakes its proxy saw fail: a client that rejected the
// certificate keploy answered with (it does not trust keploy's CA: a
// client that pins its own CA file, one whose runtime reads only the OS
// trust store) made a call keploy neither recorded nor can replay, and the
// recording otherwise ends as if it held everything. It names the test
// cases that were recorded while such a handshake failed, and how many failed
// after the last one, in one WARN when the recording ends. It counts only
// failed handshakes (models.CaptureLossTLSAbort); one a client hung up on
// (tls_handshake_abandoned) cannot be told from a client that gave up, and
// is left to the proven switch. A count it cannot read is left unsaid: it is
// a diagnostic, and says nothing it did not see.
//
// The base read is made as capture begins, on record's context, and waited
// for: a stop during it ends it, and the watch is then off for the
// recording. Every later read is asked for, not waited for, and made on a
// goroutine of the watch's, one at a time, in the order asked (so an answer
// is never applied after a later one): a test case's as it is saved, and
// the final one as the stop ends the capture (once the app has stopped, or,
// in a docker compose stop with the switch off, once the drain is over,
// the app still running). Those are not ended by the stop, which ends
// record's context before the drain saves its last test cases and before
// the final read is asked (a compose agent that goes on its own has both run
// with no stop at all); the interrupt that gave the stop's waits up
// (utils.StopNow) ends them. Every read, the base one too, is given up once
// keploy had stall.HangBudget of its own runnable time to run without an
// answer, whether or not anyone is there to press Ctrl+C.
//
// While a drain may still save what the agent held, the watch stands no
// offer of the stop's (utils.OfferStopNow): a press at it would close
// utils.StopNow, which the drains give up at. Once the frame drains are over
// the stop waits for the reads still out before it lets the agent go
// (waitForReads), saying so; it offers nothing and hears no press (it ends
// as the reads do: waitForReads says why). finish, after every
// drain, waits for the rest, and there offers that one more Ctrl+C stops the
// wait now.
type caRejectWatch struct {
	logger *zap.Logger
	reader CaptureLossReader
	ctx    context.Context // record's: each later read leaves its cancel out (stopProof)
	// stall bounds each read in time keploy could run (stall.HangBudget), on
	// clock (nil: keploy's own runnable time).
	stall time.Duration
	clock stall.Clock
	// every is how often a wait for the reads looks again (drainProgressEvery):
	// it says it waits once, and again when its offer is made.
	every time.Duration

	mu sync.Mutex
	// wanted is how many reads were asked for since the read goroutine last
	// took them; reading, whether that goroutine runs; idle is closed once it
	// has ended. finished: finish has said what it had to; nothing more is
	// read or counted.
	wanted   int
	reading  bool
	idle     chan struct{}
	finished bool
	// finalAsked: the final read has been asked for; covered: a read made
	// since then answered, so every call the app made before the stop is in
	// what the reads counted, whichever test case's they are (finish).
	finalAsked bool
	covered    bool
	// pending is the test cases saved since the last read that counted them,
	// and inflight those the read out counts.
	pending  []string
	inflight []string

	last   uint64 // failed handshakes counted at the last read that counted test cases
	during uint64 // failed while test cases were recorded
	tests  []string
	to     string // the destination of the last failed handshake a test case's read saw
	// tail is what the last read that came with no test case to count saw,
	// when no read that counted test cases came after it (finish). A read
	// that counts test cases counts from last, which tail leaves as it was, so
	// a test case the drain saves after the final read (a native or docker
	// stop's order) is counted and named, when its read comes.
	tail struct {
		read bool
		n    uint64
		to   string
	}
}

// newCARejectWatch marks the agent's count of failed handshakes as capture
// begins; nil when it cannot be read.
func newCARejectWatch(ctx context.Context, logger *zap.Logger, reader CaptureLossReader) *caRejectWatch {
	c := &caRejectWatch{logger: logger, reader: reader, ctx: ctx, stall: stall.HangBudget, clock: caReadClock, every: drainProgressEvery}
	n, _, err := c.boundedRead(ctx)
	if err != nil {
		logger.Debug("keploy cannot read its agent's count of failed TLS handshakes; it will not say which test cases a client that rejected keploy's certificate was recorded in", zap.Error(err))
		return nil
	}
	c.last = n
	return c
}

// failedHandshakes is losses' count of client-side TLS handshakes that
// failed other than by the client hanging up, and the last one's
// destination.
func failedHandshakes(losses models.CaptureLosses) (uint64, string) {
	for _, c := range losses {
		if c.Kind == models.CaptureLossTLSAbort {
			return c.Count, c.What
		}
	}
	return 0, ""
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

// boundedRead reads the agent's count (failedHandshakes) on ctx, which ends
// it, given up once keploy had c.stall of its own runnable time to run
// without an answer: a starved keploy is not taken for a silent agent.
func (c *caRejectWatch) boundedRead(ctx context.Context) (uint64, string, error) {
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	type answer struct {
		losses models.CaptureLosses
		err    error
	}
	got := make(chan answer, 1) // a read that outlives its bound does not block
	go func() {
		losses, err := c.reader.GetCaptureLosses(readCtx)
		got <- answer{losses, err}
	}()
	m := stall.NewMeter(c.clock, c.stall)
	defer m.Stop()
	for {
		select {
		case a := <-got:
			if a.err != nil {
				return 0, "", a.err
			}
			n, to := failedHandshakes(a.losses)
			return n, to, nil
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-m.Wait():
			if m.Expired() {
				return 0, "", errCountSilent
			}
		}
	}
}

// afterTestCase asks for a read that counts the handshakes that failed since
// the last one as test case name's (and those of the test cases saved since,
// if the reads lag): a handshake's failure is the call that made it, as the
// call is made (models.PerTest). It does not wait for the read.
func (c *caRejectWatch) afterTestCase(_ context.Context, name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	c.pending = append(c.pending, name)
	c.askLocked()
}

// afterTestCases asks for the final read, once the app has stopped, while the
// agent still is up. It does not wait for it.
func (c *caRejectWatch) afterTestCases(context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	c.finalAsked = true
	c.askLocked()
}

// askLocked asks for one more read, starting the read goroutine if it is not
// running. c.mu is held.
func (c *caRejectWatch) askLocked() {
	c.wanted++
	if c.reading {
		return
	}
	c.reading = true
	c.idle = make(chan struct{})
	go c.readAll()
}

// readAll makes the reads asked for, one at a time, until none is wanted.
func (c *caRejectWatch) readAll() {
	for {
		c.mu.Lock()
		if c.wanted == 0 || c.finished {
			c.reading = false
			close(c.idle)
			c.mu.Unlock()
			return
		}
		c.wanted = 0
		names := c.pending
		c.pending = nil
		c.inflight = names
		afterFinal := c.finalAsked
		c.mu.Unlock()

		readCtx, cancelRead := stopProof(c.ctx)
		n, to, err := c.boundedRead(readCtx)
		cancelRead()

		c.mu.Lock()
		c.inflight = nil // finish, if it came first, has taken them
		if err == nil && afterFinal && !c.finished {
			c.covered = true
		}
		switch {
		case c.finished:
		case err != nil:
			// Its test cases are the next read's to count.
			c.pending = append(names, c.pending...)
			c.logger.Debug("keploy could not read its agent's count of failed TLS handshakes; the next read counts what it missed",
				zap.Int("testCases", len(names)), zap.Error(err))
		case len(names) > 0:
			grew := growth(c.last, n)
			c.last = n
			c.tail.read = false
			if grew > 0 {
				c.during += grew
				c.tests = append(c.tests, names...)
				c.to = to
			}
		default:
			c.tail.read, c.tail.n, c.tail.to = true, n, to
		}
		c.mu.Unlock()
	}
}

// growth is how many more handshakes failed between a read of last and one
// of n: a count lower than the last (an agent started again, counting from
// nothing) is taken whole, as models.CaptureLosses.Since takes it. The reads
// are in order, so a lower count is never an earlier answer. (An agent
// started again whose count has passed last by the next read is not told
// from one that was not: what it counted up to last is not said.)
func growth(last, n uint64) uint64 {
	switch {
	case n > last:
		return n - last
	case n < last:
		return n
	}
	return 0
}

// waitForReads waits for the reads asked for so far (and those asked while
// it waits), once the frame drains are over, before the stop lets the agent
// go: once it has looked (every c.every) and found a read still out, it says
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
func (c *caRejectWatch) waitForReads() {
	if c != nil {
		c.wait(false)
	}
}

// wait waits until no read is out, looking every c.every, and says once that
// it waits (and again when its offer is made).
// With offer (finish, after every drain), once it has said so, one more
// Ctrl+C stops it now (utils.StopNowNotice), and ends the watch's reads
// (utils.StopNow): what they would have counted is not said, and the test
// cases they would have counted are said not to be checked (finish); a copy
// of the stop's signal taken for that press costs no more than that, as
// every offer of the stop's must (utils.OfferStopNow). Without offer, it
// ends as the reads do (waitForReads).
// It reports whether it waited for all of them.
func (c *caRejectWatch) wait(offer bool) bool {
	c.mu.Lock()
	if !c.reading {
		c.mu.Unlock()
		return true
	}
	idle := c.idle
	c.mu.Unlock()
	var notice *utils.StopNowNotice
	if offer {
		notice = utils.NewStopNowNotice(c.every)
		defer notice.Withdraw()
	}
	tick := time.NewTicker(c.every)
	defer tick.Stop()
	said, offering := false, false
	for {
		var interrupt <-chan struct{}
		if notice != nil {
			interrupt = notice.Interrupt()
		}
		select {
		case <-idle:
			return true
		case <-interrupt:
			return false // the press closed utils.StopNow, which ends the reads out
		case <-tick.C:
		}
		wasOffering := offering
		if notice != nil {
			offering = notice.Next()
		}
		if !said || offering != wasOffering {
			msg := "Still waiting for the keploy agent to say how many TLS handshakes with keploy's proxy failed while this recording ran"
			switch {
			case offering:
				msg += "; press Ctrl+C again to stop now: what it would count is then not said"
			case notice == nil:
				msg += fmt.Sprintf("; Ctrl+C does not cut this short: each read is given up once keploy has had %s of its own time to run without an answer", c.stall)
			}
			c.logger.Info(msg)
			said = true
		}
	}
}

// finish waits for the reads still out (wait, offering one more Ctrl+C:
// every drain is over), then says, at WARN, how many handshakes failed and
// the test cases they were recorded in, if any did. What the final read saw
// beyond the last read that counted test cases, when none came after it,
// failed after the last test case; or, when test cases are left whose reads
// did not come (the agent went first), in them or after the last, and they
// are named as not checked.
func (c *caRejectWatch) finish(testSetID string) {
	if c == nil {
		return
	}
	c.wait(true)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finished = true
	// Test cases whose reads did not come, or were still out: a wait the
	// user stopped, or a read asked as finish began (a consumer of a record
	// drain that gave up).
	unread := append(append([]string(nil), c.inflight...), c.pending...)
	var beyond uint64
	to := c.to
	if c.tail.read {
		if beyond = growth(c.last, c.tail.n); beyond > 0 {
			to = c.tail.to
		}
	}
	// Test cases whose reads did not come: a read made since the final one
	// was asked, when nothing it saw is beyond the last counted, saw what
	// they failed too (their calls came before the stop).
	notChecked := unread
	if c.covered && beyond == 0 {
		notChecked = nil
	}
	n := c.during + beyond
	if n == 0 {
		if len(notChecked) > 0 {
			// Unknown is not none: these test cases' reads never came (the
			// agent went first, or the user stopped the wait for them).
			c.logger.Warn("keploy could not check whether a client rejected keploy's certificate while these test cases were recorded: its agent's count of failed TLS handshakes was not read after them",
				zap.String("testSetID", testSetID), zap.Strings("testCasesNotChecked", notChecked),
				zap.String("next_step", "if a client of the app pins its own CA, make it trust keploy's CA ("+caDocs+"), then re-record these test cases"))
		}
		return
	}
	fields := []zap.Field{zap.String("testSetID", testSetID)}
	next := "make the client trust keploy's CA (" + caDocs + "), then record again"
	if len(c.tests) > 0 {
		fields = append(fields, zap.Strings("testCases", c.tests))
	}
	switch {
	case beyond > 0 && len(notChecked) > 0:
		fields = append(fields, zap.Uint64("inTheseOrAfterTheLastTestCase", beyond), zap.Strings("testCasesNotChecked", notChecked))
	case beyond > 0:
		fields = append(fields, zap.Uint64("afterTheLastTestCase", beyond))
	case len(notChecked) > 0:
		fields = append(fields, zap.Strings("testCasesNotChecked", notChecked))
	}
	if len(c.tests) > 0 || (beyond > 0 && len(notChecked) > 0) {
		next = "make the client trust keploy's CA (" + caDocs + "), then re-record these test cases"
	}
	if to != "" {
		fields = append(fields, zap.String("last", to))
	}
	fields = append(fields, zap.String("next_step", next))
	c.logger.Warn(fmt.Sprintf("%d TLS handshake(s) with keploy's proxy failed while this recording ran (a client that does not trust keploy's CA rejects keploy's certificate); those calls are not recorded", n), fields...)
}
