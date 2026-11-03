package relay

import (
	"context"
	"net"
	"time"
)

// ResetClientOnClose resets the application's TCP socket under c, once it has
// acknowledged every byte written to it, as resetClientOnClose does for the
// relay: replay enacts a recorded reset with it. It reports whether the
// socket was reset; when it was not (the bytes not acknowledged within grace,
// ctx ended, or a platform where the socket cannot say what it holds), the
// caller's close stays a FIN.
func ResetClientOnClose(ctx context.Context, c net.Conn, grace time.Duration) bool {
	return resetClientOnClose(ctx, c, grace)
}
