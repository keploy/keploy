package replayer

import (
	"bytes"
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// anotherConnectionOf is nativeHandshake as another connection of the same
// client to the same server records it: its own connection id and
// scramble in the greeting, its own auth response, connection attributes
// and payload lengths in what the client sent, its own raw payloads, its own
// connID and times.
func anotherConnectionOf() *models.Mock {
	c := nativeHandshake()
	c.Name = "mock-9"
	c.Spec.Metadata["connID"] = "9"
	c.Spec.ReqTimestampMock = time.Now()
	c.Spec.ResTimestampMock = c.Spec.ReqTimestampMock.Add(time.Millisecond)
	g := c.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet)
	g.ConnectionID, g.AuthPluginData = 99, bytes.Repeat([]byte{0x22}, 20)
	c.Spec.MySQLResponses[0].Payload = "AAAA"
	hr := c.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet)
	hr.AuthResponse = bytes.Repeat([]byte{0xCD}, 20)
	hr.ConnectionAttributes = map[string]string{"_pid": "4242"}
	c.Spec.MySQLRequests[0].Header.Header.PayloadLength = 177
	return c
}

// askCopy is ServedByCopy(m), with a panic in it failing the test as name's.
func askCopy(t *testing.T, name string, m *models.Mock) (key string, ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: ServedByCopy panicked: %v", name, r)
			key, ok = "", false
		}
	}()
	return ServedByCopy(m)
}

// sslHandshake is nativeHandshake opened with an SSLRequest of charset.
func sslHandshake(charset uint8) *models.Mock {
	m := nativeHandshake()
	m.Spec.MySQLRequests = append([]mysql.Request{{PacketBundle: bundle(1, mysql.SSLRequest,
		&mysql.SSLRequestPacket{CapabilityFlags: testServerCaps | mysql.CLIENT_SSL, MaxPacketSize: 1 << 24, CharacterSet: charset})}},
		m.Spec.MySQLRequests...)
	return m
}

// A MySQL handshake is served by copy: its key is the same for another
// connection of the same client to the same server, which the replay
// serves a client of the lost one's from, and differs where the replay
// compares or serves something else. COM_QUIT needs no mock; every other
// MySQL mock, and a mock of another kind, is needed in itself.
func TestServedByCopyKeysAHandshakeByWhatTheReplayComparesAndServes(t *testing.T) {
	h := nativeHandshake()
	key, ok := ServedByCopy(h)
	if !ok || key == "" {
		t.Fatalf("a handshake: ServedByCopy = %q, %v; want a key", key, ok)
	}
	lost := anotherConnectionOf()
	if got, ok := ServedByCopy(lost); !ok || got != key {
		t.Fatalf("another connection's handshake of the same client and server: key %q, %v; want %q", got, ok, key)
	}
	// The replay serves the lost connection's client from the copy alone.
	if err := matchHanshakeResponse41(context.Background(), zap.NewNop(), h.Spec.MySQLRequests[0].PacketBundle, lost.Spec.MySQLRequests[0].PacketBundle); err != nil {
		t.Fatalf("the copy does not match the lost connection's HandshakeResponse41: %v", err)
	}
	if v := login(t, &fakeMockDb{session: []*models.Mock{h}}, nativeLogin); v != mysql.OK {
		t.Fatalf("a client of the lost connection, served from the copy alone, read %x; want the recorded OK", v)
	}

	for name, change := range map[string]func(m *models.Mock){
		"another server": func(m *models.Mock) { m.Spec.Metadata["destAddr"] = "10.0.0.6:3306" },
		"another user": func(m *models.Mock) {
			m.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet).Username = "admin"
		},
		"another database": func(m *models.Mock) {
			m.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet).Database = "other"
		},
		"another client plugin": func(m *models.Mock) {
			m.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet).AuthPluginName = string(mysql.CachingSha2)
		},
		"another character set": func(m *models.Mock) {
			m.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet).CharacterSet = 0x2d
		},
		"another max packet size": func(m *models.Mock) {
			m.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet).MaxPacketSize = 1 << 20
		},
		"another compression level": func(m *models.Mock) {
			m.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet).ZstdCompressionLevel = 3
		},
		"another client capability": func(m *models.Mock) {
			m.Spec.MySQLRequests[0].Message.(*mysql.HandshakeResponse41Packet).CapabilityFlags |= mysql.CLIENT_SSL
		},
		"another server version": func(m *models.Mock) {
			m.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet).ServerVersion = "8.4.0"
		},
		"another server plugin": func(m *models.Mock) {
			m.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet).AuthPluginName = string(mysql.CachingSha2)
		},
		"a refusal":           func(m *models.Mock) { m.Spec.MySQLResponses[1] = refusal(2) },
		"an SSLRequest first": func(m *models.Mock) { m.Spec.MySQLRequests = sslHandshake(0x21).Spec.MySQLRequests },
		"an auth switch": func(m *models.Mock) {
			s := switchedHandshake(okAnswer(4))
			m.Spec.MySQLRequests, m.Spec.MySQLResponses = s.Spec.MySQLRequests, s.Spec.MySQLResponses
		},
		"another sequence": func(m *models.Mock) {
			m.Spec.MySQLResponses[1].Header.Header.SequenceID = 4
		},
	} {
		c := nativeHandshake()
		change(c)
		if got, ok := askCopy(t, name, c); !ok || got == key {
			t.Errorf("a handshake with %s: key %q, %v; want a key of its own", name, got, ok)
		}
	}
	if a, _ := askCopy(t, "an SSLRequest", sslHandshake(0x21)); a == "" {
		t.Error("a handshake opened with an SSLRequest has no key")
	} else if b, _ := askCopy(t, "another SSLRequest", sslHandshake(0x2d)); b == a {
		t.Error("two handshakes whose SSLRequests differ in their character set: one key")
	}
	bare := nativeHandshake()
	bare.Spec.MySQLResponses[1].Header.Header = nil
	if k, ok := askCopy(t, "an answer with no length or sequence", bare); !ok || k == key {
		t.Errorf("a handshake whose answer has no length or sequence: key %q, %v; want a key of its own", k, ok)
	}
	// What a client sends after its HandshakeResponse41 is its credential:
	// only its type is compared.
	a, b := switchedHandshake(okAnswer(4)), switchedHandshake(okAnswer(4))
	b.Spec.MySQLRequests[1].Message = &mysql.AuthSwitchResponsePacket{Data: "y"}
	b.Spec.MySQLResponses[1].Message.(*mysql.AuthSwitchRequestPacket).PluginData = "BQYHCA"
	if ka, _ := ServedByCopy(a); ka == "" {
		t.Fatal("an auth switch's handshake has no key")
	} else if kb, _ := ServedByCopy(b); kb != ka {
		t.Fatalf("two handshakes that differ only in the client's switched credential and the switch's scramble: keys %q, %q", ka, kb)
	}

	quit := &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{
		Metadata:      map[string]string{"type": "mocks"},
		MySQLRequests: []mysql.Request{{PacketBundle: bundle(0, mysql.CommandStatusToString(mysql.COM_QUIT), nil)}},
	}}
	if key, ok := askCopy(t, "a COM_QUIT", quit); !ok || key != "" {
		t.Fatalf("a COM_QUIT: ServedByCopy = %q, %v; want no mock needed", key, ok)
	}
	for name, m := range map[string]*models.Mock{
		"a COM_PING": {Kind: models.MySQL, Spec: models.MockSpec{
			Metadata:      map[string]string{"type": "mocks"},
			MySQLRequests: []mysql.Request{{PacketBundle: bundle(0, mysql.CommandStatusToString(mysql.COM_PING), nil)}},
		}},
		"a COM_QUIT with another command": {Kind: models.MySQL, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			MySQLRequests: []mysql.Request{
				{PacketBundle: bundle(0, mysql.CommandStatusToString(mysql.COM_QUIT), nil)},
				{PacketBundle: bundle(0, mysql.CommandStatusToString(mysql.COM_PING), nil)},
			},
		}},
		"a command with no header": {Kind: models.MySQL, Spec: models.MockSpec{
			Metadata:      map[string]string{"type": "mocks"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{}}},
		}},
		"another kind's handshake": func() *models.Mock { c := nativeHandshake(); c.Kind = models.Postgres; return c }(),
		"a handshake packet with no header": func() *models.Mock {
			c := nativeHandshake()
			c.Spec.MySQLResponses[1].Header = nil
			return c
		}(),
		"a client packet with no header": func() *models.Mock {
			c := nativeHandshake()
			c.Spec.MySQLRequests[0].Header = nil
			return c
		}(),
		"a greeting that cannot be read": func() *models.Mock {
			c := nativeHandshake()
			c.Spec.MySQLResponses[0].Message = (*mysql.HandshakeV10Packet)(nil)
			return c
		}(),
		"an answer that cannot be read": func() *models.Mock {
			c := nativeHandshake()
			c.Spec.MySQLRequests[0].Message = (*mysql.HandshakeResponse41Packet)(nil)
			return c
		}(),
		"an auth switch that cannot be read": func() *models.Mock {
			c := nativeHandshake()
			c.Spec.MySQLResponses[1].Message = (*mysql.AuthSwitchRequestPacket)(nil)
			return c
		}(),
		"a packet that cannot be written": func() *models.Mock {
			c := nativeHandshake()
			c.Spec.MySQLResponses[1].Message = func() {}
			return c
		}(),
	} {
		if key, ok := askCopy(t, name, m); ok {
			t.Errorf("%s: ServedByCopy = %q, true; want it needed in itself", name, key)
		}
	}
	if _, ok := askCopy(t, "no mock", nil); ok {
		t.Error("no mock is served by copy")
	}
}
