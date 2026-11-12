package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
	yamlLib "gopkg.in/yaml.v3"
)

// readWatchedConn is a connection with its reads in progress counted. Each
// read that starts once the connection has been written to (the parser's
// answer) is told on reading, when nothing waits there already.
type readWatchedConn struct {
	net.Conn
	inFlight atomic.Int32
	answered atomic.Bool
	reading  chan struct{}
	// waitSize is the buffer size of the first read that started once the
	// connection had been written to.
	waitSize atomic.Int32
}

func (c *readWatchedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.answered.Store(true)
	return n, err
}

func (c *readWatchedConn) Read(p []byte) (int, error) {
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	if c.answered.Load() {
		c.waitSize.CompareAndSwap(0, int32(len(p)))
		select {
		case c.reading <- struct{}{}:
		default:
		}
	}
	return c.Conn.Read(p)
}

// A replay waits for a keep-alive connection's next request on a small read
// (pUtil.WaitReadSize, as the old loop read): the read holds its buffer for as
// long as the connection is idle, which for a client's pool is hours. The
// rest of the request is read on by its framing (readRequestHead).
func TestDecodeHTTPWaitsForTheNextRequestOnASmallRead(t *testing.T) {
	app, proxy := tcpPair(t)
	conn := &readWatchedConn{Conn: proxy, reading: make(chan struct{}, 1)}
	h := &HTTP{Logger: zap.NewNop()}
	rec := recorded("u", "/u", nil, "")
	rec.Spec.HTTPReq.ProtoMajor, rec.Spec.HTTPReq.ProtoMinor = 1, 1
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		returned <- h.decodeHTTP(ctx, []byte("POST /u HTTP/1.1\r\nHost: api.example\r\nContent-Length: 0\r\n\r\n"), conn, nil,
			&mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
	}()
	t.Cleanup(func() { cancel(); <-returned })
	// The bounds below only turn a hang into a failure.
	if got := readUntil(app, "\r\n\r\n", time.Minute); !strings.HasPrefix(got, "HTTP/1.1 200") {
		t.Fatalf("the request was answered %q, want the mock's 200", got)
	}
	select {
	case <-conn.reading: // waiting for the next request
	case <-time.After(time.Minute):
		t.Fatal("decodeHTTP did not read on for the next request")
	}
	if got := conn.waitSize.Load(); got != pUtil.WaitReadSize {
		t.Fatalf("decodeHTTP waits for the next request on a %d-byte read, want pUtil.WaitReadSize (%d)", got, pUtil.WaitReadSize)
	}
}

// A replay reads a POST whose body is many reads long (100 KiB: past one
// 32 KiB read, and a multiple of 1 KiB, the sizes the relay's reads changed at)
// whole, by its framing, matches it with its recording and answers it, and
// reads the connection's next call cleanly after it: a body read short would
// leave its tail to be parsed as the next request.
func TestDecodeHTTPReplaysAPostLongerThanARead(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 100<<10; i++ {
		b.WriteString(strconv.Itoa(i))
		b.WriteByte(',')
	}
	body := b.String()[:100<<10]
	big := recorded("big", "/big", nil, body)
	next := recorded("next", "/next", nil, "small")
	next.Spec.HTTPResp.Body = `{"next":true}`
	for _, m := range []*models.Mock{big, next} {
		m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, 1
	}
	app, proxy := tcpPair(t)
	h := &HTTP{Logger: zap.NewNop()}
	head := "POST /big HTTP/1.1\r\nHost: api.example\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	// The proxy's first read: the head and the body's first KiB. The rest
	// comes on the connection.
	first := head + body[:1<<10]
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		returned <- h.decodeHTTP(ctx, []byte(first), proxy, nil,
			&mockMemDb{mocks: []*models.Mock{big, next}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
	}()
	t.Cleanup(func() { cancel(); <-returned })
	go func() { _, _ = app.Write([]byte(body[1<<10:])) }()
	// The bounds below only turn a hang into a failure.
	if got := readUntil(app, `{"ok":true}`, time.Minute); !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.HasSuffix(got, `{"ok":true}`) {
		t.Fatalf("the 100 KiB POST was answered %q, want its recording's 200", got)
	}
	if _, err := app.Write([]byte("POST /next HTTP/1.1\r\nHost: api.example\r\nContent-Length: 5\r\n\r\nsmall")); err != nil {
		t.Fatal(err)
	}
	if got := readUntil(app, `{"next":true}`, time.Minute); !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.HasSuffix(got, `{"next":true}`) {
		t.Fatalf("the call after the 100 KiB POST was answered %q, want its recording's 200", got)
	}
}

// Every other path of a replay that serves a call and keeps the connection
// for the next waits for that request on a small read too (pUtil.WaitReadSize):
// a call a bypassRule passes through, one a pass-through egress rule answers,
// an async lane's keep-alive, and a miss passed through on-miss. Each row is
// red with its wait back on a 32 KiB read.
func TestDecodeHTTPWaitsForTheNextRequestOnASmallReadOnEachPath(t *testing.T) {
	dep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("real " + r.URL.Path))
	}))
	t.Cleanup(dep.Close)
	depAddr := strings.TrimPrefix(dep.URL, "http://")
	depPort, _ := strconv.Atoi(depAddr[strings.LastIndex(depAddr, ":")+1:])
	for _, tc := range []struct {
		name  string
		call  string
		dst   *models.ConditionalDstCfg
		opts  models.OutgoingOptions
		setup func(t *testing.T, h *HTTP)
	}{
		{
			name: "a call a bypassRule passes through",
			call: "GET /a HTTP/1.1\r\nHost: dep.internal\r\n\r\n",
			dst:  &models.ConditionalDstCfg{Addr: depAddr, Port: uint(depPort)},
			opts: models.OutgoingOptions{Rules: []models.BypassRule{{Host: `^dep\.internal$`}}},
		},
		{
			name: "a call a pass-through egress rule answers",
			call: "POST /x HTTP/1.1\r\nHost: app\r\nContent-Length: 0\r\n\r\n",
			dst:  &models.ConditionalDstCfg{Addr: "127.0.0.1:9999", Port: 9999},
			opts: models.OutgoingOptions{PassThroughPorts: []models.PassThroughRule{{Port: 9999, Mode: models.PassThroughSkip}}},
			setup: func(t *testing.T, _ *HTTP) {
				t.Setenv(provenswitch.Env, "1") // the switch reads the user's rules
			},
		},
		{
			name: "an async lane's keep-alive",
			call: "GET /w HTTP/1.1\r\nHost: cfg.local\r\n\r\n",
			setup: func(t *testing.T, h *HTTP) {
				h.SetAsyncEngine(async.NewEngine(zap.NewNop(),
					[]models.AsyncLane{{Name: "w", Type: "http", Match: map[string]string{"pathRegex": "^/w$"}}},
					map[string]async.AsyncParser{"http": h}))
			},
		},
		{
			name: "a miss passed through on-miss",
			call: "GET /m HTTP/1.1\r\nHost: dep.local\r\n\r\n",
			dst:  &models.ConditionalDstCfg{Addr: depAddr, Port: uint(depPort)},
			opts: models.OutgoingOptions{OnMiss: models.MissPassthrough},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, proxy := tcpPair(t)
			conn := &readWatchedConn{Conn: proxy, reading: make(chan struct{}, 1)}
			h := &HTTP{Logger: zap.NewNop()}
			if tc.setup != nil {
				tc.setup(t, h)
			}
			ctx, cancel := context.WithCancel(context.Background())
			returned := make(chan error, 1)
			go func() { returned <- h.decodeHTTP(ctx, []byte(tc.call), conn, tc.dst, &mockMemDb{}, tc.opts) }()
			t.Cleanup(func() { cancel(); <-returned })
			// The bounds below only turn a hang into a failure.
			if got := readUntil(app, "\r\n\r\n", time.Minute); !strings.HasPrefix(got, "HTTP/1.1 2") {
				t.Fatalf("the call was answered %q, want a 2xx (an async keep-alive is a 204)", got)
			}
			select {
			case <-conn.reading: // waiting for the next request
			case err := <-returned:
				t.Fatalf("decodeHTTP returned %v after the call, want it waiting for the next", err)
			case <-time.After(time.Minute):
				t.Fatal("decodeHTTP did not read on for the next request")
			}
			if got := conn.waitSize.Load(); got != pUtil.WaitReadSize {
				t.Fatalf("decodeHTTP waits for the next request on a %d-byte read, want pUtil.WaitReadSize (%d)", got, pUtil.WaitReadSize)
			}
		})
	}
}

// A replay whose context ends while decodeHTTP waits for the client's next
// request returns only once the goroutine that reads the client has ended:
// its read ended, and its last line logged. decodeHTTP returned the context's
// error at once and left that goroutine blocked on the read, which logged its
// failed read once the connection closed: in a parser test, through the
// test's logger after the test had ended (a DATA RACE under go test -race).
// Nothing closes the client here, so only the context's end can end the read.
func TestDecodeHTTPReturnsOnlyOnceItsReaderHasEnded(t *testing.T) {
	app, proxy := tcpPair(t)
	conn := &readWatchedConn{Conn: proxy, reading: make(chan struct{}, 1)}
	// Not the test's logger: what the goroutine logs after a failure here
	// must not fail the run by racing the test's end.
	core, logs := observer.New(zapcore.DebugLevel)
	h := &HTTP{Logger: zap.New(core)}
	rec := recorded("u", "/u", nil, "")
	rec.Spec.HTTPReq.ProtoMajor, rec.Spec.HTTPReq.ProtoMinor = 1, 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		returned <- h.decodeHTTP(ctx, []byte("POST /u HTTP/1.1\r\nHost: api.example\r\nContent-Length: 0\r\n\r\n"), conn, nil,
			&mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
	}()
	// The bounds below only turn a hang into a failure.
	if got := readUntil(app, "\r\n\r\n", time.Minute); !strings.HasPrefix(got, "HTTP/1.1 200") {
		t.Fatalf("the request was answered %q, want the mock's 200", got)
	}
	select {
	case <-conn.reading: // waiting for the next request
	case <-time.After(time.Minute):
		t.Fatal("decodeHTTP did not read on for the next request")
	}
	cancel()
	var err error
	select {
	case err = <-returned:
	case <-time.After(time.Minute):
		t.Fatal("decodeHTTP did not return once its context was cancelled")
	}
	if n := conn.inFlight.Load(); n != 0 {
		t.Fatalf("decodeHTTP returned with %d read(s) of the client still in progress: the goroutine it started outlives it", n)
	}
	if logs.FilterMessage("failed to read the request buffer from the client").Len() == 0 {
		t.Fatal("decodeHTTP returned before the goroutine it started had logged its last line")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeHTTP = %v, want the context's error", err)
	}
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		t.Errorf("logged an ERROR at the context's end: %s %v", e.Message, e.Context)
	}
}

// A replay whose context ends while it reads the rest of a request's header
// section returns the context's error and logs no ERROR: it logged the read's
// end with no error, so not as the context's. Nothing writes the rest here,
// so only the context's end can end the read.
func TestDecodeHTTPStoppedMidRequestLogsNoError(t *testing.T) {
	_, proxy := tcpPair(t)
	conn := &readWatchedConn{Conn: proxy, reading: make(chan struct{}, 1)}
	conn.answered.Store(true) // tell every read: the first is the header's
	core, logs := observer.New(zapcore.DebugLevel)
	h := &HTTP{Logger: zap.New(core)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		returned <- h.decodeHTTP(ctx, []byte("GET /u HTTP/1.1\r\nHost: api"), conn, nil, &mockMemDb{}, models.OutgoingOptions{})
	}()
	// The bounds below only turn a hang into a failure.
	select {
	case <-conn.reading:
	case <-time.After(time.Minute):
		t.Fatal("decodeHTTP did not read the rest of the request")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("decodeHTTP = %v, want the context's error", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("decodeHTTP did not return once its context was cancelled")
	}
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		t.Errorf("logged an ERROR at the context's end: %s %v", e.Message, e.Context)
	}
}

// A mock answers with the response header lines the dependency sent, not one
// comma-joined line per name. Two Set-Cookie lines joined into one is a single
// cookie to every client (the Expires date carries a comma of its own, so the
// join is not even splittable), and the app under test loses the second.
// Recorded by both record paths, written as mocks.yaml writes it, read back and
// served.
func TestAMockServesRepeatedResponseHeaderLinesAsRecorded(t *testing.T) {
	req := []byte("GET /login HTTP/1.1\r\nHost: upstream\r\n\r\n")
	resp := []byte("HTTP/1.1 200 OK\r\n" +
		"Set-Cookie: a=1; Path=/\r\n" +
		"Content-Type: text/plain\r\n" +
		"Set-Cookie: b=2; Expires=Wed, 21 Oct 2015 07:28:00 GMT\r\n" +
		"Content-Length: 2\r\n\r\nok")
	want := []string{"a=1; Path=/", "b=2; Expires=Wed, 21 Oct 2015 07:28:00 GMT"}

	h := &HTTP{Logger: zaptest.NewLogger(t)}
	v2, err := h.buildHTTPMock(&FinalHTTP{Req: req, Resp: resp, ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
		80, "test-conn", models.OutgoingOptions{})
	if err != nil || v2 == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", v2, err)
	}
	legacy := runLegacyParseFinalHTTP(t, h, req, resp, 80, "test-conn")

	for path, recorded := range map[string]*models.Mock{"V2 record": v2, "legacy record": legacy} {
		doc, err := mockdb.EncodeMock(recorded, zap.NewNop())
		if err != nil {
			t.Fatalf("%s: EncodeMock: %v", path, err)
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
			t.Fatalf("%s: DecodeMocks = (%d mocks, %v)", path, len(mocks), err)
		}

		served, err := h.buildMockResponseBytes(mocks[0])
		if err != nil {
			t.Fatalf("%s: buildMockResponseBytes: %v", path, err)
		}
		got, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(served)), nil)
		if err != nil {
			t.Fatalf("%s: the served response does not parse: %v\n%s", path, err, served)
		}
		if !reflect.DeepEqual(got.Header.Values("Set-Cookie"), want) {
			t.Errorf("%s: served Set-Cookie %q, recorded %q\n%s", path, got.Header.Values("Set-Cookie"), want, onDisk)
		}
		if ct := got.Header.Values("Content-Type"); !reflect.DeepEqual(ct, []string{"text/plain"}) {
			t.Errorf("%s: served Content-Type %q, recorded [text/plain]", path, ct)
		}
	}
}

// A response with no body by rule (to a HEAD, a 204, a 304) keeps the
// Content-Length it was sent with, recorded by both record paths, written as
// mocks.yaml writes it, read back and served. A HEAD's is the size of the
// object: recorded and served as 0, an S3 client's Stat took every object for
// empty and its read returned EOF without sending the GET. A response with a
// body still gets the length of the body served.
func TestANoBodyResponseKeepsItsContentLength(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("hello, object"))
	_ = zw.Close()

	for _, tc := range []struct {
		name, method, resp string
		wantCL             []string // nil: the body's own length
	}{
		{"HEAD", "HEAD", "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 37267\r\n\r\n", []string{"37267"}},
		{"204 without a Content-Length", "DELETE", "HTTP/1.1 204 No Content\r\n\r\n", []string{}},
		{"304", "GET", "HTTP/1.1 304 Not Modified\r\nEtag: \"v1\"\r\nContent-Length: 37267\r\n\r\n", []string{"37267"}},
		{"GET with a body", "GET", "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: " + strconv.Itoa(gz.Len()) + "\r\n\r\n" + gz.String(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := []byte(tc.method + " /bucket/mocks.bin HTTP/1.1\r\nHost: s3\r\n\r\n")
			h := &HTTP{Logger: zaptest.NewLogger(t)}
			v2, err := h.buildHTTPMock(&FinalHTTP{Req: req, Resp: []byte(tc.resp), ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
				80, "test-conn", models.OutgoingOptions{})
			if err != nil || v2 == nil {
				t.Fatalf("buildHTTPMock = (%v, %v)", v2, err)
			}
			legacy := runLegacyParseFinalHTTP(t, h, req, []byte(tc.resp), 80, "test-conn")

			for path, recorded := range map[string]*models.Mock{"V2 record": v2, "legacy record": legacy} {
				doc, err := mockdb.EncodeMock(recorded, zap.NewNop())
				if err != nil {
					t.Fatalf("%s: EncodeMock: %v", path, err)
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
					t.Fatalf("%s: DecodeMocks = (%d mocks, %v)", path, len(mocks), err)
				}

				served, err := h.buildMockResponseBytes(mocks[0])
				if err != nil {
					t.Fatalf("%s: buildMockResponseBytes: %v", path, err)
				}
				head, body, ok := bytes.Cut(served, []byte("\r\n\r\n"))
				if !ok {
					t.Fatalf("%s: served no header section:\n%s", path, served)
				}
				got, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(append(head, "\r\n\r\n"...))), &http.Request{Method: tc.method})
				if err != nil {
					t.Fatalf("%s: the served response does not parse: %v\n%s", path, err, served)
				}
				want := tc.wantCL
				if want == nil {
					want = []string{strconv.Itoa(len(body))}
					if dec, err := pkg.Decompress(zap.NewNop(), "gzip", body, pkg.MaxDecompressedSize); err != nil || string(dec) != "hello, object" {
						t.Errorf("%s: served body decodes to (%q, %v), want %q", path, dec, err, "hello, object")
					}
				} else if len(body) != 0 {
					t.Errorf("%s: served a %d-byte body with a response that has none", path, len(body))
				}
				if cl := got.Header.Values("Content-Length"); !reflect.DeepEqual(cl, want) && !(len(cl) == 0 && len(want) == 0) {
					t.Errorf("%s: served Content-Length %q, want %q\n%s", path, cl, want, onDisk)
				}
			}
		})
	}
}
