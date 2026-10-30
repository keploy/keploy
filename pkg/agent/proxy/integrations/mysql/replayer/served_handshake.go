package replayer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"slices"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// servedHandshake is what one connection's handshake gave the application
// (stateHandshake): mock, the recorded handshake whose verdict F serves (nil
// until one is matched); sent, each packet the application sent, in the form
// a recording holds it, in order; and written, each packet F wrote to it, in
// order. The greeting F writes comes from the handshake recorded for the
// server the client dialled, before the client's answer picks mock, so it may
// be another recording's.
type servedHandshake struct {
	mock    *models.Mock
	sent    []mysql.PacketBundle
	written []mysql.PacketBundle
}

// gotPacket notes p, a packet the application sent, as decoded: the form a
// recording holds it in (the decoder keeps a HandshakeResponse41's auth
// response by its length only, as the recorder does).
func (s *servedHandshake) gotPacket(p *mysql.PacketBundle) {
	if s != nil && p != nil {
		s.sent = append(s.sent, *p)
	}
}

// gotAuthData notes pkt, a packet of authentication data the application
// sent, of type typ, in the form a recording holds it: its header and its
// length (mysqlUtils.ClientAuthData).
func (s *servedHandshake) gotAuthData(pkt mysql.Packet, typ string) {
	if s != nil {
		s.sent = append(s.sent, mysqlUtils.ClientAuthData(pkt, typ).PacketBundle)
	}
}

// wrote notes p, a recorded packet F wrote to the application.
func (s *servedHandshake) wrote(p *mysql.PacketBundle) {
	if s != nil && p != nil {
		s.written = append(s.written, *p)
	}
}

// stateHandshake says what F's serve of s.mock, the handshake whose verdict
// the connection phase just served and consumed (serveVerdict's updateMock),
// gave the application, as the learn step reads a recording F served
// (learn.Order.ServedView): as recorded (learn.Tracker.ServedMockAsRecorded)
// where the application sent the recorded packets and F wrote the recorded
// ones, packet for packet; otherwise the view the serve gave
// (learn.Tracker.ServedMockLive, s.view). A HandshakeResponse41 differs from
// its recording where a client names its own process in its connection
// attributes (a pid, at every run), and the greeting where F wrote another
// recording's. Without a statement a later call of the set reads the
// handshake as a serve no adapter stated, and refuses every changed value
// that may stand there: one connection opened anywhere earlier in a set
// refused every value learned after it. It is told after the consume, which
// numbered the serve, and only under the proven switch.
func stateHandshake(ctx context.Context, s *servedHandshake) {
	if s == nil || s.mock == nil {
		return
	}
	integrations.TellServed(ctx, s.mock, s.view)
}

// view is F's view of s.mock (stateHandshake): the recording with each
// packet the application sent as its requests and each packet F wrote as
// its answers, and the key that names it among the recording's views;
// asRecorded where they are the recording's own, packet for packet. A packet
// keploy cannot read gives a view keploy cannot read: nil, "" (fail closed).
func (s *servedHandshake) view() (func() (*models.Mock, error), string, bool) {
	m := s.mock
	sentKeys, ok := packetKeys(s.sent)
	if !ok {
		return nil, "", false
	}
	writtenKeys, ok := packetKeys(s.written)
	if !ok {
		return nil, "", false
	}
	reqs := make([]mysql.PacketBundle, len(m.Spec.MySQLRequests))
	for i := range m.Spec.MySQLRequests {
		reqs[i] = m.Spec.MySQLRequests[i].PacketBundle
	}
	resps := make([]mysql.PacketBundle, len(m.Spec.MySQLResponses))
	for i := range m.Spec.MySQLResponses {
		resps[i] = m.Spec.MySQLResponses[i].PacketBundle
	}
	reqKeys, ok := packetKeys(reqs)
	if !ok {
		return nil, "", false
	}
	respKeys, ok := packetKeys(resps)
	if !ok {
		return nil, "", false
	}
	if slices.Equal(sentKeys, reqKeys) && slices.Equal(writtenKeys, respKeys) {
		return nil, "", true
	}
	h := sha256.New()
	for _, keys := range [][]string{sentKeys, writtenKeys} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(keys)))
		h.Write(n[:])
		for _, k := range keys {
			binary.BigEndian.PutUint64(n[:], uint64(len(k)))
			h.Write(n[:])
			h.Write([]byte(k))
		}
	}
	sent, written := s.sent, s.written
	return func() (*models.Mock, error) {
		v := m.ShallowCopy()
		v.Spec.MySQLRequests = make([]mysql.Request, len(sent))
		for i := range sent {
			v.Spec.MySQLRequests[i] = mysql.Request{PacketBundle: sent[i]}
		}
		v.Spec.MySQLResponses = make([]mysql.Response, len(written))
		for i := range written {
			v.Spec.MySQLResponses[i] = mysql.Response{PacketBundle: written[i]}
		}
		return v, nil
	}, string(h.Sum(nil)), false
}

// packetKeys is each of ps as the learn step reads it (packetKey); false
// where one cannot be encoded.
func packetKeys(ps []mysql.PacketBundle) ([]string, bool) {
	keys := make([]string, len(ps))
	for i := range ps {
		k, ok := packetKey(&ps[i])
		if !ok {
			return nil, false
		}
		keys[i] = k
	}
	return keys, true
}
