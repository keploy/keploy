package models

import "errors"

// ProxyConnCounts are the counts a test's begin and end read about the
// connections to the agent's proxy, each only ever growing for the life of the
// agent process:
//
//   - Established: the connections the kernel has completed to the proxy's
//     listener (the one the hooks marked), the highest number it gave them.
//     accept() returns a connection only after the kernel numbered it, so
//     every accepted connection is in this count, and so is one still
//     waiting in the accept queue.
//   - Accepted: every connection numbered up to it has been accepted by the
//     proxy's listener. Read before Established, it is never higher.
//   - ListenDrops: the packets the kernel's TCP layer dropped at the proxy's
//     listener (its sk_drops): a SYN or a final ACK it could not take, its
//     accept queue full among the causes. A connection dropped so is in
//     neither proxy-side count until the client's retransmission completes
//     it, so a test during which this grew is not verified: any change
//     between two reads is a drop.
//   - AppConns: the application's own connections to the proxy (the ones
//     keploy redirected) whose connect() has returned, numbered by the
//     kernel as they do. One is in this count before its final ACK reaches
//     the proxy's side, which is when it enters Established.
//   - AppSettled: every application connection numbered up to it has been
//     paired with the proxy's side of it, or lost (closed first: a capture
//     loss). Read before AppConns, it is never higher.
//
// A test's end (and begin) waits until every application connection counted
// is settled, then until every connection then completed to the proxy has
// been accepted (Agent.WaitProxySettled), so that a call the application made
// is in the proxy's hands before the test is judged (design v14 §5.2,
// verify-v14 M1).
type ProxyConnCounts struct {
	Established uint64 `json:"established"`
	Accepted    uint64 `json:"accepted"`
	ListenDrops uint64 `json:"listen_drops"`
	AppConns    uint64 `json:"app_conns"`
	AppSettled  uint64 `json:"app_settled"`
}

// Waiting is how many of the counted connections the proxy has not accepted
// yet.
func (c ProxyConnCounts) Waiting() uint64 {
	if c.Accepted >= c.Established {
		return 0
	}
	return c.Established - c.Accepted
}

// ErrProxyConnsUnsupported reports that an agent cannot count the connections
// waiting for its proxy (hooks or a proxy without the counters: other
// platforms, an older build, a listener that is not marked or has closed, a
// count that was lost). It means "unknown", never "none waiting".
var ErrProxyConnsUnsupported = errors.New("this agent cannot count the connections waiting for its proxy")
