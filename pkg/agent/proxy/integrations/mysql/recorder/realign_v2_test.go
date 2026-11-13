package recorder

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
		// stops: bytes lost from the server's stream leave the packet walk
		// in the middle of a packet at the cut (packetWalk), so the cut cannot
		// be shown to start the next answer: the recording stops there,
		// naming it, with no wrong mock. A capture that loses bytes ends the
		// stream at the hole, so a stream that lost them unsaid is not one a
		// capture hands the recorder; these are what a header misread
		// looks like.
		stops bool
	}{
		{
			// The reader is at a packet boundary of the 8,209-row result, and
			// the next bytes it gets are the middle of a later row's payload:
			// "0000" read as a header claims a 3,158,064-byte packet.
			name:  "bytes lost inside the 8,209-row result",
			stops: true,
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
			name:  "a header read out of row data where a response starts",
			stops: true,
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
				if c.stops {
					requireFramingLost(t, rec)
					if !strings.Contains(rec.err.Error(), "do not show the response left out ended at the cut: it falls inside a packet") {
						t.Fatalf("RecordV2 = %v, want the stop to name the cut inside a packet", rec.err)
					}
					return
				}
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
// next answer, and the recording stops, as it did for every response. The
// capture's order says so (fakeconn.Chunk.ConnSeq): the command was captured
// before the last byte of the answer before it, whatever the clock stamped.
func TestRecordV2_APipeliningClientsResponseItCannotFrameStopsTheConnection(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// stamp gives the 2nd command its capture time from the 1st command's
		// and the last byte of the answer to it.
		stamp func(command, answered time.Time) time.Time
	}{
		{"stamped before the answer to the one before arrived", func(command, _ time.Time) time.Time { return command.Add(time.Nanosecond) }},
		{"stamped after the answer to the one before arrived", func(_, answered time.Time) time.Time { return answered.Add(time.Microsecond) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			xs[5].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[5].reply...)
			client, server := onTheWire(xs, 16384)
			// The 2nd command was captured before the answer to the 1st.
			client[1].ConnSeq, server[0].ConnSeq = server[0].ConnSeq, client[1].ConnSeq
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

// A client that waits for each answer is not taken for one that pipelines on
// the capture's clock: a command stamped alike with the last byte of the
// answer before it, or before it (a clock that is coarse, or steps back), or
// not stamped at all, was still captured after that byte
// (fakeconn.Chunk.ConnSeq). Nor is one numbered alike with that byte: only a
// command numbered before it was sent before it. So a response of its that
// cannot be framed costs its own exchange, and the recording goes on. Taken
// for pipelining, the connection was no longer recorded from that response
// on: on one recording, dozens of pooled connections stopped so.
func TestRecordV2_ACommandStampedNotAfterTheAnswerBeforeItIsNotPipelining(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// mark changes the 2nd command's chunk (command) against the chunk
		// the answer to the 1st ends in (answer).
		mark func(command *fakeconn.Chunk, first, answer fakeconn.Chunk)
	}{
		{"stamped alike with the end of the answer before it", func(command *fakeconn.Chunk, _, answer fakeconn.Chunk) { command.ReadAt = answer.ReadAt }},
		{"stamped before the answer before it arrived", func(command *fakeconn.Chunk, first, _ fakeconn.Chunk) {
			command.ReadAt = first.ReadAt.Add(time.Nanosecond)
		}},
		{"not stamped", func(command *fakeconn.Chunk, _, _ fakeconn.Chunk) { command.ReadAt = time.Time{} }},
		{"numbered alike with the end of the answer before it", func(command *fakeconn.Chunk, _, answer fakeconn.Chunk) { command.ConnSeq = answer.ConnSeq }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			xs[5].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[5].reply...)
			client, server := onTheWire(xs, 16384)
			if !server[0].CapturedBefore(client[1].ConnSeq) {
				t.Fatal("fixture: the 2nd command must be numbered after the answer to the 1st")
			}
			c.mark(&client[1], client[0], server[0])
			rec := recordChunks(t, client, server, true, time.Minute)
			requireRealigned(t, rec, commandAt(t, xs, 16384, 5), 18-1)
		})
	}
}

// A command is sent from its first byte on: one whose first chunk was captured
// before the last byte of the answer before it was sent before the client
// could have read that answer whole, though the chunk it ends in was captured
// after. The client pipelines, and the response it cannot frame stops the
// recording, as for a command captured whole before that byte.
func TestRecordV2_ACommandBegunBeforeTheAnswerBeforeItEndedIsPipelining(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	xs[5].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[5].reply...)
	client, server := onTheWire(xs, 16384)
	// Room between every two capture numbers, for a chunk numbered between.
	for i := range client {
		client[i].ConnSeq *= 2
	}
	for i := range server {
		server[i].ConnSeq *= 2
	}
	// The 2nd command in two chunks: its first captured before the answer to
	// the 1st (one chunk), its last after it, as onTheWire numbered it whole.
	head, tail := client[1], client[1]
	head.Bytes, tail.Bytes = client[1].Bytes[:2], client[1].Bytes[2:]
	head.ConnSeq, head.ReadAt = server[0].ConnSeq-1, server[0].ReadAt.Add(-time.Nanosecond)
	if !client[0].CapturedBefore(head.ConnSeq) || !head.CapturedBefore(server[0].ConnSeq) || !server[0].CapturedBefore(tail.ConnSeq) {
		t.Fatal("fixture: the 2nd command must start after the 1st and before the answer to it, and end after that answer")
	}
	client = append([]fakeconn.Chunk{client[0], head, tail}, client[2:]...)
	rec := recordChunks(t, client, server, true, time.Minute)
	requireFramingLost(t, rec)
	if got := queryMocks(rec.mocks); got != 5 {
		t.Fatalf("%d query mocks, want the 5 before the response it could not frame", got)
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
	// The query after SELECT 1 confirms it, taken up after the COM_FIELD_LIST
	// left out (takenUp): the end of the connection would not.
	j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{cannedOK(t, 1, j.caps)})
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v", err)
	}
	if len(mocks) != 2 || describe(mocks[0]) != "a OK" || describe(mocks[1]) != "a OK" {
		t.Fatalf("%d mocks, want the OKs of SELECT 1 and SELECT 2 alone", len(mocks))
	}
	if leftOut.count() != 2 {
		t.Fatalf("%d exchanges left out, want the PREPARE and the COM_FIELD_LIST", leftOut.count())
	}
}

// A PREPARE taken up after a response left out, and held for its framing
// besides, when the client's stream ends with no command after it: nothing
// confirms the answer taken as its own (takenUp), so it is left out with its
// framing unsettled, and the end of the server's stream does not settle it
// into a mock (heldPrepare.end).
func TestRecordV2_PostTLS_APrepareTakenUpAndHeldIsLeftOutAtTheClientsEnd(t *testing.T) {
	t.Parallel()
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	// COM_FIELD_LIST: a reply it cannot frame.
	j.exchange(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), frame([][]byte{columnPayload(t, undecodableColumns[0]), eofPayload}))
	// Columns only, no EOF (CLIENT_DEPRECATE_EOF): held, its framing open.
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t"...), 0),
		prepareOK(true, nil, [][]byte{columnPayload(t, undecodableColumns[0])}))
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v", err)
	}
	if n := len(mocksOf(mocks, "COM_STMT_PREPARE")); n != 0 {
		t.Fatalf("recorded %d PREPAREs taken up after a response left out with nothing to confirm their answer", n)
	}
	if len(mocks) != 1 || describe(mocks[0]) != "a OK" {
		t.Fatalf("%d mocks, want SELECT 1's OK alone", len(mocks))
	}
	if leftOut.count() != 2 {
		t.Fatalf("%d exchanges left out, want the COM_FIELD_LIST and the PREPARE", leftOut.count())
	}
}

// A PREPARE held on an assumed framing and then left out takes its statement
// with it: an EXECUTE of that statement cannot replay without the PREPARE's
// mock, so it is left out as well, a CLOSE of it is recorded without its
// query, as for a statement prepared before the recording began, and the
// command after them is recorded.
func TestRecordV2_PostTLS_AnExecuteOfAHeldPrepareLeftOutIsLeftOut(t *testing.T) {
	t.Parallel()
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	// Columns only, no EOF (CLIENT_DEPRECATE_EOF): held, its framing open.
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t"...), 0),
		prepareOK(true, nil, [][]byte{columnPayload(t, undecodableColumns[0])}))
	// COM_FIELD_LIST: a reply it cannot frame, so the PREPARE is left out.
	j.exchange(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), frame([][]byte{columnPayload(t, undecodableColumns[0]), eofPayload}))
	j.exchange(stmtExecuteCommand(0, undecodableStmtID), [][]byte{okResponseWithAffectedRows(1, 1)})
	j.exchange(stmtCloseCommand(0, undecodableStmtID), nil)
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v", err)
	}
	if n := len(mocksOf(mocks, "COM_STMT_EXECUTE")); n != 0 {
		t.Fatalf("recorded %d executes of a PREPARE left out: they cannot replay", n)
	}
	requireStamped(t, mocks, []string{""})
	if len(mocks) != 2 || len(mocksOf(mocks, "COM_QUERY")) != 1 {
		t.Fatalf("%d mocks, want the CLOSE and SELECT 1's OK", len(mocks))
	}
	if leftOut.count() != 3 {
		t.Fatalf("%d exchanges left out, want the PREPARE, the COM_FIELD_LIST and the EXECUTE", leftOut.count())
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

// intoTheTail lays out the 18 exchanges of kitTraffic(7, 8) with the client's
// 4th command (exchange 3) pipelined into the tail of the 3rd answer: sent
// after packet sentAfter of that answer reached the capture, before the rest
// of it, each packet a chunk of its own, then tail (the bytes the server sent
// after the command and before its answer), then the 4th answer, captured
// before the 5th command, as a client that waits for each answer from then on
// sends it. An empty packet stands where the 3rd answer's column count should
// be: a response the recorder cannot frame.
func intoTheTail(t *testing.T, sentAfter int, tail [][]byte) (client, server []fakeconn.Chunk) {
	t.Helper()
	laid, xs := upToTheTail(t, sentAfter, tail)
	laid = append(laid, exchange{reply: [][]byte{bytes.Join(xs[3].reply, nil)}})
	laid = append(laid, xs[4:]...)
	return onTheWire(laid, 1<<20)
}

// upToTheTail is intoTheTail's layout up to the tail, and the 18 exchanges it
// lays out.
func upToTheTail(t *testing.T, sentAfter int, tail [][]byte) (laid, xs []exchange) {
	t.Helper()
	xs = kitTraffic(t, []int{7, 8}, false) // 18 exchanges, the 3rd a SELECT
	reply := append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	if sentAfter < 4 || sentAfter >= len(reply)-1 {
		t.Fatalf("fixture: the command is sent after packet %d of %d", sentAfter, len(reply))
	}
	laid = append([]exchange{}, xs[:2]...)
	for j, p := range reply[:sentAfter+1] {
		x := exchange{reply: [][]byte{p}}
		if j == 0 {
			x.command = xs[2].command
		}
		laid = append(laid, x)
	}
	for j, p := range tail {
		x := exchange{reply: [][]byte{p}}
		if j == 0 {
			x.command = xs[3].command
		}
		laid = append(laid, x)
	}
	return laid, xs
}

// requireUnconfirmedStop asserts the recording stopped at the answer taken up
// after a response left out, which the capture's order did not confirm: no
// wrong mock, the error naming it, the stop stamped once, and the exchanges
// before the response left out recorded.
func requireUnconfirmedStop(t *testing.T, rec recording, want string) {
	t.Helper()
	requireFramingLost(t, rec)
	if !strings.Contains(rec.err.Error(), "the answer to the command taken up after a response left out is not confirmed") || !strings.Contains(rec.err.Error(), want) {
		t.Fatalf("RecordV2 = %v, want the stop to name the answer taken up that is not confirmed, %s", rec.err, want)
	}
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response left out", got)
	}
}

// A client that pipelines its next command into the tail of an answer the
// recorder left out (it cannot be framed), though it had not pipelined before, has the rest of that answer captured after its
// command: the re-align takes the server's first chunk captured after the
// command for the start of its answer. A tail packet there whose sequence id is
// not 1 is no answer's first: the exchange taken up is not confirmed, and the
// recording stops at it, with no wrong mock (takenUp). Going on, it was left
// out alone in turn, and the next re-align was the same guess.
func TestRecordV2_ACommandPipelinedIntoTheTailOfAnAnswerLeftOutStops(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	reply := append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	// Sent after the column definitions and 5 rows: the tail is the other rows
	// and the terminator, numbered on from there.
	at := 1 + 6 + 1 + 5
	tail := reply[at+1:]
	if tail[0][3] == 1 {
		t.Fatal("fixture: the tail's first packet has sequence id 1")
	}
	client, server := intoTheTail(t, at, tail)
	rec := recordChunks(t, client, server, true, time.Minute)
	requireUnconfirmedStop(t, rec, "where 1 comes next")
}

// The same, where the tail's first packet after the command has sequence id 1
// (the sequence wraps every 256 packets) and frames whole as an answer to the
// command (an OK), and the command's own answer follows, captured before the
// client's next command, as a client that waits for the answer it reads sends
// it. Recorded at once, the command had the tail's packet for its answer: a
// wrong mock. Held, the server's next bytes after it, captured before the
// client's next command, do not confirm it: the recording stops there, with no
// wrong mock.
func TestRecordV2_ATailThatFramesAsTheAnswerTakenUpIsNotConfirmed(t *testing.T) {
	t.Parallel()
	ok := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})
	client, server := intoTheTail(t, 1+6+1+5, ok)
	rec := recordChunks(t, client, server, true, time.Minute)
	requireUnconfirmedStop(t, rec, "the server's next bytes after it were captured before the client's next command")
}

// Nor does the end of the server's stream confirm the answer taken up: a
// capture that loses bytes of a stream (a capture hole) ends it there, with
// the io.EOF the connection's end reads, and what it lost may be the rest of
// the answer the re-align cut into. Here the tail's packet at sequence id 1
// frames as an OK, and then the server's stream ends: the client goes on with
// its next queries (the capture lost the server's answers to them), or ends
// too. Confirmed by that end, the command was recorded with the tail's packet:
// a wrong mock. It is left out, with no wrong mock: the recording stops at the
// client's next query, naming the end, or ends with the client's stream.
func TestRecordV2_TheEndOfTheServersStreamDoesNotConfirmTheAnswerTakenUp(t *testing.T) {
	t.Parallel()
	ok := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})
	for _, clientGoesOn := range []bool{true, false} {
		t.Run(fmt.Sprintf("clientGoesOn=%v", clientGoesOn), func(t *testing.T) {
			t.Parallel()
			laid, xs := upToTheTail(t, 1+6+1+5, ok)
			if clientGoesOn {
				for _, x := range xs[4:6] {
					laid = append(laid, exchange{command: x.command})
				}
			}
			client, server := onTheWire(laid, 1<<20)
			rec := recordChunks(t, client, server, true, time.Minute)
			if clientGoesOn {
				requireUnconfirmedStop(t, rec, "the server's stream ended before its next bytes after it")
				return
			}
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
			}
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 = %v (returned %v), want nil at the end of the connection", rec.err, rec.returned)
			}
			if got := queryMocks(rec.mocks); got != 2 {
				t.Fatalf("%d query mocks, want the 2 before the response left out", got)
			}
			if got := rec.leftOut.count(); got != 2 {
				t.Fatalf("%d exchanges left out, want the one it could not frame and the one taken up after it", got)
			}
			if n := rec.logs.FilterFieldKey("reason").Filter(func(e observer.LoggedEntry) bool {
				r, _ := e.ContextMap()["reason"].(string)
				return strings.Contains(r, "the client's stream ended with no command after it whose answer would confirm it")
			}).Len(); n != 1 {
				t.Fatalf("%d exchanges left out for the end of the client's stream, want the one taken up", n)
			}
		})
	}
}

// A client that pipelines two deep into the tail of an answer the recorder
// left out (it cannot be framed), though it had not pipelined before, has the
// command after that answer (C2) and the one after it (C3) both captured
// before the answer's tail. The re-align cuts the server's stream at C2 by
// capture time: in the tail. Held until C3, a tail packet that frames as an
// answer to C2 was confirmed by the tail's next bytes, captured after C3, and
// C2 was recorded with the tail's bytes. The server's packet headers show
// where the answer left out ended (packetWalk): the cut is taken only where
// the packet before it is not one a packet at sequence id 1 is numbered on
// from, and is a packet of the answer left out. Each case ends the recording
// at C2, naming why, with no wrong mock and the 2 exchanges before the answer
// left out recorded.
func TestRecordV2_TwoDeepPipeliningIntoTheTailOfAnAnswerLeftOut(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false) // 18 exchanges, the 3rd a SELECT
	ok := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})[0]
	at := func(p []byte, id byte) []byte {
		p = append([]byte{}, p...)
		p[3] = id
		return p
	}
	// rows is the packets of an answer the recorder cannot frame (an empty
	// packet where its column count should be), numbered from 1: through
	// 255, 0, and an OK at 1, then on to 3.
	var wrapped [][]byte
	for id := 1; id <= 258; id++ {
		switch {
		case id == 1:
			wrapped = append(wrapped, wrapPacket(nil, 1))
		case id == 257:
			wrapped = append(wrapped, at(ok, 1))
		default:
			wrapped = append(wrapped, wrapPacket([]byte("row"), byte(id)))
		}
	}
	notWrapped := append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	// split is notWrapped with its 14th packet in two chunks.
	split := append(append(append([][]byte{}, notWrapped[:13]...), notWrapped[13][:6], notWrapped[13][6:]), notWrapped[14:]...)
	fieldList := wrapPacket(append([]byte{0x04}, "sessions\x00"...), 0)
	errAnswer := frame([][]byte{append([]byte{0xff, 0x7a, 0x04, '#'}, "42S02no such table"...)})
	query, upload, uploadOK := loadDataUpload(t, 254, '9')
	for _, c := range []struct {
		name string
		// command is the one whose answer the recorder leaves out, answer
		// its packets (each a chunk), and client what the client sends
		// inside that exchange; C2 is sent after answer[c2], C3 after
		// answer[c3] (-1: before the first).
		command []byte
		answer  [][]byte
		client  [][]byte
		c2, c3  int
		want    string
	}{
		{name: "the cut where the run wraps, an OK at 1 after it", command: xs[2].command, answer: wrapped,
			c2: 255, c3: 256, want: "the last packet before it has sequence id 0, so a packet at 1"},
		{name: "the cut in the run", command: xs[2].command, answer: notWrapped,
			c2: 12, c3: 13, want: "where 1 comes next"},
		{name: "the cut inside a packet", command: xs[2].command, answer: split,
			c2: 13, c3: 14, want: "it falls inside a packet"},
		{name: "the answer left out all after the cut", command: fieldList, answer: errAnswer,
			c2: -1, c3: 0, want: "no packet of the response left out came before it"},
		{name: "an upload whose last packet is at 0, the cut before the OK", command: query.command,
			answer: append(append([][]byte{}, query.reply...), uploadOK), client: upload,
			c2: 0, c3: 1, want: "the client sent a packet at sequence id 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			laid := append([]exchange{}, xs[:2]...)
			laid = append(laid, exchange{command: c.command})
			for _, p := range c.client {
				laid = append(laid, exchange{command: p})
			}
			if c.c2 < 0 {
				laid = append(laid, exchange{command: xs[3].command})
			}
			for j, p := range c.answer {
				laid = append(laid, exchange{reply: [][]byte{p}})
				switch j {
				case c.c2:
					laid = append(laid, exchange{command: xs[3].command})
				case c.c3:
					laid = append(laid, exchange{command: xs[4].command})
				}
			}
			laid = append(laid, exchange{reply: [][]byte{bytes.Join(xs[3].reply, nil)}}, exchange{reply: [][]byte{bytes.Join(xs[4].reply, nil)}})
			laid = append(laid, xs[5:]...)
			client, server := onTheWire(laid, 1<<20)
			rec := recordChunks(t, client, server, true, time.Minute)
			requireFramingLost(t, rec)
			if !strings.Contains(rec.err.Error(), c.want) {
				t.Fatalf("RecordV2 = %v, want the stop to name %q", rec.err, c.want)
			}
			if got := queryMocks(rec.mocks); got != 2 {
				t.Fatalf("%d query mocks, want the 2 before the answer left out", got)
			}
		})
	}
}
