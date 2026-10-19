package record

import (
	"context"
	"fmt"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// caDocs is where keploy says how a client that pins its own CA trusts
// keploy's, natively and in Docker.
const caDocs = "https://keploy.io/docs/running-keploy/docker-tls/#a-client-that-pins-its-own-ca"

// caRejectWatch is, with the proven switch off, the one thing keploy record
// says of the TLS handshakes its proxy saw fail: a client that rejected the
// certificate keploy answered with (it does not trust keploy's CA: a
// client that pins its own CA file, one whose runtime reads only the OS
// trust store) made a call keploy neither recorded nor can replay, and the
// recording otherwise ends as if it held everything. It names the test
// cases that were recorded while such a handshake failed, and how many failed
// after the last one, in one WARN when the recording ends. It changes nothing
// else: no test case, recording, verdict or exit code (with the switch on,
// captureWatch fails the recording instead). It counts only failed
// handshakes (models.CaptureLossTLSAbort); one a client hung up on
// (tls_handshake_abandoned) cannot be told from a client that gave up, and
// is left to the proven switch. A count it cannot read is left unsaid: it is
// a diagnostic, and says nothing it did not see.
//
// Its reads are made as the switch-on watch's are: not ended by the stop,
// which ends record's context before the drain saves its last test cases and
// before the final read, but by the second interrupt that gives the stop's
// waits up. They are made one at a time, and each applied before the next is
// made (readMu), so an answer is never applied after a later one.
type caRejectWatch struct {
	logger *zap.Logger
	reader CaptureLossReader

	// readMu orders the reads: one at a time, each applied before the next.
	// finish does not take it, so a read the agent never answers holds no
	// end of the recording up.
	readMu sync.Mutex

	mu     sync.Mutex
	last   uint64 // failed handshakes counted at the last read
	during uint64 // failed while test cases were recorded
	after  uint64 // failed after the last test case
	tests  []string
	to     string // the destination of the last failed handshake seen
}

// newCARejectWatch marks the agent's count of failed handshakes as capture
// begins; nil when it cannot be read.
func newCARejectWatch(ctx context.Context, logger *zap.Logger, reader CaptureLossReader) *caRejectWatch {
	n, _, err := failedHandshakes(ctx, reader)
	if err != nil {
		logger.Debug("keploy cannot read its agent's count of failed TLS handshakes; it will not say which test cases a client that rejected keploy's certificate was recorded in", zap.Error(err))
		return nil
	}
	return &caRejectWatch{logger: logger, reader: reader, last: n}
}

// failedHandshakes is the agent's count of client-side TLS handshakes that
// failed other than by the client hanging up, and the last one's
// destination, read on a context the stop does not end (only the second
// interrupt does).
func failedHandshakes(ctx context.Context, reader CaptureLossReader) (uint64, string, error) {
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRead()
	go func() {
		select {
		case <-utils.StopNow():
			cancelRead()
		case <-readCtx.Done():
		}
	}()
	losses, err := reader.GetCaptureLosses(readCtx)
	if err != nil {
		return 0, "", err
	}
	for _, c := range losses {
		if c.Kind == models.CaptureLossTLSAbort {
			return c.Count, c.What, nil
		}
	}
	return 0, "", nil
}

// afterTestCase counts the handshakes that failed since the last read as
// test case name's: a handshake's failure is the call that made it, as the
// call is made (models.PerTest). A read that fails leaves its count to the
// next read.
func (c *caRejectWatch) afterTestCase(ctx context.Context, name string) {
	if c == nil {
		return
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	n, to, err := failedHandshakes(ctx, c.reader)
	if err != nil {
		c.logger.Debug("keploy could not read its agent's count of failed TLS handshakes as a test case was saved; the next read counts what it missed",
			zap.String("testCase", name), zap.Error(err))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if grew := c.grew(n); grew > 0 {
		c.during += grew
		c.tests = append(c.tests, name)
		c.to = to
	}
}

// grew is how many more handshakes failed since the last read, which reads
// n: a count lower than the last (an agent started again, counting from
// nothing) is taken whole, as models.CaptureLosses.Since takes it. The reads
// are in order (readMu), so a lower count is never an earlier answer.
func (c *caRejectWatch) grew(n uint64) uint64 {
	last := c.last
	c.last = n
	switch {
	case n > last:
		return n - last
	case n < last:
		return n
	}
	return 0
}

// afterTestCases counts what failed after the last test case was saved.
func (c *caRejectWatch) afterTestCases(ctx context.Context) {
	if c == nil {
		return
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	n, to, err := failedHandshakes(ctx, c.reader)
	if err != nil {
		c.logger.Debug("keploy could not read its agent's count of failed TLS handshakes after the last test case; one that failed then is not said", zap.Error(err))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if grew := c.grew(n); grew > 0 {
		c.after += grew
		c.to = to
	}
}

// finish says, at WARN, how many handshakes failed and the test cases they
// were recorded in, if any did.
func (c *caRejectWatch) finish(testSetID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.during + c.after
	if n == 0 {
		return
	}
	fields := []zap.Field{zap.String("testSetID", testSetID)}
	next := "make the client trust keploy's CA (" + caDocs + "), then record again"
	if len(c.tests) > 0 {
		fields = append(fields, zap.Strings("testCases", c.tests))
		next = "make the client trust keploy's CA (" + caDocs + "), then re-record these test cases"
	}
	if c.after > 0 {
		fields = append(fields, zap.Uint64("afterTheLastTestCase", c.after))
	}
	if c.to != "" {
		fields = append(fields, zap.String("last", c.to))
	}
	fields = append(fields, zap.String("next_step", next))
	c.logger.Warn(fmt.Sprintf("%d TLS handshake(s) with keploy's proxy failed while this recording ran (a client that does not trust keploy's CA rejects keploy's certificate); those calls are not recorded", n), fields...)
}
