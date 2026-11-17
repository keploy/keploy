package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	yamlLib "gopkg.in/yaml.v3"
)

const chunkedCall = "GET /suite HTTP/1.1\r\nHost: example.com\r\nAccept-Encoding: gzip\r\n\r\n"

// chunkedAnswers are answers an upstream sent chunked: whole in one chunk,
// in several, and gzip-encoded.
func chunkedAnswers(t *testing.T) map[string]string {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"suite":"made"}`))
	_ = zw.Close()
	return map[string]string{
		"one chunk": "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nDate: Mon, 16 Nov 2026 07:40:50 GMT\r\nTransfer-Encoding: chunked\r\n\r\n" +
			"10\r\n{\"suite\":\"made\"}\r\n0\r\n\r\n",
		"several chunks": "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" +
			"9\r\n{\"suite\":\r\n7\r\n\"made\"}\r\n0\r\n\r\n",
		"no body": "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
		"gzip": "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" +
			strconv.FormatInt(int64(gz.Len()), 16) + "\r\n" + gz.String() + "\r\n0\r\n\r\n",
	}
}

// clientView is what the application's net/http client reads of answer to call:
// its header names, its length (-1: none) and framing, and its decoded body.
type clientView struct {
	names    []string
	length   int64
	encoding []string
	body     string
}

func clientViewOf(t *testing.T, r *bufio.Reader, call string) clientView {
	t.Helper()
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(call)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		t.Fatalf("the answer does not parse: %v", err)
	}
	var body io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatalf("the gzip body does not read: %v", err)
		}
		body = zr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("the body does not read: %v", err)
	}
	var names []string
	for k := range resp.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	return clientView{names: names, length: resp.ContentLength, encoding: resp.TransferEncoding, body: string(data)}
}

// throughDisk is m written as mocks.yaml writes it and read back.
func throughDisk(t *testing.T, m *models.Mock) *models.Mock {
	t.Helper()
	doc, err := mockdb.EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	onDisk, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var readBack yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(onDisk, &readBack); err != nil {
		t.Fatal(err)
	}
	mocks, err := mockdb.DecodeMocks([]*yaml.NetworkTrafficDoc{&readBack}, zap.NewNop())
	if err != nil || len(mocks) != 1 {
		t.Fatalf("DecodeMocks = (%d mocks, %v)\n%s", len(mocks), err, onDisk)
	}
	return mocks[0]
}

// recordedThroughDisk is answer to call recorded by both record paths, written
// as mocks.yaml writes it and read back.
func recordedThroughDisk(t *testing.T, call, answer string) map[string]*models.Mock {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	at := time.Now()
	v2, err := h.buildHTTPMock(context.Background(), &FinalHTTP{Req: []byte(call), Resp: []byte(answer), ReqTimestampMock: at, ResTimestampMock: at,
		RespWrites: []respWrite{{end: len(answer), at: at}}}, 80, "test-conn", models.OutgoingOptions{})
	if err != nil || v2 == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", v2, err)
	}
	legacy := runLegacyParseFinalHTTP(t, h, []byte(call), []byte(answer), 80, "test-conn")
	out := map[string]*models.Mock{}
	for path, m := range map[string]*models.Mock{"V2 record": v2, "legacy record": legacy} {
		out[path] = throughDisk(t, m)
	}
	return out
}

// servedTo is what the application reads when call is served from m through
// the replay's serve path (decodeHTTP). Nothing may follow the answer: a byte
// after it would be read as the start of the next call's answer.
func servedTo(t *testing.T, m *models.Mock, call string) clientView {
	t.Helper()
	return servedBy(t, &HTTP{Logger: zap.NewNop()}, m, call, nil, models.OutgoingOptions{})
}

// servedBy is servedTo through h, for a call to dst under opts.
func servedBy(t *testing.T, h *HTTP, m *models.Mock, call string, dst *models.ConditionalDstCfg, opts models.OutgoingOptions) clientView {
	t.Helper()
	app, proxy := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		returned <- h.decodeHTTP(ctx, []byte(call), proxy, dst,
			&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, opts)
	}()
	t.Cleanup(func() { cancel(); _ = proxy.Close(); <-returned })
	// The bound only turns a hang into a failure.
	_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(app)
	got := clientViewOf(t, r, call)
	if rest := readFor(app, quiet); r.Buffered() > 0 || rest != "" {
		t.Errorf("%d bytes followed the answer: %q", r.Buffered()+len(rest), rest)
	}
	return got
}

// An answer its upstream sent chunked reaches the application's client with
// no Content-Length: net/http reads none, and keeps Transfer-Encoding out of
// the header. Recorded by either record path, written to disk and read back,
// and served to an HTTP/1.1 call, it reaches the application chunked again,
// with the header it had on record. Served with a Content-Length, the
// application got one header more than on record: an application that keeps
// the headers its calls got (a stored suite step's response) wrote other
// data than its recording, and that write was refused.
func TestAChunkedAnswerIsServedChunked(t *testing.T) {
	for name, answer := range chunkedAnswers(t) {
		t.Run(name, func(t *testing.T) {
			onRecord := clientViewOf(t, bufio.NewReader(strings.NewReader(answer)), chunkedCall)
			if onRecord.length != -1 || !reflect.DeepEqual(onRecord.encoding, []string{"chunked"}) {
				t.Fatalf("the upstream's answer reads as length %d, framing %q: not a chunked answer", onRecord.length, onRecord.encoding)
			}
			for path, m := range recordedThroughDisk(t, chunkedCall, answer) {
				if got := servedTo(t, m, chunkedCall); !reflect.DeepEqual(got, onRecord) {
					t.Errorf("%s: the application read\n%+v\nserved, and\n%+v\non record", path, got, onRecord)
				}
			}
		})
	}
}

// An HTTP/1.0 call cannot take a chunked answer (RFC 9112 7.1): served from
// a chunked recording it gets the body with its length, as before.
func TestAChunkedAnswerIsServedWithItsLengthToAnHTTP10Call(t *testing.T) {
	const call = "GET /suite HTTP/1.0\r\nHost: example.com\r\nAccept-Encoding: gzip\r\n\r\n"
	for name, answer := range chunkedAnswers(t) {
		t.Run(name, func(t *testing.T) {
			onRecord := clientViewOf(t, bufio.NewReader(strings.NewReader(answer)), chunkedCall)
			for path, m := range recordedThroughDisk(t, chunkedCall, answer) {
				got := servedTo(t, m, call)
				if got.length < 0 || got.encoding != nil || got.body != onRecord.body {
					t.Errorf("%s: an HTTP/1.0 call read length %d, framing %q, body %q; want the body %q with its length", path, got.length, got.encoding, got.body, onRecord.body)
				}
			}
		})
	}
}

// An answer sent with its length is served with its length, and so is a
// recording that does not say how its answer was framed (one made before
// keploy kept the framing): its answer is served as before.
func TestAnAnswerNotRecordedChunkedIsServedWithItsLength(t *testing.T) {
	const answer = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 16\r\n\r\n{\"suite\":\"made\"}"
	onRecord := clientViewOf(t, bufio.NewReader(strings.NewReader(answer)), chunkedCall)
	for path, m := range recordedThroughDisk(t, chunkedCall, answer) {
		if got := servedTo(t, m, chunkedCall); !reflect.DeepEqual(got, onRecord) {
			t.Errorf("%s: the application read\n%+v\nserved, and\n%+v\non record", path, got, onRecord)
		}
	}
	for path, m := range recordedThroughDisk(t, chunkedCall, chunkedAnswers(t)["one chunk"]) {
		older := m.DeepCopy()
		older.Spec.HTTPResp.Chunked = false
		older = throughDisk(t, older)
		got := servedTo(t, older, chunkedCall)
		if got.length != 16 || got.encoding != nil {
			t.Errorf("%s: a recording that does not say its answer was chunked was served with length %d, framing %q; want its length, as before", path, got.length, got.encoding)
		}
	}
}

// A chunked answer is HTTP/1.1's framing: a client reads an older status
// line's Transfer-Encoding as none, and its body to the connection's close.
// So a recording whose call was HTTP/1.0, answered chunked, goes chunked to
// an HTTP/1.1 call with an HTTP/1.1 status line.
func TestAChunkedAnswerToAnHTTP10RecordingGoesOutAsHTTP11(t *testing.T) {
	const recordedCall = "GET /suite HTTP/1.0\r\nHost: example.com\r\nAccept-Encoding: gzip\r\n\r\n"
	answer := chunkedAnswers(t)["one chunk"]
	onRecord := clientViewOf(t, bufio.NewReader(strings.NewReader(answer)), chunkedCall)
	for path, m := range recordedThroughDisk(t, recordedCall, answer) {
		if m.Spec.HTTPReq.ProtoMinor != 0 || !m.Spec.HTTPResp.Chunked {
			t.Fatalf("%s: recorded as HTTP/1.%d, chunked %v; want an HTTP/1.0 call answered chunked", path, m.Spec.HTTPReq.ProtoMinor, m.Spec.HTTPResp.Chunked)
		}
		if got := servedTo(t, m, chunkedCall); !reflect.DeepEqual(got, onRecord) {
			t.Errorf("%s: the application read\n%+v\nserved, and\n%+v\non record", path, got, onRecord)
		}
	}
}

// An answer with no body by rule keeps its head as recorded, chunked or not:
// a HEAD's answer framed as a body would leave the body's end on the
// connection, read as the next call's answer.
func TestAChunkedAnswerWithNoBodyByRuleKeepsItsHead(t *testing.T) {
	const call = "HEAD /suite HTTP/1.1\r\nHost: example.com\r\n\r\n"
	const answer = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"
	for path, m := range recordedThroughDisk(t, call, answer) {
		if !m.Spec.HTTPResp.Chunked {
			t.Fatalf("%s: a HEAD answered chunked was not recorded chunked", path)
		}
		if got := servedTo(t, m, call); got.length != -1 || got.encoding != nil || got.body != "" {
			t.Errorf("%s: the HEAD's answer read as length %d, framing %q, body %q; want its head as recorded", path, got.length, got.encoding, got.body)
		}
	}
}

// A recording whose header also names its Transfer-Encoding (one written by
// hand) goes out with one Transfer-Encoding line: net/http refuses an answer
// with two.
func TestAChunkedAnswerGoesOutWithOneTransferEncoding(t *testing.T) {
	answer := chunkedAnswers(t)["one chunk"]
	onRecord := clientViewOf(t, bufio.NewReader(strings.NewReader(answer)), chunkedCall)
	for path, m := range recordedThroughDisk(t, chunkedCall, answer) {
		m.Spec.HTTPResp.Header["Transfer-Encoding"] = "chunked"
		if got := servedTo(t, m, chunkedCall); !reflect.DeepEqual(got, onRecord) {
			t.Errorf("%s: the application read\n%+v\nserved, and\n%+v\non record", path, got, onRecord)
		}
	}
}

// A chunked answer goes out with no Content-Length line, though its recording
// keeps one: a sender must not send both (RFC 9112 6.1). net/http drops the
// length when it reads both, so the rows that read the answer as a Go client
// does cannot see it, but a stricter client may refuse such an answer.
func TestAChunkedAnswerGoesOutWithNoContentLength(t *testing.T) {
	for name, answer := range chunkedAnswers(t) {
		for path, m := range recordedThroughDisk(t, chunkedCall, answer) {
			if _, kept := m.Spec.HTTPResp.Header["Content-Length"]; !kept {
				t.Fatalf("%s, %s: the recording keeps no Content-Length; the row would read nothing", name, path)
			}
			out, err := (&HTTP{Logger: zap.NewNop()}).buildMockResponseBytes(m, true)
			if err != nil {
				t.Fatal(err)
			}
			head, _, found := strings.Cut(string(out), "\r\n\r\n")
			if !found {
				t.Fatalf("%s, %s: the answer has no end of head: %q", name, path, out)
			}
			var lengths, framings []string
			for _, line := range strings.Split(head, "\r\n")[1:] {
				key, value, _ := strings.Cut(line, ":")
				switch {
				case strings.EqualFold(key, "Content-Length"):
					lengths = append(lengths, line)
				case strings.EqualFold(key, "Transfer-Encoding"):
					framings = append(framings, strings.TrimSpace(value))
				}
			}
			if len(lengths) != 0 || !reflect.DeepEqual(framings, []string{"chunked"}) {
				t.Errorf("%s, %s: the chunked answer went out with length lines %q and framings %q; want no length and one chunked", name, path, lengths, framings)
			}
		}
	}
}

// A held answer that went out in one write goes out whole at its place, as
// before, and chunked to an HTTP/1.1 call when its upstream sent it so; an
// HTTP/1.0 call takes it with its length.
func TestHeldAnswer_AChunkedAnswerGoesOutChunkedAtItsPlace(t *testing.T) {
	registerStandIn(t)
	answer := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n10\r\n{\"state\":\"done\"}\r\n0\r\n\r\n"
	for _, tc := range []struct {
		call    string
		chunked bool
	}{
		{releaseCall, true},
		{"GET /job HTTP/1.0\r\nHost: api.example\r\n\r\n", false},
	} {
		db := newPlacedDb(heldMock(t, releaseAt(5), answer, releaseAt(45)))
		app, _ := serveHeldCall(t, db, nil, tc.call)
		db.waitHeld(t)
		if got := readFor(app, quiet); got != "" {
			t.Fatalf("%q: written while test-1 runs: %q", tc.call, got)
		}
		db.reach(models.RunOpen(2))
		_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
		got := clientViewOf(t, bufio.NewReader(app), tc.call)
		if (got.length == -1) != tc.chunked || (got.encoding != nil) != tc.chunked || got.body != `{"state":"done"}` {
			t.Errorf("%q: read length %d, framing %q, body %q; want chunked %v", tc.call, got.length, got.encoding, got.body, tc.chunked)
		}
	}
}

// An async lane's recorded delivery and a pass-through egress rule's
// recorded representative go chunked to an HTTP/1.1 call too, and with their
// length to an HTTP/1.0 call.
func TestAChunkedAnswerIsServedChunkedOnEveryServePath(t *testing.T) {
	const call10 = "GET /suite HTTP/1.0\r\nHost: example.com\r\nAccept-Encoding: gzip\r\n\r\n"
	answer := chunkedAnswers(t)["one chunk"]
	onRecord := clientViewOf(t, bufio.NewReader(strings.NewReader(answer)), chunkedCall)
	recorded := recordedThroughDisk(t, chunkedCall, answer)["V2 record"]
	paths := map[string]func(t *testing.T, call string) clientView{
		"an async lane's delivery": func(t *testing.T, call string) clientView {
			m := recorded.DeepCopy()
			m.Spec.Async = &models.AsyncMeta{Lane: "suite", Seq: 1}
			h := &HTTP{Logger: zap.NewNop()}
			engine := async.NewEngine(zap.NewNop(),
				[]models.AsyncLane{{Name: "suite", Type: "http", Match: map[string]string{"pathRegex": "^/suite$"}}},
				map[string]async.AsyncParser{"http": h})
			engine.Load([]*models.Mock{m})
			h.SetAsyncEngine(engine)
			return servedBy(t, h, m, call, nil, models.OutgoingOptions{})
		},
		"a pass-through rule's representative": func(t *testing.T, call string) clientView {
			t.Setenv(provenswitch.Env, "1") // the switch reads the user's rules
			m := recorded.DeepCopy()
			m.Spec.Metadata["type"] = "config"
			m.TestModeInfo.Lifetime = models.LifetimeSession
			return servedBy(t, &HTTP{Logger: zap.NewNop()}, m, call, &models.ConditionalDstCfg{Addr: "127.0.0.1:80", Port: 80},
				models.OutgoingOptions{PassThroughHosts: []models.PassThroughRule{{Host: "example.com", Mode: models.PassThroughRecordOne}}})
		},
		"a pass-through rule's representative that did not succeed": func(t *testing.T, call string) clientView {
			t.Setenv(provenswitch.Env, "1")
			m := recorded.DeepCopy()
			m.Spec.Metadata["type"] = "config"
			m.TestModeInfo.Lifetime = models.LifetimeSession
			m.Spec.HTTPResp.StatusCode = http.StatusServiceUnavailable // served only when no recording succeeded
			return servedBy(t, &HTTP{Logger: zap.NewNop()}, m, call, &models.ConditionalDstCfg{Addr: "127.0.0.1:80", Port: 80},
				models.OutgoingOptions{PassThroughHosts: []models.PassThroughRule{{Host: "example.com", Mode: models.PassThroughRecordOne}}})
		},
	}
	for name, serve := range paths {
		t.Run(name, func(t *testing.T) {
			if got := serve(t, chunkedCall); !reflect.DeepEqual(got, onRecord) {
				t.Errorf("an HTTP/1.1 call read\n%+v\nserved, and\n%+v\non record", got, onRecord)
			}
			if got := serve(t, call10); got.length < 0 || got.encoding != nil || got.body != onRecord.body {
				t.Errorf("an HTTP/1.0 call read length %d, framing %q, body %q; want the body %q with its length", got.length, got.encoding, got.body, onRecord.body)
			}
		})
	}
}

// A recorded answer served again is written from the bytes kept for it
// (keptAnswers), and the bytes kept for one framing are never another
// call's: a chunked recording served again and again to HTTP/1.1 and
// HTTP/1.0 calls, either first, goes chunked to every HTTP/1.1 call and with
// its length to every HTTP/1.0 call, and is kept once it is served again.
func TestAKeptChunkedAnswerIsServedEachCallsFraming(t *testing.T) {
	const call10 = "GET /suite HTTP/1.0\r\nHost: example.com\r\nAccept-Encoding: gzip\r\n\r\n"
	answer := chunkedAnswers(t)["one chunk"]
	onRecord := clientViewOf(t, bufio.NewReader(strings.NewReader(answer)), chunkedCall)
	for _, http11First := range []bool{true, false} {
		ownKeptAnswers(t)
		h := &HTTP{Logger: zap.NewNop()}
		m := recordedThroughDisk(t, chunkedCall, answer)["V2 record"].DeepCopy()
		m.MarkPooled()
		if !keepsAnswer(m) {
			t.Fatal("the recording does not keep its answer: the row would not read the kept bytes")
		}
		order := []bool{http11First, http11First, http11First, !http11First, !http11First, !http11First, http11First, !http11First}
		for i, takesChunks := range order {
			out, err := h.buildMockResponseBytes(m, takesChunks)
			if err != nil {
				t.Fatal(err)
			}
			call := call10
			if takesChunks {
				call = chunkedCall
			}
			r := bufio.NewReader(bytes.NewReader(out))
			got := clientViewOf(t, r, call)
			if r.Buffered() > 0 {
				t.Errorf("HTTP/1.1 first %v, serve %d: %d bytes followed the answer", http11First, i, r.Buffered())
			}
			if takesChunks && !reflect.DeepEqual(got, onRecord) {
				t.Errorf("HTTP/1.1 first %v, serve %d: an HTTP/1.1 call read\n%+v\nand\n%+v\non record", http11First, i, got, onRecord)
			}
			if !takesChunks && (got.length != int64(len(onRecord.body)) || got.encoding != nil || got.body != onRecord.body) {
				t.Errorf("HTTP/1.1 first %v, serve %d: an HTTP/1.0 call read length %d, framing %q, body %q; want the body %q with its length",
					http11First, i, got.length, got.encoding, got.body, onRecord.body)
			}
		}
		if n := keptAnswers.builds.Load(); n < 2 || n == int64(len(order)) {
			t.Errorf("HTTP/1.1 first %v: %d serves built the answer %d times: it was never kept", http11First, len(order), n)
		}
	}
}
