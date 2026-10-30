package proxy

import (
	"bufio"
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
// if no more came. stop ends the watch and reports whether the client ended
// the exchange so; one that did is not recorded: its client did not take the
// answer, and the app's end of it may be the client's going.
//
// The watch only peeks at the client's socket: what the client sends next
// (a request it pipelined) stays for the next read, and ends the watch. A
// client whose next bytes the ingress has already read (buffered) is not
// watched: its end, if it sent one, comes after that request, as without
// keploy. Nothing may read client until stop has returned; the response may
// be written to it meanwhile.
//
// A client with no socket to be found under it (a wrapper that does not
// give the conn it wraps, or a platform with no watch) is not watched, and
// the first of each kind is told at Info: its app is not told of its going.
func watchClient(logger *zap.Logger, client net.Conn, buffered *bufio.Reader, app func() net.Conn) (stop func() (ended bool)) {
	if buffered.Buffered() > 0 {
		return noClientEnd
	}
	conn := client
	if r, ok := client.(*replayConn); ok {
		// Its prefix (the bytes read to tell HTTP/2's preface) is all
		// read by the time a request is: bufio's first fill takes it.
		conn = r.Conn
	}
	stop, watched := proxyutil.WatchPeerEnd(conn, func(reset bool) {
		up := app()
		if up == nil {
			return
		}
		if reset || proxyutil.CloseWriteIfPossible(up) != nil {
			_ = up.Close()
		}
	})
	if !watched {
		tellUnwatched(logger, conn)
	}
	return stop
}

func noClientEnd() bool { return false }

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
