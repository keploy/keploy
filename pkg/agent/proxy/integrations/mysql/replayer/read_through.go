package replayer

import (
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// tryReadThrough attempts an exact-match fallback against MockManager's
// deduplicated session read-through tier for a read-only MySQL COM_QUERY
// strictly AFTER primary matching, refused-consume retries, and OwnRepeat miss.
//
// Invariants:
//   - Only single COM_QUERY SELECT statements classified read-only by
//     models.ReadThroughRequestKey can ever hit; writes, locking reads, and
//     non-SELECT commands return (nil, false).
//   - Propagates the connection's active database (decodeCtx.Database) so
//     identical queries across different tenant databases never collide.
//   - Refuses reads when decodeCtx.DatabaseUnknown or decodeCtx.InTx is set:
//     transactional reads can observe uncommitted writes and must never fall
//     back to the session read-through tier.
//   - Strictly exact match (zero fuzzy fallback).
//   - Never touches DeleteFilteredMock / UpdateUnFilteredMock, avoiding
//     refusedConsume failures in both KEPLOY_PROVEN unset and =1 modes.
func tryReadThrough(logger *zap.Logger, req mysql.Request, decodeCtx *wire.DecodeContext, mockDb integrations.MockMemDb) (*mysql.Response, bool) {
	if mockDb == nil {
		return nil, false
	}
	if decodeCtx != nil && (decodeCtx.DatabaseUnknown || decodeCtx.InTx) {
		return nil, false
	}
	switch req.Message.(type) {
	case *mysql.QueryPacket, mysql.QueryPacket:
	default:
		return nil, false
	}
	var meta map[string]string
	if decodeCtx != nil {
		if db := strings.TrimSpace(decodeCtx.Database); db != "" {
			meta = map[string]string{"database": db}
		}
		if port := strings.TrimSpace(decodeCtx.DestPort); port != "" {
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["destPort"] = port
		}
	}
	live := &models.Mock{
		Kind: models.MySQL,
		Spec: models.MockSpec{
			Metadata:      meta,
			MySQLRequests: []mysql.Request{req},
		},
	}
	matched, ok := integrations.MatchReadThrough(mockDb, live)
	if !ok || matched == nil || len(matched.Spec.MySQLResponses) == 0 {
		return nil, false
	}
	if logger != nil {
		logger.Debug("mysql: served exact-match fallback from session read-through tier",
			zap.String("mockName", matched.Name))
	}
	resp := matched.Spec.MySQLResponses[0]
	return &resp, true
}
