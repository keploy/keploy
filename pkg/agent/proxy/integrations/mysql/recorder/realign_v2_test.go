package recorder

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
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
// skipped up to the next command the server answers. The CLOSE starts no answer
// on the server's stream (packetWalk.mark): when the response left out was read
// whole before it (here one packet), its packets still show the cut at the
// next command where it ended.
func TestRecordV2_ACommandWithoutAnAnswerAfterAResponseLeftOut(t *testing.T) {
	t.Parallel()
	for _, readWhole := range []bool{false, true} {
		t.Run(fmt.Sprintf("readWhole=%v", readWhole), func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
			if readWhole {
				xs[2].reply = xs[2].reply[:1]
			}
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
		})
	}
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
		{"not stamped", func(command *fakeconn.Chunk, _, _ fakeconn.Chunk) {
			command.ReadAt, command.WrittenAt = time.Time{}, time.Time{}
		}},
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
			// A chunk left with no capture time reaches the recorder with none
			// (recordChunksStamped).
			rec := recordChunksStamped(t, client, server, true, time.Minute, false)
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
// the response after it cannot be framed: the first packet of that response
// settles the PREPARE's framing before the response is left out (no EOF came:
// the client negotiated CLIENT_DEPRECATE_EOF), so the PREPARE is recorded, the
// response alone is left out, and the recording goes on. A response of one
// packet (an ERR) is walked from the mark too, though settling it read it: the
// re-align's cut sees it as the response left out.
func TestRecordV2_PostTLS_APrepareHeldWhenTheNextResponseCannotBeFramed(t *testing.T) {
	t.Parallel()
	def := func(name string) []byte {
		return columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: name, OrgName: name, FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	}
	for name, reply := range map[string][][]byte{
		"definitions": frame([][]byte{def("id"), eofPayload}),
		"an ERR":      {errReply(1, 1146, "Table 't' doesn't exist")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			leftOut := &orphanSpans{}
			j.h.sess.Orphans = leftOut
			j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t WHERE a = ? AND b = ?"...), 0),
				prepareOK(true, [][]byte{def("?"), def("?")}, nil))
			// COM_FIELD_LIST: a reply it cannot frame.
			j.exchange(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), reply)
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
			// The query after SELECT 1 confirms it, taken up after the
			// COM_FIELD_LIST left out (takenUp): the end of the connection
			// would not.
			j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{cannedOK(t, 1, j.caps)})
			mocks, err := j.run(t)
			if err != nil {
				t.Fatalf("RecordV2 = %v", err)
			}
			if got, want := strings.Join(pairings(mocks), "; "), "COM_STMT_PREPARE -> COM_STMT_PREPARE_OK; SELECT 1 -> OK 0; SELECT 2 -> OK 0"; got != want {
				t.Fatalf("mocks %q, want %q", got, want)
			}
			if leftOut.count() != 1 {
				t.Fatalf("%d exchanges left out, want the COM_FIELD_LIST", leftOut.count())
			}
		})
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
	logs := j.observe()
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
	// Left out as the exchange taken up that nothing confirms, at the
	// client's end, not later as a PREPARE whose framing the recording's end
	// did not settle.
	if n := leftOutFor(logs, "the client's stream ended with no command after it whose answer would confirm it"); n != 1 {
		t.Fatalf("%d exchanges left out for the end of the client's stream, want the PREPARE taken up", n)
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
	// COM_FIELD_LIST: a reply it cannot frame, whose first packet is at
	// neither the PREPARE's EOF's sequence id nor the one an answer starts at,
	// so it does not settle the PREPARE's framing: the PREPARE is left out.
	j.exchange(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), [][]byte{wrapPacket(columnPayload(t, undecodableColumns[0]), 5), wrapPacket(eofPayload, 6)})
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
// answers. The re-align's cut is by capture number (the command's floor), so
// the stamp says nothing there: the answer, numbered after the command, is
// its answer, and only the exchange it could not frame is left out.
func TestRecordV2_AnAnswerStampedAlikeWithItsCommandIsItsAnswer(t *testing.T) {
	t.Parallel()
	xs := unframableThird(t)
	client, server := onTheWire(xs, 16384)
	// The answer to the 4th command (its first chunk), stamped with the command.
	for i, c := range server {
		if c.ReadAt.After(client[3].ReadAt) {
			server[i].ReadAt = client[3].ReadAt
			break
		}
	}
	rec := recordChunks(t, client, server, true, time.Minute)
	requireRealigned(t, rec, commandAt(t, xs, 16384, 2), 18-1)
}

// A capture clock coarser than the connection's traffic stamps every chunk,
// of both directions, alike. The re-align's cut and its checks are by capture
// number, so it records what a fine clock records: every exchange but the one
// it cannot frame. Cut by capture time, the recording stopped at the cut: no
// chunk was placed before or after the command taken up.
func TestRecordV2_ACoarseClockDoesNotStopTheReAlign(t *testing.T) {
	t.Parallel()
	for _, n := range []int{7, 16384} {
		t.Run(fmt.Sprintf("reads of %d", n), func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			xs[5].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[5].reply...)
			client, server := onTheWire(xs, n)
			for i := range client {
				client[i].ReadAt = wireBase
			}
			for i := range server {
				server[i].ReadAt = wireBase
			}
			rec := recordChunks(t, client, server, true, time.Minute)
			requireRealigned(t, rec, wireBase, 18-1)
		})
	}
}

// Two exchanges in a row whose responses the recorder cannot frame, from a
// client that waits for each answer: two LOAD DATA LOCAL INFILE (the server's
// request for the file, the client's upload, the server's OK), two SELECTs
// whose column count is an empty packet, and such a SELECT followed by one whose
// second packet is off its sequence. The second is the command taken
// up after the first is left out, and its answer's first packet is at sequence
// id 1, where its answer starts: the packet walk proved the cut, and the fault
// after that packet is that answer's own framing. Each is left out alone, and
// the recording goes on, with no wrong mock. Stopped at the second, the
// connection's recording lost every exchange after it.
func TestRecordV2_TwoResponsesItCannotFrameInARowAreEachLeftOutAlone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		traffic func(t *testing.T) []exchange
		queries int
	}{
		{"two LOAD DATA LOCAL INFILE", func(t *testing.T) []exchange {
			return insert(kitTraffic(t, []int{7, 8}, false), 3, append(loadDataLocal(t), loadDataLocal(t)...)...)
		}, 18},
		{"two SELECTs with an empty column count", func(t *testing.T) []exchange {
			xs := unframableThird(t)
			xs[3].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[3].reply...)
			return xs
		}, 18 - 2},
		// The second's first packet passes, and its next is off its sequence:
		// the fault is in a later packet, past the one that proved the cut.
		{"a SELECT with an empty column count, then one whose second packet is off its sequence", func(t *testing.T) []exchange {
			xs := unframableThird(t)
			reply := append([][]byte(nil), xs[3].reply...)
			reply[1] = append([]byte(nil), reply[1]...)
			reply[1][3] = 9
			xs[3].reply = reply
			return xs
		}, 18 - 2},
	} {
		for _, n := range []int{7, 16384} {
			t.Run(fmt.Sprintf("%s, reads of %d", c.name, n), func(t *testing.T) {
				t.Parallel()
				rec := recordWire(t, c.traffic(t), n, true, time.Minute)
				if w := wrongMocks(t, rec.mocks); len(w) > 0 {
					t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
				}
				if !rec.returned || rec.err != nil {
					t.Fatalf("RecordV2 returned=%v err=%v: the second response in a row it cannot frame must cost its own exchange, not the rest of the connection's recording", rec.returned, rec.err)
				}
				if got := queryMocks(rec.mocks); got != c.queries {
					t.Fatalf("%d query mocks, want %d: every exchange but the two left out", got, c.queries)
				}
				if got := rec.leftOut.count(); got != 2 {
					t.Fatalf("%d exchanges left out, want the 2 it could not frame", got)
				}
				if rec.stops != 0 {
					t.Fatalf("the connection's stop was stamped %d times, want never: its recording went on", rec.stops)
				}
			})
		}
	}
}

// A response the recorder cannot frame because its first packet is off its
// sequence (an empty packet at sequence id 2), then a command it has no
// decoder for (COM_FIELD_LIST), taken up after it. That command's fault is
// found before a byte of its answer is read, so no packet of that answer was
// walked, and nothing says where it starts: the left-out response's first
// packet, at 2, is not that answer's. Each is left out alone, and the
// recording goes on, with no wrong mock. Judged by the left-out response's
// first packet, the command taken up stopped the connection's recording.
func TestRecordV2_ACommandTakenUpWithNoAnswerReadAfterAResponseStartedOffItsSequenceIsLeftOutAlone(t *testing.T) {
	t.Parallel()
	for _, n := range []int{7, 16384} {
		t.Run(fmt.Sprintf("reads of %d", n), func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false) // 18 exchanges
			xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x02}}, xs[2].reply...)
			fieldList := exchange{command: wrapPacket(append([]byte{0x04}, "sessions\x00"...), 0),
				reply: frame(append(sessionColumns(t, "payload"), eofPayload))}
			xs = insert(xs, 3, fieldList)
			rec := recordWire(t, xs, n, true, time.Minute)
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
			}
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 returned=%v err=%v: the command taken up with no answer read must cost its own exchange, not the rest of the connection's recording", rec.returned, rec.err)
			}
			if got := queryMocks(rec.mocks); got != 18-1 {
				t.Fatalf("%d query mocks, want 17: every exchange but the two left out", got)
			}
			if spans, counted := rec.leftOut.count(), rec.mgr.MocksLeftOut(); spans != 2 || counted != 2 {
				t.Fatalf("%d exchanges left out, %d counted; want the 2 it could not frame, each once", spans, counted)
			}
			if rec.stops != 0 {
				t.Fatalf("the connection's stop was stamped %d times, want never: its recording went on", rec.stops)
			}
		})
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
	// The exchange the recording stops in is the one taken up: the window left
	// out at the stop holds when its command was sent, so the test case that
	// sent it is left out with it.
	xs := kitTraffic(t, []int{7, 8}, false)
	var sentAt time.Time
	for _, c := range client {
		if bytes.Equal(c.Bytes, xs[3].command) {
			sentAt = c.ReadAt
		}
	}
	if sentAt.IsZero() {
		t.Fatal("fixture: the command taken up is not on the wire")
	}
	if w, _ := rec.leftOut.last(); w[0].After(sentAt) {
		t.Fatalf("the window left out at the stop starts at %v, after the command taken up was sent (%v)", w[0].Format(time.StampMicro), sentAt.Format(time.StampMicro))
	}
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
// capture number: in the tail. Held until C3, a tail packet that frames as an
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
		switch id {
		case 1:
			wrapped = append(wrapped, wrapPacket(nil, 1))
		case 257:
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
		// stampedAfter: answer[c2], numbered before C2, is stamped after it
		// (a clock that does not agree with the capture's order). The
		// command's floor drops it at the cut all the same, and the walk
		// counts it there.
		stampedAfter bool
	}{
		{name: "the cut where the run wraps, an OK at 1 after it", command: xs[2].command, answer: wrapped,
			c2: 255, c3: 256, want: "the last packet before it has sequence id 0, so a packet at 1"},
		{name: "the cut where the run wraps, its packet at 0 stamped after the command", command: xs[2].command, answer: wrapped,
			c2: 255, c3: 256, want: "the last packet before it has sequence id 0, so a packet at 1", stampedAfter: true},
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
			if c.stampedAfter {
				c2 := firstAfter(t, server, client[2]) + c.c2
				i := firstAfter(t, client, server[c2])
				if !bytes.Equal(server[c2].Bytes, c.answer[c.c2]) || !bytes.Equal(client[i].Bytes, xs[3].command) {
					t.Fatal("fixture: the packet before C2 is not where it is laid out")
				}
				server[c2].ReadAt = client[i].ReadAt.Add(time.Nanosecond)
			}
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

// The client's next command after the exchange taken up confirms it however
// short the command is: a COM_PING is one byte. On a connection that ends
// after the ping, the exchange taken up is recorded, confirmed at the ping;
// held on to the end, it was left out.
func TestRecordV2_APingConfirmsTheAnswerTakenUp(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	ping := exchange{command: wrapPacket([]byte{mysql.COM_PING}, 0), reply: frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})}
	laid := append(append([]exchange{}, xs[:4]...), ping)
	rec := recordWire(t, laid, 16384, true, time.Minute)
	pings := 0
	var queries []*models.Mock
	for _, m := range rec.mocks {
		if m.Spec.Metadata["requestOperation"] == "COM_PING" {
			pings++
			continue
		}
		queries = append(queries, m)
	}
	if pings != 1 {
		t.Fatalf("%d COM_PING mocks, want 1", pings)
	}
	rec.mocks = queries
	requireRealigned(t, rec, commandAt(t, laid, 16384, 2), 3)
}

// A command without an answer (COM_STMT_CLOSE) sent after the exchange taken
// up after a response left out is recorded behind it, in the order sent, once
// the next answer confirms it; and when the client's stream ends with the
// exchange taken up unconfirmed, it is left out and the CLOSE recorded still:
// a command without an answer has no answer to be paired wrong (takenUp).
func TestRecordV2_ACommandWithoutAnAnswerAfterTheAnswerTakenUpIsRecordedBehindIt(t *testing.T) {
	t.Parallel()
	stmtClose := exchange{command: wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0, 0}, 0)}
	for _, clientEnds := range []bool{false, true} {
		t.Run(fmt.Sprintf("clientEnds=%v", clientEnds), func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
			laid := insert(xs, 4, stmtClose)
			if clientEnds {
				laid = laid[:5]
			}
			rec := recordWire(t, laid, 16384, true, time.Minute)
			at := -1
			var queries []*models.Mock
			for i, m := range rec.mocks {
				if m.Spec.Metadata["requestOperation"] == "COM_STMT_CLOSE" {
					if at >= 0 {
						t.Fatal("2 COM_STMT_CLOSE mocks, want 1")
					}
					at = i
					continue
				}
				queries = append(queries, m)
			}
			if !clientEnds {
				// The handshake's, the 2 exchanges' before the response left
				// out, the one taken up, then the CLOSE sent after it.
				if at != 4 {
					t.Fatalf("the COM_STMT_CLOSE is mock %d of %d, want mock 4: behind the exchange taken up, as it was sent", at, len(rec.mocks))
				}
				rec.mocks = queries
				requireRealigned(t, rec, commandAt(t, laid, 16384, 2), 18-1)
				return
			}
			if at < 0 {
				t.Fatal("no COM_STMT_CLOSE mock: the command without an answer queued behind the exchange taken up is lost at the client's end")
			}
			if w := wrongMocks(t, queries); len(w) > 0 {
				t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
			}
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 = %v (returned %v), want nil at the end of the connection", rec.err, rec.returned)
			}
			if got := queryMocks(queries); got != 2 {
				t.Fatalf("%d query mocks, want the 2 before the response left out", got)
			}
			if got := rec.leftOut.count(); got != 2 {
				t.Fatalf("%d exchanges left out, want the one it could not frame and the one taken up after it", got)
			}
		})
	}
}

// A command without an answer (COM_STMT_CLOSE) does not confirm the exchange
// taken up after a response left out: the server sends nothing for it, so the
// server's next bytes after it say nothing of where the answer taken up
// ended. Here the tail of the response left out frames as the answer (the
// sequence wraps to 1), the client sends a CLOSE, and the command's own answer
// follows, captured after the CLOSE and before the client's next query.
// Confirmed at the CLOSE, the command was recorded with the tail's packet: a
// wrong mock. The next query does not confirm it, and the recording stops
// there, with no wrong mock.
func TestRecordV2_ACommandWithoutAnAnswerDoesNotConfirmTheAnswerTakenUp(t *testing.T) {
	t.Parallel()
	ok := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})
	laid, xs := upToTheTail(t, 1+6+1+5, ok)
	laid = append(laid, exchange{command: wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0, 0}, 0)})
	laid = append(laid, exchange{reply: [][]byte{bytes.Join(xs[3].reply, nil)}})
	laid = append(laid, xs[4:]...)
	client, server := onTheWire(laid, 1<<20)
	rec := recordChunks(t, client, server, true, time.Minute)
	// The CLOSE queued behind the exchange taken up is recorded at the stop:
	// it has no answer to be paired wrong.
	var queries []*models.Mock
	for _, m := range rec.mocks {
		if m.Spec.Metadata["requestOperation"] != "COM_STMT_CLOSE" {
			queries = append(queries, m)
		}
	}
	if n := len(rec.mocks) - len(queries); n != 1 {
		t.Fatalf("%d COM_STMT_CLOSE mocks, want 1", n)
	}
	rec.mocks = queries
	requireUnconfirmedStop(t, rec, "the server's next bytes after it were captured before the client's next command")
}

// A command whose first chunk is numbered past half the number space (a
// connection that carried that many chunks) is not taken for pipelining when
// no answer was read before it: there is no answer's last byte to compare its
// number with (fakeconn.Chunk.CapturedBefore against no number is a wrapped
// comparison). Taken for it, the response it cannot frame stopped the
// recording; here it costs its exchange alone.
func TestRecordV2_PostTLS_ACommandWithNoAnswerReadBeforeItIsNotPipelining(t *testing.T) {
	t.Parallel()
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	j.h.captured.Store(1 << 31)
	j.exchange(stmtCloseCommand(0, 7), nil)
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	// COM_FIELD_LIST: a reply it cannot frame.
	j.exchange(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), frame([][]byte{columnPayload(t, undecodableColumns[0]), eofPayload}))
	j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{cannedOK(t, 1, j.caps)})
	j.exchange(cannedCOMQuery(t, 0, "SELECT 3"), [][]byte{cannedOK(t, 1, j.caps)})
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v: a command with no answer read before it is not pipelining", err)
	}
	if n := len(mocksOf(mocks, "COM_QUERY")); n != 3 || len(mocks) != 4 {
		t.Fatalf("%d mocks (%d queries), want the CLOSE and the 3 queries' OKs", len(mocks), n)
	}
	if leftOut.count() != 1 {
		t.Fatalf("%d exchanges left out, want the COM_FIELD_LIST", leftOut.count())
	}
}

// observe makes the joined connection log to an observer, and returns it.
func (j *joinedMidStream) observe() *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	j.h.logger = zap.New(core)
	j.h.sess.Logger = j.h.logger
	return logs
}

// leftOutFor counts the exchanges logged as left out for a reason holding why.
func leftOutFor(logs *observer.ObservedLogs, why string) int {
	return logs.FilterFieldKey("reason").Filter(func(e observer.LoggedEntry) bool {
		r, _ := e.ContextMap()["reason"].(string)
		return strings.Contains(r, why)
	}).Len()
}

// A PREPARE taken up after a response left out and held for its framing, when
// the server's next bytes after it were captured before the client's next
// command: the answer taken as the PREPARE's is not confirmed (takenUp), and
// the recording stops there, with the PREPARE left out once, in the exchange
// the recording stops in, not again as a PREPARE whose framing nothing
// settled.
func TestRecordV2_PostTLS_APrepareTakenUpAndHeldThatIsNotConfirmedIsLeftOutOnce(t *testing.T) {
	t.Parallel()
	j := newJoinedMidStream(t)
	logs := j.observe()
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
	// COM_FIELD_LIST: a reply it cannot frame.
	j.exchange(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), frame([][]byte{columnPayload(t, undecodableColumns[0]), eofPayload}))
	// Columns only, no EOF (CLIENT_DEPRECATE_EOF): held, its framing open.
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t"...), 0),
		prepareOK(true, nil, [][]byte{columnPayload(t, undecodableColumns[0])}))
	j.h.pushDest(cannedOK(t, 1, j.caps), j.at())
	j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{cannedOK(t, 1, j.caps)})
	mocks, err := j.run(t)
	if !errors.Is(err, ErrFramingLost) || !strings.Contains(err.Error(), "the answer to the command taken up after a response left out is not confirmed") {
		t.Fatalf("RecordV2 = %v, want the stop at the answer taken up that is not confirmed", err)
	}
	if n := len(mocksOf(mocks, "COM_STMT_PREPARE")); n != 0 {
		t.Fatalf("recorded %d PREPAREs whose answer taken up is not confirmed", n)
	}
	if len(mocks) != 1 || describe(mocks[0]) != "a OK" {
		t.Fatalf("%d mocks, want SELECT 1's OK alone", len(mocks))
	}
	if leftOut.count() != 2 {
		t.Fatalf("%d exchanges left out, want the COM_FIELD_LIST and the PREPARE the recording stopped in", leftOut.count())
	}
	if n := leftOutFor(logs, "settled the"); n != 0 {
		t.Fatalf("the PREPARE was left out %d more times as one whose framing nothing settled", n)
	}
}

// A post-TLS stream joined mid-connection whose first command has no answer
// (COM_STMT_CLOSE) has had no server byte read when the next command comes: no
// response read whole puts the packet walk at a packet boundary yet
// (packetWalk), as for the first command's own response. A response the
// recorder cannot frame there stops the recording, naming it, as it does
// for the first command's: the server's first bytes on a stream joined
// mid-way may be the rest of an answer sent before it, and a walk anchored at
// them follows the wrong headers to the cut.
//
// The same after server bytes the floor dropped before a second command
// without an answer: dropped, not read, they show no boundary either.
func TestRecordV2_PostTLS_AResponseLeftOutBeforeAnyWasReadWholeStopsAfterACommandWithoutAnAnswer(t *testing.T) {
	t.Parallel()
	for _, dropped := range []bool{false, true} {
		t.Run(fmt.Sprintf("dropped=%v", dropped), func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			leftOut := &orphanSpans{}
			j.h.sess.Orphans = leftOut
			closes := 1
			if dropped {
				// The rest of an answer sent before the stream was joined,
				// captured before the second CLOSE.
				j.exchange(stmtCloseCommand(0, 7), [][]byte{cannedOK(t, 1, j.caps)[2:]})
				j.exchange(stmtCloseCommand(0, 8), nil)
				closes = 2
			} else {
				j.exchange(stmtCloseCommand(0, 7), nil)
			}
			// COM_FIELD_LIST: a reply it cannot frame.
			j.exchange(wrapPacket(append([]byte{0x04}, "t\x00"...), 0), frame([][]byte{columnPayload(t, undecodableColumns[0]), eofPayload}))
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{cannedOK(t, 1, j.caps)})
			j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{cannedOK(t, 1, j.caps)})
			mocks, err := j.run(t)
			if !errors.Is(err, ErrFramingLost) || !strings.Contains(err.Error(), "the server's packets were not followed from a packet boundary to it") {
				t.Fatalf("RecordV2 = %v, want the stop at the cut no response read whole anchors", err)
			}
			if len(mocks) != closes || len(mocksOf(mocks, "COM_STMT_CLOSE")) != closes {
				t.Fatalf("%d mocks, want the %d CLOSE(s) alone", len(mocks), closes)
			}
		})
	}
}

// unframableThird is kitTraffic(7, 8) with the 3rd answer one the recorder
// cannot frame (an empty packet where its column count should be): the 4th
// command is the one taken up after it, the 5th the one that confirms it.
func unframableThird(t *testing.T) []exchange {
	t.Helper()
	xs := kitTraffic(t, []int{7, 8}, false)
	xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
	return xs
}

// firstAfter is the index of the first server chunk numbered after c.
func firstAfter(t *testing.T, server []fakeconn.Chunk, c fakeconn.Chunk) int {
	t.Helper()
	for i, s := range server {
		if c.CapturedBefore(s.ConnSeq) {
			return i
		}
	}
	t.Fatal("fixture: no server chunk numbered after the command")
	return -1
}

// The re-align's cut is by capture number: a chunk numbered after the command
// taken up but stamped before it (a clock that does not agree with the
// capture's order) is the command's own answer, as its number says. Cut away
// by its stamp with the rest of the response left out, the command was paired
// with the next answer the server sent. It is recorded with its own answer.
func TestRecordV2_AnAnswerNumberedAfterTheCommandTakenUpButStampedBeforeItIsItsAnswer(t *testing.T) {
	t.Parallel()
	xs := unframableThird(t)
	client, server := onTheWire(xs, 16384)
	i := firstAfter(t, server, client[3])
	server[i].ReadAt = client[3].ReadAt.Add(-time.Nanosecond)
	rec := recordChunks(t, client, server, true, time.Minute)
	requireRealigned(t, rec, commandAt(t, xs, 16384, 2), 18-1)
}

// A command taken up that reached the capture in two chunks, with the server's
// next chunk after the cut numbered between them and stamped after the second:
// by the capture's order the server sent it before it had the whole command,
// so it is not proven to be the command's answer, whatever its stamp says.
// Taken for it, the command was recorded with bytes the order does not pair
// with it. The recording stops at the command taken up, naming why.
func TestRecordV2_AnAnswerNumberedBeforeTheEndOfTheCommandTakenUpStops(t *testing.T) {
	t.Parallel()
	xs := unframableThird(t)
	command := xs[3].command
	laid := append([]exchange{}, xs[:3]...)
	laid = append(laid, exchange{command: command[:10]}, exchange{reply: [][]byte{bytes.Join(xs[3].reply, nil)}}, exchange{command: command[10:]})
	laid = append(laid, xs[4:]...)
	client, server := onTheWire(laid, 1<<20)
	i := firstAfter(t, server, client[3])
	if !server[i].CapturedBefore(client[4].ConnSeq) {
		t.Fatal("fixture: the answer is not numbered between the command's two chunks")
	}
	server[i].ReadAt = client[4].ReadAt.Add(time.Nanosecond)
	rec := recordChunks(t, client, server, true, time.Minute)
	requireFramingLost(t, rec)
	if !strings.Contains(rec.err.Error(), "were not captured after the command taken up") {
		t.Fatalf("RecordV2 = %v, want the stop to name the server's next bytes numbered before the command's end", rec.err)
	}
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response left out", got)
	}
}

// The server's next chunk after the cut that cannot be placed (no capture
// number: fakeconn.ErrUnnumbered) is not the answer to the command taken up
// found: the recording stops at that command as lost framing, naming it.
func TestRecordV2_AnUnnumberedChunkWhereTheAnswerTakenUpStartsStops(t *testing.T) {
	t.Parallel()
	client, server := onTheWire(unframableThird(t), 16384)
	server[firstAfter(t, server, client[3])].ConnSeq = 0
	rec := recordPushed(t, client, server, true, time.Minute, true)
	requireFramingLost(t, rec)
	if !strings.Contains(rec.err.Error(), "the answer to the command taken up after a response left out cannot be found") {
		t.Fatalf("RecordV2 = %v, want the stop to name the answer taken up that cannot be found", rec.err)
	}
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response left out", got)
	}
}

// A client that sends its next command before the answer taken up ended (it
// pipelined into it, though it had not before) is not one whose next command
// waits for the answer it reads: the server's next bytes after that answer
// confirm nothing of where it started. The exchange taken up is left out and
// the recording stops at that command, naming why, though the server's next
// bytes were captured after the command.
func TestRecordV2_ACommandPipelinedIntoTheAnswerTakenUpStops(t *testing.T) {
	t.Parallel()
	xs := unframableThird(t)
	k := 1 + 6 + 1 + 5 // its column count, definitions, EOF and 5 rows
	laid := append([]exchange{}, xs[:3]...)
	laid = append(laid,
		exchange{command: xs[3].command, reply: [][]byte{bytes.Join(xs[3].reply[:k], nil)}},
		exchange{command: xs[4].command, reply: [][]byte{bytes.Join(xs[3].reply[k:], nil)}},
		exchange{reply: [][]byte{bytes.Join(xs[4].reply, nil)}})
	laid = append(laid, xs[5:]...)
	client, server := onTheWire(laid, 1<<20)
	rec := recordChunks(t, client, server, true, time.Minute)
	requireUnconfirmedStop(t, rec, "the client sent its next command before the answer taken up ended")
}

// A client that sent its next command before the first byte of the response
// left out reached the capture, and the command after that before the next
// answer: it pipelines from the cut. The response's first packet, the one the
// recorder cannot frame, is a chunk of its own, and the rest of the response
// starts at sequence id 1, as an answer does. Bytes the recorder read of that
// response were captured after the command, so that command's answer is not
// the server's first bytes captured after it, whatever the packets after them
// look like: the recording stops at the command, naming why, with the 2
// queries before the response recorded and no wrong mock. Taking the next
// chunk for its answer recorded the command with the rows of the response
// left out.
func TestRecordV2_ACommandSentBeforeTheBytesReadOfTheResponseLeftOutStops(t *testing.T) {
	t.Parallel()
	xs := unframableThird(t)
	laid := append([]exchange{}, xs[:2]...)
	laid = append(laid,
		exchange{command: xs[2].command},
		exchange{command: xs[3].command},
		exchange{reply: xs[2].reply[:1]},
		exchange{reply: [][]byte{bytes.Join(xs[2].reply[1:], nil)}},
		exchange{command: xs[4].command},
		exchange{reply: [][]byte{bytes.Join(xs[3].reply, nil)}},
		exchange{reply: [][]byte{bytes.Join(xs[4].reply, nil)}})
	laid = append(laid, xs[5:]...)
	client, server := onTheWire(laid, 1<<20)
	rec := recordChunks(t, client, server, true, time.Minute)
	requireFramingLost(t, rec)
	if !strings.Contains(rec.err.Error(), "the server's bytes read before the answer to the command taken up after a response left out were not all captured before that command") {
		t.Fatalf("RecordV2 = %v, want the stop to name the bytes read that were captured after the command", rec.err)
	}
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response left out", got)
	}
}

// The server's stream after the answer taken up that cannot be read (a chunk
// with no capture number: fakeconn.ErrUnnumbered) confirms nothing: the
// exchange taken up is left out, and the recording stops there, naming why.
func TestRecordV2_AnUnreadableServerStreamDoesNotConfirmTheAnswerTakenUp(t *testing.T) {
	t.Parallel()
	client, server := onTheWire(unframableThird(t), 16384)
	server[firstAfter(t, server, client[4])].ConnSeq = 0
	rec := recordPushed(t, client, server, true, time.Minute, true)
	requireUnconfirmedStop(t, rec, "the server's stream after it cannot be read")
}

// A recording that stops after the answer taken up was read whole, before the
// client's next command that the server answers confirmed it (here client
// bytes lost: a packet at sequence id 3 where a command must start), leaves
// that exchange out, counted: neither recorded unconfirmed, nor lost from the
// count of what the recording left out.
func TestRecordV2_AStopBeforeTheAnswerTakenUpIsConfirmedLeavesItOut(t *testing.T) {
	t.Parallel()
	xs := unframableThird(t)
	xs[4].command = append([]byte{}, xs[4].command...)
	xs[4].command[3] = 3
	rec := recordWire(t, xs, 16384, true, time.Minute)
	requireFramingLost(t, rec)
	if !strings.Contains(rec.err.Error(), "where a command (sequence id 0) must start") {
		t.Fatalf("RecordV2 = %v, want the stop at the client's packet out of sequence", rec.err)
	}
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response left out", got)
	}
	if n := leftOutFor(rec.logs, "the recording ended before the next answer confirmed the answer taken up"); n != 1 {
		t.Fatalf("%d exchanges left out as taken up and not confirmed at the stop, want 1", n)
	}
	if got := rec.leftOut.count(); got != 3 {
		t.Fatalf("%d exchanges left out, want the one it could not frame, the one taken up, and the one the recording stopped in", got)
	}
}

// withoutCloses is mocks with its COM_STMT_CLOSE mocks taken out, of which
// there must be want (recorded behind the exchange taken up, as they were
// sent): a CLOSE's mock is the client's alone, which no answer can make wrong.
func withoutCloses(t *testing.T, mocks []*models.Mock, want int) []*models.Mock {
	t.Helper()
	var rest []*models.Mock
	for _, m := range mocks {
		if m.Spec.Metadata["requestOperation"] != "COM_STMT_CLOSE" {
			rest = append(rest, m)
		}
	}
	if n := len(mocks) - len(rest); n != want {
		t.Fatalf("%d COM_STMT_CLOSE mocks, want %d: each one queued behind the exchange taken up is recorded", n, want)
	}
	return rest
}

// The same stop with a command without an answer (COM_STMT_CLOSE) queued
// behind the exchange taken up: the exchange taken up is left out, and the
// CLOSE is recorded all the same. Its mock is the client's alone, which no
// answer can make wrong.
func TestRecordV2_AStopBeforeTheAnswerTakenUpIsConfirmedRecordsTheCommandsQueuedBehindIt(t *testing.T) {
	t.Parallel()
	xs := insert(unframableThird(t), 4, exchange{command: wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0, 0}, 0)})
	xs[5].command = append([]byte{}, xs[5].command...)
	xs[5].command[3] = 3
	rec := recordWire(t, xs, 16384, true, time.Minute)
	rec.mocks = withoutCloses(t, rec.mocks, 1)
	requireFramingLost(t, rec)
	if !strings.Contains(rec.err.Error(), "where a command (sequence id 0) must start") {
		t.Fatalf("RecordV2 = %v, want the stop at the client's packet out of sequence", rec.err)
	}
	if n := leftOutFor(rec.logs, "the recording ended before the next answer confirmed the answer taken up"); n != 1 {
		t.Fatalf("%d exchanges left out as taken up and not confirmed at the stop, want 1", n)
	}
	if got := queryMocks(rec.mocks); got != 2 {
		t.Fatalf("%d query mocks, want the 2 before the response left out", got)
	}
}

// After the answer taken up was read whole, a command without an answer that
// does not decode (a COM_STMT_CLOSE one byte short) is left out alone where it
// is read: it leaves nothing on the server's stream to skip, so nothing is
// re-aligned, and the server's next answer confirms the exchange taken up as
// it does after a CLOSE that decodes. The exchange taken up is recorded, with
// the CLOSE queued behind it; only the exchange it could not frame and the
// CLOSE that does not decode are left out, both counted.
func TestRecordV2_ACommandLeftOutAfterTheAnswerTakenUpLeavesItHeld(t *testing.T) {
	t.Parallel()
	for _, size := range []int{7, 16384} {
		t.Run(fmt.Sprintf("reads of %d", size), func(t *testing.T) {
			t.Parallel()
			xs := insert(unframableThird(t), 4,
				exchange{command: wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0, 0}, 0)},
				exchange{command: wrapPacket([]byte{mysql.COM_STMT_CLOSE, 2, 0, 0}, 0)})
			rec := recordWire(t, xs, size, true, time.Minute)
			rec.mocks = withoutCloses(t, rec.mocks, 1)
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
			}
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 returned=%v err=%v: a command left out after the answer taken up must not cost the rest of the connection's recording", rec.returned, rec.err)
			}
			if n := leftOutFor(rec.logs, "is not confirmed"); n != 0 {
				t.Fatalf("%d exchanges left out as taken up and not confirmed: the server's next answer confirms it past a command it does not answer", n)
			}
			if n := leftOutFor(rec.logs, "a COM_STMT_CLOSE that does not decode"); n != 1 {
				t.Fatalf("%d exchanges left out as a CLOSE that does not decode, want 1", n)
			}
			if got := rec.leftOut.count(); got != 2 {
				t.Fatalf("%d exchanges left out, want the one it could not frame and the CLOSE that does not decode", got)
			}
			if got := queryMocks(rec.mocks); got != 18-1 {
				t.Fatalf("%d query mocks, want %d: every exchange but the one it could not frame", got, 18-1)
			}
		})
	}
}

// pairings names each mock as "query -> response", an OK with its affected
// rows and an ERR with its code, in the order recorded.
func pairings(mocks []*models.Mock) []string {
	var out []string
	for _, m := range mocks {
		q := m.Spec.Metadata["requestOperation"]
		if len(m.Spec.MySQLRequests) > 0 {
			if qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); ok {
				q = qp.Query
			}
		}
		r := m.Spec.Metadata["responseOperation"]
		if len(m.Spec.MySQLResponses) == 1 {
			switch p := m.Spec.MySQLResponses[0].Message.(type) {
			case *mysql.OKPacket:
				r = fmt.Sprintf("OK %d", p.AffectedRows)
			case *mysql.ERRPacket:
				r = fmt.Sprintf("ERR %d", p.ErrorCode)
			}
		}
		out = append(out, q+" -> "+r)
	}
	return out
}

// afterLeftOut pushes a command whose answer is reply, then SELECT 2, 3 and 4,
// answered with OKs of 7, 8 and 9 rows. pipelined: the client sent SELECT 2
// before reply, and SELECT 3 after reply and before SELECT 2's answer, as a
// client does that took reply for SELECT 2's.
func (j *joinedMidStream) afterLeftOut(t *testing.T, command, reply []byte, pipelined bool) {
	t.Helper()
	j.h.pushClient(command, j.at())
	if pipelined {
		j.h.pushClient(cannedCOMQuery(t, 0, "SELECT 2"), j.at())
		j.h.pushDest(reply, j.at())
		j.h.pushClient(cannedCOMQuery(t, 0, "SELECT 3"), j.at())
		j.h.pushDest(okResponseWithAffectedRows(1, 7), j.at())
		j.h.pushDest(okResponseWithAffectedRows(1, 8), j.at())
	} else {
		j.h.pushDest(reply, j.at())
		j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{okResponseWithAffectedRows(1, 7)})
		j.exchange(cannedCOMQuery(t, 0, "SELECT 3"), [][]byte{okResponseWithAffectedRows(1, 8)})
	}
	j.exchange(cannedCOMQuery(t, 0, "SELECT 4"), [][]byte{okResponseWithAffectedRows(1, 9)})
}

// errReply is an ERR packet at seq with code, as a server answers a command
// it rejects.
func errReply(seq byte, code uint16, msg string) []byte {
	return wrapPacket(append([]byte{0xff, byte(code), byte(code >> 8), '#', 'H', 'Y', '0', '0', '0'}, msg...), seq)
}

// A PREPARE held for its framing (post-TLS, parameters only) whose server sent
// the EOF after its definitions, then a command whose answer the recorder
// cannot frame, found before it reads a byte of it (COM_FIELD_LIST). The held
// PREPARE is settled at that command, before its answer is left out: its EOF
// is read as the PREPARE's, and the walk marks the answer's start after it.
// Left unsettled, the EOF was walked as a packet of the answer left out, so a
// client that sent SELECT 2 before COM_FIELD_LIST's answer (an ERR) and SELECT
// 3 after it had SELECT 2 recorded with that ERR and SELECT 3 with SELECT 2's
// answer. Now that client's connection stops at the cut, as the walk saw no
// packet of the answer left out before SELECT 2; one that waited for the ERR
// has every query recorded with its own answer, and the PREPARE with its EOF.
func TestRecordV2_PostTLS_AHeldPrepareIsSettledBeforeAnAnswerItCannotFrameIsLeftOut(t *testing.T) {
	t.Parallel()
	for _, pipelined := range []bool{true, false} {
		t.Run(fmt.Sprintf("SELECT 2 sent before the ERR %v", pipelined), func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0),
				prepareOK(false, [][]byte{columnPayload(t, undecodableColumns[0])}, nil))
			j.afterLeftOut(t, wrapPacket([]byte{mysql.COM_FIELD_LIST, 't', 0}, 0), errReply(1, 1146, "Table 't' doesn't exist"), pipelined)
			mocks, err := j.run(t)
			got := strings.Join(pairings(mocks), "; ")
			want := "SELECT 1 -> OK 1; COM_STMT_PREPARE -> COM_STMT_PREPARE_OK"
			for _, m := range mocks {
				if sp, ok := m.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket); ok && sp.EOFAfterParamDefs == nil {
					t.Fatal("the PREPARE was recorded without the EOF its server sent after its definitions")
				}
			}
			if !pipelined {
				want += "; SELECT 2 -> OK 7; SELECT 3 -> OK 8; SELECT 4 -> OK 9"
			}
			if got != want {
				t.Fatalf("mocks %q, want %q (RecordV2 = %v)", got, want, err)
			}
			if pipelined && (!errors.Is(err, ErrFramingLost) || !strings.Contains(err.Error(), "no packet of the response left out came before it")) {
				t.Fatalf("RecordV2 = %v, want the stop at the cut: no packet of COM_FIELD_LIST's answer came before SELECT 2", err)
			}
			if !pipelined && err != nil {
				t.Fatalf("RecordV2 = %v, want no stop: SELECT 2 was sent after COM_FIELD_LIST's answer", err)
			}
		})
	}
}

// A command without an answer that does not decode (a COM_STMT_CLOSE one byte
// short) leaves nothing on the server's stream for the recorder to skip: it is
// left out alone where it is read, and both streams are read on as its client
// reads them. As the connection's first command, it stopped the connection at
// a re-align's cut, the walk having seen no answer since the handshake, and
// none of the 18 queries after it was recorded.
func TestRecordV2_ACommandWithoutAnAnswerThatDoesNotDecodeFirstIsLeftOutAlone(t *testing.T) {
	t.Parallel()
	for _, size := range []int{7, 16384} {
		t.Run(fmt.Sprintf("reads of %d", size), func(t *testing.T) {
			t.Parallel()
			xs := insert(kitTraffic(t, []int{7, 8}, false), 0, exchange{command: wrapPacket([]byte{mysql.COM_STMT_CLOSE, 2, 0, 0}, 0)})
			rec := recordWire(t, xs, size, true, time.Minute)
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s); the first: %s", len(w), w[0])
			}
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 returned=%v err=%v, want no stop: no response was left out", rec.returned, rec.err)
			}
			if got := queryMocks(rec.mocks); got != 18 {
				t.Fatalf("%d query mocks, want all 18", got)
			}
			if got := rec.leftOut.count(); got != 1 {
				t.Fatalf("%d exchanges left out, want the CLOSE that does not decode", got)
			}
		})
	}
}

// malformedClose is a COM_STMT_CLOSE one byte short, and malformedLongData a
// COM_STMT_SEND_LONG_DATA without its parameter id: commands without an answer
// that do not parse. MySQL 5.7 and later answer each with malformedERR;
// MariaDB and MySQL 5.6 send nothing.
var (
	malformedClose    = wrapPacket([]byte{mysql.COM_STMT_CLOSE, 2, 0, 0}, 0)
	malformedLongData = wrapPacket([]byte{mysql.COM_STMT_SEND_LONG_DATA, 2, 0, 0, 0}, 0)
	malformedERR      = errReply(1, 1835, "Malformed communication packet")
)

// runCounted runs j with a mock manager that counts the exchanges left out.
func (j *joinedMidStream) runCounted(t *testing.T) ([]*models.Mock, int64, error) {
	t.Helper()
	mgr := syncMock.New(nil)
	j.h.sess.Mgr = mgr
	mocks, err := j.run(t)
	return mocks, mgr.MocksLeftOut(), err
}

// A server that answers a command without an answer that does not parse (MySQL
// 5.7 and later: an ERR 1835, malformed packet) sent that ERR before the
// client's next command was captured: it is that command's answer, proven by
// the capture's order, and is left out with it. Every later command is
// recorded with its own answer. Recorded as its client read it, as before,
// SELECT 2 had the ERR and every command after it its predecessor's answer,
// for the rest of the connection. Two in a row are answered in order; a
// command taken up after a response left out is confirmed by the ERR to the
// next one, sent after its answer; a PREPARE held for its framing is settled
// before the ERR is read.
func TestRecordV2_AnERRToACommandWithoutAnAnswerIsThatCommandsAnswer(t *testing.T) {
	t.Parallel()
	unframable := [][]byte{wrapPacket(nil, 1), wrapPacket([]byte{0x01, 0x02}, 2), wrapPacket([]byte{0x03}, 3)}
	prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0)
	one := [][]byte{columnPayload(t, undecodableColumns[0])}
	cases := []struct {
		name    string
		build   func(t *testing.T, j *joinedMidStream)
		want    string
		leftOut int64
	}{
		{"one", func(t *testing.T, j *joinedMidStream) {
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.exchange(malformedClose, [][]byte{malformedERR})
		}, "SELECT 1 -> OK 1; SELECT 2 -> OK 2; SELECT 3 -> OK 3", 1},
		{"two in a row", func(t *testing.T, j *joinedMidStream) {
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.h.pushClient(malformedClose, j.at())
			j.h.pushClient(malformedLongData, j.at())
			j.h.pushDest(malformedERR, j.at())
			j.h.pushDest(malformedERR, j.at())
		}, "SELECT 1 -> OK 1; SELECT 2 -> OK 2; SELECT 3 -> OK 3", 2},
		{"after an exchange taken up", func(t *testing.T, j *joinedMidStream) {
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.exchange(cannedCOMQuery(t, 0, "SELECT u"), unframable)
			j.exchange(cannedCOMQuery(t, 0, "SELECT 9"), [][]byte{okResponseWithAffectedRows(1, 9)})
			j.exchange(malformedClose, [][]byte{malformedERR})
		}, "SELECT 1 -> OK 1; SELECT 9 -> OK 9; SELECT 2 -> OK 2; SELECT 3 -> OK 3", 2},
		{"with a response left out before it", func(t *testing.T, j *joinedMidStream) {
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.exchange(cannedCOMQuery(t, 0, "SELECT u"), unframable)
			j.exchange(malformedClose, [][]byte{malformedERR})
		}, "SELECT 1 -> OK 1; SELECT 2 -> OK 2; SELECT 3 -> OK 3", 2},
		{"after a PREPARE held, its EOF sent", func(t *testing.T, j *joinedMidStream) {
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.exchange(prepare, prepareOK(false, one, nil))
			j.exchange(malformedClose, [][]byte{malformedERR})
		}, "SELECT 1 -> OK 1; PREPARE -> PREPARE_OK eof=true; SELECT 2 -> OK 2; SELECT 3 -> OK 3", 1},
		{"after a PREPARE held, no EOF", func(t *testing.T, j *joinedMidStream) {
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.exchange(prepare, prepareOK(true, one, nil))
			j.exchange(malformedClose, [][]byte{malformedERR})
		}, "SELECT 1 -> OK 1; PREPARE -> PREPARE_OK eof=false; SELECT 2 -> OK 2; SELECT 3 -> OK 3", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			c.build(t, j)
			j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{okResponseWithAffectedRows(1, 2)})
			j.exchange(cannedCOMQuery(t, 0, "SELECT 3"), [][]byte{okResponseWithAffectedRows(1, 3)})
			mocks, leftOut, err := j.runCounted(t)
			if err != nil {
				t.Fatalf("RecordV2 = %v, want no stop", err)
			}
			if got := strings.Join(prepareOKPairing(mocks), "; "); got != c.want {
				t.Fatalf("mocks %q, want %q: each command with its own answer", got, c.want)
			}
			if leftOut != c.leftOut {
				t.Fatalf("%d exchanges left out, want %d: each command that does not decode, with its ERR, and the response left out", leftOut, c.leftOut)
			}
		})
	}
}

// A server that answers nothing to a command without an answer that does not
// parse (MariaDB, MySQL 5.6): the server's next packet is the next command's
// answer, an OK whose first bytes after its header would read as an ERR 1835's
// code (43 rows, last insert id 7) or an ERR of another code, and is recorded
// as that command's.
func TestRecordV2_ACommandWithoutAnAnswerThatAServerLeavesUnansweredLeavesTheNextAnswerAlone(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string][]byte{
		"an OK":       wrapPacket([]byte{0x00, 0x2b, 0x07, 0x02, 0x00, 0x00, 0x00}, 1),
		"an ERR 1064": errReply(1, 1064, "You have an error in your SQL syntax"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.h.pushClient(malformedClose, j.at())
			j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{answer})
			want := "SELECT 1 -> OK 1; SELECT 2 -> OK 43; SELECT 3 -> OK 3"
			if answer[4] == 0xff {
				want = "SELECT 1 -> OK 1; SELECT 2 -> ERR 1064; SELECT 3 -> OK 3"
			}
			j.exchange(cannedCOMQuery(t, 0, "SELECT 3"), [][]byte{okResponseWithAffectedRows(1, 3)})
			mocks, err := j.run(t)
			if err != nil {
				t.Fatalf("RecordV2 = %v, want no stop", err)
			}
			if got := strings.Join(pairings(mocks), "; "); got != want {
				t.Fatalf("mocks %q, want %q", got, want)
			}
		})
	}
}

// An ERR 1835 captured after the client's next command may answer the command
// without an answer that did not parse (MySQL 5.7 and later, a client that did
// not wait) or that next command (a server that answers nothing to the first,
// and the next did not parse either): nothing tells which, so the recording
// stops there, the stop named, with no command recorded with another's answer.
// One captured before the command it would answer is no answer to it: the
// recording stops too. The states the recorder can be in at it: a command
// taken up after a response left out, a re-align pending, a PREPARE held for
// its framing with and without its EOF, and none.
func TestRecordV2_AnERRThatCannotBeToldToAnswerACommandWithoutAnAnswerStops(t *testing.T) {
	t.Parallel()
	unframable := [][]byte{wrapPacket(nil, 1), wrapPacket([]byte{0x01, 0x02}, 2), wrapPacket([]byte{0x03}, 3)}
	prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0)
	q := func(t *testing.T, k int) []byte { return cannedCOMQuery(t, 0, fmt.Sprintf("SELECT %d", k)) }
	ok := func(k int) []byte { return okResponseWithAffectedRows(1, byte(k)) }
	cases := map[string]func(t *testing.T, j *joinedMidStream){
		"none": func(t *testing.T, j *joinedMidStream) {},
		"taken up": func(t *testing.T, j *joinedMidStream) {
			j.exchange(cannedCOMQuery(t, 0, "SELECT u"), unframable)
			j.exchange(q(t, 2), [][]byte{ok(2)})
		},
		"re-align pending": func(t *testing.T, j *joinedMidStream) {
			j.exchange(cannedCOMQuery(t, 0, "SELECT u"), unframable)
		},
		"PREPARE held, its EOF sent": func(t *testing.T, j *joinedMidStream) {
			j.exchange(prepare, prepareOK(false, [][]byte{columnPayload(t, undecodableColumns[0])}, nil))
		},
		"PREPARE held, no EOF": func(t *testing.T, j *joinedMidStream) {
			j.exchange(prepare, prepareOK(true, [][]byte{columnPayload(t, undecodableColumns[0])}, nil))
		},
		"captured before the command": func(t *testing.T, j *joinedMidStream) {
			j.exchange(prepare, prepareOK(true, [][]byte{columnPayload(t, undecodableColumns[0])}, nil))
			j.h.pushDest(malformedERR, j.at())
		},
		// The first of two answered before the query: the second's ERR is
		// judged as the first's was, not taken for having one before it.
		"the second of two": func(t *testing.T, j *joinedMidStream) {
			j.exchange(malformedLongData, [][]byte{malformedERR})
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			j.exchange(q(t, 1), [][]byte{ok(1)})
			build(t, j)
			j.h.pushClient(malformedClose, j.at())
			j.h.pushClient(q(t, 3), j.at())
			if name != "captured before the command" {
				j.h.pushDest(malformedERR, j.at())
			}
			j.h.pushDest(ok(3), j.at())
			j.exchange(q(t, 4), [][]byte{ok(4)})
			mocks, err := j.run(t)
			for _, p := range pairings(mocks) {
				var k, r int
				if n, _ := fmt.Sscanf(p, "SELECT %d -> OK %d", &k, &r); n == 2 && k != r || strings.Contains(p, "ERR") {
					t.Fatalf("mock %q: a command recorded with another's answer (all: %q)", p, pairings(mocks))
				}
			}
			if !errors.Is(err, ErrFramingLost) || !strings.Contains(err.Error(), "ERR 1835") {
				t.Fatalf("RecordV2 = %v, want the stop at the ERR that cannot be told to answer one command or the other", err)
			}
		})
	}
}

// Once the ERR to a command without an answer that does not decode is taken,
// the next command's answer is its own, an ERR 1835 too: a query's (its
// attributes did not parse) is recorded with it, and an execute's that does not
// decode is framed and left out. Neither stops the recording. Dropped as server
// bytes captured before the command, the first ERR left the second to be told
// from it, and the recording stopped there.
func TestRecordV2_AnERRToACommandWithoutAnAnswerLeavesTheNextCommandsOwnERRToIt(t *testing.T) {
	t.Parallel()
	execute := wrapPacket([]byte{mysql.COM_STMT_EXECUTE, 9, 0, 0, 0, 0, 1, 0, 0, 0}, 0)
	for name, c := range map[string]struct {
		command []byte
		want    string
		leftOut int64
	}{
		"a query":    {cannedCOMQuery(t, 0, "SELECT 2"), "SELECT 1 -> OK 1; SELECT 2 -> ERR 1835; SELECT 3 -> OK 3", 1},
		"an execute": {execute, "SELECT 1 -> OK 1; SELECT 3 -> OK 3", 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.exchange(malformedClose, [][]byte{malformedERR})
			j.exchange(c.command, [][]byte{malformedERR})
			j.exchange(cannedCOMQuery(t, 0, "SELECT 3"), [][]byte{okResponseWithAffectedRows(1, 3)})
			mocks, leftOut, err := j.runCounted(t)
			if err != nil {
				t.Fatalf("RecordV2 = %v, want no stop", err)
			}
			if got := strings.Join(pairings(mocks), "; "); got != c.want || leftOut != c.leftOut {
				t.Fatalf("mocks %q with %d left out, want %q with %d", got, leftOut, c.want, c.leftOut)
			}
		})
	}
}

// A command without an answer sent after a PREPARE taken up after a response
// left out, the PREPARE held for its framing: its mock is recorded behind the
// PREPARE's, in the order sent, once the exchange taken up is confirmed. Left
// behind the PREPARE that the exchange's hold took over (takenUp.holdPrepare),
// it was recorded as the PREPARE was settled, before the PREPARE's own mock.
func TestRecordV2_ACommandWithoutAnAnswerAfterAPrepareTakenUpIsRecordedBehindIt(t *testing.T) {
	t.Parallel()
	j := newJoinedMidStream(t)
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
	j.exchange(cannedCOMQuery(t, 0, "SELECT u"), [][]byte{wrapPacket(nil, 1), wrapPacket([]byte{0x01, 0x02}, 2), wrapPacket([]byte{0x03}, 3)})
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0), prepareOK(false, [][]byte{columnPayload(t, undecodableColumns[0])}, nil))
	j.exchange(stmtCloseCommand(0, 7), nil)
	j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{okResponseWithAffectedRows(1, 2)})
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v, want no stop", err)
	}
	want := "SELECT 1 -> OK 1; PREPARE -> PREPARE_OK eof=true; COM_STMT_CLOSE -> NO Response Packet; SELECT 2 -> OK 2"
	if got := strings.Join(prepareOKPairing(mocks), "; "); got != want {
		t.Fatalf("mocks %q, want %q: the CLOSE behind the PREPARE", got, want)
	}
}

// A command without an answer that does not decode is an exchange handled: the
// client bytes lost after it were sent after it, so the exchange the recording
// stops in at that loss is spanned from it (leaveOutInFlight from lastEnd), not
// from the answer before it. Spanned from that answer, a test case that ended
// between the two was left out with the stop, though every mock it used was
// recorded.
func TestRecordV2_AStopAfterACommandWithoutAnAnswerThatDoesNotDecodeIsSpannedFromIt(t *testing.T) {
	t.Parallel()
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
	closeAt := j.at()
	j.h.pushClient(malformedClose, closeAt)
	// Client bytes lost: a packet with sequence id 3 where a command starts.
	j.h.pushClient(cannedCOMQuery(t, 3, "SELECT 2"), j.at())
	mocks, err := j.run(t)
	if !errors.Is(err, ErrFramingLost) || !strings.Contains(err.Error(), "sequence id 3") {
		t.Fatalf("RecordV2 = %v, want the stop at the client's packet out of sequence", err)
	}
	if got := strings.Join(pairings(mocks), "; "); got != "SELECT 1 -> OK 1" {
		t.Fatalf("mocks %q, want SELECT 1's alone", got)
	}
	w, ok := leftOut.last()
	if leftOut.count() != 2 || !ok || !w[0].Equal(closeAt) {
		t.Fatalf("%d spans left out, the last %v; want the CLOSE's, then the stop's from the CLOSE (%v)", leftOut.count(), w, closeAt)
	}
}

// An empty command packet is answered: the server takes it for COM_SLEEP and
// sends an ERR. It does not decode, so its exchange is left out and the
// recording re-aligns at the next command; the walk is marked at it as at any
// command the server answers. Unmarked, the walk still held the answer before
// it, so a client that sent SELECT 2 before the empty packet's ERR had SELECT 2
// recorded with that ERR and SELECT 3 with SELECT 2's answer. Now that
// connection stops at the cut; one that waited for the ERR is recorded whole.
func TestRecordV2_AnEmptyCommandIsAnsweredAndItsAnswerLeftOut(t *testing.T) {
	t.Parallel()
	for _, pipelined := range []bool{true, false} {
		t.Run(fmt.Sprintf("SELECT 2 sent before the ERR %v", pipelined), func(t *testing.T) {
			t.Parallel()
			j := newJoinedMidStream(t)
			// Two answers read whole: the walk, anchored after the first,
			// has walked the second.
			j.exchange(cannedCOMQuery(t, 0, "SELECT 0"), [][]byte{okResponseWithAffectedRows(1, 0)})
			j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
			j.afterLeftOut(t, wrapPacket(nil, 0), errReply(1, 1047, "Unknown command"), pipelined)
			mocks, err := j.run(t)
			got := strings.Join(pairings(mocks), "; ")
			want := "SELECT 0 -> OK 0; SELECT 1 -> OK 1"
			if !pipelined {
				want += "; SELECT 2 -> OK 7; SELECT 3 -> OK 8; SELECT 4 -> OK 9"
			}
			if got != want {
				t.Fatalf("mocks %q, want %q (RecordV2 = %v)", got, want, err)
			}
			if pipelined && (!errors.Is(err, ErrFramingLost) || !strings.Contains(err.Error(), "no packet of the response left out came before it")) {
				t.Fatalf("RecordV2 = %v, want the stop at the cut: no packet of the empty command's answer came before SELECT 2", err)
			}
			if !pipelined && err != nil {
				t.Fatalf("RecordV2 = %v, want no stop: SELECT 2 was sent after the empty command's answer", err)
			}
		})
	}
}

// A PREPARE held for its framing (post-TLS, parameters only) with 255
// parameters has its EOF at sequence id 1, the id the next answer starts at.
// An empty command packet is answered with an ERR, which never starts like an
// EOF, so an EOF there is the PREPARE's: the PREPARE is recorded with it, and
// only the empty command's exchange is left out. Settled as a command whose
// answer may be an EOF, the PREPARE was left out too.
func TestRecordV2_PostTLS_AnEOFBeforeAnEmptyCommandsAnswerIsTheHeldPreparesOwn(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	// The reply in one chunk: the harness holds a few chunks before the
	// recorder runs.
	defs := make([][]byte, 255)
	for i := range defs {
		defs[i] = def
	}
	j := newJoinedMidStream(t)
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
	// One chunk: the harness's channels hold fewer packets than 255.
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0),
		[][]byte{bytes.Join(prepareOK(false, defs, nil), nil)})
	j.afterLeftOut(t, wrapPacket(nil, 0), errReply(1, 1047, "Unknown command"), false)
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v, want no stop", err)
	}
	got := strings.Join(pairings(mocks), "; ")
	want := "SELECT 1 -> OK 1; COM_STMT_PREPARE -> COM_STMT_PREPARE_OK; SELECT 2 -> OK 7; SELECT 3 -> OK 8; SELECT 4 -> OK 9"
	if got != want {
		t.Fatalf("mocks %q, want %q", got, want)
	}
	sp, ok := mocks[1].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
	if !ok || len(sp.ParamDefs) != 255 || sp.EOFAfterParamDefs == nil {
		t.Fatal("the PREPARE was not recorded with its 255 parameters and the EOF its server sent after them")
	}
}

// An empty command packet is answered, so after a response left out it is
// handled as any command the server answers: it confirms the exchange taken up
// before it, and when it comes right after the response left out, the
// server's stream is cut at it. Not confirming, the exchange taken up was
// dropped, uncounted, when the empty command's own fault was left out. Not
// cut there, the rest of the response left out stayed on the stream past the
// empty command's mark, so a client that sent SELECT 2 before the empty
// command's ERR had the walk take that rest for the empty command's answer,
// and SELECT 2 recorded with the ERR.
func TestRecordV2_AnEmptyCommandAfterAResponseLeftOutIsHandledAsAnswered(t *testing.T) {
	t.Parallel()
	col := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "n", OrgName: "n", FixedLength: 0x0c, CharacterSet: 0x2e, ColumnLength: 64, Type: byte(mysql.FieldTypeVarString)})
	// An empty column count, then a result's packets: the recorder cannot
	// frame it, and the rest of it is on the server's stream after the fault.
	unframable := append([][]byte{{0x00, 0x00, 0x00, 0x01}}, resultSet(true, [][]byte{col}, [][]byte{textRowPayload("a"), textRowPayload("b")})...)
	start := func(t *testing.T) *joinedMidStream {
		j := newJoinedMidStream(t)
		j.exchange(cannedCOMQuery(t, 0, "SELECT 0"), [][]byte{okResponseWithAffectedRows(1, 0)})
		j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
		j.exchange(cannedCOMQuery(t, 0, "SELECT n FROM t"), unframable)
		return j
	}
	t.Run("it confirms the exchange taken up before it", func(t *testing.T) {
		t.Parallel()
		j := start(t)
		j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{okResponseWithAffectedRows(1, 7)})
		j.exchange(wrapPacket(nil, 0), [][]byte{errReply(1, 1047, "Unknown command")})
		j.exchange(cannedCOMQuery(t, 0, "SELECT 3"), [][]byte{okResponseWithAffectedRows(1, 8)})
		j.exchange(cannedCOMQuery(t, 0, "SELECT 4"), [][]byte{okResponseWithAffectedRows(1, 9)})
		mocks, err := j.run(t)
		if err != nil {
			t.Fatalf("RecordV2 = %v, want no stop", err)
		}
		if got, want := strings.Join(pairings(mocks), "; "), "SELECT 0 -> OK 0; SELECT 1 -> OK 1; SELECT 2 -> OK 7; SELECT 3 -> OK 8; SELECT 4 -> OK 9"; got != want {
			t.Fatalf("mocks %q, want %q: SELECT 2, taken up after the response left out, is confirmed by the empty command after it", got, want)
		}
	})
	for _, pipelined := range []bool{true, false} {
		t.Run(fmt.Sprintf("the cut is made at it, SELECT 2 sent before its ERR %v", pipelined), func(t *testing.T) {
			t.Parallel()
			j := start(t)
			j.afterLeftOut(t, wrapPacket(nil, 0), errReply(1, 1047, "Unknown command"), pipelined)
			mocks, err := j.run(t)
			got := strings.Join(pairings(mocks), "; ")
			want := "SELECT 0 -> OK 0; SELECT 1 -> OK 1"
			if !pipelined {
				want += "; SELECT 2 -> OK 7; SELECT 3 -> OK 8; SELECT 4 -> OK 9"
			}
			if got != want {
				t.Fatalf("mocks %q, want %q (RecordV2 = %v)", got, want, err)
			}
			if pipelined && (!errors.Is(err, ErrFramingLost) || !strings.Contains(err.Error(), "no packet of the response left out came before it")) {
				t.Fatalf("RecordV2 = %v, want the stop at the cut: no packet of the empty command's answer came before SELECT 2", err)
			}
			if !pipelined && err != nil {
				t.Fatalf("RecordV2 = %v, want no stop: SELECT 2 was sent after the empty command's answer", err)
			}
		})
	}
}

// prepareOKPairing is pairings' line, with a PREPARE's OK shown with whether
// it was recorded with the EOF after its definitions.
func prepareOKPairing(mocks []*models.Mock) []string {
	out := pairings(mocks)
	for i, m := range mocks {
		if len(m.Spec.MySQLResponses) == 1 {
			if sp, ok := m.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket); ok {
				out[i] = fmt.Sprintf("PREPARE -> PREPARE_OK eof=%v", sp.EOFAfterParamDefs != nil || sp.EOFAfterColumnDefs != nil)
			}
		}
	}
	return out
}

// A PREPARE taken up after a response left out, on a stream joined
// mid-connection whose client frames with EOFs, is held for its framing; its
// server sends the EOF after its definitions before the client's next command.
// That EOF is the PREPARE's own answer's last packet: the exchange is confirmed
// by what the server sends after it. Judged as the server's next bytes after
// the answer taken up, it stopped the connection at every such PREPARE, which
// main records. The PREPARE's mock waits for the confirm: when the server's
// bytes after the EOF were captured before the client's next command too, the
// recording stops with the PREPARE left out.
func TestRecordV2_APrepareTakenUpIsConfirmedAfterItsEOF(t *testing.T) {
	t.Parallel()
	one := [][]byte{columnPayload(t, undecodableColumns[0])}
	for _, defs := range []string{"params", "columns"} {
		for _, more := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, more before the next command %v", defs, more), func(t *testing.T) {
				t.Parallel()
				j := newJoinedMidStream(t)
				j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
				j.exchange(cannedCOMQuery(t, 0, "SELECT u"), [][]byte{wrapPacket(nil, 1), wrapPacket([]byte{0x01, 0x02}, 2), wrapPacket([]byte{0x03}, 3)})
				reply := prepareOK(false, one, nil)
				if defs == "columns" {
					reply = prepareOK(false, nil, one)
				}
				j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0), reply)
				if more {
					j.h.pushDest(okResponseWithAffectedRows(1, 9), j.at())
				}
				j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{okResponseWithAffectedRows(1, 2)})
				j.exchange(cannedCOMQuery(t, 0, "SELECT 3"), [][]byte{okResponseWithAffectedRows(1, 3)})
				mocks, err := j.run(t)
				got := strings.Join(prepareOKPairing(mocks), "; ")
				if more {
					if !errors.Is(err, ErrFramingLost) || !strings.Contains(err.Error(), "captured before the client's next command") || got != "SELECT 1 -> OK 1" {
						t.Fatalf("RecordV2 = %v with mocks %q, want the stop at SELECT 2 with the PREPARE left out", err, got)
					}
					return
				}
				if err != nil {
					t.Fatalf("RecordV2 = %v, want no stop: the client waited for each answer", err)
				}
				if want := "SELECT 1 -> OK 1; PREPARE -> PREPARE_OK eof=true; SELECT 2 -> OK 2; SELECT 3 -> OK 3"; got != want {
					t.Fatalf("mocks %q, want %q", got, want)
				}
			})
		}
	}
}

// A PREPARE of 255 parameters taken up after a response left out, its client
// framing with EOFs: the EOF after its definitions is at sequence id 1, as the
// next answer's first packet is, and the next command (COM_FIELD_LIST) may be
// answered with an EOF, so the packet cannot be told to be either, and the
// PREPARE is left out (heldPrepare.settle). Nothing of the exchange taken up
// is recorded, so nothing waits to be confirmed: the recording re-aligns at
// the next command, as main does. Judging the packet as the server's next
// bytes after the PREPARE stopped the connection.
func TestRecordV2_APrepareTakenUpWhoseEOFCannotBeToldIsLeftOutAlone(t *testing.T) {
	t.Parallel()
	defs := make([][]byte, 255)
	for d := range defs {
		defs[d] = columnPayload(t, undecodableColumns[0])
	}
	j := newJoinedMidStream(t)
	j.exchange(cannedCOMQuery(t, 0, "SELECT 1"), [][]byte{okResponseWithAffectedRows(1, 1)})
	j.exchange(cannedCOMQuery(t, 0, "SELECT u"), [][]byte{wrapPacket(nil, 1), wrapPacket([]byte{0x01, 0x02}, 2), wrapPacket([]byte{0x03}, 3)})
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0), [][]byte{bytes.Join(prepareOK(false, defs, nil), nil)})
	j.exchange(wrapPacket([]byte{mysql.COM_FIELD_LIST, 't', 0}, 0), [][]byte{errReply(1, 1146, "Table 't' doesn't exist")})
	j.exchange(cannedCOMQuery(t, 0, "SELECT 2"), [][]byte{okResponseWithAffectedRows(1, 2)})
	j.exchange(cannedCOMQuery(t, 0, "SELECT 3"), [][]byte{okResponseWithAffectedRows(1, 3)})
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v, want no stop", err)
	}
	if got, want := strings.Join(prepareOKPairing(mocks), "; "), "SELECT 1 -> OK 1; SELECT 2 -> OK 2; SELECT 3 -> OK 3"; got != want {
		t.Fatalf("mocks %q, want %q", got, want)
	}
}
