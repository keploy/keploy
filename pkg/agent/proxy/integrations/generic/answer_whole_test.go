package generic

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// relayReadSize is how much of a destination's answer the relay reads at a
// time: an answer over it reaches the parser in more than one chunk.
const relayReadSize = 32 * 1024

// answerParts are answers a server sends in more than one chunk: one over the
// relay's read, and a header and a body written apart.
var answerParts = []struct {
	name  string
	parts [][]byte
}{
	{"over the relay's read", [][]byte{bytes.Repeat([]byte{'a'}, relayReadSize), bytes.Repeat([]byte{'b'}, 5000)}},
	{"header and body written apart", [][]byte{[]byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\n"), []byte("body")}},
}

// partsChunks is a request, its answer in parts (each read a second after the
// one before it, its write returned 100ms after its read), and, withNext, the
// connection's next request and its one-chunk answer.
func partsChunks(parts [][]byte, withNext bool) (chunks []fakeconn.Chunk, lastSent time.Time) {
	t0 := time.Unix(9000, 0)
	chunks = []fakeconn.Chunk{{Dir: fakeconn.FromClient, Bytes: []byte("GET big\r\n"), ReadAt: t0, ConnSeq: 1}}
	for i, p := range parts {
		read := t0.Add(time.Duration(i+1) * time.Second)
		lastSent = read.Add(100 * time.Millisecond)
		chunks = append(chunks, fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: p, ReadAt: read, WrittenAt: lastSent, ConnSeq: uint32(i + 2)})
	}
	if withNext {
		next := t0.Add(time.Duration(len(parts)+1) * time.Second)
		n := uint32(len(parts) + 2)
		chunks = append(chunks,
			fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("GET small\r\n"), ReadAt: next, ConnSeq: n},
			fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("+OK\r\n"), ReadAt: next.Add(time.Second), WrittenAt: next.Add(time.Second), ConnSeq: n + 1})
	}
	return chunks, lastSent
}

func strs(parts [][]byte) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

// checkWhole fails t unless m is the request with every part of its answer,
// its answer ending when the relay's write of the last part returned, and r
// left nothing out.
func checkWhole(t *testing.T, m *models.Mock, parts [][]byte, lastSent time.Time, r tailRun) {
	t.Helper()
	assertExchange(t, m, []string{"GET big\r\n"}, strs(parts))
	if !m.Spec.ResTimestampMock.Equal(lastSent) {
		t.Errorf("the answer ends at %v, want the last part's write, %v", m.Spec.ResTimestampMock, lastSent)
	}
	if len(r.leftOut) != 0 || r.counted != 0 {
		t.Errorf("left out %q (the manager counts %d), want nothing", r.leftOut, r.counted)
	}
	if c, o := r.spans.Counts(); c != 0 || o != 0 {
		t.Errorf("the spans hold (closed=%d, open=%d), want none", c, o)
	}
}

// An answer the server sends in more than one chunk is recorded whole, with
// the exchange it ends, whether the connection's next request or its end is
// what ends it, on both deliveries and under both switches. Before, the
// exchange was recorded with the answer's first chunk and the rest left out,
// so the test case over it was left out of the recording.
func TestV2_AnAnswerInPartsIsRecordedWhole(t *testing.T) {
	for _, a := range answerParts {
		for _, withNext := range []bool{true, false} {
			for _, proven := range []string{"0", "1"} {
				for _, viaSyncMock := range []bool{false, true} {
					name := a.name + "/next=" + map[bool]string{false: "end", true: "request"}[withNext] +
						"/proven=" + proven + "/viaSyncMock=" + map[bool]string{false: "no", true: "yes"}[viaSyncMock]
					t.Run(name, func(t *testing.T) {
						t.Setenv(provenswitch.Env, proven)
						chunks, lastSent := partsChunks(a.parts, withNext)
						want := map[bool]int{false: 1, true: 2}[withNext]
						r := recordChunks(t, chunks, viaSyncMock, want)
						if len(r.recorded) != want {
							t.Fatalf("recorded %d mocks, want %d", len(r.recorded), want)
						}
						checkWhole(t, r.recorded[0], a.parts, lastSent, r)
						if withNext {
							assertExchange(t, r.recorded[1], []string{"GET small\r\n"}, []string{"+OK\r\n"})
						}
					})
				}
			}
		}
	}
}

// An answer in parts teed ahead of its request is that request's whole
// answer, as in wire order: the bytes a test replays do not depend on which
// way the relay's two goroutines ran.
func TestV2_AnAnswerInPartsTeedAheadOfItsRequestIsRecordedWhole(t *testing.T) {
	t.Parallel()
	spans := &syncMock.Spans{}
	got, counted := pairCounted(t, spans, false, []chunkEvent{
		destChunk("head", 2, time.Unix(1001, 0)),
		destChunk("tail", 3, time.Unix(1002, 0)),
		clientChunk("GET big\r\n", 1, time.Unix(1000, 0)),
		clientChunk("GET small\r\n", 4, time.Unix(1003, 0)),
		destChunk("+OK\r\n", 5, time.Unix(1004, 0)),
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 mocks, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"GET big\r\n"}, []string{"head", "tail"})
	assertExchange(t, got[1], []string{"GET small\r\n"}, []string{"+OK\r\n"})
	if !got[0].Spec.ResTimestampMock.Equal(time.Unix(1002, 0)) {
		t.Errorf("the answer ends at %v, want the tail's write", got[0].Spec.ResTimestampMock)
	}
	if c, o := spans.Counts(); counted != 0 || c != 0 || o != 0 {
		t.Fatalf("left out %d (spans closed=%d, open=%d), want nothing", counted, c, o)
	}
}

// pooledRun records the last exchange of a pooled connection: its request and
// an answer in two parts, then nothing more while the connection stays open,
// until the recording's stop begins. The mocks go to an unbuffered channel,
// read only once the stop has begun, so the parser still has the exchange
// when the stop asks what is pending. after, when set, is one more chunk of
// the answer the server sends once the exchange was handed on.
func pooledRun(t *testing.T, viaSyncMock bool, after []byte) (m *models.Mock, pendingAtStop syncMock.BacklogState, r tailRun) {
	t.Helper()
	t0 := time.Unix(6000, 0)
	stop := t0.Add(10 * time.Second)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetWatermark(neverBehind{})
	clientCh := make(chan fakeconn.Chunk, 2)
	destCh := make(chan fakeconn.Chunk, 4)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("GET big\r\n"), ReadAt: t0, ConnSeq: 1}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("head"), ReadAt: t0.Add(time.Second), WrittenAt: t0.Add(time.Second), ConnSeq: 2}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("tail"), ReadAt: t0.Add(2 * time.Second), WrittenAt: t0.Add(2 * time.Second), ConnSeq: 3}
	sess, _ := newV2Session(t, clientCh, destCh)
	sess.Ctx = syncMock.NewContext(context.Background(), mgr)
	sess.Mgr = mgr
	core, logs := observer.New(zapcore.DebugLevel)
	sess.Logger = zap.New(core)
	r.spans = &syncMock.Spans{}
	sess.Orphans = r.spans
	mocks := make(chan *models.Mock)
	if viaSyncMock {
		mgr.SetOutputChannel(mocks)
		sess.RouteMocksViaSyncMock = true
	} else {
		sess.Mocks = mocks
	}
	// Each answer chunk read and held clears the session's pending work.
	clears := make(chan struct{}, 8)
	sess.OnPendingCleared = func() { clears <- struct{}{} }
	done := make(chan error, 1)
	go func() { done <- dispatch(t, sess) }()
	// A row that fails early ends the parser too: its streams end, and the
	// mocks it hands on are taken, or it would wait on them for good.
	ended := false
	end := func() {
		if !ended {
			ended = true
			close(clientCh)
			close(destCh)
		}
	}
	t.Cleanup(func() {
		if !ended {
			end()
			awaitParser(mocks, done)
		}
	})
	for i := 0; i < 2; i++ {
		select {
		case <-clears:
		case <-time.After(10 * time.Second):
			t.Fatalf("the parser did not hold the answer's %d chunks read", 2)
		}
	}
	mgr.BacklogBefore(stop) // the stop begins
	pendingAtStop = mgr.BacklogBefore(stop)
	select {
	case m = <-mocks:
	case <-time.After(10 * time.Second):
		t.Fatal("the exchange held was not handed on as the stop began")
	}
	if after != nil {
		destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: after, ReadAt: t0.Add(3 * time.Second), WrittenAt: t0.Add(3 * time.Second), ConnSeq: 4}
	}
	end()
	if n := endOfParser(t, mocks, done); n != 0 {
		t.Fatalf("handed on %d more mocks after the one the stop decided", n)
	}
	if st := mgr.BacklogBefore(stop); st.Pending {
		t.Fatalf("handed on, the stop still finds it pending: %+v", st)
	}
	for _, e := range logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			reason, _ := e.ContextMap()["reason"].(string)
			r.leftOut = append(r.leftOut, reason)
		}
	}
	r.counted = mgr.MocksLeftOut()
	return m, pendingAtStop, r
}

// The last exchange of a pooled connection has no next request to end its
// answer, and the connection outlives the recording: the stop finds it
// pending while the parser holds it, and as the stop begins the parser hands
// it on with its answer whole, on both deliveries and under both switches.
// Held unseen, it would miss the stop.
func TestV2_ThePooledConnectionsLastExchangeIsHandedOnWholeAsTheStopBegins(t *testing.T) {
	for _, proven := range []string{"0", "1"} {
		for _, viaSyncMock := range []bool{false, true} {
			t.Run("proven="+proven+"/viaSyncMock="+map[bool]string{false: "no", true: "yes"}[viaSyncMock], func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				m, st, r := pooledRun(t, viaSyncMock, nil)
				if !st.Pending {
					t.Errorf("the stop found nothing pending while the parser held the exchange: %+v", st)
				}
				assertExchange(t, m, []string{"GET big\r\n"}, []string{"head", "tail"})
				if len(r.leftOut) != 0 || r.counted != 0 {
					t.Errorf("left out %q (the manager counts %d), want nothing", r.leftOut, r.counted)
				}
			})
		}
	}
}

// A chunk of the answer the server sends after the stop handed its exchange
// on is left out with its span, so a test case over it is left out, never
// saved over the answer cut short.
func TestV2_AChunkOfAnAnswerAfterTheStopDecidedItIsLeftOut(t *testing.T) {
	for _, viaSyncMock := range []bool{false, true} {
		t.Run("viaSyncMock="+map[bool]string{false: "no", true: "yes"}[viaSyncMock], func(t *testing.T) {
			m, _, r := pooledRun(t, viaSyncMock, []byte("more"))
			assertExchange(t, m, []string{"GET big\r\n"}, []string{"head", "tail"})
			if len(r.leftOut) != 1 || r.leftOut[0] != leftOutAnswerTail || r.counted != 1 {
				t.Fatalf("left out %q (the manager counts %d), want the chunk after the stop, for %q", r.leftOut, r.counted, leftOutAnswerTail)
			}
			at := time.Unix(6003, 0)
			if over, _ := r.spans.Overlaps(at, at); !over {
				t.Fatal("a test case over the chunk left out is not left out")
			}
			if over, _ := r.spans.Overlaps(time.Unix(6000, 0), at.Add(-time.Millisecond)); over {
				t.Fatal("the span starts before the chunk left out: it takes in the exchange recorded whole")
			}
		})
	}
}

// The stop decides what the parser holds however its answer came, and only
// what it holds: an answer in parts teed ahead of its request, the
// connection's last exchange, is pending at the stop and handed on whole as
// it begins; a server's banner, with no request before it, is no exchange,
// so the stop finds nothing pending, and it is left out at the streams' end.
func TestV2_TheStopDecidesAnAnswerHeldHoweverItCameAndNothingElse(t *testing.T) {
	t0 := time.Unix(7000, 0)
	stop := t0.Add(10 * time.Second)
	for _, tc := range []struct {
		name   string
		evs    []chunkEvent
		parts  []string // the exchange handed on as the stop begins; none: nothing is
		leftAt time.Time
	}{
		{"teed ahead of its request", []chunkEvent{
			destChunk("head", 2, t0.Add(time.Second)),
			destChunk("tail", 3, t0.Add(2*time.Second)),
			clientChunk("GET big\r\n", 1, t0),
		}, []string{"head", "tail"}, time.Time{}},
		{"a banner with no request", []chunkEvent{
			destChunk("220 ready\r\n", 1, t0.Add(time.Second)),
		}, nil, t0.Add(time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := syncMock.New(zap.NewNop())
			mgr.SetWatermark(neverBehind{})
			spans := &syncMock.Spans{}
			mocks := make(chan *models.Mock) // read once the stop has begun
			sess := &supervisor.Session{Mocks: mocks, Logger: zap.NewNop(), Mgr: mgr, Orphans: spans,
				Ctx: syncMock.NewContext(context.Background(), mgr), ClientConnID: "test-client-conn"}
			events := make(chan chunkEvent)
			done := make(chan error, 1)
			go func() { done <- pairChunkEvents(events, sess, zap.NewNop()) }()
			closed := false
			t.Cleanup(func() {
				if !closed {
					close(events)
					awaitParser(mocks, done)
				}
			})
			// The parser takes its events one at a time: with the empty one
			// last taken, it is done with those before it. A mock it hands
			// on before the stop was not held.
			for _, ev := range append(tc.evs, chunkEvent{dir: fakeconn.FromDest}) {
				select {
				case events <- ev:
				case m := <-mocks:
					t.Fatalf("handed on %d response chunk(s) before the stop began: the answer was not held", len(m.Spec.GenericResponses))
				}
			}
			mgr.BacklogBefore(stop) // the stop begins
			st := mgr.BacklogBefore(stop)
			if st.Pending != (tc.parts != nil) {
				t.Fatalf("the stop finds pending %v (%+v), want %v", st.Pending, st, tc.parts != nil)
			}
			if tc.parts != nil {
				select {
				case m := <-mocks:
					assertExchange(t, m, []string{"GET big\r\n"}, tc.parts)
				case <-time.After(10 * time.Second):
					t.Fatal("the exchange held was not handed on as the stop began")
				}
			}
			closed = true
			close(events)
			if n := endOfParser(t, mocks, done); n != 0 {
				t.Fatalf("handed on %d more mocks at the streams' end, want none", n)
			}
			if st := mgr.BacklogBefore(stop); st.Pending {
				t.Fatalf("at the streams' end, the stop still finds something pending: %+v", st)
			}
			if c, o := spans.Counts(); tc.leftAt.IsZero() != (c == 0 && o == 0) {
				t.Fatalf("the spans hold closed=%d, open=%d; want a span left out only for the banner", c, o)
			}
			if over, _ := spans.Overlaps(tc.leftAt, tc.leftAt); !tc.leftAt.IsZero() && !over {
				t.Fatal("the banner left out has no span")
			}
		})
	}
}

// awaitParser waits for a parser whose input has ended to return, taking the
// mocks it still hands on: a row that failed before it read them would
// otherwise leave the parser waiting on them, and hang instead of failing.
func awaitParser(mocks <-chan *models.Mock, done <-chan error) {
	for {
		select {
		case <-mocks:
		case <-done:
			return
		}
	}
}

// endOfParser waits for a parser whose input has ended to return, and says
// how many more mocks it handed on before it did.
func endOfParser(t *testing.T, mocks <-chan *models.Mock, done <-chan error) (more int) {
	t.Helper()
	for {
		select {
		case <-mocks:
			more++
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return more
		}
	}
}

// A test case can be streamed while the parser still holds an exchange its
// test made: the test's verdict does not wait on the hold, which on a pooled
// connection ends at a next request that may come only after the test, or at
// the recording's stop. Once handed on, whole, the mock reaches that test by
// its request time, as any mock that comes after its test case went: under
// the proven switch the test's record-time mapping (--sync) names it, as a
// late mapping, and so does the mapping keploy mock record derives from the
// recorded times, whether the connection's next request or the stop ends the
// answer. With the switch off a Generic exchange is a session mock, held or
// not, which no test's mapping names: it reaches the recording all the same.
func TestV2_AnExchangeHeldPastItsTestCaseStillReachesThatTest(t *testing.T) {
	for _, proven := range []string{"0", "1"} {
		for _, endedBy := range []string{"the next request", "the stop"} {
			t.Run("proven="+proven+"/ended by "+endedBy, func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				heldPastItsTestCase(t, endedBy == "the stop")
			})
		}
	}
}

func heldPastItsTestCase(t *testing.T, byStop bool) {
	t.Helper()
	// Recent times: the manager bins a late mock by the test cases it
	// resolved within its stale cutoff.
	base := time.Now()
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	testStart, testEnd := base, ms(5)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetWatermark(neverBehind{})
	out := make(chan *models.Mock, 64)
	mappings := make(chan models.TestMockMapping, 64)
	mgr.SetOutputChannel(out)
	mgr.SetMappingChannel(context.Background(), mappings)
	mgr.SetFirstRequestSignaled()
	sess := &supervisor.Session{Logger: zap.NewNop(), Mgr: mgr, Orphans: &syncMock.Spans{},
		Ctx: syncMock.NewContext(context.Background(), mgr), ClientConnID: "test-client-conn",
		RouteMocksViaSyncMock: true}
	// The test's request opens its window.
	w := mgr.OpenWindow(testStart, nil)
	events := make(chan chunkEvent)
	done := make(chan error, 1)
	go func() { done <- pairChunkEvents(events, sess, zap.NewNop()) }()
	closed := false
	t.Cleanup(func() {
		if !closed {
			close(events)
			<-done
		}
	})
	send := func(evs ...chunkEvent) {
		t.Helper()
		for _, ev := range evs {
			select {
			case events <- ev:
			case <-time.After(10 * time.Second):
				t.Fatal("the parser did not take its next event")
			}
		}
	}
	read := func(c chunkEvent, at time.Time) chunkEvent { c.readAt = at; return c }
	// The connection's preamble, before the test; then the test's call, its
	// answer in two chunks, both inside the test. The empty event last
	// taken says the parser is done with those before it.
	send(read(clientChunk("HELLO", 1, ms(-20)), ms(-20)), read(destChunk("hello", 2, ms(-19)), ms(-19)),
		read(clientChunk("GET big", 3, ms(1)), ms(1)),
		read(destChunk("head", 4, ms(2)), ms(2)), read(destChunk("tail", 5, ms(3)), ms(3)),
		chunkEvent{dir: fakeconn.FromDest})
	// The test is answered and its test case streamed while the parser holds
	// the exchange: with --sync its mapping goes now, without the mock.
	mgr.ResolveKept(w, testStart, testEnd, "test-1", true)
	mgr.FlushOwnedWindows()
	for len(out) > 0 {
		if m := <-out; slices.Equal(payloads(m.Spec.GenericRequests), []string{"GET big"}) {
			t.Fatal("the exchange was handed on before its test case streamed: the answer was not held")
		}
	}
	if byStop {
		stop := ms(1000)
		mgr.BacklogBefore(stop) // the stop begins
	} else {
		send(read(clientChunk("GET small", 6, ms(10)), ms(10)), read(destChunk("+OK", 7, ms(11)), ms(11)),
			chunkEvent{dir: fakeconn.FromDest})
	}
	// The parser hands the exchange on, and the manager's periodic flush
	// takes it from its buffer.
	var held *models.Mock
	deadline := time.Now().Add(10 * time.Second)
	for held == nil {
		select {
		case m := <-out:
			if slices.Equal(payloads(m.Spec.GenericRequests), []string{"GET big"}) {
				held = m
			}
		default:
			if time.Now().After(deadline) {
				t.Fatalf("the exchange held past its test case never reached the recording: %+v", mgr.BacklogBefore(testEnd))
			}
			mgr.FlushOwnedWindows()
			time.Sleep(time.Millisecond)
		}
	}
	assertExchange(t, held, []string{"GET big"}, []string{"head", "tail"})
	if l := held.Spec.Metadata[models.MetadataLifetimeKey]; l != models.MetadataLifetimePerTest {
		t.Fatalf("the exchange is stated %q, want %q", l, models.MetadataLifetimePerTest)
	}
	if !provenswitch.On() {
		if held.TestModeInfo.Lifetime != models.LifetimeSession {
			t.Fatalf("with the switch off the exchange is a %v mock, want a session one, as every Generic exchange", held.TestModeInfo.Lifetime)
		}
		closed = true
		close(events)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return
	}
	// The record-time mapping: the test's, sent as it streamed, and the late
	// one the mock was binned by.
	named := false
	for drained := false; !drained; {
		select {
		case mp := <-mappings:
			if mp.TestName == "test-1" && slices.Contains(mp.MockIDs, held.Name) {
				named = true
			}
		default:
			drained = true
		}
	}
	if !named {
		t.Fatalf("the test's record-time mapping does not name the mock %q held past its test case", held.Name)
	}
	// The mapping keploy mock record derives under the proven switch.
	derived := models.DeriveWindowMappings([]models.TestWindow{{TestCase: "test-1", Start: testStart, End: testEnd}},
		[]models.WindowedMock{models.NewWindowedMock(held)})
	derivedNamed := false
	for _, tc := range derived.Tests {
		for _, e := range tc.Mocks {
			if tc.ID == "test-1" && e.Name == held.Name {
				derivedNamed = true
			}
		}
	}
	if !derivedNamed {
		t.Fatalf("the mapping derived from the recorded times does not give the mock %q to its test: %+v", held.Name, derived.Tests)
	}
	closed = true
	close(events)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
