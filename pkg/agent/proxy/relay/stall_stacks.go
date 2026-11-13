package relay

import (
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/utils/log"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// StalledParserMsg is the WARN parserStalled logs, once per process.
const StalledParserMsg = "a connection's parser took none of the bytes captured for it for a whole stall window: " +
	"every goroutine's stack is written to the file named here, once per run"

// stacksWritten is set once this process has written its goroutines' stacks
// for a stalled parser (parserStalled): a stall after that writes nothing.
var stacksWritten atomic.Bool

// writeStacks writes every goroutine's stack to a fresh file beside the run's
// log and returns its name (log.WriteStacks). A test swaps in its own.
var writeStacks = log.WriteStacks

// parserStalled is told by a tee that its parser has taken none of the bytes
// it was handed for stalled, a whole stall window (Config.ConsumerStallGrace)
// of the time the process could run, with bytes of them still waiting
// (waitingBytes, in waitingChunks chunks of its channel and the drain's hand).
// While the connection is open the tee waits on (the parser is behind, not
// gone); once it has closed with the parser's channel full, the tee abandons
// what is left (DropConsumerGone), and the mocks it held are left out. Either
// way nothing in the log says what the parser waited on, and a stall that
// comes once in many runs, under load, is not found by running it again. So
// the first one in the process writes every goroutine's stack to a file in
// the run's log folder (log.WriteStacks: stacks only, never a captured byte,
// and never to the console) and says so in one WARN that names the file and,
// by logger's fields, the connection. Later stalls write nothing: one set of
// stacks shows what the process was doing, and a stall that repeats would
// fill the disk.
//
// The window is the tee's own stall bound, so a stall is written down before
// the tee gives up on it: a CI run lost a ZooKeeper mock to a 4.7 s stall of
// both its parsers, against the 2 s default. Any stall that long is written
// down, however few bytes wait: it fires once per process, so it costs one
// file, and a floor on what waits could skip the stall that is wanted. A
// parser that is slow but takes some of its bytes within each window never
// trips it.
func parserStalled(logger *zap.Logger, dir fakeconn.Direction, stalled time.Duration, waitingChunks int, waitingBytes int64) {
	if !stacksWritten.CompareAndSwap(false, true) {
		return
	}
	name, err := writeStacks()
	if logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("direction", dir.String()),
		zap.Duration("stalledFor", stalled),
		zap.Int("chunksWaiting", waitingChunks),
		zap.Int64("bytesWaiting", waitingBytes),
		zap.String("next_step", "the app's traffic is unaffected. If the parser is still stalled when this connection "+
			"closes with its channel full, what is left is not recorded (reason=consumer_gone). The stacks show what the "+
			"parser waited on: attach the file to a bug report; it holds no captured bytes"),
	}
	// A nil err is no field (zap.NamedError).
	fields = append(fields, zap.NamedError("stacksError", err))
	if name != "" {
		fields = append(fields, zap.String("stacks", name))
	}
	logger.Warn(StalledParserMsg, fields...)
}

// parserLook is a drain's look at its parser while the connection is open,
// armed as the drain waits (on an empty queue or on a full channel) with bytes
// it handed the parser still waiting. took is how many bytes the parser had
// taken (tee.consumed) at the last look. A look that finds it took more, or
// finds nothing waiting, starts afresh: a parser that takes some of its bytes
// within each window is not stalled. Its looks come every stall.CheckEvery of
// the window on a plain timer, which holds nothing: a parser is mostly still
// at work on what the drain just handed it when the drain waits, and holding
// the process clock (stall.Process, which samples the process's threads
// every stall.Tick while held) for each such wait would sample through all of
// a recording's traffic. Only a look that finds the parser took nothing since
// the one before starts meter, the stall window counted in time the process
// could run (stall.Meter, as the stall bound is); a window that passes with
// nothing taken and bytes waiting is a stall (parserStalled).
type parserLook struct {
	c     <-chan time.Time
	stopC func() bool
	meter *stall.Meter
	took  int64
}

// armLook arms l, unless it is armed, and returns the channel of its next look
// (nil: none to wait on). It arms nothing once close() has been called (the
// stall bound's own verdict, as it abandons the chunks, tells parserStalled
// then; a look armed before goes on), once the process has written its
// stacks (there is nothing left to write), and while nothing waits for the
// parser.
func (t *tee) armLook(l *parserLook) <-chan time.Time {
	if l.c != nil {
		return l.c
	}
	if t.closedFlag.Load() || stacksWritten.Load() {
		return nil
	}
	took, waiting := t.parserWaiting()
	if waiting <= 0 {
		return nil
	}
	l.took = took
	l.c, l.stopC = t.lookClock().After(stall.CheckEvery(t.stallGrace))
	return l.c
}

// lookClock is the clock of a look's plain timer: the tee's, or, outside
// tests, the process clock unheld (stall.ProcessClock), whose timers are wall
// time.
func (t *tee) lookClock() stall.Clock {
	if t.clock != nil {
		return t.clock
	}
	return stall.ProcessClock()
}

// parserWaiting is how many bytes of this direction the parser has taken
// (tee.consumed), and how many of those the drain has handed it it has not
// taken yet. Nothing waits for a parser that closed its FakeConn
// (consumerGone): it will take nothing more, and has not stalled. Nor is
// anything known to wait on a tee with no reader count (tee.consumed: New
// sets one for every connection, so a tee of a package test only).
func (t *tee) parserWaiting() (took, waiting int64) {
	if t.consumed == nil {
		return 0, 0
	}
	select {
	case <-t.consumerGone:
		return 0, 0
	default:
	}
	t.mu.Lock()
	handed := t.progress.deliveredBytes
	t.mu.Unlock()
	took = t.consumed()
	return took, handed - took
}

// lookedAt is told that l's look came, with waitingChunks chunks in the
// parser's channel and the drain's hand. With nothing waiting the look ends;
// a parser that took more since the last look goes back to the plain timer;
// one that took nothing starts the stall window, and a window passed with
// nothing taken is a stall, told once (parserStalled).
func (t *tee) lookedAt(l *parserLook, waitingChunks int) {
	took, waiting := t.parserWaiting()
	switch {
	case waiting <= 0:
		l.stop()
		return
	case took != l.took:
		l.took = took
		l.meter.Stop()
		l.meter = nil
	case l.meter == nil:
		l.meter = stall.NewMeter(t.clock, t.stallGrace)
	case l.meter.Expired():
		stalled := l.meter.Stalled()
		l.stop()
		parserStalled(t.logger, t.dir, stalled, waitingChunks, waiting)
		return
	}
	if l.meter != nil {
		l.c, l.stopC = l.meter.Wait(), nil
		return
	}
	l.c, l.stopC = t.lookClock().After(stall.CheckEvery(t.stallGrace))
}

// stop ends l and releases its timer and its meter. Safe on a look not armed.
func (l *parserLook) stop() {
	if l.stopC != nil {
		l.stopC()
	}
	l.meter.Stop()
	*l = parserLook{}
}
