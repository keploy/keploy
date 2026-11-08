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
// START TRANSACTION (or BEGIN) binds nothing: every transaction sends it, so
// the recording that answers it says nothing of which transaction is open.
// The command after it binds the transaction to the connection of the
// recording that answers it. COMMIT and ROLLBACK end the binding, and so do
// a new test window or test, and a reset of the connection. Outside a
// transaction nothing is bound: a pool hands one live connection to one
// request after another, which were recorded on other connections.
//
// A limit of today's behaviour, kept: a transaction begun without START
// TRANSACTION or BEGIN (autocommit turned off, XA) is not bound, and its
// commands are answered as every command outside a transaction is.

// txControl is what a live command does to the connection's transaction.
type txControl int

const (
	// txNone leaves the transaction as it is.
	txNone txControl = iota
	// txBegin begins a transaction (START TRANSACTION, BEGIN, COMMIT AND
	// CHAIN).
	txBegin
	// txEnd ends it (COMMIT, ROLLBACK, a reset of the connection).
	txEnd
)

var (
	sComResetConnection = mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION)
	sComChangeUser      = mysql.CommandStatusToString(mysql.COM_CHANGE_USER)
)

// txControlOf reads what req does to its connection's transaction.
func txControlOf(req mysql.Request) txControl {
	if req.Header != nil && (req.Header.Type == sComResetConnection || req.Header.Type == sComChangeUser) {
		return txEnd
	}
	qp, _ := req.Message.(*mysql.QueryPacket)
	if qp == nil {
		return txNone
	}
	id := sqlStatementIdentity(qp.Query) // comments aside, trimmed
	// The first word decides; a text that cannot begin with one of them is
	// not upper-cased and split.
	if !hasPrefixFold(id, "BEGIN") && !hasPrefixFold(id, "START") &&
		!hasPrefixFold(id, "COMMIT") && !hasPrefixFold(id, "ROLLBACK") {
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
	}
	return txNone
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
// bound: the transaction's next command as recorded.
func onBound(bound, mk *models.Mock) bool {
	return mk != bound && mk.Spec.Metadata["connID"] == bound.Spec.Metadata["connID"] &&
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
		d.InTx, d.Bound, d.BoundWindow = false, nil, time.Time{}
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
