package replayer

import (
	"context"
	"net"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// The view a connection's handshake states is the recording as the
// application sent its packets and F wrote its answers: every packet the
// client sent, as a recording holds it, and every packet F wrote, the
// greeting included, in order. It is the recording itself only where both
// are the recording's, packet for packet: a recorded packet F never wrote,
// or one the application never sent, makes it a view. It is built from the
// recording, which it never changes. Two handshakes that gave the same view
// share its key; another packet sent, another written, or the same packets
// split otherwise between the two, name another. A packet keploy cannot
// encode is a view keploy cannot read.
func TestAHandshakesViewIsWhatTheApplicationSentAndFWrote(t *testing.T) {
	hr := func(pid string) mysql.PacketBundle {
		return viewPacket(mysql.HandshakeResponse41, &mysql.HandshakeResponse41Packet{Username: "app", ConnectionAttributes: map[string]string{"_pid": pid}})
	}
	greeting := func(id uint32) mysql.PacketBundle {
		return viewPacket(mysql.AuthStatusToString(mysql.HandshakeV10), &mysql.HandshakeV10Packet{ProtocolVersion: 10, ConnectionID: id})
	}
	ok := viewPacket(mysql.StatusToString(mysql.OK), &mysql.OKPacket{Header: mysql.OK})
	switchReq := viewPacket(mysql.AuthStatusToString(mysql.AuthSwitchRequest), &mysql.AuthSwitchRequestPacket{PluginName: string(mysql.Native)})
	rec := viewRecording(
		[]mysql.Request{{PacketBundle: hr("6801")}},
		[]mysql.Response{{PacketBundle: greeting(7)}, {PacketBundle: ok}},
	)
	served := func(sent []mysql.PacketBundle, written ...mysql.PacketBundle) *servedHandshake {
		return &servedHandshake{mock: rec, sent: sent, written: written}
	}

	if _, _, asRecorded := served([]mysql.PacketBundle{hr("6801")}, greeting(7), ok).view(); !asRecorded {
		t.Fatal("the recorded packets, sent and written as recorded, read as a view; want as recorded")
	}
	for name, s := range map[string]*servedHandshake{
		"a recorded answer F never wrote":           served([]mysql.PacketBundle{hr("6801")}, greeting(7)),
		"a packet the recording does not hold sent": served([]mysql.PacketBundle{hr("6801"), ok}, greeting(7), ok),
		"nothing sent": served(nil, greeting(7), ok),
		"a greeting from another recording written":  served([]mysql.PacketBundle{hr("6801")}, greeting(8), ok),
		"the client of another process":              served([]mysql.PacketBundle{hr("4242")}, greeting(7), ok),
		"an auth switch the recording does not hold": served([]mysql.PacketBundle{hr("6801")}, greeting(7), switchReq, ok),
	} {
		if _, _, asRecorded := s.view(); asRecorded {
			t.Errorf("%s read as recorded; want a view", name)
		}
	}

	live := served([]mysql.PacketBundle{hr("4242")}, greeting(8), ok)
	load, key, asRecorded := live.view()
	if asRecorded || load == nil || key == "" {
		t.Fatalf("view() = %v, %q, %v; want a view with its key", load != nil, key, asRecorded)
	}
	v, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != rec.Name || len(v.Spec.MySQLRequests) != 1 || !reflect.DeepEqual(v.Spec.MySQLRequests[0].PacketBundle, hr("4242")) ||
		len(v.Spec.MySQLResponses) != 2 || !reflect.DeepEqual(v.Spec.MySQLResponses[0].PacketBundle, greeting(8)) || !reflect.DeepEqual(v.Spec.MySQLResponses[1].PacketBundle, ok) {
		t.Fatalf("the view is %s with requests %+v and answers %+v; want %s with the live HandshakeResponse41 and the greeting and OK F wrote", v.Name, v.Spec.MySQLRequests, v.Spec.MySQLResponses, rec.Name)
	}
	if got := rec.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet).ConnectionAttributes["_pid"]; got != "6801" || len(rec.Spec.MySQLResponses) != 2 ||
		rec.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet).ConnectionID != 7 {
		t.Fatalf("building the view changed the recording: pid %s, %d answers", got, len(rec.Spec.MySQLResponses))
	}

	keyOf := func(s *servedHandshake) string {
		t.Helper()
		_, k, _ := s.view()
		return k
	}
	if again := keyOf(served([]mysql.PacketBundle{hr("4242")}, greeting(8), ok)); again != key {
		t.Error("two handshakes that gave the same view have different keys")
	}
	for name, s := range map[string]*servedHandshake{
		"another pid sent":               served([]mysql.PacketBundle{hr("4243")}, greeting(8), ok),
		"another greeting written":       served([]mysql.PacketBundle{hr("4242")}, greeting(9), ok),
		"the greeting sent, not written": served([]mysql.PacketBundle{hr("4242"), greeting(8)}, ok),
		"an auth switch written as well": served([]mysql.PacketBundle{hr("4242")}, greeting(8), switchReq, ok),
	} {
		if k := keyOf(s); k == key || k == "" {
			t.Errorf("%s: key %q; want another key than the view's", name, k)
		}
	}

	unencodable := viewPacket(mysql.HandshakeResponse41, make(chan int))
	for name, s := range map[string]*servedHandshake{
		"a packet sent":    served([]mysql.PacketBundle{unencodable}, greeting(7), ok),
		"a packet written": served([]mysql.PacketBundle{hr("6801")}, greeting(7), unencodable),
		"a recorded packet": {mock: viewRecording([]mysql.Request{{PacketBundle: unencodable}}, rec.Spec.MySQLResponses),
			sent: []mysql.PacketBundle{hr("6801")}, written: []mysql.PacketBundle{greeting(7), ok}},
		"a recorded answer": {mock: viewRecording(rec.Spec.MySQLRequests, []mysql.Response{{PacketBundle: unencodable}}),
			sent: []mysql.PacketBundle{hr("6801")}, written: []mysql.PacketBundle{greeting(7), ok}},
	} {
		if load, key, asRecorded := s.view(); load != nil || key != "" || asRecorded {
			t.Errorf("%s keploy cannot encode: view() = %v, %q, %v; want a view keploy cannot read (nil, \"\", false)", name, load != nil, key, asRecorded)
		}
	}
}

// A handshake verdict states its serve only where the store consumed the
// handshake (updateMock), which numbers the serve. Where the store turned the
// consume down, no serve was numbered: a statement would stand for another
// serve of the recording, one F may have given otherwise (here one no adapter
// stated), and a later call would read that serve as stated.
func TestAHandshakeTheStoreDidNotConsumeStatesNothing(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	for _, tc := range []struct {
		name     string
		consumed bool
	}{
		{"the store consumed it", true},
		{"the store turned the consume down", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := nativeHandshake()
			tracker := learn.NewTracker()
			tracker.Begin()
			tracker.ServedMock(hs) // an earlier serve of it, no adapter stated
			db := &poolDb{}        // holds no handshake: it turns every consume down
			if tc.consumed {
				db.session = []*models.Mock{hs}
			}
			app, agentEnd := net.Pipe()
			t.Cleanup(func() { _ = app.Close(); _ = agentEnd.Close() })
			decodeCtx := newDecodeCtx()
			decodeCtx.LastOp = wire.NewLastOpMap()
			decodeCtx.ServerGreetings = wire.NewGreetings()
			decodeCtx.ServerGreetings.Store(agentEnd, hs.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet))
			served := &servedHandshake{mock: hs,
				sent:    []mysql.PacketBundle{hs.Spec.MySQLRequests[0].PacketBundle},
				written: []mysql.PacketBundle{hs.Spec.MySQLResponses[0].PacketBundle}}
			ctx := learn.WithTracker(context.Background(), tracker)
			ret := make(chan error, 1)
			go func() {
				_, err := serveVerdict(ctx, zap.NewNop(), agentEnd, testDBAddr, stepHandshakeResponse, &hs.Spec.MySQLResponses[1].PacketBundle, served, db, decodeCtx)
				ret <- err
			}()
			if v := (mysqlClient{t: t, c: app}).read(); len(v) == 0 || v[0] != mysql.OK {
				t.Fatalf("the client read %x; want the recorded OK", v)
			}
			if err := <-ret; err != nil {
				t.Fatalf("serveVerdict returned %v; want the verdict served", err)
			}
			if integrations.ExactServing(db) {
				t.Fatal("the store serves exactly: its turned-down consume would be no refusal")
			}
			tracker.End()
			tracker.Begin()
			order := tracker.Placed().Order
			first, last := order.Of(hs)
			r := order.ServedView(hs, learn.RawOf(hs, true).ServedIn(first, last))
			if r.AsRecorded() != tc.consumed || r.Live() {
				t.Fatalf("%s read as recorded %v, with a view %v; want as recorded %v", hs.Name, r.AsRecorded(), r.Live(), tc.consumed)
			}
		})
	}
}
