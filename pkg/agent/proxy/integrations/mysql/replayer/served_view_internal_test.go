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
// the recorded answer, a PREPARE's answer with the statement id F gave it,
// and no answer to a command the server does not answer. It is the
// recording itself only where both are the recording's; it is built from
// the recording, which it never changes; two serves that gave the same view
// share its key, and another command or another id names another. An
// answer F wrote that the recording does not hold, or a recording that is
// not one command, is a view keploy cannot read.
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
	if _, _, asRecorded := servedView(rec, &live, &renumbered); asRecorded {
		t.Fatal("a PREPARE answered with another statement id is read as recorded")
	}
	load, key, _ := servedView(rec, &live, &renumbered)
	if load == nil || key == "" {
		t.Fatal("a PREPARE answered with another statement id states no view")
	}
	v, err := load()
	if err != nil || len(v.Spec.MySQLResponses) != 1 || v.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket).StatementID != 7 {
		t.Fatalf("view %+v %v; want the recorded answer with statement id 7", v, err)
	}
	if v.Spec.MySQLResponses[0].Payload != "raw" || v.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket).NumParams != 1 {
		t.Fatalf("the view lost the rest of the recorded answer: %+v", v.Spec.MySQLResponses[0])
	}
	if rec.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket).StatementID != 1 {
		t.Fatal("the view changed the recording")
	}
	if _, k, _ := servedView(rec, &live, &renumbered); k != key {
		t.Fatal("the same command and id name another view")
	}
	other := prepOK(8)
	if _, k, _ := servedView(rec, &live, &other); k == key {
		t.Fatal("two statement ids share a view's key")
	}

	// Another command, the recorded answer: the live command in the view.
	sent := prepare("SELECT  ?").PacketBundle
	load, key2, asRecorded := servedView(rec, &sent, &rec.Spec.MySQLResponses[0].PacketBundle)
	if asRecorded || load == nil || key2 == "" || key2 == key {
		t.Fatalf("another command: as recorded %v, key %q; want a view of its own", asRecorded, key2)
	}
	if v, err := load(); err != nil || v.Spec.MySQLRequests[0].Message.(*mysql.StmtPreparePacket).Query != "SELECT  ?" ||
		v.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket).StatementID != 1 {
		t.Fatalf("view %+v %v; want the command as sent with the recorded answer", v, err)
	}
	if rec.Spec.MySQLRequests[0].Message.(*mysql.StmtPreparePacket).Query != "SELECT ?" {
		t.Fatal("the view changed the recorded command")
	}
	if _, k, _ := servedView(rec, func() *mysql.PacketBundle { p := prepare("SELECT   ?").PacketBundle; return &p }(), &rec.Spec.MySQLResponses[0].PacketBundle); k == key2 {
		t.Fatal("two commands share a view's key")
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
	load, _, asRecorded = servedView(answeredRec, &sameClose, nil)
	if asRecorded || load == nil {
		t.Fatal("a recorded answer F did not write is read as given")
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
