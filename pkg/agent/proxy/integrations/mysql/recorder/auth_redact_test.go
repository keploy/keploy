package recorder

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// rsaEncrypted stands for the password a caching_sha2_password full
// authentication sends RSA-encrypted with the server's public key when there
// is no TLS. The server's private key decrypts it.
var rsaEncrypted = bytes.Repeat([]byte{0x9E}, 256)

// recordedPassword is the database password the application sends in the
// tests below. It must never reach a mock, in any format, nor a log line.
const recordedPassword = "Tr0ub4dor&3-recorded"

// scramble is the auth response of a HandshakeResponse41 or an
// AuthSwitchResponse under caching_sha2_password: derived from the password
// and the greeting's salt, which the recording keeps, so it is as good as the
// password to a dictionary attack. Replay never reads it either.
var scramble = bytes.Repeat([]byte{0xC7}, 32)

// cannedSha2Greeting is a HandshakeV10 that announces caching_sha2_password,
// MySQL 8's default, whose full authentication sends the password in clear
// over TLS.
func cannedSha2Greeting(t *testing.T) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL | mysql.CLIENT_SECURE_CONNECTION)
	buf, err := connphase.EncodeHandshakeV10(context.Background(), zap.NewNop(), &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a,
		ServerVersion:   "8.4.test-keploy",
		ConnectionID:    7,
		// A real server's 20-byte salt and its NUL: the length the
		// greeting states is then what its decoder reads.
		AuthPluginData:  append(bytes.Repeat([]byte{0x22}, 20), 0x00),
		CapabilityFlags: caps,
		CharacterSet:    0x21,
		StatusFlags:     0x02,
		AuthPluginName:  string(mysql.CachingSha2),
	})
	if err != nil {
		t.Fatalf("encode greeting: %v", err)
	}
	return wrapPacket(buf, 0)
}

// cannedHR41 is a HandshakeResponse41 under plugin whose auth response is
// authResponse.
func cannedHR41(t *testing.T, seq byte, sslBit bool, plugin string, authResponse []byte) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION)
	if sslBit {
		caps |= mysql.CLIENT_SSL
	}
	buf, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), &mysql.HandshakeResponse41Packet{
		CapabilityFlags: caps,
		MaxPacketSize:   1 << 24,
		CharacterSet:    0x21,
		Username:        "appuser",
		AuthResponse:    authResponse,
		AuthPluginName:  plugin,
	})
	if err != nil {
		t.Fatalf("encode handshake response 41: %v", err)
	}
	return wrapPacket(buf, seq)
}

// fullAuthRequest is the server's AuthMoreData asking caching_sha2_password
// for a full authentication.
func fullAuthRequest(seq byte) []byte {
	return wrapPacket([]byte{mysql.AuthMoreData, byte(mysql.PerformFullAuthentication)}, seq)
}

// switchTo is the server's AuthSwitchRequest to plugin.
func switchTo(plugin string, seq byte) []byte {
	return wrapPacket(append(append([]byte{mysql.AuthSwitchRequest}, plugin...), 0x00), seq)
}

// clearPassword is the password as mysql_clear_password and a full
// authentication over TLS send it: in clear, NUL-terminated.
func clearPassword(seq byte) []byte {
	return wrapPacket(append([]byte(recordedPassword), 0x00), seq)
}

// TestARecordingKeepsNoCredential records each way a client sends its
// credential, and checks that no mock holds any of it, in any format a
// recording is written in, and that no log line does. The cleartext password
// of a caching_sha2_password full authentication over TLS (MySQL 8's default
// on a cold server cache) used to be written to the mocks, base64 encoded,
// as was the cleartext password of mysql_clear_password (PAM, LDAP, cloud
// IAM tokens): anyone with read access to a test set had the database
// password. Replay checks neither: a client's authentication data is matched
// by its sequence id only.
//
// What a recording keeps of it is its length, as zero bytes: its packet
// header and the length its encoding states then agree with what is
// recorded.
func TestARecordingKeepsNoCredential(t *testing.T) {
	sha2Greeting := cannedSha2Greeting(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), sha2Greeting[4:])
	if err != nil {
		t.Fatalf("decode greeting: %v", err)
	}
	okPkt := func(seq byte) []byte { return cannedOK(t, seq, greeting.CapabilityFlags) }

	for _, tc := range []struct {
		name string
		// postTLS records the decrypted stream of a capture that cannot
		// MITM the TLS (the uprobe path); otherwise the relay upgrades,
		// unless noTLS.
		postTLS, noTLS bool
		client         [][]byte // after the SSLRequest, if any
		server         [][]byte // after the greeting
		// want are the client packets the config mock records after the
		// SSLRequest, by type, with each one's payload length.
		want []wantAuthReq
	}{
		{
			name:   "a caching_sha2_password full authentication without TLS",
			noTLS:  true,
			client: [][]byte{cannedHR41(t, 1, false, string(mysql.CachingSha2), scramble), wrapPacket([]byte{0x02}, 3), wrapPacket(rsaEncrypted, 5)},
			server: [][]byte{fullAuthRequest(2), wrapPacket(append([]byte{mysql.AuthMoreData}, "-----BEGIN PUBLIC KEY-----\nMIIB\n-----END PUBLIC KEY-----\n"...), 4), okPkt(6)},
			want: []wantAuthReq{{mysql.HandshakeResponse41, 0, 32, "appuser"}, {mysql.CachingSha2PasswordToString(mysql.RequestPublicKey), 3, -1, ""},
				{mysql.EncryptedPassword, 5, len(rsaEncrypted), ""}},
		},
		{
			name:   "a caching_sha2_password full authentication without TLS, by a client that holds the server's key",
			noTLS:  true,
			client: [][]byte{cannedHR41(t, 1, false, string(mysql.CachingSha2), scramble), wrapPacket(rsaEncrypted, 3)},
			server: [][]byte{fullAuthRequest(2), okPkt(4)},
			want:   []wantAuthReq{{mysql.HandshakeResponse41, 0, 32, "appuser"}, {mysql.EncryptedPassword, 3, len(rsaEncrypted), ""}},
		},
		{
			name:   "a caching_sha2_password full authentication over TLS",
			client: [][]byte{cannedHR41(t, 2, true, string(mysql.CachingSha2), scramble), clearPassword(4)},
			server: [][]byte{fullAuthRequest(3), okPkt(5)},
			want:   []wantAuthReq{{mysql.HandshakeResponse41, 0, 32, "appuser"}, {mysql.PlainPassword, 4, len(recordedPassword) + 1, ""}},
		},
		{
			name:    "a caching_sha2_password full authentication on a decrypted TLS stream",
			postTLS: true,
			client:  [][]byte{cannedHR41(t, 2, true, string(mysql.CachingSha2), scramble), clearPassword(4)},
			server:  [][]byte{fullAuthRequest(3), okPkt(5)},
			want:    []wantAuthReq{{mysql.HandshakeResponse41, 0, 32, "appuser"}, {mysql.PlainPassword, 4, len(recordedPassword) + 1, ""}},
		},
		{
			name:   "a switch to mysql_clear_password",
			client: [][]byte{cannedHR41(t, 2, true, string(mysql.CachingSha2), scramble), clearPassword(4)},
			server: [][]byte{switchTo("mysql_clear_password", 3), okPkt(5)},
			want:   []wantAuthReq{{mysql.HandshakeResponse41, 0, 32, "appuser"}, {mysql.AuthSwithResponse, 4, len(recordedPassword) + 1, ""}},
		},
		{
			name:    "a switch to mysql_clear_password on a decrypted TLS stream",
			postTLS: true,
			client:  [][]byte{cannedHR41(t, 2, true, string(mysql.CachingSha2), scramble), clearPassword(4)},
			server:  [][]byte{switchTo("mysql_clear_password", 3), okPkt(5)},
			want:    []wantAuthReq{{mysql.HandshakeResponse41, 0, 32, "appuser"}, {mysql.AuthSwithResponse, 4, len(recordedPassword) + 1, ""}},
		},
		{
			name:   "mysql_clear_password in the HandshakeResponse41",
			client: [][]byte{cannedHR41(t, 2, true, "mysql_clear_password", append([]byte(recordedPassword), 0x00))},
			server: [][]byte{okPkt(3)},
			want:   []wantAuthReq{{mysql.HandshakeResponse41, 0, len(recordedPassword) + 1, "appuser"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newV2Harness(t)
			logger, logs := encodedLogger()
			h.logger, h.sess.Logger = logger, logger
			base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }

			sslReq := cannedSSLRequest(t, 1)
			ctx := context.Background()
			switch {
			case tc.noTLS:
				h.pushDest(sha2Greeting, at(0))
			case tc.postTLS:
				ctx = postTLSCtx(t, sha2Greeting, sslReq, base, 3306)
			default:
				h.pushDest(sha2Greeting, at(0))
				h.pushClient(sslReq, at(1))
				go func() {
					select {
					case d := <-h.dirs:
						h.acks <- directive.Ack{Kind: d.Kind, OK: true, BoundaryReadAt: at(2), BoundaryWrittenAt: at(2)}
					case <-time.After(5 * time.Second):
						t.Errorf("the recorder never asked for the TLS upgrade")
					}
				}()
			}
			for i := range tc.client {
				h.pushClient(tc.client[i], at(10+2*i))
				h.pushDest(tc.server[i], at(11+2*i))
			}

			cfg := recordConfigMock(t, h, ctx)
			reqs := cfg.Spec.MySQLRequests
			if !tc.noTLS {
				if len(reqs) == 0 || reqs[0].Header == nil || reqs[0].Header.Type != mysql.SSLRequest {
					t.Fatalf("the config mock records %s; want the SSLRequest first", reqTypes(reqs))
				}
				reqs = reqs[1:]
			}
			if len(reqs) != len(tc.want) {
				t.Fatalf("the config mock records %s after the greeting; want %d requests", reqTypes(reqs), len(tc.want))
			}
			for i, w := range tc.want {
				w.check(t, reqs[i])
			}

			for _, f := range []string{"yaml", "json", "gob"} {
				written, read := writeMock(t, cfg, f)
				if secret := heldSecret(written); secret != "" {
					t.Errorf("the %s mocks file holds the client's credential (%s)", f, secret)
				}
				// What replay reads back is the recorded form: the
				// packets replay checks, by type, sequence and length.
				reqs := read.Spec.MySQLRequests
				if !tc.noTLS && len(reqs) > 0 {
					reqs = reqs[1:]
				}
				if len(reqs) != len(tc.want) {
					t.Fatalf("the %s mocks file reads back %s; want %d requests after the greeting", f, reqTypes(reqs), len(tc.want))
				}
				for i, w := range tc.want {
					w.check(t, reqs[i])
				}
			}
			if secret := heldSecret(logs.Bytes()); secret != "" {
				t.Errorf("the log holds the client's credential (%s):\n%s", secret, logs.Bytes())
			}
		})
	}
}

// encodedLogger is a Debug logger whose lines are written, as an agent writes
// them, by zap's JSON and console encoders into the returned buffer: a
// credential logged in a field of any type is in it as the encoder renders it
// (a []byte field as base64, a value as %v).
func encodedLogger() (*zap.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	cfg := zap.NewDevelopmentEncoderConfig()
	core := zapcore.NewTee(
		zapcore.NewCore(zapcore.NewJSONEncoder(cfg), zapcore.AddSync(buf), zapcore.DebugLevel),
		zapcore.NewCore(zapcore.NewConsoleEncoder(cfg), zapcore.AddSync(buf), zapcore.DebugLevel),
	)
	return zap.New(core), buf
}

// syncBuffer is a bytes.Buffer goroutines may write to at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// wantAuthReq is a recorded client packet of type typ: its sequence id (for
// a packet replay checks by it) and the length of its payload, or for a
// HandshakeResponse41 of its auth response.
type wantAuthReq struct {
	typ string
	seq byte
	n   int
	// user is a HandshakeResponse41's user, which replay matches on: it is
	// kept as sent.
	user string
}

func (w wantAuthReq) check(t *testing.T, r mysql.Request) {
	t.Helper()
	if r.Header == nil || r.Header.Type != w.typ {
		t.Errorf("recorded %s; want a %s", reqTypes([]mysql.Request{r}), w.typ)
		return
	}
	if w.n < 0 {
		return // not authentication data: recorded as decoded
	}
	if w.typ == mysql.HandshakeResponse41 {
		hr, ok := r.Message.(*mysql.HandshakeResponse41Packet)
		if !ok {
			t.Errorf("the recorded HandshakeResponse41 holds a %T", r.Message)
			return
		}
		if !bytes.Equal(hr.AuthResponse, make([]byte, w.n)) {
			t.Errorf("the recorded HandshakeResponse41's auth response is %x; want its length (%d) in zero bytes, and nothing of it", hr.AuthResponse, w.n)
		}
		if hr.Username != w.user {
			t.Errorf("the recorded HandshakeResponse41's user is %q; want %q, which replay matches on, kept", hr.Username, w.user)
		}
		return
	}
	if r.Header.Header == nil || r.Header.Header.SequenceID != w.seq || r.Header.Header.PayloadLength != uint32(w.n) {
		t.Errorf("the recorded %s's header is %+v; want sequence %d and length %d, which replay checks", w.typ, r.Header.Header, w.seq, w.n)
	}
	if r.Message == nil && w.typ == mysql.AuthSwithResponse {
		// What a mocks file reads back as: replay checks an AuthSwitchResponse
		// by its sequence id only, so the reader keeps none of its message.
		return
	}
	msg, ok := r.Message.(string)
	if !ok {
		t.Errorf("the recorded %s holds a %T; want a string, which replay requires", w.typ, r.Message)
		return
	}
	if got, err := base64.StdEncoding.DecodeString(msg); err != nil || !bytes.Equal(got, make([]byte, w.n)) {
		t.Errorf("the recorded %s is %q (%x, %v); want its length (%d) in zero bytes, and nothing of it", w.typ, msg, got, err, w.n)
	}
}

func reqTypes(reqs []mysql.Request) string {
	var b bytes.Buffer
	for i, r := range reqs {
		if i > 0 {
			b.WriteString(", ")
		}
		if r.Header == nil {
			b.WriteString("<no header>")
			continue
		}
		b.WriteString(r.Header.Type)
	}
	return b.String()
}

// recordConfigMock runs RecordV2 on what h holds and returns the config mock
// it records, the first mock.
func recordConfigMock(t *testing.T, h *v2Harness, ctx context.Context) *models.Mock {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RecordV2(ctx, h.logger, h.sess) }()
	var cfg *models.Mock
	select {
	case cfg = <-h.mocks:
	case err := <-done:
		t.Fatalf("RecordV2 returned %v before it recorded the handshake", err)
	case <-ctx.Done():
		t.Fatal("no config mock was recorded")
	}
	h.closeStreams()
	if err := <-done; err != nil {
		t.Fatalf("RecordV2 returned %v", err)
	}
	if cfg.Name != "config" {
		t.Fatalf("the first mock is %q; want the config mock", cfg.Name)
	}
	return cfg
}

// writeMock writes m to a test set the way a recording does, in format
// (yaml, json or gob), and returns what was written and the mock replay reads
// back from it.
func writeMock(t *testing.T, m *models.Mock, format string) ([]byte, *models.Mock) {
	t.Helper()
	dir := t.TempDir()
	var db *mockdb.MockYaml
	switch format {
	case "json":
		db = mockdb.NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatJSON)
	case "gob":
		db = mockdb.New(zap.NewNop(), dir, "mocks")
		db.MockFormat = "gob"
	default:
		db = mockdb.New(zap.NewNop(), dir, "mocks")
		db.MockFormat = "yaml"
	}
	written := *m // InsertMock names the mock it writes
	if err := db.InsertMock(context.Background(), &written, "test-set-0"); err != nil {
		t.Fatalf("writing the %s mock: %v", format, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the %s mock db: %v", format, err)
	}
	pools, err := db.GetTestSetMocks(context.Background(), "test-set-0", time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatalf("reading the %s mock back: %v", format, err)
	}
	var read *models.Mock
	for _, rm := range append(append([]*models.Mock{}, pools.Filtered...), pools.Unfiltered...) {
		if rm.Name == written.Name {
			read = rm
		}
	}
	if read == nil {
		t.Fatalf("the %s mock %q does not read back (filtered %d, unfiltered %d)", format, written.Name, len(pools.Filtered), len(pools.Unfiltered))
	}
	var all []byte
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		all = append(all, b...)
		return err
	})
	if err != nil || len(all) == 0 {
		t.Fatalf("reading the %s mocks file: %d bytes, %v", format, len(all), err)
	}
	return all, read
}

// heldSecret names the form of the client's credential b holds, if any: the
// password, the scramble or the encrypted password the tests send.
func heldSecret(b []byte) string {
	return heldSecretOf(b, map[string][]byte{"password": []byte(recordedPassword), "scramble": scramble, "encrypted password": rsaEncrypted})
}

// heldSecretOf names the form in which b holds one of secrets, if any: raw,
// hex, base64 encoded at any offset into what was encoded, or as a list of
// byte values (YAML's flow list of a []byte, fmt's %v).
func heldSecretOf(b []byte, secrets map[string][]byte) string {
	for name, secret := range secrets {
		if bytes.Contains(b, secret) {
			return "the raw " + name
		}
		values := make([]string, 0, len(secret))
		for _, c := range secret {
			values = append(values, strconv.Itoa(int(c)))
		}
		// %x and hex.EncodeToString, in either case.
		if h := hex.EncodeToString(secret); bytes.Contains(b, []byte(h)) || bytes.Contains(b, []byte(strings.ToUpper(h))) {
			return "the hex " + name
		}
		// YAML's flow list, and fmt's %v of a []byte.
		for _, sep := range []string{", ", " "} {
			if bytes.Contains(b, []byte(strings.Join(values, sep))) {
				return "the " + name + "'s byte values"
			}
		}
		// Base64 of a payload holding the secret at any offset holds the
		// encoding of the secret's bytes from the first one at a multiple
		// of three into the payload: try each of the three.
		for skip := 0; skip < 3 && len(secret)-skip >= 6; skip++ {
			part := secret[skip:]
			part = part[:len(part)-len(part)%3]
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
				if bytes.Contains(b, []byte(strings.TrimRight(enc.EncodeToString(part), "="))) {
					return "the base64 " + name
				}
			}
		}
	}
	return ""
}

// A client that changes its user mid-connection (COM_CHANGE_USER) answers
// the server's auth switch with its credential in the command phase. The
// recorder does not follow that exchange: it leaves it out and goes on with
// the next command (a client packet that is not packet 0 of a new sequence
// never starts a command). Nothing of the credential reaches a mock or a log.
func TestAChangeOfUserKeepsNoCredential(t *testing.T) {
	h := newV2Harness(t)
	logger, logs := encodedLogger()
	h.logger, h.sess.Logger = logger, logger
	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }

	greetingBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
	if err != nil {
		t.Fatalf("decode greeting: %v", err)
	}
	h.pushDest(greetingBuf, at(0))
	h.pushClient(cannedHandshakeResponse41(t, 1, false), at(1))
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), at(2))

	// COM_CHANGE_USER: user, its scramble, database, plugin.
	change := append([]byte{mysql.COM_CHANGE_USER}, "otheruser\x00"...)
	change = append(change, byte(len(scramble)))
	change = append(change, scramble...)
	change = append(change, "appdb\x00"...)
	change = append(change, 0x21, 0x00)
	change = append(change, "caching_sha2_password\x00"...)
	h.pushClient(wrapPacket(change, 0), at(10))
	h.pushDest(switchTo("mysql_clear_password", 1), at(11))
	h.pushClient(clearPassword(2), at(12))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), at(13))
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), at(20))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), at(21))
	h.closeStreams()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = RecordV2(ctx, logger, h.sess) // how it ends is not what this checks
	close(h.mocks)
	var recorded []string
	for m := range h.mocks {
		recorded = append(recorded, reqTypes(m.Spec.MySQLRequests))
		for _, f := range []string{"yaml", "json", "gob"} {
			if written, _ := writeMock(t, m, f); heldSecret(written) != "" {
				secret := heldSecret(written)
				t.Errorf("the %s mock %q holds the client's credential (%s)", f, m.Name, secret)
			}
		}
	}
	if fmt.Sprint(recorded) != "[HandshakeResponse41 COM_QUERY]" {
		t.Errorf("recorded %q; want the handshake and the query after the change of user, and nothing of the change", recorded)
	}
	if secret := heldSecret(logs.Bytes()); secret != "" {
		t.Errorf("the log holds the client's credential (%s):\n%s", secret, logs.Bytes())
	}
}

// The legacy recorder, which proxies the bytes itself, forwards the client's
// credential to the server untouched and records only its length: on a full
// authentication (over TLS; without it, asking for the server's public key or
// holding it already), and on an auth switch.
func TestLegacyRecordingKeepsNoCredential(t *testing.T) {
	sha2Greeting := cannedSha2Greeting(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), sha2Greeting[4:])
	if err != nil {
		t.Fatalf("decode greeting: %v", err)
	}

	ok := func(seq byte) []byte { return cannedOK(t, seq, greeting.CapabilityFlags) }
	publicKey := wrapPacket(append([]byte{mysql.AuthMoreData}, "-----BEGIN PUBLIC KEY-----\nMIIB\n-----END PUBLIC KEY-----\n"...), 5)
	for _, tc := range []struct {
		name string
		tls  bool
		// client and server are the packets each sends, in turn, after the
		// server asked for a full authentication.
		client, server [][]byte
		want           []wantAuthReq
	}{
		{
			name: "a full authentication over TLS", tls: true,
			client: [][]byte{clearPassword(4)}, server: [][]byte{ok(5)},
			want: []wantAuthReq{{mysql.PlainPassword, 4, len(recordedPassword) + 1, ""}},
		},
		{
			name:   "a full authentication without TLS, asking for the server's key",
			client: [][]byte{wrapPacket([]byte{byte(mysql.RequestPublicKey)}, 4), wrapPacket(rsaEncrypted, 6)}, server: [][]byte{publicKey, ok(7)},
			want: []wantAuthReq{{mysql.CachingSha2PasswordToString(mysql.RequestPublicKey), 4, -1, ""}, {mysql.EncryptedPassword, 6, len(rsaEncrypted), ""}},
		},
		{
			name:   "a full authentication without TLS, holding the server's key",
			client: [][]byte{wrapPacket(rsaEncrypted, 4)}, server: [][]byte{ok(5)},
			want: []wantAuthReq{{mysql.EncryptedPassword, 4, len(rsaEncrypted), ""}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, clientPeer := net.Pipe()
			dest, destPeer := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = dest.Close(); _ = clientPeer.Close(); _ = destPeer.Close() })
			forwarded := make(chan [][]byte, 1)
			go func() {
				var got [][]byte
				for i := range tc.client {
					_, _ = clientPeer.Write(tc.client[i])
					got = append(got, readPacket(destPeer))
					_, _ = destPeer.Write(tc.server[i])
					_ = readPacket(clientPeer)
				}
				forwarded <- got
				// The scripted exchange is over: a recorder that waits for
				// more ends on EOF instead of hanging.
				_ = clientPeer.Close()
				_ = destPeer.Close()
			}()
			decodeCtx := legacyDecodeCtx(client, greeting)
			decodeCtx.UseSSL = tc.tls
			logger, logs := encodedLogger()
			res, err := handleFullAuth(context.Background(), logger, client, dest, decodeCtx)
			if err != nil {
				t.Fatalf("handleFullAuth: %v", err)
			}
			if got := <-forwarded; fmt.Sprint(got) != fmt.Sprint(tc.client) {
				t.Fatalf("the server got %x; want the client's packets forwarded as sent", got)
			}
			if len(res.req) != len(tc.want) {
				t.Fatalf("recorded %s; want %d requests", reqTypes(res.req), len(tc.want))
			}
			for i, w := range tc.want {
				w.check(t, res.req[i])
			}
			if secret := heldSecret(logs.Bytes()); secret != "" {
				t.Errorf("the log holds the client's credential (%s):\n%s", secret, logs.Bytes())
			}
		})
	}

	t.Run("an auth switch", func(t *testing.T) {
		client, clientPeer := net.Pipe()
		dest, destPeer := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = dest.Close(); _ = clientPeer.Close(); _ = destPeer.Close() })
		hr41 := cannedHR41(t, 1, false, string(mysql.Native), scramble[:20])
		verdict := cannedOK(t, 4, greeting.CapabilityFlags)
		forwarded := make(chan [][]byte, 1)
		go func() {
			var got [][]byte
			_, _ = destPeer.Write(sha2Greeting)
			_ = readPacket(clientPeer)
			_, _ = clientPeer.Write(hr41)
			got = append(got, readPacket(destPeer))
			_, _ = destPeer.Write(switchTo(string(mysql.Native), 2))
			_ = readPacket(clientPeer)
			_, _ = clientPeer.Write(wrapPacket(scramble[:20], 3))
			got = append(got, readPacket(destPeer))
			_, _ = destPeer.Write(verdict)
			_ = readPacket(clientPeer)
			forwarded <- got
			_ = clientPeer.Close()
			_ = destPeer.Close()
		}()
		logger, logs := encodedLogger()
		res, err := handleInitialHandshake(context.Background(), logger, client, dest, legacyDecodeCtx(client, nil), models.OutgoingOptions{
			DstCfg: &models.ConditionalDstCfg{Addr: "10.0.0.9:3306", Port: 3306},
		}, nil)
		if err != nil {
			t.Fatalf("handleInitialHandshake: %v", err)
		}
		if secret := heldSecret(logs.Bytes()); secret != "" {
			t.Errorf("the log holds the client's credential (%s):\n%s", secret, logs.Bytes())
		}
		got := <-forwarded
		if !bytes.Equal(got[0], hr41) || !bytes.Equal(got[1], wrapPacket(scramble[:20], 3)) {
			t.Fatalf("the server got %x; want the client's packets forwarded as sent", got)
		}
		if len(res.req) != 2 {
			t.Fatalf("recorded %d requests (%s); want the HandshakeResponse41 and the AuthSwitchResponse", len(res.req), reqTypes(res.req))
		}
		wantAuthReq{mysql.HandshakeResponse41, 0, 20, "appuser"}.check(t, res.req[0])
		wantAuthReq{mysql.AuthSwithResponse, 3, 20, ""}.check(t, res.req[1])
	})
}

func legacyDecodeCtx(client net.Conn, greeting *mysql.HandshakeV10Packet) *wire.DecodeContext {
	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}
	if greeting != nil {
		decodeCtx.ServerGreetings.Store(client, greeting)
		decodeCtx.LastOp.Store(client, mysql.AuthMoreData)
		decodeCtx.PluginName = greeting.AuthPluginName
	}
	return decodeCtx
}

// readPacket reads one MySQL packet off c, header included.
func readPacket(c net.Conn) []byte {
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil
	}
	payload := make([]byte, int(hdr[0])|int(hdr[1])<<8|int(hdr[2])<<16)
	if _, err := io.ReadFull(c, payload); err != nil {
		return nil
	}
	return append(hdr[:], payload...)
}

// TestE2E_MySQLTLSFullAuth_RecordsNoPassword records a real client's login to
// a real MySQL 8 server over TLS, as a user the server has not cached yet, so
// caching_sha2_password authenticates it in full: the client sends its
// password in clear inside the TLS. The recording, written in every format,
// holds the exchange (a plain_password packet of the password's length) and
// not the password.
//
// It runs where TestE2E_MySQLTLSGreeting_* does (greeting_e2e_test.go), with
// its relay: KEPLOY_E2E_MYSQL_ADDR, a routable address, and
// KEPLOY_E2E_MYSQL_PASSWORD, the root password, through
// .github/workflows/test_workflow_scripts/mysql-tls-greeting-e2e.sh.
func TestE2E_MySQLTLSFullAuth_RecordsNoPassword(t *testing.T) {
	addr := os.Getenv(e2eMySQLAddrEnv)
	rootPassword := os.Getenv(e2eMySQLPasswordEnv)
	if addr == "" {
		t.Skipf("%s is not set; this end-to-end test needs a real MySQL 8 server", e2eMySQLAddrEnv)
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%s=%q: %v", e2eMySQLAddrEnv, addr, err)
	}
	port, _ := strconv.Atoi(portStr)

	// A user of its own, with a password of its own: the server has never
	// cached it, so its first login is a full authentication.
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	user := "kcred_" + hex.EncodeToString(suffix[:3])
	password := "e2e-Secret-" + hex.EncodeToString(suffix[3:]) + "-pw"
	admin, err := sql.Open("mysql", fmt.Sprintf("root:%s@tcp(%s)/?timeout=5s", rootPassword, addr))
	if err != nil {
		t.Fatalf("admin open: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.Exec(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED WITH caching_sha2_password BY '%s'", user, password)); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(fmt.Sprintf("DROP USER '%s'@'%%'", user)) })

	relay := startE2ERelay(t, addr, port)
	rec := relay.startRecording(models.NewTLSHandshakeStore(), addr, port, "e2e/app/test-set-credential")
	app, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/?tls=skip-verify&timeout=5s&readTimeout=30s", user, password, relay.addr))
	if err != nil {
		t.Fatalf("app open: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	var one int
	if err := app.QueryRow("SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("the application's query through the relay: %d, %v", one, err)
	}
	legs := rec.waitForQueries(t, 1, 45*time.Second)
	_ = app.Close() // the client quits, ending its streams
	rec.stop(t)

	var cfg *models.Mock
	for _, l := range legs {
		l.mu.Lock()
		for _, m := range l.got {
			if m.Name == "config" {
				cfg = m
			}
		}
		l.mu.Unlock()
	}
	if cfg == nil {
		t.Fatal("the recording holds no config mock for the login")
	}
	var plain, hr41 *mysql.Request
	for i, r := range cfg.Spec.MySQLRequests {
		if r.Header == nil {
			continue
		}
		switch r.Header.Type {
		case mysql.PlainPassword:
			plain = &cfg.Spec.MySQLRequests[i]
		case mysql.HandshakeResponse41:
			hr41 = &cfg.Spec.MySQLRequests[i]
		}
	}
	if plain == nil || hr41 == nil {
		t.Fatalf("the login recorded %s: no full authentication, so this run checks nothing", reqTypes(cfg.Spec.MySQLRequests))
	}
	wantAuthReq{mysql.PlainPassword, plain.Header.Header.SequenceID, len(password) + 1, ""}.check(t, *plain)
	// The client's scramble of its password, a SHA-256 one.
	wantAuthReq{mysql.HandshakeResponse41, 0, 32, user}.check(t, *hr41)
	secrets := map[string][]byte{"password": []byte(password)}
	for _, f := range []string{"yaml", "json", "gob"} {
		if written, _ := writeMock(t, cfg, f); heldSecretOf(written, secrets) != "" {
			t.Errorf("the %s mocks file holds the password (%s)", f, heldSecretOf(written, secrets))
		}
	}
}
