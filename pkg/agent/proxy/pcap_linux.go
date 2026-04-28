//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/afpacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"go.uber.org/zap"
)

const (
	pcapSnaplen      = 65535
	pcapPollTimeout  = 300 * time.Millisecond
	pcapBlockTimeout = 200 * time.Millisecond
	pcapFrameSize    = 1 << 11 // 2 KiB
	pcapBlockSize    = 1 << 20 // 1 MiB
	pcapNumBlocks    = 64
)

// pcapBroadcaster fans out every captured frame to its current
// subscribers. Each subscriber owns a private pcapgo.Writer wrapping
// its own io.Writer (typically an http.ResponseWriter wrapped with a
// flush callback) so the subscribers never share Writer state and
// concurrent serialisation is fine. A subscriber that returns an
// error from WritePacket is marked failed and skipped on subsequent
// frames; the agent HTTP handler's defer cleans it up when the
// underlying request context cancels.
type pcapBroadcaster struct {
	mu   sync.RWMutex
	subs []*pcapSubscriber
	next atomic.Int64
}

type pcapSubscriber struct {
	id     int64
	w      *pcapgo.Writer
	flush  func()
	failed atomic.Bool
}

// write fans the (ci, data) pair out to every active subscriber.
// Held under RLock so subscribe/unsubscribe can run concurrently
// without blocking the capture loop, except briefly when the
// subscriber slice itself is being mutated.
func (b *pcapBroadcaster) write(ci gopacket.CaptureInfo, data []byte) {
	b.mu.RLock()
	subs := b.subs
	b.mu.RUnlock()
	for _, s := range subs {
		if s.failed.Load() {
			continue
		}
		if err := s.w.WritePacket(ci, data); err != nil {
			s.failed.Store(true)
			continue
		}
		if s.flush != nil {
			s.flush()
		}
	}
}

// subscribe registers w to receive captured frames as a pcap byte
// stream. The pcap file header is emitted onto w synchronously
// before this returns so the consumer always sees a well-formed
// stream regardless of how soon frames arrive. flush, when non-nil,
// runs after each WritePacket so chunked HTTP responses push bytes
// per-frame rather than holding them in transport buffers.
func (b *pcapBroadcaster) subscribe(w io.Writer, flush func()) (func(), error) {
	pw := pcapgo.NewWriter(w)
	if err := pw.WriteFileHeader(uint32(pcapSnaplen), layers.LinkTypeEthernet); err != nil {
		return nil, fmt.Errorf("write pcap header to subscriber: %w", err)
	}
	if flush != nil {
		flush()
	}
	s := &pcapSubscriber{
		id:    b.next.Add(1),
		w:     pw,
		flush: flush,
	}
	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, sub := range b.subs {
			if sub.id == s.id {
				b.subs = append(b.subs[:i], b.subs[i+1:]...)
				return
			}
		}
	}, nil
}

// startPacketCapture spins up the afpacket capture goroutines on
// every up interface and installs a fresh broadcaster. The outPath
// argument is ignored — kept for the cross-platform signature only;
// the streaming model writes nothing to disk on the agent side.
func (p *Proxy) startPacketCapture(parent context.Context, _ string) error {
	p.stopPacketCapture()

	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("list interfaces: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	broadcaster := &pcapBroadcaster{}

	outPort := uint16(p.Port)
	inPort := p.IncomingProxyPort

	go func() {
		defer close(done)

		var wg sync.WaitGroup
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			wg.Add(1)
			go p.captureOnInterface(ctx, iface.Name, broadcaster, outPort, inPort, &wg)
		}
		wg.Wait()
		p.logger.Info("packet capture stopped",
			zap.Uint16("outgoingProxyPort", outPort),
			zap.Uint16("incomingProxyPort", inPort),
		)
	}()

	p.pcapMu.Lock()
	p.pcapCancel = cancel
	p.pcapDone = done
	p.pcapBroadcaster = broadcaster
	p.pcapMu.Unlock()

	p.logger.Info("packet capture started (streaming mode)",
		zap.Uint16("outgoingProxyPort", outPort),
		zap.Uint16("incomingProxyPort", inPort),
	)
	return nil
}

// stopPacketCapture cancels the in-flight capture loop and waits
// for the goroutines to drain. Subscribers see EOF on their
// streams shortly after this returns (the broadcaster slice goes
// empty and the request handlers exit).
func (p *Proxy) stopPacketCapture() {
	p.pcapMu.Lock()
	cancel := p.pcapCancel
	done := p.pcapDone
	p.pcapCancel = nil
	p.pcapDone = nil
	p.pcapBroadcaster = nil
	p.pcapMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (p *Proxy) captureOnInterface(ctx context.Context, ifaceName string, b *pcapBroadcaster, outPort, inPort uint16, wg *sync.WaitGroup) {
	defer wg.Done()

	tp, err := afpacket.NewTPacket(
		afpacket.OptInterface(ifaceName),
		afpacket.OptFrameSize(pcapFrameSize),
		afpacket.OptBlockSize(pcapBlockSize),
		afpacket.OptNumBlocks(pcapNumBlocks),
		afpacket.OptBlockTimeout(pcapBlockTimeout),
		afpacket.OptPollTimeout(pcapPollTimeout),
		afpacket.OptAddVLANHeader(false),
		afpacket.SocketRaw,
		afpacket.OptTPacketVersion(afpacket.TPacketVersion3),
	)
	if err != nil {
		// Per-interface open failures are routine — interface
		// enumeration includes things like virtual / down / wireguard
		// devices that NewTPacket can refuse, and loopback variants
		// that need extra capabilities. Capture is best-effort across
		// all interfaces, so we keep this at debug; if no interface
		// ever yields packets the resulting empty pcap is itself the
		// aggregate signal.
		p.logger.Debug("packet capture: failed to open interface",
			zap.String("iface", ifaceName), zap.Error(err))
		return
	}
	defer tp.Close()

	for ctx.Err() == nil {
		data, ci, err := tp.ZeroCopyReadPacketData()
		if err != nil {
			if errors.Is(err, afpacket.ErrTimeout) || errors.Is(err, syscall.EAGAIN) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			p.logger.Debug("packet capture read error",
				zap.String("iface", ifaceName), zap.Error(err))
			continue
		}

		pkt := gopacket.NewPacket(data, layers.LinkTypeEthernet, gopacket.NoCopy)
		tl := pkt.TransportLayer()
		if tl == nil {
			continue
		}

		var srcPort, dstPort uint16
		switch t := tl.(type) {
		case *layers.TCP:
			srcPort = uint16(t.SrcPort)
			dstPort = uint16(t.DstPort)
		case *layers.UDP:
			srcPort = uint16(t.SrcPort)
			dstPort = uint16(t.DstPort)
		default:
			continue
		}

		if !portMatches(srcPort, dstPort, outPort, inPort) {
			continue
		}

		if len(data) > pcapSnaplen {
			data = data[:pcapSnaplen]
		}

		// Copy the slice — afpacket reuses the buffer on the next
		// ZeroCopyReadPacketData; the broadcaster may hand it to a
		// subscriber whose pcapgo.Writer queues writes asynchronously.
		buf := make([]byte, len(data))
		copy(buf, data)
		b.write(ci, buf)
	}
}

func portMatches(srcPort, dstPort, outPort, inPort uint16) bool {
	if outPort != 0 && (srcPort == outPort || dstPort == outPort) {
		return true
	}
	if inPort != 0 && (srcPort == inPort || dstPort == inPort) {
		return true
	}
	return false
}
