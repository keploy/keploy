package replayer

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// stateServed says what F's serve of m, a command's recording the command
// phase just consumed (updateMock), gave the application, as the learn step
// reads a recording F served (learn.Order.ServedView): live is the command
// as the application sent it, and written the answer F writes for it (nil:
// a command the server does not answer, which F answers with nothing). The
// recording byte for byte (learn.Tracker.ServedMockAsRecorded) where the
// command is the recorded one and the answer the recorded one; otherwise the
// view the serve gave (learn.Tracker.ServedMockLive, servedView). Without a
// statement a later call of the set reads m as a serve no adapter stated,
// and refuses every changed value that may stand there. It is told after
// the consume, which numbered the serve, and only under the proven switch:
// with the switch off nothing is stated, and the serve is as it was.
func stateServed(ctx context.Context, m *models.Mock, live, written *mysql.PacketBundle) {
	if m == nil || live == nil {
		return
	}
	integrations.TellServed(ctx, m, func() (func() (*models.Mock, error), string, bool) {
		return servedView(m, live, written)
	})
}

// servedView is F's view of m, which F served to the command live with the
// answer written (stateServed): m as the application sent live and F
// answered it, built from the recording where a rule reads it, and the key
// that names it among m's views; asRecorded where that view is m's
// recording (live is its command, written its answer, packet for packet).
// The view keeps the live command and the recorded answer. An answer F
// wrote that is the recorded one with another statement id (a PREPARE's,
// which F numbers per connection: matchCommandWith) gave the recorded
// answer: F's own count of the connection's statements, which the
// application's driver reads back in its next commands, is not part of
// what F gave it, so a poller that prepares one recorded statement anew on
// every call (Go's database/sql with interpolateParams off prepares each
// query that has arguments) is one view, not one for each statement id.
// An answer that is neither the recorded one nor that one with another
// statement id, or a recording that is not one command, is a view keploy
// cannot read: nil, "" (fail closed). The key names the answer F gave,
// never the command it answered: none, or the recorded one. Serves of m
// that gave the same answer are one view, the first one's, so a command the
// application makes anew on every call (a timestamp, a nonce in its SQL)
// fills no more of the views a set keeps of m than one; each command, then
// each statement id, named a view of its own before, and a serve past the
// views a set keeps was read as one keploy could not read, which refused
// every changed value of a later call that may stand there.
func servedView(m *models.Mock, live, written *mysql.PacketBundle) (func() (*models.Mock, error), string, bool) {
	reqs, resps := m.Spec.MySQLRequests, m.Spec.MySQLResponses
	if len(reqs) != 1 {
		return nil, "", false
	}
	lk, ok := packetKey(live)
	if !ok {
		return nil, "", false
	}
	rk, ok := packetKey(&reqs[0].PacketBundle)
	if !ok {
		return nil, "", false
	}
	sameAnswer := false
	switch {
	case written == nil:
		sameAnswer = len(resps) == 0
	case len(resps) == 1:
		wk, ok := packetKey(written)
		if !ok {
			return nil, "", false
		}
		ak, ok := packetKey(&resps[0].PacketBundle)
		if !ok {
			return nil, "", false
		}
		// A PREPARE answered with the statement id F numbered gave the
		// recorded answer.
		if wk != ak && !renumberedPrepare(&resps[0].PacketBundle, written) {
			return nil, "", false
		}
		sameAnswer = true
	default:
		// F wrote an answer the recording does not hold as its one.
		return nil, "", false
	}
	if lk == rk && sameAnswer {
		return nil, "", true
	}
	h := sha256.New()
	if written == nil {
		h.Write([]byte{0})
	} else {
		h.Write([]byte{1})
	}
	header, message := live.Header, live.Message
	return func() (*models.Mock, error) {
		v := m.ShallowCopy()
		req := reqs[0]
		req.Header, req.Message = header, message
		v.Spec.MySQLRequests = []mysql.Request{req}
		v.Spec.MySQLResponses = nil
		if written != nil {
			v.Spec.MySQLResponses = []mysql.Response{resps[0]}
		}
		return v, nil
	}, string(h.Sum(nil)), false
}

// renumberedPrepare reports whether written is the recorded PREPARE answer
// rec with another statement id.
func renumberedPrepare(rec, written *mysql.PacketBundle) bool {
	r, ok := rec.Message.(*mysql.StmtPrepareOkPacket)
	if !ok || r == nil {
		return false
	}
	w, ok := written.Message.(*mysql.StmtPrepareOkPacket)
	if !ok || w == nil {
		return false
	}
	same := *r
	same.StatementID = w.StatementID
	sk, ok := packetKey(&mysql.PacketBundle{Header: rec.Header, Message: &same})
	if !ok {
		return false
	}
	wk, ok := packetKey(written)
	return ok && sk == wk
}

// packetKey is a packet as the learn step reads it (its header and message,
// as a recording's JSON holds them), so two packets that hold the same text
// share it; false where it cannot be encoded.
func packetKey(p *mysql.PacketBundle) (string, bool) {
	if p == nil {
		return "", false
	}
	b, err := json.Marshal(struct {
		Header  *mysql.PacketInfo `json:"header"`
		Message interface{}       `json:"message"`
	}{p.Header, p.Message})
	if err != nil {
		return "", false
	}
	return string(b), true
}
