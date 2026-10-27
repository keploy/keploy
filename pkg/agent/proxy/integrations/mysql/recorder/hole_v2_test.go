package recorder

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// A capture hole: bytes the producer saw on the wire that never reached the
// recorder, as the producer stamps it: every chunk of a direction numbered
// from 1 (fakeconn.Chunk.SeqNo), and each lost chunk still taking its number.
// Proxyless capture loses a send whole this way when its bytes cannot be read
// from the app's memory (keploy/enterprise mysql-pooled-unframed-e2e: one
// 110-byte COM_QUERY of a pooled connection). The recorder is fed on across
// the hole (MySQL.CanResyncAfterGap), and one hole must cost the exchanges it
// cut, counted as left out, never a mock that pairs a command with another
// command's answer, and not the rest of the connection's recording where the
// streams show where the client's next command and its answer start. Where
// they do not (the client sent its next command before an answer to what the
// hole lost came, or the hole came while the rest of a response left out was
// still unread), the recording stops at the hole, as it did for every hole.
// It stops too where more than one chunk of the client's stream was lost
// before the command taken up after a hole was answered: each may have held a
// command, and the answer to an earlier one may have come before the client's
// next command and the answer to the last after it, which nothing on the
// streams tells from that command's own answer.

// holed numbers chunks on from the handshake's (recordChunks: the client's
// from 2, the server's from 3) and leaves out those lose picks, by index.
func holed(chunks []fakeconn.Chunk, lose ...int) []fakeconn.Chunk {
	first := uint64(2)
	if len(chunks) > 0 && chunks[0].Dir == fakeconn.FromDest {
		first = 3
	}
	gone := map[int]bool{}
	for _, i := range lose {
		gone[i] = true
	}
	var out []fakeconn.Chunk
	for i, c := range chunks {
		c.SeqNo = first + uint64(i)
		if !gone[i] {
			out = append(out, c)
		}
	}
	return out
}

// clientChunkOf is the index of exchange i's command among the client chunks
// onTheWire lays out (one per exchange with a command).
func clientChunkOf(xs []exchange, i int) int {
	n := -1
	for _, x := range xs[:i+1] {
		if len(x.command) > 0 {
			n++
		}
	}
	return n
}

// serverChunksOf is the indexes of exchange i's answer among the server
// chunks onTheWire lays out in reads of n bytes.
func serverChunksOf(xs []exchange, n, i int) (from, to int) {
	_, before := onTheWire(xs[:i], n)
	_, through := onTheWire(xs[:i+1], n)
	return len(before), len(through)
}

// requireHoleCost checks a recording over a hole: no wrong mock, RecordV2
// still recording at the end of the connection, left out exactly the
// exchanges the hole cut (each window holding when its command was sent), and
// every other query recorded.
func requireHoleCost(t *testing.T, rec recording, sentAt []time.Time, queries int) {
	t.Helper()
	if w := wrongMocks(t, rec.mocks); len(w) > 0 {
		t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
	}
	if !rec.returned || rec.err != nil {
		t.Fatalf("RecordV2 returned=%v err=%v: a capture hole must cost the exchanges it cut, not the rest of the connection's recording", rec.returned, rec.err)
	}
	if got := rec.leftOut.count(); got != len(sentAt) {
		t.Fatalf("%d exchanges left out, want %d: the ones the hole cut, each counted", got, len(sentAt))
	}
	for _, at := range sentAt {
		held := false
		for _, w := range rec.leftOut.all() {
			if !w[0].After(at) && !w[1].Before(at) {
				held = true
			}
		}
		if !held {
			t.Fatalf("no window left out holds %v, when a command the hole cut was sent: the test case that sent it would be saved without its mock", at.Format(time.StampMicro))
		}
	}
	if got := queryMocks(rec.mocks); got != queries {
		t.Fatalf("%d query mocks, want %d: every exchange but the ones the hole cut", got, queries)
	}
}

// One COM_QUERY lost whole on the client's stream, what the lane lost: its
// answer is on the server's stream, and the next command is paired with the
// next answer, not with it.
func TestRecordV2_ACommandLostToACaptureHole(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false) // 18 exchanges, all queries
	// The last command lost has no chunk after it to show the hole: that one
	// is the producer's to report, as it ends the stream there.
	for _, lost := range []int{0, 3, 9, 16} {
		for _, n := range []int{7, 16384} {
			t.Run(fmt.Sprintf("exchange %d, reads of %d", lost, n), func(t *testing.T) {
				t.Parallel()
				client, server := onTheWire(xs, n)
				rec := recordChunks(t, holed(client, clientChunkOf(xs, lost)), holed(server), true, time.Minute)
				requireHoleCost(t, rec, []time.Time{commandAt(t, xs, n, lost)}, 18-1)
			})
		}
	}
}

// A command sent in two chunks, the first lost: the second starts in the
// middle of the packet, where a header read out of the query's text claims
// megabytes. It is passed over whole, and the next command is taken up.
func TestRecordV2_TheStartOfACommandLostToACaptureHole(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	client, server := onTheWire(xs, 7)
	j := clientChunkOf(xs, 5)
	first, second := client[j], client[j]
	first.Bytes, second.Bytes = client[j].Bytes[:10], client[j].Bytes[10:]
	split := append(append(append([]fakeconn.Chunk{}, client[:j]...), first, second), client[j+1:]...)
	rec := recordChunks(t, holed(split, j), holed(server), true, time.Minute)
	requireHoleCost(t, rec, []time.Time{commandAt(t, xs, 7, 5)}, 18-1)
}

// A hole after which no chunk proves a command before the client's stream
// ends: what the hole cut is left out all the same.
func TestRecordV2_ACaptureHoleWithNoCommandAfterIt(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	xs[17].command = wrapPacket([]byte("1,7,a\n"), 2)
	client, server := onTheWire(xs, 7)
	rec := recordChunks(t, holed(client, clientChunkOf(xs, 16)), holed(server), true, time.Minute)
	requireHoleCost(t, rec, []time.Time{commandAt(t, xs, 7, 16)}, 18-2)
}

// Two commands in a row lost to one hole (two chunks of the client's stream):
// the answer to the first came before the client's next command, and the one
// to the second came before it too, or, sent before it, only after it. The
// streams cannot tell those apart, and in the second the next command's
// first answer is the second lost command's: the recording stops at the
// hole, with what it cut left out, and records no command with another
// command's answer.
func TestRecordV2_TwoCommandsLostToOneCaptureHole(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	t.Run("both answered before the next command", func(t *testing.T) {
		t.Parallel()
		client, server := onTheWire(xs, 7)
		rec := recordChunks(t, holed(client, clientChunkOf(xs, 4), clientChunkOf(xs, 5)), holed(server), true, time.Minute)
		requireStoppedAtTheHole(t, rec, commandAt(t, xs, 7, 4), 4)
	})
	for _, n := range []int{7, 16384} {
		t.Run(fmt.Sprintf("the second's answer after the next command, reads of %d", n), func(t *testing.T) {
			t.Parallel()
			client, server := pipelinedGroup(xs, n, 4, 5)
			first := clientChunkOf(xs, 3)
			rec := recordChunks(t, holed(client, first, clientChunkOf(xs, 4)), holed(server), true, time.Minute)
			requireStoppedAtTheHole(t, rec, client[first].ReadAt, 3)
		})
	}
}

// Two holes of one chunk each, with a command that has no answer taken up
// between them (COM_STMT_CLOSE): the second came before any command taken up
// after the first was answered, so the two lost commands' answers are on the
// server's stream as after one hole that lost both. The second lost command
// was sent before the next one's answer came: the recording stops at the
// second hole, and that answer is not recorded with the next command.
func TestRecordV2_ASecondCaptureHoleBeforeTheFirstIsAnswered(t *testing.T) {
	t.Parallel()
	xs := insert(kitTraffic(t, []int{7, 8}, false), 4, exchange{command: stmtCloseCommand(0, 1)})
	for _, n := range []int{7, 16384} {
		t.Run(fmt.Sprintf("reads of %d", n), func(t *testing.T) {
			t.Parallel()
			client, server := pipelinedGroup(xs, n, 5, 6)
			second := clientChunkOf(xs, 5)
			rec := recordChunks(t, holed(client, clientChunkOf(xs, 3), second), holed(server), true, time.Minute)
			var mocks []*models.Mock
			closes := 0
			for _, m := range rec.mocks {
				if m.Spec.Metadata["requestOperation"] == "COM_STMT_CLOSE" {
					closes++
					continue
				}
				mocks = append(mocks, m)
			}
			if closes != 1 {
				t.Fatalf("%d COM_STMT_CLOSE mocks, want the one taken up after the first hole", closes)
			}
			rec.mocks = mocks
			requireStoppedAtTheHole(t, rec, client[second].ReadAt, 3)
		})
	}
}

// Bytes lost in the middle of an answer on the server's stream: that exchange
// is left out, and the recording goes on from the client's next command.
func TestRecordV2_AnAnswerCutByACaptureHole(t *testing.T) {
	t.Parallel()
	for _, n := range []int{7, 4096} {
		t.Run(fmt.Sprintf("reads of %d", n), func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{300, 301}, true) // the 2nd exchange is an 8,209-row result
			client, server := onTheWire(xs, n)
			from, to := serverChunksOf(xs, n, 1)
			mid := from + (to-from)/2
			rec := recordChunks(t, holed(client), holed(server, mid, mid+1), true, time.Minute)
			requireHoleCost(t, rec, []time.Time{commandAt(t, xs, n, 1)}, 19-1)
		})
	}
	// A small answer lost whole: the hole is right before the next answer.
	t.Run("an answer lost whole", func(t *testing.T) {
		t.Parallel()
		xs := kitTraffic(t, []int{7, 8}, false)
		client, server := onTheWire(xs, 16384)
		from, to := serverChunksOf(xs, 16384, 4)
		var lose []int
		for i := from; i < to; i++ {
			lose = append(lose, i)
		}
		rec := recordChunks(t, holed(client), holed(server, lose...), true, time.Minute)
		requireHoleCost(t, rec, []time.Time{commandAt(t, xs, 16384, 4)}, 18-1)
	})
}

// A client that pipelines its commands gets answers that do not start after
// their commands, so after a hole neither stream can be taken up at a command
// the answers after it are known to follow: the recording stops at the hole,
// with the exchange it cut left out and nothing recorded wrong, as for a
// pipelining client's response it cannot frame.
func TestRecordV2_APipeliningClientStopsAtACaptureHole(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		lose func(xs []exchange, client, server []fakeconn.Chunk) (c, s []fakeconn.Chunk)
	}{
		{"a command lost on the client's stream", func(xs []exchange, client, server []fakeconn.Chunk) ([]fakeconn.Chunk, []fakeconn.Chunk) {
			return holed(client, clientChunkOf(xs, 5)), holed(server)
		}},
		{"an answer lost on the server's stream", func(xs []exchange, client, server []fakeconn.Chunk) ([]fakeconn.Chunk, []fakeconn.Chunk) {
			from, _ := serverChunksOf(xs, 16384, 5)
			return holed(client), holed(server, from)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			client, server := onTheWire(xs, 16384)
			// The 2nd command sent before the answer to the 1st arrived.
			client[1].ReadAt = client[0].ReadAt.Add(time.Nanosecond)
			holedClient, holedServer := c.lose(xs, client, server)
			rec := recordChunks(t, holedClient, holedServer, true, time.Minute)
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
			}
			if !rec.returned || !errors.Is(rec.err, fakeconn.ErrCaptureHole) {
				t.Fatalf("RecordV2 returned=%v err=%v, want it stopped with ErrCaptureHole: a pipelining client's streams cannot be taken up again after a hole", rec.returned, rec.err)
			}
			if rec.stops != 1 {
				t.Fatalf("the connection's stop was stamped %d times, want once", rec.stops)
			}
			if got := queryMocks(rec.mocks); got != 5 {
				t.Fatalf("%d query mocks, want the 5 before the hole", got)
			}
			if w, ok := rec.leftOut.last(); !ok || w[0].After(commandAt(t, xs, 16384, 5)) {
				t.Fatalf("the window left out is %v (found %v); it must hold the command the hole cut, sent at %v", w, ok, commandAt(t, xs, 16384, 5).Format(time.StampMicro))
			}
		})
	}
}

// A hole inside a LOAD DATA LOCAL INFILE: its upload, or the LOAD DATA
// itself, lost on the client's stream. The upload's packets (sequence ids 2,
// 3, ...) are never taken for a command. The LOAD DATA is left out either
// way, alone: the recorder does not record one (its answer asks for the
// upload).
func TestRecordV2_ALoadDataCutByACaptureHole(t *testing.T) {
	t.Parallel()
	small := kitTraffic(t, []int{7, 8}, false)
	xs := insert(small, 3, loadDataLocal(t)...)
	// The LOAD DATA lost: what follows it on the server's stream answers
	// what the hole lost, and the recording goes on.
	t.Run("the LOAD DATA lost, its upload not", func(t *testing.T) {
		t.Parallel()
		client, server := onTheWire(xs, 7)
		rec := recordChunks(t, holed(client, clientChunkOf(xs, 3)), holed(server), true, time.Minute)
		requireHoleCost(t, rec, []time.Time{commandAt(t, xs, 7, 3)}, 18)
	})
	// The upload lost, after the LOAD DATA was left out: the server's OK to
	// it, still unread, cannot be told from an answer to a command the hole
	// may also have lost, sent before the client's next command
	// (TestRecordV2_ACaptureHoleWhileARestOfAResponseLeftOutIsUnread). The
	// recording stops there.
	t.Run("the upload lost", func(t *testing.T) {
		t.Parallel()
		client, server := onTheWire(xs, 7)
		rec := recordChunks(t, holed(client, clientChunkOf(xs, 4)), holed(server), true, time.Minute)
		requireStoppedAtTheHole(t, rec, commandAt(t, xs, 7, 3), 3)
	})
}

// A PREPARE whose answer was lost: the PREPARE is left out, and so is the
// EXECUTE of the statement it would have defined (its parameters cannot be
// decoded); the connection's later queries are recorded.
func TestRecordV2_APrepareWhoseAnswerIsLostToACaptureHole(t *testing.T) {
	t.Parallel()
	small := kitTraffic(t, []int{7, 8}, false)
	def := sessionColumns(t, "payload")[0]
	execute := []byte{mysql.COM_STMT_EXECUTE, 0, 0, 0, 0, 0x00, 1, 0, 0, 0, 0x00, 0x01, 0x03, 0x00, 7, 0, 0, 0}
	binary.LittleEndian.PutUint32(execute[1:], undecodableStmtID)
	inserted := frame([][]byte{{0x00, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00}})
	xs := insert(small, 3,
		exchange{command: wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0),
			reply: prepareOK(false, [][]byte{def}, nil)},
		exchange{command: wrapPacket(execute, 0), reply: inserted})
	for _, n := range []int{7, 16384} {
		t.Run(fmt.Sprintf("reads of %d", n), func(t *testing.T) {
			t.Parallel()
			client, server := onTheWire(xs, n)
			from, to := serverChunksOf(xs, n, 3)
			var lose []int
			for i := from; i < to; i++ {
				lose = append(lose, i)
			}
			rec := recordChunks(t, holed(client), holed(server, lose...), true, time.Minute)
			requireHoleCost(t, rec, []time.Time{commandAt(t, xs, n, 3), commandAt(t, xs, n, 4)}, 18)
		})
	}
}

// provesCommand takes up the client's stream only at a chunk that starts a
// command a driver sends on its own.
func TestProvesCommand(t *testing.T) {
	t.Parallel()
	q := cannedCOMQuery(t, 0, "SELECT 1")
	closeStmt := wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0, 0}, 0)
	for _, c := range []struct {
		name string
		b    []byte
		want bool
	}{
		{"a query", q, true},
		{"a query going on in later chunks", q[:6], true},
		{"a close, then a query", append(append([]byte{}, closeStmt...), q...), true},
		{"an upload packet", wrapPacket([]byte("1,7,a\n"), 2), false},
		{"an upload packet at sequence id 0 after 255", wrapPacket([]byte("1,7,a\n"), 0), false},
		{"a query, then a packet that is not a command", append(append([]byte{}, q...), wrapPacket([]byte{0x03}, 1)...), false},
		{"a ping of the wrong length", wrapPacket([]byte{mysql.COM_PING, 0}, 0), false},
		{"a close of the wrong length", wrapPacket([]byte{mysql.COM_STMT_CLOSE, 1, 0, 0}, 0), false},
		{"an execute too short to name its statement", wrapPacket([]byte{mysql.COM_STMT_EXECUTE, 1, 0, 0, 0}, 0), false},
		{"the middle of a packet", q[2:], false},
		{"a header alone", q[:4], false},
	} {
		if got := provesCommand(c.b); got != c.want {
			t.Errorf("%s: provesCommand = %v, want %v", c.name, got, c.want)
		}
	}
}

// all is every window left out.
func (o *orphanSpans) all() [][2]time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([][2]time.Time{}, o.windows...)
}

// pipelinedGroup lays the exchanges out as onTheWire does in reads of n, but
// with the commands of exchanges first+1 to last captured right after the
// command of first, before its answer: a client that pipelines that group,
// and waits for each answer everywhere else.
func pipelinedGroup(xs []exchange, n, first, last int) (client, server []fakeconn.Chunk) {
	client, server = onTheWire(xs, n)
	var order []*fakeconn.Chunk
	for i := range xs {
		if len(xs[i].command) > 0 && (i <= first || i > last) {
			order = append(order, &client[clientChunkOf(xs, i)])
		}
		if i == first {
			for j := first + 1; j <= last; j++ {
				if len(xs[j].command) > 0 {
					order = append(order, &client[clientChunkOf(xs, j)])
				}
			}
		}
		from, to := serverChunksOf(xs, n, i)
		for k := from; k < to; k++ {
			order = append(order, &server[k])
		}
	}
	for k, c := range order {
		c.ConnSeq = fakeconn.ConnSeqOf(uint64(handshakeChunks + 1 + k))
		c.ReadAt = wireBase.Add(time.Duration(k+1) * time.Microsecond)
	}
	return client, server
}

// requireStoppedAtTheHole checks a recording that stops at a hole: no wrong
// mock, stopped with ErrCaptureHole and stamped once, every query before the
// hole recorded (queries), and a window left out that holds lostAt, when the
// command the hole cut was sent.
func requireStoppedAtTheHole(t *testing.T, rec recording, lostAt time.Time, queries int) {
	t.Helper()
	if w := wrongMocks(t, rec.mocks); len(w) > 0 {
		t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
	}
	if !rec.returned || !errors.Is(rec.err, fakeconn.ErrCaptureHole) {
		t.Fatalf("RecordV2 returned=%v err=%v, want it stopped with ErrCaptureHole", rec.returned, rec.err)
	}
	if rec.stops != 1 {
		t.Fatalf("the connection's stop was stamped %d times, want once", rec.stops)
	}
	if got := queryMocks(rec.mocks); got != queries {
		t.Fatalf("%d query mocks, want the %d before the hole", got, queries)
	}
	held := false
	for _, w := range rec.leftOut.all() {
		if !w[0].After(lostAt) && !w[1].Before(lostAt) {
			held = true
		}
	}
	if !held {
		t.Fatalf("no window left out holds %v, when the command the hole cut was sent", lostAt.Format(time.StampMicro))
	}
}

// A client that first pipelines at the hole: the command lost was followed by
// the next before its answer came. That answer is the first after the next
// command, and nothing on either stream tells it from the next command's own,
// so the recording stops at the hole, with what it cut left out, and records
// no command paired with another command's answer. The client waited for
// every answer before the hole, so it is not known to pipeline there.
func TestRecordV2_ACommandSentBeforeTheAnswerToOneLostToACaptureHole(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	for _, p := range []int{0, 2, 3, 9} {
		t.Run(fmt.Sprintf("exchange %d lost, %d sent before its answer", p, p+1), func(t *testing.T) {
			t.Parallel()
			client, server := pipelinedGroup(xs, 16384, p, p+1)
			lost := clientChunkOf(xs, p)
			rec := recordChunks(t, holed(client, lost), holed(server), true, time.Minute)
			requireStoppedAtTheHole(t, rec, client[lost].ReadAt, p)
		})
	}
}

// Connector/J writes a streamed BLOB parameter as COM_STMT_RESET, then
// COM_STMT_SEND_LONG_DATA and COM_STMT_EXECUTE, sent before the RESET's OK
// arrives (pipelined_commands_test.go). With the RESET lost to a hole, that
// OK comes after the EXECUTE, which must not be recorded with it.
func TestRecordV2_AStreamedBlobWriteWhoseResetIsLostToACaptureHole(t *testing.T) {
	t.Parallel()
	small := kitTraffic(t, []int{7, 8}, false)
	def := sessionColumns(t, "payload")[0]
	execute := []byte{mysql.COM_STMT_EXECUTE, 0, 0, 0, 0, 0x00, 1, 0, 0, 0, 0x00, 0x01, 0x03, 0x00, 7, 0, 0, 0}
	binary.LittleEndian.PutUint32(execute[1:], undecodableStmtID)
	reset := []byte{mysql.COM_STMT_RESET, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(reset[1:], undecodableStmtID)
	longData := []byte{mysql.COM_STMT_SEND_LONG_DATA, 0, 0, 0, 0, 0, 0, 'b', 'l', 'o', 'b'}
	binary.LittleEndian.PutUint32(longData[1:], undecodableStmtID)
	inserted := frame([][]byte{{0x00, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00}}) // OK, 1 row
	resetOK := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})  // OK, 0 rows
	for _, withLongData := range []bool{false, true} {
		t.Run(fmt.Sprintf("with COM_STMT_SEND_LONG_DATA %v", withLongData), func(t *testing.T) {
			t.Parallel()
			group := []exchange{{command: wrapPacket(reset, 0), reply: resetOK}}
			if withLongData {
				group = append(group, exchange{command: wrapPacket(longData, 0)})
			}
			group = append(group, exchange{command: wrapPacket(execute, 0), reply: inserted})
			xs := insert(small, 3, append([]exchange{
				{command: wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO t VALUES (?)"...), 0), reply: prepareOK(false, [][]byte{def}, nil)},
				{command: wrapPacket(execute, 0), reply: inserted},
			}, group...)...)
			client, server := pipelinedGroup(xs, 16384, 5, 5+len(group)-1)
			lost := clientChunkOf(xs, 5)
			rec := recordChunks(t, holed(client, lost), holed(server), true, time.Minute)
			executes := 0
			for _, m := range rec.mocks {
				if len(m.Spec.MySQLRequests) == 0 {
					continue
				}
				if _, ok := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket); !ok {
					continue
				}
				executes++
				if len(m.Spec.MySQLResponses) != 1 {
					t.Fatalf("an EXECUTE mock with %d responses", len(m.Spec.MySQLResponses))
				}
				if ok, isOK := m.Spec.MySQLResponses[0].Message.(*mysql.OKPacket); !isOK || ok.AffectedRows != 1 {
					t.Fatalf("an EXECUTE mock answered with %#v, not its own OK (1 row): the RESET's OK paired with it", m.Spec.MySQLResponses[0].Message)
				}
			}
			if executes != 1 {
				t.Fatalf("%d EXECUTE mocks, want the one before the hole", executes)
			}
			// The PREPARE, EXECUTE and COM_STMT_SEND_LONG_DATA mocks are
			// checked above; the queries' as everywhere.
			var queries []*models.Mock
			for _, m := range rec.mocks {
				if len(m.Spec.MySQLRequests) > 0 {
					if _, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); ok {
						queries = append(queries, m)
					}
				}
			}
			rec.mocks = queries
			requireStoppedAtTheHole(t, rec, client[lost].ReadAt, 3)
		})
	}
}

// A hole on the client's stream while the server's still holds the rest of a
// response left out: an answer to what the hole lost cannot be told from that
// rest, so the recording stops there. Here the client sent the command after
// the hole before the answer to the one lost came, and that answer must not
// be recorded with it.
func TestRecordV2_ACaptureHoleWhileARestOfAResponseLeftOutIsUnread(t *testing.T) {
	t.Parallel()
	for _, n := range []int{7, 16384} {
		t.Run(fmt.Sprintf("reads of %d", n), func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			// A response head that does not decode: its exchange is left out.
			xs[3].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[3].reply...)
			client, server := pipelinedGroup(xs, n, 4, 5)
			lost := clientChunkOf(xs, 4)
			rec := recordChunks(t, holed(client, lost), holed(server), true, time.Minute)
			requireStoppedAtTheHole(t, rec, client[lost].ReadAt, 3)
		})
	}
}

// A connection whose capture cannot order its two directions (each chunk the
// same ConnSeq, as the proxyless capture numbers a connection it has no
// copied_seq for): no floor drops the answer to the command a hole lost, and
// only the cut by time at the next command (realignAnswer) keeps it from
// that command.
func TestRecordV2_ACommandLostToACaptureHoleOnAConnectionNotOrderedByConnSeq(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	for _, lost := range []int{3, 9} {
		t.Run(fmt.Sprintf("exchange %d", lost), func(t *testing.T) {
			t.Parallel()
			client, server := onTheWire(xs, 16384)
			for i := range client {
				client[i].ConnSeq = 1
			}
			for i := range server {
				server[i].ConnSeq = 1
			}
			rec := recordChunks(t, holed(client, clientChunkOf(xs, lost)), holed(server), true, time.Minute)
			requireHoleCost(t, rec, []time.Time{commandAt(t, xs, 16384, lost)}, 18-1)
		})
	}
}

// A second hole inside the command taken up after the first: two chunks of
// the client's stream lost before a command taken up was answered, so the
// recording stops at the first hole, with what the two cut left out.
func TestRecordV2_ASecondCaptureHoleInsideTheCommandTakenUpAfterTheFirst(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	client, server := onTheWire(xs, 7)
	j := clientChunkOf(xs, 6)
	first, second := client[j], client[j]
	first.Bytes, second.Bytes = client[j].Bytes[:10], client[j].Bytes[10:]
	split := append(append(append([]fakeconn.Chunk{}, client[:j]...), first, second), client[j+1:]...)
	// Exchange 5's command lost, and the second chunk of exchange 6's.
	rec := recordChunks(t, holed(split, j-1, j+1), holed(server), true, time.Minute)
	requireStoppedAtTheHole(t, rec, commandAt(t, xs, 7, 5), 5)
	requireStoppedAtTheHole(t, rec, commandAt(t, xs, 7, 6), 5)
}

// A capture hole on the client's stream while a PREPARE is held (a
// connection joined mid-stream, whose framing is only assumed): the server's
// stream may still hold the PREPARE's EOF, captured before the command the
// hole lost, and that EOF cannot be told from an answer to it. The client
// here sent its next command before the answer to the one lost came: the
// recording stops at the hole, the PREPARE left out, and that answer is not
// recorded with the next command.
func TestRecordV2_ACaptureHoleWhileAPrepareIsHeld(t *testing.T) {
	t.Parallel()
	def := func(name string) []byte {
		return columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: name, OrgName: name, FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	}
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	client := func(b []byte, seqNo uint64) {
		at := j.at()
		j.h.clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: fakeconn.ConnSeqOf(j.h.captured.Add(1)),
			Bytes: b, ReadAt: at, WrittenAt: at, SeqNo: seqNo}
	}
	answer := func(affected byte) {
		for _, p := range frame([][]byte{{0x00, affected, 0x00, 0x02, 0x00, 0x00, 0x00}}) {
			j.h.pushDest(p, j.at())
		}
	}
	// The PREPARE, answered with its definitions and the EOF after them,
	// each a chunk of its own.
	client(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t WHERE a = ? AND b = ?"...), 0), 1)
	for _, p := range prepareOK(false, [][]byte{def("?"), def("?")}, nil) {
		j.h.pushDest(p, j.at())
	}
	j.at() // the lost command (SeqNo 2) was sent here
	client(cannedCOMQuery(t, 0, "UPDATE t SET b = 2"), 3)
	answer(5) // the lost command's
	answer(7) // the UPDATE's
	mocks, err := j.run(t)
	for _, m := range mocks {
		if len(m.Spec.MySQLRequests) == 0 || len(m.Spec.MySQLResponses) == 0 {
			continue
		}
		if ok, isOK := m.Spec.MySQLResponses[0].Message.(*mysql.OKPacket); isOK && ok.AffectedRows == 5 {
			t.Fatalf("a %T mock recorded with the answer to the command the hole lost", m.Spec.MySQLRequests[0].Message)
		}
	}
	if !errors.Is(err, fakeconn.ErrCaptureHole) {
		t.Fatalf("RecordV2 = %v, want it stopped with ErrCaptureHole", err)
	}
	if len(mocks) != 0 {
		t.Fatalf("%d mocks, want none: the PREPARE is left out, and the recording stops at the hole", len(mocks))
	}
	if leftOut.count() < 1 {
		t.Fatalf("%d exchanges left out, want the PREPARE's at least", leftOut.count())
	}
}
