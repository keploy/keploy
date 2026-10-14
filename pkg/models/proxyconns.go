package models

import "errors"

// ProxyConnCounts are the counts a test's begin and end read about the
// connections to the agent's proxy, each only ever growing for the life of the
// agent process:
//
//   - Established: the connections the kernel has completed to the proxy's
//     listener (the one the hooks marked). accept() returns a connection
//     only after the kernel counted it, so every accepted connection is in
//     this count, and so is one still waiting in the accept queue.
//   - Accepted: the connections the proxy's listener has accepted. Read
//     before Established, it is never higher; when the two are equal, every
//     connection the kernel had counted has been accepted.
//   - ListenDrops: the packets the kernel dropped at the proxy's listener
//     (its sk_drops): a SYN or a final ACK it could not take, its accept
//     queue full among the causes. A connection dropped so is in neither
//     count until the client's retransmission completes it, so a test during
//     which this grew cannot be verified to hold every call its application
//     made: any change between two reads is a drop.
//
// A test's end (and begin) waits until every connection counted has been
// accepted (Agent.WaitProxyAccepted), so that a call the application made is
// in the proxy's hands before the test is judged (design v14 §5.2, verify-v14
// M1).
type ProxyConnCounts struct {
	Established uint64 `json:"established"`
	Accepted    uint64 `json:"accepted"`
	ListenDrops uint64 `json:"listen_drops"`
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
