package replayer

import (
	"reflect"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// isConsumablePerTestMock reports whether mk is a per-test data mock consumed
// on match (rather than a reusable session/connection/config mock).
func isConsumablePerTestMock(mk *models.Mock) bool {
	if mk == nil || mk.Name == "" {
		return false
	}
	switch mk.TestModeInfo.Lifetime {
	case models.LifetimeSession, models.LifetimeConnection:
		return false
	}
	if mk.Spec.Metadata != nil && mk.Spec.Metadata["type"] == "config" {
		return false
	}
	return true
}

// sameRecordedResponse reports whether a and b carry identical recorded MySQL
// responses (header types, payload strings, and exported message fields —
// deliberately ignoring unexported wire-encoding caches on result sets).
func sameRecordedResponse(a, b *models.Mock) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	ra, rb := a.Spec.MySQLResponses, b.Spec.MySQLResponses
	if len(ra) == 0 || len(ra) != len(rb) {
		return false
	}
	for i := range ra {
		if ra[i].Payload != rb[i].Payload {
			return false
		}
		ha, hb := ra[i].Header, rb[i].Header
		if (ha == nil) != (hb == nil) {
			return false
		}
		if ha != nil && ha.Type != hb.Type {
			return false
		}
		if !sameMySQLMessage(ra[i].Message, rb[i].Message) {
			return false
		}
	}
	return true
}

func sameMySQLMessage(a, b interface{}) bool {
	switch ma := a.(type) {
	case *mysql.TextResultSet:
		mb, ok := b.(*mysql.TextResultSet)
		if !ok || (ma == nil) != (mb == nil) {
			return false
		}
		if ma == nil {
			return true
		}
		return ma.ColumnCount == mb.ColumnCount &&
			reflect.DeepEqual(ma.Columns, mb.Columns) &&
			reflect.DeepEqual(ma.EOFAfterColumns, mb.EOFAfterColumns) &&
			reflect.DeepEqual(ma.Rows, mb.Rows) &&
			reflect.DeepEqual(ma.FinalResponse, mb.FinalResponse)
	case *mysql.BinaryProtocolResultSet:
		mb, ok := b.(*mysql.BinaryProtocolResultSet)
		if !ok || (ma == nil) != (mb == nil) {
			return false
		}
		if ma == nil {
			return true
		}
		return ma.ColumnCount == mb.ColumnCount &&
			reflect.DeepEqual(ma.Columns, mb.Columns) &&
			reflect.DeepEqual(ma.EOFAfterColumns, mb.EOFAfterColumns) &&
			reflect.DeepEqual(ma.Rows, mb.Rows) &&
			reflect.DeepEqual(ma.FinalResponse, mb.FinalResponse)
	default:
		return reflect.DeepEqual(a, b)
	}
}

// txSpanOf computes the [txStart, txEnd] timestamp span of the recorded
// transaction containing mk on mk's connID within the current window's
// candidates. Returns ok=false if mk has no connID or no multi-command span can
// be determined.
func txSpanOf(mk *models.Mock, windowMocks []*models.Mock, winEnd time.Time) (start, end time.Time, ok bool) {
	if mk == nil || mk.Spec.ReqTimestampMock.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	cid := mk.Spec.Metadata["connID"]
	if cid == "" {
		return time.Time{}, time.Time{}, false
	}
	var connMocks []*models.Mock
	for _, c := range windowMocks {
		if c == nil || c.Kind != models.MySQL || c.Spec.Metadata["connID"] != cid || c.Spec.ReqTimestampMock.IsZero() {
			continue
		}
		connMocks = append(connMocks, c)
	}
	if len(connMocks) < 2 {
		return time.Time{}, time.Time{}, false
	}
	sort.SliceStable(connMocks, func(i, j int) bool {
		ti, tj := connMocks[i].Spec.ReqTimestampMock, connMocks[j].Spec.ReqTimestampMock
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return connMocks[i].TestModeInfo.SortOrder < connMocks[j].TestModeInfo.SortOrder
	})
	idx := -1
	for i, c := range connMocks {
		if c == mk || (c.Name != "" && c.Name == mk.Name) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return time.Time{}, time.Time{}, false
	}
	mockTxCtrl := func(m *models.Mock) txControl {
		if len(m.Spec.MySQLRequests) == 0 {
			return txNone
		}
		return txControlOf(m.Spec.MySQLRequests[0])
	}
	mockInTx := func(m *models.Mock) (inTx, known bool) {
		st, ok := answeredStatus(m)
		if !ok {
			return false, false
		}
		return st&serverStatusInTrans != 0, true
	}

	lo, hi := idx, idx
	for i := idx - 1; i >= 0; i-- {
		ctrl := mockTxCtrl(connMocks[i])
		if ctrl == txEnd || ctrl == txReset {
			break
		}
		if ctrl == txBegin {
			lo = i
			break
		}
		if inTx, known := mockInTx(connMocks[i]); known && !inTx {
			break
		}
		lo = i
	}
	for i := idx + 1; i < len(connMocks); i++ {
		ctrl := mockTxCtrl(connMocks[i])
		if ctrl == txBegin || ctrl == txReset {
			break
		}
		hi = i
		if ctrl == txEnd {
			break
		}
		if inTx, known := mockInTx(connMocks[i]); known && !inTx {
			break
		}
	}
	start = connMocks[lo].Spec.ReqTimestampMock
	end = connMocks[hi].Spec.ReqTimestampMock
	if res := connMocks[hi].Spec.ResTimestampMock; !res.IsZero() && res.After(end) && (winEnd.IsZero() || !res.After(winEnd)) {
		end = res
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

// queryMockSQL returns the executable SQL text of a COM_QUERY mock, or "".
func queryMockSQL(mk *models.Mock) string {
	if mk == nil || len(mk.Spec.MySQLRequests) == 0 {
		return ""
	}
	qp, _ := mk.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
	if qp == nil {
		return ""
	}
	return sqlStatementIdentity(qp.Query)
}

// tallyOwnership reports to the store what the window ownership rules decided
// for a call answered by answered (integrations.OwnershipTally): the edge rule
// gave it (rank 1, pointOwnerRank), the first exact match the walk met
// (first) was kept from answering at once (deferred) and another recording
// answered, or a drifted query's ranking chose it over a higher score
// (driftRanked).
func tallyOwnership(t integrations.OwnershipTally, answered *models.Mock, rank int, first *models.Mock, deferred, driftRanked bool) {
	if answered == nil {
		return
	}
	if rank == 1 {
		t.TallyOwnership(integrations.OwnershipEdgeAnswered)
	}
	if first != nil && deferred {
		t.TallyOwnership(integrations.OwnershipDeferred)
		if first.Name != answered.Name {
			t.TallyOwnership(integrations.OwnershipPassedOver)
		}
	}
	if driftRanked {
		t.TallyOwnership(integrations.OwnershipDriftRanked)
	}
}

// reclaimCand is a window reclaim's candidate (a consumed in-window mock of
// the live call, integrations.WindowReclaim.ReclaimableInWindow) with what the
// matcher ranks waiting matches by: recorded on the bound connection, its
// recorded transaction status agreeing with the live connection's, and its
// window ownership rank.
type reclaimCand struct {
	mock    *models.Mock
	onBound bool
	txOk    bool
	rank    int
}
