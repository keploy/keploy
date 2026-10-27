package recorder

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// A capture hole: bytes the producer saw on the wire that never reached the
// recorder, as the producer stamps it: every chunk of a direction numbered
// from 1 (fakeconn.Chunk.SeqNo), and each lost chunk still taking its number.
// Proxyless capture loses a send whole this way when its bytes cannot be read
// from the app's memory (keploy/enterprise mysql-pooled-unframed-e2e: one
// 110-byte COM_QUERY of a pooled connection). The recorder is fed on across
// the hole (MySQL.CanResyncAfterGap), and one hole must cost the exchanges it
// cut, counted as left out, never the rest of the connection's recording, and
// never a mock that pairs a command with another command's answer.

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

// Two commands in a row lost: both exchanges are left out, in one span.
func TestRecordV2_TwoCommandsLostToOneCaptureHole(t *testing.T) {
	t.Parallel()
	xs := kitTraffic(t, []int{7, 8}, false)
	client, server := onTheWire(xs, 7)
	rec := recordChunks(t, holed(client, clientChunkOf(xs, 4), clientChunkOf(xs, 5)), holed(server), true, time.Minute)
	if w := wrongMocks(t, rec.mocks); len(w) > 0 {
		t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
	}
	if !rec.returned || rec.err != nil {
		t.Fatalf("RecordV2 returned=%v err=%v", rec.returned, rec.err)
	}
	if got := queryMocks(rec.mocks); got != 18-2 {
		t.Fatalf("%d query mocks, want %d", got, 18-2)
	}
	w, ok := rec.leftOut.last()
	if !ok || w[0].After(commandAt(t, xs, 7, 4)) || w[1].Before(commandAt(t, xs, 7, 5)) {
		t.Fatalf("the span left out is %v; it must hold both lost commands", w)
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
// 3, ...) are never taken for a command.
func TestRecordV2_ALoadDataCutByACaptureHole(t *testing.T) {
	t.Parallel()
	small := kitTraffic(t, []int{7, 8}, false)
	xs := insert(small, 3, loadDataLocal(t)...)
	for _, c := range []struct {
		name string
		lose int // the exchange whose command is lost
	}{
		{"the upload lost", 4},
		{"the LOAD DATA lost, its upload not", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			client, server := onTheWire(xs, 7)
			rec := recordChunks(t, holed(client, clientChunkOf(xs, c.lose)), holed(server), true, time.Minute)
			// The LOAD DATA is left out either way, alone: the recorder
			// does not record one (its answer asks for the upload).
			requireHoleCost(t, rec, []time.Time{commandAt(t, xs, 7, 3)}, 18)
		})
	}
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
