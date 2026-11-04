package replayer

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

func viewPacket(typ string, msg interface{}) mysql.PacketBundle {
	return mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 1}, Type: typ}, Message: msg}
}

func viewRecording(reqs []mysql.Request, resps []mysql.Response) *models.Mock {
	m := &models.Mock{Name: "rec", Kind: models.MySQL}
	m.Spec.MySQLRequests, m.Spec.MySQLResponses = reqs, resps
	return m
}

// The view the command phase states of a serve is the recording as the
// application sent the command and F wrote the answer: the live command,
// the recorded answer, and no answer to a command the server does not
// answer. A PREPARE's answer with the statement id F gave it is the
// recorded answer: that id is F's own count of the connection's
// statements, not part of what F gave the application. The view is the
// recording itself only where the command is the recording's; it is built
// from the recording, which it never changes; its key names the answer F
// gave, never the command: two serves that gave the same answer share it,
// whatever command each answered and whatever statement id F numbered, and
// no answer where the recorded one was written names another. An answer F
// wrote that the recording does not hold, or a recording that is not one
// command, is a view keploy cannot read.
func TestServedView_IsTheCommandAsSentAndTheAnswerAsWritten(t *testing.T) {
	prepare := func(q string) mysql.Request {
		return mysql.Request{PacketBundle: viewPacket("COM_STMT_PREPARE", &mysql.StmtPreparePacket{Command: 0x16, Query: q})}
	}
	prepOK := func(id uint32) mysql.PacketBundle {
		return viewPacket(mysql.COM_STMT_PREPARE_OK, &mysql.StmtPrepareOkPacket{StatementID: id, NumParams: 1})
	}
	rec := viewRecording([]mysql.Request{prepare("SELECT ?")}, []mysql.Response{{PacketBundle: prepOK(1), Payload: "raw"}})
	live := prepare("SELECT ?").PacketBundle

	if _, _, asRecorded := servedView(rec, &live, &rec.Spec.MySQLResponses[0].PacketBundle); !asRecorded {
		t.Fatal("the recorded command answered with the recorded answer is not read as recorded")
	}
	renumbered := prepOK(7)
	if load, key, asRecorded := servedView(rec, &live, &renumbered); !asRecorded || load != nil || key != "" {
		t.Fatalf("the recorded PREPARE answered with F's statement id 7: as recorded %v, view %v, key %q; want as recorded: the id is F's count, not what it gave",
			asRecorded, load != nil, key)
	}

	// Another command, the recorded answer: the live command in the view.
	sent := prepare("SELECT  ?").PacketBundle
	load, key2, asRecorded := servedView(rec, &sent, &rec.Spec.MySQLResponses[0].PacketBundle)
	if asRecorded || load == nil || key2 == "" {
		t.Fatalf("another command: as recorded %v, key %q; want a view of its own", asRecorded, key2)
	}
	if v, err := load(); err != nil || v.Spec.MySQLRequests[0].Message.(*mysql.StmtPreparePacket).Query != "SELECT  ?" ||
		v.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket).StatementID != 1 {
		t.Fatalf("view %+v %v; want the command as sent with the recorded answer", v, err)
	}
	if rec.Spec.MySQLRequests[0].Message.(*mysql.StmtPreparePacket).Query != "SELECT ?" {
		t.Fatal("the view changed the recorded command")
	}
	if _, k, _ := servedView(rec, func() *mysql.PacketBundle { p := prepare("SELECT   ?").PacketBundle; return &p }(), &rec.Spec.MySQLResponses[0].PacketBundle); k != key2 {
		t.Fatal("two commands answered with the same recorded answer are named different views")
	}
	// Answered with the statement ids F numbered: the recorded answer, one
	// view whatever the id.
	for _, id := range []uint32{7, 8} {
		written := prepOK(id)
		load, k, asRecorded := servedView(rec, &sent, &written)
		if asRecorded || load == nil || k != key2 {
			t.Fatalf("another command answered with statement id %d: as recorded %v, view %v, its key the recorded answer's %v; want the recorded answer's view",
				id, asRecorded, load != nil, k == key2)
		}
		v, err := load()
		if err != nil || len(v.Spec.MySQLResponses) != 1 || v.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket).StatementID != 1 {
			t.Fatalf("view %+v %v; want the recorded answer, statement id 1", v, err)
		}
		if v.Spec.MySQLResponses[0].Payload != "raw" || v.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket).NumParams != 1 {
			t.Fatalf("the view lost the rest of the recorded answer: %+v", v.Spec.MySQLResponses[0])
		}
	}
	if rec.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket).StatementID != 1 {
		t.Fatal("the view changed the recording")
	}
	// The command's header is the application's too: another sequence id
	// is another command.
	resent := prepare("SELECT ?").PacketBundle
	resent.Header = &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 2}, Type: resent.Header.Type}
	if _, k, asRecorded := servedView(rec, &resent, &rec.Spec.MySQLResponses[0].PacketBundle); asRecorded || k == "" {
		t.Fatal("a command sent with another header is read as recorded")
	}

	// A command the server does not answer: no answer in the view, and the
	// recording itself where it holds none either.
	closeRec := viewRecording([]mysql.Request{{PacketBundle: viewPacket("COM_STMT_CLOSE", &mysql.StmtClosePacket{StatementID: 1})}}, nil)
	sameClose := viewPacket("COM_STMT_CLOSE", &mysql.StmtClosePacket{StatementID: 1})
	if _, _, asRecorded := servedView(closeRec, &sameClose, nil); !asRecorded {
		t.Fatal("a close sent as recorded, answered with nothing as recorded, is not read as recorded")
	}
	answeredRec := viewRecording(closeRec.Spec.MySQLRequests, []mysql.Response{{PacketBundle: prepOK(1)}})
	load, noAnswer, asRecorded := servedView(answeredRec, &sameClose, nil)
	if asRecorded || load == nil {
		t.Fatal("a recorded answer F did not write is read as given")
	}
	otherClose := viewPacket("COM_STMT_CLOSE", &mysql.StmtClosePacket{StatementID: 2})
	if _, k, _ := servedView(answeredRec, &otherClose, &answeredRec.Spec.MySQLResponses[0].PacketBundle); k == "" || k == noAnswer {
		t.Fatalf("a serve that wrote the recorded answer and one that wrote none share key %q; want a view each", k)
	}
	if v, err := load(); err != nil || len(v.Spec.MySQLResponses) != 0 {
		t.Fatalf("view %+v %v; want no answer: F wrote none", v, err)
	}
	twoRec := viewRecording(closeRec.Spec.MySQLRequests, []mysql.Response{{PacketBundle: prepOK(1)}, {PacketBundle: prepOK(2)}})
	if load, _, asRecorded := servedView(twoRec, &sameClose, nil); asRecorded || load == nil {
		t.Fatal("recorded answers F did not write are read as given")
	}

	// Views keploy cannot read: fail closed.
	for name, tc := range map[string]struct {
		rec     *models.Mock
		written *mysql.PacketBundle
	}{
		"an answer the recording does not hold": {rec, func() *mysql.PacketBundle {
			p := viewPacket(mysql.COM_STMT_PREPARE_OK, &mysql.StmtPrepareOkPacket{StatementID: 7, NumParams: 2})
			return &p
		}()},
		"an answer to a recording that holds none": {closeRec, &renumbered},
		"a recording of two commands":              {viewRecording([]mysql.Request{prepare("SELECT ?"), prepare("SELECT ?")}, rec.Spec.MySQLResponses), &rec.Spec.MySQLResponses[0].PacketBundle},
		"a recording of two answers":               {viewRecording(rec.Spec.MySQLRequests, []mysql.Response{rec.Spec.MySQLResponses[0], rec.Spec.MySQLResponses[0]}), &rec.Spec.MySQLResponses[0].PacketBundle},
	} {
		if load, key, asRecorded := servedView(tc.rec, &live, tc.written); load != nil || key != "" || asRecorded {
			t.Errorf("%s: a view %v, key %q, as recorded %v; want one keploy cannot read", name, load != nil, key, asRecorded)
		}
	}
}
