package replayer

import (
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// phaseFilterMocks is every shape the command-phase filter and
// recordedPrepEntry tell apart: each tier (and an unknown one), with no
// metadata, an empty one, and a "type" tag of config, mocks or connection;
// with no request, one or two, each request a COM_* command, another packet
// type or one with no header; each first response a PREPARE_OK, a result set
// or none, and each first request a PREPARE of a query, of only spaces, or
// another command.
func phaseFilterMocks() []*models.Mock {
	lifetimes := []models.Lifetime{models.LifetimePerTest, models.LifetimeSession, models.LifetimeConnection, models.Lifetime(42)}
	metas := []map[string]string{nil, {}, {"type": "config"}, {"type": "mocks"}, {"type": "connection", "connID": "c9"}}
	reqOf := func(kind string) mysql.Request {
		switch kind {
		case "prepare":
			return mysql.Request{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "COM_STMT_PREPARE"}, Message: &mysql.StmtPreparePacket{Query: " SELECT ? "}}}
		case "prepare-blank":
			return mysql.Request{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "COM_STMT_PREPARE"}, Message: &mysql.StmtPreparePacket{Query: "   "}}}
		case "query":
			return mysql.Request{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "COM_QUERY"}, Message: &mysql.QueryPacket{Query: "SELECT 1"}}}
		case "handshake":
			return mysql.Request{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "HANDSHAKE_RESPONSE41"}}}
		default: // no header
			return mysql.Request{}
		}
	}
	reqSets := [][]string{nil, {"prepare"}, {"prepare-blank"}, {"query"}, {"handshake"}, {"none"}, {"prepare", "query"}, {"handshake", "query"}}
	respOf := map[string][]mysql.Response{
		"none":       nil,
		"prepareOK":  {{PacketBundle: mysql.PacketBundle{Message: &mysql.StmtPrepareOkPacket{StatementID: 3}}}},
		"nilOK":      {{PacketBundle: mysql.PacketBundle{Message: (*mysql.StmtPrepareOkPacket)(nil)}}},
		"result set": {{PacketBundle: mysql.PacketBundle{Message: &mysql.TextResultSet{}}}},
	}
	var out []*models.Mock
	for _, lt := range lifetimes {
		for mi, meta := range metas {
			for _, rs := range reqSets {
				for rk, resp := range respOf {
					m := &models.Mock{Kind: models.MySQL, Name: fmt.Sprintf("lt%d-meta%d-%s-%s", lt, mi, strings.Join(rs, "+"), rk)}
					m.TestModeInfo.Lifetime = lt
					m.Spec.Metadata = meta
					for _, k := range rs {
						m.Spec.MySQLRequests = append(m.Spec.MySQLRequests, reqOf(k))
					}
					m.Spec.MySQLResponses = resp
					out = append(out, m)
				}
			}
		}
	}
	return out
}

// TestCommandPhaseFilterSkipsWhatMainSkips: the command phase skips exactly
// the mocks main's filter skipped, now that the shape check runs before the
// tier is read: main's filter is the reference, over every shape the two
// conditions tell apart.
func TestCommandPhaseFilterSkipsWhatMainSkips(t *testing.T) {
	mains := func(mock *models.Mock) bool {
		if mock.TestModeInfo.Lifetime == models.LifetimeSession ||
			(mock.TestModeInfo.Lifetime == models.LifetimePerTest && hasConfigTag(mock)) {
			if !isSessionReusableCommandMock(mock) {
				return true
			}
		}
		return false
	}
	skipped := 0
	for _, m := range phaseFilterMocks() {
		want := mains(m)
		if got := skippedAtCommandPhase(m); got != want {
			t.Errorf("%s: skipped %v, main skipped %v", m.Name, got, want)
		}
		if want {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatal("no shape is skipped: the table does not exercise the filter")
	}
}

// TestRecordedPrepEntryIsMainsEntry: recordedPrepEntry files the same entry,
// under the same connection, for every shape as main's, which read the tier
// before the shape.
func TestRecordedPrepEntryIsMainsEntry(t *testing.T) {
	mains := func(m *models.Mock) (string, prepEntry, bool) {
		if m == nil || m.Kind != models.MySQL {
			return "", prepEntry{}, false
		}
		if m.TestModeInfo.Lifetime == models.LifetimeSession ||
			(m.TestModeInfo.Lifetime == models.LifetimePerTest && hasConfigTag(m)) {
			return "", prepEntry{}, false
		}
		if len(m.Spec.MySQLResponses) == 0 || len(m.Spec.MySQLRequests) == 0 {
			return "", prepEntry{}, false
		}
		spok, ok := m.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
		if !ok || spok == nil {
			return "", prepEntry{}, false
		}
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
	filed := 0
	mocks := append(phaseFilterMocks(), nil, &models.Mock{Kind: models.Postgres})
	for _, m := range mocks {
		wc, we, wok := mains(m)
		gc, ge, gok := recordedPrepEntry(m)
		if gc != wc || ge != we || gok != wok {
			name := "<nil>"
			if m != nil {
				name = m.Name
			}
			t.Errorf("%s: entry (%q, %+v, %v), main's (%q, %+v, %v)", name, gc, ge, gok, wc, we, wok)
		}
		if wok {
			filed++
		}
	}
	if filed == 0 {
		t.Fatal("no shape files an entry: the table does not exercise recordedPrepEntry")
	}
}
