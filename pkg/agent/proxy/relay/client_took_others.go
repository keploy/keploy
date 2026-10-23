//go:build !linux

package relay

import "net"

// canAskClientSockets: off Linux (macOS and Windows, where keploy's enterprise
// build records natively) no client socket is asked: every destination chunk
// is teed as it is written, as before, and one numbered after the client's
// FIN carries fakeconn.Chunk.AfterClientClose.
const canAskClientSockets = false

func clientSocketOf(net.Conn) clientSocket { return nil }
