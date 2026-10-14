package replayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// testDBAddr is the upstream the application dialled, as the proxy names it.
const testDBAddr = "10.0.0.5:3306"

// testServerCaps are the capabilities the recorded greeting advertises: no TLS,
// so the client answers the greeting with its HandshakeResponse41 at once.
const testServerCaps = uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION)

// bundle is one recorded packet: its sequence id, its type and its message.
func bundle(seq uint8, typ string, msg interface{}) mysql.PacketBundle {
	return mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{SequenceID: seq}, Type: typ}, Message: msg}
}

// clientHandshake is the HandshakeResponse41 a client sends for user, asking
// for plugin.
func clientHandshake(user string, plugin mysql.AuthPluginName) *mysql.HandshakeResponse41Packet {
	return &mysql.HandshakeResponse41Packet{
		CapabilityFlags: testServerCaps,
		MaxPacketSize:   1 << 24,
		CharacterSet:    0x21,
		Username:        user,
		AuthResponse:    bytes.Repeat([]byte{0xAB}, 20),
		AuthPluginName:  string(plugin),
	}
}

// handshakeMock is a recorded handshake of user "app" with plugin: the
// server's greeting, the client's HandshakeResponse41, then the server's
// answers in answers and the client's further packets in requests.
func handshakeMock(plugin mysql.AuthPluginName, answers []mysql.Response, requests ...mysql.Request) *models.Mock {
	greeting := &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a,
		ServerVersion:   "8.0.36",
		ConnectionID:    7,
		AuthPluginData:  bytes.Repeat([]byte{0x11}, 20),
		CapabilityFlags: testServerCaps,
		CharacterSet:    0x21,
		StatusFlags:     0x02,
		AuthPluginName:  string(plugin),
	}
	return &models.Mock{
		Kind: models.MySQL,
		Name: "mock-0",
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config", "connID": "0", "destAddr": testDBAddr},
			MySQLRequests: append([]mysql.Request{
				{PacketBundle: bundle(1, mysql.HandshakeResponse41, clientHandshake("app", plugin))},
			}, requests...),
			MySQLResponses: append([]mysql.Response{
				{PacketBundle: bundle(0, mysql.AuthStatusToString(mysql.HandshakeV10), greeting)},
			}, answers...),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
	}
}

// okAnswer is the server's OK, the last packet of a handshake that succeeded.
func okAnswer(seq uint8) mysql.Response {
	return mysql.Response{PacketBundle: bundle(seq, mysql.StatusToString(mysql.OK), &mysql.OKPacket{Header: mysql.OK, StatusFlags: 2})}
}

// authMoreData is the server's caching_sha2_password verdict: fast auth
// succeeded, or the client must authenticate in full.
func authMoreData(seq uint8, verdict mysql.CachingSha2Password) mysql.Response {
	return mysql.Response{PacketBundle: bundle(seq, mysql.AuthStatusToString(mysql.AuthMoreData),
		&mysql.AuthMoreDataPacket{StatusTag: mysql.AuthMoreData, Data: mysql.CachingSha2PasswordToString(verdict)})}
}

// refusal is the server's ERR for a login it refused.
func refusal(seq uint8) mysql.Response {
	return mysql.Response{PacketBundle: bundle(seq, mysql.StatusToString(mysql.ERR),
		&mysql.ERRPacket{Header: mysql.ERR, ErrorCode: 1045, SQLStateMarker: "#", SQLState: "28000", ErrorMessage: "Access denied"})}
}

// switchedHandshake is a recorded handshake in which the server switched the
// client to mysql_native_password and then gave verdict.
func switchedHandshake(verdict mysql.Response) *models.Mock {
	return handshakeMock(mysql.Native,
		[]mysql.Response{
			{PacketBundle: bundle(2, mysql.AuthStatusToString(mysql.AuthSwitchRequest),
				&mysql.AuthSwitchRequestPacket{StatusTag: mysql.AuthSwitchRequest, PluginName: string(mysql.Native), PluginData: "AQIDBA"})},
			verdict,
		},
		mysql.Request{PacketBundle: bundle(3, mysql.AuthSwithResponse, &mysql.AuthSwitchResponsePacket{Data: "x"})},
	)
}

// answerSwitch reads the recorded AuthSwitchRequest and answers it with an
// AuthSwitchResponse of sequence seq.
func (c mysqlClient) answerSwitch(seq uint8) {
	c.t.Helper()
	if req := c.read(); len(req) == 0 || req[0] != mysql.AuthSwitchRequest {
		c.t.Fatalf("the client read %x, want the recorded AuthSwitchRequest", req)
	}
	c.send(seq, []byte("x"))
}

// nativeHandshake is a recorded mysql_native_password handshake that succeeded.
func nativeHandshake() *models.Mock {
	return handshakeMock(mysql.Native, []mysql.Response{okAnswer(2)})
}

// mysqlClient is the application's end of a replayed connection.
type mysqlClient struct {
	t *testing.T
	c net.Conn
}

// read reads one packet from the replayer and returns its payload.
func (c mysqlClient) read() []byte {
	c.t.Helper()
	var hdr [4]byte
	if _, err := io.ReadFull(c.c, hdr[:]); err != nil {
		c.t.Fatalf("the client waited for a packet and got %v", err)
	}
	payload := make([]byte, int(hdr[0])|int(hdr[1])<<8|int(hdr[2])<<16)
	if _, err := io.ReadFull(c.c, payload); err != nil {
		c.t.Fatalf("the client read a packet header and then %v", err)
	}
	return payload
}

// send writes payload as one packet with sequence id seq. It returns once the
// replayer has read all of it (a net.Pipe write completes only then).
func (c mysqlClient) send(seq uint8, payload []byte) {
	c.t.Helper()
	pkt := make([]byte, 4, 4+len(payload))
	binary.LittleEndian.PutUint32(pkt, uint32(len(payload)))
	pkt[3] = seq
	if _, err := c.c.Write(append(pkt, payload...)); err != nil {
		c.t.Fatalf("the client's packet %d: %v", seq, err)
	}
}

// greetAndAnswer reads the server's greeting and answers it with the
// HandshakeResponse41 of user.
func (c mysqlClient) greetAndAnswer(user string, plugin mysql.AuthPluginName) {
	c.t.Helper()
	if g := c.read(); len(g) == 0 || g[0] != mysql.HandshakeV10 {
		c.t.Fatalf("the client was greeted with %x, want a HandshakeV10", g)
	}
	payload, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), clientHandshake(user, plugin))
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(1, payload)
}

// replay serves one client connection from db as the proxy does, and returns
// the client's end and what Replay returns. The client's end is closed once
// Replay returns, as the proxy closes it then.
func replay(t *testing.T, db integrations.MockMemDb) (mysqlClient, <-chan error) {
	t.Helper()
	client, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close() })
	ret := make(chan error, 1)
	go func() {
		ret <- Replay(context.Background(), zap.NewNop(), agentEnd, &models.ConditionalDstCfg{Addr: testDBAddr}, db, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	return mysqlClient{t: t, c: client}, ret
}

// reportCarrier is an error that carries a mismatch report, as
// models.NewMockMismatchError's does: the proxy reads a miss's report from the
// first one in its chain.
type reportCarrier interface {
	MismatchReport() *models.MockMismatchReport
}

// requireConnectionMiss fails the test unless err is a miss of the connection
// phase the proxy keeps as one: it wraps models.ErrNoMockMatched and carries a
// MySQL report naming the destination, the step summary, the phase and a diff
// that says diff.
func requireConnectionMiss(t *testing.T, err error, summary, phase, diff string) {
	t.Helper()
	if !errors.Is(err, models.ErrNoMockMatched) {
		t.Fatalf("Replay returned %v; want a miss (models.ErrNoMockMatched), which the proxy keeps — anything else it drops, and the test passes with no miss", err)
	}
	var r reportCarrier
	if !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("the miss %v carries no mismatch report", err)
	}
	rep := r.MismatchReport()
	if rep.Protocol != "MySQL" || rep.ActualSummary != summary || rep.MatchPhase != phase ||
		rep.Destination != testDBAddr || rep.At.IsZero() || !strings.Contains(rep.Diff, diff) || rep.NextSteps == "" {
		t.Fatalf("report %+v;\nwant protocol MySQL, summary %q, phase %q, destination %s, a time, next steps, and a diff saying %q",
			*rep, summary, phase, testDBAddr, diff)
	}
}

// The recorded handshake serves the client it was recorded for, so each miss
// below is down to what the case changes, not to a broken fixture.
func TestTheRecordedHandshakeIsServed(t *testing.T) {
	client, done := replay(t, &fakeMockDb{session: []*models.Mock{nativeHandshake()}})
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	_ = client.c.Close() // the application quits
	// Not nil: on a net.Pipe the command phase's SetReadDeadline fails once
	// the client has closed (a TCP socket takes it, and the read then ends on
	// EOF). What matters is that a connection served and closed is no miss.
	if err := <-done; errors.Is(err, models.ErrNoMockMatched) {
		t.Fatalf("Replay returned the miss %v for a connection it served and the client closed", err)
	}
}

// poolDb is a session pool whose handshakes change place when reused: a
// reused one is re-stamped to the back of the pool (UpdateUnFilteredMock), as
// the mock manager does once the first test window is staged (at boot it does
// not), and a deleted one is gone (DeleteUnFilteredMock). What a login is
// answered must not depend on either. Mocks are told apart by name.
type poolDb struct{ fakeMockDb }

func (p *poolDb) UpdateUnFilteredMock(old, updated *models.Mock) bool {
	for i, m := range p.session {
		if m.Name == old.Name {
			p.session = append(append(p.session[:i:i], p.session[i+1:]...), updated)
			return true
		}
	}
	return false
}

func (p *poolDb) DeleteUnFilteredMock(old models.Mock) bool {
	for i, m := range p.session {
		if m.Name == old.Name {
			p.session = append(p.session[:i:i], p.session[i+1:]...)
			return true
		}
	}
	return false
}

// login opens one connection to db as client says, and returns the first byte
// of the server's verdict on the login: an OK, or an ERR, after which the
// replayer must end the connection itself. It fails the test on a miss.
func login(t *testing.T, db integrations.MockMemDb, client func(c mysqlClient)) byte {
	t.Helper()
	c, done := replay(t, db)
	c.t = t
	client(c)
	got := c.read()
	if len(got) == 0 {
		t.Fatal("the client read an empty verdict")
	}
	if got[0] == mysql.ERR {
		// The replayer ends a refused connection itself: the client reads
		// the end of it without closing its own side first.
		if rest, err := io.ReadAll(c.c); len(rest) != 0 || err != nil {
			t.Fatalf("after the refusal the client read %q, %v; want the connection ended", rest, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("Replay returned %v for a refusal it served; want nil", err)
		}
		return got[0]
	}
	_ = c.c.Close()
	if err := <-done; errors.Is(err, models.ErrNoMockMatched) {
		t.Fatalf("Replay returned the miss %v for a connection it served", err)
	}
	return got[0]
}

// nativeLogin is a client logging in as "app" with mysql_native_password.
func nativeLogin(c mysqlClient) { c.greetAndAnswer("app", mysql.Native) }

// A session mock replay cannot serve does not shadow a recorded handshake it
// can, ahead of which it sorts. Each was chosen before and the client was
// never answered; now that an unanswerable connection is a miss, choosing one
// would report a miss although the recording holds a handshake for the client.
func TestAnUnservableMockDoesNotShadowAServableHandshake(t *testing.T) {
	greetingLater := nativeHandshake() // its first response is not the greeting
	greetingLater.Spec.MySQLResponses = append([]mysql.Response{okAnswer(0)}, greetingLater.Spec.MySQLResponses...)
	greetingOnly := nativeHandshake() // a connection that closed once greeted
	greetingOnly.Spec.MySQLRequests, greetingOnly.Spec.MySQLResponses = nil, greetingOnly.Spec.MySQLResponses[:1]
	nextFactor := handshakeMock(mysql.Native, []mysql.Response{{PacketBundle: bundle(2, mysql.AuthStatusToString(mysql.AuthNextFactor),
		&mysql.AuthNextFactorPacket{PacketType: mysql.AuthNextFactor, PluginName: "authentication_fido_client"})}})
	cutShort := handshakeMock(mysql.CachingSha2, []mysql.Response{authMoreData(2, mysql.FastAuthSuccess)})                  // no OK after it
	fullAuthCutShort := handshakeMock(mysql.CachingSha2, []mysql.Response{authMoreData(2, mysql.PerformFullAuthentication), // no public key, no OK
		{PacketBundle: bundle(4, mysql.AuthStatusToString(mysql.AuthMoreData), &mysql.AuthMoreDataPacket{StatusTag: mysql.AuthMoreData, Data: "PUBLIC KEY"})}})
	for name, first := range map[string]*models.Mock{
		"a greeting that is not the first response":         greetingLater,
		"a greeting and nothing more":                       greetingOnly,
		"a verdict replay does not serve":                   nextFactor,
		"an AuthMoreData with nothing recorded after it":    cutShort,
		"a full authentication cut short before its answer": fullAuthCutShort,
		"a greeting with no header":                         headerlessGreeting(),
	} {
		t.Run(name, func(t *testing.T) {
			first.Name = "mock-unservable"
			if got := login(t, &fakeMockDb{session: []*models.Mock{first, nativeHandshake()}}, nativeLogin); got != mysql.OK {
				t.Fatalf("the client read %#x; want the recorded OK", got)
			}
		})
	}
}

// A login the recording holds only a refusal for is refused, as the server
// refused it: the client reads the recorded ERR, and the replayer ends the
// connection, as the server ended it. That is no miss: the recording answers
// the client. Before, the refusal was not served at all, and the client
// waited, unanswered, for as long as it would.
//
// The replayer cannot tell the password a client sent, so it never refuses a
// login the recording holds a success for, from the server the client
// dialled: a login refused and then retried with success, as the recording
// holds it, is let in, on every connection. Another server's success does not
// let in a login this server refused. Nothing here depends on what earlier
// connections did, so each case opens several.
func TestARecordedRefusalIsServed(t *testing.T) {
	named := func(m *models.Mock, name string) *models.Mock { m.Name = name; return m }
	elsewhere := func(m *models.Mock) *models.Mock { // recorded for another server
		m.Spec.Metadata = map[string]string{"type": "config", "connID": "1", "destAddr": "10.0.0.6:3306"}
		return m
	}
	switchedLogin := func(c mysqlClient) {
		c.greetAndAnswer("app", mysql.Native)
		c.answerSwitch(3)
	}
	refused := func() *models.Mock {
		return named(handshakeMock(mysql.Native, []mysql.Response{refusal(2)}), "mock-refused")
	}
	for _, tc := range []struct {
		name    string
		session []*models.Mock
		client  func(c mysqlClient)
		want    byte
	}{{
		name:    "a refusal of the HandshakeResponse41",
		session: []*models.Mock{refused()},
		client:  nativeLogin,
		want:    mysql.ERR,
	}, {
		name:    "a refusal of the AuthSwitchResponse",
		session: []*models.Mock{named(switchedHandshake(refusal(4)), "mock-refused")},
		client:  switchedLogin,
		want:    mysql.ERR,
	}, {
		name:    "a refusal, then its retry's success",
		session: []*models.Mock{refused(), named(nativeHandshake(), "mock-ok")},
		client:  nativeLogin,
		want:    mysql.OK,
	}, {
		name:    "a refusal after an auth switch, then its retry's success",
		session: []*models.Mock{named(switchedHandshake(refusal(4)), "mock-refused"), named(switchedHandshake(okAnswer(4)), "mock-ok")},
		client:  switchedLogin,
		want:    mysql.OK,
	}, {
		name:    "a full authentication refused (a wrong password on MySQL 8), then its retry's success",
		session: []*models.Mock{named(fullAuthHandshake(refusal(6)), "mock-refused"), named(fullAuthHandshake(okAnswer(6)), "mock-ok")},
		client:  fullAuthLogin,
		want:    mysql.OK,
	}, {
		name:    "a refused full authentication",
		session: []*models.Mock{named(fullAuthHandshake(refusal(6)), "mock-refused")},
		client:  fullAuthLogin,
		want:    mysql.ERR,
	}, {
		name:    "a fast authentication refused (an unknown database), then its retry's success",
		session: []*models.Mock{named(fastAuthHandshake(refusal(3)), "mock-refused"), named(fastAuthHandshake(okAnswer(3)), "mock-ok")},
		client:  fastAuthLogin,
		want:    mysql.OK,
	}, {
		name:    "a refused fast authentication",
		session: []*models.Mock{named(fastAuthHandshake(refusal(3)), "mock-refused")},
		client:  fastAuthLogin,
		want:    mysql.ERR,
	}, {
		name:    "this server's refusal, and another server's success recorded first",
		session: []*models.Mock{elsewhere(named(nativeHandshake(), "mock-elsewhere")), refused()},
		client:  nativeLogin,
		want:    mysql.ERR,
	}, {
		name:    "another server's refusal, recorded first, and this server's success",
		session: []*models.Mock{elsewhere(refused()), named(nativeHandshake(), "mock-ok")},
		client:  nativeLogin,
		want:    mysql.OK,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			db := &poolDb{fakeMockDb{session: tc.session}}
			for i := 1; i <= 3; i++ {
				if got := login(t, db, tc.client); got != tc.want {
					t.Fatalf("connection %d read %#x; want the recorded %#x", i, got, tc.want)
				}
			}
		})
	}
}

// fastAuthHandshake is a recorded caching_sha2_password handshake whose
// fast authentication ended in verdict.
func fastAuthHandshake(verdict mysql.Response) *models.Mock {
	return handshakeMock(mysql.CachingSha2, []mysql.Response{authMoreData(2, mysql.FastAuthSuccess), verdict})
}

// fullAuthHandshake is a recorded caching_sha2_password handshake whose full
// authentication without TLS (the client asked for the server's public key
// and sent its password encrypted with it) ended in verdict.
func fullAuthHandshake(verdict mysql.Response) *models.Mock {
	return handshakeMock(mysql.CachingSha2,
		[]mysql.Response{authMoreData(2, mysql.PerformFullAuthentication),
			{PacketBundle: bundle(4, mysql.AuthStatusToString(mysql.AuthMoreData), &mysql.AuthMoreDataPacket{StatusTag: mysql.AuthMoreData, Data: "PUBLIC KEY"})},
			verdict},
		mysql.Request{PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 3}, Type: mysql.CachingSha2PasswordToString(mysql.RequestPublicKey)},
			Message: "request_public_key",
		}},
		mysql.Request{PacketBundle: bundle(5, mysql.EncryptedPassword, "encrypted")},
	)
}

// fastAuthLogin is a client logging in as "app" with caching_sha2_password,
// whose password the server has cached.
func fastAuthLogin(c mysqlClient) {
	c.t.Helper()
	c.greetAndAnswer("app", mysql.CachingSha2)
	if more := c.read(); len(more) == 0 || more[0] != mysql.AuthMoreData {
		c.t.Fatalf("the client read %x, want the recorded AuthMoreData", more)
	}
}

// fullAuthLogin is a client logging in as "app" with caching_sha2_password in
// full, without TLS: it asks for the server's public key and sends its
// password encrypted with it.
func fullAuthLogin(c mysqlClient) {
	c.t.Helper()
	fastAuthLogin(c)
	c.send(3, []byte{byte(mysql.RequestPublicKey)})
	if key := c.read(); len(key) == 0 || key[0] != mysql.AuthMoreData {
		c.t.Fatalf("the client read %x, want the recorded public key", key)
	}
	c.send(5, bytes.Repeat([]byte{0x5A}, 32)) // the password, encrypted with that key
}

// A caching_sha2_password full authentication leaves its handshake in the
// pool: a connection the recording holds no other handshake for authenticates
// in full again, as the server asks. Its handshake was deleted, and a second
// connection of an application that opened one while it was recorded was
// closed unanswered.
func TestAFullAuthenticationServesTheNextConnectionToo(t *testing.T) {
	db := &poolDb{fakeMockDb{session: []*models.Mock{fullAuthHandshake(okAnswer(6))}}}
	for i := 1; i <= 2; i++ {
		if got := login(t, db, fullAuthLogin); got != mysql.OK {
			t.Fatalf("connection %d read %#x; want the recorded OK", i, got)
		}
	}
}

// Which handshakes replay can serve, and which refuse the login: the final
// answer decides, after a caching_sha2_password exchange too, and a full
// authentication's final answer comes after the public key only without TLS.
func TestServableVerdict(t *testing.T) {
	tlsFullAuth := func(verdict mysql.Response) *models.Mock {
		return handshakeMock(mysql.CachingSha2, []mysql.Response{authMoreData(2, mysql.PerformFullAuthentication), verdict})
	}
	sha256Key := handshakeMock(mysql.CachingSha2, []mysql.Response{{PacketBundle: bundle(2, mysql.AuthStatusToString(mysql.AuthMoreData),
		&mysql.AuthMoreDataPacket{StatusTag: mysql.AuthMoreData, Data: "-----BEGIN PUBLIC KEY-----"})}, okAnswer(3)})
	for _, tc := range []struct {
		name              string
		m                 *models.Mock
		useSSL            bool
		servable, refuses bool
	}{
		{name: "native OK", m: nativeHandshake(), servable: true},
		{name: "native ERR", m: handshakeMock(mysql.Native, []mysql.Response{refusal(2)}), servable: true, refuses: true},
		{name: "ERR after an auth switch", m: switchedHandshake(refusal(4)), servable: true, refuses: true},
		{name: "fast auth, OK", m: fastAuthHandshake(okAnswer(3)), servable: true},
		{name: "fast auth, ERR", m: fastAuthHandshake(refusal(3)), servable: true, refuses: true},
		{name: "full auth, OK", m: fullAuthHandshake(okAnswer(6)), servable: true},
		{name: "full auth, ERR", m: fullAuthHandshake(refusal(6)), servable: true, refuses: true},
		{name: "full auth over TLS, OK", m: tlsFullAuth(okAnswer(4)), useSSL: true, servable: true},
		{name: "full auth over TLS, ERR", m: tlsFullAuth(refusal(4)), useSSL: true, servable: true, refuses: true},
		{name: "a TLS full auth, for a client without TLS", m: tlsFullAuth(okAnswer(4))},
		{name: "fast auth cut short", m: handshakeMock(mysql.CachingSha2, []mysql.Response{authMoreData(2, mysql.FastAuthSuccess)})},
		{name: "another mechanism's AuthMoreData", m: sha256Key},
		{name: "no verdict", m: handshakeMock(mysql.Native, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if servable, refuses := servableVerdict(tc.m, tc.useSSL); servable != tc.servable || refuses != tc.refuses {
				t.Fatalf("servableVerdict = %v, %v; want %v, %v", servable, refuses, tc.servable, tc.refuses)
			}
		})
	}
}

// A plain password, sent over TLS in a full authentication, is not compared
// with the recorded one, nor is its length, as no other authentication step
// compares a credential: an application replayed with another password than
// it was recorded with is served the recorded verdict. It was a miss. Its
// sequence is checked, as the encrypted password's is.
func TestAPlainPasswordIsNotCompared(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  uint8
		miss bool
	}{
		{name: "another password", seq: 5},
		{name: "a packet out of sequence", seq: 7, miss: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := handshakeMock(mysql.CachingSha2, nil)
			app, agentEnd := net.Pipe()
			t.Cleanup(func() { _ = app.Close(); _ = agentEnd.Close() })
			decodeCtx := newDecodeCtx()
			decodeCtx.LastOp = wire.NewLastOpMap()
			decodeCtx.ServerGreetings = wire.NewGreetings()
			decodeCtx.ServerGreetings.Store(agentEnd, hs.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet))
			type outcome struct {
				refused bool
				err     error
			}
			ret := make(chan outcome, 1)
			go func() {
				refused, err := simulatePlainPassword(context.Background(), zap.NewNop(), agentEnd, testDBAddr, reqResp{
					req:  []mysql.Request{{PacketBundle: bundle(5, "plain password", "cmVjb3JkZWQtc2VjcmV0AA==")}}, // "recorded-secret\x00"
					resp: []mysql.Response{okAnswer(6)},
				}, hs, &poolDb{fakeMockDb{session: []*models.Mock{hs}}}, decodeCtx)
				ret <- outcome{refused, err}
			}()
			c := mysqlClient{t: t, c: app}
			c.send(tc.seq, []byte("another-secret\x00"))
			verdict := make(chan []byte, 1)
			go func() {
				var hdr [4]byte
				if _, err := io.ReadFull(app, hdr[:]); err != nil {
					verdict <- nil
					return
				}
				payload := make([]byte, int(hdr[0])|int(hdr[1])<<8|int(hdr[2])<<16)
				if _, err := io.ReadFull(app, payload); err != nil {
					verdict <- nil
					return
				}
				verdict <- payload
			}()
			o := <-ret
			_ = agentEnd.Close() // ends the client's read when nothing was written
			got := <-verdict
			if tc.miss {
				requireConnectionMiss(t, o.err, "connection phase: plain password", models.MatchPhaseExhausted, "sequence")
				if len(got) != 0 {
					t.Fatalf("the client read %x; want nothing before its connection is closed", got)
				}
				return
			}
			if o.err != nil || o.refused {
				t.Fatalf("simulatePlainPassword returned refused=%v, %v; want the recorded OK served", o.refused, o.err)
			}
			if len(got) == 0 || got[0] != mysql.OK {
				t.Fatalf("the client read %x; want the recorded OK", got)
			}
		})
	}
}

// Every step of the connection phase at which the recording holds no answer
// for what the client sent is a miss the proxy keeps. Before, each of these
// returned nil (and the client waited, unanswered, for as long as it would)
// or an error that did not wrap models.ErrNoMockMatched (and the proxy logged
// it and dropped it): either way no miss reached the test, and a test whose
// application could not open its database passed.
//
// Each client sends its last packet and then closes its connection, so a
// replayer that does not see the miss ends on the client's EOF instead of
// waiting forever.
func TestEveryUnansweredHandshakeStepIsAMiss(t *testing.T) {
	noGreeting := nativeHandshake()
	noGreeting.Spec.MySQLResponses = []mysql.Response{okAnswer(0)} // a config mock, but no greeting in it

	answerless := handshakeMock(mysql.Native, nil) // the greeting, and no answer to the client's response

	authSwitch := switchedHandshake(okAnswer(4))

	headerless := nativeHandshake()
	headerless.Spec.MySQLResponses[1].Header = nil
	nextFactor := handshakeMock(mysql.Native, []mysql.Response{{PacketBundle: bundle(2, mysql.AuthStatusToString(mysql.AuthNextFactor),
		&mysql.AuthNextFactorPacket{PacketType: mysql.AuthNextFactor, PluginName: "authentication_fido_client"})}})

	fastAuthNoOK := handshakeMock(mysql.CachingSha2, []mysql.Response{authMoreData(2, mysql.FastAuthSuccess)})

	fullAuth := handshakeMock(mysql.CachingSha2,
		[]mysql.Response{authMoreData(2, mysql.PerformFullAuthentication),
			{PacketBundle: bundle(4, mysql.AuthStatusToString(mysql.AuthMoreData), &mysql.AuthMoreDataPacket{StatusTag: mysql.AuthMoreData, Data: "PUBLIC KEY"})},
			okAnswer(6)},
		mysql.Request{PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 3}, Type: mysql.CachingSha2PasswordToString(mysql.RequestPublicKey)},
			Message: "request_public_key",
		}},
	)

	for _, tc := range []struct {
		name    string
		session []*models.Mock
		client  func(c mysqlClient)
		summary string
		phase   string
		diff    string
	}{{
		name:    "a next authentication factor after a fast authentication",
		session: []*models.Mock{fastAuthHandshake(nextFactorAnswer(3))},
		client:  fastAuthLogin,
		summary: "connection phase: caching_sha2_password authentication",
		phase:   models.MatchPhaseExhausted,
		diff:    "AuthNextFactor, which replay does not serve",
	}, {
		name:    "a next authentication factor after a full authentication",
		session: []*models.Mock{fullAuthHandshake(nextFactorAnswer(6))},
		client: func(c mysqlClient) {
			fullAuthLogin(c)
		},
		summary: "connection phase: encrypted password",
		phase:   models.MatchPhaseExhausted,
		diff:    "AuthNextFactor, which replay does not serve",
	}, {
		name:    "a recorded public key with no header",
		session: []*models.Mock{headerlessPublicKey()},
		client: func(c mysqlClient) {
			fastAuthLogin(c)
			c.send(3, []byte{byte(mysql.RequestPublicKey)})
		},
		summary: "connection phase: public key request",
		phase:   models.MatchPhaseExhausted,
		diff:    "cannot be read",
	}, {
		name:    "a recorded public key request with no header",
		session: []*models.Mock{headerlessPublicKeyRequest()},
		client: func(c mysqlClient) {
			fastAuthLogin(c)
			c.send(3, []byte{byte(mysql.RequestPublicKey)})
		},
		summary: "connection phase: public key request",
		phase:   models.MatchPhaseExhausted,
		diff:    "header mismatch",
	}, {
		name:    "a recorded verdict with no header, after a fast authentication",
		session: []*models.Mock{headerlessFastAuthVerdict()},
		client:  fastAuthLogin,
		summary: "connection phase: caching_sha2_password authentication",
		phase:   models.MatchPhaseExhausted,
		diff:    "cannot be read",
	}, {
		name:    "no recorded greeting",
		session: []*models.Mock{noGreeting},
		client:  func(mysqlClient) {},
		summary: "connection phase: new connection",
		phase:   models.MatchPhaseNoMocks,
		diff:    "server greeting",
	}, {
		name:    "a HandshakeResponse41 no recorded handshake matches",
		session: []*models.Mock{nativeHandshake()},
		client:  func(c mysqlClient) { c.greetAndAnswer("intruder", mysql.Native) },
		summary: "connection phase: HandshakeResponse41",
		phase:   models.MatchPhaseExhausted,
		diff:    "username mismatch",
	}, {
		name:    "the recording ends before the answer to the HandshakeResponse41",
		session: []*models.Mock{answerless},
		client:  func(c mysqlClient) { c.greetAndAnswer("app", mysql.Native) },
		summary: "connection phase: HandshakeResponse41",
		phase:   models.MatchPhaseExhausted,
		diff:    "no answer",
	}, {
		name:    "a recorded answer that cannot be read",
		session: []*models.Mock{headerless},
		client:  func(c mysqlClient) { c.greetAndAnswer("app", mysql.Native) },
		summary: "connection phase: HandshakeResponse41",
		phase:   models.MatchPhaseExhausted,
		diff:    "cannot be read",
	}, {
		name:    "a recorded next authentication factor, which replay does not serve",
		session: []*models.Mock{nextFactor},
		client:  func(c mysqlClient) { c.greetAndAnswer("app", mysql.Native) },
		summary: "connection phase: HandshakeResponse41",
		phase:   models.MatchPhaseExhausted,
		diff:    "AuthNextFactor, which replay does not serve",
	}, {
		name:    "an AuthSwitchResponse out of sequence",
		session: []*models.Mock{authSwitch},
		client: func(c mysqlClient) {
			c.greetAndAnswer("app", mysql.Native)
			c.answerSwitch(5) // the recording's is sequence 3
		},
		summary: "connection phase: AuthSwitchResponse",
		phase:   models.MatchPhaseExhausted,
		diff:    "sequence",
	}, {
		name:    "fast auth with no recorded OK",
		session: []*models.Mock{fastAuthNoOK},
		client: func(c mysqlClient) {
			c.greetAndAnswer("app", mysql.CachingSha2)
			if more := c.read(); len(more) == 0 || more[0] != mysql.AuthMoreData {
				c.t.Fatalf("the client read %x, want the recorded AuthMoreData", more)
			}
		},
		summary: "connection phase: caching_sha2_password authentication",
		phase:   models.MatchPhaseExhausted,
		diff:    "after",
	}, {
		name:    "a public key request out of sequence",
		session: []*models.Mock{fullAuth},
		client: func(c mysqlClient) {
			c.greetAndAnswer("app", mysql.CachingSha2)
			if more := c.read(); len(more) == 0 || more[0] != mysql.AuthMoreData {
				c.t.Fatalf("the client read %x, want the recorded AuthMoreData", more)
			}
			c.send(5, []byte{byte(mysql.RequestPublicKey)}) // the recording's is sequence 3
		},
		summary: "connection phase: public key request",
		phase:   models.MatchPhaseExhausted,
		diff:    "header",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			client, done := replay(t, &fakeMockDb{session: tc.session})
			client.t = t
			tc.client(client)
			_ = client.c.Close()
			requireConnectionMiss(t, <-done, tc.summary, tc.phase, tc.diff)
		})
	}
}

// headerlessPublicKeyRequest is a recorded full authentication whose public
// key request has no header.
func headerlessPublicKeyRequest() *models.Mock {
	m := fullAuthHandshake(okAnswer(6))
	m.Spec.MySQLRequests[1].Header = nil
	return m
}

// headerlessFastAuthVerdict is a recorded fast authentication whose final
// answer has no header.
func headerlessFastAuthVerdict() *models.Mock {
	m := fastAuthHandshake(okAnswer(3))
	m.Spec.MySQLResponses[2].Header = nil
	return m
}

// nextFactorAnswer is the server asking for a next authentication factor, at
// seq: MySQL 8.0.27's multi-factor authentication, after the first factor.
func nextFactorAnswer(seq uint8) mysql.Response {
	return mysql.Response{PacketBundle: bundle(seq, mysql.AuthStatusToString(mysql.AuthNextFactor),
		&mysql.AuthNextFactorPacket{PacketType: mysql.AuthNextFactor, PluginName: "authentication_fido_client"})}
}

// headerlessPublicKey is a recorded full authentication whose public key has
// no header.
func headerlessPublicKey() *models.Mock {
	m := fullAuthHandshake(okAnswer(6))
	m.Spec.MySQLResponses[2].Header = nil
	return m
}

// headerlessGreeting is a recorded handshake whose greeting has no header.
func headerlessGreeting() *models.Mock {
	m := nativeHandshake()
	m.Spec.MySQLResponses[0].Header = nil
	return m
}
