package recorder

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A pooled connection whose decrypted stream the capture joined while a
// command was in flight (the agent restarted, or the TLS hook attached, after
// the command was sent) starts with that command's answer. The first command
// the capture has is the next one: its answer is what the server sent after
// it was captured. The answer before it answers a command the capture does not
// have: it is reported once, and every command is recorded with its own
// answer. Before, the first command was recorded with the answer in flight,
// and every later command with the answer before its own.
func TestRecordV2_PostTLS_AnAnswerCapturedBeforeTheFirstCommandIsNotItsAnswer(t *testing.T) {
	t.Parallel()
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	// The answer to the command in flight when the capture began: 3 rows.
	for _, p := range resultSet(true, [][]byte{col}, [][]byte{textRowPayload("x"), textRowPayload("y"), textRowPayload("z")}) {
		j.h.pushDest(p, time.Now())
	}
	j.exchange(cannedCOMQuery(t, 0, "SELECT n FROM t"), resultSet(true, [][]byte{col}, [][]byte{textRowPayload("a"), textRowPayload("b")}))
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v", err)
	}
	if len(mocks) != 2 {
		t.Fatalf("%d mocks, want the 2 commands the capture has", len(mocks))
	}
	if got := describe(mocks[0]); got != "a result of 2 rows" {
		t.Fatalf("SELECT n recorded %s, want its own 2 rows", got)
	}
	if got := describe(mocks[1]); got != "a OK" {
		t.Fatalf("SELECT 1 recorded %s, want its OK", got)
	}
	if n := leftOut.count(); n != 1 {
		t.Fatalf("%d spans left out, want the answer in flight, once", n)
	}
}

// The answer in flight when the capture joined a pooled connection is reported
// once however the recorder leaves it. Here the first command the capture has
// is one whose response the recorder cannot frame, found before it reads a
// byte of the server's stream (COM_FIELD_LIST): it leaves that exchange out,
// and skips the server's stream by capture time to the next command's answer
// (fakeconn.FakeConn.SkipThrough), which takes the answer in flight with it,
// unread. The skip counts what of it was captured before the first command
// in the floor's run there (the stream's start), and ends that run at the
// first byte of the response left out, captured after that command, so the
// answer in flight is reported as it is when the recorder reads up to it:
// once, with its own cause, over a span of its own bytes alone. The exchange
// left out is reported too, by the recorder, and SELECT 1 is recorded with its
// OK.
// Before, the skip took the answer in flight with nothing to say so, and the
// test case it belonged to was saved without its mock. With no answer in
// flight, the skip takes the response left out alone, captured after the
// first command: nothing more is reported. With a first command it can frame,
// the answer in flight is reported as before.
//
// Not parallel: the WARNs are limited process-wide, and this test counts them.
func TestRecordV2_PostTLS_AnAnswerInFlightIsReportedWhenTheFirstResponseCannotBeFramed(t *testing.T) {
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	for _, c := range []struct {
		name string
		// inFlight starts the server's stream with an answer to a command
		// the capture does not have; unframable makes the first command
		// COM_FIELD_LIST, else a query.
		inFlight, unframable bool
		// mocks are the exchanges recorded, and leftOut the ones left out:
		// COM_FIELD_LIST's, and the answer in flight.
		mocks, leftOut int
	}{
		{"an answer in flight, then a command whose response it cannot frame", true, true, 1, 2},
		{"no answer in flight, then a command whose response it cannot frame", false, true, 1, 1},
		{"an answer in flight, then a query", true, false, 2, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetWarnLimiters()
			j := newJoinedMidStream(t)
			core, logs := observer.New(zapcore.DebugLevel)
			j.h.logger = zap.New(core)
			j.h.sess.Logger = j.h.logger
			leftOut := &orphanSpans{}
			j.h.sess.Orphans = leftOut
			mgr := syncMock.New(nil)
			j.h.sess.Mgr = mgr
			// from and to are the capture times of the answer in flight's
			// first and last bytes.
			var from, to time.Time
			if c.inFlight {
				for i, p := range resultSet(true, [][]byte{col}, [][]byte{textRowPayload("x"), textRowPayload("y")}) {
					to = j.at()
					if i == 0 {
						from = to
					}
					j.h.pushDest(p, to)
				}
			}
			if c.unframable {
				j.h.pushClient(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), j.at())
				for _, p := range frame([][]byte{col, okEOFPayload}) {
					j.h.pushDest(p, j.at())
				}
			} else {
				j.exchange(cannedCOMQuery(t, 0, "SELECT n FROM t"), resultSet(true, [][]byte{col}, [][]byte{textRowPayload("a")}))
			}
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
			mocks, err := j.run(t)
			if err != nil {
				t.Fatalf("RecordV2 = %v", err)
			}
			if len(mocks) != c.mocks || describe(mocks[len(mocks)-1]) != "a OK" {
				t.Fatalf("%d mocks, want %d, the last SELECT 1 with its OK", len(mocks), c.mocks)
			}
			if !c.unframable && describe(mocks[0]) != "a result of 1 rows" {
				t.Fatalf("SELECT n recorded %s, want its own row", describe(mocks[0]))
			}
			if spans, counted := leftOut.count(), mgr.MocksLeftOut(); spans != c.leftOut || counted != int64(c.leftOut) {
				t.Fatalf("%d spans left out, %d counted; want %d each", spans, counted, c.leftOut)
			}
			var unanswered []observer.LoggedEntry
			for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("was not recorded as a mock").All() {
				if r, _ := e.ContextMap()["reason"].(string); strings.HasPrefix(r, "server bytes that answer no captured request") {
					unanswered = append(unanswered, e)
				}
			}
			if want := map[bool]int{false: 0, true: 1}[c.inFlight]; len(unanswered) != want {
				t.Fatalf("%d WARN lines for server bytes that answer no captured request, want %d", len(unanswered), want)
			}
			if !c.inFlight || !c.unframable {
				return
			}
			// The span of the answer in flight is its own bytes', though the
			// skip took the response left out with it.
			found := false
			for i := 0; i < leftOut.count(); i++ {
				leftOut.mu.Lock()
				w := leftOut.windows[i]
				leftOut.mu.Unlock()
				found = found || (w[0].Equal(from) && w[1].Equal(to))
			}
			if !found {
				t.Fatalf("no span over the answer in flight's bytes alone, [%v %v]: %v", from, to, leftOut.windows)
			}
		})
	}
}

// A pool closes a connection with COM_QUIT, which has no answer, so the
// recorder never reads the server's stream after it. The answer in flight when
// the capture joined the connection is reported all the same, once, over its
// own bytes, as the recorder ends (Session.EndExchanges), as it is when the
// recorder reads past it. Before, nothing was reported, and the test case it
// belonged to was saved without its mock. So it is when the client closes
// after a COM_STMT_CLOSE, which has no answer either. A connection on which the
// client sends no command reports nothing: with no command, nothing orders the
// server's bytes against one, so they cannot be told to be an answer in flight.
//
// Not parallel: the WARNs are limited process-wide, and this test counts them.
func TestRecordV2_PostTLS_AnAnswerInFlightIsReportedWhenThePoolQuits(t *testing.T) {
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	for _, c := range []struct {
		name string
		// commands are what the client sends after the answer in flight,
		// none of which has an answer, before the connection closes.
		commands [][]byte
		// leftOut is 1 when the answer in flight is reported.
		leftOut int
	}{
		{"COM_QUIT", [][]byte{wrapPacket([]byte{mysql.COM_QUIT}, 0)}, 1},
		{"COM_STMT_CLOSE, then the client closes", [][]byte{wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0, 0}, 0)}, 1},
		{"no command", nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetWarnLimiters()
			j := newJoinedMidStream(t)
			core, logs := observer.New(zapcore.DebugLevel)
			j.h.logger = zap.New(core)
			j.h.sess.Logger = j.h.logger
			leftOut := &orphanSpans{}
			j.h.sess.Orphans = leftOut
			mgr := syncMock.New(nil)
			j.h.sess.Mgr = mgr
			// from and to are the capture times of the answer in flight's
			// first and last bytes.
			var from, to time.Time
			for i, p := range resultSet(true, [][]byte{col}, [][]byte{textRowPayload("x"), textRowPayload("y")}) {
				to = j.at()
				if i == 0 {
					from = to
				}
				j.h.pushDest(p, to)
			}
			for _, cmd := range c.commands {
				j.h.pushClient(cmd, j.at())
			}
			if _, err := j.run(t); err != nil {
				t.Fatalf("RecordV2 = %v", err)
			}
			if spans, counted := leftOut.count(), mgr.MocksLeftOut(); spans != c.leftOut || counted != int64(c.leftOut) {
				t.Fatalf("%d spans left out, %d counted; want %d each", spans, counted, c.leftOut)
			}
			unanswered := 0
			for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("was not recorded as a mock").All() {
				if r, _ := e.ContextMap()["reason"].(string); strings.HasPrefix(r, "server bytes that answer no captured request") {
					unanswered++
				}
			}
			if unanswered != c.leftOut {
				t.Fatalf("%d WARN lines for server bytes that answer no captured request, want %d", unanswered, c.leftOut)
			}
			if w, ok := leftOut.last(); c.leftOut == 1 && (!ok || !w[0].Equal(from) || !w[1].Equal(to)) {
				t.Fatalf("left out %v, want the answer in flight's bytes, [%v %v]", w, from, to)
			}
		})
	}
}

// The EOF a client that frames with EOFs is sent after a PREPARE's one run of
// definitions is captured before the client's next command, but the relay
// tees each direction on its own goroutine, so it can reach the recorder after
// that command. It is still the PREPARE's: while the PREPARE is held, the
// recorder starts each command with Session.NextRequest's answerUnread true
// (it passes held != nil), so the floor stays below the EOF until the PREPARE
// is settled; it records the PREPARE with its EOF, and leaves nothing out. A floor raised at the next command
// would drop it as an answer to no command, settle the framing as
// CLIENT_DEPRECATE_EOF, and record the PREPARE without it.
func TestRecordV2_PostTLS_APrepareEOFTeedAfterTheNextCommandIsStillThePrepares(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t WHERE a = ? AND b = ?"...), 0)
	reply := prepareOK(false, [][]byte{def, def}, nil) // PREPARE_OK, 2 definitions, EOF
	j.exchange(prepare, reply[:len(reply)-1])
	// The EOF is captured now, before the next command, and teed after it.
	eofSeq := fakeconn.ConnSeqOf(j.h.captured.Add(1))
	j.h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), time.Now())
	j.h.destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, ConnSeq: eofSeq, Bytes: reply[len(reply)-1], ReadAt: time.Now()}
	j.h.pushDest(cannedOK(t, 1, j.caps), time.Now())
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v", err)
	}
	if len(mocks) != 2 || leftOut.count() != 0 {
		t.Fatalf("%d mocks, %d left out; want the PREPARE and SELECT 1, nothing left out", len(mocks), leftOut.count())
	}
	sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
	if !ok || len(sp.ParamDefs) != 2 || !bytes.Equal(sp.EOFAfterParamDefs, reply[len(reply)-1]) {
		t.Fatalf("the PREPARE recorded %+v; want its 2 definitions and the EOF it was sent", mocks[0].Spec.MySQLResponses[0].Message)
	}
	if got := describe(mocks[1]); got != "a OK" {
		t.Fatalf("SELECT 1 recorded %s, want its OK", got)
	}
}

// A server drops an idle connection with an ERR of its own (MySQL 8's 4031 at
// wait_timeout, one at shutdown), and closes. The pool's ping on borrow is
// captured after it, as it is on the wire: the ERR answers no command. It was
// captured before the ping, so it is not the ping's answer, and after the
// command before it, so it is no command's: it is dropped with a Debug line,
// nothing is left out and nothing is said at WARN, and the connection's end
// ends the recording. Before, it was counted as a lost mock, with a WARN that
// blamed the capture's start and a span that left out the test case in
// flight.
func TestRecordV2_TheServersOwnErrorCapturedBeforeTheNextCommandAnswersNone(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zapcore.DebugLevel)
	h := newV2Harness(t)
	h.logger = zap.New(core)
	h.sess.Logger = h.logger
	leftOut := &orphanSpans{}
	h.sess.Orphans = leftOut
	greetingBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	h.pushDest(greetingBuf, at)
	h.pushClient(cannedHandshakeResponse41(t, 1, false), at)
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), at)
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), at)
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), at)
	h.pushDest(wrapPacket(append([]byte{0xff, 0xbf, 0x0f, '#'}, "HY000The client was disconnected by the server because of inactivity."...), 0), at)
	h.pushClient(wrapPacket([]byte{mysql.COM_PING}, 0), at)
	h.closeStreams()
	if err := RecordV2(context.Background(), h.logger, h.sess); err != nil {
		t.Fatalf("RecordV2 = %v: the server's own ERR and its close end the connection", err)
	}
	var mocks []*models.Mock
	for len(h.mocks) > 0 {
		if m := <-h.mocks; m.Spec.Metadata["type"] != "config" {
			mocks = append(mocks, m)
		}
	}
	if len(mocks) != 1 || describe(mocks[0]) != "a OK" {
		t.Fatalf("%d mocks; want SELECT 1 with its OK alone (the ping has no answer)", len(mocks))
	}
	if n := leftOut.count(); n != 0 {
		t.Fatalf("%d exchanges left out, want none", n)
	}
	if w := logs.FilterLevelExact(zapcore.WarnLevel); w.Len() != 0 {
		t.Fatalf("%d WARN lines, want none: %v", w.Len(), w.All()[0].Message)
	}
	if n := logs.FilterMessageSnippet("dropped server bytes captured before the request after them").Len(); n != 1 {
		t.Fatalf("%d Debug lines for the ERR, want 1", n)
	}
}

// While a PREPARE is held the floor stays below the server's bytes after its
// definitions (Session.NextRequest's answerUnread); once the next answer has
// settled it, the floor is set at every command again. Here the PREPARE sent
// no EOF, SELECT 1's OK settles it, and an OK captured before the command
// after it answers no command: that command is recorded with its own rows.
// A floor that stayed down once the PREPARE was settled would record it with
// that OK, the mispairing the floor is there to stop, on that connection
// alone and with nothing to say so.
func TestRecordV2_PostTLS_AfterAHeldPrepareIsSettledEveryCommandGetsItsOwnAnswer(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT n FROM t WHERE a = ?"...), 0), prepareOK(true, [][]byte{def}, nil))
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	j.h.pushDest(cannedOK(t, 1, j.caps), time.Now())
	j.exchange(cannedCOMQuery(t, 0, "SELECT n FROM t"), resultSet(true, [][]byte{col}, [][]byte{textRowPayload("a"), textRowPayload("b")}))
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v", err)
	}
	if len(mocks) != 3 || leftOut.count() != 0 {
		t.Fatalf("%d mocks, %d left out; want the PREPARE, SELECT 1 and SELECT n, nothing left out", len(mocks), leftOut.count())
	}
	if _, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket); !ok {
		t.Fatalf("the first mock is %s, want the PREPARE", describe(mocks[0]))
	}
	if got := describe(mocks[1]); got != "a OK" {
		t.Fatalf("SELECT 1 recorded %s, want its OK", got)
	}
	if got := describe(mocks[2]); got != "a result of 2 rows" {
		t.Fatalf("SELECT n recorded %s, want its own 2 rows", got)
	}
}

// A command whose chunk its producer did not number is refused
// (fakeconn.ErrUnnumbered): with no capture number, nothing says what the
// server had sent before it, and its answer cannot be told from those bytes.
// So is the command after a response the recorder could not frame, which it
// reads from where Session.NextRequest finds it, as it reads every command.
// Nothing is recorded for a command refused.
func TestRecordV2_ACommandWithNoCaptureNumberIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// afterFault: the command follows a response left out.
		afterFault bool
	}{
		{"a command", false},
		{"the command after a response left out", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newV2Harness(t)
			leftOut := &orphanSpans{}
			h.sess.Orphans = leftOut
			greetingBuf := cannedHandshakeV10(t)
			greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
			if err != nil {
				t.Fatal(err)
			}
			// A client that waits for each answer: every chunk is captured
			// after the one before it.
			base := time.Now().Add(-time.Minute)
			tick := 0
			at := func() time.Time {
				tick++
				return base.Add(time.Duration(tick) * time.Millisecond)
			}
			h.pushDest(greetingBuf, at())
			h.pushClient(cannedHandshakeResponse41(t, 1, false), at())
			h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), at())
			if c.afterFault {
				// COM_FIELD_LIST: a reply the recorder cannot frame. Its
				// exchange is left out, and the recording goes on from the
				// next command.
				def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "id", OrgName: "id", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
				h.pushClient(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), at())
				h.pushDest(bytes.Join(frame([][]byte{def, eofPayload}), nil), at())
			}
			sent := at()
			h.clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: cannedCOMQuery(t, 0, "SELECT 1"), ReadAt: sent, WrittenAt: sent}
			h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), at())
			h.closeStreams()
			if err := RecordV2(context.Background(), h.logger, h.sess); !errors.Is(err, fakeconn.ErrUnnumbered) {
				t.Fatalf("RecordV2 = %v, want fakeconn.ErrUnnumbered", err)
			}
			for len(h.mocks) > 0 {
				if m := <-h.mocks; m.Spec.Metadata["type"] != "config" {
					t.Fatalf("a %s mock was recorded for a command with no capture number", m.Spec.Metadata["requestOperation"])
				}
			}
			if want := map[bool]int{false: 0, true: 1}[c.afterFault]; leftOut.count() != want {
				t.Fatalf("%d exchanges left out, want %d: the response it could not frame, and nothing for the command refused", leftOut.count(), want)
			}
		})
	}
}
