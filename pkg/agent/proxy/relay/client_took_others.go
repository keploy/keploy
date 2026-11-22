//go:build !linux && !darwin && !windows

package relay

import (
	"net"

	"go.uber.org/zap"
)

// canAskClientSockets: off Linux, macOS and Windows no client socket is asked:
// every destination chunk is teed as it is written, as before, and one
// numbered after the client's FIN carries fakeconn.Chunk.AfterClientClose.
const canAskClientSockets = false

func clientSocketOf(net.Conn, *zap.Logger) clientSocket { return nil }
