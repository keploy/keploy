package replayer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strconv"

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
// The view keeps the live command and, of the answer, only where it is not
// the recording's: a PREPARE's statement id, which F numbers per connection
// (matchCommandWith). An answer that is neither the recorded one nor that one
// with another statement id, or a recording that is not one command, is a
// view keploy cannot read: nil, "" (fail closed).
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
	sameAnswer, stmtID, renumbered := false, uint32(0), false
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
		if sameAnswer = wk == ak; !sameAnswer {
			if stmtID, renumbered = renumberedPrepare(&resps[0].PacketBundle, written); !renumbered {
				return nil, "", false
			}
		}
	default:
		// F wrote an answer the recording does not hold as its one.
		return nil, "", false
	}
	if lk == rk && sameAnswer {
		return nil, "", true
	}
	h := sha256.New()
	h.Write([]byte(lk))
	switch {
	case written == nil:
		h.Write([]byte{0})
	case renumbered:
		h.Write([]byte{2})
		h.Write([]byte(strconv.FormatUint(uint64(stmtID), 10)))
	default:
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
			resp := resps[0]
			if renumbered {
				ok := *resp.Message.(*mysql.StmtPrepareOkPacket)
				ok.StatementID = stmtID
				resp.Message = &ok
			}
			v.Spec.MySQLResponses = []mysql.Response{resp}
		}
		return v, nil
	}, string(h.Sum(nil)), false
}

// renumberedPrepare reports whether written is the recorded PREPARE answer
// rec with another statement id, and that id.
func renumberedPrepare(rec, written *mysql.PacketBundle) (uint32, bool) {
	r, ok := rec.Message.(*mysql.StmtPrepareOkPacket)
	if !ok || r == nil {
		return 0, false
	}
	w, ok := written.Message.(*mysql.StmtPrepareOkPacket)
	if !ok || w == nil {
		return 0, false
	}
	same := *r
	same.StatementID = w.StatementID
	sk, ok := packetKey(&mysql.PacketBundle{Header: rec.Header, Message: &same})
	if !ok {
		return 0, false
	}
	wk, ok := packetKey(written)
	return w.StatementID, ok && sk == wk
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
