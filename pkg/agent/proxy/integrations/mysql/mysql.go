// Package mysql provides the MySQL integration.
package mysql

import (
	"context"
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/recorder"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	integrations.Register(integrations.MYSQL, &integrations.Parsers{
		Initializer: New,
		Priority:    100,
	})
}

type MySQL struct {
	logger *zap.Logger
}

func New(logger *zap.Logger) integrations.Integrations {
	return &MySQL{
		logger: logger,
	}
}

func (m *MySQL) MatchType(_ context.Context, buf []byte) bool {
	// The default proxy path routes MySQL by destination port (3306), so
	// this matcher historically returned false — the caller already knew.
	// Capture paths that deliver bytes without a port context (e.g. TLS
	// uprobe streams, or any future transport that doesn't preserve the
	// L4 dest port) need a content-based fallback so MySQL plaintext is
	// routed to the MySQL parser rather than dropping into Generic.
	//
	// MySQL frames: 3-byte little-endian length + 1-byte sequence + body.
	//
	// Very strict detection — we need to avoid false positives on
	// HTTP POST bodies and arbitrary TLS record bytes.
	//
	// The two specific shapes we care about are both small packets
	// (< 256 bytes body) with a small sequence number (<= 2):
	//
	//   1. HandshakeV10 from the server: body starts with 0x0a and
	//      contains a null-terminated server version string.
	//
	//   2. HandshakeResponse41 from the client: 4-byte client caps with
	//      BOTH CLIENT_PROTOCOL_41 (0x00000200) and CLIENT_PLUGIN_AUTH
	//      (0x00080000) set — always true for modern drivers talking to
	//      MySQL 5.7+ with caching_sha2_password or mysql_native_password.
	//      Followed by 4-byte max_packet_size (typically 0x01000000 =
	//      16 MB) and 1-byte charset (0-255), 23 zero bytes reserved.
	//
	// Byte budget: we need at least the 4-byte header + 4 caps + 4
	// max_packet_size + 1 charset = 13 bytes to make a confident decision.
	if len(buf) < 16 {
		return false
	}
	pktLen := int(buf[0]) | int(buf[1])<<8 | int(buf[2])<<16
	seq := buf[3]
	// MySQL handshake packets are small (< 512 bytes) and always at
	// sequence 0 or 1. Anything else is highly unlikely to be a fresh
	// MySQL handshake — reject to avoid false positives on HTTP/binary
	// data whose first 3 bytes happen to form a sane length.
	if pktLen < 5 || pktLen > 512 || seq > 2 {
		return false
	}
	// The full packet must fit in the buffer (handshake is always short).
	if 4+pktLen > len(buf) {
		return false
	}
	// Slice to exactly the first MySQL packet's body — buf may carry
	// subsequent packets that would otherwise make the reserved-bytes
	// check pass spuriously when pktLen < 32.
	body := buf[4 : 4+pktLen]
	// Server greeting: starts with protocol version 0x0a (10).
	if body[0] == 0x0a {
		return true
	}
	// Client HandshakeResponse41 must fit the 4 caps bytes within this
	// packet's own body.
	if pktLen < 4 {
		return false
	}
	// Client HandshakeResponse41: first 4 body bytes = capability flags.
	caps := uint32(body[0]) | uint32(body[1])<<8 |
		uint32(body[2])<<16 | uint32(body[3])<<24
	const (
		clientProto41    = 0x00000200
		clientPluginAuth = 0x00080000
	)
	const requiredBits = clientProto41 | clientPluginAuth
	if caps&requiredBits != requiredBits {
		return false
	}
	// Sanity: the 23 reserved bytes at body[9:32] must all be zero.
	// Length check uses pktLen (== len(body)) so multi-packet buffers
	// can't satisfy this with bytes from the next packet.
	if pktLen < 32 {
		return false
	}
	for _, b := range body[9:32] {
		if b != 0 {
			return false
		}
	}
	return true
}

// IsV2 signals the proxy dispatcher that this parser consumes the new
// supervisor + relay + FakeConn architecture. The V2 path handles the
// MySQL mid-stream TLS upgrade (CLIENT_SSL capability bit) via the
// directive channel rather than touching the real sockets directly.
func (m *MySQL) IsV2() bool { return true }

func (m *MySQL) RecordOutgoing(ctx context.Context, session *integrations.RecordSession) error {
	if session != nil && session.V2 != nil {
		return m.recordV2(ctx, session)
	}
	return m.recordLegacy(ctx, session)
}

// recordLegacy is the byte-for-byte preserved pre-V2 record path. It
// is invoked when the session is not running under the supervisor +
// relay architecture (RecordSession.V2 == nil). Do not edit this body
// without a matching change in recordV2.
func (m *MySQL) recordLegacy(ctx context.Context, session *integrations.RecordSession) error {
	logger := session.Logger

	err := recorder.Record(ctx, logger, session.Ingress, session.Egress, session.Mocks, session.Opts, session.TLSUpgrader)

	if err != nil {
		utils.LogError(logger, err, "failed to encode the mysql message into the yaml")
		return err
	}
	return nil
}

// recordV2 delegates to the V2 recorder which consumes the supervisor
// Session's FakeConns and uses directives for the CLIENT_SSL TLS
// upgrade. The legacy record path is not entered on this code path.
func (m *MySQL) recordV2(ctx context.Context, session *integrations.RecordSession) error {
	logger := session.Logger
	err := recorder.RecordV2(ctx, logger, session.V2)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the mysql message into the yaml",
			zap.String("next_step", "set KEPLOY_NEW_RELAY=off to route MySQL through the legacy record path while investigating, or KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely (raw passthrough); the supervisor will have already fallen through to passthrough on the affected connection so user traffic continues"),
		)
		return err
	}
	return nil
}

func (m *MySQL) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := m.logger.With(zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.Any("Client IP Address", src.RemoteAddr().String()))
	err := replayer.Replay(ctx, logger, src, dstCfg, mockDb, opts)
	if err != nil && err != io.EOF {
		utils.LogError(logger, err, "failed to decode the mysql message from the yaml")
		return err
	}
	return nil
}
