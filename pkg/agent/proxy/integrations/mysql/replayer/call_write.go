package replayer

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// callWrite is what a MySQL recording's command writes, for
// models.RegisterCallWrite: "MySQL UPDATE" for a statement whose first
// keyword (inert comments aside: sqlStatementIdentity) changes what the
// server holds: INSERT, UPDATE, DELETE, REPLACE, LOAD DATA, LOAD XML, and
// the statements that change its tables, indexes or grants (CREATE, ALTER,
// DROP, TRUNCATE, RENAME, GRANT, REVOKE). The statement is a COM_QUERY's
// text, or the query a COM_STMT_EXECUTE's recording kept (mysql.MetaQuery).
//
// It names the statement's kind, never a table: one statement may name
// several, and only a parse of the whole statement says which it writes.
//
// Any other statement is a read or one keploy cannot tell, so none is a
// write: a SELECT (FOR UPDATE too: it locks, it changes nothing), SHOW, SET,
// a transaction's BEGIN or COMMIT, a CALL (what a procedure does is not on
// the recording), a WITH (its body may be a SELECT or an UPDATE), a
// statement that starts with an executable comment (/*! ... */), and an
// EXECUTE whose recording kept no query (an older keploy's). So is any other
// command (a PREPARE runs nothing), and a recording of more than one
// request. A COM_QUERY of several statements is told by its first.
func callWrite(m *models.Mock) (string, bool) {
	if m == nil || len(m.Spec.MySQLRequests) != 1 {
		return "", false
	}
	bundle := m.Spec.MySQLRequests[0].PacketBundle
	var stmt string
	switch msg := bundle.Message.(type) {
	case *mysql.QueryPacket:
		if msg == nil {
			return "", false
		}
		stmt = msg.Query
	case *mysql.StmtExecutePacket:
		stmt = bundle.StoredQuery()
	}
	verb, ok := statementWrite(stmt)
	if !ok {
		return "", false
	}
	return "MySQL " + verb, true
}

// writeKeywords are the first keywords of the statements that write
// (callWrite), each with the second keyword it needs ("" for none).
var writeKeywords = map[string][]string{
	"INSERT":   {""},
	"UPDATE":   {""},
	"DELETE":   {""},
	"REPLACE":  {""},
	"LOAD":     {"DATA", "XML"},
	"CREATE":   {""},
	"ALTER":    {""},
	"DROP":     {""},
	"TRUNCATE": {""},
	"RENAME":   {""},
	"GRANT":    {""},
	"REVOKE":   {""},
}

// statementWrite is the kind of write statement stmt is ("UPDATE", "LOAD
// DATA"), by its first keyword (callWrite), and false for any other
// statement.
func statementWrite(stmt string) (string, bool) {
	s := sqlStatementIdentity(stmt)
	first, rest := sqlKeyword(s)
	seconds, ok := writeKeywords[first]
	if !ok {
		return "", false
	}
	for _, second := range seconds {
		if second == "" {
			return first, true
		}
		if next, _ := sqlKeyword(strings.TrimLeft(rest, sqlWhitespace)); next == second {
			return first + " " + second, true
		}
	}
	return "", false
}

// sqlKeyword is the word s starts with, upper-cased (ASCII letters, digits,
// "_" and "$"), and the text after it: "" when s starts with none.
func sqlKeyword(s string) (word, rest string) {
	end := 0
	for end < len(s) && isKeywordByte(s[end]) {
		end++
	}
	return strings.ToUpper(s[:end]), s[end:]
}

func isKeywordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$'
}

func init() {
	models.RegisterCallWrite(models.MySQL, callWrite)
}
