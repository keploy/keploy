//go:build linux

package linux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/acceptcount"
	"go.keploy.io/server/v3/pkg/models"
)

// The application's side of the connections to the proxy (app_conns_*):
// while counting is on, the kernel numbers each redirected connection of the
// application as its connect() completes, and resolves it, by an event on
// app_conn_events, as paired (the proxy's side of it was counted), lost (it
// closed having sent data, or keploy could not route or keep it, first) or
// empty (it closed having sent nothing). The hooks keep the numbers resolved
// (appSettled, every number up to Settled resolved) and the lost ones (a
// capture loss); the agent waits until every number up to a count it read is
// resolved, so a connection whose connect() returned is never out of sight.

const (
	appConnPaired = 1
	appConnLost   = 2
	// appConnEmpty: closed before its pair landed, having sent nothing: no
	// call was made on it, so it is settled and nothing was lost.
	appConnEmpty = 3
	// appConnEventSize is struct app_conn_event_t's size.
	appConnEventSize = 24
)

// appConnState is the application side's state in the hooks.
type appConnState struct {
	settled acceptcount.Counter
	reader  *ringbuf.Reader
	// stopped: unLoad closed the reader (stopAppConns) and is closing the
	// object, under objectsMutex held for writing.
	stopped bool
	// startErr is why the count could not start (startAppConns), kept so
	// that every later mark fails with it too.
	startErr error
	// lost and lostPort, under the hooks' appMu: the connections resolved
	// as lost, and the port of the latest.
	lost     uint64
	lostPort uint32
}

// startAppConns turns the application's side of the count on: the reader of
// its events first, then the kernel's switch. Called once, by the first
// successful MarkProxyListener, with objectsMutex read-held.
func (h *Hooks) startAppConns() error {
	rd, err := ringbuf.NewReader(h.objects.AppConnEvents)
	if err != nil {
		return fmt.Errorf("read app_conn_events: %w", err)
	}
	h.app.reader = rd
	go h.readAppConns(rd, h.appConnEventsDropped)
	if err := h.objects.AppConnsOn.Update(uint32(0), uint32(1), ebpf.UpdateAny); err != nil {
		_ = rd.Close()
		return fmt.Errorf("turn the application's connection count on: %w", err)
	}
	return nil
}

// readAppConns keeps the resolutions the kernel reports, until the reader is
// closed (the hooks unloading): then every count of them is unknown. After
// each record it reads the kernel's count of the events it had no room for
// (dropped): a resolution that was dropped would leave a wait for its number
// parked for good, so a drop makes the count unknown at once, waking every
// wait with that said. The kernel drops an event when it cannot reserve room
// for it: while the ring is full, a record that was there at the drop is
// still to be read after it, so the drop is seen then. A reserve that fails on
// the ring's lock (6.15 and later) can leave no record to follow; that drop is
// seen at the next record, or at the next read of the counts
// (AppConnsEstablished), which a test's settle makes at every wake-up of its
// watchdog.
func (h *Hooks) readAppConns(rd *ringbuf.Reader, dropped func() (uint64, error)) {
	failed := false
	for {
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				h.app.settled.Fail(fmt.Errorf("%w: the hooks are unloaded", models.ErrProxyConnsUnsupported))
				return
			}
			h.app.settled.Fail(fmt.Errorf("%w: read app_conn_events: %v", models.ErrProxyConnsUnsupported, err))
			return
		}
		if len(rec.RawSample) < appConnEventSize {
			h.app.settled.Fail(fmt.Errorf("%w: an app_conn_events record of %d bytes", models.ErrProxyConnsUnsupported, len(rec.RawSample)))
			return
		}
		seq := binary.LittleEndian.Uint64(rec.RawSample[0:8])
		kind := binary.LittleEndian.Uint32(rec.RawSample[16:20])
		port := binary.LittleEndian.Uint32(rec.RawSample[20:24])
		if kind == appConnLost {
			h.appMu.Lock()
			h.app.lost++
			h.app.lostPort = port
			h.appMu.Unlock()
			h.logger.Debug("an application connection closed before keploy's proxy received it", zap.Uint32("port", port), zap.Uint64("app_seq", seq))
		}
		h.app.settled.Add(seq)
		// Once unknown, the count stays unknown; the reader goes on
		// reading, so the ring drains and the losses stay counted.
		if !failed && !h.appConnDropsNone(dropped) {
			failed = true
		}
	}
}

// appConnDropsNone reads the kernel's count of the application-connection
// events it had no room for. One or more, or a count that cannot be read,
// makes the settled count unknown (failing every wait on it), and is false.
func (h *Hooks) appConnDropsNone(dropped func() (uint64, error)) bool {
	n, err := dropped()
	if err != nil {
		h.app.settled.Fail(fmt.Errorf("%w: read app_conn_events_dropped: %v", models.ErrProxyConnsUnsupported, err))
		return false
	}
	if n > 0 {
		h.app.settled.Fail(fmt.Errorf("%w: the kernel had no room for %d of its application-connection events", models.ErrProxyConnsUnsupported, n))
		return false
	}
	return true
}

// AppConnsEstablished is the count of the application's redirected
// connections whose connect() returned, the highest number given. An event
// the kernel had no room for makes it, and every wait on it, unknown.
func (h *Hooks) AppConnsEstablished(_ context.Context) (uint64, error) {
	if !h.proxyMarked.Load() {
		return 0, fmt.Errorf("%w: no listener of the proxy has been marked for the kernel's count", models.ErrProxyConnsUnsupported)
	}
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	if h.objects.AppConnsEstablished == nil || h.objects.AppConnEventsDropped == nil {
		return 0, errors.New("the hooks are not loaded: app_conns_established is not open")
	}
	var dropped uint64
	if err := h.objects.AppConnEventsDropped.Lookup(uint32(0), &dropped); err != nil {
		return 0, fmt.Errorf("read app_conn_events_dropped: %w", err)
	}
	if dropped > 0 {
		err := fmt.Errorf("%w: the kernel had no room for %d of its application-connection events", models.ErrProxyConnsUnsupported, dropped)
		h.app.settled.Fail(err)
		return 0, err
	}
	var v proxyConnsValue
	if err := h.objects.AppConnsEstablished.LookupWithFlags(uint32(0), &v, ebpf.LookupLock); err != nil {
		return 0, fmt.Errorf("read app_conns_established: %w", err)
	}
	return v.N, nil
}

// AppConnsSettled is the highest n such that every application connection
// numbered 1..n has been resolved (paired, or lost), or why it cannot be told.
func (h *Hooks) AppConnsSettled() (uint64, error) {
	if !h.proxyMarked.Load() {
		return 0, fmt.Errorf("%w: no listener of the proxy has been marked for the kernel's count", models.ErrProxyConnsUnsupported)
	}
	return h.app.settled.Accepted()
}

// WaitAppConnsSettled returns once every application connection numbered
// 1..n is resolved, or with ctx's error, or when the count is unknown. It has
// no deadline of its own: each resolution wakes it.
func (h *Hooks) WaitAppConnsSettled(ctx context.Context, n uint64) error {
	if !h.proxyMarked.Load() {
		return fmt.Errorf("%w: no listener of the proxy has been marked for the kernel's count", models.ErrProxyConnsUnsupported)
	}
	return h.app.settled.WaitAccepted(ctx, n)
}

// CaptureLosses is what the hooks saw the application do that keploy could
// not capture, besides the PID-map refusals: its connections lost before the
// proxy got them (appConnLosses) and its AF_UNIX connects to sockets no member
// of it owns (unixLosses).
func (h *Hooks) CaptureLosses() models.CaptureLosses {
	return append(h.appConnLosses(), h.unixLosses()...)
}

// appConnLosses are the application connections lost before the proxy got
// them (models.CaptureLossAppConnLost), each a call keploy never saw.
func (h *Hooks) appConnLosses() models.CaptureLosses {
	h.appMu.Lock()
	defer h.appMu.Unlock()
	if h.app.lost == 0 {
		return nil
	}
	return models.CaptureLosses{{Kind: models.CaptureLossAppConnLost, Count: h.app.lost, What: fmt.Sprintf("the latest on local port %d", h.app.lostPort)}}
}

// appConnEventsDropped reads app_conn_events_dropped for the reader, under
// objectsMutex: unLoad closes the object's maps under it, right after closing
// the reader, while the reader may be between two records. Once stopped it
// reads nothing: the reader's next Read says the hooks are unloaded.
func (h *Hooks) appConnEventsDropped() (uint64, error) {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	if h.app.stopped {
		return 0, nil
	}
	var n uint64
	err := h.objects.AppConnEventsDropped.Lookup(uint32(0), &n)
	return n, err
}

// stopAppConns closes the events' reader before the object closes. Called by
// unLoad with objectsMutex held.
func (h *Hooks) stopAppConns() {
	h.app.stopped = true
	if h.app.reader != nil {
		_ = h.app.reader.Close()
	}
}
