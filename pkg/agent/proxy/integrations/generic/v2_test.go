package generic

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// newV2Session wires a supervisor.Session around two caller-supplied
// chunk channels. The caller pushes Chunks onto clientCh / destCh and
// closes them to drive the parser; the returned session's ClientStream
// and DestStream surface those channels as read-only FakeConns.
func newV2Session(t *testing.T, clientCh, destCh chan fakeconn.Chunk) (*supervisor.Session, chan *models.Mock) {
	t.Helper()
	mocks := make(chan *models.Mock, 16)
	sess := &supervisor.Session{
		ClientStream: fakeconn.New(clientCh, nil, nil),
		DestStream:   fakeconn.New(destCh, nil, nil),
		Mocks:        mocks,
		Logger:       zaptest.NewLogger(t),
		Ctx:          context.Background(),
		ClientConnID: "test-client-conn",
		DestConnID:   "test-dest-conn",
	}
	return sess, mocks
}

// dispatch calls the public RecordOutgoing boundary rather than the
// unexported V2 entry so the test exercises the same dispatch branch
// production traffic hits.
func dispatch(t *testing.T, sess *supervisor.Session) error {
	t.Helper()
	g := New(zaptest.NewLogger(t)).(*Generic)
	return g.RecordOutgoing(context.Background(), &integrations.RecordSession{V2: sess})
}

// TestV2_SingleReqRespPair drives one request chunk and one response
// chunk through and asserts the resulting mock matches the legacy
// shape exactly: GENERIC kind, payloads tagged with the right origin,
// timestamps lifted from the chunks, metadata carries the connID.
func TestV2_SingleReqRespPair(t *testing.T) {
	t.Parallel()

	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 1)

	reqReadAt := time.Unix(1000, 0)
	respReadAt := time.Unix(1001, 0)
	respWrittenAt := time.Unix(1002, 0)

	clientCh <- fakeconn.Chunk{
		Dir:    fakeconn.FromClient,
		Bytes:  []byte("PING\r\n"),
		ReadAt: reqReadAt,
		SeqNo:  1,
	}
	close(clientCh)

	destCh <- fakeconn.Chunk{
		Dir:       fakeconn.FromDest,
		Bytes:     []byte("PONG\r\n"),
		ReadAt:    respReadAt,
		WrittenAt: respWrittenAt,
		SeqNo:     1,
	}
	close(destCh)

	sess, mocks := newV2Session(t, clientCh, destCh)
	if err := dispatch(t, sess); err != nil {
		t.Fatalf("RecordOutgoing returned error: %v", err)
	}

	close(mocks)
	got := drainMocks(mocks)
	if len(got) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(got))
	}
	m := got[0]
	if m.Kind != models.GENERIC {
		t.Errorf("Kind = %v, want %v", m.Kind, models.GENERIC)
	}
	if m.Name != "mocks" {
		t.Errorf("Name = %q, want %q", m.Name, "mocks")
	}
	if n := len(m.Spec.GenericRequests); n != 1 {
		t.Fatalf("GenericRequests len = %d, want 1", n)
	}
	if n := len(m.Spec.GenericResponses); n != 1 {
		t.Fatalf("GenericResponses len = %d, want 1", n)
	}
	if m.Spec.GenericRequests[0].Origin != models.FromClient {
		t.Errorf("request Origin = %q, want %q",
			m.Spec.GenericRequests[0].Origin, models.FromClient)
	}
	if m.Spec.GenericResponses[0].Origin != models.FromServer {
		t.Errorf("response Origin = %q, want %q",
			m.Spec.GenericResponses[0].Origin, models.FromServer)
	}
	if got := m.Spec.GenericRequests[0].Message[0].Data; got != "PING\r\n" {
		t.Errorf("request Data = %q, want %q", got, "PING\r\n")
	}
	if got := m.Spec.GenericResponses[0].Message[0].Data; got != "PONG\r\n" {
		t.Errorf("response Data = %q, want %q", got, "PONG\r\n")
	}
	if !m.Spec.ReqTimestampMock.Equal(reqReadAt) {
		t.Errorf("ReqTimestampMock = %v, want %v (chunk ReadAt)",
			m.Spec.ReqTimestampMock, reqReadAt)
	}
	if !m.Spec.ResTimestampMock.Equal(respWrittenAt) {
		t.Errorf("ResTimestampMock = %v, want %v (chunk WrittenAt)",
			m.Spec.ResTimestampMock, respWrittenAt)
	}
	if m.Spec.Metadata["type"] != "config" {
		t.Errorf("metadata[type] = %q, want config", m.Spec.Metadata["type"])
	}
	if m.Spec.Metadata["connID"] != "test-client-conn" {
		t.Errorf("metadata[connID] = %q, want test-client-conn",
			m.Spec.Metadata["connID"])
	}
}

// TestV2_TimestampsComeFromChunks uses distinctive chunk timestamps
// (well in the past, with microsecond offsets) and asserts the emitted
// mock's timestamps equal the exact chunk values rather than anything
// derived from time.Now().
func TestV2_TimestampsComeFromChunks(t *testing.T) {
	t.Parallel()

	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 1)

	// Year 2001 to be unmistakable vs. wall-clock now.
	reqAt := time.Date(2001, 1, 2, 3, 4, 5, 6789, time.UTC)
	respWrittenAt := time.Date(2001, 1, 2, 3, 4, 5, 9876, time.UTC)

	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("q"), ReadAt: reqAt}
	close(clientCh)
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("r"), WrittenAt: respWrittenAt}
	close(destCh)

	sess, mocks := newV2Session(t, clientCh, destCh)
	if err := dispatch(t, sess); err != nil {
		t.Fatalf("RecordOutgoing: %v", err)
	}
	close(mocks)
	got := drainMocks(mocks)
	if len(got) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(got))
	}
	if !got[0].Spec.ReqTimestampMock.Equal(reqAt) {
		t.Errorf("ReqTimestampMock = %v, want %v", got[0].Spec.ReqTimestampMock, reqAt)
	}
	if !got[0].Spec.ResTimestampMock.Equal(respWrittenAt) {
		t.Errorf("ResTimestampMock = %v, want %v",
			got[0].Spec.ResTimestampMock, respWrittenAt)
	}
	// Sanity: these must not be anywhere near "now".
	if time.Since(got[0].Spec.ReqTimestampMock) < 365*24*time.Hour {
		t.Errorf("ReqTimestampMock looks like time.Now(), not chunk-derived: %v",
			got[0].Spec.ReqTimestampMock)
	}
}

// TestV2_MultipleExchanges feeds two full req/resp pairs and expects
// two mocks, each anchored to the timestamps of its own exchange.
func TestV2_MultipleExchanges(t *testing.T) {
	t.Parallel()

	clientCh := make(chan fakeconn.Chunk, 4)
	destCh := make(chan fakeconn.Chunk, 4)

	req1At := time.Unix(2000, 0)
	resp1WrittenAt := time.Unix(2001, 0)
	req2At := time.Unix(3000, 0)
	resp2WrittenAt := time.Unix(3001, 0)

	// Push first exchange, then second. Closing the dest channel after
	// the second response signals EOF so the loop drains and exits.
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("req1"), ReadAt: req1At}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("resp1"), WrittenAt: resp1WrittenAt}

	// Small pause to let the parser observe the first response before
	// the second request arrives. Using a tiny sleep here is OK because
	// we're driving synthetic chunks, not measuring real time.
	go func() {
		time.Sleep(10 * time.Millisecond)
		clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("req2"), ReadAt: req2At}
		time.Sleep(10 * time.Millisecond)
		destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("resp2"), WrittenAt: resp2WrittenAt}
		close(clientCh)
		close(destCh)
	}()

	sess, mocks := newV2Session(t, clientCh, destCh)
	if err := dispatch(t, sess); err != nil {
		t.Fatalf("RecordOutgoing: %v", err)
	}
	close(mocks)
	got := drainMocks(mocks)
	if len(got) != 2 {
		t.Fatalf("expected 2 mocks, got %d", len(got))
	}
	if got[0].Spec.GenericRequests[0].Message[0].Data != "req1" {
		t.Errorf("mock[0] req = %q, want req1",
			got[0].Spec.GenericRequests[0].Message[0].Data)
	}
	if got[1].Spec.GenericRequests[0].Message[0].Data != "req2" {
		t.Errorf("mock[1] req = %q, want req2",
			got[1].Spec.GenericRequests[0].Message[0].Data)
	}
	if !got[0].Spec.ReqTimestampMock.Equal(req1At) {
		t.Errorf("mock[0] ReqTimestampMock = %v, want %v",
			got[0].Spec.ReqTimestampMock, req1At)
	}
	if !got[1].Spec.ReqTimestampMock.Equal(req2At) {
		t.Errorf("mock[1] ReqTimestampMock = %v, want %v",
			got[1].Spec.ReqTimestampMock, req2At)
	}
	if !got[0].Spec.ResTimestampMock.Equal(resp1WrittenAt) {
		t.Errorf("mock[0] ResTimestampMock = %v, want %v",
			got[0].Spec.ResTimestampMock, resp1WrittenAt)
	}
	if !got[1].Spec.ResTimestampMock.Equal(resp2WrittenAt) {
		t.Errorf("mock[1] ResTimestampMock = %v, want %v",
			got[1].Spec.ResTimestampMock, resp2WrittenAt)
	}
}

// TestV2_PartialPairNotEmitted: a request with no response arriving
// before EOF must produce zero mocks — the legacy encoder also drops
// such partial exchanges.
func TestV2_PartialPairNotEmitted(t *testing.T) {
	t.Parallel()

	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 0)

	clientCh <- fakeconn.Chunk{
		Dir:    fakeconn.FromClient,
		Bytes:  []byte("lonely-request"),
		ReadAt: time.Unix(4000, 0),
	}
	close(clientCh)
	close(destCh)

	sess, mocks := newV2Session(t, clientCh, destCh)
	if err := dispatch(t, sess); err != nil {
		t.Fatalf("RecordOutgoing: %v", err)
	}
	close(mocks)
	got := drainMocks(mocks)
	if len(got) != 0 {
		t.Errorf("expected 0 mocks for partial exchange, got %d", len(got))
	}
}

// TestV2_IncompleteFlagDropsMock: when the session's incomplete flag
// is set (as the relay would on a dropped chunk), the pending mock
// must be dropped without emit. EmitMock itself also honours the
// flag — we assert the end-to-end behaviour: no mock reaches the
// channel even though the req/resp were both seen.
func TestV2_IncompleteFlagDropsMock(t *testing.T) {
	t.Parallel()

	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 1)

	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("q"), ReadAt: time.Unix(5000, 0)}
	// Trip the incomplete flag BEFORE the response arrives so the
	// flush path sees it set.
	go func() {
		// Small sleep so the client chunk is definitely consumed first.
		time.Sleep(10 * time.Millisecond)
		destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("r"), WrittenAt: time.Unix(5001, 0)}
		close(clientCh)
		close(destCh)
	}()

	sess, mocks := newV2Session(t, clientCh, destCh)
	sess.MarkMockIncomplete("test-synthetic-drop")

	if err := dispatch(t, sess); err != nil {
		t.Fatalf("RecordOutgoing: %v", err)
	}
	close(mocks)
	got := drainMocks(mocks)
	if len(got) != 0 {
		t.Errorf("expected 0 mocks when incomplete flag set, got %d", len(got))
	}
}

// TestV2_OnMockRecordedHookFires verifies the post-record hook chain
// runs via Session.EmitMock. This is the contract the migration spec
// requires — wrapper parsers install hooks via AddPostRecordHook and
// expect them to fire exactly once per emitted mock.
func TestV2_OnMockRecordedHookFires(t *testing.T) {
	t.Parallel()

	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 1)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("q"), ReadAt: time.Unix(6000, 0)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("r"), WrittenAt: time.Unix(6001, 0)}
	close(clientCh)
	close(destCh)

	sess, mocks := newV2Session(t, clientCh, destCh)

	var hookCalls int
	sess.AddPostRecordHook(func(m *models.Mock) {
		if m == nil {
			t.Error("hook received nil mock")
			return
		}
		if m.Spec.Metadata == nil {
			m.Spec.Metadata = map[string]string{}
		}
		m.Spec.Metadata["hook"] = "ran"
		hookCalls++
	})

	if err := dispatch(t, sess); err != nil {
		t.Fatalf("RecordOutgoing: %v", err)
	}
	close(mocks)
	got := drainMocks(mocks)
	if len(got) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(got))
	}
	if hookCalls != 1 {
		t.Errorf("post-record hook calls = %d, want 1", hookCalls)
	}
	if got[0].Spec.Metadata["hook"] != "ran" {
		t.Errorf("hook annotation missing; metadata = %+v", got[0].Spec.Metadata)
	}
}

// TestV2_NilSessionSafe: recordV2 must accept a nil supervisor.Session
// without panicking. The dispatcher guards against this, but defensive
// behaviour on the parser side keeps bug-hunting cheap.
func TestV2_NilSessionSafe(t *testing.T) {
	t.Parallel()
	g := &Generic{logger: zaptest.NewLogger(t)}
	if err := g.recordV2(context.Background(), nil); err != nil {
		t.Errorf("recordV2(nil) = %v, want nil", err)
	}
}

// TestV2_BinaryPayloadEncoding: a chunk that's not ASCII must be
// base64-encoded in the payload with Type=binary, matching the legacy
// encodePayload behaviour.
func TestV2_BinaryPayloadEncoding(t *testing.T) {
	t.Parallel()

	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 1)

	bin := []byte{0x00, 0x01, 0xFE, 0xFF}
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: bin, ReadAt: time.Unix(7000, 0)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok"), WrittenAt: time.Unix(7001, 0)}
	close(clientCh)
	close(destCh)

	sess, mocks := newV2Session(t, clientCh, destCh)
	if err := dispatch(t, sess); err != nil {
		t.Fatalf("RecordOutgoing: %v", err)
	}
	close(mocks)
	got := drainMocks(mocks)
	if len(got) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(got))
	}
	p := got[0].Spec.GenericRequests[0]
	if p.Message[0].Type == models.String {
		t.Errorf("expected binary encoding for non-ASCII bytes, got %q", p.Message[0].Type)
	}
}

// TestV2_IsV2True sanity-checks the capability signal used by the
// dispatcher to route this parser through the supervisor path.
func TestV2_IsV2True(t *testing.T) {
	t.Parallel()
	g := &Generic{logger: zaptest.NewLogger(t)}
	if !g.IsV2() {
		t.Errorf("IsV2() = false, want true")
	}
}

// drainMocks reads all mocks off ch (which must have been closed) and
// returns them as a slice. Keeps test bodies focused on assertions.
func drainMocks(ch <-chan *models.Mock) []*models.Mock {
	var out []*models.Mock
	for m := range ch {
		out = append(out, m)
	}
	return out
}

// The readers read ahead of the loop (ReadAhead), and the loop marks each
// chunk processed once it is done with it: what a reader took that the loop
// never got to stays unprocessed, so it is counted when the parser stops,
// and the stream's LastReadTime is where the loop got to.
func TestV2_TheLoopMarksProcessedWhatItIsDoneWith(t *testing.T) {
	t.Parallel()

	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 2)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("PING\r\n"), ReadAt: time.Unix(1000, 0)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("PONG\r\n"), ReadAt: time.Unix(1001, 0), WrittenAt: time.Unix(1002, 0)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("never queued"), ReadAt: time.Unix(1003, 0)}
	sess, _ := newV2Session(t, clientCh, destCh)

	// The readers: each takes its chunks ahead of the loop; the second dest
	// chunk is still in its reader's hand when the parser stops.
	events := make(chan chunkEvent, 2)
	for _, fc := range []*fakeconn.FakeConn{sess.ClientStream, sess.DestStream} {
		c, err := fc.ReadAhead()
		if err != nil {
			t.Fatalf("ReadAhead: %v", err)
		}
		events <- chunkEvent{dir: c.Dir, bytes: c.Bytes, readAt: c.ReadAt, writtenAt: c.WrittenAt}
	}
	if _, err := sess.DestStream.ReadAhead(); err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	close(events)

	if err := pairChunkEvents(events, sess, sess.Logger); err != nil {
		t.Fatalf("pairChunkEvents: %v", err)
	}
	if c, p := sess.ClientStream.Consumed(), sess.ClientStream.Processed(); c != 6 || p != 6 {
		t.Errorf("client Consumed, Processed = %d, %d, want 6, 6: the loop processed its one chunk", c, p)
	}
	if c, p := sess.DestStream.Consumed(), sess.DestStream.Processed(); c != 18 || p != 6 {
		t.Errorf("dest Consumed, Processed = %d, %d, want 18, 6: the loop processed the answer, not the chunk its reader held", c, p)
	}
	if got, want := sess.DestStream.LastReadTime(), time.Unix(1001, 0); !got.Equal(want) {
		t.Errorf("dest LastReadTime = %v, want %v, the last chunk the loop processed", got, want)
	}
	if got, want := sess.DestStream.LastWrittenTime(), time.Unix(1002, 0); !got.Equal(want) {
		t.Errorf("dest LastWrittenTime = %v, want %v, the last chunk the loop processed", got, want)
	}
}

// The readers, and the first read, take chunks ahead of the loop: a chunk one
// of them has handed on is taken (Consumed) but not processed until the loop
// is done with it.
func TestV2_TheReadersReadAhead(t *testing.T) {
	t.Parallel()

	ch := make(chan fakeconn.Chunk, 3)
	ch <- fakeconn.Chunk{Bytes: []byte("first")}
	ch <- fakeconn.Chunk{Bytes: []byte("one")}
	ch <- fakeconn.Chunk{Bytes: []byte("two")}
	close(ch)
	fc := fakeconn.New(ch, nil, nil)

	if c, err := readFirst(fc); err != nil || string(c.Bytes) != "first" {
		t.Fatalf("readFirst = %q, %v", c.Bytes, err)
	}
	if c, p := fc.Consumed(), fc.Processed(); c != 5 || p != 0 {
		t.Fatalf("after the first read, Consumed, Processed = %d, %d, want 5, 0", c, p)
	}

	out := make(chan chunkEvent, 1)
	done := make(chan struct{}, 1)
	go readStream(fc, fakeconn.FromClient, out, make(chan struct{}), done)
	if ev := <-out; string(ev.bytes) != "one" {
		t.Fatalf("readStream handed on %q, want %q", ev.bytes, "one")
	}
	if p := fc.Processed(); p != 0 {
		t.Fatalf("Processed = %d once the reader handed a chunk on, want 0: it read ahead", p)
	}
	for ev := range out {
		if ev.err != nil {
			break
		}
	}
	<-done
	if c, p := fc.Consumed(), fc.Processed(); c != 11 || p != 0 {
		t.Fatalf("after the reader ended, Consumed, Processed = %d, %d, want 11, 0", c, p)
	}
}

// errEmitFailed is what a test's emit fails with to stop the loop mid-chunk.
var errEmitFailed = errors.New("the emit failed")

// v2Feed hands the loop each next chunk of a stream as its reader would: read
// ahead, then queued for the loop.
func v2Feed(t *testing.T, events chan<- chunkEvent, fc *fakeconn.FakeConn) {
	t.Helper()
	c, err := fc.ReadAhead()
	if err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	events <- chunkEvent{dir: c.Dir, bytes: c.Bytes, connSeq: c.ConnSeq, readAt: c.ReadAt, writtenAt: c.WrittenAt}
}

// A chunk in the loop's hand when it fails is not processed: the answer that
// completes the first exchange is in its hand while that exchange is emitted,
// and the emit fails.
func TestV2_AChunkInTheLoopsHandWhenItFailsIsNotProcessed(t *testing.T) {
	t.Setenv(provenswitch.Env, "0") // emitted at once, not held
	t0 := time.Unix(5000, 0)
	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 1)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("REQ1"), ReadAt: t0.Add(time.Second)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("RES1"), ReadAt: t0.Add(2 * time.Second), WrittenAt: t0.Add(2 * time.Second)}
	sess, _ := newV2Session(t, clientCh, destCh)
	emitted := 0
	sess.AddPostRecordHook(func(*models.Mock) {
		emitted++
		panic(errEmitFailed)
	})
	events := make(chan chunkEvent, 2)
	v2Feed(t, events, sess.ClientStream)
	v2Feed(t, events, sess.DestStream)
	close(events)

	failed := func() (r any) {
		defer func() { r = recover() }()
		_ = pairChunkEvents(events, sess, sess.Logger)
		return nil
	}()
	if failed != errEmitFailed || emitted != 1 {
		t.Fatalf("the loop ended with %v after %d emits, want the failed emit of the first exchange", failed, emitted)
	}
	if c, p := sess.ClientStream.Consumed(), sess.ClientStream.Processed(); c != 4 || p != 4 {
		t.Errorf("client Consumed, Processed = %d, %d, want 4, 4: the loop was done with the request", c, p)
	}
	if c, p := sess.DestStream.Consumed(), sess.DestStream.Processed(); c != 4 || p != 0 {
		t.Errorf("dest Consumed, Processed = %d, %d, want 4, 0: the answer was in the loop's hand", c, p)
	}
}

// The loop marks each chunk processed once: a turn that takes no chunk (the
// hold's hand-on as the recording's stop begins) marks nothing, so what a
// reader read ahead is not counted processed in its place.
func TestV2_ATurnThatTakesNoChunkMarksNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetWatermark(neverBehind{})
	_ = mgr.OpenWindow(t0, nil) // open: the first exchange may be its test's, so it is held
	clientCh := make(chan fakeconn.Chunk, 3)
	destCh := make(chan fakeconn.Chunk, 1)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("REQ1"), ReadAt: t0.Add(time.Second)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("RES1"), ReadAt: t0.Add(2 * time.Second), WrittenAt: t0.Add(2 * time.Second)}
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("REQ2"), ReadAt: t0.Add(3 * time.Second)}
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("read ahead"), ReadAt: t0.Add(4 * time.Second)}
	sess, _ := newV2Session(t, clientCh, destCh)
	sess.Ctx = syncMock.NewContext(context.Background(), mgr)
	mocks := make(chan *models.Mock) // unbuffered: the test is told of each emit
	sess.Mocks = mocks
	events := make(chan chunkEvent) // unbuffered: the loop has each chunk as the feed returns
	done := make(chan error, 1)
	go func() { done <- pairChunkEvents(events, sess, sess.Logger) }()

	v2Feed(t, events, sess.ClientStream)
	v2Feed(t, events, sess.DestStream) // completes the first exchange, which is held
	v2Feed(t, events, sess.ClientStream)
	if _, err := sess.ClientStream.ReadAhead(); err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	mgr.BacklogBefore(t0.Add(10 * time.Second)) // the stop begins: the hold is handed on
	<-mocks
	close(events)
	var err error
	for waiting := true; waiting; {
		select {
		case <-mocks: // what the loop emits as it ends
		case err = <-done:
			waiting = false
		}
	}
	if err != nil {
		t.Fatalf("pairChunkEvents: %v", err)
	}
	if c, p := sess.ClientStream.Consumed(), sess.ClientStream.Processed(); c != 18 || p != 8 {
		t.Errorf("client Consumed, Processed = %d, %d, want 18, 8: the loop processed two requests, not the chunk read ahead", c, p)
	}
	if c, p := sess.DestStream.Consumed(), sess.DestStream.Processed(); c != 4 || p != 4 {
		t.Errorf("dest Consumed, Processed = %d, %d, want 4, 4", c, p)
	}
}
