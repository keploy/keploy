package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
	"golang.org/x/net/http/httpguts"
)

// clientWriteError is an interim response the ingress could not forward: the
// client is gone, not the app.
type clientWriteError struct{ err error }

func (e *clientWriteError) Error() string {
	return "forward the app's interim response to the client: " + e.err.Error()
}

func (e *clientWriteError) Unwrap() error { return e.err }

// readFinalResponse reads the app's response to req from app, forwarding each
// interim response (a 100 Continue, a 102 Processing, a 103 Early Hints) to
// client as it comes, as the app sent it, and returns the final one: the one
// the exchange is recorded with. Interim responses are not recorded: they are
// not the app's answer, and replay's client, net/http's, passes over them.
// interims is how many came. A failure to forward one is a *clientWriteError.
//
// An HTTP/1.0 client is sent none (RFC 9110 15.2): it does not know them, and
// only the ingress, which forwards its request as HTTP/1.1, made the app think
// it would.
func readFinalResponse(app *bufio.Reader, req *http.Request, client io.Writer) (resp *http.Response, interims int, err error) {
	resp, err = proxyutil.ReadFinalResponse(app, req, func(interim *http.Response) error {
		interims++
		if !req.ProtoAtLeast(1, 1) {
			return nil
		}
		if err := proxyutil.WriteInterimResponse(client, interim); err != nil {
			return &clientWriteError{err}
		}
		return nil
	})
	return resp, interims, err
}

// expectsContinue reports whether req's client waits for a 100 (Continue)
// before it sends the body (RFC 9110 10.1.1).
func expectsContinue(req *http.Request) bool {
	return req.Body != nil && req.Body != http.NoBody &&
		httpguts.HeaderValuesContainsToken(req.Header["Expect"], "100-continue")
}

// aLongTimeAgo is a deadline already passed: set on a connection, it ends the
// read or write in progress on it.
var aLongTimeAgo = time.Unix(1, 0)

// What the ingress still forwards of a body the app answered before it had it
// all: at most maxContinueDrainBytes more of it, for at most
// continueDrainTimeout, before it forwards the answer. The body comes:
//   - The client the app's 100 (Continue) reached sends it whatever the answer
//     (RFC 9110 10.1.1). Node's and Python's servers send the 100 before the
//     handler runs, so a handler that turns the request down at once (an auth
//     check) answers before the body.
//   - A client the app sent no 100 sends it once it stops waiting for one:
//     curl and Go's client after a second. Go's server sends no 100 to a
//     handler that does not read the body, and answers without it.
//
// The app decides what becomes of it: Node reads and drops a body its handler
// left, keeping the connection; Go's server closes the connection. Forwarded,
// the exchange is recorded whole, and the connection carries the next request
// if the app keeps it, as without keploy. Past either bound the rest is cut
// off, the exchange is not recorded, and its connection ends: the bounds keep
// a body the app does not want, or a client that never sends it, from holding
// the exchange (and in --sync, the lock), and the client gets the answer then.
// 256 KiB is what Go's server reads of a body its handler left (one sent
// without Expect), to keep the connection.
const maxContinueDrainBytes = 256 << 10

// continueDrainTimeout is a variable so a test can shorten it, or make it
// outlast what it checks.
var continueDrainTimeout = 2 * time.Second

// errDrainBound is a body the ingress cut off at maxContinueDrainBytes.
var errDrainBound = errors.New("more of the request body came after the app's answer than the ingress forwards")

// requestRelay writes a request to the app while its response is read. The
// client of a request that expects a 100 (Continue) sends the body only after
// the app's 100 reaches it, and the response read forwards the 100
// (readFinalResponse). Written before the read, as other requests are, the
// body waited for the 100 and the 100 for the body. A nil *requestRelay stands
// for a request already written whole.
type requestRelay struct {
	done        chan error
	client, app net.Conn
	body        *clientBody
	length      int64       // the body's Content-Length, -1 when not known (chunked)
	answered    atomic.Bool // finish has begun: the app has answered, or failed to
}

// relayRequest starts writing req to app; client is the connection its body
// comes from. finish must be called before anything else reads from client or
// writes to app, or reads what the body was teed to.
func relayRequest(req *http.Request, app, client net.Conn) *requestRelay {
	body := &clientBody{ReadCloser: req.Body}
	body.limit.Store(math.MaxInt64)
	r := &requestRelay{done: make(chan error, 1), client: client, app: app, body: body, length: req.ContentLength}
	req.Body = body
	go func() {
		err := req.Write(app)
		_ = req.Body.Close()
		// The body broke off on the client's side (the client went away after
		// the 100, or sent a body that does not parse): the app is told, by
		// the end of what it is sent, as the client's going would have told
		// it. Left waiting for the rest, it would not answer, and the read of
		// its response, which nothing else ends, would wait with it (in sync
		// recording, holding the lock). Not once the app has answered: it may
		// still be sending the answer, and a body cut off then is the
		// ingress's doing, not the client's.
		if body.err != nil && !r.answered.Load() {
			_ = proxyutil.CloseWriteIfPossible(app)
		}
		r.done <- err
	}()
	return r
}

// clientBody is a request body that counts what is read of it and keeps the
// error its read from the client failed with, if any (the end of the body is
// no failure). A read that takes it past limit fails with errDrainBound.
type clientBody struct {
	io.ReadCloser
	read  atomic.Int64
	limit atomic.Int64
	err   error // read and written by the relay's goroutine alone
	ended bool  // likewise: a read returned io.EOF
}

func (b *clientBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.read.Add(int64(n)) > b.limit.Load() {
		err = errDrainBound
	}
	switch {
	case err == io.EOF:
		b.ended = true
	case err != nil:
		b.err = err
	}
	return n, err
}

// Close does not read what is left of a body that did not end, as net/http's
// does, to find its end: that read would wait on the very client a cut-off
// stopped waiting on, and nothing reads the client's connection after a body
// that did not end (its exchange is not whole, and its connection closes).
func (b *clientBody) Close() error {
	if !b.ended {
		return nil
	}
	return b.ReadCloser.Close()
}

// allow lets n more bytes of the body be read, and reports whether that is
// all that is left of it, as far as its length tells (length -1: not known).
func (b *clientBody) allow(length, n int64) bool {
	read := b.read.Load()
	if length >= 0 && length-read > n {
		return false
	}
	b.limit.Store(read + n)
	return true
}

// finish waits for the request's write to end, once the app has sent its
// final response, and returns nil when the whole request reached the app,
// else why it did not. The rest of a body the app answered before it came is
// forwarded within maxContinueDrainBytes and continueDrainTimeout, then cut
// off. A request cut off did not reach the app whole: it is not recorded, and
// its connection carries no request after it.
func (r *requestRelay) finish() error {
	return r.end(true)
}

// abandon ends the request's write at once: the app gave no answer to wait
// for the rest of the body with.
func (r *requestRelay) abandon() {
	_ = r.end(false)
}

// end is finish (drain: forward the rest of the body within the bounds) or
// abandon.
func (r *requestRelay) end(drain bool) error {
	if r == nil {
		return nil
	}
	select {
	case err := <-r.done:
		return err
	default:
	}
	r.answered.Store(true)
	// Whatever the write still waits on, the client's body or room on the
	// app's side, ends at the deadline. A read or write already done is not
	// undone: a write that sent the last of the body ends without an error.
	deadline, why := aLongTimeAgo, "the app gave no answer"
	if drain {
		why = fmt.Sprintf("more than %d KiB of the body was left when the app answered", maxContinueDrainBytes>>10)
		if r.body.allow(r.length, maxContinueDrainBytes) {
			deadline = time.Now().Add(continueDrainTimeout)
			why = fmt.Sprintf("the rest of the body did not reach the app within %s, and %d KiB, of its answer", continueDrainTimeout, maxContinueDrainBytes>>10)
		}
	}
	_ = r.client.SetReadDeadline(deadline)
	_ = r.app.SetWriteDeadline(deadline)
	err := <-r.done
	// The connections may carry the next exchange.
	_ = r.client.SetReadDeadline(time.Time{})
	_ = r.app.SetWriteDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("%s: %w", why, err)
	}
	return nil
}

// lingerTimeout is how long lingerClose reads, and drops, what a client still
// sends: Go's server waits as long (rstAvoidanceDelay). A variable so a test
// can make it outlast what it checks.
var lingerTimeout = 500 * time.Millisecond

// lingerClose readies for its close a connection whose request the ingress
// cut off (requestRelay.finish), once the response is written: its client may
// still be sending the body. It ends what the ingress sends, so the client
// knows the response is all, and reads and drops what the client sends until
// the client ends the connection too, or lingerTimeout passes. A connection
// closed with the client's bytes unread is reset, and a reset can take with it
// a response its client has not read yet (Windows discards it); Go's server
// waits so too (closeWriteAndWait). The caller closes the connection.
func lingerClose(c net.Conn) {
	_ = proxyutil.CloseWriteIfPossible(c)
	_ = c.SetReadDeadline(time.Now().Add(lingerTimeout))
	_, _ = io.Copy(io.Discard, c)
}

// warnNotRecorded tells the user why an exchange whose request the app
// answered before all of it came (finish's err) is not a test case: a test
// the user expects is otherwise missing with no trace.
func warnNotRecorded(logger *zap.Logger, req *http.Request, resp *http.Response, err error) {
	logger.Warn("Not recording this request as a test case: the app answered it before the whole request body reached the app, so the request the app answered is not the one the client sent. The client still got the app's answer. To record it, send the request without the \"Expect: 100-continue\" header (curl: -H 'Expect:').",
		zap.String("method", req.Method), zap.String("url", req.URL.String()),
		zap.Int("status_code", resp.StatusCode), zap.Error(err))
}
