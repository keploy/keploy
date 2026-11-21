package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// An exchange known never to be captured gives its sampling slot back as
// that is known, not as its answer ends, and lets go of what its captures
// hold then too: with one sampling slot, a request that comes on another
// connection while it is still in flight is captured, and by then the
// exchange holds nothing for its capture.
// That is known at the request's head (a body over the capture limit), at
// the answer's headers (a stream, a length over the limit), or as the
// answer's capture is cut short, for an answer of unknown length (chunked,
// or ended by the app's close). Held until the answer's end, a stream (an
// SSE feed, a long poll) held the slot for as long as it stayed open, and an
// upload or a download over the capture limit for as long as it took, and
// every request that came meanwhile was passed through uncaptured, missing
// from the recording and from the static-dedup counts. A slot given back
// while the exchange kept what it had captured (up to the capture limit for
// each body) would no longer bound how many such exchanges hold that.
func TestAnExchangeKnownNeverCapturedGivesItsSamplingSlotBackThen(t *testing.T) {
	over := strings.Repeat("x", maxHTTPBodyCaptureBytes+1)
	const firstBytes = "data: one\n\n"
	for _, tc := range []struct {
		name    string
		request string
		// held answers the held exchange: it holds once its first bytes
		// (if any) are sent, until release closes.
		held func(w http.ResponseWriter, r *http.Request, release <-chan struct{})
		// first: what of the answer's body the client reads before the
		// next request is sent, past what shows the exchange is never
		// captured. Empty: that is in the request's head, and the app holds
		// before it answers.
		first string
	}{
		{"a streamed answer", "GET /events HTTP/1.1\r\nHost: app\r\n\r\n",
			func(w http.ResponseWriter, _ *http.Request, release <-chan struct{}) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(firstBytes))
				w.(http.Flusher).Flush()
				<-release
				_, _ = w.Write([]byte("data: two\n\n"))
			}, firstBytes},
		{"an answer over the capture limit", "GET /download HTTP/1.1\r\nHost: app\r\n\r\n",
			func(w http.ResponseWriter, _ *http.Request, release <-chan struct{}) {
				w.Header().Set("Content-Length", strconv.Itoa(len(firstBytes)+len(over)))
				_, _ = w.Write([]byte(firstBytes))
				w.(http.Flusher).Flush()
				<-release
				_, _ = w.Write([]byte(over))
			}, firstBytes},
		{"a chunked answer past the capture limit", "GET /export HTTP/1.1\r\nHost: app\r\n\r\n",
			func(w http.ResponseWriter, _ *http.Request, release <-chan struct{}) {
				_, _ = w.Write([]byte(over))
				w.(http.Flusher).Flush()
				<-release
				_, _ = w.Write([]byte("tail"))
			}, over},
		{"an answer ended by the app's close past the capture limit", "GET /export.csv HTTP/1.1\r\nHost: app\r\n\r\n",
			func(w http.ResponseWriter, _ *http.Request, release <-chan struct{}) {
				conn, bw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/csv\r\nConnection: close\r\n\r\n" + over)
				_ = bw.Flush()
				<-release
				_, _ = bw.WriteString("tail")
				_ = bw.Flush()
			}, over},
		{"a request body over the capture limit", "POST /upload HTTP/1.1\r\nHost: app\r\nContent-Length: " +
			strconv.Itoa(len(over)) + "\r\n\r\n" + over,
			func(w http.ResponseWriter, _ *http.Request, release <-chan struct{}) {
				<-release
				w.Header().Set("Content-Length", "2")
				_, _ = w.Write([]byte("ok"))
			}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			heldIn := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			letGo := func() { releaseOnce.Do(func() { close(release) }) }
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/order" {
					w.Header().Set("Content-Length", "2")
					_, _ = w.Write([]byte("ok"))
					return
				}
				close(heldIn)
				tc.held(w, r, release)
			}))
			t.Cleanup(app.Close)
			appAddr := app.Listener.Addr().String()
			var (
				mu   sync.Mutex
				made []*captureBuffer
			)
			saved := newCaptureBuffer
			t.Cleanup(func() { newCaptureBuffer = saved })
			newCaptureBuffer = func(limit int) *captureBuffer {
				b := saved(limit)
				mu.Lock()
				defer mu.Unlock()
				made = append(made, b)
				return b
			}

			pm, captured := recordingPM(t, 1, &models.IncomingOptions{})
			serve := servedConns(t, pm, letGo)

			held, heldServed := serve.open(t, appAddr, tc.name, tc.request)
			waitFor(t, serve.ctx, heldIn, "the held exchange's request at the app")
			heldReader := bufio.NewReader(held)
			var resp *http.Response
			if tc.first != "" {
				var err error
				if resp, err = http.ReadResponse(heldReader, nil); err != nil {
					t.Fatalf("%s: the client had no answer's headers: %v", tc.name, err)
				}
				first := make([]byte, len(tc.first))
				if n, err := io.ReadFull(resp.Body, first); err != nil || string(first) != tc.first {
					t.Fatalf("%s: the client had %d bytes (%v) of the answer, not the %d it starts with", tc.name, n, err, len(tc.first))
				}
			}
			mu.Lock()
			heldCaptures := append([]*captureBuffer(nil), made...)
			mu.Unlock()
			orderServed := serve.exchange(t, appAddr, "/order", `{"sku":"a","qty":1}`)
			waitFor(t, serve.ctx, orderServed, "/order's handler")
			// The held exchange let go of its captures before it gave its
			// slot back, which /order took, and its handler now waits for
			// the app, so nothing writes them.
			for i, b := range heldCaptures {
				if n := b.held(); n != 0 {
					t.Fatalf("%s: once another request had taken its sampling slot, capture %d of the held exchange (never captured) still held %d bytes, want 0: it gave its slot back but kept what it had captured", tc.name, i+1, n)
				}
			}
			letGo()
			if resp == nil {
				var err error
				if resp, err = http.ReadResponse(heldReader, nil); err != nil {
					t.Fatalf("%s: the client had no answer: %v", tc.name, err)
				}
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatalf("%s: the client did not have the answer whole: %v", tc.name, err)
			}
			_ = held.Close()
			waitFor(t, serve.ctx, heldServed, "the held exchange's handler")
			waitFor(t, serve.ctx, capturesDone(pm), "the captures")

			if got := captured(); !reflect.DeepEqual(got, []string{"/order"}) {
				t.Fatalf("with one sampling slot, a request sent while %s (never captured) was in flight had %v handed to the capture, want [/order]: the exchange held the slot past the point that showed it is never captured", tc.name, got)
			}
		})
	}
}

// A request the recording's filters leave out is never recorded, and nor is
// one whose body is over the capture limit, so a pause of the memory guard
// as it comes costs the recording nothing: on a sampled connection it is not
// counted as not recorded under pressure (the count the recording's summary
// reports as lost), while a request the filters keep, under the same pause,
// is.
func TestUnderAPauseASampledRequestTheFiltersLeaveOutIsNotCountedAsLost(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(app.Close)
	appAddr := app.Listener.Addr().String()
	over := strings.Repeat("x", maxHTTPBodyCaptureBytes+1)

	pm, captured := recordingPM(t, 1, leaveOutProbes())
	stubIngressPaused(t, func() bool { return true })
	mgr := syncMock.New(zap.NewNop())
	serve := servedConns(t, pm, func() {})
	serve.ctx = syncMock.NewContext(serve.ctx, mgr)

	for _, step := range []struct {
		what, request string
		counted       uint64
	}{
		{"a kubelet's probe", probeRequest, 0},
		{"a request whose body is over the capture limit", "POST /upload HTTP/1.1\r\nHost: app\r\nContent-Length: " +
			strconv.Itoa(len(over)) + "\r\n\r\n" + over, 0},
		{"/order", "GET /order HTTP/1.1\r\nHost: app\r\n\r\n", 1},
	} {
		client, served := serve.open(t, appAddr, step.what, step.request)
		_ = answerOf(t, step.what, client)
		waitFor(t, serve.ctx, served, step.what+"'s handler")
		if got := mgr.NotRecordedUnderPressure().Total; got != step.counted {
			t.Fatalf("under a pause, once %s was served on a sampled connection, %d request(s) were counted as not recorded under pressure, want %d: a request the filters leave out, or one over the capture limit, is never recorded, and one they keep is lost to the pause", step.what, got, step.counted)
		}
	}
	waitFor(t, serve.ctx, capturesDone(pm), "the captures")
	if got := captured(); len(got) != 0 {
		t.Fatalf("under a pause, %v was handed to the capture", got)
	}
}

// An answer to a HEAD request has no body, whatever length its headers
// declare: an exchange whose answer declares a length over the capture limit
// is captured, not taken for one cut short.
func TestAHeadAnswerDeclaringALengthOverTheCaptureLimitIsCaptured(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(maxHTTPBodyCaptureBytes+1))
	}))
	t.Cleanup(app.Close)
	pm, captured := recordingPM(t, 1, &models.IncomingOptions{})
	serve := servedConns(t, pm, func() {})
	client, served := serve.open(t, app.Listener.Addr().String(), "HEAD /big", "HEAD /big HTTP/1.1\r\nHost: app\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodHead})
	if err != nil {
		t.Fatalf("HEAD /big: the client had no answer: %v", err)
	}
	if resp.ContentLength != int64(maxHTTPBodyCaptureBytes+1) {
		t.Fatalf("HEAD /big was answered with Content-Length %d, want %d", resp.ContentLength, maxHTTPBodyCaptureBytes+1)
	}
	_ = client.Close()
	waitFor(t, serve.ctx, served, "HEAD /big's handler")
	waitFor(t, serve.ctx, capturesDone(pm), "the captures")
	if got := captured(); !reflect.DeepEqual(got, []string{"/big"}) {
		t.Fatalf("a HEAD exchange whose answer declares a length over the capture limit had %v handed to the capture, want [/big]", got)
	}
}

// What an exchange that is never captured holds for its capture (its
// captures, and a body read whole before it is written, as a PUT's is) is
// let go as that is known, not kept through the answer's last forward to its
// client, which has no deadline: a sampled connection gives its slot back
// then, so the sampling budget no longer bounds the handlers that hold such
// bodies. Known at the answer's headers (a stream, a length over the capture
// limit), it holds nothing as the answer's forward begins. That holds with
// sampling and without it.
func TestAnExchangeNeverCapturedHoldsNoCaptureThroughItsAnswersLastForward(t *testing.T) {
	over := strings.Repeat("x", maxHTTPBodyCaptureBytes+1)
	const body = `{"sku":"a"}`
	request := func(method string) string {
		return method + " /first HTTP/1.1\r\nHost: app\r\nContent-Type: application/json\r\nContent-Length: " +
			strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	chunkedOver := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(over))
		w.(http.Flusher).Flush()
	}
	stream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
	}
	lengthOver := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(over)))
		_, _ = w.Write([]byte(over))
	}
	const nothing = "0 bytes, a reference to the request body kept false"
	for _, tc := range []struct {
		name, request string
		answer        http.HandlerFunc
		// atHeaders: the answer's headers show the exchange is never
		// captured, so it holds nothing as the answer's forward begins.
		atHeaders bool
	}{
		{"a chunked answer over the capture limit", request(http.MethodPost), chunkedOver, false},
		{"a chunked answer over the capture limit, to a PUT", request(http.MethodPut), chunkedOver, false},
		{"a streamed answer", request(http.MethodPost), stream, true},
		{"a streamed answer, to a PUT", request(http.MethodPut), stream, true},
		{"an answer whose length is over the capture limit", request(http.MethodPost), lengthOver, true},
		{"an answer whose length is over the capture limit, to a PUT", request(http.MethodPut), lengthOver, true},
	} {
		for _, sampling := range []int{1, 0} {
			mode := "sampled"
			if sampling == 0 {
				mode = "unsampled"
			}
			t.Run(tc.name+", "+mode, func(t *testing.T) {
				app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					tc.answer(w, r)
				}))
				t.Cleanup(app.Close)
				var (
					mu    sync.Mutex
					holds = map[string]string{}
				)
				saved := captureHolds
				t.Cleanup(func() { captureHolds = saved })
				// Read on the handler's own goroutine.
				captureHolds = func(at string, held int, bodyKept bool) {
					mu.Lock()
					defer mu.Unlock()
					holds[at] = fmt.Sprintf("%d bytes, a reference to the request body kept %v", held, bodyKept)
				}
				pm, captured := recordingPM(t, sampling, &models.IncomingOptions{})
				serve := servedConns(t, pm, func() {})
				client, served := serve.open(t, app.Listener.Addr().String(), tc.name, tc.request)
				_ = answerOf(t, tc.name, client)
				waitFor(t, serve.ctx, served, "the exchange's handler")
				waitFor(t, serve.ctx, capturesDone(pm), "the captures")

				mu.Lock()
				defer mu.Unlock()
				if _, ok := holds["headers"]; !ok || len(holds) != 2 {
					t.Fatalf("%s: what the exchange held was seen at %v, want at its answer's headers and once it was written", tc.name, holds)
				}
				if tc.atHeaders && holds["headers"] != nothing {
					t.Fatalf("%s: as its answer's forward began, the exchange held %s, want %s: what is never captured is let go as that is known", tc.name, holds["headers"], nothing)
				}
				if holds["written"] != nothing {
					t.Fatalf("%s: as its answer's last forward ended, the exchange held %s, want %s: what is never captured is let go as that is known", tc.name, holds["written"], nothing)
				}
				if got := captured(); len(got) != 0 {
					t.Fatalf("%s had %v handed to the capture", tc.name, got)
				}
			})
		}
	}
}

// A request whose capture a pause of the memory guard cuts short is known
// never captured as it is cut, not as its answer ends: a sampled connection
// gives its slot back then, so that with one sampling slot a request sent on
// another connection while the app still holds the cut one is captured, and
// nothing of the cut one's answer is captured.
func TestARequestCutShortByAPauseGivesItsSamplingSlotBackAsItIsCut(t *testing.T) {
	heldIn, bodyIn, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/order" {
			close(heldIn)
			_, _ = io.Copy(io.Discard, r.Body)
			close(bodyIn)
			<-release
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(app.Close)
	appAddr := app.Listener.Addr().String()
	var (
		mu   sync.Mutex
		made int
	)
	saved := newCaptureBuffer
	t.Cleanup(func() { newCaptureBuffer = saved })
	newCaptureBuffer = func(limit int) *captureBuffer {
		mu.Lock()
		made++
		mu.Unlock()
		return saved(limit)
	}
	pm, captured := recordingPM(t, 1, &models.IncomingOptions{})
	var paused atomic.Bool
	stubIngressPaused(t, paused.Load)
	serve := servedConns(t, pm, letGo)

	const body = `{"file":"a"}`
	held, heldServed := serve.open(t, appAddr, "/upload",
		"POST /upload HTTP/1.1\r\nHost: app\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n")
	waitFor(t, serve.ctx, heldIn, "/upload's request at the app")
	paused.Store(true)
	if _, err := held.Write([]byte(body)); err != nil {
		t.Fatalf("/upload: the client could not send its body: %v", err)
	}
	waitFor(t, serve.ctx, bodyIn, "/upload's body at the app")
	paused.Store(false)
	orderServed := serve.exchange(t, appAddr, "/order", `{"sku":"a","qty":1}`)
	waitFor(t, serve.ctx, orderServed, "/order's handler")
	mu.Lock()
	before := made
	mu.Unlock()
	letGo()
	_ = answerOf(t, "/upload", held)
	waitFor(t, serve.ctx, heldServed, "/upload's handler")
	waitFor(t, serve.ctx, capturesDone(pm), "the captures")

	if got := captured(); !reflect.DeepEqual(got, []string{"/order"}) {
		t.Fatalf("with one sampling slot, a request sent while the app held one whose capture a pause had cut short had %v handed to the capture, want [/order]: the cut request held the slot past its cut", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if made != before {
		t.Fatalf("the request a pause had cut short made %d capture(s) of its answer, want none: it is never captured", made-before)
	}
}

// A capture dropped holds nothing from then on, while the tee still counts
// what passes through it, and reads as cut short, so that its exchange is
// not recorded. A nil capture (no body, or capture off) drops nothing.
func TestACaptureDroppedHoldsNothing(t *testing.T) {
	b := newCaptureBuffer(16)
	_, _ = b.Write([]byte("0123456789"))
	b.drop()
	_, _ = b.Write([]byte("abc"))
	if len(b.Bytes()) != 0 || b.buf.Cap() != 0 {
		t.Fatalf("a dropped capture holds %q (capacity %d), want nothing", b.Bytes(), b.buf.Cap())
	}
	if !b.Truncated() || b.LostToPause() {
		t.Fatalf("a dropped capture reads truncated %v, lost to a pause %v; want cut short, not by a pause", b.Truncated(), b.LostToPause())
	}
	if b.Total() != 13 {
		t.Fatalf("a dropped capture counted %d bytes through its tee, want 13", b.Total())
	}
	// A side with no capture (capture off, a request with no body or a
	// pre-buffered one, or an answer whose headers show it is never
	// captured, before any capture of it is made) has a nil one, and the
	// handler drops it all the same: a panic there, on the handler's
	// goroutine, which nothing recovers, would crash the agent. Recovered
	// here, so when this test runs alone it fails by name and not by a
	// crash of the test binary.
	var none *captureBuffer
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("dropping a nil capture panicked (%v), want a no-op", r)
			}
		}()
		none.drop()
	}()
}

// A capture runs its onCut as it is cut short, by its limit however the
// writes cross it, or by a pause, and once: a capture cut short takes no
// more. One within its limit is not cut.
func TestACaptureCutShortRunsItsOnCutOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int
		paused bool
		writes []string
		cuts   int
	}{
		{"a write across the limit", 4, false, []string{"ab", "cdef", "g"}, 1},
		{"a write once the limit is filled", 4, false, []string{"abcd", "e", "f"}, 1},
		{"no room at all", 0, false, []string{"a", "b"}, 1},
		{"a pause", 16, true, []string{"a", "b"}, 1},
		{"writes within the limit", 4, false, []string{"ab", "cd"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubIngressPaused(t, func() bool { return tc.paused })
			b := newCaptureBuffer(tc.limit)
			cuts := 0
			b.onCut = func() { cuts++ }
			for _, w := range tc.writes {
				_, _ = b.Write([]byte(w))
			}
			if cuts != tc.cuts || b.Truncated() != (tc.cuts > 0) {
				t.Fatalf("%s: onCut ran %d time(s), the capture reads cut short %v; want %d, %v", tc.name, cuts, b.Truncated(), tc.cuts, tc.cuts > 0)
			}
		})
	}
}
