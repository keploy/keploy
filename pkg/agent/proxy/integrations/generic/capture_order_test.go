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
	want    [2]int64
	read    bool // the client's first chunk is put: the dest's reader starts once it is read
	mocks   chan *models.Mock
	mgr     *syncMock.SyncMockManager
	done    chan error
	got     []*models.Mock
}

func startRecording(t *testing.T, plan []fakeconn.Chunk, viaSyncMock bool) *recording {
	t.Helper()
	clientCh, destCh := make(chan fakeconn.Chunk, len(plan)), make(chan fakeconn.Chunk, len(plan))
	sess, mocks := newV2Session(t, clientCh, destCh)
	sess.Logger = zap.NewNop()
	order := newCaptureOrder(plan)
	sess.Order = order.StreamedBefore
	mgr := syncMock.New(nil)
	sess.Mgr = mgr
	sess.Orphans = &syncMock.Spans{}
	if viaSyncMock {
		mgr.SetOutputChannel(mocks)
		sess.RouteMocksViaSyncMock = true
	}
	r := &recording{t: t, order: order, streams: [2]*fakeconn.FakeConn{sess.ClientStream, sess.DestStream},
		chans: [2]chan fakeconn.Chunk{clientCh, destCh}, mocks: mocks, mgr: mgr, done: make(chan error, 1)}
	go func() { r.done <- dispatch(t, sess) }()
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

// end ends both streams, as the relay's teardown does, and waits for the
// parser to return.
func (r *recording) end() orderedRun {
	r.t.Helper()
	close(r.chans[0])
	close(r.chans[1])
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
// read). A mock recorded late lands in the window of a test that did not make
// its call.
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
	}
}
