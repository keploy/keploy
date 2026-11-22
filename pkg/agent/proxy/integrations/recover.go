package integrations

import (
	"context"
	"fmt"
	"net"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// RecoverCtx is pUtil.Recover for a parser serving the connection of ctx
// (WithRequestHolder): a recovered panic first ends that connection's request
// aloft untold (InFlightOf, learn.Flight.EndUntold), then is logged and closes
// client and dest. A parser that panicked writes no more of an answer to the
// request aloft, while the close of the TLS layer it was handed writes TLS's
// close_notify, which the proxy's kept connection reads as an answer's first
// bytes and lands the request on: a serve told in it before the panic was read
// as one the application was given its answer to (Order.Of 1,1), where it was
// given none. Ended untold, each serve in it is in an order keploy cannot tell
// (0,0), which fails closed. With no request aloft on ctx's connection, or
// none told (the proven switch off), it is pUtil.Recover. ctx is the parser's
// own, never nil (InFlightOf reads it):
//
//	defer integrations.RecoverCtx(ctx, logger, clientConn, destConn)
func RecoverCtx(ctx context.Context, logger *zap.Logger, client, dest net.Conn) {
	if logger == nil {
		fmt.Println(pUtil.Emoji + "Failed to recover from panic. Logger is nil.")
		return
	}
	if r := recover(); r != nil {
		InFlightOf(ctx).EndUntold()
		pUtil.Recovered(logger, r, client, dest)
	}
}

// RecoverIntoCtx is pUtil.RecoverInto for a parser goroutine serving the
// connection of ctx: a recovered panic first ends that connection's request
// aloft untold, as RecoverCtx's does, then is logged, closes client and dest,
// and is the goroutine's result, pUtil.ErrParserPanicked:
//
//	defer close(errCh)
//	defer integrations.RecoverIntoCtx(ctx, logger, clientConn, destConn, errCh)
func RecoverIntoCtx(ctx context.Context, logger *zap.Logger, client, dest net.Conn, result chan<- error) {
	if logger == nil {
		fmt.Println(pUtil.Emoji + "Failed to recover from panic. Logger is nil.")
		return
	}
	if r := recover(); r != nil {
		InFlightOf(ctx).EndUntold()
		pUtil.Recovered(logger, r, client, dest)
		select {
		case result <- pUtil.ErrParserPanicked:
		default:
		}
	}
}
