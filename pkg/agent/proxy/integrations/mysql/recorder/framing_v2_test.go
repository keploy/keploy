package recorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The traffic below is what a JDBC app on a pooled connection sends: per
// request SET autocommit=0, six 20-row SELECTs of one bucket each, commit,
// SET autocommit=1, and once an 8,209-row result of ~540-byte rows (4.4 MB).
// With the capture short of some bytes, the recorder paired every later
// SELECT of a connection with the previous one's rows, or read a packet header
// out of the middle of a row and waited for 3 MB that never came.

// sessionColumns are the columns the app selects; the 8,209-row query returns
// CONCAT(payload x4) as "p" in the payload's place.
func sessionColumns(t *testing.T, payloadName string) [][]byte {
	t.Helper()
	col := func(name string, typ mysql.FieldType, charset uint16, length uint32) []byte {
		return columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Schema: "test", Table: "sessions", OrgTable: "sessions",
			Name: name, OrgName: name, FixedLength: 0x0c, CharacterSet: charset, ColumnLength: length, Type: byte(typ)})
	}
	return [][]byte{
		col("id", mysql.FieldTypeLongLong, 0x3f, 20),
		col("name", mysql.FieldTypeVarString, 0x2e, 256),
		col("email", mysql.FieldTypeVarString, 0x2e, 512),
		col("status", mysql.FieldTypeVarString, 0x2e, 64),
		col(payloadName, mysql.FieldTypeVarString, 0x2e, 2048),
		col("updated_at", mysql.FieldTypeLongLong, 0x3f, 20),
	}
}

// lenenc is a length-encoded string.
func lenenc(s string) []byte {
	switch n := len(s); {
	case n < 251:
		return append([]byte{byte(n)}, s...)
	case n < 1<<16:
		return append([]byte{0xfc, byte(n), byte(n >> 8)}, s...)
	case n < 1<<24:
		return append([]byte{0xfd, byte(n), byte(n >> 8), byte(n >> 16)}, s...)
	default:
		b := []byte{0xfe, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.LittleEndian.PutUint64(b[1:], uint64(n))
		return append(b, s...)
	}
}

// sessionRow is the row of session id, its 120-digit payload repeated reps
// times.
func sessionRow(id, reps int) []byte {
	var p []byte
	for _, v := range []string{
		strconv.Itoa(id), "user-" + strconv.Itoa(id), "user" + strconv.Itoa(id) + "@example.com", "ACTIVE",
		strings.Repeat(fmt.Sprintf("%0120d", id), reps), strconv.Itoa(1700000000000 + id),
	} {
		p = append(p, lenenc(v)...)
	}
	return p
}

func bucketQuery(b int) string {
	return fmt.Sprintf("SELECT id, name, email, status, payload, updated_at FROM sessions WHERE bucket = %d ORDER BY id LIMIT 20", b)
}

const bigQuery = "SELECT id, name, email, status, CONCAT(payload,payload,payload,payload) AS p, updated_at FROM sessions ORDER BY id LIMIT 8209"

// exchange is one command and the packets the server answered it with.
type exchange struct {
	command []byte
	reply   [][]byte
}

// kitTraffic is the command phase of one pooled connection serving requests
// for ids: the exchanges in order. withBig puts the 8,209-row result at the
// start of the request for the first id.
func kitTraffic(t *testing.T, ids []int, withBig bool) []exchange {
	t.Helper()
	ok := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})
	var out []exchange
	q := func(query string, reply [][]byte) {
		out = append(out, exchange{command: cannedCOMQuery(t, 0, query), reply: reply})
	}
	for i, id := range ids {
		q("SET autocommit=0", ok)
		if withBig && i == 0 {
			var rows [][]byte
			for r := 0; r < 8209; r++ {
				rows = append(rows, sessionRow(r, 4))
			}
			q(bigQuery, resultSet(false, sessionColumns(t, "p"), rows))
		}
		for k := 0; k < 6; k++ {
			b := (id*7 + k*131) % 1000
			var rows [][]byte
			for r := 0; r < 20; r++ {
				rows = append(rows, sessionRow(b+1000*r, 1))
			}
			q(bucketQuery(b), resultSet(false, sessionColumns(t, "payload"), rows))
		}
		q("commit", ok)
		q("SET autocommit=1", ok)
	}
	return out
}

// streams lays out the exchanges as the two byte streams of the connection.
// Each server packet's offset is kept, so a test can cut the stream exactly
// where it means to.
type streams struct {
	client, server []byte
	// packetAt[i][j] is where packet j of exchange i's reply starts in server.
	packetAt [][]int
}

func layOut(xs []exchange) streams {
	var s streams
	for _, x := range xs {
		s.client = append(s.client, x.command...)
		at := make([]int, len(x.reply))
		for j, p := range x.reply {
			at[j] = len(s.server)
			s.server = append(s.server, p...)
		}
		s.packetAt = append(s.packetAt, at)
	}
	return s
}

// cut is s without the bytes [from, to): what a capture that lost them hands
// the recorder.
func cut(s []byte, from, to int) []byte {
	return append(append([]byte(nil), s[:from]...), s[to:]...)
}

// chunked splits s into the reads a reader of size n makes.
func chunked(s []byte, n int) [][]byte {
	var out [][]byte
	for len(s) > 0 {
		k := min(n, len(s))
		out = append(out, s[:k])
		s = s[k:]
	}
	return out
}

// recording is what RecordV2 did with a connection.
type recording struct {
	mocks []*models.Mock
	err   error
	// returned is false when RecordV2 was still running at the deadline:
	// waiting for bytes, after every byte the connection carried was handed
	// to it.
	returned bool
	logs     *observer.ObservedLogs
	// leftOut are the exchanges RecordV2 framed but did not record, as the
	// windows it left out (Session.RecordOrphanWindow).
	leftOut *orphanSpans
	// cleared counts the times RecordV2 told the supervisor its input so far
	// was consumed (Session.OnPendingCleared), and clearedAt how many client
	// bytes it had consumed at each; clientConsumed is how many it consumed in
	// all.
	cleared        int64
	clearedAt      []int64
	clientConsumed int64
	// closedAt is when the streams of a connection left open were closed, at
	// the deadline (zero when RecordV2 returned before it).
	closedAt time.Time
}

// orphanSpans collects the windows a Session leaves out (supervisor.OrphanSpans),
// and when each was left out.
type orphanSpans struct {
	mu      sync.Mutex
	windows [][2]time.Time
	at      []time.Time
}

func (o *orphanSpans) Record(start, end time.Time) {
	o.mu.Lock()
	o.windows = append(o.windows, [2]time.Time{start, end})
	o.at = append(o.at, time.Now())
	o.mu.Unlock()
}

// recordedAt is when window i was left out.
func (o *orphanSpans) recordedAt(i int) time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.at[i]
}

func (o *orphanSpans) Open(start time.Time) func() {
	return func() { o.Record(start, time.Now()) }
}

func (o *orphanSpans) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.windows)
}

// last is the window left out last, if any.
func (o *orphanSpans) last() ([2]time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.windows) == 0 {
		return [2]time.Time{}, false
	}
	return o.windows[len(o.windows)-1], true
}

// recordConn runs RecordV2 over a connection: its handshake, then client and
// server (each delivered in the given chunks, the way a capture hands them
// over, stamped as they are pushed). closeAtEnd closes both streams after the
// last chunk, as a connection that ended does; without it they stay open, as a
// pooled connection's do.
func recordConn(t *testing.T, client, server [][]byte, closeAtEnd bool, deadline time.Duration) recording {
	t.Helper()
	chunks := func(bufs [][]byte, dir fakeconn.Direction) []fakeconn.Chunk {
		out := make([]fakeconn.Chunk, len(bufs))
		for i, b := range bufs {
			out[i] = fakeconn.Chunk{Dir: dir, Bytes: b}
		}
		return out
	}
	return recordChunks(t, chunks(client, fakeconn.FromClient), chunks(server, fakeconn.FromDest), closeAtEnd, deadline)
}

// wireBase is when the chunks onTheWire lays out are captured from: after the
// handshake recordChunks stamps.
var wireBase = time.Date(2026, 10, 2, 10, 47, 0, 0, time.UTC)

// onTheWire lays the exchanges out as a capture of a client that waits for
// each answer before its next command stamps them: each command in a chunk of
// its own, sent before its answer, then the answer in reads of n bytes, every
// one of them before the next command (the server sends nothing more until it
// arrives). An exchange with no command (nil) is more of the answer before it;
// one with no reply, a command the server does not answer.
func onTheWire(xs []exchange, n int) (client, server []fakeconn.Chunk) {
	tick := 0
	at := func() time.Time {
		tick++
		return wireBase.Add(time.Duration(tick) * time.Microsecond)
	}
	for _, x := range xs {
		if len(x.command) > 0 {
			client = append(client, fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: x.command, ReadAt: at()})
		}
		for _, c := range chunked(bytes.Join(x.reply, nil), n) {
			server = append(server, fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: c, ReadAt: at()})
		}
	}
	return client, server
}

// recordWire is recordConn over the exchanges as onTheWire lays them out.
func recordWire(t *testing.T, xs []exchange, n int, closeAtEnd bool, deadline time.Duration) recording {
	t.Helper()
	client, server := onTheWire(xs, n)
	return recordChunks(t, client, server, closeAtEnd, deadline)
}

// recordChunks is recordConn over chunks: one with no capture time is stamped
// as it is pushed, client and server alike.
func recordChunks(t *testing.T, client, server []fakeconn.Chunk, closeAtEnd bool, deadline time.Duration) recording {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	h := newV2Harness(t)
	h.logger = logger
	h.sess.Logger = logger
	mocks := make(chan *models.Mock, 1<<16)
	h.sess.Mocks = mocks
	leftOut := &orphanSpans{}
	h.sess.Orphans = leftOut
	var cleared atomic.Int64
	var clearedMu sync.Mutex
	var clearedAt []int64
	h.sess.OnPendingCleared = func() {
		cleared.Add(1)
		clearedMu.Lock()
		clearedAt = append(clearedAt, h.sess.ClientStream.Consumed())
		clearedMu.Unlock()
	}
	base := time.Date(2026, 10, 2, 10, 46, 0, 0, time.UTC)
	var tick int64
	var tickMu sync.Mutex
	at := func() time.Time {
		tickMu.Lock()
		defer tickMu.Unlock()
		tick++
		return base.Add(time.Duration(tick) * time.Microsecond)
	}

	greetingBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RecordV2(ctx, logger, h.sess) }()

	var pushers sync.WaitGroup
	pushers.Add(2)
	go func() {
		defer pushers.Done()
		h.clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: cannedHandshakeResponse41(t, 1, false), ReadAt: at()}
		for _, c := range client {
			if c.ReadAt.IsZero() {
				c.ReadAt = at()
			}
			select {
			case h.clientCh <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer pushers.Done()
		h.destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: greetingBuf, ReadAt: at()}
		h.destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: cannedOK(t, 2, greeting.CapabilityFlags), ReadAt: at()}
		for _, c := range server {
			if c.ReadAt.IsZero() {
				c.ReadAt = at()
			}
			select {
			case h.destCh <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	pushed := make(chan struct{})
	go func() {
		pushers.Wait()
		if closeAtEnd {
			h.closeStreams()
		}
		close(pushed)
	}()

	rec := recording{logs: logs, leftOut: leftOut}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case rec.err = <-done:
		rec.returned = true
	case <-timer.C:
	}
	cancel()
	if !rec.returned {
		// FakeConn reads do not watch ctx: closing the streams is what lets a
		// recorder waiting in one return.
		rec.closedAt = time.Now()
		_ = h.sess.ClientStream.Close()
		_ = h.sess.DestStream.Close()
		<-done
	}
	<-pushed
	rec.cleared = cleared.Load()
	clearedMu.Lock()
	rec.clearedAt = clearedAt
	clearedMu.Unlock()
	rec.clientConsumed = h.sess.ClientStream.Consumed()
	for {
		select {
		case m := <-mocks:
			rec.mocks = append(rec.mocks, m)
		default:
			return rec
		}
	}
}

// wrongMocks names every mock whose response is not the one its command was
// answered with: a SELECT of a bucket must hold that bucket's 20 rows, the
// 8,209-row query all its rows, and every other command an OK.
func wrongMocks(t *testing.T, mocks []*models.Mock) []string {
	t.Helper()
	var wrong []string
	for _, m := range mocks {
		if m.Spec.Metadata["type"] == "config" || len(m.Spec.MySQLRequests) == 0 {
			continue
		}
		qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
		if !ok {
			wrong = append(wrong, fmt.Sprintf("%s: request %T is not a query", m.Spec.Metadata["requestOperation"], m.Spec.MySQLRequests[0].Message))
			continue
		}
		var rs *mysql.TextResultSet
		if len(m.Spec.MySQLResponses) == 1 {
			rs, _ = m.Spec.MySQLResponses[0].Message.(*mysql.TextResultSet)
		}
		firstIDs := func() []int {
			var ids []int
			for _, r := range rs.Rows {
				id, err := strconv.Atoi(fmt.Sprint(r.Values[0].Value))
				if err != nil {
					id = -1
				}
				ids = append(ids, id)
			}
			return ids
		}
		var b int
		switch {
		case qp.Query == bigQuery:
			if rs == nil || len(rs.Rows) != 8209 {
				wrong = append(wrong, fmt.Sprintf("the 8,209-row query recorded %s", describe(m)))
				continue
			}
			for i, id := range firstIDs() {
				if id != i {
					wrong = append(wrong, fmt.Sprintf("the 8,209-row query's row %d is id %d", i, id))
					break
				}
			}
		case func() bool {
			_, err := fmt.Sscanf(qp.Query, "SELECT id, name, email, status, payload, updated_at FROM sessions WHERE bucket = %d", &b)
			return err == nil
		}():
			if rs == nil || len(rs.Rows) != 20 {
				wrong = append(wrong, fmt.Sprintf("bucket %d recorded %s", b, describe(m)))
				continue
			}
			for _, id := range firstIDs() {
				if id%1000 != b {
					wrong = append(wrong, fmt.Sprintf("bucket %d holds row id %d", b, id))
					break
				}
			}
		case qp.Query == "SET autocommit=0" || qp.Query == "SET autocommit=1" || qp.Query == "commit":
			if m.Spec.Metadata["responseOperation"] != "OK" {
				wrong = append(wrong, fmt.Sprintf("%q recorded %s", qp.Query, describe(m)))
			}
		default:
			wrong = append(wrong, fmt.Sprintf("a query the app never sent: %q", qp.Query))
		}
	}
	return wrong
}

func describe(m *models.Mock) string {
	if len(m.Spec.MySQLResponses) != 1 {
		return fmt.Sprintf("%d responses", len(m.Spec.MySQLResponses))
	}
	if rs, ok := m.Spec.MySQLResponses[0].Message.(*mysql.TextResultSet); ok {
		return fmt.Sprintf("a result of %d rows", len(rs.Rows))
	}
	return "a " + m.Spec.Metadata["responseOperation"]
}

// queryMocks counts the mocks recorded for queries (the config mock aside).
func queryMocks(mocks []*models.Mock) int {
	n := 0
	for _, m := range mocks {
		if m.Spec.Metadata["type"] != "config" {
			n++
		}
	}
	return n
}

// The recorder logs lost framing where it finds it (warnFramingLost), at WARN
// and rate-limited across connections, and the error says so to whoever runs
// the parser (supervisor.ErrReported), so that no WARN of the parser's
// retirement is logged again for every connection one fault stops.
func TestErrFramingLostSaysItWasReported(t *testing.T) {
	for _, err := range []error{ErrFramingLost, framingLost("a packet out of sequence"), fmt.Errorf("handshake: %w", framingLost("x"))} {
		if !errors.Is(err, supervisor.ErrReported) {
			t.Fatalf("%v does not say it was reported", err)
		}
		if !errors.Is(err, ErrFramingLost) {
			t.Fatalf("%v is not ErrFramingLost", err)
		}
	}
	if errors.Is(errNotRecorded, supervisor.ErrReported) {
		t.Fatal("errNotRecorded never ends a recording, and says nothing to its supervisor")
	}
}

// requireFramingLost asserts the recording stopped where the bytes went
// missing, said so, recorded nothing wrong before it, and left out the
// exchange it stopped in.
func requireFramingLost(t *testing.T, rec recording) {
	t.Helper()
	if w := wrongMocks(t, rec.mocks); len(w) > 0 {
		t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
	}
	if !rec.returned {
		t.Fatal("RecordV2 is still waiting for bytes after every byte the connection carried was handed to it: a connection that cannot be recorded any further must stop, so what it carries from here is counted as lost")
	}
	if !errors.Is(rec.err, ErrFramingLost) {
		t.Fatalf("RecordV2 = %v, want ErrFramingLost: an end that is not an error passes for the connection closing, and nothing counts what it carried after", rec.err)
	}
	// The supervisor counts what the connection carries from where the parser
	// stopped; the exchange it stopped in began before that, so the recorder
	// leaves it out itself.
	w, ok := rec.leftOut.last()
	if !ok {
		t.Fatal("no exchange left out: the test cases of the exchange the recording stopped in would be saved without its mock")
	}
	if w[1].Before(w[0]) {
		t.Fatalf("the window left out ends (%v) before it starts (%v)", w[1], w[0])
	}
}

// requireRecordedOn asserts the recording left out exactly leftOut exchanges,
// recorded every other one right, and went on to the connection's end.
func requireRecordedOn(t *testing.T, rec recording, leftOut, queries int) {
	t.Helper()
	if w := wrongMocks(t, rec.mocks); len(w) > 0 {
		t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
	}
	if !rec.returned || rec.err != nil {
		t.Fatalf("RecordV2 returned=%v err=%v: an exchange it can frame must not stop the connection's recording", rec.returned, rec.err)
	}
	if got := rec.leftOut.count(); got != leftOut {
		t.Fatalf("%d exchanges left out, want %d: an exchange without a mock must be left out where its test cases are checked, or they are saved without it", got, leftOut)
	}
	if got := queryMocks(rec.mocks); got != queries {
		t.Fatalf("%d query mocks, want %d", got, queries)
	}
	// Each exchange's client bytes armed the supervisor's pending work, and
	// each exchange handled, recorded or left out, must clear it: work left
	// pending through the next idle spell is taken for a hung parser, and the
	// connection's recording is retired.
	if want := int64(len(rec.mocks) + leftOut); rec.cleared < want {
		t.Fatalf("pending work cleared %d times, want at least %d (one per mock and per exchange left out): a connection that then sits idle is retired as hung", rec.cleared, want)
	}
}

// Without a byte missing, chunking never matters: the 8,209-row result and
// every result around it are recorded whole and right however the capture cut
// the stream: in reads of 7 bytes and up with the large result, and of one byte
// and of the 4-byte headers without it.
func TestRecordV2_LargeResultFramesAcrossAnyChunking(t *testing.T) {
	t.Parallel()
	small := layOut(kitTraffic(t, []int{300, 301}, false))
	large := layOut(kitTraffic(t, []int{300, 301}, true))
	for _, c := range []struct {
		n       int
		s       streams
		queries int
	}{
		// Reads of one byte, and of the 4-byte headers Connector/J reads alone.
		{1, small, 2 * 9}, {4, small, 2 * 9}, {7, large, 2*9 + 1},
		// Connector/J's 16 KB read-ahead, and the capture's 4 KB and 64 KB tiers.
		{4096, large, 2*9 + 1}, {16384, large, 2*9 + 1}, {65536, large, 2*9 + 1},
	} {
		t.Run(fmt.Sprintf("reads of %d, %d queries", c.n, c.queries), func(t *testing.T) {
			t.Parallel()
			rec := recordConn(t, chunked(c.s.client, max(c.n, 64)), chunked(c.s.server, c.n), true, 2*time.Minute)
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 returned=%v err=%v on a whole connection", rec.returned, rec.err)
			}
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s); the first: %s", len(w), w[0])
			}
			if got := queryMocks(rec.mocks); got != c.queries {
				t.Fatalf("%d query mocks, want %d", got, c.queries)
			}
		})
	}
}

// A capture that lost client bytes from the middle of one command into the
// next: the next packet header is read out of query text.
func TestRecordV2_ClientBytesLostMidCommandStopTheConnection(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7}, false)
	s := layOut(xs)
	// From the start of SET autocommit=0 to inside the first SELECT's text:
	// a lost send and part of the next. (A lost send and nothing more leaves
	// the client stream framed, and no parser can see it: the capture has to
	// say so.)
	from := 0
	to := len(xs[0].command) + 4 + 20
	client := cut(s.client, from, to)
	rec := recordConn(t, chunked(client, 64), chunked(s.server, 16384), true, 5*time.Second)
	requireFramingLost(t, rec)
}

// COM_SET_OPTION (Connector/J sends it around a rewritten batch) has no
// decoder either, but its reply is one EOF or ERR: it is framed and left out,
// and the connection goes on being recorded. Before, its EOF was recorded as
// the answer to the next SELECT, and every SELECT after that was paired with
// the previous one's rows.
func TestRecordV2_ASinglePacketCommandWithoutADecoderIsLeftOut(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	setOption := exchange{command: wrapPacket([]byte{0x1b, 0x00, 0x00}, 0), reply: frame([][]byte{eofPayload})}
	s := layOut(append(append(append([]exchange{}, xs[:3]...), setOption), xs[3:]...))
	rec := recordConn(t, chunked(s.client, 64), chunked(s.server, 16384), true, time.Minute)
	requireRecordedOn(t, rec, 1, len(xs))
}

// A command that does not decode is not skipped: its response is still on the
// server stream, and the next command would be paired with it. An execute of
// a statement prepared before the capture started does not decode, but its
// response describes itself: it is framed and left out, and the connection
// goes on being recorded.
func TestRecordV2_ACommandThatDoesNotDecodeIsLeftOut(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	unknownStmt := []byte{mysql.COM_STMT_EXECUTE, 9, 0, 0, 0, 0, 1, 0, 0, 0}
	for _, c := range []struct {
		name  string
		reply [][]byte
	}{
		{"answered with an ERR", frame([][]byte{append([]byte{0xff, 0x13, 0x04, '#', 'H', 'Y', '0', '0', '0'}, "Unknown prepared statement handler"...)})},
		{"answered with a result set", frame([][]byte{{0x01}, columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "id", OrgName: "id", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)}),
			eofPayload, {0x00, 0x00, 1, 0, 0, 0, 0, 0, 0, 0}, eofPayload})},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			execute := exchange{command: wrapPacket(unknownStmt, 0), reply: c.reply}
			s := layOut(append(append(append([]exchange{}, xs[:2]...), execute), xs[2:]...))
			rec := recordConn(t, chunked(s.client, 64), chunked(s.server, 16384), true, time.Minute)
			requireRecordedOn(t, rec, 1, len(xs))
		})
	}
}

// A CALL (or a multi-statement query) answers with several results, each but
// the last saying more follow (SERVER_MORE_RESULTS_EXISTS). A mock holds one,
// so the exchange is framed whole and left out, and the connection goes on
// being recorded. Before, the recorder stopped at the first result's end and
// recorded it as the whole answer; the rest was read as the next command's
// answer. With one column and 252 rows the first result ends on sequence id 0,
// so the final OK carries sequence id 1, exactly what the next command's answer
// would: no sequence check could see it.
func TestRecordV2_AResponseOfSeveralResultsIsLeftOut(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	more := []byte{0xfe, 0x00, 0x00, 0x0a, 0x00} // EOF: SERVER_STATUS_AUTOCOMMIT | SERVER_MORE_RESULTS_EXISTS
	finalOK := []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
	moreOK := []byte{0x00, 0x01, 0x00, 0x0a, 0x00, 0x00, 0x00}
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	result := func(rows int, terminator []byte) [][]byte {
		p := [][]byte{{0x01}, col, eofPayload}
		for i := 0; i < rows; i++ {
			p = append(p, textRowPayload(strconv.Itoa(i)))
		}
		return append(p, terminator)
	}
	for _, c := range []struct {
		name  string
		reply [][]byte
	}{
		{"a result, then the final OK", frame(append(result(3, more), finalOK))},
		{"a result ending on sequence id 0, then the final OK", frame(append(result(252, more), finalOK))},
		{"two results, then the final OK", frame(append(append(result(2, more), result(5, more)...), finalOK))},
		{"an OK that says more follow, a result, then an ERR", frame(append(append([][]byte{moreOK}, result(1, more)...), []byte{0xff, 0x13, 0x04, '#', '4', '2', '0', '0', '0', 'x'}))},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			call := exchange{command: cannedCOMQuery(t, 0, "CALL sessions_of(7)"), reply: c.reply}
			s := layOut(append(append(append([]exchange{}, xs[:1]...), call), xs[1:]...))
			rec := recordConn(t, chunked(s.client, 64), chunked(s.server, 16384), true, time.Minute)
			requireRecordedOn(t, rec, 1, len(xs))
		})
	}
}

// joinedMidStream is a decrypted TLS stream of a pooled connection the capture
// joined mid-stream: no handshake on it, so its framing is assumed
// (CLIENT_DEPRECATE_EOF, which the server here offers). run starts RecordV2 on
// it and returns what it recorded once it returns, or fails after a minute.
type joinedMidStream struct {
	h    *v2Harness
	caps uint32
	ctx  context.Context
	// tick is the capture clock of exchange, in microseconds past wireBase.
	tick int
	// greeting is the server's, as the raw leg captured it.
	greeting []byte
}

func newJoinedMidStream(t *testing.T) *joinedMidStream {
	t.Helper()
	h := newV2Harness(t)
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	hs := &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a, ServerVersion: "8.0.test-keploy", ConnectionID: 42,
		AuthPluginData: bytes.Repeat([]byte{0x11}, 20),
		CapabilityFlags: uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL |
			mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_DEPRECATE_EOF),
		CharacterSet: 0x21, StatusFlags: 0x02, AuthPluginName: string(mysql.Native),
	}
	gb, err := connphase.EncodeHandshakeV10(context.Background(), zap.NewNop(), hs)
	if err != nil {
		t.Fatal(err)
	}
	greeting := wrapPacket(gb, 0)
	sslReq := cannedSSLRequest(t, 1)
	h.sess.Opts.NetNS = testNetNS
	ctx := postTLSCtx(t, greeting, sslReq, base, 3306)
	store, _ := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	store.RememberLast(lastGreetingKey(h.sess.Opts.PassThroughScope, h.sess.Opts.NetNS, h.sess.Opts.DstCfg), models.TLSHandshakeEntry{
		RespPackets: [][]byte{greeting}, ReqPackets: [][]byte{sslReq}, ReqTimestamp: base,
	})
	return &joinedMidStream{h: h, caps: hs.CapabilityFlags, ctx: ctx, greeting: greeting}
}

// ownLeg makes the connection's own pre-TLS leg the one the store holds for
// it: the greeting and an SSLRequest with the client's capability flags,
// pushed under the connection's identity, as the raw leg does when the capture
// saw this connection's handshake but not its decrypted start (a TLS hook that
// attached after it).
func (j *joinedMidStream) ownLeg(t *testing.T, clientCaps uint32) {
	t.Helper()
	store, _ := j.ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	owner := models.HandshakeOwner{Conn: "pooled-conn-7", Proc: "app"}
	j.h.sess.Opts.ConnKey, j.h.sess.Opts.ConnProc = owner.Conn, owner.Proc
	body := make([]byte, 32)
	binary.LittleEndian.PutUint32(body[0:4], clientCaps)
	binary.LittleEndian.PutUint32(body[4:8], 1<<24)
	body[8] = 0x21
	store.PushFor(models.HandshakeStoreKey("", 3306), owner, models.TLSHandshakeEntry{
		RespPackets: [][]byte{j.greeting}, ReqPackets: [][]byte{wrapPacket(body, 1)}, ReqTimestamp: time.Now(),
	})
}

// at is the capture time of the next chunk pushed: every chunk of the stream
// is stamped on one clock, in the order it is pushed.
func (j *joinedMidStream) at() time.Time {
	j.tick++
	return wireBase.Add(time.Duration(j.tick) * time.Microsecond)
}

// exchange pushes a command and its reply in the order a capture stamps them:
// the command, then each packet of the reply after it.
func (j *joinedMidStream) exchange(command []byte, reply [][]byte) {
	j.h.pushClient(command, j.at())
	for _, p := range reply {
		j.h.pushDest(p, j.at())
	}
}

func (j *joinedMidStream) run(t *testing.T) ([]*models.Mock, error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- RecordV2(j.ctx, j.h.logger, j.h.sess) }()
	j.h.closeStreams()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Minute):
		t.Fatal("RecordV2 still running a minute after the streams closed")
	}
	var mocks []*models.Mock
	for {
		select {
		case m := <-j.h.mocks:
			if m.Spec.Metadata["type"] != "config" {
				mocks = append(mocks, m)
			}
		default:
			return mocks, err
		}
	}
}

// A connection joined mid-stream on its decrypted TLS stream has no captured
// handshake, so its framing is assumed: CLIENT_DEPRECATE_EOF, which every
// current driver negotiates. A client that never negotiated it is sent an EOF
// after the column definitions, which read as the rows' terminator, and the
// SELECT was recorded with no rows. Under CLIENT_DEPRECATE_EOF no 5-byte EOF follows the
// column definitions, so the first one that does settles the framing: EOFs,
// from there on.
func TestRecordV2_PostTLS_AnAssumedFramingIsSettledByTheFirstResult(t *testing.T) {
	t.Parallel()
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	for _, c := range []struct {
		name         string
		deprecateEOF bool
	}{{"a client that framed with EOFs", false}, {"a client that negotiated CLIENT_DEPRECATE_EOF", true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			for i := 0; i < 2; i++ {
				j.exchange(cannedCOMQuery(t, 0, "SELECT n FROM t"), resultSet(c.deprecateEOF, [][]byte{col}, [][]byte{textRowPayload("a"), textRowPayload("b")}))
			}
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
			mocks, err := j.run(t)
			if err != nil {
				t.Fatalf("RecordV2 = %v", err)
			}
			if len(mocks) != 3 {
				t.Fatalf("%d mocks, want the 3 exchanges", len(mocks))
			}
			for _, m := range mocks[:2] {
				if got := describe(m); got != "a result of 2 rows" {
					t.Fatalf("the SELECT recorded %s, want its 2 rows", got)
				}
			}
			if got := mocks[2].Spec.Metadata["responseOperation"]; got != "OK" {
				t.Fatalf("SELECT 1 recorded %s", got)
			}
		})
	}
}

// A prepared statement's response settles an assumed framing when it has both
// parameter and column definitions: what follows the parameters is an EOF, or
// a column definition. With only one run, the response cannot settle it: the
// packet after the run is its EOF, or the next command's answer. They differ
// in sequence id (the EOF continues the response's sequence, an answer starts
// its own), so the next packet the server sends settles it: the PREPARE is
// recorded with the EOF its client was sent, or without one, and the rest of
// the connection is framed that way.
func TestRecordV2_PostTLS_APrepareOnAnAssumedFraming(t *testing.T) {
	t.Parallel()
	def := func(name string) []byte {
		return columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: name, OrgName: name, FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	}
	defs := func(n int) [][]byte {
		var out [][]byte
		for i := 0; i < n; i++ {
			out = append(out, def("?"))
		}
		return out
	}
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	rows := [][]byte{textRowPayload("a"), textRowPayload("b")}
	prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t WHERE a = ? AND b = ?"...), 0)
	t.Run("parameters and columns", func(t *testing.T) {
		t.Parallel()
		j := newJoinedMidStream(t)
		j.exchange(prepare, prepareOK(false, [][]byte{def("?"), def("?")}, [][]byte{def("id")}))
		j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
		mocks, err := j.run(t)
		if err != nil {
			t.Fatalf("RecordV2 = %v", err)
		}
		if len(mocks) != 2 {
			t.Fatalf("%d mocks, want 2", len(mocks))
		}
		sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
		if !ok || len(sp.ParamDefs) != 2 || len(sp.ColumnDefs) != 1 || sp.EOFAfterParamDefs == nil || sp.EOFAfterColumnDefs == nil {
			t.Fatalf("the PREPARE recorded %+v; want 2 parameters and 1 column, each run with its EOF", mocks[0].Spec.MySQLResponses[0].Message)
		}
	})
	for _, c := range []struct {
		name           string
		params, cols   int
		deprecateEOF   bool
		answeredWithOK bool // the next command is answered with an OK, not a result
	}{
		{name: "parameters only, a client that framed with EOFs", params: 2},
		{name: "parameters only, a client that negotiated CLIENT_DEPRECATE_EOF", params: 2, deprecateEOF: true},
		{name: "columns only, a client that framed with EOFs", cols: 1},
		{name: "columns only, a client that negotiated CLIENT_DEPRECATE_EOF", cols: 1, deprecateEOF: true},
		// 254 definitions: the EOF has sequence id 0, the next answer 1.
		{name: "254 parameters, a client that framed with EOFs", params: 254, answeredWithOK: true},
		{name: "254 parameters, a client that negotiated CLIENT_DEPRECATE_EOF", params: 254, deprecateEOF: true, answeredWithOK: true},
		// 255 definitions: the EOF has sequence id 1, as the next answer does.
		// The next command is a query, whose answer never starts with an EOF,
		// so an EOF there is the PREPARE's.
		{name: "255 parameters, a client that framed with EOFs", params: 255, answeredWithOK: true},
		{name: "255 parameters, a client that negotiated CLIENT_DEPRECATE_EOF", params: 255, deprecateEOF: true, answeredWithOK: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			leftOut := &orphanSpans{}
			j.h.sess.Orphans = leftOut
			// One chunk: the harness's channels hold fewer packets than 254.
			j.exchange(prepare, [][]byte{bytes.Join(prepareOK(c.deprecateEOF, defs(c.params), defs(c.cols)), nil)})
			if c.answeredWithOK {
				j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
			}
			// A result after it shows the framing the PREPARE settled.
			j.exchange(cannedCOMQuery(t, 0, "SELECT n FROM t"), resultSet(c.deprecateEOF, [][]byte{col}, rows))
			mocks, err := j.run(t)
			if err != nil {
				t.Fatalf("RecordV2 = %v", err)
			}
			if leftOut.count() != 0 {
				t.Fatalf("%d exchanges left out, want none", leftOut.count())
			}
			want := 2
			if c.answeredWithOK {
				want = 3
			}
			if len(mocks) != want {
				t.Fatalf("%d mocks, want %d", len(mocks), want)
			}
			sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
			if !ok || len(sp.ParamDefs) != c.params || len(sp.ColumnDefs) != c.cols {
				t.Fatalf("the PREPARE recorded %+v; want %d parameters and %d columns", mocks[0].Spec.MySQLResponses[0].Message, c.params, c.cols)
			}
			eof := sp.EOFAfterParamDefs
			if c.cols > 0 {
				eof = sp.EOFAfterColumnDefs
			}
			if (eof != nil) == c.deprecateEOF {
				t.Fatalf("the PREPARE recorded an EOF after its definitions: %v; its client was sent one: %v", eof != nil, !c.deprecateEOF)
			}
			if c.answeredWithOK {
				if got := describe(mocks[1]); got != "a OK" {
					t.Fatalf("SELECT 1 recorded %s, want its OK", got)
				}
			}
			if got := describe(mocks[len(mocks)-1]); got != "a result of 2 rows" {
				t.Fatalf("the SELECT after the PREPARE recorded %s, want its 2 rows", got)
			}
		})
	}
	// 255 definitions under CLIENT_DEPRECATE_EOF: the next answer starts at
	// sequence id 1, which an EOF of the PREPARE would also have. COM_SET_OPTION
	// is answered with an EOF, so an EOF there is either: both exchanges are
	// left out, and the recording goes on from the next command.
	t.Run("an EOF that may be the PREPARE's or the next answer", func(t *testing.T) {
		t.Parallel()
		j := newJoinedMidStream(t)
		leftOut := &orphanSpans{}
		j.h.sess.Orphans = leftOut
		j.exchange(prepare, [][]byte{bytes.Join(prepareOK(true, defs(255), nil), nil)})
		j.exchange(wrapPacket([]byte{0x1b, 0x00, 0x00}, 0), [][]byte{wrapPacket(eofPayload, 1)})
		mocks, err := j.run(t)
		if len(mocks) != 0 {
			t.Fatalf("recorded %d mock(s); the first: %s for %s", len(mocks), describe(mocks[0]), mocks[0].Spec.Metadata["requestOperation"])
		}
		if err != nil {
			t.Fatalf("RecordV2 = %v: the exchanges it cannot frame are left out, and the recording goes on", err)
		}
		if leftOut.count() != 2 {
			t.Fatalf("%d exchanges left out, want the PREPARE and the COM_SET_OPTION", leftOut.count())
		}
	})
	// What follows the PREPARE's definitions is checked on what it can be:
	// its EOF, the start of the next answer, or the server's own ERR. A header
	// read out of row data (the capture lost bytes) is none of them, and is
	// refused before the 3 MB it claims are waited for. As the connection
	// ends, the recording stops there; where the next command's answer
	// starts, that exchange is left out with the PREPARE, and the recording
	// goes on from the command after it.
	for _, next := range []struct {
		name    string
		command []byte
	}{{"as the connection ends", nil}, {"where the next command's answer starts", cannedCOMQuery(t, 0, "SELECT 1")}} {
		t.Run("a misread header after it, "+next.name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			leftOut := &orphanSpans{}
			j.h.sess.Orphans = leftOut
			j.exchange(prepare, prepareOK(true, defs(2), nil))
			if next.command != nil {
				j.h.pushClient(next.command, j.at())
			}
			j.h.pushDest([]byte("0000 of a row"), j.at())
			mocks, err := j.run(t)
			if len(mocks) != 0 {
				t.Fatalf("recorded %d mock(s); the first: %s", len(mocks), describe(mocks[0]))
			}
			if next.command == nil && !errors.Is(err, ErrFramingLost) {
				t.Fatalf("RecordV2 = %v, want ErrFramingLost", err)
			}
			if next.command != nil && err != nil {
				t.Fatalf("RecordV2 = %v: the exchange it cannot frame is left out, and the recording goes on", err)
			}
			if leftOut.count() == 0 {
				t.Fatal("the PREPARE is neither recorded nor left out")
			}
		})
	}
	// A server drops an idle connection with an ERR of its own (MySQL 8's
	// 4031 at wait_timeout), at sequence id 0. No EOF came after the
	// definitions, so the client negotiated CLIENT_DEPRECATE_EOF: the
	// PREPARE is recorded without one, and the ERR answers no command.
	t.Run("the server's own ERR as it drops the idle connection", func(t *testing.T) {
		t.Parallel()
		j := newJoinedMidStream(t)
		leftOut := &orphanSpans{}
		j.h.sess.Orphans = leftOut
		j.exchange(prepare, prepareOK(true, defs(2), nil))
		timeout := append([]byte{0xff, 0xbf, 0x0f, '#'}, "HY000The client was disconnected by the server because of inactivity."...)
		j.h.pushDest(wrapPacket(timeout, 0), j.at())
		mocks, err := j.run(t)
		if err != nil {
			t.Fatalf("RecordV2 = %v", err)
		}
		if len(mocks) != 1 || leftOut.count() != 0 {
			t.Fatalf("%d mock(s), %d exchange(s) left out; want the PREPARE recorded", len(mocks), leftOut.count())
		}
		sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
		if !ok || len(sp.ParamDefs) != 2 || sp.EOFAfterParamDefs != nil {
			t.Fatalf("the PREPARE recorded %+v; want 2 parameters and no EOF", mocks[0].Spec.MySQLResponses[0].Message)
		}
	})
	// A header at sequence id 0 that claims more than an ERR holds is not the
	// server's own ERR, and is refused before its payload is waited for.
	t.Run("a header at sequence id 0 that claims more than an ERR", func(t *testing.T) {
		t.Parallel()
		j := newJoinedMidStream(t)
		j.exchange(prepare, prepareOK(true, defs(2), nil))
		j.h.pushDest([]byte{0x30, 0x30, 0x30, 0x00, 'r', 'o', 'w'}, j.at())
		mocks, err := j.run(t)
		if len(mocks) != 0 {
			t.Fatalf("recorded %d mock(s); the first: %s", len(mocks), describe(mocks[0]))
		}
		if !errors.Is(err, ErrFramingLost) {
			t.Fatalf("RecordV2 = %v, want ErrFramingLost", err)
		}
	})
	// A connection that ends after a PREPARE whose client framed with EOFs
	// was sent the EOF with the response: the PREPARE is recorded with it.
	t.Run("the connection ends after it, a client that framed with EOFs", func(t *testing.T) {
		t.Parallel()
		j := newJoinedMidStream(t)
		leftOut := &orphanSpans{}
		j.h.sess.Orphans = leftOut
		j.exchange(prepare, prepareOK(false, defs(2), nil))
		j.h.pushClient(wrapPacket([]byte{mysql.COM_QUIT}, 0), j.at())
		mocks, err := j.run(t)
		if err != nil {
			t.Fatalf("RecordV2 = %v", err)
		}
		if len(mocks) != 2 || leftOut.count() != 0 {
			t.Fatalf("%d mock(s), %d exchange(s) left out; want the PREPARE and the COM_QUIT recorded", len(mocks), leftOut.count())
		}
		sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
		if !ok || len(sp.ParamDefs) != 2 || sp.EOFAfterParamDefs == nil {
			t.Fatalf("the PREPARE recorded %+v; want 2 parameters and their EOF", mocks[0].Spec.MySQLResponses[0].Message)
		}
		if got := mocks[1].Spec.Metadata["requestOperation"]; got != "COM_QUIT" {
			t.Fatalf("the second mock is a %s, want the COM_QUIT after the PREPARE", got)
		}
	})
	// A connection that ends before the server sends anything more never
	// settles it: the PREPARE is left out.
	t.Run("the connection ends after it", func(t *testing.T) {
		t.Parallel()
		j := newJoinedMidStream(t)
		leftOut := &orphanSpans{}
		j.h.sess.Orphans = leftOut
		j.exchange(prepare, prepareOK(true, defs(2), nil))
		mocks, err := j.run(t)
		if err != nil {
			t.Fatalf("RecordV2 = %v", err)
		}
		if len(mocks) != 0 || leftOut.count() != 1 {
			t.Fatalf("%d mock(s), %d exchange(s) left out; want the PREPARE left out", len(mocks), leftOut.count())
		}
	})
}

// A PREPARE with one run of definitions on an assumed framing waits for the
// server's next packet, which comes only after the client's next command. Its
// response is read whole by then: the wait must not leave the client's bytes
// pending, or a pooled connection idle after a PREPARE is retired as hung.
func TestRecordV2_PostTLS_APrepareWaitingToSettleIsNotPendingWork(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	j := newJoinedMidStream(t)
	var cleared atomic.Int64
	var pending atomic.Bool
	j.h.sess.OnPendingCleared = func() {
		pending.Store(false) // before the count a wait below reads
		cleared.Add(1)
	}
	waitFor := func(cond func() bool) bool {
		deadline := time.Now().Add(5 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return cond()
	}
	// One exchange first: its mock, and the connection's config mock, clear
	// what came before the PREPARE.
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	done := make(chan error, 1)
	go func() { done <- RecordV2(j.ctx, j.h.logger, j.h.sess) }()
	if !waitFor(func() bool { return cleared.Load() >= 2 }) {
		t.Fatalf("pending work cleared %d times after the config mock and SELECT 1, want 2", cleared.Load())
	}
	pending.Store(true) // the PREPARE's bytes, as the supervisor marks them
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?, ?)"...), 0), prepareOK(true, [][]byte{def, def}, nil))
	settled := waitFor(func() bool { return !pending.Load() })
	j.h.closeStreams()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("RecordV2 still running a minute after the streams closed")
	}
	if !settled {
		t.Fatal("the PREPARE's bytes are still pending work while it waits for the packet that settles its framing")
	}
}

// A PREPARE that has not settled an assumed framing does not hold up the
// client. Its mock waits for the first packet of the next response, and the
// commands without a response the client sends meanwhile (COM_STMT_CLOSE,
// COM_STMT_SEND_LONG_DATA) are read, their bytes cleared from the pending
// work, and recorded behind it, in the order they were sent. Before, the
// recorder waited on the server stream for that packet, which comes only once
// the client sends a command with a response: the client's bytes were left
// unread, still pending through the idle spell after them, and the hang
// watchdog retired a connection whose recording was going on.
func TestRecordV2_PostTLS_CommandsAfterAnUnsettledPrepareAreRead(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?, ?)"...), 0)
	closeStmt := wrapPacket(binary.LittleEndian.AppendUint32([]byte{mysql.COM_STMT_CLOSE}, undecodableStmtID), 0)
	for _, c := range []struct {
		name         string
		deprecateEOF bool
	}{{"a client that framed with EOFs", false}, {"a client that negotiated CLIENT_DEPRECATE_EOF", true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			leftOut := &orphanSpans{}
			j.h.sess.Orphans = leftOut
			var cleared atomic.Int64
			var pending atomic.Bool
			j.h.sess.OnPendingCleared = func() {
				pending.Store(false) // before the count a wait below reads
				cleared.Add(1)
			}
			done := make(chan error, 1)
			go func() { done <- RecordV2(j.ctx, j.h.logger, j.h.sess) }()
			j.exchange(prepare, prepareOK(c.deprecateEOF, [][]byte{def, def}, nil))
			// The config mock, and the PREPARE read whole.
			read := waitFor(func() bool { return cleared.Load() >= 2 })
			pending.Store(true) // the COM_STMT_CLOSE's bytes, as the supervisor marks them
			j.h.pushClient(closeStmt, j.at())
			settled := read && waitFor(func() bool { return !pending.Load() })
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
			j.h.closeStreams()
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Minute):
				t.Fatal("RecordV2 still running a minute after the streams closed")
			}
			if !read {
				t.Fatal("the PREPARE was not read")
			}
			if !settled {
				t.Fatal("the COM_STMT_CLOSE after a PREPARE that has not settled its framing is still pending work: a connection idle after it is retired as hung")
			}
			if err != nil {
				t.Fatalf("RecordV2 = %v", err)
			}
			var mocks []*models.Mock
			for len(j.h.mocks) > 0 {
				if m := <-j.h.mocks; m.Spec.Metadata["type"] != "config" {
					mocks = append(mocks, m)
				}
			}
			var ops []string
			for _, m := range mocks {
				ops = append(ops, m.Spec.Metadata["requestOperation"])
			}
			if got, want := strings.Join(ops, ","), "COM_STMT_PREPARE,COM_STMT_CLOSE,COM_QUERY"; got != want || leftOut.count() != 0 {
				t.Fatalf("recorded %s with %d exchange(s) left out; want %s, in the order they were sent, and none left out", got, leftOut.count(), want)
			}
			for i := 1; i < len(mocks); i++ {
				if mocks[i].Spec.ReqTimestampMock.Before(mocks[i-1].Spec.ReqTimestampMock) {
					t.Fatalf("mock %d (%s) was sent before mock %d (%s)", i, ops[i], i-1, ops[i-1])
				}
			}
			sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
			if !ok || len(sp.ParamDefs) != 2 || (sp.EOFAfterParamDefs != nil) == c.deprecateEOF {
				t.Fatalf("the PREPARE recorded %+v; want 2 parameters, with an EOF after them only for a client that framed with EOFs", mocks[0].Spec.MySQLResponses[0].Message)
			}
		})
	}
}

// waitFor polls cond for up to 5 seconds, and reports whether it held.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// The recording stops while a PREPARE waits for the packet that settles its
// framing: its response was read whole, and its mock is not recorded. The
// exchange is left out, so the test case recorded over it is not saved
// without its mock. Before, the stop ended the wait as the end of a recording
// does, which leaves nothing out.
func TestRecordV2_PostTLS_AnUnsettledPrepareIsLeftOutWhenTheRecordingStops(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	var cleared atomic.Int64
	j.h.sess.OnPendingCleared = func() { cleared.Add(1) }
	ctx, cancel := context.WithCancel(j.ctx)
	defer cancel()
	prepareAt := j.at()
	j.h.pushClient(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?, ?)"...), 0), prepareAt)
	for _, p := range prepareOK(true, [][]byte{def, def}, nil) {
		j.h.pushDest(p, j.at())
	}
	done := make(chan error, 1)
	go func() { done <- RecordV2(ctx, j.h.logger, j.h.sess) }()
	// The config mock, and the PREPARE read whole.
	read := waitFor(func() bool { return cleared.Load() >= 2 })
	cancel()
	// The client's next command, and its answer, arrive as the recording stops.
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	j.h.closeStreams()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("RecordV2 still running a minute after the streams closed")
	}
	if !read {
		t.Fatal("the PREPARE was not read")
	}
	for len(j.h.mocks) > 0 {
		if m := <-j.h.mocks; m.Spec.Metadata["requestOperation"] == "COM_STMT_PREPARE" {
			return // recorded: nothing to leave out
		}
	}
	for _, w := range leftOut.windows {
		if !prepareAt.Before(w[0]) && !prepareAt.After(w[1]) {
			return
		}
	}
	t.Fatalf("the PREPARE is neither recorded nor left out (windows left out: %v): the test case recorded over it is saved without its mock", leftOut.windows)
}

// A connection joined mid-stream whose own pre-TLS leg was captured (its
// SSLRequest, pushed under its identity) is framed by the capability flags in
// that SSLRequest, which a client sends again in its HandshakeResponse41 and
// which its config mock declares. Nothing is assumed, so there is nothing to
// settle: a PREPARE with one run of definitions is recorded when it is read,
// and a run of 255 definitions followed by a command answered with an EOF is
// framed.
func TestRecordV2_PostTLS_AJoinedConnectionsOwnSSLRequestFramesIt(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	defs := func(n int) [][]byte {
		var out [][]byte
		for i := 0; i < n; i++ {
			out = append(out, def)
		}
		return out
	}
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?, ?)"...), 0)
	clientCaps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_SSL | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_PLUGIN_AUTH)
	t.Run("CLIENT_DEPRECATE_EOF: a PREPARE the connection ends after", func(t *testing.T) {
		t.Parallel()
		j := newJoinedMidStream(t)
		j.ownLeg(t, clientCaps|uint32(mysql.CLIENT_DEPRECATE_EOF))
		leftOut := &orphanSpans{}
		j.h.sess.Orphans = leftOut
		j.exchange(prepare, prepareOK(true, defs(2), nil))
		mocks, err := j.run(t)
		if err != nil {
			t.Fatalf("RecordV2 = %v", err)
		}
		if len(mocks) != 1 || leftOut.count() != 0 {
			t.Fatalf("%d mock(s), %d exchange(s) left out; want the PREPARE recorded", len(mocks), leftOut.count())
		}
		sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
		if !ok || len(sp.ParamDefs) != 2 || sp.EOFAfterParamDefs != nil {
			t.Fatalf("the PREPARE recorded %+v; want 2 parameters and no EOF", mocks[0].Spec.MySQLResponses[0].Message)
		}
	})
	t.Run("EOFs: 255 parameters, then a command answered with an EOF", func(t *testing.T) {
		t.Parallel()
		j := newJoinedMidStream(t)
		j.ownLeg(t, clientCaps)
		leftOut := &orphanSpans{}
		j.h.sess.Orphans = leftOut
		// One chunk: the harness's channels hold fewer packets than 255.
		j.exchange(prepare, [][]byte{bytes.Join(prepareOK(false, defs(255), nil), nil)})
		j.exchange(wrapPacket([]byte{0x1b, 0x00, 0x00}, 0), [][]byte{wrapPacket(eofPayload, 1)})
		j.exchange(cannedCOMQuery(t, 0, "SELECT n FROM t"), resultSet(false, [][]byte{col}, [][]byte{textRowPayload("a"), textRowPayload("b")}))
		mocks, err := j.run(t)
		if err != nil {
			t.Fatalf("RecordV2 = %v", err)
		}
		if len(mocks) != 2 || leftOut.count() != 1 {
			t.Fatalf("%d mock(s), %d exchange(s) left out; want the PREPARE and the SELECT recorded, and the COM_SET_OPTION left out", len(mocks), leftOut.count())
		}
		sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
		if !ok || len(sp.ParamDefs) != 255 || sp.EOFAfterParamDefs == nil {
			t.Fatalf("the PREPARE recorded %T; want 255 parameters and their EOF", mocks[0].Spec.MySQLResponses[0].Message)
		}
		if got := describe(mocks[1]); got != "a result of 2 rows" {
			t.Fatalf("the SELECT recorded %s, want its 2 rows", got)
		}
	})
}

// A server drops an idle connection with an ERR of its own (MySQL 8's 4031
// at wait_timeout, one at shutdown), at sequence id 0: it answers no command.
// The client reads it as the answer to its next command (a pool's ping on
// borrow), and the server has closed. That is the connection's end, not lost
// framing: every exchange before it is recorded, and nothing is left out.
func TestRecordV2_TheServersOwnErrorEndsTheRecording(t *testing.T) {
	t.Parallel()
	resetWarnLimiters()
	xs := kitTraffic(t, []int{7}, false)
	s := layOut(xs)
	ping := wrapPacket([]byte{0x0e}, 0)
	timeout := wrapPacket(append([]byte{0xff, 0xbf, 0x0f, '#'}, "HY000The client was disconnected by the server because of inactivity."...), 0)
	client := append(append([]byte{}, s.client...), ping...)
	server := append(append([]byte{}, s.server...), timeout...)
	rec := recordConn(t, chunked(client, 64), chunked(server, 64), false, 5*time.Second)
	if !rec.returned || rec.err != nil {
		t.Fatalf("RecordV2 returned=%v err=%v: the server's own ERR ends the connection; it is not lost framing", rec.returned, rec.err)
	}
	if w := wrongMocks(t, rec.mocks); len(w) > 0 {
		t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
	}
	if got := queryMocks(rec.mocks); got != len(xs) {
		t.Fatalf("%d query mocks, want the %d before the ERR", got, len(xs))
	}
	if n := rec.leftOut.count(); n != 0 {
		t.Fatalf("%d exchanges left out, want none", n)
	}
	if n := rec.logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("%d WARN lines, want none: %v", n, rec.logs.FilterLevelExact(zapcore.WarnLevel).All()[0].Message)
	}
}

// A packet at sequence id 0 that starts with 0xFF but has no ERR's shape (a
// '#' and a SQL state of five digits or capital letters after the code) is not
// the server's own ERR: row bytes a hole put the reader on can start that way
// (a binary row's small INT, then a negative one). It is lost framing, and ends
// the recording with an error, never as the connection's end. Each case fails
// one condition of the shape only.
func TestRecordV2_RowBytesShapedLikeAnErrorAtSequenceIDZeroAreLostFraming(t *testing.T) {
	t.Parallel()
	resetWarnLimiters()
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"one byte short of an ERR's code and state", []byte{0xff, 0xbf, 0x0f, '#', 'H', 'Y', '0', '0'}},
		{"no '#' before the state", append([]byte{0xff, 0xbf, 0x0f, 'x'}, "HY000not an error"...)},
		{"a state that is not digits and capital letters", append([]byte{0xff, 0xbf, 0x0f, '#'}, "hy000not an error"...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7}, false)
			s := layOut(xs)
			ping := wrapPacket([]byte{0x0e}, 0)
			rowBytes := wrapPacket(tc.payload, 0)
			client := append(append([]byte{}, s.client...), ping...)
			server := append(append([]byte{}, s.server...), rowBytes...)
			rec := recordConn(t, chunked(client, 64), chunked(server, 64), false, 5*time.Second)
			if !rec.returned || !errors.Is(rec.err, ErrFramingLost) {
				t.Fatalf("RecordV2 returned=%v err=%v, want ErrFramingLost", rec.returned, rec.err)
			}
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
			}
			if got := queryMocks(rec.mocks); got != len(xs) {
				t.Fatalf("%d query mocks, want the %d before the misread packet", got, len(xs))
			}
		})
	}
}

// A packet of 16 MiB - 1 bytes continues in the next one, and so on: a
// command or a row of 16 MiB or more is one packet over several, and the
// response to such a command starts at the sequence id after its last. Read
// as separate packets, the command was recorded cut short and its rest as a
// command of its own. Joined, it is framed, and the connection's recording
// goes on past it; but the replayer reads a command one packet at a time and
// writes a row's length in one header, so a mock of either exchange would
// fail its test case on replay. Both are left out instead.
func TestRecordV2_PacketsOf16MiBOrMoreAreFramedAndLeftOut(t *testing.T) {
	t.Parallel()
	bigText := strings.Repeat("x", 17<<20)
	insert := "INSERT INTO blobs VALUES ('" + bigText + "')"
	xs := kitTraffic(t, []int{7}, false)
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "b", OrgName: "b", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 1 << 30, Type: byte(mysql.FieldTypeVarString)})
	row := lenenc(bigText)
	// The INSERT: packets 0 and 1, answered at sequence id 2. The SELECT:
	// its row spans packets 4 and 5.
	insertCmd := splitPacket(append([]byte{mysql.COM_QUERY}, insert...), 0)
	okAt2 := wrapPacket([]byte{0x00, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00}, 2)
	selectCmd := cannedCOMQuery(t, 0, "SELECT b FROM blobs")
	selectReply := append(append(append(wrapPacket([]byte{0x01}, 1), wrapPacket(col, 2)...), wrapPacket(eofPayload, 3)...), splitPacket(row, 4)...)
	selectReply = append(selectReply, wrapPacket(eofPayload, 6)...)
	s := layOut(xs)
	client := append(append(append([]byte{}, insertCmd...), selectCmd...), s.client...)
	server := append(append(append([]byte{}, okAt2...), selectReply...), s.server...)
	rec := recordConn(t, chunked(client, 65536), chunked(server, 65536), true, 2*time.Minute)
	requireRecordedOn(t, rec, 2, len(xs))
	for _, m := range rec.mocks {
		if qp, _ := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); qp != nil && strings.Contains(qp.Query, "blobs") {
			t.Fatalf("recorded %q (%s): an exchange with a packet of 16 MiB or more must be left out", qp.Query[:min(len(qp.Query), 40)], describe(m))
		}
	}
}

// The first command on a TLS connection joined mid-stream is read by the
// handshake, which tells it from a HandshakeResponse41. One of 16 MiB or more
// is joined there too, and its response starts after its last packet.
func TestRecordV2_PostTLS_AFirstCommandOf16MiBOrMoreIsFramed(t *testing.T) {
	t.Parallel()
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	insert := "INSERT INTO blobs VALUES ('" + strings.Repeat("x", 17<<20) + "')"
	j.exchange(splitPacket(append([]byte{mysql.COM_QUERY}, insert...), 0), [][]byte{wrapPacket([]byte{0x00, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00}, 2)})
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v", err)
	}
	if len(mocks) != 1 || describe(mocks[0]) != "a OK" || leftOut.count() != 1 {
		t.Fatalf("%d mock(s), %d exchange(s) left out; want the INSERT left out and SELECT 1 recorded", len(mocks), leftOut.count())
	}
	if qp, _ := mocks[0].Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); qp == nil || qp.Query != "SELECT 1" {
		t.Fatalf("recorded %v, want SELECT 1", mocks[0].Spec.MySQLRequests[0].Message)
	}
}

// splitPacket frames payload as the protocol does: packets of 16 MiB - 1
// bytes, numbered on from first, to one that is shorter.
func splitPacket(payload []byte, first byte) []byte {
	var out []byte
	seq := first
	for {
		n := min(len(payload), 1<<24-1)
		out = append(out, wrapPacket(payload[:n], seq)...)
		seq++
		payload = payload[n:]
		if n < 1<<24-1 {
			return out
		}
	}
}

// A query killed, or out of time, partway through its rows is answered with
// the rows sent so far and then an ERR where the next row would be. The
// response ends there, framed: it is left out (a mock holds rows or an error,
// not both), and the connection's recording goes on.
func TestRecordV2_AnErrorInPlaceOfARowIsLeftOut(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7}, false)
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	errPkt := append([]byte{0xff, 0xd0, 0x0b, '#'}, "HY000Query execution was interrupted, maximum statement execution time exceeded"...)
	killed := exchange{command: cannedCOMQuery(t, 0, "SELECT n FROM t"),
		reply: frame([][]byte{{0x01}, col, eofPayload, textRowPayload("a"), textRowPayload("b"), errPkt})}
	s := layOut(append(append([]exchange{}, killed), xs...))
	rec := recordConn(t, chunked(s.client, 64), chunked(s.server, 64), true, 5*time.Second)
	requireRecordedOn(t, rec, 1, len(xs))
}

// buffered is what RecordV2 did with a connection whose bytes were all
// buffered before it read any (recordBuffered).
type buffered struct {
	err     error
	leftOut *orphanSpans
	// handshakeEnd is when the handshake's last packet (the server's OK)
	// arrived, commandAt when each exchange's command did (zero for one the
	// capture lost), and lastArrival when the last byte of the connection did.
	handshakeEnd, lastArrival time.Time
	commandAt                 []time.Time
}

// recordBuffered records a connection whose bytes were all buffered before the
// recorder read any of them, as they are when a parser falls behind its
// connection: the handshake, then each exchange's command and reply, each a
// chunk stamped a millisecond after the one before, as a capture stamps them
// (when they arrived, all within the last minute).
func recordBuffered(t *testing.T, xs []exchange) buffered {
	t.Helper()
	h := newV2Harness(t)
	b := buffered{leftOut: &orphanSpans{}}
	h.sess.Orphans = b.leftOut
	base := time.Now().Add(-time.Minute)
	tick := 0
	at := func() time.Time {
		tick++
		b.lastArrival = base.Add(time.Duration(tick) * time.Millisecond)
		return b.lastArrival
	}
	greetingBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
	if err != nil {
		t.Fatal(err)
	}
	h.pushDest(greetingBuf, at())
	h.pushClient(cannedHandshakeResponse41(t, 1, false), at())
	b.handshakeEnd = at()
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), b.handshakeEnd)
	for _, x := range xs {
		var cmdAt time.Time
		if len(x.command) > 0 {
			cmdAt = at()
			h.pushClient(x.command, cmdAt)
		}
		b.commandAt = append(b.commandAt, cmdAt)
		if reply := bytes.Join(x.reply, nil); len(reply) > 0 {
			h.pushDest(reply, at())
		}
	}
	h.closeStreams()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b.err = RecordV2(ctx, h.logger, h.sess)
	return b
}

// When the recording stops in an exchange, the window it leaves out runs from
// that exchange's start to when the recorder gave up, and the supervisor counts
// what the connection carries from the parser's retirement, which follows. A
// window that ended at the last chunk the parser read left a gap between the
// two: the bytes a parser behind its connection had not read yet arrived in it,
// and a test case recorded there was saved without its mocks.
//
// When the bytes the capture lost were client bytes, of the first command after
// the handshake, the lost command was sent after the handshake ended: the
// window starts there. It started at the misread header, which arrived after
// the lost command, and covered nothing.
//
// Server bytes lost inside a result stop nothing: that exchange alone is left
// out, over a window that holds when its command was sent, and the recording
// goes on (realign).
func TestRecordV2_TheWindowLeftOutReachesTheParsersRetirement(t *testing.T) {
	t.Parallel()
	requireWindow := func(t *testing.T, b buffered, startBy time.Time, startWhat string) {
		t.Helper()
		if !errors.Is(b.err, ErrFramingLost) {
			t.Fatalf("RecordV2 = %v, want ErrFramingLost", b.err)
		}
		w, ok := b.leftOut.last()
		if !ok {
			t.Fatal("no window left out")
		}
		if w[0].After(startBy) {
			t.Fatalf("the window left out starts at %v, after %s (%v): the test case of the exchange the recording stopped in is not left out", w[0].Format(time.StampMicro), startWhat, startBy.Format(time.StampMicro))
		}
		if w[1].Before(b.lastArrival) {
			t.Fatalf("the window left out ends at %v, before the last byte the connection had buffered arrived (%v): the test cases recorded in between are in neither this window nor the span counted from the parser's retirement, and are saved without their mocks", w[1].Format(time.StampMicro), b.lastArrival.Format(time.StampMicro))
		}
	}
	t.Run("server bytes lost inside a result: the exchange alone", func(t *testing.T) {
		t.Parallel()
		xs := kitTraffic(t, []int{7}, false)
		sel := xs[2].reply // the second SELECT: rows 5 to 9 lost
		xs[2].reply = append(append([][]byte{}, sel[:1+6+1+5]...), sel[1+6+1+10:]...)
		b := recordBuffered(t, xs)
		if b.err != nil {
			t.Fatalf("RecordV2 = %v: a response it cannot frame costs its own exchange, not the rest of the connection's recording", b.err)
		}
		if n := b.leftOut.count(); n != 1 {
			t.Fatalf("%d windows left out, want the SELECT's alone", n)
		}
		w, _ := b.leftOut.last()
		if w[0].After(b.commandAt[2]) || w[1].Before(b.commandAt[2]) {
			t.Fatalf("the window left out is [%v, %v]; it must hold when the SELECT was sent (%v)", w[0].Format(time.StampMicro), w[1].Format(time.StampMicro), b.commandAt[2].Format(time.StampMicro))
		}
	})
	t.Run("client bytes lost from the first command on", func(t *testing.T) {
		t.Parallel()
		xs := kitTraffic(t, []int{7}, false)
		// SET autocommit=0 lost whole, and the next command's header and the
		// start of its text.
		xs[0].command = nil
		xs[1].command = xs[1].command[4+20:]
		b := recordBuffered(t, xs)
		requireWindow(t, b, b.handshakeEnd, "the handshake ended, after which the lost command was sent")
	})
}

// recordFramed records a connection from its handshake, framed with EOFs or
// under CLIENT_DEPRECATE_EOF, as both its ends negotiated: the handshake, the
// exchanges push pushes, then the streams' end. It returns RecordV2's error,
// every mock but the config one, and the exchanges left out.
func recordFramed(t *testing.T, deprecateEOF bool, push func(h *v2Harness, at func() time.Time)) ([]*models.Mock, *orphanSpans, error) {
	t.Helper()
	h := newV2Harness(t)
	leftOut := &orphanSpans{}
	h.sess.Orphans = leftOut
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION)
	if deprecateEOF {
		caps |= uint32(mysql.CLIENT_DEPRECATE_EOF)
	}
	greeting, err := connphase.EncodeHandshakeV10(context.Background(), zap.NewNop(), &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a, ServerVersion: "8.0.test-keploy", ConnectionID: 42,
		AuthPluginData: bytes.Repeat([]byte{0x11}, 20), CapabilityFlags: caps,
		CharacterSet: 0x21, StatusFlags: 0x02, AuthPluginName: string(mysql.Native),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), &mysql.HandshakeResponse41Packet{
		CapabilityFlags: caps, MaxPacketSize: 1 << 24, CharacterSet: 0x21, Username: "root",
		AuthResponse: bytes.Repeat([]byte{0xAB}, 20), AuthPluginName: string(mysql.Native),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute)
	tick := 0
	at := func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Millisecond) }
	h.pushDest(wrapPacket(greeting, 0), at())
	h.pushClient(wrapPacket(response, 1), at())
	h.pushDest(cannedOK(t, 2, caps), at())
	push(h, at)
	h.closeStreams()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = RecordV2(ctx, h.logger, h.sess)
	var mocks []*models.Mock
	for {
		select {
		case m := <-h.mocks:
			if m.Spec.Metadata["type"] != "config" {
				mocks = append(mocks, m)
			}
		default:
			return mocks, leftOut, err
		}
	}
}

// cursorFixture is a cursor's traffic: the definitions of its columns, binary
// rows of them, and the packet that ends a reply with a status, all as a
// connection framed with EOFs, or under CLIENT_DEPRECATE_EOF, carries them.
type cursorFixture struct {
	cols         int
	deprecateEOF bool
	defs         [][]byte
}

const (
	statusAutocommit   = 0x0002
	statusCursorExists = 0x0040 // SERVER_STATUS_CURSOR_EXISTS
	statusLastRowSent  = 0x0080 // SERVER_STATUS_LAST_ROW_SENT
)

func newCursorFixture(t *testing.T, cols int, deprecateEOF bool) cursorFixture {
	t.Helper()
	f := cursorFixture{cols: cols, deprecateEOF: deprecateEOF}
	for i := 0; i < cols; i++ {
		name := fmt.Sprintf("c%d", i)
		f.defs = append(f.defs, columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Schema: "app", Table: "events", OrgTable: "events",
			Name: name, OrgName: name, FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)}))
	}
	return f
}

// row is a binary row whose every column holds v.
func (f cursorFixture) row(v int64) []byte {
	p := append([]byte{0x00}, make([]byte, (f.cols+7+2)/8)...) // OK header, null bitmap
	for i := 0; i < f.cols; i++ {
		p = binary.LittleEndian.AppendUint64(p, uint64(v))
	}
	return p
}

// end is the packet that ends a reply with status: an EOF, or the OK that
// replaces it under CLIENT_DEPRECATE_EOF.
func (f cursorFixture) end(status uint16) []byte {
	if f.deprecateEOF {
		return []byte{0xfe, 0x00, 0x00, byte(status), byte(status >> 8), 0x00, 0x00}
	}
	return []byte{0xfe, 0x00, 0x00, byte(status), byte(status >> 8)}
}

// executeReply is the reply to a COM_STMT_EXECUTE that opened a cursor: the
// column count and definitions, then the end of the reply with
// SERVER_STATUS_CURSOR_EXISTS, and no rows.
func (f cursorFixture) executeReply() [][]byte {
	count := []byte{byte(f.cols)}
	if f.cols >= 251 {
		count = []byte{0xfc, byte(f.cols), byte(f.cols >> 8)}
	}
	return frame(append(append([][]byte{count}, f.defs...), f.end(statusAutocommit|statusCursorExists)))
}

// cursorExecute is a COM_STMT_EXECUTE of undecodableStmtID that asks for a
// read-only cursor.
var cursorExecute = wrapPacket([]byte{mysql.COM_STMT_EXECUTE, byte(undecodableStmtID), 0, 0, 0, mysql.CURSOR_TYPE_READ_ONLY, 1, 0, 0, 0}, 0)

// cursorFetch is a COM_STMT_FETCH of two rows of undecodableStmtID's cursor.
var cursorFetch = wrapPacket([]byte{0x1c, byte(undecodableStmtID), 0, 0, 0, 2, 0, 0, 0}, 0)

// noOpenCursor is the ERR a COM_STMT_FETCH gets once the cursor has closed.
var noOpenCursor = append([]byte{0xff, 0x8d, 0x05, '#', 'H', 'Y', '0', '0', '0'}, "The statement (5) has no open cursor."...)

// A COM_STMT_EXECUTE that asks for a cursor (CURSOR_TYPE_READ_ONLY: a driver
// that fetches a result a few rows at a time) is answered with its column count
// and definitions, then an EOF whose status has SERVER_STATUS_CURSOR_EXISTS,
// and no rows: they stay on the server until the client asks for them with
// COM_STMT_FETCH. Under CLIENT_DEPRECATE_EOF that EOF is the OK that replaces
// it. Each COM_STMT_FETCH is answered with binary rows and an EOF (or that
// OK), or with an ERR.
//
// Before, framed with EOFs, the recorder read on for the EXECUTE's rows and
// stopped the connection at the FETCH's reply, which starts at sequence id 1,
// with a warning that blamed the capture; with 254 columns the EXECUTE's reply
// ends on sequence id 0, so it took the FETCH's rows for the EXECUTE's. Under
// CLIENT_DEPRECATE_EOF the EXECUTE was framed, but the FETCH, a command with
// no decoder, stopped the connection. A pooled connection that used a cursor
// once was recorded no further.
//
// The EXECUTE is recorded as the server answered it. Each FETCH is framed and
// left out, since the replayer cannot serve one, and the connection's
// recording goes on.
func TestRecordV2_ACursorsFetchesAreFramedAndLeftOut(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		deprecateEOF bool
		cols         int
	}{
		{"framed with EOFs", false, 2},
		{"framed with EOFs, 254 columns", false, 254},
		{"CLIENT_DEPRECATE_EOF", true, 2},
		{"CLIENT_DEPRECATE_EOF, 254 columns", true, 254},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newCursorFixture(t, c.cols, c.deprecateEOF)
			prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT * FROM events WHERE id > 0"...), 0)
			fetches := [][][]byte{
				frame([][]byte{f.row(1), f.row(2), f.end(statusAutocommit | statusCursorExists)}),
				frame([][]byte{f.row(3), f.end(statusAutocommit | statusLastRowSent)}),
				frame([][]byte{noOpenCursor}),
			}
			col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "1", OrgName: "1", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 1, Type: byte(mysql.FieldTypeLongLong)})
			// One chunk per reply: the harness's channels hold fewer packets
			// than 254.
			mocks, leftOut, err := recordFramed(t, c.deprecateEOF, func(h *v2Harness, at func() time.Time) {
				h.pushClient(prepare, at())
				h.pushDest(bytes.Join(prepareOK(c.deprecateEOF, nil, f.defs), nil), at())
				h.pushClient(cursorExecute, at())
				h.pushDest(bytes.Join(f.executeReply(), nil), at())
				for _, reply := range fetches {
					h.pushClient(cursorFetch, at())
					h.pushDest(bytes.Join(reply, nil), at())
				}
				h.pushClient(wrapPacket([]byte{mysql.COM_STMT_CLOSE, byte(undecodableStmtID), 0, 0, 0}, 0), at())
				h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), at())
				h.pushDest(bytes.Join(resultSet(c.deprecateEOF, [][]byte{col}, [][]byte{textRowPayload("1")}), nil), at())
			})
			if err != nil {
				t.Fatalf("RecordV2 = %v: a cursor's exchanges are framed, and must not stop the connection's recording", err)
			}
			for _, op := range []string{"COM_STMT_PREPARE", "COM_STMT_EXECUTE", "COM_STMT_CLOSE", "COM_QUERY"} {
				if n := len(mocksOf(mocks, op)); n != 1 {
					t.Fatalf("%d %s mocks, want 1", n, op)
				}
			}
			if len(mocks) != 4 {
				t.Fatalf("%d mocks, want 4: a FETCH was recorded (%s)", len(mocks), mocks[len(mocks)-1].Spec.Metadata["requestOperation"])
			}
			if n := leftOut.count(); n != len(fetches) {
				t.Fatalf("%d exchanges left out, want the %d FETCHes: the test cases recorded over a FETCH would be saved without its mock", n, len(fetches))
			}
			// The EXECUTE's mock replays what the server answered: the
			// definitions, then the EOF (or OK) that says the rows wait in a
			// cursor, and nothing after it.
			rs, _ := mocksOf(mocks, "COM_STMT_EXECUTE")[0].Spec.MySQLResponses[0].Message.(*mysql.BinaryProtocolResultSet)
			if rs == nil || len(rs.Columns) != c.cols || len(rs.Rows) != 0 {
				t.Fatalf("the EXECUTE recorded %s, want %d columns and no rows", describeBinary(rs), c.cols)
			}
			replayed, err := query.EncodeBinaryResultSet(context.Background(), zap.NewNop(), rs)
			if err != nil {
				t.Fatal(err)
			}
			if answered := bytes.Join(f.executeReply(), nil)[4:]; !bytes.Equal(replayed, answered) {
				t.Fatalf("the EXECUTE's mock replays %d bytes that differ from the %d the server answered with", len(replayed), len(answered))
			}
			if got := describe(mocksOf(mocks, "COM_QUERY")[0]); got != "a result of 1 rows" {
				t.Fatalf("SELECT 1 after the cursor recorded %s, want its row", got)
			}
		})
	}
}

func describeBinary(rs *mysql.BinaryProtocolResultSet) string {
	if rs == nil {
		return "no binary result"
	}
	return fmt.Sprintf("%d columns and %d rows", len(rs.Columns), len(rs.Rows))
}

// A pooled connection joined mid-stream can be in the middle of a cursor, its
// first command a COM_STMT_FETCH. The packet that ends the FETCH's reply
// settles the framing assumed for it: a 5-byte EOF for a client that framed
// with EOFs, the OK that replaces it for one that negotiated
// CLIENT_DEPRECATE_EOF. A PREPARE after it, with one run of definitions, is
// then framed as its client was answered, without waiting on the next packet:
// with 255 columns framed with EOFs, its EOF takes the sequence id the next
// answer starts at, and could not be told from one; under
// CLIENT_DEPRECATE_EOF, the connection can end right after it.
func TestRecordV2_PostTLS_AFetchSettlesAnAssumedFraming(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		deprecateEOF bool
		cols         int
	}{
		{"a client that framed with EOFs", false, 255},
		{"a client that negotiated CLIENT_DEPRECATE_EOF", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newCursorFixture(t, c.cols, c.deprecateEOF)
			j := newJoinedMidStream(t)
			leftOut := &orphanSpans{}
			j.h.sess.Orphans = leftOut
			j.exchange(cursorFetch, frame([][]byte{f.row(7), f.end(statusAutocommit | statusLastRowSent)}))
			prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT * FROM events"...), 0)
			// One chunk: the harness's channels hold fewer packets than 255.
			j.exchange(prepare, [][]byte{bytes.Join(prepareOK(c.deprecateEOF, nil, f.defs), nil)})
			mocks, err := j.run(t)
			if err != nil {
				t.Fatalf("RecordV2 = %v", err)
			}
			if len(mocks) != 1 || leftOut.count() != 1 {
				t.Fatalf("%d mock(s), %d exchange(s) left out; want the FETCH left out and the PREPARE recorded", len(mocks), leftOut.count())
			}
			sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
			if !ok || len(sp.ColumnDefs) != c.cols || (sp.EOFAfterColumnDefs != nil) == c.deprecateEOF {
				t.Fatalf("the PREPARE recorded %T; want %d columns, with an EOF after them only for a client that framed with EOFs", mocks[0].Spec.MySQLResponses[0].Message, c.cols)
			}
		})
	}
}
