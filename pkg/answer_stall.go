package pkg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/stall"
)

// AnswerStallGrace is the stall rule on a test's exchange with the app under
// the proven switch when no clock bounds the app's answer (Unbounded: the
// user set no apiTimeout of their own; design v14 rule 15, row W1). Keploy
// waits on the app this long of the time keploy's own process could run
// (utils/stall's runnable clock) with the app making no progress, taking no
// more of the request and sending no more of its answer (answerWatch), and
// the wait ends, the test not judged, naming what it waited for. A starved
// host accrues it only as fast as keploy gets to run, so load never ends a
// live exchange early. A user whose app takes longer by design sets a bound
// of their own (--api-timeout), which replaces this rule.
//
// A var only so that a test can shorten it.
var AnswerStallGrace = 60 * time.Second

// ErrAnswerStalled is a test's exchange that ended with nothing of the app's
// answer to compare: the stall rule ended it before the answer began (no
// response headers), or before any of its body came (a stream with no frame,
// gRPC: no message), or the app's keepalives showed it stalled with nothing
// of its answer sent (row W1's keepalive rules). The app never answered, or
// never took the whole request. The test's row is built as a test with no
// answer is (FAILED), marked not judged (models.FailureInfo.AnswerStalled),
// and its set is not judged: a reader of the status alone never sees a pass.
// A stream the rule ends after some of its body came is an ErrStreamHeldOpen
// instead, judged on what came.
var ErrAnswerStalled = errors.New("the app's answer stalled")

// AnswerStallError says what ended a test's exchange with the app: the
// request it waited on (Target, "GET /orders" or a gRPC method), how long of
// keploy's runnable time (Waited), how much of the request the app took
// (Sent of Request, and Untaken: not all of it), whether the answer began
// (Answered: its headers came) and how much of its body came (Read). Stream
// says a streamed answer the comparison read up to the cut, and Came what of
// its recording that was. Keepalives says the app's keepalives ended it (row
// W1), not the stall rule's grace.
//
// It is an ErrStreamHeldOpen when Stream is set, and an ErrAnswerStalled
// otherwise (Is).
type AnswerStallError struct {
	Target string
	Waited time.Duration
	// Sent of Request: the request's body (gRPC: its messages) keploy had
	// handed on at the cut, of all of it (0 when not known). The app took
	// at most that.
	Sent     int64
	Request  int64
	Untaken  bool
	Answered bool
	// Read is how many bytes of the answer's body (gRPC: of its messages)
	// came.
	Read int64
	// Stream: a streamed answer the comparison read up to the cut, judged
	// on what came; Came says what of the recorded answer that was.
	Stream bool
	Came   string
	// Keepalives: the app sent keepalives and nothing of its answer, and
	// the keepalive rule ended the wait (row W1), not the grace.
	// KeepalivesInBody: they were body bytes (an SSE comment, an NDJSON
	// blank line), not frames beside it (a gRPC PING).
	Keepalives       bool
	KeepalivesInBody bool
}

func (e *AnswerStallError) Error() string {
	var what string
	switch {
	case !e.Answered && e.Untaken:
		took := fmt.Sprintf("at most %d bytes of it", e.Sent)
		if e.Request > 0 {
			took = fmt.Sprintf("at most %d of its %d bytes; %d were never taken", e.Sent, e.Request, max(e.Request-e.Sent, 0))
		}
		what = fmt.Sprintf("the app stopped taking the test's request and never answered (it took %s)", took)
	case !e.Answered:
		what = "the app never answered"
	case e.Stream:
		came := ""
		if e.Came != "" {
			came = " (" + e.Came + ")"
		}
		what = "the app kept the stream open and sent nothing more" + came
	case e.Read == 0:
		what = "the app sent its answer's headers and nothing of its body"
	default:
		what = fmt.Sprintf("the app's answer stopped part-way, after %d bytes of its body", e.Read)
	}
	if e.Keepalives {
		switch {
		case e.KeepalivesInBody && e.Answered && e.Read == 0:
			what = "the app sent its answer's headers and nothing of its body but keepalives"
		case e.Answered:
			what += ", then only keepalives"
		default:
			what += ", and sent only keepalives"
		}
		if e.Target != "" {
			what += ", for " + e.Target
		}
		return what
	}
	waited := fmt.Sprintf("waited %s of keploy's runnable time", e.Waited)
	if e.Target != "" {
		waited += " for " + e.Target
	}
	return what + "; " + waited
}

// Is makes the error an ErrStreamHeldOpen for a stream the comparison read
// up to the cut (Stream), and an ErrAnswerStalled otherwise: nothing of the
// answer to compare.
func (e *AnswerStallError) Is(target error) bool {
	if e.Stream {
		return target == ErrStreamHeldOpen
	}
	return target == ErrAnswerStalled
}

// AnswerBegan reports whether err is an ErrAnswerStalled after the app's
// answer began: its headers (status, gRPC metadata) came, and nothing of its
// body. Those are judged against the recording first: a difference in them
// is a judged failure, and only a start that matches fails closed, not
// judged.
func AnswerBegan(err error) bool {
	var st *AnswerStallError
	return errors.As(err, &st) && !st.Stream && st.Answered
}

// asStream is e for a streamed answer the comparison read up to the cut,
// saying what of its recorded answer came.
func (e *AnswerStallError) asStream(came string) *AnswerStallError {
	out := *e
	out.Stream, out.Came = true, came
	return &out
}

// streamCut is err, a stream's read the stall rule cut, as the stream's
// stall: what of its recorded answer came (came of recorded frames). A
// stream none of whose frames came has nothing to judge, and stays an
// ErrAnswerStalled: its test fails closed, not judged (the coordinator's
// ruling (a)), never a pass on the recording's fill. Any other err is
// itself.
func streamCut(err error, came, recorded int, unit string) error {
	var st *AnswerStallError
	if !errors.As(err, &st) {
		return err
	}
	if came == 0 {
		return st
	}
	if came >= recorded {
		return st.asStream(fmt.Sprintf("every recorded %s came, and the stream did not end", unit))
	}
	return st.asStream(fmt.Sprintf("%d of the %d recorded %ss came", came, recorded, unit))
}

// keptOpenWithNothing is the error of a stream the app's keepalives ended
// (row W1) before any of its frames (gRPC: messages) came: nothing of the
// answer to compare, so the test fails closed, not judged (ErrAnswerStalled),
// never a pass on the recording. read is how many bytes of its body came.
func keptOpenWithNothing(target string, answered bool, read int64) *AnswerStallError {
	return &AnswerStallError{Target: target, Answered: answered, Read: read, Keepalives: true}
}

// keptOpenWithKeepalivesOnly is keptOpenWithNothing for an HTTP stream whose
// keepalives are body bytes (an SSE comment, an NDJSON blank line).
func keptOpenWithKeepalivesOnly() *AnswerStallError {
	return &AnswerStallError{Answered: true, Keepalives: true, KeepalivesInBody: true}
}

// answerWatch is the stall rule on one test's exchange with the app
// (AnswerStallGrace). It measures how long keploy waited on the app with no
// progress: the time accrues only while keploy waits on the app (waiting:
// for the answer's headers, a read of its body, a gRPC call), so a stream
// keploy is slow to read is never the app's stall. Progress is what the app
// does for the exchange, never a keepalive:
//   - HTTP: a byte of the request's body the transport took to write (it
//     takes the next only once the app made room for it), the answer's
//     headers, a byte of its body;
//   - gRPC: the app's HEADERS, CONTINUATION, DATA, WINDOW_UPDATE,
//     RST_STREAM and GOAWAY frames, and the request's DATA keploy wrote
//     (pingWatch), never a PING, its ACK or a SETTINGS frame.
//
// When keploy waited the grace with none, the watch cuts the exchange (cut:
// the HTTP request's context cancelled, the gRPC stream cancelled and its
// connection closed) and says so (stalled). nil without the switch or with a
// bound of the user's: nothing is watched.
type answerWatch struct {
	meter *stall.Meter

	mu      sync.Mutex
	cuts    []func()
	conns   []net.Conn
	target  string
	request int64
	// hasBody: the request has a body (gRPC: messages) to hand on.
	hasBody bool
	// waits is how many waits on the app are in flight (waiting).
	waits int

	stalled  atomic.Bool
	answered atomic.Bool
	read     atomic.Int64
	// body is how much of the request's body (gRPC: its messages) keploy
	// has handed on: the app took at most that. bodyDone: all of it.
	body     atomic.Int64
	bodyDone atomic.Bool
	// at the cut: how much of the request was handed on, and whether not all
	// of it.
	cutSent    int64
	cutUntaken bool

	release func()
	done    chan struct{}
	once    sync.Once
}

// newAnswerWatch starts the stall rule on an exchange with the app (describe
// names it). nil when the switch is off or the user's own bound applies
// (cfg.Unbounded false).
func newAnswerWatch(cfg SimulationConfig) *answerWatch {
	if !cfg.Unbounded || !provenswitch.On() {
		return nil
	}
	grace := cfg.StallGrace
	if grace <= 0 {
		grace = AnswerStallGrace
	}
	w := &answerWatch{meter: stall.NewMeter(cfg.StallClock, grace), release: func() {}, done: make(chan struct{})}
	go w.run()
	return w
}

func (w *answerWatch) run() {
	defer w.meter.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-w.meter.Wait():
		}
		w.mu.Lock()
		if w.waits == 0 || !w.meter.Expired() {
			// Keploy is not waiting on the app (the time does not
			// accrue: waiting starts the stall again), or the grace has
			// not passed.
			w.mu.Unlock()
			continue
		}
		select {
		case <-w.done:
			w.mu.Unlock()
			return
		default:
		}
		w.cutSent = w.body.Load()
		w.cutUntaken = w.hasBody && !w.bodyDone.Load() && (w.request <= 0 || w.cutSent < w.request)
		w.stalled.Store(true)
		cuts, conns := w.cuts, w.conns
		w.mu.Unlock()
		for _, cut := range cuts {
			cut()
		}
		for _, c := range conns {
			_ = c.Close()
		}
		return
	}
}

// waiting says keploy waits on the app from now until the returned func is
// called: the stall accrues only then, and starts again when a wait begins
// after none. A no-op without a watch.
func (w *answerWatch) waiting() func() {
	if w == nil {
		return func() {}
	}
	w.mu.Lock()
	w.waits++
	if w.waits == 1 {
		w.meter.Reset()
	}
	w.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			w.waits--
			w.mu.Unlock()
		})
	}
}

// progress says the app made progress on the exchange: the stall starts
// again.
func (w *answerWatch) progress() {
	if w != nil {
		w.meter.Reset()
	}
}

// gotAnswer counts n bytes of the answer's body (gRPC: of its messages)
// come: progress.
func (w *answerWatch) gotAnswer(n int) {
	if w != nil && n > 0 {
		w.read.Add(int64(n))
		w.meter.Reset()
	}
}

// describe names the exchange: target is its request ("GET /orders", a gRPC
// method), request the request's size in bytes (0 when unknown; one of more
// than 0 has a body).
func (w *answerWatch) describe(target string, request int64) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.target, w.request = target, max(request, 0)
	w.hasBody = w.hasBody || request > 0
	w.mu.Unlock()
}

// onCut adds what cutting the exchange does (a gRPC stream's cancel).
func (w *answerWatch) onCut(cut func()) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.cuts = append(w.cuts, cut)
	w.mu.Unlock()
}

// closeOnCut makes the cut close c, a connection of the exchange; one added
// after the cut is closed at once.
func (w *answerWatch) closeOnCut(c net.Conn) {
	if w == nil {
		return
	}
	w.mu.Lock()
	cut := w.stalled.Load()
	if !cut {
		w.conns = append(w.conns, c)
	}
	w.mu.Unlock()
	if cut {
		_ = c.Close()
	}
}

// bind is ctx for the exchange's HTTP request: the cut cancels it, which
// ends the request where it waits (its dial, its write, the wait for its
// headers, its body's read), a connection dialled after the cut included;
// stop releases it. ctx itself without a watch.
func (w *answerWatch) bind(ctx context.Context) context.Context {
	if w == nil {
		return ctx
	}
	ctx, cancel := context.WithCancel(ctx)
	w.onCut(cancel)
	w.mu.Lock()
	w.release = cancel
	w.mu.Unlock()
	return ctx
}

// answering says the app's answer began (its response headers came):
// progress.
func (w *answerWatch) answering() {
	if w != nil {
		w.answered.Store(true)
		w.meter.Reset()
	}
}

// stop ends the watch: the exchange is done, by its end or by the cut. The
// HTTP request's context (bind) is released.
func (w *answerWatch) stop() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		close(w.done)
		w.mu.Lock()
		release := w.release
		w.mu.Unlock()
		release()
	})
}

// didStall reports whether the stall rule cut the exchange.
func (w *answerWatch) didStall() bool {
	return w != nil && w.stalled.Load()
}

// err is what the stall rule waited for, once it cut the exchange.
func (w *answerWatch) err() *AnswerStallError {
	w.mu.Lock()
	target, request, sent, untaken := w.target, w.request, w.cutSent, w.cutUntaken
	w.mu.Unlock()
	return &AnswerStallError{
		Target:   target,
		Waited:   w.meter.Grace(),
		Sent:     sent,
		Request:  request,
		Untaken:  untaken,
		Answered: w.answered.Load(),
		Read:     w.read.Load(),
	}
}

// answerBody is a watched exchange's answer body as the transport hands it
// on: each byte of it is progress, and counted (gotAnswer).
type answerBody struct {
	io.ReadCloser
	w *answerWatch
}

func (b *answerBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.w.gotAnswer(n)
	return n, err
}

// answerReader is a watched exchange's streamed answer as the comparison
// reads it: each read is a wait on the app (waiting), a read the stall rule
// cut ends with what the rule waited for, and its end, or its Close, ends
// the watch. A
// clean end (io.EOF) stays one: the app ended the stream. The cut's own
// error is never io.EOF (net/http answers a read of a request whose context
// was cancelled with the cancellation).
type answerReader struct {
	io.ReadCloser
	w *answerWatch
}

func (r *answerReader) Read(p []byte) (int, error) {
	done := r.w.waiting()
	n, err := r.ReadCloser.Read(p)
	done()
	if err != nil && err != io.EOF && r.w.didStall() {
		err = r.w.err()
	}
	if err != nil {
		// The stream ended (its end, or its cut): nothing more is waited
		// on, so the watch ends, even for a reader never closed.
		r.w.stop()
	}
	return n, err
}

func (r *answerReader) Close() error {
	r.w.stop()
	return r.ReadCloser.Close()
}

// sentBody counts n bytes of the request's body handed on.
func (w *answerWatch) sentBody(n int) {
	if w != nil && n > 0 {
		w.body.Add(int64(n))
	}
}

// requestDone says the whole request was handed on (gRPC: its END_STREAM).
func (w *answerWatch) requestDone() {
	if w != nil {
		w.bodyDone.Store(true)
	}
}

// countBody is a request body whose reads the watch counts: what the
// transport took of it to write, progress (the transport takes the next
// piece only once the app made room for the last).
type countBody struct {
	io.ReadCloser
	w *answerWatch
}

func (b *countBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.w.sentBody(n)
		b.w.progress()
	}
	if err == io.EOF {
		b.w.requestDone()
	}
	return n, err
}

// countRequestBody makes req's body counted by w (sentBody), as sent and as
// sent again (GetBody, which starts the count again).
func (w *answerWatch) countRequestBody(req *http.Request) {
	if w == nil || req == nil || req.Body == nil || req.Body == http.NoBody {
		return
	}
	w.mu.Lock()
	w.hasBody = true
	w.mu.Unlock()
	req.Body = &countBody{ReadCloser: req.Body, w: w}
	if get := req.GetBody; get != nil {
		req.GetBody = func() (io.ReadCloser, error) {
			rc, err := get()
			if err != nil || rc == nil || rc == http.NoBody {
				return rc, err
			}
			w.body.Store(0)
			w.bodyDone.Store(false)
			return &countBody{ReadCloser: rc, w: w}, nil
		}
	}
}
