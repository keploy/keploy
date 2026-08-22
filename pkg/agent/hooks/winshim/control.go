//go:build windows && amd64

package winshim

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// controlTimeout bounds a single shim request end to end.
//
// The shim calls into this from inside the application's own connect(), so a
// stalled handler stalls the app. It must never be the thing that blocks an
// application thread.
const controlTimeout = 3 * time.Second

// pipeInstances is how many pipe instances accept concurrently.
//
// One is not enough: the shim opens a fresh connection per call, and an
// application connecting from several threads at once would otherwise serialise
// on a single instance — inside connect(), where the cost is paid by the
// application. Each acceptor immediately creates its replacement instance after
// a client connects, so this is a floor on concurrency, not a ceiling on total
// connections.
const pipeInstances = 8

// pipeBufSize is the kernel buffer reserved per pipe instance. Requests and
// replies are single short lines, so this is generous.
const pipeBufSize = 1024

// controlDecider is the policy the control server consults. It is implemented by
// Hooks; keeping it behind an interface means the wire handling below can be
// unit-tested without standing up the whole agent.
type controlDecider interface {
	// onHello records that the shim armed inside a process.
	onHello(pid uint32, progName string)
	// onConnect records a redirected egress connection and returns the proxy
	// port the shim should dial instead. A false second return tells the shim to
	// leave the connection alone (bypass rules, or traffic Keploy must never
	// intercept, such as the agent's own control plane).
	onConnect(srcPort uint16, version uint32, destIP string, destPort uint16) (uint16, bool)
	// onBind reports an application bind. A non-zero return moves the app to
	// that port so Keploy can own origPort; zero leaves it in place.
	onBind(pid uint32, origPort uint16) uint16
	// onListen confirms a moved socket really is a server, and is what publishes
	// the ingress event.
	onListen(pid uint32, origPort, movedPort uint16)
}

// controlServer accepts shim connections on a named pipe and dispatches each
// request to the decider.
type controlServer struct {
	logger  *zap.Logger
	decider controlDecider
	name    string

	// stop is signalled to unblock the acceptors, which are otherwise parked in
	// an overlapped ConnectNamedPipe with no deadline.
	//
	// closed mirrors it as a plain flag rather than having stopped() probe the
	// handle: close() may run before serve() has started a single acceptor, and
	// an acceptor that started afterwards would then probe a handle that had
	// already been released. Reading the flag is also what makes it safe to
	// close the handle at all — an acceptor only touches stop after finding the
	// flag clear, and close() does not release the handle until every acceptor
	// has finished.
	stop      windows.Handle
	closed    atomic.Bool
	wg        sync.WaitGroup
	closeOnce sync.Once

	// first is a pipe instance created eagerly by newControlServer and consumed
	// by whichever acceptor starts first. See the comment there.
	firstMu sync.Mutex
	first   windows.Handle
}

// newControlServer prepares the server AND creates the first pipe instance.
//
// The instance is created here rather than left to the acceptors because Load
// returns as soon as this succeeds, and the agent reports itself ready shortly
// after. If the pipe only came into existence once a goroutine happened to be
// scheduled, the agent could be "ready" with nothing listening — and a shim that
// connected in that window would find no pipe, fail to arm, and leave the
// application silently uninstrumented. Creating it synchronously orders pipe
// existence before readiness.
//
// It also turns an unusable pipe name into an immediate, explicit Load failure
// instead of interception that quietly never happens.
func newControlServer(logger *zap.Logger, decider controlDecider, name string) (*controlServer, error) {
	stop, err := windows.CreateEvent(nil, 1 /* manual reset */, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create the shim control stop event: %w", err)
	}
	c := &controlServer{logger: logger, decider: decider, name: name, stop: stop}

	h, err := c.createInstance()
	if err != nil {
		_ = windows.CloseHandle(stop)
		return nil, fmt.Errorf("failed to create the shim control pipe %s: %w", name, err)
	}
	c.first = h
	return c, nil
}

// takeFirst hands the eagerly created instance to the first acceptor that asks.
func (c *controlServer) takeFirst() (windows.Handle, bool) {
	c.firstMu.Lock()
	defer c.firstMu.Unlock()
	if c.first == 0 {
		return 0, false
	}
	h := c.first
	c.first = 0
	return h, true
}

// serve runs the acceptors until ctx is cancelled.
func (c *controlServer) serve(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.close()
		case <-done:
		}
	}()
	defer close(done)

	// One Add for the whole pool, before any acceptor can finish. Adding inside
	// the loop would let the counter fall back to zero mid-spawn when the server
	// is already stopping (every acceptor returns immediately then), and a
	// subsequent Add racing close()'s Wait is the documented WaitGroup misuse
	// panic.
	c.wg.Add(pipeInstances)
	for i := 0; i < pipeInstances; i++ {
		go func() {
			defer c.wg.Done()
			c.accept()
		}()
	}
	c.wg.Wait()
}

// acceptRetryDelay is how long an acceptor waits before retrying after a
// transient failure, so a resource shortage cannot turn into a hot spin across
// the whole pool — which would only deepen the shortage.
const acceptRetryDelay = 50 * time.Millisecond

// accept owns one pipe instance slot: create an instance, wait for a client,
// serve exactly one request, then start over.
//
// It only ever returns on shutdown. An acceptor that gave up on a transient
// error would permanently shrink the pool, and once the pool empties the shim
// can no longer reach the agent at all: every intercepted call falls back to
// bypass and the run finishes green with no mocks and nothing to explain it.
func (c *controlServer) accept() {
	for {
		if c.stopped() {
			return
		}
		h, ok := c.takeFirst()
		if !ok {
			var err error
			if h, err = c.createInstance(); err != nil {
				if c.stopped() {
					return
				}
				c.logger.Debug("failed to create a shim control pipe instance; retrying", zap.Error(err))
				c.pause(acceptRetryDelay)
				continue
			}
		}
		connected, err := c.waitForClient(h)
		if err != nil || !connected {
			_ = windows.CloseHandle(h)
			if c.stopped() {
				return
			}
			if err != nil {
				c.logger.Debug("failed to accept a shim control connection; retrying", zap.Error(err))
				c.pause(acceptRetryDelay)
			}
			continue
		}
		c.handle(h)
	}
}

// pause waits for d, or returns early if the server is shutting down, so a
// backoff can never delay teardown.
func (c *controlServer) pause(d time.Duration) {
	_, _ = windows.WaitForSingleObject(c.stop, uint32(d/time.Millisecond))
}

func (c *controlServer) createInstance() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(c.name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	// The pipe is created without an explicit security descriptor, so it inherits
	// the default: reachable by the creating user. The client, the agent and the
	// application all run as that same user on Windows (the agent is never
	// elevated), and nothing else needs to reach it.
	return windows.CreateNamedPipe(name,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		windows.PIPE_UNLIMITED_INSTANCES, pipeBufSize, pipeBufSize, 0, nil)
}

// waitForClient parks on an overlapped ConnectNamedPipe until a shim connects or
// the server is stopped.
func (c *controlServer) waitForClient(h windows.Handle) (bool, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(ev) }()

	// Heap-allocated explicitly. The kernel writes the completion into this
	// struct asynchronously, so it must not be something the compiler is free to
	// keep only on a goroutine stack (which the runtime may move as it grows).
	ov := &windows.Overlapped{HEvent: ev}
	err = windows.ConnectNamedPipe(h, ov)
	switch err {
	case nil:
		return true, nil
	case windows.ERROR_PIPE_CONNECTED:
		// A client connected between CreateNamedPipe and ConnectNamedPipe.
		return true, nil
	case windows.ERROR_IO_PENDING:
	default:
		return false, err
	}

	// Wait for either a client or shutdown. Without the stop event this parks
	// forever and the agent could not exit.
	ret, err := windows.WaitForMultipleObjects([]windows.Handle{ev, c.stop}, false, windows.INFINITE)
	if err != nil {
		// The connect is still outstanding; it must be drained before the caller
		// closes the handle and this frame goes away.
		cancelAndDrain(h, ov)
		return false, err
	}
	if ret != windows.WAIT_OBJECT_0 {
		// Stopped (or abandoned): cancel the pending connect so the handle can be
		// closed without leaking a kernel I/O request.
		cancelAndDrain(h, ov)
		return false, nil
	}
	var transferred uint32
	if err := windows.GetOverlappedResult(h, ov, &transferred, false); err != nil {
		return false, err
	}
	return true, nil
}

// handle serves exactly one request and closes the instance. Errors are answered
// with BYPASS so the application keeps working.
func (c *controlServer) handle(h windows.Handle) {
	defer func() {
		// Closed, deliberately WITHOUT DisconnectNamedPipe and without
		// FlushFileBuffers.
		//
		// DisconnectNamedPipe discards whatever the client has not read yet,
		// which would throw away the reply we just wrote. FlushFileBuffers is the
		// usual answer to that, but on a server handle it blocks until the client
		// drains the pipe, with no timeout — a shim that wrote a request and then
		// stalled without reading would park this acceptor forever, and since
		// close() waits on the acceptor pool that would wedge agent shutdown
		// permanently. Simply closing the handle keeps the written bytes readable
		// by the client and cannot block.
		_ = windows.CloseHandle(h)
		if r := recover(); r != nil {
			// A panic here would take down the agent and, with it, the app's
			// traffic. Contain it to this one request.
			c.logger.Error("recovered from a panic while serving a shim control request",
				zap.Any("panic", r))
		}
	}()

	line, err := c.readRequest(h)
	if err != nil || line == "" {
		if err != nil {
			c.logger.Debug("failed to read a shim control request", zap.Error(err))
		}
		return
	}

	reply := c.dispatch(line) + "\n"
	if err := c.writeWithTimeout(h, []byte(reply)); err != nil {
		c.logger.Debug("failed to answer a shim control request", zap.Error(err))
	}
}

// readRequest reads one newline-terminated request line.
//
// The pipe is in byte mode, so a single ReadFile is not guaranteed to return a
// whole request: reading once and parsing whatever arrived would, on a
// fragmented request, silently parse a TRUNCATED line. The field-count guards in
// dispatch reject most truncations, but one that lands mid-token parses into a
// valid-looking wrong value — a destination port of 8080 truncated to 8 is still
// a number. Framing on the newline removes that class entirely, and matches how
// the macOS control server reads.
func (c *controlServer) readRequest(h windows.Handle) (string, error) {
	buf := make([]byte, 0, 256)
	chunk := make([]byte, pipeBufSize)
	for len(buf) < pipeBufSize {
		n, err := c.io(h, func(ov *windows.Overlapped) error {
			var read uint32
			return windows.ReadFile(h, chunk, &read, ov)
		})
		if err != nil {
			// Bytes already in hand still make a usable request if the client
			// simply closed after writing a complete line without a newline.
			if len(buf) > 0 {
				return strings.TrimSpace(string(buf)), nil
			}
			return "", err
		}
		if n == 0 {
			break
		}
		buf = append(buf, chunk[:n]...)
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			return strings.TrimSpace(string(buf[:i])), nil
		}
	}
	return strings.TrimSpace(string(buf)), nil
}

// writeWithTimeout performs one overlapped write bounded by controlTimeout.
func (c *controlServer) writeWithTimeout(h windows.Handle, buf []byte) error {
	written, err := c.io(h, func(ov *windows.Overlapped) error {
		var n uint32
		return windows.WriteFile(h, buf, &n, ov)
	})
	if err != nil {
		return err
	}
	if written != len(buf) {
		return fmt.Errorf("short write to the shim control pipe: %d of %d bytes", written, len(buf))
	}
	return nil
}

// io runs one overlapped operation with a deadline, cancelling it on timeout so
// a wedged or vanished shim can never hold a pipe instance forever.
func (c *controlServer) io(h windows.Handle, start func(*windows.Overlapped) error) (int, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(ev) }()

	// Heap-allocated for the same reason as in waitForClient: the kernel writes
	// into it asynchronously.
	ov := &windows.Overlapped{HEvent: ev}

	err = start(ov)
	if err != nil && err != windows.ERROR_IO_PENDING {
		return 0, err
	}
	if err == windows.ERROR_IO_PENDING {
		ret, werr := windows.WaitForSingleObject(ev, uint32(controlTimeout/time.Millisecond))
		if werr != nil {
			// Still outstanding — drain before the caller closes the handle.
			cancelAndDrain(h, ov)
			return 0, werr
		}
		if ret != windows.WAIT_OBJECT_0 {
			cancelAndDrain(h, ov)
			return 0, fmt.Errorf("the shim control request timed out after %s", controlTimeout)
		}
	}
	var transferred uint32
	if err := windows.GetOverlappedResult(h, ov, &transferred, false); err != nil {
		return 0, err
	}
	return int(transferred), nil
}

// cancelAndDrain cancels a pending overlapped operation and waits for the kernel
// to finish with it.
//
// CancelIoEx only REQUESTS cancellation; the I/O is still outstanding when it
// returns. Every OVERLAPPED here lives on a goroutine stack, so returning
// without waiting would leave the kernel free to write a completion into memory
// the stack had moved on from. GetOverlappedResult with wait=true is what makes
// the cancellation synchronous.
// A CancelIoEx failure other than ERROR_NOT_FOUND still falls through to the
// blocking wait. That is deliberate: the alternative is to return while the
// kernel may still write into ov, and a memory-safety hazard is worse than a
// wait. In practice the call cannot fail that way here — the handle is ours and
// the operation was started on it — so the only reachable failure is
// ERROR_NOT_FOUND, which means there is nothing left to wait for.
func cancelAndDrain(h windows.Handle, ov *windows.Overlapped) {
	if err := windows.CancelIoEx(h, ov); err != nil && err == windows.ERROR_NOT_FOUND {
		return
	}
	var transferred uint32
	_ = windows.GetOverlappedResult(h, ov, &transferred, true)
}

// dispatch parses one request line and returns the reply line (without the
// trailing newline).
func (c *controlServer) dispatch(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ReplyBypass
	}
	switch fields[0] {
	case CmdHello:
		return c.dispatchHello(fields)
	case CmdConnect:
		return c.dispatchConnect(fields)
	case CmdBind:
		return c.dispatchBind(fields)
	case CmdListen:
		return c.dispatchListen(fields)
	default:
		c.logger.Debug("ignoring an unrecognised shim control request", zap.String("verb", fields[0]))
		return ReplyBypass
	}
}

func (c *controlServer) dispatchHello(fields []string) string {
	// HELLO <pid> <progname>. progname is best-effort and may be absent.
	if len(fields) < 2 {
		return ReplyBypass
	}
	pid, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return ReplyBypass
	}
	progName := ""
	if len(fields) >= 3 {
		progName = fields[2]
	}
	c.decider.onHello(uint32(pid), progName)
	return ReplyOK
}

func (c *controlServer) dispatchConnect(fields []string) string {
	// CONNECT <srcPort> <ipVersion> <destIP> <destPort>
	if len(fields) != 5 {
		return ReplyBypass
	}
	srcPort, err := parsePort(fields[1])
	if err != nil || srcPort == 0 {
		return ReplyBypass
	}
	version, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil || (version != 4 && version != 6) {
		return ReplyBypass
	}
	destIP := fields[3]
	if net.ParseIP(destIP) == nil {
		return ReplyBypass
	}
	destPort, err := parsePort(fields[4])
	if err != nil || destPort == 0 {
		return ReplyBypass
	}

	proxyPort, ok := c.decider.onConnect(srcPort, uint32(version), destIP, destPort)
	if !ok || proxyPort == 0 {
		return ReplyBypass
	}
	return fmt.Sprintf("%s %d", ReplyOK, proxyPort)
}

func (c *controlServer) dispatchBind(fields []string) string {
	// BIND <pid> <origPort>
	if len(fields) != 3 {
		return ReplyKeep
	}
	pid, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return ReplyKeep
	}
	origPort, err := parsePort(fields[2])
	if err != nil || origPort == 0 {
		return ReplyKeep
	}

	newPort := c.decider.onBind(uint32(pid), origPort)
	if newPort == 0 {
		return ReplyKeep
	}
	return fmt.Sprintf("%s %d", ReplyPort, newPort)
}

func (c *controlServer) dispatchListen(fields []string) string {
	// LISTEN <pid> <origPort> <movedPort>
	if len(fields) != 4 {
		return ReplyOK
	}
	pid, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return ReplyOK
	}
	origPort, err := parsePort(fields[2])
	if err != nil || origPort == 0 {
		return ReplyOK
	}
	movedPort, err := parsePort(fields[3])
	if err != nil || movedPort == 0 {
		return ReplyOK
	}
	c.decider.onListen(uint32(pid), origPort, movedPort)
	return ReplyOK
}

func (c *controlServer) stopped() bool { return c.closed.Load() }

// close signals the acceptors to exit. Safe to call more than once.
func (c *controlServer) close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		_ = windows.SetEvent(c.stop)
		// Wait for the acceptors to finish before releasing the event they wait
		// on, so nothing can be left holding a closed handle.
		c.wg.Wait()
		// A close before serve ever ran leaves the eagerly created instance
		// unconsumed.
		if h, ok := c.takeFirst(); ok {
			_ = windows.CloseHandle(h)
		}
		_ = windows.CloseHandle(c.stop)
	})
}

func parsePort(s string) (uint16, error) {
	v, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint16(v), nil
}
