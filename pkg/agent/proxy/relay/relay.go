package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// Relay forwards bytes between a real client and real destination
// net.Conn, teeing timestamped copies to parser-facing FakeConns and
// honouring control directives from the parser.
//
// Construct with [New]; start with [Run]. Relay does not own the
// lifecycle of the real sockets: callers close src/dst at connection
// end. Run returns when both forwarders have stopped (either EOF,
// an error, or ctx cancellation).
//
// A Relay is single-shot — allocate a new one per accepted connection.
type Relay struct {
	cfg Config

	// src is the real client-side socket. Held behind an atomic
	// pointer so the directive processor can swap it for a TLS-
	// wrapped version without synchronising on the forwarder's hot
	// path. The forwarder reloads on every iteration.
	src atomic.Pointer[net.Conn]
	// dst is the real destination-side socket; same treatment as src.
	dst atomic.Pointer[net.Conn]

	// teeC2D handles chunks flowing client → destination (Dir=FromClient).
	teeC2D *tee
	// teeD2C handles chunks flowing destination → client (Dir=FromDest).
	teeD2C *tee

	// clientStream is the FakeConn parsers read to consume
	// client-produced bytes.
	clientStream *fakeconn.FakeConn
	// destStream is the FakeConn parsers read to consume
	// destination-produced bytes.
	destStream *fakeconn.FakeConn

	// directives is the parser-writable directive channel. The
	// processor goroutine reads from here.
	directives chan directive.Directive
	// acks is the parser-readable ack channel.
	acks chan directive.Ack

	// pauseMu protects pauseCh. pauseCh is non-nil while forwarders
	// are paused (directive processor mid-TLS-upgrade). Forwarders
	// snapshot pauseCh at the top of each loop iteration and, if
	// non-nil, block on it until it is closed.
	pauseMu sync.Mutex
	pauseCh chan struct{}

	// seqC2D and seqD2C are the per-direction monotonic Chunk sequence
	// numbers, scoped to this connection.
	seqC2D atomic.Uint64
	seqD2C atomic.Uint64

	// runOnce ensures Run's one-time startup path executes exactly once.
	runOnce sync.Once
	// runErr stores the error returned from the first Run. Relay is
	// single-shot; subsequent Run calls return this cached error via
	// ErrRelayAlreadyRun.
	runErr atomic.Pointer[error]
}

// ErrRelayAlreadyRun is returned from a second [Relay.Run] call.
var ErrRelayAlreadyRun = errors.New("relay: Run called more than once")

// ErrNoTLSUpgrader is wrapped in the Ack.Err when a KindUpgradeTLS
// directive arrives but no [Config.TLSUpgradeFn] is configured.
var ErrNoTLSUpgrader = errors.New("relay: no TLSUpgradeFn configured for KindUpgradeTLS directive")

// New returns a Relay bound to the given real sockets. Ownership of
// src and dst is shared with the relay for the duration of Run: the
// relay Reads and Writes them but does NOT Close them. Callers close
// on their own schedule.
//
// src is the real CLIENT-side socket (the incoming TCP connection
// from the user's app); dst is the real DESTINATION-side socket (the
// outbound TCP connection keploy opened to the upstream service).
// The direction convention follows [fakeconn.Direction]:
//
//	bytes read from src  → Chunk{Dir: FromClient} → written to dst
//	bytes read from dst  → Chunk{Dir: FromDest}   → written to src
func New(cfg Config, src, dst net.Conn) *Relay {
	cfg = cfg.withDefaults()

	r := &Relay{
		cfg:        cfg,
		directives: make(chan directive.Directive, 8),
		acks:       make(chan directive.Ack, 8),
	}
	r.src.Store(&src)
	r.dst.Store(&dst)

	r.teeC2D = newTee(
		fakeconn.FromClient,
		cfg.PerConnCap,
		cfg.TeeChanBuf,
		cfg.MemoryGuardCheck,
		cfg.OnMarkMockIncomplete,
		cfg.Logger,
	)
	r.teeD2C = newTee(
		fakeconn.FromDest,
		cfg.PerConnCap,
		cfg.TeeChanBuf,
		cfg.MemoryGuardCheck,
		cfg.OnMarkMockIncomplete,
		cfg.Logger,
	)

	var localAddr, remoteAddr net.Addr
	if src != nil {
		localAddr = src.LocalAddr()
		remoteAddr = src.RemoteAddr()
	}
	r.clientStream = fakeconn.New(r.teeC2D.readCh(), localAddr, remoteAddr)
	var destLocal, destRemote net.Addr
	if dst != nil {
		destLocal = dst.LocalAddr()
		destRemote = dst.RemoteAddr()
	}
	r.destStream = fakeconn.New(r.teeD2C.readCh(), destLocal, destRemote)

	return r
}

// ClientStream returns the parser-facing FakeConn populated with
// chunks read from the real client (bytes flowing client → dest).
// Safe to call before or after Run starts.
func (r *Relay) ClientStream() *fakeconn.FakeConn { return r.clientStream }

// DestStream returns the parser-facing FakeConn populated with chunks
// read from the real destination (bytes flowing dest → client). Safe
// to call before or after Run starts.
func (r *Relay) DestStream() *fakeconn.FakeConn { return r.destStream }

// Directives returns the parser-writable directive channel. Parsers
// send [directive.Directive] values here; the relay's directive
// processor goroutine handles them.
func (r *Relay) Directives() chan<- directive.Directive { return r.directives }

// Acks returns the parser-readable ack channel. Parsers drain this in
// response to directives they sent.
func (r *Relay) Acks() <-chan directive.Ack { return r.acks }

// DropCounts returns the (client→dest, dest→client) tee drop counts.
// Provided for diagnostics; tests use this to assert that a directive
// or memory-guard pressure actually caused drops.
func (r *Relay) DropCounts() (c2d, d2c uint64) {
	return r.teeC2D.dropCount(), r.teeD2C.dropCount()
}

// Run starts the forwarder, drain, and directive-processor goroutines
// and blocks until both forwarders exit. Exits happen on:
//
//   - EOF or any read error from either real socket,
//   - ctx cancellation (which interrupts in-flight reads via
//     SetReadDeadline on the source socket),
//   - an error in the opposite-direction forwarder that propagates
//     by closing the shared stopping signal.
//
// Run returns the first non-EOF error observed, or nil on clean close.
// Run does NOT close the real sockets; the caller is responsible.
//
// Run is single-shot; calling twice returns [ErrRelayAlreadyRun].
func (r *Relay) Run(ctx context.Context) error {
	var started bool
	r.runOnce.Do(func() {
		started = true
		err := r.run(ctx)
		r.runErr.Store(&err)
	})
	if !started {
		return ErrRelayAlreadyRun
	}
	e := r.runErr.Load()
	if e == nil {
		return nil
	}
	return *e
}

func (r *Relay) run(ctx context.Context) error {
	// Two waitgroups so we can wait on forwarders first (to know
	// it is safe to close tees) and only then on the directive
	// processor. The processor is woken up by closing stopping.
	var wgForward sync.WaitGroup
	var wgDirective sync.WaitGroup
	stopping := make(chan struct{})

	// Interlock ctx-cancel with in-flight Read calls on the real
	// sockets by nudging their read deadlines into the past. This
	// is best effort — net.Conn implementations that don't honour
	// deadlines simply won't unblock until the caller closes the
	// conn after Run returns. net.Pipe, *net.TCPConn, and *tls.Conn
	// all support it.
	cancelNudge := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			r.nudgeDeadline(r.src.Load())
			r.nudgeDeadline(r.dst.Load())
		case <-cancelNudge:
		}
	}()
	defer close(cancelNudge)

	var firstErrMu sync.Mutex
	var firstErr error
	recordErr := func(e error) {
		if e == nil || errors.Is(e, io.EOF) || isBenignNetErr(e) {
			return
		}
		firstErrMu.Lock()
		defer firstErrMu.Unlock()
		if firstErr == nil {
			firstErr = e
		}
	}

	// Directive processor.
	wgDirective.Add(1)
	go func() {
		defer wgDirective.Done()
		r.processDirectives(ctx, stopping)
	}()

	// Forwarder src → dst (Dir=FromClient).
	wgForward.Add(1)
	go func() {
		defer wgForward.Done()
		recordErr(r.forward(ctx, stopping, fakeconn.FromClient, &r.src, &r.dst, r.teeC2D, &r.seqC2D))
	}()

	// Forwarder dst → src (Dir=FromDest).
	wgForward.Add(1)
	go func() {
		defer wgForward.Done()
		recordErr(r.forward(ctx, stopping, fakeconn.FromDest, &r.dst, &r.src, r.teeD2C, &r.seqD2C))
	}()

	// Block until both forwarders exit.
	wgForward.Wait()

	// Now it is safe to stop the tees: no more push() calls will
	// fire. Close staging channels and wait for drain goroutines to
	// flush what they already had buffered.
	r.teeC2D.close()
	r.teeD2C.close()
	r.teeC2D.waitDone()
	r.teeD2C.waitDone()

	// Close the FakeConns so any blocked parser Read returns ErrClosed.
	_ = r.clientStream.Close()
	_ = r.destStream.Close()

	// Signal the directive processor to exit and wait for it.
	close(stopping)
	wgDirective.Wait()

	// After the processor has returned, no one else can send on acks:
	// close it so parsers blocked on <-Acks get the zero value and
	// can return.
	close(r.acks)

	return firstErr
}

// forward is one forwarder goroutine. src is the read side, dst is
// the write side, t is the tee to push chunks into, seq is the
// per-direction sequence counter.
//
// Each iteration:
//  1. Check for pause; block on pauseCh if set.
//  2. Read up to ForwardBuf bytes from src. Stamp readAt = time.Now().
//  3. Write to dst; stamp writtenAt = time.Now() after Write returns.
//  4. Build Chunk and push into the tee (non-blocking).
//  5. Bump activity.
//
// Returns the first read or write error encountered. io.EOF is
// returned verbatim; callers filter it out in their error accounting.
func (r *Relay) forward(
	ctx context.Context,
	stopping <-chan struct{},
	dir fakeconn.Direction,
	srcPtr *atomic.Pointer[net.Conn],
	dstPtr *atomic.Pointer[net.Conn],
	t *tee,
	seq *atomic.Uint64,
) error {
	bufSize := r.cfg.ForwardBuf
	buf := make([]byte, bufSize)
	log := r.cfg.Logger

	for {
		// Early exit if the outer run is tearing down. We check both
		// ctx and stopping; ctx.Done fires on external cancel, stopping
		// fires after run's cleanup.
		select {
		case <-ctx.Done():
			return nil
		case <-stopping:
			return nil
		default:
		}

		// Respect the pause barrier if the directive processor is
		// mid-TLS-upgrade. The barrier is a chan that closes when
		// resumed.
		if pc := r.currentPauseCh(); pc != nil {
			select {
			case <-pc:
			case <-ctx.Done():
				return nil
			case <-stopping:
				return nil
			}
		}

		src := *srcPtr.Load()
		n, err := src.Read(buf)
		readAt := time.Now()
		if n > 0 {
			// Copy into an owned slice so the forwarder can reuse
			// the scratch buffer on the next iteration without the
			// parser observing a torn read. This copy is unavoidable
			// given the chunk has to outlive the Read buffer.
			payload := make([]byte, n)
			copy(payload, buf[:n])

			dst := *dstPtr.Load()
			wn, werr := dst.Write(payload)
			writtenAt := time.Now()

			// Tee regardless of Write outcome: the bytes were
			// observed, the parser gets to see them. If Write failed
			// the mock is still incomplete because the real peer did
			// not receive them, so flag it.
			chunk := fakeconn.Chunk{
				Dir:       dir,
				Bytes:     payload,
				ReadAt:    readAt,
				WrittenAt: writtenAt,
				SeqNo:     seq.Add(1),
			}
			t.push(chunk)

			if werr != nil {
				// The write failure means the opposite peer has
				// gone. Flag and return: the other forwarder will
				// see EOF/error too.
				if log != nil {
					log.Debug("relay: forward write error",
						zap.String("dir", dir.String()),
						zap.Int("read_bytes", n),
						zap.Int("written_bytes", wn),
						zap.Error(werr),
					)
				}
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("write_error")
				}
				return werr
			}
			if wn != n {
				// Short write on a blocking Write is a net.Conn
				// contract violation, but handle it anyway.
				short := errors.New("relay: short write on destination socket")
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("short_write")
				}
				return short
			}

			r.cfg.BumpActivity()
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return err
		}
	}
}

// currentPauseCh snapshots the current pause channel under the mutex.
// Returns nil if no pause is active. The forwarder uses the result to
// decide whether to block.
func (r *Relay) currentPauseCh() chan struct{} {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	return r.pauseCh
}

// beginPause installs a pause barrier. Subsequent [Relay.currentPauseCh]
// returns the new channel until [Relay.endPause] is called. Calling
// beginPause while a pause is already active returns the existing
// channel so concurrent directives don't clobber each other; this
// shouldn't happen in practice because the directive processor is
// single-threaded.
func (r *Relay) beginPause() chan struct{} {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	if r.pauseCh == nil {
		r.pauseCh = make(chan struct{})
	}
	return r.pauseCh
}

// endPause releases the pause barrier. No-op if no pause is active.
func (r *Relay) endPause() {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	if r.pauseCh != nil {
		close(r.pauseCh)
		r.pauseCh = nil
	}
}

// nudgeDeadline sets a deadline in the past on the conn if non-nil.
// Errors are swallowed because (a) not every net.Conn honours
// deadlines, and (b) the conn may already be in teardown.
func (r *Relay) nudgeDeadline(c *net.Conn) {
	if c == nil || *c == nil {
		return
	}
	_ = (*c).SetReadDeadline(time.Unix(1, 0))
}

// isBenignNetErr returns true for errors that are expected during
// normal connection teardown and should not be surfaced as the relay's
// return value. That includes io.ErrClosedPipe, the "io: read/write
// on closed pipe" text produced by net.Pipe, the "use of closed
// network connection" text produced by the stdlib net package's
// unexported errors, and net.Error with Timeout()==true produced by
// our own ctx-cancel nudge.
func isBenignNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	// Timeout() == true means ctx was cancelled and we nudged the
	// read deadline; the caller is doing teardown, not reporting a
	// genuine protocol error.
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	// String fallback for errors that predate net.ErrClosed (and for
	// net.Pipe's io.ErrClosedPipe-equivalent message).
	msg := err.Error()
	if strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "io: read/write on closed pipe") {
		return true
	}
	return false
}
