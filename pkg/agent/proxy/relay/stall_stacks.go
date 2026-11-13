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

// parserStalled is told by a tee that its parser has taken nothing from it for
// stalled, a whole stall window (Config.ConsumerStallGrace) of the time the
// process could run, with its channel full of captured chunks (waiting of
// them). While the connection is open the tee waits on (the parser is behind,
// not gone); once it has closed, the tee abandons what is left
// (DropConsumerGone), and the mocks it held are left out. Either way nothing
// in the log says what the parser waited on, and a stall that comes once in
// many runs, under load, is not found by running it again. So the first one
// in the process writes every goroutine's stack to a file in the run's log
// folder (log.WriteStacks: stacks only, never a captured byte, and never to
// the console) and says so in one WARN that names the file and, by logger's
// fields, the connection. Later stalls write nothing: one set of stacks shows
// what the process was doing, and a stall that repeats would fill the disk.
//
// The window is the tee's own stall bound, so a stall is written down before
// the tee gives up on it, and only one that is long enough to cost a mock once
// its connection closes: a CI run lost a ZooKeeper mock to a 4.7 s stall of
// both its parsers, against the 2 s default. A parser that is slow but takes
// a chunk within each window never trips it.
func parserStalled(logger *zap.Logger, dir fakeconn.Direction, stalled time.Duration, waiting int) {
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
		zap.Int("chunksWaiting", waiting),
		zap.String("next_step", "the app's traffic is unaffected. If this connection closes before its parser takes its "+
			"chunks again, they are left out (reason=consumer_gone). The stacks show what the parser waited on: attach the "+
			"file to a bug report; it holds no captured bytes"),
	}
	// A nil err is no field (zap.NamedError).
	fields = append(fields, zap.NamedError("stacksError", err))
	if name != "" {
		fields = append(fields, zap.String("stacks", name))
	}
	logger.Warn(StalledParserMsg, fields...)
}

// lookAtStall arms the look at a parser that takes nothing from the tee's full
// channel while its connection is open (drain): a stall window (stallGrace) on
// the tee's clock, which parserStalled is told of once it has passed with
// nothing taken. Nil once close() has been called: the stall bound's own
// verdict, as it abandons the chunks, tells parserStalled then.
func (t *tee) lookAtStall() (*stall.Meter, <-chan time.Time) {
	if t.closedFlag.Load() {
		return nil, nil
	}
	m := stall.NewMeter(t.clock, t.stallGrace)
	return m, m.Wait()
}
