package generic

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A panic of the generic V2 recorder's main loop left its stream readers
// blocked for good. The supervisor recovers the panic, and its abort Closes
// both FakeConns, which ends a reader waiting on a read. But a reader with an
// event to send, a chunk or the error that ends its stream, was blocked on
// its send to the events channel, which nothing drained once the loop was
// gone, and the closer of that channel was blocked waiting for the readers:
// up to three goroutines, the FakeConns with every chunk still queued on
// them, and the events buffered were kept for the agent's life. It happened
// whenever the readers had more to send than the channel had room for when
// the loop panicked. A reader must also take at most one more chunk off its
// stream once the loop is gone: each one it takes can count as read
// (FakeConn.ReadThrough) when the proxy decides which requests the parser
// left unread, and nothing records it. The row reads its verdicts from the
// goroutines' state and the streams, with no clock: a goroutine of the
// recorder parked on a channel send or a select once the loop is gone fails
// it at once, and it passes when every goroutine the recorder started has
// ended having taken no more chunks than the case allows.
func TestEncodeGenericV2_LeavesNoReaderBehindAfterAPanicOfItsMainLoop(t *testing.T) {
	for _, c := range []struct {
		name string
		fill readersFill
		runs int
	}{
		{"its streams not filled", fillNeither, 1},
		{"the client's stream filled", fillClient, 1},
		{"both streams filled", fillBoth, 1},
		// A reader that sends once the loop is gone wins its select by chance
		// while the events channel has room, one time in two, and then takes
		// another chunk: 32 sessions leave such a reader one chance in 2^32
		// of passing.
		{"chunks reach the client's stream after the loop is gone", fillAfterTheLoop, 32},
	} {
		t.Run(c.name, func(t *testing.T) {
			for i := 0; i < c.runs; i++ {
				checkReadersEndAfterAPanicOfTheMainLoop(t, c.fill)
			}
		})
	}
}

// readersPanicMsg is what the main loop panics with in
// checkReadersEndAfterAPanicOfTheMainLoop.
const readersPanicMsg = "a panic of the generic V2 recorder's main loop"

// readersStarted is how many goroutines encodeGenericV2 has started, and not
// ended, while its main loop runs: the two stream readers and the closer of
// their events channel. The check fails its setup at once on another count,
// so a creation line that names no goroutine cannot pass as one whose
// goroutines all ended.
const readersStarted = 3

// readersFill says what reaches the streams past their one exchange.
type readersFill int

const (
	// fillNeither: nothing more, so both readers wait on a read, or come to
	// one, once the main loop panics, and the abort's Close ends them.
	fillNeither readersFill = iota
	// fillClient: more requests, sent once the dest's reader waits on a read,
	// done with the reply, so the client's reader parks on its send to the
	// full events channel before the main loop panics. The dest's reader
	// reads the abort's ErrClosed while the channel is still full, so its
	// error send would find no room either if it did not look at stop.
	fillClient
	// fillBoth: more requests and more replies, so both readers park on their
	// sends to the full events channel before the main loop panics.
	fillBoth
	// fillAfterTheLoop: nothing more until the main loop is gone, and the
	// client's reader waits on a read when it panics. Then requests reach
	// the client's stream before the abort's Close, with room in the events
	// channel: the reader's read in progress hands it one, and it must take
	// no other. The dest's reader reads the abort's ErrClosed.
	fillAfterTheLoop
)

// readersFillChunks is more than the events channel holds;
// readersAfterChunks is fewer.
const (
	readersFillChunks  = 1024
	readersAfterChunks = 8
)

// checkReadersEndAfterAPanicOfTheMainLoop runs encodeGenericV2 under a
// supervisor over a session whose streams hold one exchange, and whose
// mock (recorded as the reply reaches the loop) panics in the main loop
// (Session.OnMockRecorded runs in EmitMock): for fillClient and fillBoth,
// once the fill has been sent and the readers it fills are parked on their
// sends; for fillAfterTheLoop, once the client's reader waits on a read, the
// fill then sent by the abort. The abort Closes both FakeConns, as the
// proxy's does. It then fails t if a goroutine encodeGenericV2 started is
// parked for good, and once every one has ended, if a reader took more
// chunks off its stream after the loop was gone than the case allows: one,
// for the client's reader of fillAfterTheLoop; none, for every other.
func checkReadersEndAfterAPanicOfTheMainLoop(t *testing.T, fill readersFill) {
	t.Helper()
	at := time.Unix(2_000_000_000, 0)
	chunk := func(dir fakeconn.Direction, b string) fakeconn.Chunk {
		return fakeconn.Chunk{Dir: dir, Bytes: []byte(b), ReadAt: at, WrittenAt: at}
	}
	clientCh := make(chan fakeconn.Chunk, 1+readersFillChunks)
	destCh := make(chan fakeconn.Chunk, 1+readersFillChunks)
	clientCh <- chunk(fakeconn.FromClient, "REQ")
	destCh <- chunk(fakeconn.FromDest, "REP")
	clientConn := fakeconn.New(clientCh, nil, nil)
	destConn := fakeconn.New(destCh, nil, nil)
	sess := &supervisor.Session{
		ClientStream: clientConn,
		DestStream:   destConn,
		Mocks:        make(chan *models.Mock, 1),
		Logger:       zap.NewNop(),
		ClientConnID: "readers-after-a-panic",
		DestConnID:   "readers-after-a-panic-dst",
		Ctx:          context.Background(),
	}
	var created string           // set by the parser's goroutine, before it starts any
	var clientLeft, destLeft int // what the streams hold as the loop panics
	sess.OnMockRecorded = func(*models.Mock) {
		// The main loop runs this, and drains nothing more. Every goroutine
		// encodeGenericV2 started is alive here: no reader has ended, since
		// both streams are open, and the closer waits for both readers.
		if left, _, _ := scanReaders(created); left != readersStarted {
			panic(fmt.Sprintf("setup: %d goroutines %s are alive while the main loop runs, not %d", left, created, readersStarted))
		}
		// No time limits below: a reader that never waits on its read, or
		// never parks on its send, is a hang, which go test's -timeout ends
		// with every goroutine's stack.
		switch fill {
		case fillClient:
			awaitWaiting(destConn)
			for i := 0; i < readersFillChunks; i++ {
				clientCh <- chunk(fakeconn.FromClient, "REQX")
			}
			awaitReaderSends(created, 1)
		case fillBoth:
			for i := 0; i < readersFillChunks; i++ {
				clientCh <- chunk(fakeconn.FromClient, "REQX")
				destCh <- chunk(fakeconn.FromDest, "REPX")
			}
			awaitReaderSends(created, 2)
		case fillAfterTheLoop:
			awaitWaiting(clientConn)
		}
		clientLeft, destLeft = len(clientCh), len(destCh)
		panic(readersPanicMsg)
	}
	sup := supervisor.New(supervisor.Config{Logger: zap.NewNop()})
	sup.SessionOnAbort = func() {
		if fill == fillAfterTheLoop {
			for i := 0; i < readersAfterChunks; i++ {
				clientCh <- chunk(fakeconn.FromClient, "REQX")
			}
		}
		_ = clientConn.Close()
		_ = destConn.Close()
	}
	res := sup.Run(context.Background(), func(_ context.Context, s *supervisor.Session) error {
		created = fmt.Sprintf("created by %s in goroutine %d", funcName(encodeGenericV2), goroutineID())
		return encodeGenericV2(s, zap.NewNop())
	}, sess)
	if res.Status != supervisor.StatusPanicked || res.Err == nil || !strings.Contains(res.Err.Error(), readersPanicMsg) {
		t.Fatalf("setup: the main loop did not panic with %q: status %v, err %v", readersPanicMsg, res.Status, res.Err)
	}
	awaitReadersEnd(t, created)
	// A reader parked on its send holds the chunk it read; one waiting on a
	// read is handed one chunk at most, by that read.
	reached, wantTaken := clientLeft, 0
	if fill == fillAfterTheLoop {
		reached, wantTaken = clientLeft+readersAfterChunks, 1
	}
	if taken := reached - len(clientCh); taken != wantTaken {
		t.Fatalf("the client's reader took %d chunks off its stream after its main loop was gone, not %d", taken, wantTaken)
	}
	if taken := destLeft - len(destCh); taken != 0 {
		t.Fatalf("the dest's reader took %d chunks off its stream after its main loop was gone, not 0", taken)
	}
}

// awaitWaiting waits until fc's reader waits on a read with nothing buffered
// for it.
func awaitWaiting(fc *fakeconn.FakeConn) {
	for {
		if waiting, _ := fc.Waiting(); waiting {
			return
		}
		time.Sleep(time.Millisecond) // a yield between looks
	}
}

// awaitReaderSends waits until n goroutines whose creation line is created
// are parked on a channel send or a select outside FakeConn's read: a reader
// waiting in its read (in a select, for a chunk or a Close) waits for data,
// not on its send.
func awaitReaderSends(created string, n int) {
	for {
		if _, _, sending := scanReaders(created); sending >= n {
			return
		}
		time.Sleep(time.Millisecond) // a yield between looks
	}
}

// awaitReadersEnd waits until every goroutine whose creation line is created
// has ended. It runs once the main loop is gone, and fails t at once when one
// of them is parked on a channel send or a select, inside FakeConn's read or
// not, since neither can end any more: the abort's Close has ended every wait
// in FakeConn's read, the stop channel closed on the loop's exit has ended
// every select on it, and nothing receives from the events channel. It has no
// time limit: a goroutine that neither ends nor parks is a hang, which go
// test's -timeout ends with every goroutine's stack.
func awaitReadersEnd(t *testing.T, created string) {
	t.Helper()
	for {
		left, parked, _ := scanReaders(created)
		if parked != "" {
			t.Fatalf("a goroutine %s is parked for good after its main loop is gone:\n%s", created, parked)
		}
		if left == 0 {
			return
		}
		time.Sleep(time.Millisecond) // a yield between looks, not a budget
	}
}

// scanReaders counts the live goroutines whose creation line is created, and
// those of them parked on a channel send or a select outside FakeConn's read.
// It returns the stack of one parked on a channel send or a select, inside
// FakeConn's read or not, if any.
func scanReaders(created string) (left int, parked string, sending int) {
	for _, g := range strings.Split(allStacks(), "\n\n") {
		lines := strings.Split(g, "\n")
		if !createdBy(lines, created) {
			continue
		}
		left++
		if parkedOnAChannel(lines[0]) {
			parked = g
			if !inAFakeConnRead(lines[1:]) {
				sending++
			}
		}
	}
	return left, parked, sending
}

// fakeconnFunc is the prefix of the name of every function of package
// fakeconn, as a stack prints it.
var fakeconnFunc = strings.TrimSuffix(funcName(fakeconn.New), "New")

// inAFakeConnRead reports whether a stack's first frame past package
// runtime's is a function of package fakeconn: a reader waiting in
// FakeConn's read. (runtime.Stack ignores GOTRACEBACK: it prints level 1.)
func inAFakeConnRead(frames []string) bool {
	for _, f := range frames {
		if f == "" || strings.HasPrefix(f, "\t") || strings.HasPrefix(f, "runtime.") {
			continue
		}
		return strings.HasPrefix(f, fakeconnFunc)
	}
	return false
}

// createdBy reports whether a goroutine's stack ends in the creation line
// created (Go prints "created by FUNC in goroutine N", then its file line).
func createdBy(lines []string, created string) bool {
	for _, l := range lines {
		if l == created {
			return true
		}
	}
	return false
}

// parkedOnAChannel reports whether a stack's header ("goroutine N [STATE]:"
// or "goroutine N [STATE, M minutes]:") says it waits on a channel send or a
// select.
func parkedOnAChannel(header string) bool {
	i := strings.IndexByte(header, '[')
	if i < 0 {
		return false
	}
	state := header[i+1:]
	return strings.HasPrefix(state, "chan send") || strings.HasPrefix(state, "select")
}

func allStacks() string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// goroutineID is the id of the calling goroutine, as its stack's header
// prints it.
func goroutineID() int64 {
	var buf [64]byte
	b := bytes.TrimPrefix(buf[:runtime.Stack(buf[:], false)], []byte("goroutine "))
	i := bytes.IndexByte(b, ' ')
	if i < 0 {
		panic("no goroutine id in the stack's header")
	}
	id, err := strconv.ParseInt(string(b[:i]), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("the stack's header: %v", err))
	}
	return id
}

func funcName(fn any) string {
	return runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
}
