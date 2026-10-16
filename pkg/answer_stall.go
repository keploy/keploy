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
// user set no apiTimeout of their own; design v14 rule 15, row W1). The app
// takes no byte of the request and sends no byte back for this long of the
// time keploy's own process could run (utils/stall's runnable clock), and the
// wait ends, the test not judged, naming what it waited for. A starved host
// accrues it only as fast as keploy gets to run, so load never ends a live
// exchange early. A user whose app takes longer by design sets a bound of
// their own (--api-timeout), which replaces this rule.
//
// A var only so that a test can shorten it.
var AnswerStallGrace = 60 * time.Second

// ErrAnswerStalled is a test's exchange that the stall rule ended before the
// app's answer began (no response headers): the app never answered, or never
// took the whole request. There is nothing to compare, so the test's row is
// built as a test with no answer is (FAILED), marked not judged
// (models.FailureInfo.AnswerStalled), and its set is not judged. A stream the
// rule ends after its answer began is an ErrStreamHeldOpen instead, judged on
// what came.
var ErrAnswerStalled = errors.New("the app's answer stalled")

// AnswerStallError says what the stall rule waited for when it ended a test's
// exchange: the request it waited on (Target, "GET /orders" or a gRPC
// method), how long of keploy's runnable time (Waited), how much of the
// request the app took (Sent of Request, and Untaken: not all of it), and,
// for a stream, what of the recorded answer came (Came). It is an ErrAnswerStalled before the answer began
// (Answered false), and an ErrStreamHeldOpen after.
type AnswerStallError struct {
	Target string
	Waited time.Duration
	// Sent of Request: the request's body (gRPC: its messages) keploy had
	// handed to the connection at the cut, of all of it.
	Sent     int64
	Request  int64
	Untaken  bool
	Answered bool
	Read     int64
	// Stream: a streamed answer the comparison read up to the cut, judged
	// on what came; Came says what of the recorded answer that was.
	Stream bool
	Came   string
}

func (e *AnswerStallError) Error() string {
	waited := fmt.Sprintf("waited %s of keploy's runnable time for %s", e.Waited, e.Target)
	switch {
	case !e.Answered && e.Untaken:
		took := fmt.Sprintf("at most %d bytes of it", e.Sent)
		if e.Request > 0 {
			took = fmt.Sprintf("at most %d of its %d bytes; %d were never taken", e.Sent, e.Request, max(e.Request-e.Sent, 0))
		}
		return fmt.Sprintf("the app stopped taking the test's request and never answered (it took %s); %s", took, waited)
	case !e.Answered:
		return "the app never answered; " + waited
	case e.Stream:
		came := ""
		if e.Came != "" {
			came = " (" + e.Came + ")"
		}
		return fmt.Sprintf("the app kept the stream open and sent nothing more%s; %s", came, waited)
	default:
		return fmt.Sprintf("the app's answer stopped part-way, after %d bytes; %s", e.Read, waited)
	}
}

// Is makes the error an ErrStreamHeldOpen for a stream the comparison read
// up to the cut (Stream), and an ErrAnswerStalled otherwise: no answer, or
// one that stopped part-way with nothing compared.
func (e *AnswerStallError) Is(target error) bool {
	if e.Stream {
		return target == ErrStreamHeldOpen
	}
	return target == ErrAnswerStalled
}

// asStream is e for a streamed answer the comparison read up to the cut,
// saying what of its recorded answer came.
func (e *AnswerStallError) asStream(came string) *AnswerStallError {
	out := *e
	out.Stream, out.Came = true, came
	return &out
}

// streamCut is err, a stream's read the stall rule cut, as the stream's
// stall: what of its recorded answer came (came of recorded frames). Any
// other err is itself.
func streamCut(err error, came, recorded int, unit string) error {
	var st *AnswerStallError
	if !errors.As(err, &st) {
		return err
	}
	if came >= recorded {
		return st.asStream(fmt.Sprintf("every recorded %s came, and the stream did not end", unit))
	}
	return st.asStream(fmt.Sprintf("%d of the %d recorded %ss came", came, recorded, unit))
}

// answerWatch is the stall rule on one test's exchange with the app
// (AnswerStallGrace). Every byte the app sends back, and every byte of the
// request it takes, is progress; a write keploy is still waiting on is not.
// When the app has made none for the grace, the watch cuts the exchange
// (cut: the connections closed, a gRPC stream cancelled) and says so
// (stalled). nil without the switch or with a bound of the user's: nothing
// is watched.
type answerWatch struct {
	meter *stall.Meter

	mu      sync.Mutex
	cuts    []func()
	conns   []net.Conn
	target  string
	request int64

	stalled  atomic.Bool
	answered atomic.Bool
	writing  atomic.Int32
	read     atomic.Int64
	// body is how much of the request's body (gRPC: its messages) keploy
	// has handed to the connection: the app took at most that.
	body atomic.Int64
	// at the cut: whether keploy was still writing the request, and how
	// much of it the app had taken (the cut ends the write).
	cutWriting bool
	cutSent    int64

	done chan struct{}
	once sync.Once
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
	w := &answerWatch{meter: stall.NewMeter(cfg.StallClock, grace), done: make(chan struct{})}
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
		if !w.meter.Expired() {
			continue
		}
		select {
		case <-w.done:
			return
		default:
		}
		w.mu.Lock()
		w.cutWriting, w.cutSent = w.writing.Load() > 0, w.body.Load()
		cuts, conns := w.cuts, w.conns
		w.mu.Unlock()
		w.stalled.Store(true)
		for _, cut := range cuts {
			cut()
		}
		for _, c := range conns {
			_ = c.Close()
		}
		return
	}
}

// describe names the exchange: target is its request ("GET /orders", a gRPC
// method), request the request's size in bytes (0 when unknown).
func (w *answerWatch) describe(target string, request int64) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.target, w.request = target, max(request, 0)
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

// wrap watches c, a connection of the exchange: the cut closes it.
func (w *answerWatch) wrap(c net.Conn) net.Conn {
	if w == nil {
		return c
	}
	w.mu.Lock()
	w.conns = append(w.conns, c)
	w.mu.Unlock()
	return &answerConn{Conn: c, w: w}
}

// dial is the dial of an HTTP transport for the exchange: each connection
// it dials is watched (wrap). nil without a watch: the transport's own.
func (w *answerWatch) dial() func(ctx context.Context, network, addr string) (net.Conn, error) {
	if w == nil {
		return nil
	}
	var d net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return w.wrap(c), nil
	}
}

// answering says the app's answer began (its response headers came).
func (w *answerWatch) answering() {
	if w != nil {
		w.answered.Store(true)
	}
}

// stop ends the watch: the exchange is done, by its end or by the cut.
func (w *answerWatch) stop() {
	if w == nil {
		return
	}
	w.once.Do(func() { close(w.done) })
}

// didStall reports whether the stall rule cut the exchange.
func (w *answerWatch) didStall() bool {
	return w != nil && w.stalled.Load()
}

// err is what the stall rule waited for, once it cut the exchange.
func (w *answerWatch) err() *AnswerStallError {
	w.mu.Lock()
	target, request, writing, sent := w.target, w.request, w.cutWriting, w.cutSent
	w.mu.Unlock()
	// The request was not all taken: keploy still waited on a write of it,
	// or had not handed all of it to the connection (a gRPC request held
	// back by the app's flow-control window, which it opens only as it
	// reads).
	writing = writing || request > 0 && sent < request
	return &AnswerStallError{
		Target:   target,
		Waited:   w.meter.Grace(),
		Sent:     sent,
		Request:  request,
		Untaken:  writing,
		Answered: w.answered.Load(),
		Read:     w.read.Load(),
	}
}

// answerConn is a connection of a watched exchange: what passes on it is
// progress.
type answerConn struct {
	net.Conn
	w *answerWatch
}

func (c *answerConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.w.read.Add(int64(n))
		c.w.meter.Reset()
	}
	return n, err
}

func (c *answerConn) Write(p []byte) (int, error) {
	c.w.writing.Add(1)
	n, err := c.Conn.Write(p)
	c.w.writing.Add(-1)
	if n > 0 {
		c.w.meter.Reset()
	}
	return n, err
}

// NetConn is the watched connection, for code that reaches through wrappers.
func (c *answerConn) NetConn() net.Conn { return c.Conn }

// answerReader is a watched exchange's streamed answer: its read, once the
// stall rule cut the exchange, ends with what the rule waited for, and its
// Close ends the watch.
type answerReader struct {
	io.ReadCloser
	w *answerWatch
}

func (r *answerReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && r.w.didStall() {
		err = r.w.err()
	}
	return n, err
}

func (r *answerReader) Close() error {
	r.w.stop()
	return r.ReadCloser.Close()
}

// sentBody counts n bytes of the request's body handed to the connection.
func (w *answerWatch) sentBody(n int) {
	if w != nil && n > 0 {
		w.body.Add(int64(n))
	}
}

// countBody is a request body whose reads the watch counts: what the
// transport took of it to write.
type countBody struct {
	io.ReadCloser
	w *answerWatch
}

func (b *countBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.w.sentBody(n)
	return n, err
}

// countRequestBody makes req's body counted by w (sentBody), as sent and as
// sent again (GetBody).
func (w *answerWatch) countRequestBody(req *http.Request) {
	if w == nil || req == nil || req.Body == nil || req.Body == http.NoBody {
		return
	}
	req.Body = &countBody{ReadCloser: req.Body, w: w}
	if get := req.GetBody; get != nil {
		req.GetBody = func() (io.ReadCloser, error) {
			rc, err := get()
			if err != nil || rc == nil || rc == http.NoBody {
				return rc, err
			}
			return &countBody{ReadCloser: rc, w: w}, nil
		}
	}
}
