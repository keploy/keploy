package replayer_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// handshakeServedOn opens one connection to replayer.HandshakeDstForTest
// through the MySQL replayer (Replay), from a store that holds recs and tells
// tracker of each consume as the agent's does: the application answers the
// greeting with hr41, then with what login sends (nil: nothing more), and
// reads the verdict. It returns served read with the view the connection's
// serve stated (learn.Order.ServedView), in the next test of the set, as a
// later call reads it.
func handshakeServedOn(t *testing.T, tracker *learn.Tracker, recs []*models.Mock, served *models.Mock, hr41 *mysql.HandshakeResponse41Packet, login func(app net.Conn)) learn.Raw {
	t.Helper()
	return handshakeServedOver(t, tracker, recs, served, hr41, login, false)
}

// handshakeServedOver is handshakeServedOn; overTLS, the application asks
// for TLS first, as a client with ssl-mode set does: it answers the greeting
// with an SSLRequest (sslRequestPayload), upgrades the connection with a TLS
// handshake with keploy's MITM, and sends hr41 and the rest over TLS.
func handshakeServedOver(t *testing.T, tracker *learn.Tracker, recs []*models.Mock, served *models.Mock, hr41 *mysql.HandshakeResponse41Packet, login func(app net.Conn), overTLS bool) learn.Raw {
	t.Helper()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetServedObserver(tracker.ServedMock)
	mm.SetServedInObserver(tracker.ServedMockIn)
	mm.SetMocksWithWindow(nil, recs, models.BaseTime, time.Now().Add(time.Hour))
	tracker.Begin()
	ctx := learn.WithTracker(context.Background(), tracker)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
	app, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = agentEnd.Close() })
	done := make(chan error, 1)
	go func() {
		done <- replayer.Replay(ctx, zap.NewNop(), agentEnd, &models.ConditionalDstCfg{Addr: replayer.HandshakeDstForTest}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	if g := readPacket(t, app); len(g) == 0 || g[0] != mysql.HandshakeV10 {
		t.Fatalf("the application was greeted with %x; want a HandshakeV10", g)
	}
	payload, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), hr41)
	if err != nil {
		t.Fatal(err)
	}
	conn, seq := app, uint8(1)
	if overTLS {
		sendPacket(t, app, 1, sslRequestPayload(hr41))
		c := tls.Client(app, &tls.Config{ServerName: "db.test", InsecureSkipVerify: true}) //nolint:gosec // keploy's MITM leaf is not under test
		if err := c.Handshake(); err != nil {
			t.Fatalf("the application's TLS handshake after its SSLRequest: %v", err)
		}
		conn, seq = c, 2
	}
	sendPacket(t, conn, seq, payload)
	if login != nil {
		login(conn)
	}
	if v := readPacket(t, conn); len(v) == 0 || v[0] != mysql.OK {
		t.Fatalf("the application read %x as its verdict; want the recorded OK", v)
	}
	_ = app.Close()
	if err := <-done; err != nil && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Replay returned %v for a connection it served and the application closed", err)
	}
	tracker.End()
	tracker.Begin()
	order := tracker.Placed().Order
	first, last := order.Of(served)
	return order.ServedView(served, learn.RawOf(served, true).ServedIn(first, last))
}

// sslRequestPayload is the SSLRequest a client that asks for TLS sends in
// place of hr41 (which it sends once TLS is up): hr41's capability flags with
// CLIENT_SSL, its max packet size and character set, and 23 bytes of filler.
func sslRequestPayload(hr41 *mysql.HandshakeResponse41Packet) []byte {
	p := make([]byte, 4+4+1+23)
	binary.LittleEndian.PutUint32(p, hr41.CapabilityFlags|mysql.CLIENT_SSL)
	binary.LittleEndian.PutUint32(p[4:], hr41.MaxPacketSize)
	p[8] = hr41.CharacterSet
	return p
}

// readPacket reads one MySQL packet's payload from conn.
func readPacket(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		t.Fatalf("the application waited for a packet and got %v", err)
	}
	payload := make([]byte, int(hdr[0])|int(hdr[1])<<8|int(hdr[2])<<16)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("the application read a packet header and then %v", err)
	}
	return payload
}

// sendPacket writes payload to conn as the MySQL packet numbered seq.
func sendPacket(t *testing.T, conn net.Conn, seq uint8, payload []byte) {
	t.Helper()
	pkt := make([]byte, 4, 4+len(payload))
	binary.LittleEndian.PutUint32(pkt, uint32(len(payload)))
	pkt[3] = seq
	if _, err := conn.Write(append(pkt, payload...)); err != nil {
		t.Fatalf("the application's packet %d: %v", seq, err)
	}
}

// recordedPacket is payload, the packet numbered seq that a client sent
// after greeting, the server's last packet of type after, as a recording
// holds it: decoded as the recorder decodes it.
func recordedPacket(t *testing.T, greeting *mysql.HandshakeV10Packet, after byte, seq uint8, payload []byte) mysql.PacketBundle {
	t.Helper()
	conn, other := net.Pipe()
	defer func() { _ = conn.Close(); _ = other.Close() }()
	dctx := &wire.DecodeContext{Mode: models.MODE_RECORD, LastOp: wire.NewLastOpMap(), ServerGreetings: wire.NewGreetings()}
	dctx.LastOp.Store(conn, after)
	dctx.ServerGreetings.Store(conn, greeting)
	pkt := make([]byte, 4, 4+len(payload))
	binary.LittleEndian.PutUint32(pkt, uint32(len(payload)))
	pkt[3] = seq
	b, err := wire.DecodePayload(context.Background(), zap.NewNop(), append(pkt, payload...), conn, dctx)
	if err != nil {
		t.Fatal(err)
	}
	return *b
}

// recordedLogin is m, a recorded login of replayer's tests, with hr41 as the
// client's HandshakeResponse41 and every packet the client sent as a
// recording holds it (recordedPacket): a request for the server's public
// key decoded as well.
func recordedLogin(t *testing.T, m *models.Mock, hr41 *mysql.HandshakeResponse41Packet) *models.Mock {
	t.Helper()
	payload, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), hr41)
	if err != nil {
		t.Fatal(err)
	}
	greeting := m.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet)
	m.Spec.MySQLRequests[0].PacketBundle = recordedPacket(t, greeting, mysql.HandshakeV10, 1, payload)
	for i := range m.Spec.MySQLRequests {
		if h := m.Spec.MySQLRequests[i].Header; h != nil && h.Type == mysql.CachingSha2PasswordToString(mysql.RequestPublicKey) {
			m.Spec.MySQLRequests[i].PacketBundle = recordedPacket(t, greeting, mysql.AuthMoreData, h.Header.SequenceID, []byte{byte(mysql.RequestPublicKey)})
		}
	}
	return m
}

// withPid is the recorded client handshake replayer.HandshakeMockForTest
// holds, with connection attributes that name the client process pid, as a
// MySQL driver sends them: another at every run.
func withPid(pid string) *mysql.HandshakeResponse41Packet {
	hr := replayer.ClientHandshakeForTest()
	hr.CapabilityFlags |= mysql.CLIENT_CONNECT_ATTRS
	hr.ConnectionAttributes = map[string]string{"_client_name": "Go-MySQL-Driver", "_pid": pid}
	return hr
}

// Every connection the MySQL replayer opens from a recorded handshake under
// the proven switch states what that serve gave the application
// (learn.Tracker.ServedMockAsRecorded where the application sent the
// recorded packets and F wrote the recorded answers, else
// learn.Tracker.ServedMockLive with the packets as the application sent
// them and the answers F wrote). A later call of the set then reads the
// handshake with that view: a changed value that holds no word of it is
// learned; one that holds a word only the live packets or F's answers held
// there is refused, naming the handshake at that place. Before, the
// handshake's serve (serveVerdict's updateMock) was numbered and no
// statement followed: the later call refused every changed value of the set
// ("its adapter did not say what it gave the application there"), so one
// MySQL connection opened anywhere earlier in a set refused every learned
// value after it.
func TestEveryMySQLHandshakeServeStatesTheViewItGave(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const (
		unstated = "where its adapter did not say what it gave the application there"
		path     = "body.ref"
		recorded = "82"
		fresh    = "9f3e"
	)
	// Each login is recorded with a client of pid 6801; the server's
	// greeting names the connection 7.
	switched := func(n int) func(app net.Conn) {
		return func(app net.Conn) {
			if req := readPacket(t, app); len(req) == 0 || req[0] != mysql.AuthSwitchRequest {
				t.Fatalf("the application read %x; want the recorded AuthSwitchRequest", req)
			}
			sendPacket(t, app, 3, bytes.Repeat([]byte{0x3C}, n))
		}
	}
	fullAuth := func(n int) func(app net.Conn) {
		return func(app net.Conn) {
			for _, step := range []string{"PerformFullAuthentication", "public key"} {
				if more := readPacket(t, app); len(more) == 0 || more[0] != mysql.AuthMoreData {
					t.Fatalf("the application read %x; want the recorded %s", more, step)
				}
				if step == "PerformFullAuthentication" {
					sendPacket(t, app, 3, []byte{byte(mysql.RequestPublicKey)})
				}
			}
			sendPacket(t, app, 5, bytes.Repeat([]byte{0x5A}, n)) // the password, encrypted with that key
		}
	}
	for _, tc := range []struct {
		name       string
		login      string // "native", "switched", "fullauth", "greeted"
		pid        string // the live client's pid
		n          int    // the length of the live client's auth data
		value      string // the later call's changed value
		asRecorded bool
		why        string // "" learns; else a part of the refusal
	}{
		{"a handshake sent as recorded, a fresh value", "native", "6801", 0, fresh, true, ""},
		{"a handshake from another process, a fresh value", "native", "4242", 0, fresh, false, ""},
		{"a handshake from another process, the pid only it sent", "native", "4242", 0, "4242", false, "as record's full round gave it"},
		{"an auth switch answered with another scramble of the recorded length", "switched", "6801", 20, fresh, true, ""},
		{"an auth switch answered with a scramble of another length", "switched", "6801", 21, fresh, false, ""},
		{"a full authentication with another encryption of the recorded length", "fullauth", "6801", 256, fresh, true, ""},
		{"a full authentication from another process, the pid only it sent", "fullauth", "4242", 256, "4242", false, "as record's full round gave it"},
		{"a greeting from another recording, a fresh value", "greeted", "6801", 0, fresh, false, ""},
		{"a greeting from another recording, the connection id only it held", "greeted", "6801", 0, "4243", false, "as record's full round gave it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				served *models.Mock
				recs   []*models.Mock
				login  func(app net.Conn)
			)
			switch tc.login {
			case "native":
				served = recordedLogin(t, replayer.HandshakeMockForTest(), withPid("6801"))
			case "switched":
				served, login = recordedLogin(t, replayer.SwitchedHandshakeForTest(20), withPid("6801")), switched(tc.n)
			case "fullauth":
				served, login = recordedLogin(t, replayer.FullAuthHandshakeForTest(256), withPid("6801")), fullAuth(tc.n)
			case "greeted":
				// The greeting comes from the first handshake recorded for
				// the server (connection 4243), whose client logged in as
				// another user; the client's answer then picks the second.
				other := withPid("6801")
				other.Username = "other"
				first := recordedLogin(t, replayer.HandshakeMockForTest(), other)
				first.Name = "mock-first"
				first.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet).ConnectionID = 4243
				served = recordedLogin(t, replayer.HandshakeMockForTest(), withPid("6801"))
				recs = []*models.Mock{first}
			}
			recs = append(recs, served)
			r := handshakeServedOn(t, learn.NewTracker(), recs, served, withPid(tc.pid), login)
			rec := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: recorded}}}
			now := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: tc.value}}}
			got := learn.Classify(learn.NewDriftCache(), []string{path}, now, rec, time.Now(), nil, []learn.Raw{r}, nil)
			if len(got) != 1 {
				t.Fatalf("%d verdicts; want one", len(got))
			}
			switch {
			case tc.why == "" && got[0].Kind == learn.KindUnaccounted:
				t.Fatalf("FALSE RED: %s refused (%q); want learned: no word of it stands in what F gave at %s", tc.value, got[0].Why, served.Name)
			case tc.why != "" && got[0].Kind != learn.KindUnaccounted:
				t.Fatalf("FALSE GREEN: %s %s (%q); want refused, naming %s", tc.value, got[0].Kind, got[0].Why, tc.why)
			case tc.why != "" && (!strings.Contains(got[0].Why, served.Name) || !strings.Contains(got[0].Why, tc.why) || strings.Contains(got[0].Why, unstated)):
				t.Fatalf("FALSE REASON: %s refused (%q); want it to name %s by what F gave there: %s", tc.value, got[0].Why, served.Name, tc.why)
			}
			if r.AsRecorded() != tc.asRecorded || r.Live() == tc.asRecorded {
				t.Fatalf("%s read as recorded %v, with a view %v; want as recorded %v", served.Name, r.AsRecorded(), r.Live(), tc.asRecorded)
			}
		})
	}
}

// tlsClient is withPid(pid) from a client that asked for TLS: CLIENT_SSL is
// among its capability flags, in its SSLRequest and in the
// HandshakeResponse41 it sends once TLS is up.
func tlsClient(pid string) *mysql.HandshakeResponse41Packet {
	hr := withPid(pid)
	hr.CapabilityFlags |= mysql.CLIENT_SSL
	return hr
}

// recordedTLSLogin is a recorded native-password login over TLS that the
// server let in, named mock-0: the greeting, the client's SSLRequest, the
// HandshakeResponse41 hr41 it sent once TLS was up, and the OK; each packet
// the client sent as a recording holds it (recordedPacket).
func recordedTLSLogin(t *testing.T, hr41 *mysql.HandshakeResponse41Packet) *models.Mock {
	t.Helper()
	m := replayer.HandshakeMockForTest()
	greeting := m.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet)
	payload, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), hr41)
	if err != nil {
		t.Fatal(err)
	}
	m.Spec.MySQLRequests = []mysql.Request{
		{PacketBundle: recordedPacket(t, greeting, mysql.HandshakeV10, 1, sslRequestPayload(hr41))},
		{PacketBundle: recordedPacket(t, greeting, mysql.HandshakeV10, 2, payload)},
	}
	if typ := m.Spec.MySQLRequests[0].Header.Type; typ != mysql.SSLRequest {
		t.Fatalf("the recorded SSLRequest decoded as a %s", typ)
	}
	m.Spec.MySQLResponses[1].Header.Header.SequenceID = 3
	return m
}

// A connection whose client asks for TLS states its view as one without TLS
// does: the packets the application sent are its SSLRequest and the
// HandshakeResponse41 it sent over TLS, read after the MITM upgrade, and the
// view is as recorded only where both are the recording's own. A recording
// made without TLS serves such a client too (no recorded SSLRequest), and its
// serve is a view of the packets the client sent; so is the serve of a
// recording whose SSLRequest alone differs from the one the client sent (its
// max packet size), every later packet as recorded. Before this row no
// connection of the package's rows asked for TLS, so the HandshakeResponse41
// read after the upgrade could go unnoted, and a pid only the live client
// sent there would be learned (a false green), or the view stated without it.
func TestAMySQLHandshakeOverTLSStatesTheViewItGave(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const (
		unstated = "where its adapter did not say what it gave the application there"
		path     = "body.ref"
		recorded = "82"
		fresh    = "9f3e"
	)
	for _, tc := range []struct {
		name       string
		plaintext  bool   // the recording was made without TLS
		otherSSL   bool   // the recorded SSLRequest gives another max packet size
		pid        string // the live client's pid
		value      string // the later call's changed value
		asRecorded bool
		why        string // "" learns; else a part of the refusal
	}{
		{"a TLS handshake sent as recorded, a fresh value", false, false, "6801", fresh, true, ""},
		{"a TLS handshake from another process, a fresh value", false, false, "4242", fresh, false, ""},
		{"a TLS handshake from another process, the pid only it sent over TLS", false, false, "4242", "4242", false, "as record's full round gave it"},
		{"a TLS client served from a recording without TLS, a fresh value", true, false, "6801", fresh, false, ""},
		{"a TLS client served from a recording without TLS, the pid only it sent", true, false, "4242", "4242", false, "as record's full round gave it"},
		{"a TLS handshake whose SSLRequest alone is not the recording's, a fresh value", false, true, "6801", fresh, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := recordedTLSLogin(t, tlsClient("6801"))
			if tc.plaintext {
				served = recordedLogin(t, replayer.HandshakeMockForTest(), withPid("6801"))
			}
			if tc.otherSSL {
				other := tlsClient("6801")
				other.MaxPacketSize /= 2
				greeting := served.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet)
				served.Spec.MySQLRequests[0].PacketBundle = recordedPacket(t, greeting, mysql.HandshakeV10, 1, sslRequestPayload(other))
			}
			r := handshakeServedOver(t, learn.NewTracker(), []*models.Mock{served}, served, tlsClient(tc.pid), nil, true)
			rec := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: recorded}}}
			now := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: tc.value}}}
			got := learn.Classify(learn.NewDriftCache(), []string{path}, now, rec, time.Now(), nil, []learn.Raw{r}, nil)
			if len(got) != 1 {
				t.Fatalf("%d verdicts; want one", len(got))
			}
			switch {
			case tc.why == "" && got[0].Kind == learn.KindUnaccounted:
				t.Fatalf("FALSE RED: %s refused (%q); want learned: no word of it stands in what F gave at %s", tc.value, got[0].Why, served.Name)
			case tc.why != "" && got[0].Kind != learn.KindUnaccounted:
				t.Fatalf("FALSE GREEN: %s %s (%q); want refused, naming %s", tc.value, got[0].Kind, got[0].Why, tc.why)
			case tc.why != "" && (!strings.Contains(got[0].Why, served.Name) || !strings.Contains(got[0].Why, tc.why) || strings.Contains(got[0].Why, unstated)):
				t.Fatalf("FALSE REASON: %s refused (%q); want it to name %s by what F gave there: %s", tc.value, got[0].Why, served.Name, tc.why)
			}
			if r.AsRecorded() != tc.asRecorded || r.Live() == tc.asRecorded {
				t.Fatalf("%s read as recorded %v, with a view %v; want as recorded %v", served.Name, r.AsRecorded(), r.Live(), tc.asRecorded)
			}
		})
	}
}

// With the proven switch off the connection phase states nothing, and serves
// as it did: the recorded verdict, and the handshake read as a serve no
// adapter stated (which no learn step reads with the switch off); over TLS
// as well.
func TestAMySQLHandshakeStatesNothingWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "0")
	for _, overTLS := range []bool{false, true} {
		served := recordedLogin(t, replayer.HandshakeMockForTest(), withPid("6801"))
		hr41 := withPid("4242")
		if overTLS {
			served, hr41 = recordedTLSLogin(t, tlsClient("6801")), tlsClient("4242")
		}
		r := handshakeServedOver(t, learn.NewTracker(), []*models.Mock{served}, served, hr41, nil, overTLS)
		if r.AsRecorded() || r.Live() {
			t.Fatalf("%s (over TLS %v) read as recorded %v, with a view %v; want nothing stated with the switch off", served.Name, overTLS, r.AsRecorded(), r.Live())
		}
	}
}
