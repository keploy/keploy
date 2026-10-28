package replayer

import (
	"context"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A BACKGROUND COMMAND RUN AGAIN IN ITS TEST'S WINDOW (under exact serving,
// the proven switch; keploy's HTTP and Generic parsers and the integrations
// Mongo parser do the same).
//
// Judging (keploy enterprise's) marks a test's recording as the
// application's own background work (mappings.yaml test entry: background:
// <why>): a timer, a start-up job that outlives the health check, a delayed
// write. The replay serves it in the test's window and never requires it.
// Such a job may run more often at replay than it did at record: its
// command then comes once more than the test recorded it, after the marked
// recording answered, and exact serving answers a recording once. Without
// this the command misses, the connection closes, and the test fails for
// the application's own background work.
//
// Such a command is answered again with the marked recording it repeats,
// unchecked, and reported as such (models.AnswerBackgroundAgain, naming the
// recording): it counts against no test. It is asked only once the
// matcher's exact matches found nothing, so whatever answered the command
// exactly before a mark still does; and before its answers by score or by
// another parameter's recording, which would spend a recording of the
// test's own that a later command needs.
//
// What repeats a recording is what the matcher's exact match reads, never
// its scores: a COM_QUERY with the same statement (matchQueryLive: the
// statement's identity, an observability comment aside), a COM_STMT_EXECUTE
// of the same statement (the query its recording kept, mysql.MetaQuery,
// against the live statement's) with every parameter the same, the
// recording's noise aside (matchStmtExecutePacketQueryAware's query-exact
// match). A command only like a marked recording (another literal, another
// parameter, a statement of the same shape) may be the test's own, and
// stays a miss. Not answered again:
//   - an EXECUTE whose recording kept no query (an older keploy's): which
//     statement it ran is not on the recording, and it stays a miss;
//   - a COM_STMT_PREPARE: the matcher already answers a PREPARE no recording
//     matches with a PREPARE_OK of its own;
//   - any other command (PING, INIT_DB, ...): the session's recordings
//     answer those, and a test's own seldom are background work.

// backgroundAgain is, for a command no recording answered under exact
// serving, the recording of the open test that judging marked as the
// application's background work, that already answered in the test's window
// (integrations.AnsweredBackgroundReader), and that the command repeats (see
// above): the first recorded of them. It reads the marked recordings only:
// nothing is consumed or bound. nil when exact serving is off, the store
// marks nothing, or the command repeats none.
func backgroundAgain(logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) *models.Mock {
	if !integrations.ExactServing(mockDb) {
		return nil
	}
	br, ok := mockDb.(integrations.AnsweredBackgroundReader)
	if !ok {
		return nil
	}
	var marked []*models.Mock
	for _, mk := range br.AnsweredBackground() {
		if mk != nil && mk.Kind == models.MySQL && len(mk.Spec.MySQLRequests) == 1 && len(mk.Spec.MySQLResponses) > 0 {
			marked = append(marked, mk)
		}
	}
	// In recorded order, so which of two recordings of the same command
	// answers again does not depend on which answered first.
	slices.SortStableFunc(marked, func(a, b *models.Mock) int {
		return a.Spec.ReqTimestampMock.Compare(b.Spec.ReqTimestampMock)
	})
	for _, mk := range marked {
		if repeats(logger, mk, req, decodeCtx) {
			return mk
		}
	}
	return nil
}

// repeats reports whether the live command req is the command recording mk
// recorded, as backgroundAgain reads it.
func repeats(logger *zap.Logger, mk *models.Mock, req mysql.Request, decodeCtx *wire.DecodeContext) bool {
	recorded := mk.Spec.MySQLRequests[0].PacketBundle
	// Which command each side is, is its message's type (only a COM_QUERY
	// decodes to a QueryPacket, only a COM_STMT_EXECUTE to a
	// StmtExecutePacket), and both matchers compare the header's type too.
	if recorded.Header == nil || req.Header == nil {
		return false
	}
	switch act := req.Message.(type) {
	case *mysql.QueryPacket:
		exp, _ := recorded.Message.(*mysql.QueryPacket)
		if exp == nil || act == nil {
			return false
		}
		var live liveQuery
		// A COM_QUERY an older keploy recorded with its query attributes
		// read out of step is compared with the live one read the same way,
		// as the matcher compares it.
		if legacyRecordedQuery(exp) && readsLegacy(act) {
			live.fill(act.LegacyQuery)
		}
		ok, _, _ := matchQueryLive(logger, recorded, req.PacketBundle, &live, queryPacketText)
		return ok
	case *mysql.StmtExecutePacket:
		exp, _ := recorded.Message.(*mysql.StmtExecutePacket)
		if exp == nil || act == nil || decodeCtx == nil || decodeCtx.StmtIDToQuery == nil {
			return false
		}
		expectedQuery := recorded.StoredQuery()
		actualQuery := strings.TrimSpace(decodeCtx.StmtIDToQuery[act.StatementID])
		if expectedQuery == "" || actualQuery == "" {
			return false
		}
		liveParams := -1
		if prep := decodeCtx.PreparedStatements[act.StatementID]; prep != nil {
			liveParams = int(prep.NumParams)
		}
		ok, _, queryExact := matchStmtExecutePacketQueryAware(logger, recorded, req.PacketBundle, expectedQuery, actualQuery, liveParams, mk.Name, util.NewNoiseChecker(mk.Noise))
		return ok && queryExact
	}
	return false
}

// answerAgain is the answer to a command that repeats marked recording mk
// (backgroundAgain): a copy of its recorded response, as the matcher hands
// out a match.
func answerAgain(mk *models.Mock) *mysql.Response {
	return &mysql.Response{PacketBundle: mk.Spec.MySQLResponses[0].PacketBundle, Payload: mk.Spec.MySQLResponses[0].Payload}
}

// reportBackgroundAgain reports a command answered again
// (models.AnswerBackgroundAgain): it repeats mk, a recording of the open test
// that judging marked as the application's background work and that already
// answered in its window (backgroundAgain). It counts against no test; the
// replay lists it under the test's answered calls, naming mk.
func reportBackgroundAgain(ctx context.Context, req mysql.Request, decodeCtx *wire.DecodeContext, dst string, mk *models.Mock) {
	r := mismatch.NewReport(mismatch.ProtocolMySQL, commandSummary(req, decodeCtx)).WithDestination(dst).Build()
	r.Answer, r.ClosestMock = models.AnswerBackgroundAgain, mk.Name
	models.ReportAnswer(ctx, r)
}
