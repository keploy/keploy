package record

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// caDocs is where keploy says how a client that pins its own CA trusts
// keploy's, natively and in Docker.
const caDocs = "https://keploy.io/docs/running-keploy/docker-tls/#a-client-that-pins-its-own-ca"

// errCountSilent is why a read of the agent's count was given up: keploy had
// the whole of the read's bound (countReads.stall) to run, and the agent did
// not answer.
var errCountSilent = errors.New("the keploy agent did not answer its count of failed TLS handshakes")

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
// recording. Every later read is one of countReads': asked for, not waited
// for, bounded, and not ended by the stop. A read that failed, or was given
// up, leaves its test cases to the next read.
type caRejectWatch struct {
	*countReads

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
	c := &caRejectWatch{}
	c.countReads = newCountReads(ctx, logger, reader, new(sync.Mutex), errCountSilent,
		"how many TLS handshakes with keploy's proxy failed while this recording ran",
		"what it would count is then not said",
		c.counted)
	losses, err := c.boundedRead(ctx)
	if err != nil {
		logger.Debug("keploy cannot read its agent's count of failed TLS handshakes; it will not say which test cases a client that rejected keploy's certificate was recorded in", zap.Error(err))
		return nil
	}
	c.last, _ = failedHandshakes(losses)
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

// afterTestCase asks for a read that counts the handshakes that failed since
// the last one as test case name's (and those of the test cases saved since,
// if the reads lag): a handshake's failure is the call that made it, as the
// call is made (models.PerTest). It does not wait for the read.
func (c *caRejectWatch) afterTestCase(_ context.Context, name string) {
	if c == nil {
		return
	}
	c.askTest(name)
}

// afterTestCases asks for the final read, once the app has stopped, while the
// agent still is up. It does not wait for it.
func (c *caRejectWatch) afterTestCases(context.Context) {
	if c == nil {
		return
	}
	c.askFinal()
}

// counted takes a read's answer (countReads.apply), c.mu held.
func (c *caRejectWatch) counted(r countRead) {
	switch {
	case r.err != nil:
		// Its test cases are the next read's to count.
		c.pending = append(r.names, c.pending...)
		c.logger.Debug("keploy could not read its agent's count of failed TLS handshakes; the next read counts what it missed",
			zap.Int("testCases", len(r.names)), zap.Error(r.err))
	case len(r.names) > 0:
		n, to := failedHandshakes(r.losses)
		grew := growth(c.last, n)
		c.last = n
		c.tail.read = false
		if grew > 0 {
			c.during += grew
			c.tests = append(c.tests, r.names...)
			c.to = to
		}
	default:
		n, to := failedHandshakes(r.losses)
		c.tail.read, c.tail.n, c.tail.to = true, n, to
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

// waitForReads waits for the reads still out (countReads.waitForReads).
func (c *caRejectWatch) waitForReads() {
	if c != nil {
		c.countReads.waitForReads()
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
	unread := c.unreadLocked()
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
