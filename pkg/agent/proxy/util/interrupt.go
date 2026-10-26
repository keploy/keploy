package util

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// aLongTimeAgo is a deadline already passed: set on a connection, it ends the
// read or write in progress on it, and fails every later one at once.
var aLongTimeAgo = time.Unix(1, 0)

// InterruptOnDone ends conn's reads and writes once ctx is done: the one in
// progress and every later one return a timeout (os.ErrDeadlineExceeded), by a
// deadline already passed that it sets on conn then. A parser that serves conn
// on a goroutine of its own thereby ends with its call's context, and the call
// waits for it (JoinParser) instead of returning while the goroutine is still
// blocked on conn, to read, match, write and log after the call has returned.
//
// It ends a read or write only on a conn that honours deadlines (a TCP or TLS
// connection, or one that embeds it), and only one not given a later deadline
// after ctx is done: code that sets one before it reads checks ctx after
// setting it (ReadBytes and ReadRequiredBytes check it before every read).
//
// The function it returns undoes it, once nothing reads or writes conn for the
// call any more: it stops the watch, or, when ctx is done, waits for the
// deadline to be set and clears it. Neither the watch nor the deadline
// outlives the call. Clearing it clears any deadline conn had before, so two
// of these on one conn, one inside the other (a parser that lends its
// connection to another parser that uses this), are not supported: the inner
// one's undo clears the outer one's deadline, and only the outer parser's own
// checks of ctx before its reads then end them.
func InterruptOnDone(ctx context.Context, conn net.Conn) (undo func()) {
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		_ = conn.SetDeadline(aLongTimeAgo)
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stop() {
				<-interrupted
				_ = conn.SetDeadline(time.Time{})
			}
		})
	}
}

// JoinParser waits for a parser's goroutine to return and returns what it
// reported: the first result it sent on errCh, which it closes as it returns
// (its last deferred call, after its recover: RecoverInto). A result of
// io.EOF, the client's end, is nil. A first result that is a panic
// (ErrParserPanicked) is returned even once ctx is done, so the proxy counts
// it. Any other result is ctx's error once ctx is done, as the call returned
// on ctx's end before: what the goroutine reported then is most likely how
// the end stopped it (InterruptOnDone's deadline, or the context), and a miss
// made by a match the end cut short is no miss.
//
// The parser's call used to return ctx's error as soon as ctx was done, and
// left the goroutine blocked on the connection: it read, matched and logged on
// after the call had returned, through a logger its caller was done with (a
// test's logger after the test: a data race under go test -race).
func JoinParser(ctx context.Context, errCh <-chan error) error {
	var err error
	reported := false
	for e := range errCh {
		if !reported {
			err, reported = e, true
		}
	}
	if errors.Is(err, ErrParserPanicked) {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err == io.EOF {
		return nil
	}
	return err
}
