package replayer

import (
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// The connection rule. A transaction runs on one connection, when it is
// recorded and when it is replayed: database/sql, JDBC and every pool pin a
// transaction to the connection it began on. Each recording names the
// connection it was made on (its connID metadata). Under load two requests'
// transactions are open at once and often send the same text (one product
// read for two orders), recorded on two connections inside the overlapping
// windows of two tests. The times of the two recordings cannot tell which is
// whose; their connections can.
//
// So, inside a live transaction, a command is answered first with the exact
// match the open test owns (testsOwn) that was recorded on the connection of
// the recording that answered the transaction's latest command, after that
// recording (onBound). Only with none such does the first exact match the
// open test owns answer, as before. That holds for keploy test, where the
// test's window says what it owns, and under exact serving, where its
// mapping does, so both answer a recorded set alike.
//
// START TRANSACTION (or BEGIN, or XA START) binds nothing: every
// transaction sends it, so the recording that answers it says nothing of
// which transaction is open. The command after it binds the transaction to
// the connection of the recording that answers it. COMMIT and ROLLBACK (and
// XA END, COMMIT and ROLLBACK) end the binding, and so do a new test window
// or test, and a reset of the connection. Outside a transaction nothing is
// bound: a pool hands one live connection to one request after another,
// which were recorded on other connections.
//
// A session with autocommit off (SET autocommit = 0, as JDBC's
// setAutoCommit(false) sends it) is always in a transaction: its next
// statement begins one, and so does the statement after each COMMIT or
// ROLLBACK. So turning autocommit off begins a transaction as START does, a
// COMMIT or ROLLBACK under it begins the next one at once, and turning it on
// again ends the open one (the server commits it). What a SET made of the
// session's autocommit is read from the status of the recorded answer
// (SERVER_STATUS_AUTOCOMMIT), so every spelling of every value the server
// accepts (OFF, FALSE, DEFAULT, a variable) is read as the server read it,
// and a refused SET (an ERR answer) changes nothing. A reset of the
// connection brings back the server's default autocommit, which its recorded
// answer's status says too.

// txControl is what a live command does to the connection's transaction.
type txControl int

const (
	// txNone leaves the transaction as it is.
	txNone txControl = iota
	// txBegin begins a transaction (START TRANSACTION, BEGIN, COMMIT AND
	// CHAIN).
	txBegin
	// txEnd ends it (COMMIT, ROLLBACK, XA END, XA COMMIT, XA ROLLBACK).
	txEnd
	// txAutocommit sets the session's autocommit (SET [SESSION | LOCAL]
	// autocommit, SET @@[session. | local.]autocommit) to what the recorded
	// answer's status says.
	txAutocommit
	// txReset resets the session (COM_RESET_CONNECTION, COM_CHANGE_USER): it
	// ends the transaction, and the session's autocommit is the server's
	// default, which the recorded answer's status says.
	txReset
)

// serverStatusAutocommit is the SERVER_STATUS_AUTOCOMMIT bit of an OK
// packet's status flags: the session's autocommit is on.
const serverStatusAutocommit = 0x0002

var (
	sComResetConnection = mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION)
	sComChangeUser      = mysql.CommandStatusToString(mysql.COM_CHANGE_USER)
)

// txControlOf reads what req does to its connection's transaction.
func txControlOf(req mysql.Request) txControl {
	if req.Header != nil && (req.Header.Type == sComResetConnection || req.Header.Type == sComChangeUser) {
		return txReset
	}
	qp, _ := req.Message.(*mysql.QueryPacket)
	if qp == nil {
		return txNone
	}
	id := sqlStatementIdentity(qp.Query) // comments aside, trimmed
	// The first word decides; a text that cannot begin with one of them is
	// not upper-cased and split.
	if hasPrefixFold(id, "SET") {
		if setsSessionAutocommit(id[len("SET"):]) {
			return txAutocommit
		}
		return txNone
	}
	if !hasPrefixFold(id, "BEGIN") && !hasPrefixFold(id, "START") &&
		!hasPrefixFold(id, "COMMIT") && !hasPrefixFold(id, "ROLLBACK") &&
		!hasPrefixFold(id, "XA") {
		return txNone
	}
	words := strings.Fields(strings.ToUpper(strings.TrimRight(id, "; \t\r\n")))
	rest := words[1:]
	switch words[0] {
	case "BEGIN":
		if len(rest) > 0 && rest[0] == "NOT" {
			// MariaDB's BEGIN NOT ATOMIC ... END: a compound statement.
			return txNone
		}
		return txBegin
	case "START":
		if len(rest) > 0 && rest[0] == "TRANSACTION" {
			return txBegin
		}
		return txNone
	case "COMMIT", "ROLLBACK":
		if len(rest) > 0 && rest[0] == "WORK" {
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0] == "TO" {
			// ROLLBACK TO SAVEPOINT: the transaction stays open.
			return txNone
		}
		if len(rest) > 1 && rest[0] == "AND" && rest[1] == "CHAIN" {
			return txBegin
		}
		return txEnd
	case "XA":
		if len(rest) > 0 {
			switch rest[0] {
			case "START", "BEGIN":
				return txBegin
			case "END", "COMMIT", "ROLLBACK":
				// XA END closes the transaction to every command but XA
				// PREPARE, COMMIT and ROLLBACK.
				return txEnd
			}
		}
		return txNone
	}
	return txNone
}

// setsSessionAutocommit reports whether the assignments of a SET statement
// (its text after SET) set the session's autocommit: an assignment of
// autocommit (or `autocommit`) in session scope. That is a bare name after
// no modifier, or after SESSION or LOCAL (the latest modifier holds for the
// assignments after it, as the server reads them), or @@autocommit,
// @@session.autocommit or @@local.autocommit; not after GLOBAL, PERSIST or
// PERSIST_ONLY, nor @@global.autocommit and the like, which set the
// server's default for new sessions, nor @autocommit, a user variable (its
// name begins with @, which no identifier does).
func setsSessionAutocommit(assignments string) bool {
	if assignments == "" || !startsToken(assignments) && assignments[0] != '@' {
		// SETTINGS, SET_x: another word.
		return false
	}
	session, sets := true, false
	for _, a := range splitSQLList(assignments) {
		a = strings.TrimLeft(a, sqlWhitespace)
		word := a[:identEnd(a, 0)]
		switch strings.ToUpper(word) {
		case "GLOBAL", "PERSIST", "PERSIST_ONLY":
			session, a = false, strings.TrimLeft(a[len(word):], sqlWhitespace)
		case "SESSION", "LOCAL":
			session, a = true, strings.TrimLeft(a[len(word):], sqlWhitespace)
		}
		inSession := session
		if after, ok := strings.CutPrefix(a, "@@"); ok {
			// @@name is the session's, whatever modifier came before.
			// @@global.autocommit (or @@persist., @@persist_only.) is
			// read as the name global, which is not autocommit.
			a, inSession = after, true
			for _, scope := range []string{"SESSION.", "LOCAL."} {
				if hasPrefixFold(a, scope) {
					a = a[len(scope):]
				}
			}
		}
		name, rest := a[:identEnd(a, 0)], ""
		if strings.HasPrefix(a, "`") {
			if end := strings.IndexByte(a[1:], '`'); end >= 0 {
				name = a[1 : 1+end]
			}
			rest = a[min(len(a), len(name)+2):]
		} else {
			rest = a[len(name):]
		}
		rest = strings.TrimLeft(rest, sqlWhitespace)
		if inSession && strings.EqualFold(name, "autocommit") &&
			(strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":=")) {
			sets = true
		}
	}
	return sets
}

// startsToken reports whether s begins with what ends the word before it
// and begins another token: whitespace, or a backquoted name (SET`x`=0).
func startsToken(s string) bool {
	return strings.ContainsRune(sqlWhitespace, rune(s[0])) || s[0] == '`'
}

// identEnd is the end of the unquoted identifier that begins at s[i] (i when
// none does).
func identEnd(s string, i int) int {
	for i < len(s) && isIdentByte(s[i]) {
		i++
	}
	return i
}

// splitSQLList splits s at each comma outside quotes ('...', "...", `...`,
// a backslash escaping the next byte in the first two) and parentheses.
func splitSQLList(s string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote != '`' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// answeredAutocommit is the session's autocommit after the command a
// recording answered, as the status of the answer it was replayed with (its
// first response, as the matcher writes it) says: ok is false when that
// answer is no OK packet (none answered, or the server refused the command
// with an ERR).
func answeredAutocommit(mk *models.Mock) (on, ok bool) {
	if mk == nil || len(mk.Spec.MySQLResponses) == 0 {
		return false, false
	}
	okp, _ := mk.Spec.MySQLResponses[0].Message.(*mysql.OKPacket)
	if okp == nil {
		return false, false
	}
	return okp.StatusFlags&serverStatusAutocommit != 0, true
}

// servedRecording is the recording a command was answered with, whether it
// is the open test's own (testsOwn), and the start of the test window it
// was answered in.
type servedRecording struct {
	mock   *models.Mock
	own    bool
	window time.Time
}

// boundRecording is the recording that answered the latest command of the
// live connection's open transaction (only a transaction binds: bindAfter),
// while it binds the command matched now (in window, the start of the open
// test window, zero with none): it was answered in the same window, it is
// still the open test's own (owns), and it names its connection. Nil
// otherwise.
func boundRecording(d *wire.DecodeContext, window time.Time, owns func(*models.Mock) bool) *models.Mock {
	if d == nil || d.Bound == nil || !d.BoundWindow.Equal(window) ||
		d.Bound.Spec.Metadata["connID"] == "" || !owns(d.Bound) {
		return nil
	}
	return d.Bound
}

// onBound reports whether mk was recorded on bound's connection, at or after
// bound: the transaction's next command as recorded. Bound itself is never
// read again: the consume that served it took it out of the pool or filed a
// fresh copy over it (updateMock). A reusable one's copy is on bound's
// connection at bound's time, so it answers the transaction's repeat of its
// command when the walk reads no later recording on the connection first;
// the pool files that copy after every other recording (its sort order is
// stamped anew), so the walk reads a later one first.
func onBound(bound, mk *models.Mock) bool {
	return mk.Spec.Metadata["connID"] == bound.Spec.Metadata["connID"] &&
		!mk.Spec.ReqTimestampMock.Before(bound.Spec.ReqTimestampMock)
}

// bindAfter keeps the live connection's transaction and its binding up to
// date once req is decided: served is the recording it was answered with
// (its mock nil when none answered it).
func bindAfter(d *wire.DecodeContext, req mysql.Request, served servedRecording) {
	if d == nil {
		return
	}
	switch txControlOf(req) {
	case txBegin:
		d.InTx, d.Bound, d.BoundWindow = true, nil, time.Time{}
		return
	case txEnd:
		// Under autocommit off the next statement begins the next
		// transaction.
		d.InTx, d.Bound, d.BoundWindow = d.AutocommitOff, nil, time.Time{}
		return
	case txReset:
		on, ok := answeredAutocommit(served.mock)
		d.AutocommitOff = ok && !on
		d.InTx, d.Bound, d.BoundWindow = d.AutocommitOff, nil, time.Time{}
		return
	case txAutocommit:
		on, ok := answeredAutocommit(served.mock)
		switch {
		case !ok:
			// Not answered, or refused: the session's autocommit is as it
			// was.
		case on && d.AutocommitOff:
			// Turning autocommit on commits the open transaction.
			d.AutocommitOff, d.InTx, d.Bound, d.BoundWindow = false, false, nil, time.Time{}
		case !on:
			// Turning it off outside a transaction begins one with the
			// next statement (nothing is bound outside a transaction).
			// Inside one begun by START, the transaction stays open and
			// bound, and the next one begins at its end.
			d.AutocommitOff, d.InTx = true, true
		}
		// On while it is on changes nothing.
		return
	}
	if !d.InTx || !served.own {
		// Outside a transaction nothing binds. A command answered with no
		// recording (served is then zero: it owns nothing), or with one the
		// open test does not own, keeps the binding the transaction's latest
		// answered command made.
		return
	}
	d.Bound, d.BoundWindow = served.mock, served.window
}
