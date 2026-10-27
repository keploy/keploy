package replayer

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// preparedIndex files the mocks commands are matched against, so matchCommand
// finds them in the session tier through the store's index
// (integrations.SessionKeyReader) instead of walking the whole pool for every
// COM_STMT_PREPARE, COM_STMT_CLOSE and, while no test window is open,
// COM_QUERY and COM_STMT_EXECUTE. No window is open under exact serving (keploy
// mock replay), and in keploy test only before a set's first test: a test's
// window stays open until the next test is staged. In lax mode that tier holds
// every test's traffic, and a walk per command cost more with every test the
// set held.
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
	// keyQuery + a statement's identity (sqlStatementIdentity): a mock with a
	// COM_QUERY request of that statement. A recorded COM_QUERY is an exact
	// match for a live one only when the two identities are equal
	// (matchQueryLive).
	keyQuery = "Q\x00"
	// keyExecute + the case folding (foldKey) of the identity of the query a
	// COM_STMT_EXECUTE request recorded (mysql.PacketBundle.StoredQuery): a
	// mock with such a request. A recorded EXECUTE matches a live one on its
	// query, definitively or for the FIFO fallback, only when the two
	// identities are equal ignoring case (matchStmtExecutePacketQueryAware).
	keyExecute = "E\x00"
	// keyExecuteUnnamed: a mock with a COM_STMT_EXECUTE request that recorded
	// no query. Its statement is the one its PREPARE in the pool names, which
	// no key of its own can say, so a live EXECUTE is matched by the full scan
	// while the session tier holds one.
	keyExecuteUnnamed = "E?"
)

var (
	comStmtPrepare = mysql.CommandStatusToString(mysql.COM_STMT_PREPARE)
	comStmtClose   = mysql.CommandStatusToString(mysql.COM_STMT_CLOSE)
	comQuery       = mysql.CommandStatusToString(mysql.COM_QUERY)
	comStmtExecute = mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE)
)

// appendKey appends key to keys unless keys holds it.
func appendKey(keys []string, key string) []string {
	if slices.Contains(keys, key) {
		return keys
	}
	return append(keys, key)
}

// foldKey is s case-folded so that foldKey(a) == foldKey(b) exactly when
// strings.EqualFold(a, b): each rune, an invalid byte read as
// utf8.RuneError as EqualFold reads it, becomes the smallest rune of its
// simple case folding orbit (unicode.SimpleFold). An ASCII letter's is its
// upper case.
func foldKey(s string) string {
	ascii, upper := true, true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			ascii = false
			break
		}
		if 'a' <= c && c <= 'z' {
			upper = false
		}
	}
	if ascii {
		if upper {
			return s
		}
		b := []byte(s)
		for i, c := range b {
			if 'a' <= c && c <= 'z' {
				b[i] = c - ('a' - 'A')
			}
		}
		return string(b)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < least {
				least = f
			}
		}
		b.WriteRune(least)
	}
	return b.String()
}

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
		case comQuery:
			if qp, ok := r.PacketBundle.Message.(*mysql.QueryPacket); ok && qp != nil {
				keys = appendKey(keys, keyQuery+sqlStatementIdentity(qp.Query))
			}
		case comStmtExecute:
			if ep, ok := r.PacketBundle.Message.(*mysql.StmtExecutePacket); ok && ep != nil {
				if q := r.PacketBundle.StoredQuery(); q != "" {
					keys = appendKey(keys, keyExecute+foldKey(sqlStatementIdentity(q)))
				} else {
					keys = appendKey(keys, keyExecuteUnnamed)
				}
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
	// MySQL matcher now reads the typed Lifetime with a defensive
	// fallback to the raw metadata tag. This handles both the
	// fully-migrated path (DeriveLifetime has run, Lifetime is
	// set) and the edge case where a mock reached the pool
	// without DeriveLifetime having set Lifetime — the raw tag
	// still says config so we skip it correctly. It is checked last:
	// the tag is a map lookup, and only a PREPARE mock needs it. Every
	// check here is a condition the entry needs, so their order does
	// not change which mocks make one.
	if m.TestModeInfo.Lifetime == models.LifetimeSession ||
		(m.TestModeInfo.Lifetime == models.LifetimePerTest && hasConfigTag(m)) {
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
