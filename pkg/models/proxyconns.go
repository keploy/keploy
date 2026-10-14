package models

import "errors"

// ProxyConnCounts are two counts of the connections to the agent's proxy, both
// only ever growing for the life of the agent process:
//
//   - Established: the connections the kernel has completed to the proxy's
//     listener at the addresses the connect hooks redirect to. A connection
//     is counted the moment it can be accept()ed, so one that waits in the
//     listener's accept queue is in this count.
//   - Accepted: of the connections to those same addresses, the ones the
//     proxy has accepted.
//
// Established - Accepted is what is still waiting to be accepted. A test's end
// (and begin) waits for Accepted to reach the Established it read, so that a
// call the application made is in the proxy's hands before the test is judged
// (design v14 §5.2, verify-v14 M1).
type ProxyConnCounts struct {
	Established uint64 `json:"established"`
	Accepted    uint64 `json:"accepted"`
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
// platforms, an older build). It means "unknown", never "none waiting".
var ErrProxyConnsUnsupported = errors.New("this agent cannot count the connections waiting for its proxy")
