package mysql

import "strings"

// A server-side prepared statement spans several commands, and so several
// recorded mocks: COM_STMT_PREPARE sends the query and its COM_STMT_PREPARE_OK
// answers with a statement ID, then COM_STMT_EXECUTE, COM_STMT_SEND_LONG_DATA,
// COM_STMT_RESET and COM_STMT_CLOSE name the statement by that ID alone. The
// ID means something only on the connection that prepared it, and only
// through that PREPARE.

// MetaQuery is the key, in the Meta of a recorded command that names a
// prepared statement by its ID, under which the recorder keeps the query the
// statement was prepared with on its connection. It makes the command's mock
// readable without the PREPARE's, which a test set can lose (keploy #4810:
// the mock prune deleted PREPAREs it judged unused).
//
// It is kept in Meta, not in a field of the command's packet, so a keploy
// that does not know it keeps it too: Meta decodes into the same map in every
// keploy that writes it, and a rewrite of the mock file (the prune, the noise
// write-back) writes it back. An unknown packet field would be dropped by the
// first such rewrite.
const MetaQuery = "query"

// StatementID reports the prepared statement a command names by its ID:
// COM_STMT_EXECUTE, COM_STMT_SEND_LONG_DATA, COM_STMT_RESET, COM_STMT_CLOSE
// and COM_STMT_FETCH.
func StatementID(msg interface{}) (uint32, bool) {
	switch m := msg.(type) {
	case *StmtExecutePacket:
		if m != nil {
			return m.StatementID, true
		}
	case *StmtSendLongDataPacket:
		if m != nil {
			return m.StatementID, true
		}
	case *StmtResetPacket:
		if m != nil {
			return m.StatementID, true
		}
	case *StmtClosePacket:
		if m != nil {
			return m.StatementID, true
		}
	case *StmtFetchPacket:
		if m != nil {
			return m.StatementID, true
		}
	}
	return 0, false
}

// Prepared reports the statement an exchange prepared: the ID the server's
// COM_STMT_PREPARE_OK (resp) gave the query of the COM_STMT_PREPARE (req).
// ok is false for any other exchange, and for a PREPARE of no query.
func Prepared(req, resp interface{}) (stmtID uint32, query string, ok bool) {
	sp, isPrepare := req.(*StmtPreparePacket)
	spok, isPrepareOk := resp.(*StmtPrepareOkPacket)
	if !isPrepare || !isPrepareOk || sp == nil || spok == nil {
		return 0, "", false
	}
	query = strings.TrimSpace(sp.Query)
	if query == "" {
		return 0, "", false
	}
	return spok.StatementID, query, true
}

// StoredQuery returns the query recorded on a command that names a prepared
// statement (see MetaQuery), or "" when the recording kept none: a recording
// made before keploy kept it, or a statement the recorder never saw prepared.
func (p *PacketBundle) StoredQuery() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Meta[MetaQuery])
}

// SetStoredQuery records query on a command that names a prepared statement
// (see MetaQuery). It copies Meta before writing, so a map the bundle shares
// with another is left as it was. An empty query records nothing.
func (p *PacketBundle) SetStoredQuery(query string) {
	query = strings.TrimSpace(query)
	if p == nil || query == "" {
		return
	}
	meta := make(map[string]string, len(p.Meta)+1)
	for k, v := range p.Meta {
		meta[k] = v
	}
	meta[MetaQuery] = query
	p.Meta = meta
}
