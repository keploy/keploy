package util

import (
	"errors"
	"net"

	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// IsAlreadyClosedErr reports whether err, returned by Close on a proxied
// connection, says only that the connection was already gone: the proxy closed
// it before, or the peer closed or reset it first. Such a Close can neither lose
// nor recover anything: any loss shows on the reads and writes before it.
//
// The usual shape is a TLS connection whose peer hung up first. tls.Conn.Close
// still writes a close_notify alert, the write fails, and the error comes back
// as "tls: failed to send closeNotify alert (but connection was closed anyway):
// write tcp ...: write: broken pipe". crypto/tls wraps the write error with %w,
// so it is matched through the chain here, never by its text.
//
// Every other Close error (an I/O error, a close_notify that timed out against
// a peer that stopped reading, ...) is not covered and stays a real error.
func IsAlreadyClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) ||
		neterr.IsBrokenPipe(err) ||
		neterr.IsConnReset(err)
}

// LogCloseErr logs err, the result of closing a proxied connection, under msg.
// A Close that failed only because the connection was already closed by
// either side (IsAlreadyClosedErr) is logged at Debug: it is the normal end of a
// connection whose peer hung up first, and must not read as a keploy failure.
// Any other Close error is logged at Error. A nil err logs nothing.
func LogCloseErr(logger *zap.Logger, err error, msg string, fields ...zap.Field) {
	if err == nil {
		return
	}
	if logger != nil && IsAlreadyClosedErr(err) {
		logger.Debug(msg, append(fields, zap.Error(err))...)
		return
	}
	utils.LogError(logger, err, msg, fields...)
}
