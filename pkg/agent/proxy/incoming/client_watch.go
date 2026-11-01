package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync"

	"go.uber.org/zap"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
)

// watchClient watches the client of an exchange for its end (its FIN or a
// reset) from when the app has the whole request until the answer's end is
// read from the app, and tells the app of it, as the client's going would
// have told it without keploy: the client's FIN half-closes the app's
// connection (a Go server cancels the request's context on it), a reset
// closes it. In that time the ingress reads only the app. An app that sends
// nothing more for a while (a stream of a job's events, a long poll) never
// learned its client had gone: it kept the request, and the ingress, the
// connection's handler, waited on it with the client's end unread, for good
// if no more came. endedBefore ends the watch and reports whether the client
// ended the exchange so; one that did is not recorded: its client did not take
// the answer, and the app's end of it may be the client's going.
//
// Whether the client went before the answer's end is decided by what had
// arrived from the app when its going was seen, not by how far the ingress
// had forwarded the answer: arrived counts what the app's side has brought,
// its bytes (those keploy has read, and then those still unread in the app
// connection's socket) and its close as one more once keploy has read it,
// and is read as the going is seen, before the app is told of it;
// endedBefore is given where the answer ended in that count (answerEnd), or
// -1 while its end is not found. Read in that order, it can only undercount:
// a byte the reads move out of the socket in between is in neither. A client
// that went once the answer's end had arrived did not end the exchange: the
// app sent that end before it could learn of the going, so the answer is the
// app's answer to the request, whatever the client then took of it, and is
// recorded even when forwarding the rest of it to the gone client fails (the
// ingress reads on to its end, finishAnswer). A client that
// takes the headers and closes without reading the body (Go's http.Get and a
// Body.Close, a client that wanted only the status) does that as soon as
// the ingress forwards the headers, which it does before it reads the body
// on: an answer the app wrote whole at once, its end in the read that brought
// its headers, was not recorded whenever the watch saw the close before the
// ingress read the body, which a loaded host makes often.
//
// The watch only peeks at the client's socket: what the client sends next
// (a request it pipelined) stays for the next read, and ends the watch. A
// client whose next bytes the ingress has already read (buffered) is not
// watched: its end, if it sent one, comes after that request, as without
// keploy. Nothing may read client until the watch is halted; the response
// may be written to it meanwhile.
//
// A client with no socket to be found under it (a wrapper that does not
// give the conn it wraps, or a platform with no watch) is not watched, and
// the first of each kind is told at Info: its app is not told of its going.
//
// Under the proxy's settle, the connection it keeps under client holds the
// hang-up while the watch runs and, once the hang-up is told on to the app,
// holds no test's work (proxyutil.HoldHangUp): the exchange is not recorded,
// or, its answer's end with keploy before the client went, is recorded from
// what had arrived, and nothing waits on what the app sends after.
func watchClient(logger *zap.Logger, client net.Conn, buffered *bufio.Reader, app func() net.Conn, arrived func() int64) *clientWatch {
	if buffered.Buffered() > 0 {
		return nil
	}
	conn := client
	if r, ok := client.(*replayConn); ok {
		// Its prefix (the bytes read to tell HTTP/2's preface) is all
		// read by the time a request is: bufio's first fill takes it.
		conn = r.Conn
	}
	// The proxy's kept connection under conn holds its client's hang-up
	// until it is told on, and holds no test's work once it is
	// (proxyutil.HangUpTeller): an app that goes on with the request its
	// client left (one deaf to its request's context) is no test's to wait
	// for.
	release := proxyutil.HoldHangUp(conn)
	w := &clientWatch{release: release}
	stopWatch, watched := proxyutil.WatchPeerEnd(conn, func(reset bool) {
		// Read before the app is told: all it counts, the app sent before
		// it could learn of the going. Read by halt only once the watch has
		// ended (stopWatch returns after this callback).
		w.arrivedAtGoing = arrived()
		up := app()
		if up == nil {
			return
		}
		if reset || proxyutil.CloseWriteIfPossible(up) != nil {
			_ = up.Close()
		}
		release(true)
	})
	if !watched {
		release(false)
		tellUnwatched(logger, conn)
		return nil
	}
	w.stop = stopWatch
	return w
}

// errPastArrived ends a read of an app's answer bounded at what had arrived
// from the app before it could learn its client had gone (readTo): what comes
// after may answer the going, not the request.
var errPastArrived = errors.New("the app's answer goes on past what had arrived from the app before it could learn its client had gone")

// clientWatch is the watch on an exchange's client (watchClient). nil is no
// watch: a client that is not watched never ends an exchange, and its app is
// told nothing by keploy while it answers.
type clientWatch struct {
	stop    func() (ended bool) // the watch's; nil once halted
	release func(told bool)
	// went is whether the watch saw the client go, and told the app.
	went bool
	// arrivedAtGoing is what had arrived from the app when the client's
	// going was seen, read before the app was told of it.
	arrivedAtGoing int64
}

// halt ends the watch, if it still runs, and reports whether the client went
// while it ran (and the app was told) and, if it did, what had arrived from
// the app by then: all of it the app sent before it could learn of the going.
// Once halted, nothing more is told the app until the ingress closes its
// connection.
func (w *clientWatch) halt() (went bool, arrived int64) {
	if w == nil {
		return false, 0
	}
	if w.stop != nil {
		w.went = w.stop()
		w.stop = nil
		w.release(false) // no-op once told
	}
	return w.went, w.arrivedAtGoing
}

// endedBefore halts the watch and reports whether the client ended the
// exchange: it went before the answer's end had arrived from the app.
// answerEnd is where the answer ended in what arrives from the app, or -1
// while its end is not found.
func (w *clientWatch) endedBefore(answerEnd int64) bool {
	went, arrived := w.halt()
	return went && (answerEnd < 0 || arrived < answerEnd)
}

// unwatched is the kinds of client connection told as not watched
// (tellUnwatched), by type: a handful at most, as the proxy's wrappers are.
var unwatched sync.Map

// tellUnwatched tells, once per kind of conn, that a client of that kind is
// not watched for its going (watchClient).
func tellUnwatched(logger *zap.Logger, conn net.Conn) {
	kind := fmt.Sprintf("%T", conn)
	if _, told := unwatched.LoadOrStore(kind, struct{}{}); told {
		return
	}
	logger.Info("keploy cannot watch this kind of client connection for its client's hang-up: an app still answering when its client goes is not told, and the call's connection holds the test's end until the app's answer ends",
		zap.String("connection", kind), zap.String("os", runtime.GOOS),
		zap.String("note", "told once for each kind of connection"))
}

// tellClientLeft tells that the exchange of req is not recorded because its
// client ended its side before the app's answer ended (watchClient), as
// warnNotRecorded tells of another: a test the user expects is otherwise
// missing with no trace. A client that only half-closed once its request was
// sent may still have read the whole answer.
func tellClientLeft(logger *zap.Logger, req *http.Request) {
	logger.Info("Not recording this request as a test case: its client closed its side of the connection before the app's answer ended. Keploy told the app, as the client's going would have told it without keploy, so what the app sent after that may answer the client's going rather than the request. A client that half-closes once its request is sent may still have read the whole answer: to record such a request, keep the client's side open until the answer ends.",
		zap.String("method", req.Method), zap.String("url", req.URL.String()))
}
