//go:build !linux

package relay

import (
	"context"
	"net"
	"time"
)

// resetClientOnClose: off Linux the client socket is not asked what it holds,
// so its close stays a FIN, as before.
func resetClientOnClose(context.Context, net.Conn, time.Duration) bool { return false }
