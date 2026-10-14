package recorder

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap/zapcore"
)

// A pooled connection carries the traffic of every request that borrows it.
// When the recorder could not frame one response on it, it used to stop
// recording the connection there, and every test case recorded while the
// connection went on carrying traffic was left out: on recordings whose
// requests all ran on a pool of ten connections, one response it could not
// frame left out about 1,600 of 3,199 test cases (the CPU-starved lane's
// pipeline 10210) and, one LOAD DATA LOCAL INFILE, 2,701 of 3,300, nearly all
// of them with every mock recorded (keploy/enterprise#2754). A client that
// waits for each answer before its next command has the whole of that answer
// captured before the next command is, and the next answer after it: the
// recorder leaves out the one exchange, skips the rest of its response, and
// goes on from the next command.

// commandAt is when onTheWire stamps exchange i's command.
func commandAt(t *testing.T, xs []exchange, n, i int) time.Time {
	t.Helper()
	if len(xs[i].command) == 0 {
		t.Fatalf("fixture: exchange %d has no command", i)
	}
	client, _ := onTheWire(xs[:i+1], n)
	return client[len(client)-1].ReadAt
}

// insert puts x in xs before exchange i.
func insert(xs []exchange, i int, x ...exchange) []exchange {
	return append(append(append([]exchange{}, xs[:i]...), x...), xs[i:]...)
}

// requireRealigned asserts the recording left out one exchange, whose command
// was sent at sentAt, recorded every other exchange right (queries of them),
// and went on to the connection's end.
func requireRealigned(t *testing.T, rec recording, sentAt time.Time, queries int) {
	t.Helper()
	if w := wrongMocks(t, rec.mocks); len(w) > 0 {
		t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
	}
	if !rec.returned || rec.err != nil {
		t.Fatalf("RecordV2 returned=%v err=%v: a response it cannot frame must cost its own exchange, not the rest of the connection's recording", rec.returned, rec.err)
	}
	if got := rec.leftOut.count(); got != 1 {
		t.Fatalf("%d exchanges left out, want the 1 it could not frame", got)
	}
	w, _ := rec.leftOut.last()
	if w[1].Before(w[0]) {
		t.Fatalf("the window left out ends (%v) before it starts (%v)", w[1], w[0])
	}
	if w[0].After(sentAt) || w[1].Before(sentAt) {
		t.Fatalf("the window left out is [%v, %v]; it must hold %v, when the exchange's command was sent: the test case that sent it is recorded over that", w[0].Format(time.StampMicro), w[1].Format(time.StampMicro), sentAt.Format(time.StampMicro))
	}
	if got := queryMocks(rec.mocks); got != queries {
		t.Fatalf("%d query mocks, want %d: every exchange but the one left out", got, queries)
	}
	if want := int64(len(rec.mocks) + 1); rec.cleared < want {
		t.Fatalf("pending work cleared %d times, want at least %d: a connection that then sits idle is retired as hung", rec.cleared, want)
	}
}

// loadDataLocal is an exchange the recorder does not follow: a LOAD DATA
// LOCAL INFILE, answered with the server's request for the file (0xFB), the
// client's upload, and the server's OK.
func loadDataLocal(t *testing.T) []exchange {
	t.Helper()
	ok := frame([][]byte{{0x00, 0x05, 0x00, 0x02, 0x00, 0x00, 0x00}})[0]
	ok[3] = 4 // after the request (1) and the upload's two packets (2, 3)
	return []exchange{
		{command: cannedCOMQuery(t, 0, "LOAD DATA LOCAL INFILE '/tmp/sessions.csv' INTO TABLE sessions"),
			reply: [][]byte{wrapPacket(append([]byte{0xfb}, "/tmp/sessions.csv"...), 1)}},
		{command: append(wrapPacket([]byte("1,7,a\n2,8,b\n3,9,c\n4,7,d\n5,8,e\n"), 2), wrapPacket(nil, 3)...),
			reply: [][]byte{ok}},
	}
}

func TestRecordV2_AResponseItCannotFrameCostsItsExchangeAlone(t *testing.T) {
	t.Parallel()
	small := kitTraffic(t, []int{7, 8}, false) // 18 exchanges, the 3rd a SELECT
	for _, c := range []struct {
		name string
		// traffic is the connection's exchanges, and the index of the one
		// whose response the recorder cannot frame.
		traffic func(t *testing.T) ([]exchange, int)
		queries int
	}{
		{
			// The reader is at a packet boundary of the 8,209-row result, and
			// the next bytes it gets are the middle of a later row's payload:
			// "0000" read as a header claims a 3,158,064-byte packet.
			name: "bytes lost inside the 8,209-row result",
			traffic: func(t *testing.T) ([]exchange, int) {
				xs := kitTraffic(t, []int{300, 301}, true)
				big := bytes.Join(xs[1].reply, nil)
				at := func(j int) int { return len(bytes.Join(xs[1].reply[:j], nil)) }
				from := at(1 + 6 + 1 + 6000)
				row := at(1 + 6 + 1 + 6400)
				to := row + 4 + len(lenenc("6400")) + len(lenenc("user-6400")) + len(lenenc("user6400@example.com")) + len(lenenc("ACTIVE")) + 3 + 10
				if !bytes.Equal(big[to:to+4], []byte("0000")) {
					t.Fatalf("fixture: the cut does not land in the payload's zeros: %q", big[to:to+8])
				}
				xs[1].reply = [][]byte{cut(big, from, to)}
				return xs, 1
			},
			queries: 19 - 1,
		},
		{
			// Rows 5 to 9 of the second SELECT lost whole: the terminator after
			// them must not end the result with the rows that are left.
			name: "packets lost inside a result",
			traffic: func(t *testing.T) ([]exchange, int) {
				xs := append([]exchange{}, small...)
				sel := xs[2].reply
				xs[2].reply = append(append([][]byte{}, sel[:1+6+1+5]...), sel[1+6+1+10:]...)
				return xs, 2
			},
			queries: 18 - 1,
		},
		{
			// An empty packet where the column count should be.
			name: "a response head that does not decode",
			traffic: func(t *testing.T) ([]exchange, int) {
				xs := append([]exchange{}, small...)
				xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
				return xs, 2
			},
			queries: 18 - 1,
		},
		{
			// What keploy/enterprise#2754's recording stopped at: "the first
			// packet of the COM_QUERY response has sequence id 0 where 1 comes
			// next", a header read one byte before a row's.
			name: "a header read out of row data where a response starts",
			traffic: func(t *testing.T) ([]exchange, int) {
				xs := append([]exchange{}, small...)
				xs[2].reply = append([][]byte{{'5', 0x12, 0x02, 0x00}}, xs[2].reply...)
				return xs, 2
			},
			queries: 18 - 1,
		},
		{
			// COM_FIELD_LIST's reply is a run of column definitions whose length
			// nothing announces.
			name: "a command whose response it cannot size",
			traffic: func(t *testing.T) ([]exchange, int) {
				fieldList := exchange{command: wrapPacket(append([]byte{0x04}, "sessions\x00"...), 0),
					reply: frame(append(sessionColumns(t, "payload"), eofPayload))}
				return insert(small, 3, fieldList), 3
			},
			queries: 18,
		},
		{
			// COM_CHANGE_USER answered with an auth switch, the client's answer
			// to it (sequence id 2) and the server's OK.
			name: "an auth exchange it does not follow",
			traffic: func(t *testing.T) ([]exchange, int) {
				changeUser := exchange{command: wrapPacket(append([]byte{0x11}, "app\x00\x00test\x00"...), 0),
					reply: frame([][]byte{append(append([]byte{0xfe}, "mysql_native_password\x00"...), bytes.Repeat([]byte{0x22}, 20)...)})}
				authOK := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})[0]
				authOK[3] = 3
				authAnswer := exchange{command: wrapPacket(bytes.Repeat([]byte{0x33}, 20), 2), reply: [][]byte{authOK}}
				return insert(small, 3, changeUser, authAnswer), 3
			},
			queries: 18,
		},
		{
			name: "a LOAD DATA LOCAL INFILE",
			traffic: func(t *testing.T) ([]exchange, int) {
				return insert(small, 3, loadDataLocal(t)...), 3
			},
			queries: 18,
		},
	} {
		for _, n := range []int{7, 16384} {
			t.Run(fmt.Sprintf("%s, reads of %d", c.name, n), func(t *testing.T) {
				t.Parallel()
				xs, bad := c.traffic(t)
				rec := recordWire(t, xs, n, true, time.Minute)
				requireRealigned(t, rec, commandAt(t, xs, n, bad), c.queries)
			})
		}
	}
}

// The response left out is reported once, at WARN, with what follows from it:
// the connection's recording goes on. It is not reported as the connection's
// recording stopping, which an operator reads as every later query lost.
//
// Not parallel: the WARN is limited process-wide, and this test counts it.
func TestRecordV2_AResponseLeftOutIsReportedAsSuch(t *testing.T) {
	resetWarnLimiters()
	xs := kitTraffic(t, []int{7}, false)
	xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	rec := recordWire(t, xs, 16384, true, time.Minute)
	requireRealigned(t, rec, commandAt(t, xs, 16384, 2), 9-1)
	warned := rec.logs.FilterMessageSnippet("failed to decode mysql response head; the exchange is left out, and the connection's recording goes on").FilterLevelExact(zapcore.WarnLevel)
	if warned.Len() != 1 {
		t.Fatalf("%d WARN lines for the response left out, want 1", warned.Len())
	}
	if n := rec.logs.FilterMessageSnippet("no longer recorded").Len(); n != 0 {
		t.Fatalf("%d lines say the connection is no longer recorded; its recording went on", n)
	}
}

// A connection that carries nothing after the response left out (a pooled
// connection back in its pool) has the exchange left out at once: the
// recorder does not wait for the client's next command to say so, or the test
// case that sent it would be saved before then, without its mock.
func TestRecordV2_AResponseLeftOutOnAnIdleConnectionIsLeftOutAtOnce(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7}, false)[:3]
	xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	rec := recordWire(t, xs, 16384, false, 2*time.Second)
	if rec.returned {
		t.Fatalf("RecordV2 returned (%v) on a connection still open: it must go on waiting for the client's next command", rec.err)
	}
	if rec.leftOut.count() != 1 {
		t.Fatalf("%d exchanges left out, want 1", rec.leftOut.count())
	}
	if at := rec.leftOut.recordedAt(0); !at.Before(rec.closedAt) {
		t.Fatal("the exchange was left out only once the streams were closed: it must be left out as soon as its response is found unframable")
	}
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response left out", got)
	}
}

// A command the server does not answer (COM_STMT_CLOSE) right after the
// response left out is recorded as it comes, and the rest of the response is
// skipped up to the next command the server answers.
func TestRecordV2_ACommandWithoutAnAnswerAfterAResponseLeftOut(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	stmtClose := exchange{command: wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0, 0}, 0)}
	xs = insert(xs, 3, stmtClose)
	rec := recordWire(t, xs, 16384, true, time.Minute)
	var queries []*models.Mock
	closes := 0
	for _, m := range rec.mocks {
		if m.Spec.Metadata["requestOperation"] == "COM_STMT_CLOSE" {
			closes++
			continue
		}
		queries = append(queries, m)
	}
	if closes != 1 {
		t.Fatalf("%d COM_STMT_CLOSE mocks, want 1", closes)
	}
	rec.mocks = queries
	requireRealigned(t, rec, commandAt(t, xs, 16384, 2), 18-1)
}

// A client that pipelines its commands, sending one before it read all of the
// answer to the one before, gets answers that do not start after their
// commands: the rest of a response it cannot frame cannot be told from the
// next answer, and the recording stops, as it did for every response.
//
// So does a client whose command the capture's clock stamped alike with the
// last byte of the answer before it: that command cannot be told to come after
// the answer.
func TestRecordV2_APipeliningClientsResponseItCannotFrameStopsTheConnection(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// stamp gives the 2nd command its capture time from the 1st command's
		// and the last byte of the answer to it.
		stamp func(command, answered time.Time) time.Time
	}{
		{"a command sent before the answer to the one before arrived", func(command, _ time.Time) time.Time { return command.Add(time.Nanosecond) }},
		{"a command stamped alike with the end of the answer before it", func(_, answered time.Time) time.Time { return answered }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			xs[5].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[5].reply...)
			client, server := onTheWire(xs, 16384)
			client[1].ReadAt = c.stamp(client[0].ReadAt, server[0].ReadAt)
			rec := recordChunks(t, client, server, true, time.Minute)
			requireFramingLost(t, rec)
			if got := queryMocks(rec.mocks); got != 5 {
				t.Fatalf("%d query mocks, want the 5 before the response it could not frame", got)
			}
			// The supervisor counts what the connection carries from the
			// parser's retirement: the window reaches it, past every byte
			// buffered.
			if w, _ := rec.leftOut.last(); w[1].Before(server[len(server)-1].ReadAt) {
				t.Fatalf("the window left out ends at %v, before the last byte the connection carried (%v)", w[1].Format(time.StampMicro), server[len(server)-1].ReadAt.Format(time.StampMicro))
			}
		})
	}
}

// After the response left out, the client's stream is taken up again only at
// a command the client sent on its own, which starts a chunk of the capture:
// one in the middle of a chunk was not sent after the answer before it, and
// the recording stops.
func TestRecordV2_ACommandInTheMiddleOfAChunkIsNotWhereTheClientIsTakenUp(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7}, false)
	load := loadDataLocal(t)
	// The upload and the next command in one chunk.
	load[1].command = append(append([]byte{}, load[1].command...), xs[3].command...)
	load[1].reply = append(append([][]byte{}, load[1].reply...), xs[3].reply...)
	xs = append(append(append([]exchange{}, xs[:3]...), load...), xs[4:]...)
	rec := recordWire(t, xs, 16384, true, time.Minute)
	requireFramingLost(t, rec)
	if got := queryMocks(rec.mocks); got != 3 {
		t.Fatalf("%d query mocks, want the 3 before the LOAD DATA", got)
	}
}

// loadDataUpload is a LOAD DATA LOCAL INFILE whose upload runs over packets
// packets, numbered on from 2 past 255 to 0, with first the first byte of the
// packet the numbering wraps at; then the empty packet that ends the upload,
// and the server's OK.
func loadDataUpload(t *testing.T, packets int, first byte) (query exchange, upload [][]byte, ok []byte) {
	t.Helper()
	query = exchange{command: cannedCOMQuery(t, 0, "LOAD DATA LOCAL INFILE '/tmp/sessions.csv' INTO TABLE sessions"),
		reply: [][]byte{wrapPacket(append([]byte{0xfb}, "/tmp/sessions.csv"...), 1)}}
	seq := byte(2)
	for i := 0; i < packets; i++ {
		p := []byte(fmt.Sprintf("%d,7,abcdefghij\n", i))
		if seq == 0 {
			p = append([]byte{first}, p...)
		}
		upload = append(upload, wrapPacket(p, seq))
		seq++
	}
	upload = append(upload, wrapPacket(nil, seq))
	ok = frame([][]byte{{0x00, 0x05, 0x00, 0x02, 0x00, 0x00, 0x00}})[0]
	ok[3] = seq + 1
	return query, upload, ok
}

// An upload of more than 254 packets numbers on past 255 to 0: that packet is
// more of the upload, not the client's next command, whether the capture read
// the upload whole or a packet at a time, and whatever byte it starts with (a
// binary upload can start one with a command's byte).
func TestRecordV2_ALoadDataUploadOfMoreThan254PacketsIsPassedOver(t *testing.T) {
	t.Parallel()
	small := kitTraffic(t, []int{7, 8}, false)
	for _, c := range []struct {
		name  string
		first byte
		// whole: the capture read the upload in one chunk.
		whole bool
	}{
		{"read whole", '9', true},
		{"read a packet at a time, the wrapped one starting like COM_STMT_CLOSE", mysql.COM_STMT_CLOSE, false},
		{"read a packet at a time, the wrapped one starting like COM_QUERY", mysql.COM_QUERY, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			query, upload, ok := loadDataUpload(t, 300, c.first)
			ld := []exchange{query}
			if c.whole {
				ld = append(ld, exchange{command: bytes.Join(upload, nil), reply: [][]byte{ok}})
			} else {
				for i, p := range upload {
					x := exchange{command: p}
					if i == len(upload)-1 {
						x.reply = [][]byte{ok}
					}
					ld = append(ld, x)
				}
			}
			xs := insert(small, 3, ld...)
			rec := recordWire(t, xs, 16384, true, time.Minute)
			for _, m := range rec.mocks {
				if op := m.Spec.Metadata["requestOperation"]; op != "COM_QUERY" && m.Spec.Metadata["type"] != "config" {
					t.Fatalf("the upload's bytes were recorded as a command: a %s mock", op)
				}
			}
			requireRealigned(t, rec, commandAt(t, xs, 16384, 3), 18)
		})
	}
}

// What the client sends inside the exchange left out (an upload) is input
// consumed, as a command is: a connection that then sits idle must not be
// left with its pending work armed, or the supervisor retires it as hung, and
// what it carries from there is left out with every test case recorded
// meanwhile.
func TestRecordV2_PassingOverTheExchangesClientPacketsClearsPendingWork(t *testing.T) {
	t.Parallel()
	xs := append(kitTraffic(t, []int{7}, false)[:3], loadDataLocal(t)...)
	rec := recordWire(t, xs, 16384, false, 2*time.Second)
	if rec.returned {
		t.Fatalf("RecordV2 returned (%v) on a connection still open", rec.err)
	}
	if n := len(rec.clearedAt); n == 0 || rec.clearedAt[n-1] != rec.clientConsumed {
		t.Fatalf("pending work last cleared with %v client bytes consumed, of %d: the upload's bytes left it armed on an idle connection", rec.clearedAt, rec.clientConsumed)
	}
}

// A packet whose length ran over the rest of the response and into the next
// answer, found not to decode where a chunk ends: the reader is past the next
// command's capture time, and an answer after it would be taken for its. The
// recording stops instead, as for a pipelining client, with nothing recorded
// wrong.
//
// Not parallel: the WARN is limited process-wide, and this test counts it.
func TestRecordV2_AResponseReadPastTheNextAnswerStopsTheConnection(t *testing.T) {
	resetWarnLimiters()
	xs := kitTraffic(t, []int{7, 8}, false)
	r2, r3 := bytes.Join(xs[2].reply, nil), bytes.Join(xs[3].reply, nil)
	if len(r2)+len(r3)+5 > 16384 {
		t.Fatalf("fixture: the replies do not fit a chunk each: %d, %d", len(r2), len(r3))
	}
	// Sequence id 1, a length that runs over the rest of reply 2 and all of
	// reply 3, and a head that does not decode (0xFB).
	n := 1 + len(r2) + len(r3)
	xs[2].reply = [][]byte{append([]byte{byte(n), byte(n >> 8), byte(n >> 16), 1, 0xfb}, r2...)}
	rec := recordWire(t, xs, 16384, true, time.Minute)
	requireFramingLost(t, rec)
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response it could not frame", got)
	}
	if n := rec.logs.FilterMessageSnippet("the connection is no longer recorded").FilterLevelExact(zapcore.WarnLevel).Len(); n != 1 {
		t.Fatalf("%d WARN lines saying the connection is no longer recorded, want 1", n)
	}
}

// A command the server does not answer (COM_STMT_CLOSE), sent with the next
// command in one write after the response left out: the client is in step,
// and both are recorded.
func TestRecordV2_ACommandWithoutAnAnswerSentWithTheNextOne(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	xs[3].command = append(wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0, 0}, 0), xs[3].command...)
	rec := recordWire(t, xs, 16384, true, time.Minute)
	var queries []*models.Mock
	closes := 0
	for _, m := range rec.mocks {
		if m.Spec.Metadata["requestOperation"] == "COM_STMT_CLOSE" {
			closes++
			continue
		}
		queries = append(queries, m)
	}
	if closes != 1 {
		t.Fatalf("%d COM_STMT_CLOSE mocks, want 1", closes)
	}
	rec.mocks = queries
	requireRealigned(t, rec, commandAt(t, xs, 16384, 2), 18-1)
}

// A PREPARE held on an assumed framing (a connection joined mid-stream) when
// the response after it cannot be framed: what would settle its framing is
// skipped with that response, so it is left out too, and the recording goes
// on.
func TestRecordV2_PostTLS_APrepareHeldWhenTheNextResponseCannotBeFramed(t *testing.T) {
	t.Parallel()
	def := func(name string) []byte {
		return columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: name, OrgName: name, FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	}
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t WHERE a = ? AND b = ?"...), 0),
		prepareOK(true, [][]byte{def("?"), def("?")}, nil))
	// COM_FIELD_LIST: a reply it cannot frame.
	j.exchange(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), frame([][]byte{def("id"), eofPayload}))
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v", err)
	}
	if len(mocks) != 1 || describe(mocks[0]) != "a OK" {
		t.Fatalf("%d mocks, want SELECT 1's OK alone", len(mocks))
	}
	if leftOut.count() != 2 {
		t.Fatalf("%d exchanges left out, want the PREPARE and the COM_FIELD_LIST", leftOut.count())
	}
}

// The capture's clock stamped the next answer alike with the command it
// answers: the rest of the response left out cannot be told from that answer,
// and the recording stops, with the command's exchange left out.
func TestRecordV2_AnAnswerStampedAlikeWithItsCommandStopsTheConnection(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	client, server := onTheWire(xs, 16384)
	// The answer to the 4th command (its first chunk), stamped with the command.
	for i, c := range server {
		if c.ReadAt.After(client[3].ReadAt) {
			server[i].ReadAt = client[3].ReadAt
			break
		}
	}
	rec := recordChunks(t, client, server, true, time.Minute)
	requireFramingLost(t, rec)
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response it could not frame", got)
	}
	if got := rec.leftOut.count(); got != 2 {
		t.Fatalf("%d exchanges left out, want the one it could not frame and the one the recording stopped in", got)
	}
}
