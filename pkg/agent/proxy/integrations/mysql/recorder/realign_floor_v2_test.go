package recorder

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
)

// One rule cuts the server's stream of one connection: at every command the
// floor drops the server bytes captured before the command, by capture number
// (Session.NextRequest). After a response the recorder cannot frame, that is
// what cuts the rest of that response off, at the client's next command
// (realignAnswer, answerStartsAfter); capture times play no part.
//
// Here one connection has a response that cannot be framed, then an answer to
// no command of the capture (a whole OK, captured before the command after
// it), then its other exchanges. Each stale byte leaves the stream once, by
// the floor: the rest of the response and the stale answer together, in one
// run, when the stale answer was numbered before the command the cut is made
// at, however it was stamped (one stamped after that command waited at the
// capture while the command was read). No live byte is dropped: every other
// exchange is recorded with its own answer. The exchange left out is counted
// once, the recorder's line for it is logged once, and the connection's stop
// is never stamped. A stale answer captured before a later command stops the
// recording at the answer taken up, which it does not confirm.
//
// Without the floor, the stale answer is recorded as the next command's, and
// every command after it with the answer before its own.
//
// Not parallel: the WARNs are limited process-wide, and this test counts them.
func TestRecordV2_AStaleAnswerAfterAResponseLeftOutIsDroppedOnce(t *testing.T) {
	const (
		skipLine  = "V2: skipped the rest of a mysql response left out"
		floorLine = "dropped server bytes captured before the request after them"
		ownLine   = "V2: failed to decode mysql response head; the exchange is left out, and the connection's recording goes on from its next command"
	)
	// A whole answer: an OK at sequence id 1, as a command's answer starts.
	stale := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})
	for _, c := range []struct {
		name string
		// before is the exchange whose command the stale answer was captured
		// just before: 3 is the command after the response left out, where
		// the cut is made.
		before int
		// stampedAfter stamps the stale answer after that command, which it
		// is still numbered before.
		stampedAfter bool
		// stops: captured after the answer taken up after the response left
		// out and before the client's next command, it is what the rest of
		// an answer the re-align cut into looks like (the client pipelined
		// that command into it, and the answer taken up is that answer's
		// tail): the answer taken up is not confirmed, and the recording
		// stops (takenUp), with no wrong mock.
		stops bool
	}{
		{"captured before the command after the response left out", 3, false, false},
		{"numbered before the command after the response left out, stamped after it", 3, true, false},
		{"captured before the command after that one", 4, false, true},
	} {
		for _, n := range []int{512, 16384} {
			t.Run(fmt.Sprintf("%s, reads of %d", c.name, n), func(t *testing.T) {
				resetWarnLimiters()
				xs := kitTraffic(t, []int{7, 8}, false) // 18 exchanges
				// An empty packet where the column count should be: the
				// recorder reads that packet, 4 bytes, and cannot frame the
				// rest of the response.
				xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
				rest := len(bytes.Join(xs[2].reply, nil)) - 4
				// More of the server's stream with no command of its own: it
				// is captured after the answer before it, and before the
				// command of exchange c.before.
				xs = insert(xs, c.before, exchange{reply: stale})
				client, server := onTheWire(xs, n)
				// The stale answer is the last chunk laid out up to it.
				_, upTo := onTheWire(xs[:c.before+1], n)
				at := len(upTo) - 1
				if !bytes.Equal(server[at].Bytes, stale[0]) {
					t.Fatal("fixture: the stale answer is not a chunk of its own")
				}
				if c.stampedAfter {
					// After the command it was captured before, and before
					// that command's answer.
					server[at].ReadAt = commandAt(t, xs, n, c.before+1).Add(500 * time.Nanosecond)
				}
				staleAt := server[at].ReadAt

				rec := recordChunks(t, client, server, true, time.Minute)
				if c.stops {
					requireFramingLost(t, rec)
					if !strings.Contains(rec.err.Error(), "is not confirmed: the server's next bytes after it were captured before the client's next command") {
						t.Fatalf("RecordV2 = %v, want the stop to name the answer taken up that the server's next bytes do not confirm", rec.err)
					}
					if got := queryMocks(rec.mocks); got != 2 {
						t.Fatalf("%d query mocks, want the 2 before the response left out", got)
					}
					return
				}
				// Every exchange but the one left out is recorded, each with
				// its own answer: no live byte was dropped, and the stale
				// answer was recorded as no command's.
				requireRealigned(t, rec, commandAt(t, xs, n, 2), 18-1)
				if got := rec.mgr.MocksLeftOut(); got != 1 {
					t.Fatalf("%d mocks left out counted, want the 1 exchange it could not frame", got)
				}
				if rec.stops != 0 {
					t.Fatalf("the connection's stop was stamped %d times, want never: its recording went on", rec.stops)
				}
				if got := rec.logs.FilterMessage(ownLine).FilterLevelExact(zapcore.WarnLevel).Len(); got != 1 {
					t.Fatalf("%d WARN lines %q, want 1", got, ownLine)
				}

				// The cut ran once, and the floor dropped the rest of the
				// response and the stale answer, numbered before the command
				// it cut at, however that answer was stamped.
				skips := rec.logs.FilterMessageSnippet(skipLine).All()
				if len(skips) != 1 {
					t.Fatalf("the rest of the response left out was skipped %d times, want once", len(skips))
				}
				want := int64(rest + len(stale[0]))
				if got := skips[0].ContextMap()["skippedBytes"]; got != want {
					t.Fatalf("the cut dropped %v bytes, want %d: the rest of the response left out (%d) and the stale answer (%d), both numbered before the command it cut at", got, want, rest, len(stale[0]))
				}

				// In one run of the floor's, which ends with the stale
				// answer: each stale byte leaves the stream once.
				floored := rec.logs.FilterMessageSnippet(floorLine).All()
				if len(floored) != 1 {
					t.Fatalf("the floor dropped %d run(s) of server bytes, want 1: the rest of the response left out with the stale answer", len(floored))
				}
				from, _ := floored[0].ContextMap()["from"].(time.Time)
				to, _ := floored[0].ContextMap()["to"].(time.Time)
				if !to.Equal(staleAt) || from.After(to) {
					t.Fatalf("the floor dropped a run captured from %v to %v, want one that ends with the stale answer, captured at %v", from, to, staleAt)
				}
			})
		}
	}
}

// The server's stream ends where the re-align looks for the answer to the
// command after a response left out: the floor drops a stale answer numbered
// before that command and stamped after it, and nothing follows it. That is the connection's end, with nothing more from the
// server: the recording ends there, with no error and no stop stamped, as any
// connection that ends does. Taken for lost framing, the end was reported as
// a stop of a connection that had nothing more to record.
func TestRecordV2_TheServersStreamEndingWhereTheAnswerTakenUpStartsEndsTheConnection(t *testing.T) {
	t.Parallel()
	stale := frame([][]byte{{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}})
	for _, n := range []int{512, 16384} {
		t.Run(fmt.Sprintf("reads of %d", n), func(t *testing.T) {
			t.Parallel()
			xs := kitTraffic(t, []int{7, 8}, false)
			// An empty packet where the column count should be: a response
			// the recorder cannot frame.
			xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
			// Then the stale answer, and the next command, which the server's
			// stream ends before answering.
			xs = append(xs[:3:3], exchange{reply: stale}, exchange{command: xs[3].command})
			client, server := onTheWire(xs, n)
			at := len(server) - 1
			if !bytes.Equal(server[at].Bytes, stale[0]) {
				t.Fatal("fixture: the stale answer is not the server's last chunk")
			}
			if !server[at].CapturedBefore(client[len(client)-1].ConnSeq) {
				t.Fatal("fixture: the stale answer is not numbered before the command after the response left out")
			}
			server[at].ReadAt = commandAt(t, xs, n, 4).Add(500 * time.Nanosecond)

			rec := recordChunks(t, client, server, true, time.Minute)
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
			}
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 returned=%v err=%v, want nil at the end of the connection: the server's stream ended", rec.returned, rec.err)
			}
			if rec.stops != 0 {
				t.Fatalf("the connection's stop was stamped %d times, want never: the connection ended", rec.stops)
			}
			if got := queryMocks(rec.mocks); got != 2 {
				t.Fatalf("%d query mocks, want the 2 before the response left out", got)
			}
		})
	}
}

// Two responses in a row that the recorder cannot frame cost their two
// exchanges, and the connection's recording goes on. The second is for a
// command it cannot size the response of (COM_FIELD_LIST), found before it
// reads a byte of that response: the answer the first cut left unread is
// dropped whole by the second, with no read between the two. Each
// exchange is counted once, and has the recorder's own line once; every other
// exchange is recorded with its own answer, and no stop is stamped.
//
// Not parallel: the WARNs are limited process-wide, and this test counts them.
func TestRecordV2_TwoResponsesInARowItCannotFrameCostTheirExchangesAlone(t *testing.T) {
	const tail = "; the exchange is left out, and the connection's recording goes on from its next command"
	for _, n := range []int{7, 512, 16384} {
		t.Run(fmt.Sprintf("reads of %d", n), func(t *testing.T) {
			resetWarnLimiters()
			xs := kitTraffic(t, []int{7, 8}, false) // 18 exchanges
			xs[2].reply = append([][]byte{{0x00, 0x00, 0x00, 0x01}}, xs[2].reply...)
			fieldList := exchange{command: wrapPacket(append([]byte{0x04}, "sessions\x00"...), 0),
				reply: frame(append(sessionColumns(t, "payload"), eofPayload))}
			xs = insert(xs, 3, fieldList)
			rec := recordWire(t, xs, n, true, time.Minute)
			if w := wrongMocks(t, rec.mocks); len(w) > 0 {
				t.Fatalf("%d wrong mock(s) recorded; the first: %s", len(w), w[0])
			}
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 returned=%v err=%v: each response it cannot frame must cost its own exchange, not the rest of the connection's recording", rec.returned, rec.err)
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
			for _, line := range []string{"V2: failed to decode mysql response head" + tail, "V2: unsupported mysql command" + tail} {
				if got := rec.logs.FilterMessage(line).FilterLevelExact(zapcore.WarnLevel).Len(); got != 1 {
					t.Fatalf("%d WARN lines %q, want 1", got, line)
				}
			}
			if got := rec.logs.FilterMessageSnippet("V2: skipped the rest of a mysql response left out").Len(); got != 2 {
				t.Fatalf("the server's stream was cut %d times, want once for each response left out", got)
			}
		})
	}
}
