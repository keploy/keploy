package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The set's recorded windows the rows below place answers among: test-1 at
// [0s,10s], test-2 at [20s,30s], test-3 at [40s,50s], test-4 at [60s,70s].
var releaseT0 = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func releaseAt(s int) time.Time { return releaseT0.Add(time.Duration(s) * time.Second) }

func releaseWindows() *models.WindowSchedule {
	var ws []models.TestWindow
	for i := 0; i < 4; i++ {
		ws = append(ws, models.TestWindow{TestCase: "test-" + strconv.Itoa(i+1), Start: releaseAt(20 * i), End: releaseAt(20*i + 10)})
	}
	return models.NewWindowSchedule(ws)
}

// standInRelease stands in for the release enterprise registers: a moment
// inside a window is due once that window opened, one in the gap after a
// test once that test ended, one before every window at once, and every one
// at the set's end.
func standInRelease(p models.RunPlace, t time.Time) bool {
	if p.SetEnd || p.Windows.Len() == 0 {
		return true
	}
	i := p.Windows.ReleaseIndex(t)
	if i < 0 {
		return true
	}
	anchor := models.RunOpen(i)
	if !p.Windows.Contains(t) {
		anchor = models.RunEnd(i)
	}
	return anchor <= p.Reached
}

func registerStandIn(t *testing.T) {
	t.Helper()
	t.Cleanup(models.RegisterAnswerRelease(models.HTTP, standInRelease))
}

// placedDb is a store that tells the run's place (integrations.RunPlacer),
// moved by the row.
type placedDb struct {
	*mockMemDb
	mu    sync.Mutex
	place models.RunPlace
	sig   chan struct{}
	// waits are the idle marks' releases of the waits on the next move.
	waits []func()
}

func newPlacedDb(mocks ...*models.Mock) *placedDb {
	return &placedDb{mockMemDb: &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}, place: models.RunPlace{Windows: releaseWindows(), Reached: models.RunOpen(0), Staging: 1}}
}

func (d *placedDb) RunPlace() models.RunPlace {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.place
}

func (d *placedDb) RunPlaceChanged() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sig == nil {
		d.sig = make(chan struct{})
	}
	return d.sig
}

func (d *placedDb) IdleUntilRunPlaceChanges(changed <-chan struct{}, mark func() func()) (func(), bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if changed == nil || changed != d.sig {
		return func() {}, false
	}
	var once sync.Once
	unmark := mark()
	release := func() { once.Do(unmark) }
	d.waits = append(d.waits, release)
	return release, true
}

// move moves the place, and releases the waits' idle marks before it
// returns, as the agent's place does.
func (d *placedDb) move(f func(p *models.RunPlace)) {
	d.mu.Lock()
	f(&d.place)
	if d.sig != nil {
		close(d.sig)
		d.sig = nil
	}
	waits := d.waits
	d.waits = nil
	d.mu.Unlock()
	for _, w := range waits {
		w()
	}
}

func (d *placedDb) reach(anchor int) { d.move(func(p *models.RunPlace) { p.Reached = anchor }) }

// idleCount is a connection's request holder that counts its idle marks
// (integrations.IdleMarker).
type idleCount struct{ marked atomic.Int32 }

func (c *idleCount) Hold() func() { return func() {} }
func (c *idleCount) MarkIdle() func() {
	c.marked.Add(1)
	var once sync.Once
	return func() { once.Do(func() { c.marked.Add(-1) }) }
}

const releaseCall = "GET /job HTTP/1.1\r\nHost: api.example\r\n\r\n"

// heldMock is a GET /job recording called at req, its answer resp (raw).
func heldMock(t *testing.T, req time.Time, resp string, res time.Time) *models.Mock {
	t.Helper()
	h := &HTTP{Logger: zap.NewNop()}
	m, err := h.buildHTTPMock(&FinalHTTP{Req: []byte(releaseCall), Resp: []byte(resp), ReqTimestampMock: req, ResTimestampMock: res}, 80, "c", models.OutgoingOptions{})
	if err != nil || m == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", m, err)
	}
	m.Name = "job"
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

func singleAnswer(t *testing.T, res time.Time) *models.Mock {
	return heldMock(t, releaseAt(5), "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 16\r\n\r\n{\"state\":\"done\"}", res)
}

// sseMock is an SSE answer in three writes: its head and first event at 5s
// (test-1), its second at at2, its third at at3.
func sseMock(t *testing.T, at2, at3 time.Time, encoding string) *models.Mock {
	body := "data:a\n\ndata:b\n\ndata:c\n\n"
	// Recorded plain, then given its encoding: the body is held decoded.
	m := heldMock(t, releaseAt(5), "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 24\r\n\r\n"+body, at3)
	m.Spec.HTTPResp.Body = body
	if encoding != "" {
		m.Spec.HTTPResp.Header["Content-Encoding"] = encoding
	}
	m.Spec.HTTPResp.Marks = &models.HTTPAnswerMarks{Head: releaseAt(5), Writes: []models.HTTPAnswerWrite{{Offset: 8, TS: releaseAt(5)}, {Offset: 16, TS: at2}, {Offset: 24, TS: at3}}}
	return m
}

// serveHeld runs decodeHTTP on the call over db and returns the app's end.
func serveHeld(t *testing.T, db integrations.MockMemDb, holder integrations.RequestHolder) (net.Conn, <-chan error) {
	t.Helper()
	return serveHeldCall(t, db, holder, releaseCall)
}

// serveHeldCall is serveHeld for the application's request call.
func serveHeldCall(t *testing.T, db integrations.MockMemDb, holder integrations.RequestHolder, call string) (net.Conn, <-chan error) {
	t.Helper()
	app, proxy := tcpPair(t)
	h := &HTTP{Logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = integrations.WithRequestHolder(ctx, holder)
	returned := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		returned <- h.decodeHTTP(ctx, []byte(call), proxy, nil, db, models.OutgoingOptions{})
	}()
	t.Cleanup(func() { cancel(); _ = app.Close(); <-finished })
	return app, returned
}

const quiet = 150 * time.Millisecond

// Row 1: a single answer recorded in test-3 for a call made in test-1 is not
// written while test-1 or test-2 runs, and is written whole, as before, once
// test-3's window opens. Red at main: written at once.
func TestHeldAnswer_ASingleAnswerIsWrittenAtItsWindowsOpen(t *testing.T) {
	registerStandIn(t)
	db := newPlacedDb(singleAnswer(t, releaseAt(45)))
	app, _ := serveHeld(t, db, nil)
	if got := readFor(app, quiet); got != "" {
		t.Fatalf("written while test-1 runs: %q", got)
	}
	db.reach(models.RunOpen(1))
	if got := readFor(app, quiet); got != "" {
		t.Fatalf("written while test-2 runs: %q", got)
	}
	db.reach(models.RunOpen(2))
	got := readUntil(app, `{"state":"done"}`, 5*time.Second)
	if !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.Contains(got, "Content-Length: 16\r\n") || !strings.HasSuffix(got, `{"state":"done"}`) {
		t.Fatalf("at test-3's open: %q, want the whole recorded answer", got)
	}
}

// Row 2: an answer recorded in the gap after test-2 is written at test-2's
// end, not at its open.
func TestHeldAnswer_AGapAnswerIsWrittenAtTheTestsEnd(t *testing.T) {
	registerStandIn(t)
	db := newPlacedDb(singleAnswer(t, releaseAt(35)))
	db.reach(models.RunOpen(1))
	app, _ := serveHeld(t, db, nil)
	if got := readFor(app, quiet); got != "" {
		t.Fatalf("written at test-2's open: %q", got)
	}
	db.reach(models.RunEnd(1))
	if got := readUntil(app, `{"state":"done"}`, 5*time.Second); !strings.HasSuffix(got, `{"state":"done"}`) {
		t.Fatalf("at test-2's end: %q", got)
	}
}

// dechunk reads a chunked body's chunks off r, in order.
func dechunk(t *testing.T, r *bufio.Reader, n int) [][]byte {
	t.Helper()
	var chunks [][]byte
	for i := 0; i < n; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("chunk %d size: %v", i, err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil {
			t.Fatalf("chunk %d size %q: %v", i, line, err)
		}
		data := make([]byte, size+2)
		if _, err := io.ReadFull(r, data); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		chunks = append(chunks, data[:size])
	}
	return chunks
}

// Row 3: an SSE stream whose events came in test-1, test-2 and test-3 has its
// head and first event written at once, chunked, its second at test-2's
// open, and its third and the last chunk at test-3's open; plain and gzip.
func TestHeldAnswer_AStreamsEventsAreWrittenEachAtItsWindow(t *testing.T) {
	for _, encoding := range []string{"", "gzip"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			registerStandIn(t)
			db := newPlacedDb(sseMock(t, releaseAt(25), releaseAt(45), encoding))
			holder := &idleCount{}
			app, _ := serveHeld(t, db, holder)
			r := bufio.NewReader(app)
			_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
			resp, err := http.ReadResponse(r, nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.ContentLength != -1 || len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
				t.Fatalf("head: length %d, transfer %v; want chunked", resp.ContentLength, resp.TransferEncoding)
			}
			first := dechunk(t, r, 1)[0]
			var gz *gzip.Reader
			var stream bytes.Buffer
			plain := func(b []byte) string {
				if encoding == "" {
					return string(b)
				}
				stream.Write(b)
				if gz == nil {
					if gz, err = gzip.NewReader(&stream); err != nil {
						t.Fatal(err)
					}
				}
				out := make([]byte, 8)
				if _, err := io.ReadFull(gz, out); err != nil {
					t.Fatalf("a flushed part does not decode on its own: %v", err)
				}
				return string(out)
			}
			if got := plain(first); got != "data:a\n\n" {
				t.Fatalf("first part %q", got)
			}
			if got := readFor(app, quiet); got != "" || r.Buffered() > 0 {
				t.Fatalf("written while test-1 runs: %q", got)
			}
			if holder.marked.Load() != 1 {
				t.Fatalf("idle marks while held = %d, want 1", holder.marked.Load())
			}
			db.reach(models.RunOpen(1))
			_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
			if got := plain(dechunk(t, r, 1)[0]); got != "data:b\n\n" {
				t.Fatalf("second part %q", got)
			}
			db.reach(models.RunOpen(2))
			_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
			if got := plain(dechunk(t, r, 1)[0]); got != "data:c\n\n" {
				t.Fatalf("third part %q", got)
			}
			if line, _ := r.ReadString('\n'); line != "0\r\n" {
				t.Fatalf("last chunk %q", line)
			}
			if encoding != "" {
				if rest, err := io.ReadAll(gz); err != nil || len(rest) != 0 {
					t.Fatalf("gzip end: %q, %v", rest, err)
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for holder.marked.Load() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if holder.marked.Load() != 0 {
				t.Fatal("still marked idle once written")
			}
		})
	}
}

// Row 4: a skipped test-2 is passed when test-3 opens: its event and test-3's
// go out together.
func TestHeldAnswer_ASkippedTestsPartIsWrittenAtTheNextOpen(t *testing.T) {
	registerStandIn(t)
	db := newPlacedDb(sseMock(t, releaseAt(25), releaseAt(45), ""))
	app, _ := serveHeld(t, db, nil)
	if got := readUntil(app, "data:a", 5*time.Second); !strings.Contains(got, "data:a") || strings.Contains(got, "data:b") {
		t.Fatalf("at once: %q", got)
	}
	db.reach(models.RunOpen(2))
	if got := readUntil(app, "0\r\n\r\n", 5*time.Second); !strings.Contains(got, "data:b") || !strings.HasSuffix(got, "data:c\n\n\r\n0\r\n\r\n") {
		t.Fatalf("at test-3's open: %q", got)
	}
}

// Row 5 (in part): the set's end, and a new set's staging, write every part
// still held.
func TestHeldAnswer_TheSetsEndAndANewStagingWriteTheRest(t *testing.T) {
	for name, end := range map[string]func(p *models.RunPlace){
		"set end":     func(p *models.RunPlace) { p.SetEnd = true },
		"new staging": func(p *models.RunPlace) { p.Staging++; p.Reached = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			registerStandIn(t)
			db := newPlacedDb(sseMock(t, releaseAt(25), releaseAt(75), ""))
			app, _ := serveHeld(t, db, nil)
			if got := readUntil(app, "data:a", 5*time.Second); strings.Contains(got, "data:b") {
				t.Fatalf("at once: %q", got)
			}
			db.move(end)
			if got := readUntil(app, "0\r\n\r\n", 5*time.Second); !strings.HasSuffix(got, "data:c\n\n\r\n0\r\n\r\n") {
				t.Fatalf("after %s: %q", name, got)
			}
		})
	}
}

// Row 7: a stream recorded before the marks is written at once; row 11: so is
// an answer of a session recording, one served again, one over a store that
// tells no place, and every answer when no release is registered.
func TestHeldAnswer_WrittenAtOnce(t *testing.T) {
	old := heldMock(t, releaseAt(5), "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 8\r\n\r\ndata:a\n\n", releaseAt(45))
	session := singleAnswer(t, releaseAt(45))
	session.TestModeInfo.Lifetime = models.LifetimeSession
	for _, tc := range []struct {
		name     string
		db       func() integrations.MockMemDb
		register bool
	}{
		{"a stream with no marks", func() integrations.MockMemDb { return newPlacedDb(old) }, true},
		{"a session recording", func() integrations.MockMemDb { return newPlacedDb(session) }, true},
		{"a store that tells no place", func() integrations.MockMemDb {
			return &mockMemDb{mocks: []*models.Mock{singleAnswer(t, releaseAt(45))}, updateUnFilteredReturn: true}
		}, true},
		{"no release registered", func() integrations.MockMemDb { return newPlacedDb(singleAnswer(t, releaseAt(45))) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.register {
				registerStandIn(t)
			}
			app, _ := serveHeld(t, tc.db(), nil)
			if got := readFor(app, time.Second); !strings.HasPrefix(got, "HTTP/1.1 200") {
				t.Fatalf("not written at once: %q", got)
			}
		})
	}
}

// Row 11 (repeat): an answer served again is never held.
func TestHeldAnswer_ARepeatIsNotHeld(t *testing.T) {
	registerStandIn(t)
	h := &HTTP{Logger: zap.NewNop()}
	db := newPlacedDb()
	if a, err := h.heldAnswerOf(singleAnswer(t, releaseAt(45)), releaseRequest(t, releaseCall), db, true); err != nil || a != nil {
		t.Fatalf("repeat: held %v, %v", a, err)
	}
	if a, err := h.heldAnswerOf(singleAnswer(t, releaseAt(45)), releaseRequest(t, releaseCall), db, false); err != nil || a == nil {
		t.Fatalf("first serve: held %v, %v; want held", a, err)
	}
}

// Row 12: the application closes its connection while the answer is held:
// the rest is dropped, and the connection ends with no error.
func TestHeldAnswer_TheAppClosingDropsTheRest(t *testing.T) {
	registerStandIn(t)
	db := newPlacedDb(sseMock(t, releaseAt(25), releaseAt(45), ""))
	app, returned := serveHeld(t, db, nil)
	if got := readUntil(app, "data:a", 5*time.Second); !strings.Contains(got, "data:a") {
		t.Fatalf("at once: %q", got)
	}
	_ = app.Close()
	time.Sleep(50 * time.Millisecond)
	db.reach(models.RunOpen(1))
	time.Sleep(50 * time.Millisecond)
	db.reach(models.RunOpen(2))
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("decodeHTTP = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("decodeHTTP did not end after the app closed")
	}
}

// Marks that do not fit the body served are read as none.
func TestHeldAnswer_MarksThatDoNotFitAreNone(t *testing.T) {
	for _, tc := range []struct {
		writes []models.HTTPAnswerWrite
		n      int
		fit    bool
	}{
		{[]models.HTTPAnswerWrite{{Offset: 4}, {Offset: 8}}, 8, true},
		{[]models.HTTPAnswerWrite{{Offset: 4}, {Offset: 4}, {Offset: 8}}, 8, true},
		{nil, 0, false},
		{[]models.HTTPAnswerWrite{{Offset: 4}, {Offset: 7}}, 8, false},
		{[]models.HTTPAnswerWrite{{Offset: 5}, {Offset: 4}, {Offset: 8}}, 8, false},
		{[]models.HTTPAnswerWrite{{Offset: 4}, {Offset: 9}}, 8, false},
	} {
		if got := marksFit(&models.HTTPAnswerMarks{Writes: tc.writes}, tc.n); got != tc.fit {
			t.Errorf("marksFit(%v, %d) = %v, want %v", tc.writes, tc.n, got, tc.fit)
		}
	}
}

// releaseRequest is call as decodeHTTP reads the application's request.
func releaseRequest(t *testing.T, call string) *http.Request {
	t.Helper()
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(call)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The application's call, not the recorded one, decides whether a held
// stream goes out chunked: its client reads the answer. An HTTP/1.0 call
// takes it whole, with its length, at its last frame's place; an HTTP/1.1
// call takes it frame by frame, chunked, though the recorded call was
// HTTP/1.0.
func TestHeldAnswer_TheCallsProtocolDecidesChunking(t *testing.T) {
	registerStandIn(t)
	t.Run("an HTTP/1.0 call", func(t *testing.T) {
		db := newPlacedDb(sseMock(t, releaseAt(25), releaseAt(45), ""))
		app, _ := serveHeldCall(t, db, nil, "GET /job HTTP/1.0\r\nHost: api.example\r\n\r\n")
		if got := readFor(app, quiet); got != "" {
			t.Fatalf("written while test-1 runs: %q", got)
		}
		db.reach(models.RunOpen(1))
		if got := readFor(app, quiet); got != "" {
			t.Fatalf("written at test-2's open, before its last frame's place: %q", got)
		}
		db.reach(models.RunOpen(2))
		_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil || len(resp.TransferEncoding) != 0 || resp.ContentLength != 24 || string(body) != "data:a\n\ndata:b\n\ndata:c\n\n" {
			t.Fatalf("answer: transfer %v, length %d, body %q, %v; want the whole body with its length", resp.TransferEncoding, resp.ContentLength, body, err)
		}
	})
	t.Run("an HTTP/1.1 call recorded as HTTP/1.0", func(t *testing.T) {
		m := sseMock(t, releaseAt(25), releaseAt(45), "")
		m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, 0
		db := newPlacedDb(m)
		app, _ := serveHeld(t, db, nil)
		r := bufio.NewReader(app)
		_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
			t.Fatalf("head: transfer %v; want chunked", resp.TransferEncoding)
		}
		if got := string(dechunk(t, r, 1)[0]); got != "data:a\n\n" {
			t.Fatalf("first part %q", got)
		}
		if got := readFor(app, quiet); got != "" || r.Buffered() > 0 {
			t.Fatalf("written while test-1 runs: %q", got)
		}
	})
}

// A mock with no recorded answer is never held: heldAnswerOf leaves its serve
// as before, and does not panic.
func TestHeldAnswer_AMockWithNoAnswerIsNotHeld(t *testing.T) {
	registerStandIn(t)
	h := &HTTP{Logger: zap.NewNop()}
	m := singleAnswer(t, releaseAt(45))
	m.Spec.HTTPResp = nil
	a, err := func() (a *heldAnswer, err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("heldAnswerOf panicked on a mock with no answer: %v", r)
			}
		}()
		return h.heldAnswerOf(m, releaseRequest(t, releaseCall), newPlacedDb(), false)
	}()
	if err != nil || a != nil {
		t.Fatalf("held %v, %v; want nil, nil", a, err)
	}
}

// An answer with no body by rule (to a HEAD, a 204) is never chunked, whatever
// marks it carries: it is written whole, with its recorded head, at its last
// mark.
func TestHeldAnswer_ANoBodyAnswerIsWrittenWhole(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	for _, tc := range []struct {
		name   string
		method models.Method
		status int
	}{
		{"to a HEAD", http.MethodHead, http.StatusOK},
		{"a 204", http.MethodGet, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := singleAnswer(t, releaseAt(45))
			m.Spec.HTTPReq.Method = tc.method
			m.Spec.HTTPResp.StatusCode = tc.status
			m.Spec.HTTPResp.Body = ""
			m.Spec.HTTPResp.Marks = &models.HTTPAnswerMarks{Head: releaseAt(5), Writes: []models.HTTPAnswerWrite{{Offset: 0, TS: releaseAt(45)}}}
			a, err := h.planHeld(m, releaseRequest(t, releaseCall))
			if err != nil || a == nil {
				t.Fatalf("planHeld = %v, %v; want the answer held whole", a, err)
			}
			if a.chunked || len(a.pieces) != 0 || !a.headAt.Equal(releaseAt(45)) || bytes.Contains(bytes.ToLower(a.head), []byte("transfer-encoding")) {
				t.Fatalf("held chunked %v, %d pieces, at %v, head %q; want it whole at %v", a.chunked, len(a.pieces), a.headAt, a.head, releaseAt(45))
			}
		})
	}
}

// An answer whose head and every part are already due when it is called is
// not held: it is served as before, whole.
func TestHeldAnswer_AnAnswerDueAtItsCallIsNotHeld(t *testing.T) {
	registerStandIn(t)
	h := &HTTP{Logger: zap.NewNop()}
	db := newPlacedDb()
	db.reach(models.RunOpen(2))
	if a, err := h.heldAnswerOf(sseMock(t, releaseAt(25), releaseAt(45), ""), releaseRequest(t, releaseCall), db, false); err != nil || a != nil {
		t.Fatalf("every part due at the call: held %v, %v; want it served as before", a, err)
	}
}

// A held answer written in parts has the recorded head: each recorded header
// once, no Content-Length, and one Transfer-Encoding, though the recording
// kept one of its own.
func TestHeldAnswer_APartedAnswersHeadIsTheRecordedHead(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	m := sseMock(t, releaseAt(25), releaseAt(45), "")
	m.Spec.HTTPResp.Header["Content-Length"] = "24"
	m.Spec.HTTPResp.Header["Transfer-Encoding"] = "chunked"
	m.Spec.HTTPResp.Header["X-Trace"] = "abc"
	a, err := h.planHeld(m, releaseRequest(t, releaseCall))
	if err != nil || a == nil || !a.chunked {
		t.Fatalf("planHeld = %v, %v; want it written in parts", a, err)
	}
	head := string(a.head)
	for _, line := range []string{"Content-Type: text/event-stream\r\n", "X-Trace: abc\r\n", "Transfer-Encoding: chunked\r\n"} {
		if n := strings.Count(head, line); n != 1 {
			t.Errorf("head %q holds %q %d times, want once", head, line, n)
		}
	}
	if strings.Contains(strings.ToLower(head), "content-length") {
		t.Errorf("head %q of an answer written in parts carries a Content-Length", head)
	}
}

// The head goes out before any part, though a hand-edited recording places it
// after its first part.
func TestHeldAnswer_TheHeadGoesOutBeforeAnyPart(t *testing.T) {
	registerStandIn(t)
	m := sseMock(t, releaseAt(25), releaseAt(45), "")
	m.Spec.HTTPResp.Marks.Head = releaseAt(25)
	db := newPlacedDb(m)
	app, _ := serveHeld(t, db, nil)
	if got := readFor(app, quiet); got != "" {
		t.Fatalf("written before the head's place: %q", got)
	}
	db.reach(models.RunOpen(1))
	if got := readUntil(app, "data:b\n\n", 5*time.Second); !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.Contains(got, "data:b\n\n") {
		t.Fatalf("at the head's place: %q, want the head, then the parts due", got)
	}
}

// Once a held answer is written, its connection answers the application's
// next call.
func TestHeldAnswer_ItsConnectionAnswersTheNextCall(t *testing.T) {
	registerStandIn(t)
	h := &HTTP{Logger: zap.NewNop()}
	const nextCall = "GET /next HTTP/1.1\r\nHost: api.example\r\n\r\n"
	next, err := h.buildHTTPMock(&FinalHTTP{Req: []byte(nextCall), Resp: []byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nnext"),
		ReqTimestampMock: releaseAt(6), ResTimestampMock: releaseAt(6)}, 80, "c", models.OutgoingOptions{})
	if err != nil || next == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", next, err)
	}
	next.Name = "next"
	next.TestModeInfo.Lifetime = models.LifetimePerTest
	db := newPlacedDb(singleAnswer(t, releaseAt(45)), next)
	app, _ := serveHeld(t, db, nil)
	db.reach(models.RunOpen(2))
	if got := readUntil(app, `{"state":"done"}`, 5*time.Second); !strings.HasSuffix(got, `{"state":"done"}`) {
		t.Fatalf("the held answer: %q", got)
	}
	if _, err := app.Write([]byte(nextCall)); err != nil {
		t.Fatal(err)
	}
	if got := readUntil(app, "next", 5*time.Second); !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.HasSuffix(got, "next") {
		t.Fatalf("the next call's answer: %q", got)
	}
}
