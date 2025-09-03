//go:build linux

package packet

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"go.uber.org/zap"
)

/*
startAppSideServer starts a TCP server on :16790.
Protocol:
  - For each incoming connection, it will read a request (any bytes) from the client.
  - After a read (or even a zero-length read if client just connects and writes later),
    it pops the next "fromProxy" payload and writes it back as the response.
  - It keeps doing this (read -> respond) in sequence until the feeder is closed.
*/
func startAppSideServer(ctx context.Context, logger *zap.Logger, listenPort int, mgr *FeederManager) (stop func(), err error) {
	listenAddr := fmt.Sprintf(":%d", listenPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	logger.Info("app side server listening", zap.String("addr", listenAddr))

	var wg sync.WaitGroup
	shutdown := make(chan struct{})

	// Cancel the shutdown channel when the context is done
	go func() {
		<-ctx.Done()
		close(shutdown)
	}()

	drainUntilEOF := func(c net.Conn, shutdown <-chan struct{}) {
		buf := make([]byte, 8<<10)
		for {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Minute))
			select {
			case <-shutdown:
				return
			default:
			}
			if _, err := c.Read(buf); err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
		}
	}

	serveConn := func(c net.Conn) {
		tcp, _ := c.(*net.TCPConn)
		defer c.Close()

		// Claim a feeder exclusively.
		srcPort, feeder, err := mgr.Acquire()
		if err != nil {
			logger.Error("server: acquire feeder failed", zap.Error(err))
			return
		}
		logger.Info("server: tcp connection established and feeder acquired", zap.Uint16("srcPort", srcPort))

		// Ensure cleanup when this conn finishes.
		defer func() {
			mgr.Release(srcPort)
			logger.Info("server: released feeder", zap.Uint16("srcPort", srcPort))
		}()

		for {
			select {
			case <-shutdown:
				logger.Debug("server: shutdown received, stopping writes")
				goto afterWrites
			case <-feeder.done:
				logger.Debug("server: feeder done, stopping writes")
				goto afterWrites
			default:
				resp, ok := feeder.pop(shutdown)
				if !ok {
					logger.Debug("server: feeder closed, stopping writes")
					goto afterWrites
				}
				_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if _, err := c.Write(resp); err != nil {
					logger.Error("server: write error", zap.Error(err))
					goto afterWrites
				}
				logger.Info("server: proxy->app wrote response", zap.Int("bytes", len(resp)), zap.Uint16("srcPort", srcPort))
			}
		}

	afterWrites:
		if tcp != nil {
			if err := tcp.CloseWrite(); err != nil {
				logger.Warn("server: CloseWrite error", zap.Error(err))
			} else {
				logger.Info("server: CloseWrite done (sent FIN)")
			}
		}
		drainUntilEOF(c, shutdown)
	}

	acceptLoop := func() {
		for {
			select {
			case <-shutdown:
				logger.Info("server: acceptLoop exiting")
				return
			default:
				conn, err := ln.Accept()
				if err != nil {
					select {
					case <-shutdown:
						logger.Info("server: acceptLoop exiting due to shutdown")
						return
					default:
					}
					logger.Error("server: accept error", zap.Error(err))
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					serveConn(conn)
				}()
			}
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		acceptLoop()
	}()

	stop = func() {
		close(shutdown)
		_ = ln.Close()
		mgr.Close()
		logger.Info("server stopped")
	}
	return stop, nil
}

/*
replaySequence sequentially executes:
- If event.dir == dirToProxy: write payload to the live proxyConn
- If event.dir == dirFromProxy: enqueue payload so the :16790 server returns it on next client request
Optionally respects preserveTiming and writeDelay using the original timestamps.
*/
func replaySequence(
	ctx context.Context,
	logger *zap.Logger,
	events []flowKeyDup,
	proxyAddr string,
	preserveTiming bool,
	writeDelay time.Duration,
) error {
	connTracker := NewConnectionTracker(ctx, logger)
	defer func() {
		if err := connTracker.Shutdown(2 * time.Minute); err != nil {
			logger.Error("replay: failed to shutdown connection tracker", zap.Error(err))
		}
	}()

	// Start the app-side server
	mgr := NewFeederManager()
	stopServer, err := startAppSideServer(ctx, logger, DefaultDestPort, mgr)
	if err != nil {
		return err
	}
	defer stopServer()

	// Sort events by timestamp
	sort.Slice(events, func(i, j int) bool { return events[i].ts.Before(events[j].ts) })

	// Process events sequentially
	var prev time.Time
	for i, ev := range events {
		// Handle timing preservation
		if preserveTiming && !prev.IsZero() {
			if d := ev.ts.Sub(prev); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					logger.Warn("replay: context cancelled during timing preservation")
					return ctx.Err()
				}
			}
		}
		prev = ev.ts

		// Handle write delay
		if writeDelay > 0 {
			select {
			case <-time.After(writeDelay):
			case <-ctx.Done():
				logger.Warn("replay: context cancelled during write delay")
				return ctx.Err()
			}
		}

		// Check for context cancellation
		select {
		case <-ctx.Done():
			logger.Warn("replay: context cancelled, aborting replay")
			return ctx.Err()
		default:
		}

		// Process the event
		switch ev.dir {
		case DirToProxy:
			// Get or create connection
			writer := connTracker.GetConnection(ev.srcPort)
			if writer == nil {
				newWriter, err := connTracker.CreateConnection(ev.srcPort, proxyAddr)
				if err != nil {
					return fmt.Errorf("failed to create connection for srcPort %d: %w", ev.srcPort, err)
				}
				writer = newWriter

				// Create/register feeder for this srcPort
				_ = mgr.GetOrCreate(ev.srcPort)
			}

			// Write payload to proxy
			if err := writer.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
				logger.Warn("replay: failed to set write deadline", zap.Error(err))
			}

			if _, err := writer.Write(ev.payload); err != nil {
				return fmt.Errorf("write to proxy (event %d, srcPort %d): %w", i+1, ev.srcPort, err)
			}

			logger.Info(fmt.Sprintf("[REPLAY %03d] →proxy wrote %d bytes", i+1, len(ev.payload)),
				zap.Uint16("srcPort", ev.srcPort))

		case DirFromProxy:
			// Queue payload for app-side server
			feeder := mgr.GetOrCreate(ev.dstPt)
			feeder.push(ev.payload)

			logger.Info(fmt.Sprintf("[REPLAY %03d] proxy→app queued %d bytes to feeder %d", i+1, len(ev.payload), ev.dstPt),
				zap.Uint16("srcPort", ev.srcPort))
		}
	}

	// Close the FeederManager to unblock any waiting operations
	mgr.Close()

	logger.Info("replay: finished processing events, shutting down connections",
		zap.Int("active_connections", connTracker.GetActiveConnectionCount()))

	return nil
}

// ReadSrcPortEvents iterates packets, filters on proxy port, dedups by TCP seq,
// and groups events by the peer (non-proxy) port.
func ReadSrcPortEvents(r *pcapgo.Reader, proxyPort int, log *zap.Logger) ([]flowKeyDup, error) {
	var srcPorts []flowKeyDup
	seenSeq := make(map[uint32]bool)

	for {
		data, ci, err := r.ZeroCopyReadPacketData()
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF || strings.Contains(strings.ToLower(err.Error()), "eof") {
				break
			}
			// soft-read issues—log and stop
			log.Debug("pcap read error", zap.Error(err))
			break
		}

		pkt := gopacket.NewPacket(data, layers.LinkTypeEthernet, gopacket.NoCopy)
		tl := pkt.TransportLayer()
		if tl == nil {
			continue
		}
		ip := pkt.NetworkLayer()
		if ip == nil {
			continue
		}
		tcp, ok := tl.(*layers.TCP)
		if !ok {
			continue
		}

		if !isProxySideTCP(tcp, proxyPort) || len(tcp.Payload) == 0 {
			continue
		}
		if seenSeq[tcp.Seq] {
			log.Debug("duplicate seq, skipping", zap.Uint32("seq", tcp.Seq))
			continue
		}
		seenSeq[tcp.Seq] = true

		srcIP := ip.NetworkFlow().Src().String()
		dstIP := ip.NetworkFlow().Dst().String()

		ev := flowKeyDup{
			srcIP:   srcIP,
			dstIP:   dstIP,
			srcPort: uint16(tcp.SrcPort),
			dstPt:   uint16(tcp.DstPort),
			payload: append([]byte(nil), tcp.Payload...), // copy
			ts:      ci.Timestamp,
		}
		if int(tcp.DstPort) == proxyPort {
			ev.dir = DirToProxy
		} else {
			ev.dir = DirFromProxy
		}

		// group by the peer (non-proxy) port
		if ev.srcPort != uint16(proxyPort) {
			srcPorts = append(srcPorts, ev)
		} else {
			srcPorts = append(srcPorts, ev)
		}
	}

	if len(srcPorts) == 0 {
		log.Error("no proxy-related payloads found in pcap")
		return nil, ErrNoProxyRelatedFlows
	}
	return srcPorts, nil
}

func isProxySideTCP(tcp *layers.TCP, proxyPort int) bool {
	return int(tcp.SrcPort) == proxyPort || int(tcp.DstPort) == proxyPort
}

func StartReplay(ctx context.Context, logger *zap.Logger, opts ReplayOptions, pcapPath string) error {

	if pcapPath == "" {
		return ErrMissingPCAP
	}

	proxyAddr, proxyPort, preserveTiming, writeDelay, err := prepareReplayInputs(opts)
	if err != nil {
		return err
	}

	// Open PCAP
	f, err := os.Open(pcapPath)
	if err != nil {
		logger.Error("failed to open pcap file", zap.Error(err))
		return fmt.Errorf("failed to open pcap: %w", err)
	}
	defer f.Close()

	pcapReader, err := pcapgo.NewReader(f)
	if err != nil {
		logger.Error("pcap reader", zap.Error(err))
		return fmt.Errorf("failed to create pcap reader: %w", err)
	}

	packetEvents, err := ReadSrcPortEvents(pcapReader, proxyPort, logger)
	if err != nil {
		return err
	}

	if len(packetEvents) == 0 {
		return ErrNoProxyRelatedFlows
	}

	for index, ev := range packetEvents {
		if ev.dir == DirToProxy {
			logger.Info(fmt.Sprintf("index %d , →proxy srcPort=%d dstPort=%d bytes=%d", index+1, ev.srcPort, ev.dstPt, len(ev.payload)))
		} else {
			logger.Info(fmt.Sprintf("index %d , proxy→app srcPort=%d dstPort=%d bytes=%d", index+1, ev.srcPort, ev.dstPt, len(ev.payload)))
		}
	}

	err = replaySequence(ctx, logger, packetEvents, proxyAddr, preserveTiming, writeDelay)
	if err != nil {
		logger.Error("replay sequence failed", zap.Error(err))
		return err
	}

	return nil
}

func prepareReplayInputs(opts ReplayOptions) (proxyAddr string, proxyPort int, preserveTiming bool, writeDelay time.Duration, err error) {
	proxyAddr = DefaultProxyAddr
	proxyPort = DefaultProxyPort
	preserveTiming = opts.PreserveTiming
	writeDelay = opts.WriteDelay
	return
}
