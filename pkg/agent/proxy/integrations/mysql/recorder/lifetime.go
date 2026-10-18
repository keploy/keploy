package recorder

import (
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"vitess.io/vitess/go/vt/sqlparser"
)

// statedLifetime is the lifetime the recorder states for a MySQL mock
// (models.MetadataLifetimeKey), from the mock's type tag, its command and,
// for a COM_QUERY, its statement:
//
//   - session: the handshake and auth (tagged "config"), the commands whose
//     answer does not depend on the test (COM_PING, COM_STATISTICS,
//     COM_DEBUG, COM_RESET_CONNECTION, COM_INIT_DB, COM_SET_OPTION,
//     COM_CHANGE_USER, COM_QUIT), and the statements that set or read the
//     connection's state (sessionStateQuery);
//   - connection: a prepared statement's lifecycle on its connection
//     (COM_STMT_PREPARE, COM_STMT_CLOSE, COM_STMT_RESET), which the
//     statements that run it reference by id;
//   - per-test: everything else, the data: COM_STMT_EXECUTE, COM_STMT_FETCH,
//     COM_STMT_SEND_LONG_DATA, and any other statement or command.
//
// The type tag a mock is written with is left as it was (older replayers
// read it); the stated lifetime is what a replayer that knows the key
// derives the mock's lifetime from.
func statedLifetime(mockType, requestOperation string, requests []mysql.Request) string {
	switch mockType {
	case "config":
		return models.MetadataLifetimeSession
	case "connection":
		return models.MetadataLifetimeConnection
	}
	if models.IsMySQLStatementLifecycleCommandType(requestOperation) {
		return models.MetadataLifetimeConnection
	}
	switch requestOperation {
	case "COM_INIT_DB", "COM_CHANGE_USER", "COM_QUIT", comSetOption:
		return models.MetadataLifetimeSession
	case "COM_QUERY":
		if len(requests) == 1 {
			if q, ok := requests[0].Message.(*mysql.QueryPacket); ok && sessionStateQuery(q.Query) {
				return models.MetadataLifetimeSession
			}
		}
		return models.MetadataLifetimePerTest
	}
	if models.IsMySQLSessionReusableCommandType(requestOperation) {
		return models.MetadataLifetimeSession
	}
	return models.MetadataLifetimePerTest
}

// comSetOption is the header type the decoder gives COM_SET_OPTION (0x1b),
// which it does not name.
const comSetOption = "0x1b"

// lifetimeOf is the runtime lifetime a stated lifetime derives to, as
// models.Mock.DeriveLifetime derives it on load: a connection lifetime with no
// connID to scope it to is session.
func lifetimeOf(stated, connID string) models.Lifetime {
	switch stated {
	case models.MetadataLifetimeSession:
		return models.LifetimeSession
	case models.MetadataLifetimeConnection:
		if connID == "" {
			return models.LifetimeSession
		}
		return models.LifetimeConnection
	}
	return models.LifetimePerTest
}

var (
	stateParserOnce sync.Once
	stateParser     *sqlparser.Parser
)

// sessionStateQuery reports whether query sets or reads the state of the
// connection rather than the application's data, so every test may be
// answered by its recording: SET, USE, transaction control (BEGIN, START
// TRANSACTION, COMMIT, ROLLBACK, SAVEPOINT, RELEASE SAVEPOINT), the SHOW
// statements drivers and ORMs set a connection up with (sessionStateShow),
// and a SELECT of nothing but system variables, literals and the
// connection's identity (sessionStateSelect). A statement that does not
// parse, that holds more than one statement, or that is longer than any of
// these is not: when in doubt a mock is per-test, which can fail a test that
// should pass, never pass one that should fail.
//
// It runs on the record path for every COM_QUERY, so a statement that cannot
// be one of these by its first keyword, or by its length, is answered
// without parsing it (sessionStateCandidate).
func sessionStateQuery(query string) bool {
	if !sessionStateCandidate(query) {
		return false
	}
	stateParserOnce.Do(func() {
		p, err := sqlparser.New(sqlparser.Options{})
		if err == nil {
			stateParser = p
		}
	})
	if stateParser == nil {
		return false
	}
	stmt, err := stateParser.Parse(query)
	if err != nil {
		return false
	}
	switch s := stmt.(type) {
	case *sqlparser.Set, *sqlparser.Use,
		*sqlparser.Begin, *sqlparser.Commit, *sqlparser.Rollback,
		*sqlparser.SRollback, *sqlparser.Savepoint, *sqlparser.Release:
		return true
	case *sqlparser.Show:
		return sessionStateShow(s)
	case *sqlparser.Select:
		return sessionStateSelect(s)
	}
	return false
}

// maxSessionStateQuery bounds the statements sessionStateQuery parses. The
// longest it classifies session, a driver's connection-setup SELECT of system
// variables (mysql-connector-j's is under 1 KiB), is well within it.
const maxSessionStateQuery = 4096

// sessionStateCandidate reports whether query may be a statement
// sessionStateQuery classifies session, by its length and its first keyword
// after any leading comments; a SELECT also by having no FROM but DUAL. A
// false is final; a true is decided by the parse.
func sessionStateCandidate(query string) bool {
	if len(query) > maxSessionStateQuery {
		return false
	}
	rest := skipSQLComments(query)
	end := 0
	for end < len(rest) && (rest[end] >= 'a' && rest[end] <= 'z' || rest[end] >= 'A' && rest[end] <= 'Z') {
		end++
	}
	switch strings.ToUpper(rest[:end]) {
	case "SET", "USE", "SHOW", "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE":
		return true
	case "SELECT":
		upper := strings.ToUpper(rest)
		return !strings.Contains(upper, "FROM") || strings.Contains(upper, "DUAL")
	}
	return false
}

// skipSQLComments returns query from its first token that is not white space
// or a comment (/* ... */, -- ..., # ...). A MySQL executable comment
// (/*!NNNNN ... */) is statement content, which MySQL runs: its text after
// the version is the query's.
func skipSQLComments(query string) string {
	for {
		query = strings.TrimLeft(query, " \t\r\n")
		switch {
		case strings.HasPrefix(query, "/*!"):
			body := strings.TrimLeft(query[3:], "0123456789")
			if end := strings.Index(body, "*/"); end >= 0 {
				body = body[:end] + " " + body[end+2:]
			}
			return strings.TrimLeft(body, " \t\r\n")
		case strings.HasPrefix(query, "/*"):
			end := strings.Index(query[2:], "*/")
			if end < 0 {
				return ""
			}
			query = query[2+end+2:]
		case strings.HasPrefix(query, "-- "), strings.HasPrefix(query, "#"):
			end := strings.IndexByte(query, '\n')
			if end < 0 {
				return ""
			}
			query = query[end+1:]
		default:
			return query
		}
	}
}

// sessionStateShow reports whether s is a SHOW drivers and ORMs set a
// connection up with: SHOW [SESSION | GLOBAL] VARIABLES, SHOW COLLATION and
// SHOW CHARACTER SET. Every other SHOW (WARNINGS, STATUS, TABLES, TABLE
// STATUS, CREATE TABLE, ...) reads what a test did, or the data, and is not.
func sessionStateShow(s *sqlparser.Show) bool {
	basic, ok := s.Internal.(*sqlparser.ShowBasic)
	if !ok {
		return false
	}
	switch basic.Command {
	case sqlparser.VariableSession, sqlparser.VariableGlobal, sqlparser.Collation, sqlparser.Charset:
		return true
	}
	return false
}

// valueVariables are the system variables whose value is what the
// connection or a statement just did (the last id, the warning count, the
// executed transactions), not how the connection is set up: a SELECT of one
// reads a test's data.
var valueVariables = map[string]bool{
	"identity":       true,
	"last_insert_id": true,
	"insert_id":      true,
	"timestamp":      true,
	"rand_seed1":     true,
	"rand_seed2":     true,
	"warning_count":  true,
	"error_count":    true,
	"gtid_executed":  true,
	"gtid_purged":    true,
	"gtid_owned":     true,
	"gtid_next":      true,
	"last_gtid":      true,
}

// sessionStateFuncs are the functions a SELECT may call, with no argument,
// and still read only the connection's identity.
var sessionStateFuncs = map[string]bool{
	"database":      true,
	"schema":        true,
	"version":       true,
	"user":          true,
	"current_user":  true,
	"connection_id": true,
}

// sessionStateSelect reports whether s reads nothing but system variables
// (@@...), literals (a pool's SELECT 1) and the connection's identity
// (sessionStateFuncs), from no table (or DUAL). Anything else, a table, a
// user variable, NOW(), UUID(), LAST_INSERT_ID() or an expression, may carry
// one test's data, and is not.
func sessionStateSelect(s *sqlparser.Select) bool {
	if s.With != nil || s.Into != nil || s.Where != nil || s.GroupBy != nil || s.Having != nil || s.SelectExprs == nil {
		return false
	}
	for _, from := range s.From {
		t, ok := from.(*sqlparser.AliasedTableExpr)
		if !ok {
			return false
		}
		name, ok := t.Expr.(sqlparser.TableName)
		if !ok || !name.Qualifier.IsEmpty() || !strings.EqualFold(name.Name.String(), "dual") {
			return false
		}
	}
	if len(s.SelectExprs.Exprs) == 0 {
		return false
	}
	for _, e := range s.SelectExprs.Exprs {
		ae, ok := e.(*sqlparser.AliasedExpr)
		if !ok || !sessionStateExpr(ae.Expr) {
			return false
		}
	}
	return true
}

func sessionStateExpr(e sqlparser.Expr) bool {
	switch x := e.(type) {
	case *sqlparser.Variable:
		return x.Scope != sqlparser.VariableScope && !valueVariables[x.Name.Lowered()]
	case *sqlparser.Literal, *sqlparser.NullVal, sqlparser.BoolVal:
		return true
	case *sqlparser.FuncExpr:
		return x.Qualifier.IsEmpty() && len(x.Exprs) == 0 && sessionStateFuncs[x.Name.Lowered()]
	}
	return false
}
