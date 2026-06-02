package http

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// Canonical test wire bytes. A GET / with Host header and a 5-byte
// "hello" body response with Content-Length. The bytes include a
// response body that covers the content-length branch of readResponseV2.
var (
	canonicalRequest = []byte(
		"GET /hello HTTP/1.1\r\n" +
			"Host: example.com\r\n" +
			"User-Agent: keploy-test/1.0\r\n" +
			"\r\n",
	)
	canonicalResponse = []byte(
		"HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/plain\r\n" +
			"Content-Length: 5\r\n" +
			"\r\n" +
			"hello",
	)
)

// makeStream wires a buffered chunk channel to a FakeConn and returns
// (stream, sendFn, closeFn). The caller pushes Chunks via sendFn; the
// parser under test reads them via stream. closeFn closes the channel
// to signal EOF to the parser.
func makeStream(t *testing.T, dir fakeconn.Direction, buf int) (
	*fakeconn.FakeConn,
	func(b []byte, readAt, writtenAt time.Time),
	func(),
) {
	t.Helper()
	ch := make(chan fakeconn.Chunk, buf)
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
	stream := fakeconn.New(ch, local, remote)
	var seq uint64
	send := func(b []byte, readAt, writtenAt time.Time) {
		seq++
		ch <- fakeconn.Chunk{
			Dir:       dir,
			Bytes:     append([]byte(nil), b...),
			ReadAt:    readAt,
			WrittenAt: writtenAt,
			SeqNo:     seq,
		}
	}
	closed := false
	closer := func() {
		if closed {
			return
		}
		closed = true
		close(ch)
	}
	t.Cleanup(func() {
		closer()
		_ = stream.Close()
	})
	return stream, send, closer
}

// newTestSession constructs a *supervisor.Session wired to fresh
// ClientStream/DestStream and a mocks channel. The session is ready
// to be passed to h.recordV2 directly — we skip the supervisor wrapper
// because the test exercises the parser logic, not the supervisor.
func newTestSession(t *testing.T) (
	sess *supervisor.Session,
	sendReq func([]byte, time.Time, time.Time),
	closeReq func(),
	sendResp func([]byte, time.Time, time.Time),
	closeResp func(),
	mocks chan *models.Mock,
) {
	t.Helper()
	client, sendReq, closeReq := makeStream(t, fakeconn.FromClient, 16)
	dest, sendResp, closeResp := makeStream(t, fakeconn.FromDest, 16)
	mocks = make(chan *models.Mock, 8)
	sess = &supervisor.Session{
		ClientStream: client,
		DestStream:   dest,
		Mocks:        mocks,
		Logger:       zaptest.NewLogger(t),
		ClientConnID: "test-client-1",
		DestConnID:   "test-dest-1",
		Ctx:          context.Background(),
	}
	return sess, sendReq, closeReq, sendResp, closeResp, mocks
}

// TestRecordV2_HappyPath_ChunkTimestampsCarried pushes one request chunk
// and one response chunk, then asserts the emitted mock's timestamps
// equal the chunk ReadAt / WrittenAt exactly (not time.Now()).
func TestRecordV2_HappyPath_ChunkTimestampsCarried(t *testing.T) {
	t.Parallel()
	h := &HTTP{Logger: zaptest.NewLogger(t)}

	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)

	reqReadAt := time.Unix(1_700_000_000, 123_456_000)
	reqWrittenAt := reqReadAt.Add(3 * time.Millisecond)
	respReadAt := reqReadAt.Add(7 * time.Millisecond)
	respWrittenAt := reqReadAt.Add(9 * time.Millisecond)

	sendReq(canonicalRequest, reqReadAt, reqWrittenAt)
	sendResp(canonicalResponse, respReadAt, respWrittenAt)
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.recordV2(ctx, sess) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recordV2 returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recordV2 did not exit within 2s")
	}

	var got *models.Mock
	select {
	case got = <-mocks:
	case <-time.After(time.Second):
		t.Fatal("no mock emitted")
	}

	if got.Kind != models.HTTP {
		t.Errorf("Kind = %v, want %v", got.Kind, models.HTTP)
	}
	if !got.Spec.ReqTimestampMock.Equal(reqReadAt) {
		t.Errorf("ReqTimestampMock = %v, want %v (chunk.ReadAt)",
			got.Spec.ReqTimestampMock, reqReadAt)
	}
	if !got.Spec.ResTimestampMock.Equal(respWrittenAt) {
		t.Errorf("ResTimestampMock = %v, want %v (last chunk.WrittenAt)",
			got.Spec.ResTimestampMock, respWrittenAt)
	}
	if got.Spec.HTTPReq == nil || got.Spec.HTTPReq.URL != "/hello" {
		t.Errorf("HTTPReq.URL = %q, want /hello", got.Spec.HTTPReq.URL)
	}
	if got.Spec.HTTPReq.Method != models.Method("GET") {
		t.Errorf("HTTPReq.Method = %v, want GET", got.Spec.HTTPReq.Method)
	}
	if got.Spec.HTTPResp == nil || got.Spec.HTTPResp.StatusCode != 200 {
		t.Errorf("HTTPResp.StatusCode = %d, want 200", got.Spec.HTTPResp.StatusCode)
	}
	if got.Spec.HTTPResp.Body != "hello" {
		t.Errorf("HTTPResp.Body = %q, want %q", got.Spec.HTTPResp.Body, "hello")
	}
	if got.Spec.Metadata["connID"] != "test-client-1" {
		t.Errorf("metadata connID = %q, want test-client-1", got.Spec.Metadata["connID"])
	}
}

// TestRecordV2_SplitChunks verifies the chunk-by-chunk reader correctly
// reassembles a request/response split across multiple chunks (the
// realistic wire pattern for TLS-record boundaries).
func TestRecordV2_SplitChunks(t *testing.T) {
	t.Parallel()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)

	// First chunk: partial headers only. Second chunk: rest + Content-Length 5 body.
	reqPart1 := []byte("POST /echo HTTP/1.1\r\nHost: ex.com\r\n")
	reqPart2 := []byte("Content-Length: 5\r\n\r\nworld")
	respPart1 := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n")
	respPart2 := []byte("\r\nhello")

	baseTime := time.Unix(1_700_000_100, 0)
	sendReq(reqPart1, baseTime, baseTime)
	sendReq(reqPart2, baseTime.Add(time.Millisecond), baseTime.Add(time.Millisecond))
	sendResp(respPart1, baseTime.Add(2*time.Millisecond), baseTime.Add(2*time.Millisecond))
	sendResp(respPart2, baseTime.Add(3*time.Millisecond), baseTime.Add(3*time.Millisecond))
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.recordV2(ctx, sess) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recordV2 returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recordV2 did not exit within 2s")
	}

	select {
	case m := <-mocks:
		if m.Spec.HTTPReq.Method != models.Method("POST") {
			t.Errorf("method = %v, want POST", m.Spec.HTTPReq.Method)
		}
		if m.Spec.HTTPReq.Body != "world" {
			t.Errorf("request body = %q, want world", m.Spec.HTTPReq.Body)
		}
		if m.Spec.HTTPResp.Body != "hello" {
			t.Errorf("response body = %q, want hello", m.Spec.HTTPResp.Body)
		}
		// The first request chunk ReadAt should be carried.
		if !m.Spec.ReqTimestampMock.Equal(baseTime) {
			t.Errorf("ReqTimestampMock = %v, want %v (first chunk ReadAt)",
				m.Spec.ReqTimestampMock, baseTime)
		}
		// The last response chunk WrittenAt should be carried.
		want := baseTime.Add(3 * time.Millisecond)
		if !m.Spec.ResTimestampMock.Equal(want) {
			t.Errorf("ResTimestampMock = %v, want %v (last chunk WrittenAt)",
				m.Spec.ResTimestampMock, want)
		}
	case <-time.After(time.Second):
		t.Fatal("no mock emitted")
	}
}

// TestRecordV2_ChunkedTransferEncoding exercises the Transfer-Encoding
// chunked branch of readResponseV2.
func TestRecordV2_ChunkedTransferEncoding(t *testing.T) {
	t.Parallel()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)

	req := []byte("GET /stream HTTP/1.1\r\nHost: ex.com\r\n\r\n")
	// A chunked response: two chunks + terminator.
	resp := []byte(
		"HTTP/1.1 200 OK\r\n" +
			"Transfer-Encoding: chunked\r\n" +
			"\r\n" +
			"5\r\nhello\r\n" +
			"6\r\n world\r\n" +
			"0\r\n\r\n",
	)
	base := time.Unix(1_700_001_000, 0)
	sendReq(req, base, base)
	sendResp(resp, base.Add(time.Millisecond), base.Add(time.Millisecond))
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.recordV2(ctx, sess) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recordV2 returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recordV2 did not exit within 2s")
	}

	select {
	case m := <-mocks:
		if m.Spec.HTTPResp.Body != "hello world" {
			t.Errorf("body = %q, want %q", m.Spec.HTTPResp.Body, "hello world")
		}
	case <-time.After(time.Second):
		t.Fatal("no mock emitted")
	}
}

// TestRecordV2_LegacyParity: the mock fields (URL/method/headers/body/
// status) must match the legacy path for identical inputs. Timestamps
// are allowed to differ (legacy uses time.Now(), V2 uses chunk times).
//
// Not t.Parallel because it plumbs the global syncMock manager's
// output channel to intercept the legacy-path mock; parallel runs
// would race for the shared instance.
func TestRecordV2_LegacyParity(t *testing.T) {
	// V2 mock via buildHTTPMock with fixed timestamps.
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	reqTs := time.Unix(1_700_002_000, 0)
	resTs := reqTs.Add(5 * time.Millisecond)
	v2Mock, err := h.buildHTTPMock(&FinalHTTP{
		Req:              canonicalRequest,
		Resp:             canonicalResponse,
		ReqTimestampMock: reqTs,
		ResTimestampMock: resTs,
	}, 80, "test-conn", models.OutgoingOptions{})
	if err != nil {
		t.Fatalf("buildHTTPMock: %v", err)
	}
	if v2Mock == nil {
		t.Fatal("V2 returned nil mock for non-passthrough request")
	}

	// Legacy mock via parseFinalHTTP on the same bytes.
	legacyMock := runLegacyParseFinalHTTP(t, h, canonicalRequest, canonicalResponse, 80, "test-conn")
	if legacyMock == nil {
		t.Fatal("legacy returned nil mock")
	}

	// Compare every field except timestamps and Created (legacy uses
	// time.Now(), V2 can't match exactly).
	if v2Mock.Kind != legacyMock.Kind {
		t.Errorf("Kind mismatch: v2=%v legacy=%v", v2Mock.Kind, legacyMock.Kind)
	}
	if v2Mock.Name != legacyMock.Name {
		t.Errorf("Name mismatch: v2=%q legacy=%q", v2Mock.Name, legacyMock.Name)
	}
	if v2Mock.Version != legacyMock.Version {
		t.Errorf("Version mismatch: v2=%v legacy=%v", v2Mock.Version, legacyMock.Version)
	}

	// HTTPReq field-by-field.
	if !reflect.DeepEqual(v2Mock.Spec.HTTPReq, legacyMock.Spec.HTTPReq) {
		t.Errorf("HTTPReq mismatch:\n  v2=%+v\n  legacy=%+v",
			v2Mock.Spec.HTTPReq, legacyMock.Spec.HTTPReq)
	}
	if !reflect.DeepEqual(v2Mock.Spec.HTTPResp, legacyMock.Spec.HTTPResp) {
		t.Errorf("HTTPResp mismatch:\n  v2=%+v\n  legacy=%+v",
			v2Mock.Spec.HTTPResp, legacyMock.Spec.HTTPResp)
	}
	if !reflect.DeepEqual(v2Mock.Spec.Metadata, legacyMock.Spec.Metadata) {
		t.Errorf("Metadata mismatch:\n  v2=%+v\n  legacy=%+v",
			v2Mock.Spec.Metadata, legacyMock.Spec.Metadata)
	}
}

// runLegacyParseFinalHTTP invokes the legacy parseFinalHTTP on the given
// bytes with a buffered mocks channel and returns the emitted mock.
// The ctx carries a ClientConnectionIDKey so parseFinalHTTP can read
// connID from it (the legacy contract).
//
// parseFinalHTTP routes through the syncMock singleton when one is
// registered; for the test, we plug our mocks channel into the syncMock
// output so the mock flows straight through. We reset the singleton's
// state afterwards so other tests are not affected.
func runLegacyParseFinalHTTP(t *testing.T, h *HTTP, reqBuf, respBuf []byte, destPort uint, connID string) *models.Mock {
	t.Helper()
	mocks := make(chan *models.Mock, 1)

	// Plumb the global syncMock to forward onto our channel. See
	// syncMock.AddMock: when an output channel is bound and
	// firstReqSeen is false, the mock is forwarded synchronously.
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetOutputChannel(mocks)
		t.Cleanup(func() {
			// Unbind by pointing the manager at a throwaway channel.
			// (SetOutputChannel's same-pointer check means we must use
			// a distinct channel to clear its closed-flag state.)
			mgr.SetOutputChannel(make(chan<- *models.Mock, 1))
		})
	}

	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, connID)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	final := &FinalHTTP{
		Req:              reqBuf,
		Resp:             respBuf,
		ReqTimestampMock: time.Now(),
		ResTimestampMock: time.Now().Add(time.Millisecond),
	}
	if err := h.parseFinalHTTP(ctx, final, destPort, mocks, models.OutgoingOptions{}, nil); err != nil {
		t.Fatalf("legacy parseFinalHTTP error: %v", err)
	}
	select {
	case m := <-mocks:
		return m
	case <-time.After(500 * time.Millisecond):
		t.Fatal("legacy parseFinalHTTP did not emit a mock")
	}
	return nil
}

// TestRecordV2_MalformedRequest marks the mock incomplete and returns
// an error rather than emitting a partial mock.
func TestRecordV2_MalformedRequest(t *testing.T) {
	t.Parallel()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, _, closeResp, mocks := newTestSession(t)

	base := time.Unix(1_700_003_000, 0)
	// Bytes that look HTTP-ish but with an invalid Content-Length so
	// buildHTTPMock fails at http.ReadRequest / body parse.
	bogus := []byte("GET / HTTP/1.1\r\nHost: ex\r\nContent-Length: NOTANUMBER\r\n\r\n")
	sendReq(bogus, base, base)
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := h.recordV2(ctx, sess)
	// Expect a decode error surfaced up the stack. Incomplete flag was
	// set; EmitMock drops silently so no mock should land on the chan.
	if err == nil {
		t.Fatal("recordV2 returned nil error for malformed request")
	}
	select {
	case m := <-mocks:
		t.Errorf("unexpected mock emitted: %+v", m)
	default:
	}
}

// TestRecordV2_Keepalive_TwoCycles sends two request/response pairs on
// a single connection and verifies two mocks emerge with correct
// timestamps.
func TestRecordV2_Keepalive_TwoCycles(t *testing.T) {
	t.Parallel()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)

	base := time.Unix(1_700_004_000, 0)

	sendReq(canonicalRequest, base, base)
	sendResp(canonicalResponse, base.Add(time.Millisecond), base.Add(time.Millisecond))

	req2 := []byte("GET /second HTTP/1.1\r\nHost: ex\r\n\r\n")
	resp2 := []byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
	sendReq(req2, base.Add(10*time.Millisecond), base.Add(10*time.Millisecond))
	sendResp(resp2, base.Add(11*time.Millisecond), base.Add(11*time.Millisecond))

	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.recordV2(ctx, sess) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recordV2 error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recordV2 timeout")
	}

	first := <-mocks
	second := <-mocks
	if first.Spec.HTTPReq.URL != "/hello" {
		t.Errorf("first URL = %q, want /hello", first.Spec.HTTPReq.URL)
	}
	if second.Spec.HTTPReq.URL != "/second" {
		t.Errorf("second URL = %q, want /second", second.Spec.HTTPReq.URL)
	}
	if !first.Spec.ReqTimestampMock.Equal(base) {
		t.Errorf("first ReqTs = %v, want %v", first.Spec.ReqTimestampMock, base)
	}
	wantSecond := base.Add(10 * time.Millisecond)
	if !second.Spec.ReqTimestampMock.Equal(wantSecond) {
		t.Errorf("second ReqTs = %v, want %v", second.Spec.ReqTimestampMock, wantSecond)
	}
}

// TestRecordV2_EOFBeforeAnyBytes returns cleanly with no mock when the
// client stream closes immediately (keepalive teardown).
func TestRecordV2_EOFBeforeAnyBytes(t *testing.T) {
	t.Parallel()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, _, closeReq, _, closeResp, mocks := newTestSession(t)
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := h.recordV2(ctx, sess)
	if err != nil {
		t.Errorf("recordV2 error on clean EOF: %v", err)
	}
	select {
	case m := <-mocks:
		t.Errorf("unexpected mock emitted on EOF-before-bytes: %+v", m)
	default:
	}
}

// TestIsV2 guards the capability marker so a refactor cannot silently
// flip this parser back to the legacy path.
func TestIsV2(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	if !h.IsV2() {
		t.Fatal("HTTP parser IsV2 returned false — dispatcher would route to legacy path")
	}
}
