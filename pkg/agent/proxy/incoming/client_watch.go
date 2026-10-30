package proxy

import (
	"bufio"
	"net"

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
// client whose next bytes the ingress has already read (buffered, or still
// in replayConn's prefix) is not watched: its end, if it sent one, comes
// after that request, as without keploy. Nothing may read client until stop
// has returned; the response may be written to it meanwhile.
func watchClient(client net.Conn, buffered *bufio.Reader, app func() net.Conn) (stop func() (ended bool)) {
	if buffered.Buffered() > 0 {
		return noClientEnd
	}
	conn := client
	if r, ok := client.(*replayConn); ok {
		if r.reader.Buffered() > 0 {
			return noClientEnd
		}
		conn = r.Conn
	}
	return proxyutil.WatchPeerEnd(conn, func(reset bool) {
		up := app()
		if up == nil {
			return
		}
		if reset || proxyutil.CloseWriteIfPossible(up) != nil {
			_ = up.Close()
		}
	})
}

func noClientEnd() bool { return false }
