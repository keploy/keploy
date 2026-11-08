package replayer

import (
	"encoding/binary"
	"strings"
	"time"

	mysqlutils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
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
// Whether the live connection is in a transaction is read from the answers
// it was replayed with, as the server said it: every OK and EOF packet it
// sends (a result set ends with one) carries the session's status,
// SERVER_STATUS_IN_TRANS (a transaction is open) and
// SERVER_STATUS_AUTOCOMMIT, and its greeting carries the status a session
// begins with. A command whose answer says a transaction is open after it
// is a command of that transaction, and binds it; one whose answer says no
// transaction is open ended one (a statement that commits implicitly,
// autocommit turned on) or ran outside one, and nothing is bound after it. With autocommit
// off the next statement begins a transaction. So every way a session's
// autocommit is turned off is read as the server read it: a SET in any
// spelling (and a refused SET, answered with an ERR, changes nothing), the
// server's default (the greeting's status), init_connect (the first
// answer's).
//
// Where the status cannot say it, the command is read:
//   - START TRANSACTION (or BEGIN, XA START, COMMIT AND CHAIN) binds
//     nothing, though its answer says a transaction is open: every
//     transaction sends it, so the recording that answers it says nothing of
//     which transaction is open. The command after it binds.
//   - COMMIT and ROLLBACK (and XA END, COMMIT and ROLLBACK) end the
//     transaction and its binding whatever answered them: keploy answers one
//     nothing recorded with an OK of its own, which says nothing of the
//     session (txEnd: under autocommit off the next statement begins the
//     next transaction).
//   - A reset of the connection (COM_RESET_CONNECTION, COM_CHANGE_USER) ends
//     them too, and brings back the server's default autocommit, which its
//     answer says, or, when that carries no status (an auth exchange, or no
//     recording), the greeting's.
//
// An answer that carries no status (an ERR, a PREPARE's, none) leaves the
// session's state as it was. A new test window or test ends the binding too
// (boundRecording). Outside a transaction nothing is bound: a pool hands one
// live connection to one request after another, which were recorded on
// other connections.

// txControl is what a live command does to the connection's transaction
// that its answer's status cannot say.
type txControl int

const (
	// txNone leaves the transaction to its answer's status.
	txNone txControl = iota
	// txBegin begins a transaction (START TRANSACTION, BEGIN, XA START,
	// COMMIT AND CHAIN).
	txBegin
	// txEnd ends it (COMMIT, ROLLBACK, XA END, XA COMMIT, XA ROLLBACK).
	txEnd
	// txReset resets the session (COM_RESET_CONNECTION, COM_CHANGE_USER).
	txReset
)

// The session status flags an OK or EOF packet carries.
const (
	// serverStatusInTrans (SERVER_STATUS_IN_TRANS): a transaction is open.
	serverStatusInTrans = 0x0001
	// serverStatusAutocommit (SERVER_STATUS_AUTOCOMMIT): the session's
	// autocommit is on.
	serverStatusAutocommit = 0x0002
)

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
				// PREPARE, COMMIT and ROLLBACK, which name it.
				return txEnd
			}
		}
		return txNone
	}
	return txNone
}

// answeredStatus is the session's status after the command a recording
// answered, as the answer it was replayed with (its first response, as the
// matcher writes it) says: the status flags of an OK or EOF packet, or of
// the packet that ends a result set. ok is false for an answer that carries
// none (an ERR, a PREPARE's answer, an auth exchange) and for no recording.
func answeredStatus(mk *models.Mock) (status uint16, ok bool) {
	if mk == nil || len(mk.Spec.MySQLResponses) == 0 {
		return 0, false
	}
	switch m := mk.Spec.MySQLResponses[0].Message.(type) {
	case *mysql.OKPacket:
		return m.StatusFlags, true
	case *mysql.EOFPacket:
		return m.StatusFlags, true
	case *mysql.TextResultSet:
		return terminatorStatus(m.FinalResponse)
	case *mysql.BinaryProtocolResultSet:
		return terminatorStatus(m.FinalResponse)
	}
	return 0, false
}

// terminatorStatus is the status of a result set's recorded terminator, the
// packet as the server sent it (its four-byte header first): an EOF (0xfe,
// warnings, status) or, under CLIENT_DEPRECATE_EOF, the OK that replaces it
// (0xfe, affected rows and last insert id, both a zero byte, status,
// warnings), which hold the status at the same place. ok is false for
// another packet (an EOF of the protocol before 4.1 has no status).
func terminatorStatus(t *mysql.GenericResponse) (status uint16, ok bool) {
	if t == nil {
		return 0, false
	}
	eof41 := mysqlutils.IsEOFPacket(t.Data) && len(t.Data) >= 9
	if !eof41 && !mysqlutils.IsOKReplacingEOF(t.Data) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(t.Data[7:9]), true
}

// startSession sets a live connection's transaction state as it connects,
// from the status of the server greeting it was answered with: a server
// whose sessions begin with autocommit off (its global autocommit is 0)
// begins a transaction with the first statement.
func startSession(d *wire.DecodeContext, greeting *mysql.HandshakeV10Packet) {
	d.AutocommitOffAtConnect = greeting.StatusFlags&serverStatusAutocommit == 0
	d.AutocommitOff, d.InTx = d.AutocommitOffAtConnect, d.AutocommitOffAtConnect
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
	status, known := answeredStatus(served.mock)
	if known {
		d.AutocommitOff = status&serverStatusAutocommit == 0
		d.InTx = status&serverStatusInTrans != 0 || d.AutocommitOff
	}
	switch txControlOf(req) {
	case txBegin:
		d.InTx, d.Bound, d.BoundWindow = true, nil, time.Time{}
		return
	case txEnd:
		d.InTx, d.Bound, d.BoundWindow = d.AutocommitOff, nil, time.Time{}
		return
	case txReset:
		if !known {
			d.AutocommitOff = d.AutocommitOffAtConnect
		}
		d.InTx, d.Bound, d.BoundWindow = d.AutocommitOff, nil, time.Time{}
		return
	}
	if known && status&serverStatusInTrans == 0 {
		// No transaction is open after it: it ended one (a statement that
		// commits implicitly, autocommit turned on) or ran outside one.
		d.Bound, d.BoundWindow = nil, time.Time{}
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
