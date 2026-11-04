package generic

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// captureOrder is a relay's capture order (relay.Relay.StreamedBefore,
// supervisor.Session.Order) for a row that puts the chunks on the two streams
// itself: it knows every chunk the connection captured (plan), and a chunk is
// on its stream once put.
type captureOrder struct {
	mu      sync.Mutex
	plan    []fakeconn.Chunk
	on      map[uint32]bool
	put     [2]int64
	changed chan struct{}
}

func newCaptureOrder(plan []fakeconn.Chunk) *captureOrder {
	return &captureOrder{plan: plan, on: map[uint32]bool{}, changed: make(chan struct{})}
}

func dirIndex(d fakeconn.Direction) int {
	if d == fakeconn.FromClient {
		return 0
	}
	return 1
}

// StreamedBefore is supervisor.Session.Order's.
func (o *captureOrder) StreamedBefore(dir fakeconn.Direction, seq uint32) (int64, bool, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if seq == 0 {
		// No number the relay gives: before it, as for any number not
		// given yet, a chunk of either direction may still come
		// (connseq.Upstream.PendingBefore).
		return 0, false, o.changed
	}
	for _, c := range o.plan {
		if c.Dir == dir && c.CapturedBefore(seq) && !o.on[c.ConnSeq] {
			return 0, false, o.changed
		}
	}
	return o.put[dirIndex(dir)], true, nil
}

// putOn puts c on its stream, ch.
func (o *captureOrder) putOn(ch chan<- fakeconn.Chunk, c fakeconn.Chunk) {
	ch <- c
	o.accept(c, int64(len(c.Bytes)))
}

// accept says c has gone on its stream, n bytes of it (none for a chunk the
// relay will never put there), whether or not it has.
func (o *captureOrder) accept(c fakeconn.Chunk, n int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.on[c.ConnSeq] = true
	o.put[dirIndex(c.Dir)] += n
	close(o.changed)
	o.changed = make(chan struct{})
}

// exchangeChunks is n exchanges numbered as the relay numbers them in wire
// order: request i is 2i+1, its answer 2i+2.
func exchangeChunks(n int) (reqs, resps []fakeconn.Chunk) {
	for i := 0; i < n; i++ {
		at := time.Unix(int64(7000+10*i), 0)
		reqs = append(reqs, fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(fmt.Sprintf("req%d", i)), ReadAt: at,
			ConnSeq: uint32(2*i + 1)}) // #nosec G115 -- test-only
		resps = append(resps, fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte(fmt.Sprintf("resp%d", i)),
			ReadAt: at.Add(time.Second), WrittenAt: at.Add(time.Second), ConnSeq: uint32(2*i + 2)}) // #nosec G115 -- test-only
	}
	return reqs, resps
}

// interleavings is every order of a's and b's chunks that keeps each one's own
// order: the orders two readers can hand on two streams in.
func interleavings(a, b []fakeconn.Chunk) [][]fakeconn.Chunk {
	if len(a) == 0 || len(b) == 0 {
		return [][]fakeconn.Chunk{append(append([]fakeconn.Chunk(nil), a...), b...)}
	}
	var all [][]fakeconn.Chunk
	for _, rest := range interleavings(a[1:], b) {
		all = append(all, append([]fakeconn.Chunk{a[0]}, rest...))
	}
	for _, rest := range interleavings(a, b[1:]) {
		all = append(all, append([]fakeconn.Chunk{b[0]}, rest...))
	}
	return all
}

func orderName(cs []fakeconn.Chunk) string {
	var s []string
	for _, c := range cs {
		s = append(s, string(c.Bytes))
	}
	return strings.Join(s, " ")
}

// pairsOf is each mock's requests and answers, "req|resp", sorted: the first
// exchange can be held until the connection's end (preambleHold), so the order
// mocks reach the session in is not the exchanges'.
func pairsOf(mocks []*models.Mock) []string {
	var got []string
	for _, m := range mocks {
		var req, resp []string
		for _, p := range m.Spec.GenericRequests {
			for _, msg := range p.Message {
				req = append(req, msg.Data)
			}
		}
		for _, p := range m.Spec.GenericResponses {
			for _, msg := range p.Message {
				resp = append(resp, msg.Data)
			}
		}
		got = append(got, strings.Join(req, ",")+"|"+strings.Join(resp, ","))
	}
	sort.Strings(got)
	return got
}

// orderedRun is what a recording saw.
type orderedRun struct {
	recorded []*models.Mock
	leftOut  int64
	returned bool
}

// recording is a connection the generic parser records from RecordOutgoing,
// under the capture order of its plan, with its mocks handed to the sync-mock
// manager when viaSyncMock is set (as in production) or to the session's
// channel.
type recording struct {
	t       *testing.T
	order   *captureOrder
	streams [2]*fakeconn.FakeConn
	chans   [2]chan fakeconn.Chunk
	ended   [2]bool // the stream has ended (endStream)
	want    [2]int64
	read    bool // the client's first chunk is put: the dest's reader starts once it is read
	mocks   chan *models.Mock
	mgr     *syncMock.SyncMockManager
	done    chan error
	got     []*models.Mock
	created string // the creation line of the parser's merge and readers
}

func startRecording(t *testing.T, plan []fakeconn.Chunk, viaSyncMock bool) *recording {
	t.Helper()
	return startRecordingUnder(t, plan, viaSyncMock, true)
}

// startRecordingUnder is startRecording, with the session told the capture
// order only when withOrder is set: without it the session has no
// Session.Order, as a producer that cannot say its order.
func startRecordingUnder(t *testing.T, plan []fakeconn.Chunk, viaSyncMock, withOrder bool) *recording {
	t.Helper()
	clientCh, destCh := make(chan fakeconn.Chunk, len(plan)), make(chan fakeconn.Chunk, len(plan))
	sess, mocks := newV2Session(t, clientCh, destCh)
	sess.Logger = zap.NewNop()
	order := newCaptureOrder(plan)
	if withOrder {
		sess.Order = order.StreamedBefore
	}
	mgr := syncMock.New(nil)
	sess.Mgr = mgr
	sess.Orphans = &syncMock.Spans{}
	if viaSyncMock {
		mgr.SetOutputChannel(mocks)
		sess.RouteMocksViaSyncMock = true
	}
	r := &recording{t: t, order: order, streams: [2]*fakeconn.FakeConn{sess.ClientStream, sess.DestStream},
		chans: [2]chan fakeconn.Chunk{clientCh, destCh}, mocks: mocks, mgr: mgr, done: make(chan error, 1)}
	created := make(chan string, 1)
	go func() {
		// RecordOutgoing runs encodeGenericV2 on this goroutine, which
		// starts the merge and the readers.
		created <- fmt.Sprintf("created by %s in goroutine %d", funcName(encodeGenericV2), goroutineID())
		r.done <- dispatch(t, sess)
	}()
	r.created = <-created
	return r
}

// put puts c on its stream, and says so in the capture order.
func (r *recording) put(c fakeconn.Chunk) {
	r.order.putOn(r.chans[dirIndex(c.Dir)], c)
	r.taken(c)
}

// push puts c on its stream, the capture order told before (accept).
func (r *recording) push(c fakeconn.Chunk) {
	r.chans[dirIndex(c.Dir)] <- c
	r.taken(c)
}

// taken waits until the reader of c's stream has taken every chunk put on it,
// c's included (the dest's reader starts once the client's first chunk is
// read, so none put before that is waited for).
func (r *recording) taken(c fakeconn.Chunk) {
	r.t.Helper()
	i := dirIndex(c.Dir)
	r.want[i] += int64(len(c.Bytes))
	r.read = r.read || i == 0
	if i == 1 && !r.read {
		return
	}
	bound := time.Now().Add(10 * time.Second) // only turns a hang into a failure
	for r.streams[i].ReadThrough() < r.want[i] {
		if time.Now().After(bound) {
			r.t.Fatalf("the %s's reader never took %q off its stream", c.Dir, c.Bytes)
		}
		time.Sleep(time.Millisecond)
	}
}

// recorded waits up to within for a mock whose pairs (pairsOf) include pair.
func (r *recording) recorded(pair string, within time.Duration) bool {
	bound := time.After(within)
	for {
		for _, p := range pairsOf(r.got) {
			if p == pair {
				return true
			}
		}
		select {
		case m := <-r.mocks:
			r.got = append(r.got, m)
		case <-bound:
			return false
		}
	}
}

// endStream ends stream i (0 the client's, 1 the dest's), as the relay ends
// one, if it has not ended.
func (r *recording) endStream(i int) {
	if !r.ended[i] {
		r.ended[i] = true
		close(r.chans[i])
	}
}

// waitBound is how long a row's wait on the parser's goroutines may take: it
// only turns a hang into a failure.
const waitBound = 10 * time.Second

// awaitWaiting waits until the reader of stream i (0 the client's, 1 the
// dest's) waits on a read with nothing buffered for it.
func (r *recording) awaitWaiting(i int) {
	r.t.Helper()
	bound := time.Now().Add(waitBound)
	for waiting, _ := r.streams[i].Waiting(); !waiting; waiting, _ = r.streams[i].Waiting() {
		if time.Now().After(bound) {
			r.t.Fatalf("setup: the reader of stream %d never waited on a read with nothing buffered", i)
		}
		time.Sleep(time.Millisecond) // a yield between looks
	}
}

// awaitMergeWaits waits until the recording's merge (mergeByCapture) is
// parked in a select: with nothing it can take or hand on, it waits on what
// the row does next.
func (r *recording) awaitMergeWaits() {
	r.t.Helper()
	merge := funcName(mergeByCapture)
	bound := time.Now().Add(waitBound)
	for {
		parked := 0
		for _, g := range strings.Split(allStacks(), "\n\n") {
			lines := strings.Split(g, "\n")
			if createdBy(lines, r.created) && inFunc(lines[1:], merge) && parkedOnAChannel(lines[0]) {
				parked++
			}
		}
		if parked > 0 {
			return
		}
		if time.Now().After(bound) {
			r.t.Fatal("setup: the merge never parked with nothing it could take or hand on")
		}
		time.Sleep(time.Millisecond) // a yield between looks
	}
}

// awaitReaders waits until n of the recording's stream readers (readStream)
// are alive: a reader ends once it has handed its stream's end on.
func (r *recording) awaitReaders(n int) {
	r.t.Helper()
	bound := time.Now().Add(waitBound)
	for {
		alive := 0
		for _, g := range strings.Split(allStacks(), "\n\n") {
			lines := strings.Split(g, "\n")
			if createdBy(lines, r.created) && inFunc(lines[1:], readStreamFunc) {
				alive++
			}
		}
		if alive == n {
			return
		}
		if time.Now().After(bound) {
			r.t.Fatalf("setup: %d of the recording's stream readers are alive, never %d", alive, n)
		}
		time.Sleep(time.Millisecond) // a yield between looks
	}
}

// end ends both streams, as the relay's teardown does, and waits for the
// parser to return.
func (r *recording) end() orderedRun {
	r.t.Helper()
	r.endStream(0)
	r.endStream(1)
	var out orderedRun
	select {
	case err := <-r.done:
		if err != nil {
			r.t.Fatalf("the parser returned %v", err)
		}
		out.returned = true
	case <-time.After(10 * time.Second): // only turns a hang into a failure
		return out
	}
	for drained := false; !drained; {
		select {
		case m := <-r.mocks:
			r.got = append(r.got, m)
		case <-time.After(50 * time.Millisecond):
			drained = true
		}
	}
	out.recorded, out.leftOut = r.got, r.mgr.MocksLeftOut()
	return out
}

// runInReadOrder records a connection under the capture order of plan,
// putting the chunks of puts on their streams in that order, each once the
// reader of its stream has taken every chunk put before it, so the readers
// hand them on in the order put; then it ends both streams.
func runInReadOrder(t *testing.T, plan, puts []fakeconn.Chunk, viaSyncMock bool) orderedRun {
	t.Helper()
	r := startRecording(t, plan, viaSyncMock)
	for _, c := range puts {
		r.put(c)
	}
	return r.end()
}

// The generic parser pairs a connection's exchanges in the order the relay
// captured them, whatever order its two readers hand the chunks on in: each of
// the 20 orders two readers can take three exchanges' chunks in records the
// three, each request with its own answer, and leaves nothing out. Before, it
// paired them in the order read: a run of answers read ahead of their requests
// (req0, then resp0 resp1 resp2, then req1 req2: a parser behind its capture,
// both streams full, the scheduler running the dest's reader first) recorded
// two of the three, the third lost with nothing left out or said.
func TestV2_PairsEveryExchangeInCaptureOrderWhateverOrderItsReadersTakeThem(t *testing.T) {
	reqs, resps := exchangeChunks(3)
	plan := append(append([]fakeconn.Chunk(nil), reqs...), resps...)
	want := []string{"req0|resp0", "req1|resp1", "req2|resp2"}
	orders := interleavings(reqs, resps)
	if len(orders) != 20 {
		t.Fatalf("setup: %d orders, want 20", len(orders))
	}
	for _, viaSyncMock := range []bool{true, false} {
		for _, puts := range orders {
			t.Run(fmt.Sprintf("viaSyncMock=%v/%s", viaSyncMock, orderName(puts)), func(t *testing.T) {
				r := runInReadOrder(t, plan, puts, viaSyncMock)
				if !r.returned {
					t.Fatal("the parser did not return once both streams ended")
				}
				if got := pairsOf(r.recorded); strings.Join(got, " ") != strings.Join(want, " ") || r.leftOut != 0 {
					t.Fatalf("recorded %q and left out %d; want %q, nothing left out", got, r.leftOut, want)
				}
			})
		}
	}
}

// A request the merge holds while a chunk of the dest's captured before it is
// still to come goes on once the dest's stream ends, though that chunk never
// reached it (the relay numbered it and ended the stream at the recording's
// stop with it still in hand): a stream's end ends every wait on it. Here the
// request after it has no answer, so the connection's one mock is the first
// exchange, and the parser returns.
func TestV2_AStreamsEndEndsTheMergesWaitOnIt(t *testing.T) {
	reqs, resps := exchangeChunks(2)
	lost := fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("lost"), ConnSeq: 3}
	reqs[1].ConnSeq = 4
	plan := []fakeconn.Chunk{reqs[0], resps[0], lost, reqs[1]}
	for _, viaSyncMock := range []bool{true, false} {
		t.Run(fmt.Sprint("viaSyncMock=", viaSyncMock), func(t *testing.T) {
			r := runInReadOrder(t, plan, []fakeconn.Chunk{reqs[0], resps[0], reqs[1]}, viaSyncMock)
			if !r.returned {
				t.Fatal("the parser did not return once both streams ended: the merge still waits on the dest's chunk captured before the last request")
			}
			if got := pairsOf(r.recorded); len(got) != 1 || got[0] != "req0|resp0" || r.leftOut != 0 {
				t.Fatalf("recorded %q and left out %d; want the first exchange alone", got, r.leftOut)
			}
		})
	}
}

// An answer the merge takes once the client's stream has ended goes on at
// once, though the capture order still waits on a client chunk captured
// before it that never reached the stream (the relay numbered it and ended
// the stream with it in hand): an ended direction hands on nothing more, so
// nothing of it can come before the answer. The client's end reaches the
// merge before the answer is put: the merge, with both readers waiting on a
// read, has taken everything they sent and waits on both, so the client's
// reader hands its end straight to it, and that reader has ended before the
// answer goes on its stream. So the answer is the merge's one head, and the
// end is not one beside it. Were the end not looked at, the merge would wait
// on the order for good: the exchange not recorded, and the parser not
// returning at the streams' end.
func TestV2_AnAnswerTakenAfterTheClientsEndWaitsOnNoClientChunk(t *testing.T) {
	reqs, resps := exchangeChunks(2)
	lost := fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("lost"), ConnSeq: 4}
	resps[1].ConnSeq = 5
	plan := []fakeconn.Chunk{reqs[0], resps[0], reqs[1], lost, resps[1]}
	for _, viaSyncMock := range []bool{true, false} {
		t.Run(fmt.Sprint("viaSyncMock=", viaSyncMock), func(t *testing.T) {
			r := startRecording(t, plan, viaSyncMock)
			r.put(reqs[0])
			r.put(resps[0])
			r.put(reqs[1])
			r.awaitWaiting(0)
			r.awaitWaiting(1)
			r.awaitMergeWaits()
			r.endStream(0)
			r.awaitReaders(1)
			r.put(resps[1])
			ok := r.recorded("req1|resp1", 5*time.Second)
			got := r.end()
			if !ok {
				t.Fatalf("req1's mock was not recorded with the dest's stream open, its answer taken after the client's end (recorded %q by the end)", pairsOf(got.recorded))
			}
			if !got.returned {
				t.Fatal("the parser did not return once both streams ended")
			}
			if p := pairsOf(got.recorded); strings.Join(p, " ") != "req0|resp0 req1|resp1" || got.leftOut != 0 {
				t.Fatalf("recorded %q and left out %d; want both exchanges, nothing left out", p, got.leftOut)
			}
		})
	}
}

// An answer the merge holds, waiting on a client chunk captured before it,
// goes on ahead of an unnumbered client chunk received after it: the merge
// cannot place a chunk with no number, so the two go on in the order it
// received them. Were the client's head taken first whatever its arrival,
// the unnumbered request would go on ahead of the answer it came after, and
// the answer would still wait: req1's exchange not recorded with the streams
// open, and paired with the later request too.
func TestV2_AHeldAnswerGoesOnAheadOfAnUnnumberedChunkReceivedAfterIt(t *testing.T) {
	reqs, resps := exchangeChunks(3)
	lost := fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("lost"), ConnSeq: 4}
	resps[1].ConnSeq = 5
	reqs[2].ConnSeq = 0
	plan := []fakeconn.Chunk{reqs[0], resps[0], reqs[1], lost, resps[1]}
	for _, viaSyncMock := range []bool{true, false} {
		t.Run(fmt.Sprint("viaSyncMock=", viaSyncMock), func(t *testing.T) {
			r := startRecording(t, plan, viaSyncMock)
			r.put(reqs[0])
			r.put(resps[0])
			r.put(reqs[1])
			r.put(resps[1])
			r.awaitWaiting(0)
			r.awaitWaiting(1)
			r.awaitMergeWaits()
			r.push(reqs[2])
			ok := r.recorded("req1|resp1", 5*time.Second)
			got := r.end()
			if !ok {
				t.Fatalf("req1's mock was not recorded with the streams open, its answer held ahead of an unnumbered request received after it (recorded %q by the end)", pairsOf(got.recorded))
			}
			if !got.returned {
				t.Fatal("the parser did not return once both streams ended")
			}
			if p := pairsOf(got.recorded); strings.Join(p, " ") != "req0|resp0 req1|resp1" || got.leftOut != 0 {
				t.Fatalf("recorded %q and left out %d; want the two answered exchanges, nothing left out", p, got.leftOut)
			}
		})
	}
}

// A held answer goes on behind a later chunk of its request, captured before
// it, though the chunk reaches the merge after it: with both heads numbered,
// their capture numbers decide, whatever the order the merge received them
// in. Here the client's reader is behind: the answer waits for the request's
// second chunk, which the capture order says is on the client's stream, and
// that chunk comes once the merge holds the answer. Were the arrival to
// decide, the answer would go on between the request's two chunks: the
// exchange recorded with its first chunk only, and the second lost with
// nothing left out.
func TestV2_AHeldAnswerGoesOnBehindARequestChunkCapturedBeforeIt(t *testing.T) {
	reqs, resps := exchangeChunks(2)
	second := fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("req1b"), ReadAt: reqs[1].ReadAt, ConnSeq: 4}
	resps[1].ConnSeq = 5
	plan := []fakeconn.Chunk{reqs[0], resps[0], reqs[1], second, resps[1]}
	for _, viaSyncMock := range []bool{true, false} {
		t.Run(fmt.Sprint("viaSyncMock=", viaSyncMock), func(t *testing.T) {
			r := startRecording(t, plan, viaSyncMock)
			r.put(reqs[0])
			r.put(resps[0])
			r.put(reqs[1])
			r.order.accept(second, int64(len(second.Bytes)))
			r.put(resps[1])
			r.awaitWaiting(0)
			r.awaitWaiting(1)
			r.awaitMergeWaits()
			r.push(second)
			got := r.end()
			if !got.returned {
				t.Fatal("the parser did not return once both streams ended")
			}
			if p := pairsOf(got.recorded); strings.Join(p, " ") != "req0|resp0 req1,req1b|resp1" || got.leftOut != 0 {
				t.Fatalf("recorded %q and left out %d; want the second exchange whole, both its request's chunks with its answer", p, got.leftOut)
			}
		})
	}
}

// A chunk the capture order says is on its stream is waited for until its
// reader hands it on: with the dest's reader ahead (resp1 resp2 read while
// req1 req2, captured before them, are queued and not yet read), the answers
// wait for their requests, and every exchange is recorded with its own
// answer. Were the order's word taken alone, the answers would be paired as
// read, and req2's lost.
func TestV2_TheMergeWaitsForAChunkOnItsStreamItsReaderHasNotHandedOn(t *testing.T) {
	reqs, resps := exchangeChunks(3)
	plan := append(append([]fakeconn.Chunk(nil), reqs...), resps...)
	for _, viaSyncMock := range []bool{true, false} {
		t.Run(fmt.Sprint("viaSyncMock=", viaSyncMock), func(t *testing.T) {
			r := startRecording(t, plan, viaSyncMock)
			r.put(reqs[0])
			r.put(resps[0])
			r.order.accept(reqs[1], int64(len(reqs[1].Bytes)))
			r.order.accept(reqs[2], int64(len(reqs[2].Bytes)))
			r.put(resps[1])
			r.put(resps[2])
			time.Sleep(100 * time.Millisecond) // room for a merge that does not wait to hand the answers on
			r.push(reqs[1])
			r.push(reqs[2])
			got := r.end()
			if !got.returned {
				t.Fatal("the parser did not return once both streams ended")
			}
			if p := pairsOf(got.recorded); strings.Join(p, " ") != "req0|resp0 req1|resp1 req2|resp2" || got.leftOut != 0 {
				t.Fatalf("recorded %q and left out %d; want every exchange with its own answer", p, got.leftOut)
			}
		})
	}
}

// An answer goes on as soon as nothing captured before it can still come, not
// at the next request or the connection's end: once the relay says the client
// chunk numbered before it will never reach its stream (refused), the second
// exchange's mock is recorded with the streams still open, and so is one whose
// chunks have no number (a producer that does not number them, merged as
// read), and one recorded on a session with no capture order (Session.Order
// unset: merged as read). A mock recorded late lands in the window of a test
// that did not make its call.
func TestV2_AnAnswerGoesOnOnceNothingCapturedBeforeItCanCome(t *testing.T) {
	for _, viaSyncMock := range []bool{true, false} {
		t.Run(fmt.Sprint("refused/viaSyncMock=", viaSyncMock), func(t *testing.T) {
			reqs, resps := exchangeChunks(2)
			refused := fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("refused"), ConnSeq: 4}
			resps[1].ConnSeq = 5
			r := startRecording(t, []fakeconn.Chunk{reqs[0], resps[0], reqs[1], refused, resps[1]}, viaSyncMock)
			r.put(reqs[0])
			r.put(resps[0])
			r.put(reqs[1])
			r.put(resps[1])
			r.order.accept(refused, 0)
			ok := r.recorded("req1|resp1", 5*time.Second)
			got := r.end()
			if !ok {
				t.Fatalf("req1's mock was not recorded with the streams open, once nothing captured before its answer could come (recorded %q by the end)", pairsOf(got.recorded))
			}
		})
		t.Run(fmt.Sprint("unnumbered/viaSyncMock=", viaSyncMock), func(t *testing.T) {
			reqs, resps := exchangeChunks(2)
			for i := range reqs {
				reqs[i].ConnSeq, resps[i].ConnSeq = 0, 0
			}
			r := startRecording(t, nil, viaSyncMock)
			for i := range reqs {
				r.put(reqs[i])
				r.put(resps[i])
			}
			ok := r.recorded("req1|resp1", 5*time.Second)
			got := r.end()
			if !ok {
				t.Fatalf("req1's mock was not recorded with the streams open (recorded %q by the end)", pairsOf(got.recorded))
			}
		})
		t.Run(fmt.Sprint("no capture order/viaSyncMock=", viaSyncMock), func(t *testing.T) {
			reqs, resps := exchangeChunks(2)
			plan := []fakeconn.Chunk{reqs[0], resps[0], reqs[1], resps[1]}
			r := startRecordingUnder(t, plan, viaSyncMock, false)
			for i := range reqs {
				r.put(reqs[i])
				r.put(resps[i])
			}
			ok := r.recorded("req1|resp1", 5*time.Second)
			got := r.end()
			if !ok {
				t.Fatalf("req1's mock was not recorded with the streams open, on a session that cannot say its capture order (recorded %q by the end)", pairsOf(got.recorded))
			}
		})
	}
}

// The connection's first chunk, the client's, read before the readers start,
// goes on ahead of everything the dest's reader hands on. Here a request and
// its reply are both on their streams before the parser starts, and neither
// is numbered, so the merge cannot place them by capture; and the merge starts
// only once the dest's reader has queued the reply (testHookBeforeMerge), so
// its first look finds the reply ready. Each of 64 connections records its
// one exchange, with nothing left out. Handed to the merge through the
// client's queue instead, the request was one of two events ready at once,
// and a select picks either: the request lost to the reply about one time in
// two, the reply was then paired with no request, and the connection
// recorded nothing.
func TestV2_TheFirstClientChunkGoesOnAheadOfTheReply(t *testing.T) {
	at := time.Unix(6000, 0)
	var dest *fakeconn.FakeConn // the connection's dest stream, set before its parser starts
	started := make(chan bool, 1)
	prev := testHookBeforeMerge
	testHookBeforeMerge = func() {
		bound := time.Now().Add(waitBound)
		for waiting, _ := dest.Waiting(); !waiting; waiting, _ = dest.Waiting() {
			if time.Now().After(bound) {
				started <- false
				return
			}
			time.Sleep(time.Millisecond) // a yield between looks
		}
		started <- true
	}
	t.Cleanup(func() { testHookBeforeMerge = prev })
	for i := 0; i < 64; i++ {
		clientCh, destCh := make(chan fakeconn.Chunk, 1), make(chan fakeconn.Chunk, 1)
		clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("req0"), ReadAt: at, WrittenAt: at}
		destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("resp0"), ReadAt: at, WrittenAt: at}
		sess, mocks := newV2Session(t, clientCh, destCh)
		sess.Logger = zap.NewNop()
		mgr := syncMock.New(nil)
		sess.Mgr = mgr
		dest = sess.DestStream
		done := make(chan error, 1)
		go func() { done <- dispatch(t, sess) }()
		select {
		case ok := <-started:
			if !ok {
				t.Fatalf("setup: connection %d: the dest's reader never queued the reply", i)
			}
		case <-time.After(waitBound):
			t.Fatalf("setup: connection %d: the parser never came to start its merge", i)
		}
		close(clientCh)
		close(destCh)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("connection %d: the parser returned %v", i, err)
			}
		case <-time.After(waitBound):
			t.Fatalf("connection %d: the parser did not return once both streams ended", i)
		}
		var got []*models.Mock
		for len(mocks) > 0 {
			got = append(got, <-mocks)
		}
		if p := pairsOf(got); len(p) != 1 || p[0] != "req0|resp0" || mgr.MocksLeftOut() != 0 {
			t.Fatalf("connection %d recorded %q and left out %d; want its one exchange", i, p, mgr.MocksLeftOut())
		}
	}
}
