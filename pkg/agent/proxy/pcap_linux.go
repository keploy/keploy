//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/afpacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.uber.org/zap"
)

// keyLogFileName is the NSS-format key log file written next to the
// pcap so Wireshark can decrypt the captured TLS sessions in-place.
const keyLogFileName = "sslkeys.log"

// syncWriter serialises writes to an underlying io.Writer. Go's
// crypto/tls calls KeyLogWriter.Write from each handshake goroutine,
// so the writer must be safe for concurrent use; *os.File alone is
// not (writes are atomic only up to PIPE_BUF and we want fully
// ordered lines anyway for Wireshark).
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

const (
	pcapSnaplen      = 65535
	pcapPollTimeout  = 300 * time.Millisecond
	pcapBlockTimeout = 200 * time.Millisecond
	pcapFrameSize    = 1 << 11 // 2 KiB
	pcapBlockSize    = 1 << 20 // 1 MiB
	pcapNumBlocks    = 64
)

// startPacketCapture begins capturing packets matching the proxy's
// outgoing and incoming proxy ports on every up interface and writes
// them to outPath. Any in-flight capture from a previous test-set is
// stopped first so each test-set gets its own pcap. Safe to call
// concurrently with stopPacketCapture.
func (p *Proxy) startPacketCapture(parent context.Context, outPath string) error {
	if outPath == "" {
		return errors.New("pcap output path is empty")
	}

	p.stopPacketCapture()

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("create pcap dir: %w", err)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create pcap file %s: %w", outPath, err)
	}

	w := pcapgo.NewWriter(f)
	if err := w.WriteFileHeader(uint32(pcapSnaplen), layers.LinkTypeEthernet); err != nil {
		_ = f.Close()
		return fmt.Errorf("write pcap header: %w", err)
	}

	// Open the NSS key log alongside the pcap. Append rather than
	// truncate so each new TLS session adds to whatever has already
	// been written for this test-set (Record() may be re-entered).
	keyLogPath := filepath.Join(filepath.Dir(outPath), keyLogFileName)
	keyLogF, err := os.OpenFile(keyLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("create keylog file %s: %w", keyLogPath, err)
	}
	pTls.SetKeyLogWriter(&syncWriter{w: keyLogF})

	ifaces, err := net.Interfaces()
	if err != nil {
		pTls.SetKeyLogWriter(nil)
		_ = keyLogF.Close()
		_ = f.Close()
		return fmt.Errorf("list interfaces: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})

	outPort := uint16(p.Port)
	inPort := p.IncomingProxyPort

	go func() {
		defer close(done)
		defer func() {
			if err := f.Close(); err != nil {
				p.logger.Warn("failed to close pcap file", zap.String("outPath", outPath), zap.Error(err))
			}
		}()

		var (
			wg      sync.WaitGroup
			writeMu sync.Mutex
		)

		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			wg.Add(1)
			go p.captureOnInterface(ctx, iface.Name, w, &writeMu, outPort, inPort, &wg)
		}

		wg.Wait()
		p.logger.Info("packet capture stopped",
			zap.String("outPath", outPath),
			zap.Uint16("outgoingProxyPort", outPort),
			zap.Uint16("incomingProxyPort", inPort),
		)
	}()

	p.pcapMu.Lock()
	p.pcapCancel = cancel
	p.pcapDone = done
	p.keyLogFile = keyLogF
	p.pcapMu.Unlock()

	p.logger.Info("packet capture started",
		zap.String("outPath", outPath),
		zap.String("keyLogPath", keyLogPath),
		zap.Uint16("outgoingProxyPort", outPort),
		zap.Uint16("incomingProxyPort", inPort),
	)
	return nil
}

// stopPacketCapture cancels any running capture, waits for the writer
// goroutines to flush, and closes the keylog file. Safe to call when
// no capture is active.
func (p *Proxy) stopPacketCapture() {
	p.pcapMu.Lock()
	cancel := p.pcapCancel
	done := p.pcapDone
	keyLogF := p.keyLogFile
	p.pcapCancel = nil
	p.pcapDone = nil
	p.keyLogFile = nil
	p.pcapMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	// Detach the package-level writer before closing the file so a
	// late TLS handshake cannot write to a closed fd.
	if keyLogF != nil {
		pTls.SetKeyLogWriter(nil)
		if err := keyLogF.Close(); err != nil {
			p.logger.Warn("failed to close keylog file", zap.String("path", keyLogF.Name()), zap.Error(err))
		}
	}
}

func (p *Proxy) captureOnInterface(ctx context.Context, ifaceName string, w *pcapgo.Writer, writeMu *sync.Mutex, outPort, inPort uint16, wg *sync.WaitGroup) {
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
		p.logger.Warn("packet capture: failed to open interface",
			zap.String("iface", ifaceName), zap.Error(err))
		return
	}
	defer tp.Close()

	var matched uint64

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

		writeMu.Lock()
		writeErr := w.WritePacket(ci, data)
		writeMu.Unlock()
		if writeErr != nil {
			p.logger.Warn("pcap write error",
				zap.String("iface", ifaceName), zap.Error(writeErr))
			continue
		}
		atomic.AddUint64(&matched, 1)
	}

	p.logger.Debug("packet capture interface drained",
		zap.String("iface", ifaceName),
		zap.Uint64("matched", atomic.LoadUint64(&matched)))
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
