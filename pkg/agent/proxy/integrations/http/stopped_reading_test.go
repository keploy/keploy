package http

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// A response the app stopped reading at record, and closed the connection on
// before its end came (models.FaultStoppedReading), is served at replay
// as far as the app read it and no further, framed as the upstream sent it
// without its end (under its Content-Length, which the body served does not
// reach; as one chunk with no last chunk after it; or as a body the
// connection's close would end, as one is below HTTP/1.1, which has no
// chunks), and the connection held open until the app closes it. The fault
// alone frames it: the mock's head is a whole answer's, as the recorder
// writes it. The app sees what it saw at record. One that had all it wanted
// (the JSON it decodes) closes as it did. One that waits on the rest (an
// event stream it abandoned) waits, and its own timeout fires, as it did.
// Nothing it sends meanwhile is answered: the answer before it never ended.
// A whole response is served as before, with its length, and the
// connection's next request is answered.
func TestDecodeHTTPServesAnAnswerTheAppStoppedReadingWithoutItsEnd(t *testing.T) {
	const charge = `{"id":"ch_1","status":"pending"}`
	for _, row := range []struct {
		name   string
		header map[string]string
		body   string
		held   bool
		// sent is how the upstream framed the body at record, kept on the
		// fault.
		sent sentAs
		// http10: the call and its recording are HTTP/1.0.
		http10 bool
		// length is the Content-Length served, 0 for none; chunked: served
		// chunked. Neither: held, the body is ended only by the
		// connection's close.
		length  int64
		chunked bool
		// json: the app decodes the body's JSON and closes, as a Go
		// http.Client with keep-alives off does on Body.Close; else it
		// reads all the body it was served, waits for more until its own
		// timeout, and closes.
		json bool
	}{
		{name: "a chunked answer the app decoded the JSON of", header: map[string]string{"Content-Type": "application/json", "Content-Length": "32"},
			body: charge, held: true, sent: sentChunked, chunked: true, json: true},
		{name: "an answer with a length the app decoded the JSON of", header: map[string]string{"Content-Type": "application/json", "Content-Length": "32"},
			body: charge, held: true, sent: sentAs{length: 200}, length: 200, json: true},
		{name: "a chunked event stream the app abandoned", header: map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"},
			body: "data: 1\n\ndata: 2\n\n", held: true, sent: sentChunked, chunked: true},
		{name: "an event stream its upstream's close was to end, the app abandoned", header: map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"},
			body: "data: 1\n\ndata: 2\n\n", held: true, sent: sentByClose},
		{name: "a chunked answer the app read none of the body of", header: map[string]string{"Content-Length": "0"}, held: true, sent: sentChunked, chunked: true},
		{name: "an answer with a length the app read none of the body of", header: map[string]string{"Content-Length": "0"}, held: true, sent: sentAs{length: 50}, length: 50},
		{name: "an HTTP/1.0 answer with a length", header: map[string]string{"Content-Type": "text/plain", "Content-Length": "10"}, body: "0123456789",
			held: true, sent: sentAs{length: 100}, http10: true, length: 100},
		// A mock file can carry what its recorder never writes: a chunked
		// HTTP/1.0 answer, which HTTP/1.0 has no chunks for, and goes
		// framed by the close; a length the body served reaches, which
		// would end it, and it goes chunked; a head with framing headers of
		// its own, which the fault's framing replaces.
		{name: "an HTTP/1.0 answer sent chunked", header: map[string]string{"Content-Type": "text/plain", "Content-Length": "10"}, body: "0123456789",
			held: true, sent: sentChunked, http10: true},
		{name: "a length the body served reaches", header: map[string]string{"Content-Type": "text/plain", "Content-Length": "10"}, body: "0123456789",
			held: true, sent: sentAs{length: 10}, chunked: true},
		{name: "an answer with a Content-Length not its body's and Transfer-Encoding", header: map[string]string{"Content-Type": "text/plain", "Content-Length": "2000", "Transfer-Encoding": "chunked"},
			body: "0123456789", held: true, sent: sentChunked, chunked: true},
		{name: "an answer with Transfer-Encoding, sent with a length", header: map[string]string{"Content-Type": "text/plain", "Content-Length": "2000", "Transfer-Encoding": "chunked"},
			body: "0123456789", held: true, sent: sentAs{length: 300}, length: 300},
		{name: "a whole answer", header: map[string]string{"Content-Type": "application/json", "Content-Length": "32"}, body: charge, json: true},
	} {
		request := "POST /v1/charges HTTP/1.1\r\nHost: api.example\r\nContent-Length: 0\r\n\r\n"
		minor := 1
		if row.http10 {
			request, minor = "POST /v1/charges HTTP/1.0\r\nHost: api.example\r\nContent-Length: 0\r\n\r\n", 0
		}
		for _, way := range servedWays {
			proven, pooled := way.proven, way.pooled
			t.Run(row.name+way.suffix(), func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				var mocks []*models.Mock
				for _, name := range []string{"charge-1", "charge-2"} {
					m := recorded(name, "/v1/charges", nil, "")
					m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, minor
					m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Header: row.header, Body: row.body}
					m.Spec.Fault = stoppedReadingIf(row.held, len(row.body), row.sent)
					if pooled {
						m.MarkPooled()
					}
					mocks = append(mocks, m)
				}
				app, proxy := tcpPair(t)
				boundAppReads(t, app)
				served := &readsOnConn{Conn: proxy, readAgain: make(chan struct{}, 1), afterWrite: make(chan struct{}, 1)}
				h := &HTTP{Logger: zap.NewNop()}
				done := runServe(t, proxy, func(ctx context.Context) error {
					return h.decodeHTTP(ctx, []byte(request), served, nil,
						&mockMemDb{mocks: mocks, updateUnFilteredReturn: true}, models.OutgoingOptions{})
				})
				// What the serve wrote before it read on, alone, is the answer
				// the app read at record: its body, then its end (a whole
				// answer, or one framed by the close, which the bytes' end
				// ends), or none (a length or a chunk the bytes do not finish).
				raw := servedBytes(t, served, app, done)
				wrote, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
				if err != nil {
					t.Fatalf("served %q, which does not parse as a response: %v", raw, err)
				}
				wantEnd := error(nil)
				if row.held && (row.length != 0 || row.chunked) {
					wantEnd = io.ErrUnexpectedEOF
				}
				if body, err := io.ReadAll(wrote.Body); string(body) != row.body || !errors.Is(err, wantEnd) {
					t.Fatalf("served %q: its body %q, ended by %v; want %q, ended by %v", raw, body, err, row.body, wantEnd)
				}
				// The app reads it as the serve wrote it, then from the
				// connection.
				br := bufio.NewReader(io.MultiReader(bytes.NewReader(raw), app))
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("the call was answered with no response that parses: %v", err)
				}
				if resp.StatusCode != 200 {
					t.Fatalf("the call was answered %d, want the recorded 200", resp.StatusCode)
				}
				if got, want := len(resp.TransferEncoding) == 1 && resp.TransferEncoding[0] == "chunked", row.chunked; got != want {
					t.Fatalf("served with Transfer-Encoding %v, want chunked %v", resp.TransferEncoding, want)
				}
				if !row.held {
					if resp.ContentLength != int64(len(row.body)) {
						t.Fatalf("a whole answer served with a length of %d, want its body's %d", resp.ContentLength, len(row.body))
					}
					var v map[string]any
					if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v["id"] != "ch_1" {
						t.Fatalf("decoded %v (%v), want the recorded JSON", v, err)
					}
					if rest, err := io.ReadAll(resp.Body); err != nil || len(rest) != 0 {
						t.Fatalf("after the JSON: %q (%v), want the body's end", rest, err)
					}
					// The connection's next request is answered.
					if _, err := app.Write([]byte(request)); err != nil {
						t.Fatal(err)
					}
					next, err := http.ReadResponse(br, nil)
					if err != nil || next.StatusCode != 200 {
						t.Fatalf("the next request on the connection was answered %v (%v), want the second recording's 200", next, err)
					}
					return
				}
				wantLength := int64(-1)
				if row.length != 0 {
					wantLength = row.length
				}
				if resp.ContentLength != wantLength {
					t.Fatalf("served with a length of %d, want %d: the length the upstream sent, which the body served does not reach, or none", resp.ContentLength, wantLength)
				}
				if row.json {
					var v map[string]any
					if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v["id"] != "ch_1" {
						t.Fatalf("decoded %v (%v), want the recorded JSON", v, err)
					}
				} else {
					got := make([]byte, len(row.body))
					if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != row.body {
						t.Fatalf("read %q (%v), want the body the app read at record, %q", got, err, row.body)
					}
					// The app waits for the rest until its own timeout, as at
					// record: no byte and no end comes meanwhile.
					if err := app.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
						t.Fatal(err)
					}
					if n, err := resp.Body.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
						t.Fatalf("waiting on the rest read %d byte(s) (%v), want the app's own timeout", n, err)
					}
					boundAppReads(t, app)
				}
				// What the app sends now is not answered: the answer before
				// it never ended. The replay reads it and holds on, reading
				// again: the app's bytes do not let the connection go. Then
				// the app closes, and the replay lets the connection go.
				if _, err := app.Write([]byte(request)); err != nil {
					t.Fatal(err)
				}
				select {
				case <-served.readAgain:
				case err := <-done:
					t.Fatalf("decodeHTTP returned %v on the bytes the app sent, want the connection held until the app closes it", err)
				case <-time.After(time.Minute): // the bound only turns a hang into a failure
					t.Fatal("the replay did not read on after the bytes the app sent")
				}
				if err := app.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("decodeHTTP returned %v once the app closed, want nil", err)
					}
				case <-time.After(time.Minute): // the bound only turns a hang into a failure
					t.Fatal("decodeHTTP did not return once the app closed the connection")
				}
				// Read on to the connection's close: nothing more of the body,
				// and no end of it (a chunked body's last chunk), came. A body
				// whose read timed out stays failed (net/http keeps the error),
				// and the connection's bytes are read past it.
				_ = proxy.Close()
				if row.json {
					rest, err := io.ReadAll(resp.Body)
					if len(rest) != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("after the JSON: %q, ended with %v; want nothing, and no end of the body", rest, err)
					}
				}
				if tail, _ := io.ReadAll(br); len(tail) != 0 {
					t.Fatalf("served %q after the answer the app stopped reading, want nothing", tail)
				}
			})
		}
	}
}

// servedWays are the ways a row serves its mocks: with the proven switch off
// and on, each built for the serve, and pooled, served through the answer
// table that keeps a pooled mock's answer (keptAnswers).
var servedWays = []servedWay{{"", false}, {"1", false}, {"", true}, {"1", true}}

type servedWay struct {
	proven string
	pooled bool
}

func (w servedWay) suffix() string {
	if w.pooled {
		return "/pooled/KEPLOY_PROVEN=" + w.proven
	}
	return "/KEPLOY_PROVEN=" + w.proven
}

// readsOnConn is the proxy's side of a connection that says when its reader
// comes back for more: after a read that gave bytes (readAgain), a hold that
// read what the app sent and holds on, or a serve that answered it and waits
// for the next; and after a write (afterWrite, when not nil), a serve that
// wrote its answer and holds the connection or waits for the next request. It
// counts the bytes written to it (wrote), which tells a hold from an answer,
// and is all a serve wrote before it read on.
type readsOnConn struct {
	net.Conn
	gave       atomic.Bool
	wrote      atomic.Int64
	readAgain  chan struct{}
	afterWrite chan struct{}
}

// NetConn is the conn under c, so the serve asks its socket (heldPastFIN).
func (c *readsOnConn) NetConn() net.Conn { return c.Conn }

func (c *readsOnConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.wrote.Add(int64(n))
	return n, err
}

func (c *readsOnConn) Read(p []byte) (int, error) {
	if c.gave.Load() {
		select {
		case c.readAgain <- struct{}{}:
		default:
		}
	}
	if c.wrote.Load() > 0 {
		select {
		case c.afterWrite <- struct{}{}: // a nil afterWrite is never ready
		default:
		}
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.gave.Store(true)
	}
	return n, err
}

// servedBytes waits until the serve on served has written its answer and
// reads on, or has returned (done), and reads from the app's end exactly the
// bytes it wrote. A row checks them as they are, so a serve that wrote other
// bytes, or fewer, fails it at once, never by a wait on bytes that do not
// come. A serve that returned fails the row: a held answer, and a whole one
// on a connection that is not closed, read on. served needs an afterWrite.
func servedBytes(t *testing.T, served *readsOnConn, app net.Conn, done <-chan error) []byte {
	t.Helper()
	returned, early := error(nil), false
	select {
	case <-served.afterWrite:
	case returned = <-done:
		early = true
	case <-time.After(time.Minute): // the bound only turns a hang into a failure
		t.Fatal("the serve neither read on after its answer nor returned")
	}
	raw := make([]byte, served.wrote.Load())
	if _, err := io.ReadFull(app, raw); err != nil {
		t.Fatalf("read %q of the %d bytes the serve wrote: %v", raw, len(raw), err)
	}
	if early {
		t.Fatalf("decodeHTTP returned %v once it served %q, want the connection read on until the app closes it", returned, raw)
	}
	return raw
}

// runServe runs decode as runDecode does, then closes the proxy's end of the
// connection once decode returns, as the proxy closes the application's
// connection once its parser is done. A serve that returns without the answer
// a row waits on (an error, no hold) ends the app's read at once, by that
// close, and fails the row by its own assertion, never by the app's bound on
// its reads or the package's timeout.
func runServe(t *testing.T, proxy net.Conn, decode func(ctx context.Context) error) <-chan error {
	t.Helper()
	return runDecode(t, proxy, func(ctx context.Context) error {
		err := decode(ctx)
		_ = proxy.Close()
		return err
	})
}

// boundAppReads bounds the app's reads on its side of the connection. The
// bound only turns a hang into a failure of the row: a serve writes its whole
// answer at once, so a row that waits for bytes a broken serve never sends
// (a chunk-size line's end, the next answer) fails on its own, not the
// package by go test's -timeout.
func boundAppReads(t *testing.T, app net.Conn) {
	t.Helper()
	if err := app.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

// An answer with no body by rule (to a HEAD, a 204) is whole once its head
// is, even with a stopped_reading fault: it is served as recorded,
// a HEAD's with the Content-Length it was sent with (the size of the body a
// GET gets), and the connection's next request is answered. The recorder
// never records one so (its end is its head's); a mock file can.
func TestDecodeHTTPServesAnAnswerTheAppStoppedReadingWithNoBodyWhole(t *testing.T) {
	for _, row := range []struct {
		name, method string
		status       int
		header       map[string]string
		length       int64
	}{
		{"a HEAD answer", "HEAD", 200, map[string]string{"Content-Type": "application/json", "Content-Length": "5000"}, 5000},
		{"a 204", "POST", 204, map[string]string{"X-Request-Id": "r1"}, 0},
	} {
		for _, way := range servedWays {
			proven, pooled := way.proven, way.pooled
			t.Run(row.name+way.suffix(), func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				request := row.method + " /v1/charges HTTP/1.1\r\nHost: api.example\r\nContent-Length: 0\r\n\r\n"
				if row.method == "HEAD" {
					request = "HEAD /v1/charges HTTP/1.1\r\nHost: api.example\r\n\r\n"
				}
				var mocks []*models.Mock
				for _, name := range []string{"charge-1", "charge-2"} {
					m := httpMock(name, row.method, "/v1/charges")
					m.Spec.HTTPReq.Header = map[string]string{"Host": "api.example"}
					m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, 1
					m.Spec.HTTPResp = &models.HTTPResp{StatusCode: row.status, Header: row.header}
					m.Spec.Fault = stoppedReadingIf(true, 0, sentAs{length: row.length})
					if pooled {
						m.MarkPooled()
					}
					mocks = append(mocks, m)
				}
				app, proxy := tcpPair(t)
				boundAppReads(t, app)
				served := &readsOnConn{Conn: proxy, readAgain: make(chan struct{}, 1)}
				h := &HTTP{Logger: zap.NewNop()}
				done := runServe(t, proxy, func(ctx context.Context) error {
					return h.decodeHTTP(ctx, []byte(request), served, nil,
						&mockMemDb{mocks: mocks, updateUnFilteredReturn: true}, models.OutgoingOptions{})
				})
				br := bufio.NewReader(app)
				for i := 1; i <= 2; i++ {
					resp, err := http.ReadResponse(br, &http.Request{Method: row.method})
					if err != nil {
						t.Fatalf("call %d was answered with no response that parses: %v", i, err)
					}
					if resp.StatusCode != row.status || resp.ContentLength != row.length {
						t.Fatalf("call %d was answered %d with a length of %d, want the recorded %d, %d", i, resp.StatusCode, resp.ContentLength, row.status, row.length)
					}
					if rest, err := io.ReadAll(resp.Body); err != nil || len(rest) != 0 {
						t.Fatalf("call %d: a body of %q (%v), want none", i, rest, err)
					}
					if i == 2 {
						break
					}
					// The answer was whole: the connection's next request is
					// answered, not read and dropped by a hold. The serve reads
					// it, then reads on: having written the answer, or not (a
					// hold), which fails the row at once.
					before := served.wrote.Load()
					if _, err := app.Write([]byte(request)); err != nil {
						t.Fatal(err)
					}
					select {
					case <-served.readAgain:
					case err := <-done:
						t.Fatalf("decodeHTTP returned %v before it answered the connection's next request, want it answered", err)
					case <-time.After(time.Minute): // the bound only turns a hang into a failure
						t.Fatal("the replay did not read on after the connection's next request")
					}
					if served.wrote.Load() == before {
						t.Fatal("the connection's next request was read and not answered, want it answered: the answer before it was whole")
					}
				}
			})
		}
	}
}

// An async lane's recorded delivery the app stopped reading at record is
// served by the same fault path as a call's answer (serveFault): without its
// end, the connection then held until the app closes it, as at record. A call
// a pass-through egress rule answers never serves a mock with a fault: it is
// no representative answer (passthrough_egress.go).
func TestTheAsyncLaneServesAnAnswerTheAppStoppedReadingOpen(t *testing.T) {
	const body = "data: 1\n\ndata: 2\n\n"
	answer := func() *models.HTTPResp {
		return &models.HTTPResp{StatusCode: 200, Body: body,
			Header: map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"}}
	}
	for _, row := range []struct {
		name  string
		call  string
		dst   *models.ConditionalDstCfg
		opts  models.OutgoingOptions
		mocks []*models.Mock
		setup func(h *HTTP)
	}{
		{
			name: "an async lane's recorded delivery",
			call: "GET /w HTTP/1.1\r\nHost: cfg.local\r\n\r\n",
			setup: func(h *HTTP) {
				e := async.NewEngine(zap.NewNop(),
					[]models.AsyncLane{{Name: "w", Type: "http", Match: map[string]string{"pathRegex": "^/w$"}}},
					map[string]async.AsyncParser{"http": h})
				e.Load([]*models.Mock{{Name: "w-1", Kind: models.HTTP, Spec: models.MockSpec{
					HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://cfg.local/w", ProtoMajor: 1, ProtoMinor: 1},
					HTTPResp: answer(),
					Fault:    stoppedReadingIf(true, len(body), sentChunked),
					Async:    &models.AsyncMeta{Lane: "w", Seq: 1},
				}}})
				h.SetAsyncEngine(e)
			},
		},
	} {
		for _, proven := range []string{"", "1"} {
			t.Run(row.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				app, proxy := tcpPair(t)
				boundAppReads(t, app)
				h := &HTTP{Logger: zap.NewNop()}
				if row.setup != nil {
					row.setup(h)
				}
				done := runServe(t, proxy, func(ctx context.Context) error {
					return h.decodeHTTP(ctx, []byte(row.call), proxy, row.dst, &mockMemDb{mocks: row.mocks}, row.opts)
				})
				br := bufio.NewReader(app)
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("the call was answered with no response that parses: %v", err)
				}
				if resp.StatusCode != 200 || resp.ContentLength != -1 || len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
					t.Fatalf("answered %d with a length of %d and Transfer-Encoding %v, want the recorded 200, chunked, with no length",
						resp.StatusCode, resp.ContentLength, resp.TransferEncoding)
				}
				got := make([]byte, len(body))
				if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != body {
					t.Fatalf("read %q (%v), want the body the app read at record, %q", got, err, body)
				}
				// The app waits for the rest until its own timeout: no byte
				// and no end comes, and the connection stays open.
				if err := app.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				if n, err := resp.Body.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("waiting on the rest read %d byte(s) (%v), want the app's own timeout", n, err)
				}
				select {
				case err := <-done:
					t.Fatalf("decodeHTTP returned %v while the app still held the connection, want it open", err)
				default:
				}
				if err := app.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("decodeHTTP returned %v once the app closed, want nil", err)
					}
				case <-time.After(time.Minute): // the bound only turns a hang into a failure
					t.Fatal("decodeHTTP did not return once the app closed the connection")
				}
				_ = proxy.Close()
				boundAppReads(t, app)
				if tail, _ := io.ReadAll(br); len(tail) != 0 {
					t.Fatalf("served %q after the answer the app stopped reading, want nothing (no end of it)", tail)
				}
			})
		}
	}
}

// wantStoppedReading fails t unless m is an exchange recorded as far as the
// app read it (models.FaultStoppedReading): a 200 with body, a whole
// answer's head (the Content-Length of the body the app read, no
// Transfer-Encoding), the fault keeping the framing the upstream sent the body
// with (sent), no note of digests to recompute at replay (the app never had
// the body's end, so it checked no digest, and its digest headers are served
// as it saw them), and an answer whose body, served from it as the mock file
// gives it back, is framed as the upstream sent it and has no end: short of
// its Content-Length, or its last chunk never coming; a body framed by the
// close ends only by the close the replay never sends, here the end of the
// bytes served. Given back by a reader of the mock file that drops the fault
// (an older keploy's codec, writing the mock back), it is served whole, with
// its length.
func wantStoppedReading(t *testing.T, m *models.Mock, body string, sent sentAs) {
	t.Helper()
	got, f := m.Spec.HTTPResp, m.Spec.Fault
	if got == nil || got.StatusCode != 200 || got.Body != body {
		t.Fatalf("recorded %+v, want a 200 with the body %q the app read", got, body)
	}
	want := models.Fault{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: int64(len(body)),
		ContentLength: sent.length, Chunked: sent.chunked, AppEndedFirst: true,
		Observed: "the application stopped reading partway through the answer and closed the connection"}
	if f == nil || *f != want {
		t.Fatalf("recorded the fault %+v, want %+v", f, want)
	}
	if note := m.Spec.Metadata[models.MetadataBodyDigestsKey]; note != "" {
		t.Fatalf("noted the digests %q to recompute at replay, want none: the app never had the body's end", note)
	}
	if cl, te := got.Header["Content-Length"], got.Header["Transfer-Encoding"]; cl != strconv.Itoa(len(body)) || te != "" {
		t.Fatalf("recorded with Content-Length %q and Transfer-Encoding %q, want a whole answer's head: %d, none", cl, te, len(body))
	}
	// Served as the mock file gives it back: the spec, its fault with it,
	// written and read again.
	file, err := yamlLib.Marshal(models.HTTPSchema{Response: *got, Fault: f})
	if err != nil {
		t.Fatal(err)
	}
	var spec models.HTTPSchema
	if err := yamlLib.Unmarshal(file, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Fault == nil || *spec.Fault != want || spec.Response.Body != body {
		t.Fatalf("the mock file gave back the fault %+v and the body %q:\n%s", spec.Fault, spec.Response.Body, file)
	}
	read := *m
	read.Spec.HTTPResp, read.Spec.Fault = &spec.Response, spec.Fault
	resp, served, err := serveFrom(t, &read)
	wantLength, wantEnd := int64(-1), io.ErrUnexpectedEOF
	if sent.length > 0 {
		wantLength = sent.length
	} else if !sent.chunked {
		wantEnd = nil
	}
	if chunked := len(resp.TransferEncoding) == 1 && resp.TransferEncoding[0] == "chunked"; string(served) != body ||
		resp.ContentLength != wantLength || chunked != sent.chunked || !errors.Is(err, wantEnd) {
		t.Fatalf("served %q with a length of %d, chunked %v, ending with %v; want %q, with a length of %d, chunked %v, ending with %v",
			served, resp.ContentLength, chunked, err, body, wantLength, sent.chunked, wantEnd)
	}
	// The mock file as a reader that drops the fault gives it back (an older
	// codec writing the mock again): the spec's key gone, the file read again.
	var doc map[string]any
	if err := yamlLib.Unmarshal(file, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["fault"]; !ok {
		t.Fatalf("no fault key in the mock file's spec:\n%s", file)
	}
	delete(doc, "fault")
	dropped, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var unmarked models.HTTPSchema
	if err := yamlLib.Unmarshal(dropped, &unmarked); err != nil {
		t.Fatal(err)
	}
	read.Spec.HTTPResp, read.Spec.Fault = &unmarked.Response, unmarked.Fault
	resp, served, err = serveFrom(t, &read)
	if string(served) != body || resp.ContentLength != int64(len(body)) || err != nil {
		t.Fatalf("its fault dropped, served %q with a length of %d, ending with %v; want %q whole, with its length", served, resp.ContentLength, err, body)
	}
}

// sentAs is how the upstream framed an answer's body at record: by its
// Content-Length (length), chunked, or, with neither, by its close.
type sentAs struct {
	length  int64
	chunked bool
}

var (
	sentChunked = sentAs{chunked: true}
	sentByClose = sentAs{}
)

// stoppedReadingIf is the fault of an answer the app stopped reading after n
// bytes of its body, its upstream having framed it as sent, when stopped,
// else nil.
func stoppedReadingIf(stopped bool, n int, sent sentAs) *models.Fault {
	if !stopped {
		return nil
	}
	return &models.Fault{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: int64(n), ContentLength: sent.length, Chunked: sent.chunked}
}

// wireConn takes what is written to it, and reads as a connection the app
// closed: an answer served on it ends where the replay holds the connection.
// It has no socket under it, so a hold past the app's FIN (a fault marked
// AppEndedFirst, as a recording writes it: heldPastFIN) waits on ctx alone:
// its first read ends the serve's ctx too (end), as the proxy's would end
// with the app's session.
type wireConn struct {
	net.Conn
	out bytes.Buffer
	end context.CancelFunc
}

func (c *wireConn) Write(p []byte) (int, error) { return c.out.Write(p) }
func (c *wireConn) Read([]byte) (int, error) {
	c.end()
	return 0, io.EOF
}

// newWireConn is a wireConn and the ctx to serve on it with, which its first
// read ends.
func newWireConn(t *testing.T) (*wireConn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &wireConn{end: cancel}, ctx
}

// serveFrom is the answer replay serves from m, parsed: its response, the body
// read to the end of the bytes served, and how that read ended. A mock with a
// fault is served as decodeHTTP serves it (serveFault), on a connection the
// app closes once the answer is out.
func serveFrom(t *testing.T, m *models.Mock) (*http.Response, []byte, error) {
	t.Helper()
	h := &HTTP{Logger: zap.NewNop()}
	var wire []byte
	over := false
	if m.Spec.Fault != nil {
		c, ctx := newWireConn(t)
		var err error
		if over, err = h.serveFault(ctx, c, m, m.Spec.Fault); err != nil {
			t.Fatal(err)
		}
		wire = c.out.Bytes()
	}
	if !over {
		var err error
		if wire, err = h.buildMockResponseBytes(m); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(wire)), nil)
	if err != nil {
		t.Fatalf("the answer served does not parse: %v", err)
	}
	served, err := io.ReadAll(resp.Body)
	return resp, served, err
}

// A compressed answer the app stopped reading at record is served with its
// stream flushed, not ended: the app's decoder gives what the app decoded at
// record, then waits on the rest until the app's own timeout, as at record.
// Ended (gzip's trailer, brotli's last meta-block), a decoder that stops at
// its stream's end (a gzip reader taking one member) ends the body the app
// never saw the end of. A brotli reader does not stop there: at a stream's
// end it reads its source on, and waits on a held connection as on a flushed
// stream. So the bytes served are decoded again as all there is: a held
// answer's stream ends short (io.ErrUnexpectedEOF), a whole one's ends. gzip
// and br bodies are recorded decoded and compressed again at replay. A
// deflate body is neither: the mock keeps the bytes the app read, and they
// are served as they are. The answer is served as the mock file (YAML) or the
// mock store (BSON) gives it back. A whole answer's stream ends, as before.
// The upstream's Content-Length is weighed against the bytes served, which
// for gzip and br is keploy's compressor's stream, here longer than the text
// decoded: a length those bytes reach would end the body, and it goes
// chunked; a length one byte past them is sent.
func TestDecodeHTTPServesACompressedAnswerTheAppStoppedReadingWithNoStreamEnd(t *testing.T) {
	const request = "POST /v1/events HTTP/1.1\r\nHost: api.example\r\nContent-Length: 0\r\n\r\n"
	text := "data: 1\n\ndata: 2\n\n"
	deflated := func(end bool) string {
		var b bytes.Buffer
		w, err := flate.NewWriter(&b, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		finish := w.Flush // what a server streaming events sends after each
		if end {
			finish = w.Close
		}
		if err := finish(); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	codecs := []struct {
		name string
		back func(models.HTTPResp, *models.Fault) (models.HTTPResp, *models.Fault, error)
	}{
		{"yaml", func(in models.HTTPResp, f *models.Fault) (models.HTTPResp, *models.Fault, error) {
			var out models.HTTPSchema
			b, err := yamlLib.Marshal(models.HTTPSchema{Response: in, Fault: f})
			if err == nil {
				err = yamlLib.Unmarshal(b, &out)
			}
			return out.Response, out.Fault, err
		}},
		{"bson", func(in models.HTTPResp, f *models.Fault) (models.HTTPResp, *models.Fault, error) {
			type spec struct {
				Resp  models.HTTPResp `bson:"resp"`
				Fault *models.Fault   `bson:"fault,omitempty"`
			}
			var out spec
			b, err := bson.Marshal(spec{Resp: in, Fault: f})
			if err == nil {
				err = bson.Unmarshal(b, &out)
			}
			return out.Resp, out.Fault, err
		}},
	}
	for _, enc := range []struct {
		name string
		// body is the mock's body, as the recorder keeps it.
		body   func(end bool) string
		decode func(io.Reader) (io.Reader, error)
	}{
		{"gzip", func(bool) string { return text }, func(r io.Reader) (io.Reader, error) {
			z, err := gzip.NewReader(r)
			if err != nil {
				return nil, err
			}
			z.Multistream(false)
			return z, nil
		}},
		{"br", func(bool) string { return text }, func(r io.Reader) (io.Reader, error) { return brotli.NewReader(r), nil }},
		{"deflate", deflated, func(r io.Reader) (io.Reader, error) { return flate.NewReader(r), nil }},
	} {
		// served is how many body bytes a held answer is served with:
		// keploy's compressor's flushed stream of the text for gzip and br,
		// the bytes the app read for deflate, which is served as it is.
		served := int64(len(enc.body(false)))
		if enc.name != "deflate" {
			b, err := pkg.CompressWithoutEnd(zap.NewNop(), enc.name, []byte(text))
			if err != nil {
				t.Fatal(err)
			}
			served = int64(len(b))
		}
		if enc.name != "deflate" && served <= int64(len(text)) {
			t.Fatalf("%s: the bytes served (%d) are not longer than the text decoded (%d): the rows below would not tell a length weighed against the one from one weighed against the other", enc.name, served, len(text))
		}
		for _, row := range []struct {
			name   string
			header map[string]string
			held   bool
			// sent is how the upstream framed the body; length, chunked:
			// how it is served (length 0: none). A length the compressed
			// body served reaches would end it: it goes chunked.
			sent    sentAs
			length  int64
			chunked bool
		}{
			{"held, sent chunked", map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"}, true, sentChunked, 0, true},
			{"held, sent with a length", map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"}, true, sentAs{length: 1000}, 1000, false},
			{"held, sent with a length the body served reaches", map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"}, true, sentAs{length: served}, 0, true},
			{"held, sent with a length one byte past the body served", map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"}, true, sentAs{length: served + 1}, served + 1, false},
			{"held, its upstream's close to end it", map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"}, true, sentByClose, 0, false},
			{"whole", map[string]string{"Content-Type": "text/plain", "Content-Length": "18"}, false, sentAs{}, 0, false},
		} {
			for _, codec := range codecs {
				for _, proven := range []string{"", "1"} {
					t.Run(enc.name+"/"+row.name+"/"+codec.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						header := map[string]string{"Content-Encoding": enc.name}
						for k, v := range row.header {
							header[k] = v
						}
						body := enc.body(!row.held)
						atRecord := stoppedReadingIf(row.held, len(body), row.sent)
						resp, fault, err := codec.back(models.HTTPResp{StatusCode: 200, Header: header, Body: body}, atRecord)
						if err != nil {
							t.Fatal(err)
						}
						if (fault != nil) != row.held || fault != nil && *fault != *atRecord || resp.Body != body {
							t.Fatalf("the %s round trip gave back the fault %+v and a body of %d bytes, want %+v and %d", codec.name, fault, len(resp.Body), atRecord, len(body))
						}
						var mocks []*models.Mock
						for _, name := range []string{"events-1", "events-2"} {
							m := recorded(name, "/v1/events", nil, "")
							m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, 1
							r := resp
							m.Spec.HTTPResp = &r
							m.Spec.Fault = fault
							mocks = append(mocks, m)
						}
						app, proxy := tcpPair(t)
						boundAppReads(t, app)
						h := &HTTP{Logger: zap.NewNop()}
						done := runServe(t, proxy, func(ctx context.Context) error {
							return h.decodeHTTP(ctx, []byte(request), proxy, nil,
								&mockMemDb{mocks: mocks, updateUnFilteredReturn: true}, models.OutgoingOptions{})
						})
						served, err := http.ReadResponse(bufio.NewReader(app), nil)
						if err != nil || served.StatusCode != 200 {
							t.Fatalf("the call was answered %v (%v), want the recorded 200", served, err)
						}
						if row.held {
							wantLength := int64(-1)
							if row.length != 0 {
								wantLength = row.length
							}
							if chunked := len(served.TransferEncoding) == 1 && served.TransferEncoding[0] == "chunked"; served.ContentLength != wantLength || chunked != row.chunked {
								t.Fatalf("served with a length of %d, chunked %v; want %d, chunked %v", served.ContentLength, chunked, wantLength, row.chunked)
							}
						}
						var wire bytes.Buffer
						dec, err := enc.decode(io.TeeReader(served.Body, &wire))
						if err != nil {
							t.Fatalf("the app's %s decoder did not start: %v", enc.name, err)
						}
						got := make([]byte, len(text))
						if _, err := io.ReadFull(dec, got); err != nil || string(got) != text {
							t.Fatalf("decoded %q (%v), want %q", got, err, text)
						}
						if !row.held {
							if rest, err := io.ReadAll(dec); err != nil || len(rest) != 0 {
								t.Fatalf("after the text: %q (%v), want the stream's end", rest, err)
							}
							wantStreamEnd(t, enc.decode, wire.Bytes(), true)
							return
						}
						// The app waits on the rest until its own timeout, as at record.
						if err := app.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
							t.Fatal(err)
						}
						if n, err := dec.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
							t.Fatalf("waiting on the rest decoded %d byte(s) (%v), want the app's own timeout: the stream's end came, which the app never saw", n, err)
						}
						// The decoder took every byte served before it waited.
						wantStreamEnd(t, enc.decode, wire.Bytes(), false)
						if err := app.(*net.TCPConn).CloseWrite(); err != nil {
							t.Fatal(err)
						}
						select {
						case err := <-done:
							if err != nil {
								t.Fatalf("decodeHTTP returned %v once the app closed, want nil", err)
							}
						case <-time.After(time.Minute): // the bound only turns a hang into a failure
							t.Fatal("decodeHTTP did not return once the app closed the connection")
						}
					})
				}
			}
		}
	}
}

// wantStreamEnd fails t unless the compressed stream served, decoded as all
// there is, ends where ended says: a whole answer's at its end, a held
// answer's short of one (io.ErrUnexpectedEOF), its text whole either way.
func wantStreamEnd(t *testing.T, decode func(io.Reader) (io.Reader, error), served []byte, ended bool) {
	t.Helper()
	dec, err := decode(bytes.NewReader(served))
	if err != nil {
		t.Fatalf("the bytes served do not start a stream: %v", err)
	}
	text, err := io.ReadAll(dec)
	if ended && err != nil || !ended && !errors.Is(err, io.ErrUnexpectedEOF) || string(text) != "data: 1\n\ndata: 2\n\n" {
		t.Fatalf("the %d bytes served decode to %q, ending with %v; want the text, and the stream's end %v", len(served), text, err, ended)
	}
}

// A held answer's bytes on the wire: its head framed as the upstream sent it,
// then its body, without its end, and nothing after it until the app closes.
// Sent chunked, the body goes as one chunk, `<size in hex>\r\n<body>\r\n`,
// with no last chunk; sent with a length, under that length, which it does not
// reach; framed by the upstream's close, as it is, with no close after it.
// Below HTTP/1.1, which has no chunks (HTTP/1.0, or a mock written with no
// proto_major, whose answer goes as HTTP/0.0), a chunked body goes as one
// framed by the close; and a length the body reaches, which would end it, is
// not sent: the body goes chunked (framed by the close below HTTP/1.1). An
// answer the app read none of the body of is its
// head alone. The mock's head is a whole answer's, as the recorder writes it:
// the fault alone frames the answer.
func TestAHeldAnswerGoesOutFramedAsSentAndNothingAfterIt(t *testing.T) {
	const events = "data: 1\n\ndata: 2\n\n"
	for _, row := range []struct {
		name         string
		major, minor int
		body         string
		sent         sentAs
		want         string
	}{
		{"a body sent chunked", 1, 1, events, sentChunked,
			"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n12\r\n" + events + "\r\n"},
		{"no body, sent chunked", 1, 1, "", sentChunked, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n"},
		{"a body sent with a length", 1, 1, events, sentAs{length: 1000},
			"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: text/event-stream\r\n\r\n" + events},
		{"no body, sent with a length", 1, 1, "", sentAs{length: 1000},
			"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: text/event-stream\r\n\r\n"},
		{"a body its upstream's close was to end", 1, 1, events, sentByClose,
			"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + events},
		{"a body sent with a length it reaches", 1, 1, events, sentAs{length: 18},
			"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n12\r\n" + events + "\r\n"},
		{"a body over HTTP/1.0 sent chunked", 1, 0, events, sentChunked,
			"HTTP/1.0 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + events},
		{"a body over HTTP/1.0 sent with a length", 1, 0, events, sentAs{length: 1000},
			"HTTP/1.0 200 OK\r\nContent-Length: 1000\r\nContent-Type: text/event-stream\r\n\r\n" + events},
		{"a body over HTTP/1.0 sent with a length it reaches", 1, 0, events, sentAs{length: 18},
			"HTTP/1.0 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + events},
		// A mock file with no proto_major: its answer goes as HTTP/0.0,
		// which has no chunks either, never chunked.
		{"a body with no protocol version sent chunked", 0, 0, events, sentChunked,
			"HTTP/0.0 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + events},
		{"a body with no protocol version sent with a length it reaches", 0, 0, events, sentAs{length: 18},
			"HTTP/0.0 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + events},
		{"a body over HTTP/0.9 sent chunked", 0, 9, events, sentChunked,
			"HTTP/0.9 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + events},
		{"a body over HTTP/2.0 sent chunked", 2, 0, events, sentChunked,
			"HTTP/2.0 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n12\r\n" + events + "\r\n"},
	} {
		for _, way := range servedWays {
			proven, pooled := way.proven, way.pooled
			t.Run(row.name+way.suffix(), func(t *testing.T) {
				checkHeldWire(t, proven, pooled, row.major, row.minor, row.body, len(row.body), row.sent, row.want)
			})
		}
	}
}

// TestAHeldAnswerServesOnlyTheBytesItsFaultSends: a mock whose body holds
// more than its fault's body_bytes (one written or edited by hand) is served
// only those bytes, framed as sent, and then nothing until the app closes.
func TestAHeldAnswerServesOnlyTheBytesItsFaultSends(t *testing.T) {
	const events, read = "data: 1\n\ndata: 2\n\n", "data: 1\n"
	for _, row := range []struct {
		name string
		sent sentAs
		want string
	}{
		{"sent chunked", sentChunked,
			"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n8\r\n" + read + "\r\n"},
		{"sent with a length", sentAs{length: 1000},
			"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: text/event-stream\r\n\r\n" + read},
		{"its upstream's close was to end it", sentByClose,
			"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + read},
	} {
		for _, way := range servedWays {
			proven, pooled := way.proven, way.pooled
			t.Run(row.name+way.suffix(), func(t *testing.T) {
				checkHeldWire(t, proven, pooled, 1, 1, events, len(read), row.sent, row.want)
			})
		}
	}
}

// A mock the agent's disk store read keeps its response apart until a serve
// loads it (Mock.HasSpilledResponse, as diskmocks' readAt sets it).
// decodeHTTP's match loads it as it claims the mock; a parser that enacts a
// mock's fault itself (EnactFault: keploy enterprise's SQS replay) hands the
// mock on as the store gave it. Its fault is served from the response loaded
// back, as from one in the mock, and the mock handed in, which other
// connections may be matching, keeps its response apart. A timeout after the
// head (faultHead) is served so too.
func TestAFaultAfterTheHeadIsServedFromAResponseKeptApart(t *testing.T) {
	const events, read = "data: 1\n\ndata: 2\n\n", "data: 1\n"
	whole := map[string]string{"Content-Type": "text/event-stream", "Content-Length": "18"}
	for _, row := range []struct {
		name   string
		header map[string]string
		fault  *models.Fault
		want   string
	}{
		{"stopped_reading, sent chunked", whole, stoppedReadingIf(true, len(read), sentChunked),
			"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n8\r\n" + read + "\r\n"},
		{"stopped_reading, sent with a length", whole, stoppedReadingIf(true, len(read), sentAs{length: 1000}),
			"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: text/event-stream\r\n\r\n" + read},
		{"stopped_reading, its upstream's close to end it", whole, stoppedReadingIf(true, len(read), sentByClose),
			"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + read},
		{"a timeout after the head", map[string]string{"Content-Length": "1000"}, &models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: int64(len(read))},
			"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n" + read},
	} {
		for _, proven := range []string{"", "1"} {
			t.Run(row.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				m := recorded("events-1", "/v1/events", nil, "")
				m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, 1
				m.Spec.HTTPResp, m.Spec.Fault = nil, row.fault
				m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
					return &models.HTTPResp{StatusCode: 200, Header: row.header, Body: events}, nil, nil
				})
				m.MarkPooled()
				h := &HTTP{Logger: zap.NewNop()}
				c, ctx := newWireConn(t) // the app closes once the answer is out
				over, err := h.EnactFault(ctx, c, m)
				if err != nil || !over {
					t.Fatalf("EnactFault = %v, %v; want the fault served, the call over", over, err)
				}
				if got := c.out.String(); got != row.want {
					t.Fatalf("served %q, want %q", got, row.want)
				}
				if m.Spec.HTTPResp != nil || !m.HasSpilledResponse() {
					t.Fatal("the serve loaded the response into the mock handed in, which other connections may be matching; want it loaded into a copy")
				}
			})
		}
	}
}

// checkHeldWire serves a mock of body, recorded over HTTP/major.minor, its
// stopped_reading fault sending bodyBytes of it framed as sent, and checks the
// app gets want on the wire, then nothing until it closes, and the serve ends
// then. The app sends its request over HTTP/1.1 to a mock below HTTP/1.0 (one
// written with no proto_major) or above HTTP/1.1. It reads exactly the bytes
// the serve wrote before it held the connection (servedBytes).
func checkHeldWire(t *testing.T, proven string, pooled bool, major, minor int, body string, bodyBytes int, sent sentAs, want string) {
	t.Helper()
	t.Setenv("KEPLOY_PROVEN", proven)
	m := recorded("events-1", "/v1/events", nil, "")
	m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = major, minor
	m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: body,
		Header: map[string]string{"Content-Type": "text/event-stream", "Content-Length": strconv.Itoa(len(body))}}
	m.Spec.Fault = stoppedReadingIf(true, bodyBytes, sent)
	if pooled {
		m.MarkPooled()
	}
	app, proxy := tcpPair(t)
	boundAppReads(t, app)
	h := &HTTP{Logger: zap.NewNop()}
	appMinor := 1
	if major == 1 {
		appMinor = minor
	}
	request := "POST /v1/events HTTP/1." + strconv.Itoa(appMinor) + "\r\nHost: api.example\r\nContent-Length: 0\r\n\r\n"
	served := &readsOnConn{Conn: proxy, readAgain: make(chan struct{}, 1), afterWrite: make(chan struct{}, 1)}
	done := runServe(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte(request), served, nil,
			&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
	})
	if got := servedBytes(t, served, app, done); string(got) != want {
		t.Fatalf("served %q, want %q", got, want)
	}
	// Nothing more comes while the app holds the connection.
	if err := app.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, err := app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("after the answer read %d byte(s) (%v), want nothing until the app's own timeout", n, err)
	}
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("decodeHTTP returned %v once the app closed, want nil", err)
		}
	case <-time.After(time.Minute): // the bound only turns a hang into a failure
		t.Fatal("decodeHTTP did not return once the app closed the connection")
	}
	_ = proxy.Close()
	boundAppReads(t, app)
	if tail, _ := io.ReadAll(app); len(tail) != 0 {
		t.Fatalf("served %q after the answer, want nothing until the close", tail)
	}
}
