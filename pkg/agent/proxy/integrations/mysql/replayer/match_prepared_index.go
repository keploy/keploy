package replayer

import (
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// preparedIndex files the mocks prepared statements are matched against, so
// matchCommand finds them in the session tier through the store's index
// (integrations.SessionKeyReader) instead of walking the whole pool for every
// COM_STMT_PREPARE, COM_STMT_EXECUTE and COM_STMT_CLOSE. In lax mode that tier
// holds every test's traffic, and a walk per command cost more with every test
// the set held.
var preparedIndex = &integrations.MockIndex{Keys: preparedKeys}

const (
	// keyPrepare + a statement's identity (sqlStatementIdentity): a mock with a
	// COM_STMT_PREPARE request of that statement. A recorded PREPARE is an
	// exact match for a live one only when the two identities are equal.
	keyPrepare = "P\x00"
	// keyPrepEntry + connID + NUL + statement ID: a mock buildRecordedPrepIndex
	// takes as the PREPARE of that statement on that recorded connection.
	keyPrepEntry = "S\x00"
	// keyClose: a mock with a COM_STMT_CLOSE request, the only requests a
	// live CLOSE scores against.
	keyClose = "C"
)

var (
	comStmtPrepare = mysql.CommandStatusToString(mysql.COM_STMT_PREPARE)
	comStmtClose   = mysql.CommandStatusToString(mysql.COM_STMT_CLOSE)
)

func prepEntryKey(connID string, stmtID uint32) string {
	return keyPrepEntry + connID + "\x00" + strconv.FormatUint(uint64(stmtID), 10)
}

// preparedKeys is preparedIndex's Keys. Each key depends only on the mock's
// recording and its Lifetime, which do not change while it is pooled.
func preparedKeys(m *models.Mock) []string {
	if m == nil || m.Kind != models.MySQL {
		return nil
	}
	var keys []string
	closeFiled := false
	for _, r := range m.Spec.MySQLRequests {
		h := r.PacketBundle.Header
		if h == nil {
			continue
		}
		switch h.Type {
		case comStmtPrepare:
			if sp, ok := r.PacketBundle.Message.(*mysql.StmtPreparePacket); ok && sp != nil {
				keys = append(keys, keyPrepare+sqlStatementIdentity(sp.Query))
			}
		case comStmtClose:
			if !closeFiled {
				keys = append(keys, keyClose)
				closeFiled = true
			}
		}
	}
	if connID, e, ok := recordedPrepEntry(m); ok {
		keys = append(keys, prepEntryKey(connID, e.statementID))
	}
	return keys
}

// recordedPrepEntry reports the entry buildRecordedPrepIndex makes of m, and
// the recorded connection it files it under, if it makes one: a MySQL mock
// that is not a session or config one, whose first request is a
// COM_STMT_PREPARE of a non-empty query and whose first response is its
// COM_STMT_PREPARE_OK.
func recordedPrepEntry(m *models.Mock) (string, prepEntry, bool) {
	if m == nil || m.Kind != models.MySQL {
		return "", prepEntry{}, false
	}
	// MySQL matcher now reads the typed Lifetime with a defensive
	// fallback to the raw metadata tag. This handles both the
	// fully-migrated path (DeriveLifetime has run, Lifetime is
	// set) and the edge case where a mock reached the pool
	// without DeriveLifetime having set Lifetime — the raw tag
	// still says config so we skip it correctly.
	if m.TestModeInfo.Lifetime == models.LifetimeSession ||
		(m.TestModeInfo.Lifetime == models.LifetimePerTest && hasConfigTag(m)) {
		return "", prepEntry{}, false
	}
	if len(m.Spec.MySQLResponses) == 0 || len(m.Spec.MySQLRequests) == 0 {
		return "", prepEntry{}, false
	}
	// The statement ID is the first response's, if it is a StmtPrepareOkPacket.
	spok, ok := m.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
	if !ok || spok == nil {
		return "", prepEntry{}, false
	}
	// The query is the first request's, if it is a StmtPreparePacket.
	sp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.StmtPreparePacket)
	if !ok || sp == nil {
		return "", prepEntry{}, false
	}
	prepQuery := strings.TrimSpace(sp.Query)
	if prepQuery == "" {
		return "", prepEntry{}, false
	}
	connID := ""
	if m.Spec.Metadata != nil {
		connID = m.Spec.Metadata["connID"]
	}
	return connID, prepEntry{statementID: spok.StatementID, query: prepQuery, mockName: m.Name}, true
}

// prepKey names a recorded statement: the connection its mock recorded and its
// statement ID.
type prepKey struct {
	connID string
	stmtID uint32
}

// firstPrepEntries maps each recorded statement to the query of its first entry
// in mocks, in order.
func firstPrepEntries(mocks []*models.Mock) map[prepKey]string {
	out := map[prepKey]string{}
	for _, m := range mocks {
		if connID, e, ok := recordedPrepEntry(m); ok {
			k := prepKey{connID, e.statementID}
			if _, seen := out[k]; !seen {
				out[k] = e.query
			}
		}
	}
	return out
}
