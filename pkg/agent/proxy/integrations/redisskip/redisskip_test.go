package redisskip

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The frames below are what real clients sent, byte for byte, captured with
// a fake RESP server on 2026-10-22: go-redis 9.7.1, redis-py 5.2.1, ioredis
// 5.4.2, jedis 5.2.0 and lettuce 6.5.1, each with default options and with
// a password and database 2. "first" marks a client's first frame on a
// connection, which is what Match is handed; the others are frames the same
// clients send later, which a client of another library sends first (redis-py
// opens with CLIENT SETINFO, jedis with PING).
var clientFrames = []struct {
	client, frame string
	first         bool
}{
	{"go-redis", "*2\r\n$5\r\nhello\r\n$1\r\n3\r\n", true},
	{"go-redis", "*5\r\n$5\r\nhello\r\n$1\r\n3\r\n$4\r\nauth\r\n$7\r\ndefault\r\n$2\r\npw\r\n", true},
	{"go-redis", "*4\r\n$6\r\nclient\r\n$7\r\nsetinfo\r\n$8\r\nLIB-NAME\r\n$19\r\ngo-redis(,go1.27.1)\r\n", false},
	{"go-redis", "*4\r\n$6\r\nclient\r\n$7\r\nsetinfo\r\n$7\r\nLIB-VER\r\n$5\r\n9.7.1\r\n", false},
	{"go-redis", "*2\r\n$4\r\nauth\r\n$2\r\npw\r\n", false},
	{"go-redis", "*2\r\n$6\r\nselect\r\n$1\r\n2\r\n", false},
	{"go-redis", "*1\r\n$4\r\nping\r\n", false},
	{"redis-py", "*4\r\n$6\r\nCLIENT\r\n$7\r\nSETINFO\r\n$8\r\nLIB-NAME\r\n$8\r\nredis-py\r\n", true},
	{"redis-py", "*2\r\n$4\r\nAUTH\r\n$2\r\npw\r\n", true},
	{"redis-py", "*2\r\n$6\r\nSELECT\r\n$1\r\n2\r\n", false},
	{"redis-py", "*1\r\n$4\r\nPING\r\n", false},
	{"ioredis", "*1\r\n$4\r\ninfo\r\n", true},
	{"ioredis", "*2\r\n$4\r\nauth\r\n$2\r\npw\r\n", true},
	{"ioredis", "*2\r\n$6\r\nselect\r\n$1\r\n2\r\n", false},
	{"ioredis", "*1\r\n$4\r\nping\r\n", false},
	{"jedis", "*1\r\n$4\r\nPING\r\n", true},
	{"jedis", "*2\r\n$4\r\nAUTH\r\n$2\r\npw\r\n", true},
	{"jedis", "*4\r\n$6\r\nCLIENT\r\n$7\r\nSETINFO\r\n$8\r\nLIB-NAME\r\n$5\r\njedis\r\n", false},
	{"jedis", "*2\r\n$6\r\nSELECT\r\n$1\r\n2\r\n", false},
	{"lettuce", "*2\r\n$5\r\nHELLO\r\n$1\r\n3\r\n", true},
	{"lettuce", "*5\r\n$5\r\nHELLO\r\n$1\r\n3\r\n$4\r\nAUTH\r\n$7\r\ndefault\r\n$2\r\npw\r\n", true},
	{"lettuce", "*4\r\n$6\r\nCLIENT\r\n$7\r\nSETINFO\r\n$8\r\nlib-name\r\n$7\r\nLettuce\r\n", false},
	{"lettuce", "*2\r\n$4\r\nAUTH\r\n$2\r\npw\r\n", false},
	{"lettuce", "*2\r\n$6\r\nSELECT\r\n$1\r\n2\r\n", false},
	{"lettuce", "*1\r\n$4\r\nPING\r\n", false},
}

// Every first frame a real Redis client sends is matched, and so is every
// later frame it sends, alone or with what follows it in the same write (a
// pipelined handshake: go-redis writes both CLIENT SETINFOs at once).
func TestMatchesRealClientFrames(t *testing.T) {
	for _, f := range clientFrames {
		if !Match([]byte(f.frame)) {
			t.Errorf("%s: %q not matched", f.client, f.frame)
		}
		if !Match([]byte(f.frame + "*1\r\n$4\r\nPING\r\n")) {
			t.Errorf("%s: %q followed by another command not matched", f.client, f.frame)
		}
	}
	if !(Skip{}).MatchType(context.Background(), []byte(clientFrames[0].frame)) {
		t.Fatal("Skip.MatchType does not match what Match matches")
	}
}

// mysqlHandshakeResponse is a MySQL client's first packet (it answers the
// server's greeting): HandshakeResponse41 for root, sequence 1.
func mysqlHandshakeResponse() []byte {
	body := []byte{0x8d, 0xa6, 0xff, 0x19, 0, 0, 0, 1, 0x21}
	body = append(body, make([]byte, 23)...)
	body = append(body, "root\x00"...)
	body = append(body, 0x14)
	body = append(body, make([]byte, 20)...)
	body = append(body, "mysql_native_password\x00"...)
	return append([]byte{byte(len(body)), byte(len(body) >> 8), byte(len(body) >> 16), 1}, body...)
}

// postgresStartup is a Postgres client's StartupMessage, protocol 3.0.
func postgresStartup() []byte {
	params := "user\x00postgres\x00database\x00shop\x00\x00"
	b := make([]byte, 8, 8+len(params))
	binary.BigEndian.PutUint32(b, uint32(8+len(params)))
	binary.BigEndian.PutUint32(b[4:], 196608)
	return append(b, params...)
}

// mongoOpMsg is a Mongo client's first message: OP_MSG {hello: 1, $db: "admin"}.
func mongoOpMsg() []byte {
	doc := []byte{0, 0, 0, 0, 0x10}
	doc = append(doc, "hello\x00"...)
	doc = append(doc, 1, 0, 0, 0, 0x02)
	doc = append(doc, "$db\x00"...)
	doc = append(doc, 6, 0, 0, 0)
	doc = append(doc, "admin\x00"...)
	doc = append(doc, 0)
	binary.LittleEndian.PutUint32(doc, uint32(len(doc)))
	msg := make([]byte, 16, 16+5+len(doc))
	binary.LittleEndian.PutUint32(msg[4:], 1)
	binary.LittleEndian.PutUint32(msg[12:], 2013)
	msg = append(msg, 0, 0, 0, 0, 0)
	msg = append(msg, doc...)
	binary.LittleEndian.PutUint32(msg, uint32(len(msg)))
	return msg
}

// kafkaAPIVersions is a Kafka client's first request: ApiVersions v3.
func kafkaAPIVersions() []byte {
	body := []byte{0, 18, 0, 3, 0, 0, 0, 1, 0, 7}
	body = append(body, "sarama1"...)
	body = append(body, 0, 7)
	body = append(body, "sarama\x00"...)
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(body)))
	return append(size, body...)
}

// tlsClientHello is the start of a TLS 1.2/1.3 ClientHello record.
func tlsClientHello() []byte {
	return append([]byte{0x16, 0x03, 0x01, 0x00, 0xf4, 0x01, 0x00, 0x00, 0xf0, 0x03, 0x03}, make([]byte, 32)...)
}

// Nothing another protocol's client sends first is matched, nor anything
// that starts with a RESP array's '*' without being, byte for byte, the
// start of a Redis command. A miss is what Match must answer whenever it is
// unsure: a connection it claims is relayed and nothing of it captured.
func TestMatchesNothingElse(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"mysql handshake response", mysqlHandshakeResponse()},
		{"mysql COM_QUERY", append([]byte{9, 0, 0, 0, 3}, "SELECT 1"...)},
		{"postgres startup", postgresStartup()},
		{"postgres SSLRequest", []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}},
		{"mongo OP_MSG", mongoOpMsg()},
		{"http/1.1", []byte("GET /kv/a HTTP/1.1\r\nHost: localhost\r\n\r\n")},
		{"http/1.1 to a path named like a command", []byte("GET /PING HTTP/1.1\r\n\r\n")},
		{"http/2 preface", append([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), 0, 0, 0, 4, 0, 0, 0, 0, 0)},
		{"tls client hello", tlsClientHello()},
		{"kafka ApiVersions", kafkaAPIVersions()},
		{"memcached get", []byte("get k\r\n")},
		{"memcached set", []byte("set k 0 0 5\r\nhello\r\n")},
		{"redis inline command (no client library sends one)", []byte("PING\r\n")},
		{"a RESP reply, not a command", []byte("+OK\r\n")},
		{"a RESP bulk string, not an array", []byte("$4\r\nPING\r\n")},
		{"binary after '*'", []byte{'*', 0x00, 0x01, 0x02, 0xff, '\r', '\n'}},
		{"'*' and digits only", []byte("*2")},
		{"no command name yet", []byte("*1\r\n")},
		{"name cut short", []byte("*1\r\n$4\r\nPI")},
		{"name without its CRLF", []byte("*1\r\n$4\r\nPING")},
		{"name with half its CRLF", []byte("*1\r\n$4\r\nPING\r")},
		{"length longer than the name", []byte("*1\r\n$4\r\nGET\r\n")},
		{"length shorter than the name", []byte("*1\r\n$3\r\nPING\r\n")},
		{"unknown command", []byte("*1\r\n$4\r\nPONG\r\n")},
		{"unknown command, command-like", []byte("*2\r\n$6\r\nGETSUM\r\n$1\r\nk\r\n")},
		{"command with punctuation", []byte("*1\r\n$5\r\nPING!\r\n")},
		{"empty array", []byte("*0\r\n")},
		{"null array", []byte("*-1\r\n")},
		{"signed count", []byte("*+1\r\n$4\r\nPING\r\n")},
		{"leading zero in the count", []byte("*01\r\n$4\r\nPING\r\n")},
		{"leading zero in the length", []byte("*1\r\n$04\r\nPING\r\n")},
		{"zero length", []byte("*1\r\n$0\r\n\r\n")},
		{"count past the bound", []byte("*9999999\r\n$4\r\nPING\r\n")},
		{"count of eight digits", []byte("*10000000\r\n$4\r\nPING\r\n")},
		{"length past any command", []byte("*1\r\n$33\r\nAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\n")},
		{"bare LF", []byte("*1\n$4\nPING\n")},
		{"integer element", []byte("*1\r\n:4\r\nPING\r\n")},
		{"space in the header", []byte("* 1\r\n$4\r\nPING\r\n")},
	}
	for _, c := range cases {
		if Match(c.buf) {
			t.Errorf("%s: %q matched", c.name, c.buf)
		}
	}
}

// Every name in the table is matched as a command, in any case, and the
// table holds each once: the table is the strict half of the match.
func TestEveryTableCommandMatches(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commandNames {
		if seen[c] {
			t.Errorf("%s is in the table twice", c)
		}
		seen[c] = true
		if c != strings.ToUpper(c) {
			t.Errorf("%s is not upper case", c)
		}
		for _, name := range []string{c, strings.ToLower(c)} {
			frame := fmt.Sprintf("*1\r\n$%d\r\n%s\r\n", len(name), name)
			if !Match([]byte(frame)) {
				t.Errorf("%q not matched", frame)
			}
		}
	}
	if len(commands) != len(commandNames) {
		t.Fatalf("the table has %d commands, the list %d", len(commands), len(commandNames))
	}
}

// The skip records nothing and serves nothing itself: a dispatcher that
// hands it a recording is told it declined (and relays the connection), and
// one that asks it for a mock is told there is none.
func TestSkipRecordsAndMocksNothing(t *testing.T) {
	s := New(zap.NewNop())
	if err := s.RecordOutgoing(context.Background(), nil); !errors.Is(err, integrations.ErrParserDeclined) {
		t.Fatalf("RecordOutgoing returned %v, want ErrParserDeclined", err)
	}
	if err := s.MockOutgoing(context.Background(), nil, nil, nil, models.OutgoingOptions{}); err == nil {
		t.Fatal("MockOutgoing served a Redis connection")
	}
}

// The notice is said once per destination, at Warn (keploy's default level
// shows it), carrying the destination and the URL, however many connections
// to that destination ask, at once or one after another.
func TestNoticeOncePerDestination(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	var n Notices
	var wg sync.WaitGroup
	var told sync.Map
	for i := 0; i < 32; i++ {
		for _, dst := range []string{"127.0.0.1:6379", "10.0.0.7:6380"} {
			wg.Add(1)
			go func(i int, dst string) {
				defer wg.Done()
				if n.Log(logger, dst) {
					told.Store(fmt.Sprint(dst, i), true)
				}
			}(i, dst)
		}
	}
	wg.Wait()
	for i := 0; i < 8; i++ {
		n.Log(logger, "127.0.0.1:6379")
	}
	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("the notice was said %d times for 2 destinations over 72 connections, want twice: %v", len(entries), entries)
	}
	got := map[string]bool{}
	for _, e := range entries {
		if e.Level != zapcore.WarnLevel {
			t.Errorf("the notice was said at %v, want warn", e.Level)
		}
		if e.Message != integrations.RedisSkipNotice || !strings.Contains(e.Message, integrations.RedisEnterpriseURL) {
			t.Errorf("the notice said %q", e.Message)
		}
		got[e.ContextMap()["destination"].(string)] = true
	}
	if !got["127.0.0.1:6379"] || !got["10.0.0.7:6380"] {
		t.Fatalf("the notices named %v, want both destinations", got)
	}
}

// The notice's wording and URL are a contract: Enterprise's and the docs'
// builders quote them.
func TestNoticeText(t *testing.T) {
	const want = "Redis calls are skipped: Keploy records and mocks Redis (and Redis-compatible servers) only in Keploy Enterprise, so calls to this Redis pass through to the real server and are neither recorded nor mocked. To record and mock Redis, use Keploy Enterprise: https://keploy.io/docs/keploy-cloud/cloud-installation/"
	if integrations.RedisSkipNotice != want {
		t.Fatalf("the notice reads %q, want %q", integrations.RedisSkipNotice, want)
	}
	if integrations.REDIS != "redis" || integrations.REDIS_SKIP != "redis_enterprise_only" {
		t.Fatalf("the integration names are %q and %q", integrations.REDIS, integrations.REDIS_SKIP)
	}
}

// A Generic mock is of Redis traffic when its first request opens with a
// Redis command, as the Generic parser stored it: the bytes when they are
// ASCII, their base64 otherwise, whatever type the payload names (the
// parser matches by data alone). Nothing else is: another protocol's
// request, a RESP array naming no command, a mock of another kind, a mock
// whose first payload is the server's, a mock with nothing in it.
func TestIsRecordedRedis(t *testing.T) {
	generic := func(origin models.OriginType, typ, data string) *models.Mock {
		return &models.Mock{Kind: models.GENERIC, Spec: models.MockSpec{
			GenericRequests: []models.Payload{{Origin: origin, Message: []models.OutputBinary{{Type: typ, Data: data}}}},
		}}
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	binarySet := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$2\r\n\xff\xfe\r\n"
	// Only a frame's head is read, whatever the frame's length.
	bigSet := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1048576\r\n" + strings.Repeat("v", 1<<20) + "\r\n"
	yes := map[string]*models.Mock{
		"go-redis HELLO, ASCII":   generic(models.FromClient, models.String, clientFrames[0].frame),
		"a SET of binary, base64": generic(models.FromClient, "binary", b64(binarySet)),
		"a pipeline, ASCII":       generic(models.FromClient, models.String, "*1\r\n$4\r\nPING\r\n*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"),
		"ASCII typed binary":      generic(models.FromClient, "binary", "*1\r\n$4\r\nPING\r\n"),
		"base64 typed string":     generic(models.FromClient, models.String, b64(binarySet)),
		"a SET of 1 MiB, base64":  generic(models.FromClient, "binary", b64(bigSet)),
		"a SET of 1 MiB, ASCII":   generic(models.FromClient, models.String, bigSet),
	}
	for name, m := range yes {
		if !IsRecordedRedis(m) {
			t.Errorf("%s: not taken for a mock of Redis traffic", name)
		}
	}
	no := map[string]*models.Mock{
		"nil":                    nil,
		"memcached":              generic(models.FromClient, models.String, "get k\r\n"),
		"no command":             generic(models.FromClient, models.String, "*1\r\n$4\r\nPONG\r\n"),
		"mysql, base64":          generic(models.FromClient, "binary", base64.StdEncoding.EncodeToString(mysqlHandshakeResponse())),
		"base64 of no command":   generic(models.FromClient, "binary", b64("*1\r\n$4\r\nPONG\r\n")),
		"the server's payload":   generic(models.FromServer, models.String, clientFrames[0].frame),
		"no message":             {Kind: models.GENERIC, Spec: models.MockSpec{GenericRequests: []models.Payload{{Origin: models.FromClient}}}},
		"no request":             {Kind: models.GENERIC},
		"an HTTP mock":           {Kind: models.HTTP},
		"a Redis kind, not ours": {Kind: models.REDIS, Spec: models.MockSpec{GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: clientFrames[0].frame}}}}}},
	}
	for name, m := range no {
		if IsRecordedRedis(m) {
			t.Errorf("%s: taken for a Generic mock of Redis traffic", name)
		}
	}
}

// genericOf is a Generic mock of connection conn (none when ""), recorded at
// at, whose first request is data.
func genericOf(name, conn string, at time.Time, data string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.GENERIC, Spec: models.MockSpec{
		ReqTimestampMock: at,
		GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: data}}}},
	}}
	if conn != "" {
		m.Spec.Metadata = map[string]string{"connID": conn}
	}
	return m
}

// A build with no Redis parser leaves a set's Redis traffic out of a replay,
// judging a Generic mock by its connection's first exchange, as the proxy
// judges a connection by its first frame: every exchange of a connection
// that opened with a Redis command (its first exchange however the set is
// ordered), and none of one that did not (its later Redis commands included:
// the Generic parser serves that connection whole). A Generic mock naming no
// connection is judged alone, and Keploy Enterprise's Redis mocks are left
// out. A build that links a Redis parser leaves out nothing.
func TestRecordedLeavesOutRedisConnectionsWhole(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	set := []*models.Mock{
		// Connection 1, go-redis: HELLO first; its GET and SET are its.
		genericOf("mock-3", "1", at(3), "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"),
		genericOf("mock-1", "1", at(1), clientFrames[0].frame),
		genericOf("mock-2", "1", at(2), "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"),
		// Connection 2 opened with a module command outside the table: the
		// skip does not take it, and Generic serves every exchange of it.
		genericOf("mock-4", "2", at(4), "*2\r\n$11\r\nGRAPH.QUERY\r\n$1\r\ng\r\n"),
		genericOf("mock-5", "2", at(5), "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"),
		// Connection 3, memcached.
		genericOf("mock-6", "3", at(6), "get k\r\n"),
		// Connection 4: two exchanges at one time, mock-10 recorded after
		// mock-9 (the order of names breaks the tie, numerically).
		genericOf("mock-10", "4", at(7), "*1\r\n$4\r\nPING\r\n"),
		genericOf("mock-9", "4", at(7), "get k\r\n"),
		// No connection named: judged alone.
		genericOf("mock-11", "", at(8), "*1\r\n$4\r\nPING\r\n"),
		genericOf("mock-12", "", at(9), "get k\r\n"),
		{Name: "mock-13", Kind: models.REDIS},
		{Name: "mock-14", Kind: models.HTTP},
		nil,
	}
	if _, linked := integrations.Registered[integrations.REDIS]; linked {
		t.Fatal("this build registers a Redis parser; the test assumes it does not")
	}
	got := LeftOutOf(set[:6], set[6:])
	want := []string{"mock-1", "mock-11", "mock-13", "mock-2", "mock-3"}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("left out %v, want %v", names, want)
	}
	if LeftOutOf([]*models.Mock{genericOf("mock-1", "9", at(1), "get k\r\n")}) != nil {
		t.Fatal("a set with no Redis traffic left something out")
	}

	integrations.Register(integrations.REDIS, &integrations.Parsers{
		Initializer: func(*zap.Logger) integrations.Integrations { return Skip{} },
		Priority:    200,
	})
	t.Cleanup(func() { delete(integrations.Registered, integrations.REDIS) })
	if got := LeftOutOf(set); got != nil {
		t.Fatalf("a build with a Redis parser left out %v", got)
	}
}
