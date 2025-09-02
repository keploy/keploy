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
func startAppSideServer(logger *zap.Logger, listenPort int, mgr *FeederManager) (stop func(), err error) {
	listenAddr := fmt.Sprintf(":%d", listenPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	logger.Info("app side server listening", zap.String("addr", listenAddr))

	var wg sync.WaitGroup
	shutdown := make(chan struct{})

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
				logger.Debug("server: proxy->app wrote response", zap.Int("bytes", len(resp)), zap.Uint16("srcPort", srcPort))
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
func ReplaySequence(
	logger *zap.Logger,
	events []flowKeyDup,
	proxyAddr string,
	preserveTiming bool,
	writeDelay time.Duration,
) error {
	mgr := NewFeederManager()
	stopServer, err := startAppSideServer(logger, DefaultDestPort, mgr)
	if err != nil {
		return err
	}
	defer stopServer()

	srcPortsTcpWriterMap := map[uint16]*net.TCPConn{}
	var connCount int
	var mu sync.Mutex

	sort.Slice(events, func(i, j int) bool { return events[i].ts.Before(events[j].ts) })

	var prev time.Time
	for i, ev := range events {
		if preserveTiming {
			if !prev.IsZero() {
				if d := ev.ts.Sub(prev); d > 0 {
					time.Sleep(d)
				}
			}
			prev = ev.ts
		}

		// Can remove this
		if writeDelay > 0 {
			time.Sleep(writeDelay)
		}

		switch ev.dir {
		case DirToProxy:
			if _, exists := srcPortsTcpWriterMap[ev.srcPort]; !exists {
				conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
				if err != nil {
					return fmt.Errorf("dial proxy %s for srcPort %d: %w", proxyAddr, ev.srcPort, err)
				}

				// Increase the connection count with mutex lock
				mu.Lock()
				connCount++
				mu.Unlock()

				go func() {
					defer func() {
						// Decrease the connection count when the goroutine is done
						mu.Lock()
						connCount--
						mu.Unlock()
					}()

					buf := make([]byte, 32<<10)
					for {
						_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
						_, err := conn.Read(buf)
						if err != nil {
							return // EOF or error stops the reader
						}
					}
				}()

				var tcpC *net.TCPConn
				if tc, ok := conn.(*net.TCPConn); ok {
					tcpC = tc
				}
				srcPortsTcpWriterMap[ev.srcPort] = tcpC
				logger.Info("replay: connected to proxy for srcPort", zap.Uint16("srcPort", ev.srcPort))

				// IMPORTANT: create/register feeder for this srcPort NOW so that
				// the app-side server can acquire it when the proxy connects.
				_ = mgr.GetOrCreate(ev.srcPort)
			}

			writer := srcPortsTcpWriterMap[ev.srcPort]
			if _, err := writer.Write(ev.payload); err != nil {
				return fmt.Errorf("write to proxy (event %d, srcPort %d): %w", i+1, ev.srcPort, err)
			}
			logger.Debug(fmt.Sprintf("[REPLAY %03d] →proxy wrote %d bytes", i+1, len(ev.payload)), zap.Uint16("srcPort", ev.srcPort))

		case DirFromProxy:
			feeder := mgr.GetOrCreate(ev.dstPt)
			feeder.push(ev.payload)
			logger.Debug(fmt.Sprintf("[REPLAY %03d] proxy→app queued %d bytes", i+1, len(ev.payload)), zap.Uint16("srcPort", ev.srcPort))

			// Optional: If you truly need to ensure the app side has drained the item
			// before proceeding, you can busy-wait on isEmpty(). Consider adding a condition var
			// inside responseFeeder instead of sleeping.
			for !feeder.isEmpty() {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	// Close the FeederManager to unblock the app-side server if waiting.
	mgr.Close()

	// Close per-srcPort conns
	for sp, c := range srcPortsTcpWriterMap {
		_ = c.CloseWrite()
		logger.Info("replay: closed proxy conn writer", zap.Uint16("srcPort", sp))
	}

	// Wait for all connections to finish
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Polling for the connection count in a safe manner
	for {
		mu.Lock()
		if connCount == 0 {
			mu.Unlock()
			return nil
		}
		mu.Unlock()

		select {
		case <-waitCtx.Done():
			logger.Warn("replay: timeout waiting for proxy to finish reading/sending", zap.Error(waitCtx.Err()))
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
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

func StartReplay(logger *zap.Logger, opts ReplayOptions, pcapPath string) error {

	if pcapPath == "" {
		return ErrMissingPCAP
	}

	proxyAddr, proxyPort, preserveTiming, writeDelay, err := prepareReplayInputs(opts)
	if err != nil {
		return err
	}

	// var streams []StreamSeq

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

	err = ReplaySequence(logger, packetEvents, proxyAddr, preserveTiming, writeDelay)
	if err != nil {
		logger.Error("replay sequence failed", zap.Error(err))
		return err
	}

	// for port, seq := range srcPorts {
	// 	if len(seq) == 0 {
	// 		continue
	// 	}
	// 	// Sort each stream strictly by original packet time
	// 	sort.Slice(seq, func(i, j int) bool { return seq[i].ts.Before(seq[j].ts) })

	// 	first := seq[0].ts
	// 	streams = append(streams, StreamSeq{
	// 		port:    port,
	// 		events:  seq,
	// 		firstTS: first,
	// 	})
	// }
	// Sort strictly by original packet time
	// sort.Slice(streams, func(i, j int) bool { return streams[i].firstTS.Before(streams[j].firstTS) })

	// logger.Info(fmt.Sprintf("Discovered %d stream(s). Replaying sequentially.", len(streams)))

	// for sidx, st := range streams {
	// 	logger.Info(fmt.Sprintf("---- Stream %d (peer port %d) ----", sidx+1, st.port))
	// 	logger.Info(fmt.Sprintf("Sequence length: %d", len(st.events)))
	// 	for i, ev := range st.events {
	// 		dirStr := "→proxy"
	// 		if ev.dir == DirFromProxy {
	// 			dirStr = "proxy→app"
	// 		}
	// 		logger.Info(fmt.Sprintf("  [%03d] %s t=%s bytes=%d", i+1, dirStr, ev.ts.Format(time.RFC3339Nano), len(ev.payload)))
	// 	}

	// 	// REPLAY this stream (sequential as captured)
	// 	if err := ReplaySequence(logger, st.events, proxyAddr, preserveTiming, writeDelay); err != nil {
	// 		logger.Error(fmt.Sprintf("replay failed for stream %d (port %d)", sidx+1, st.port), zap.Error(err))
	// 		return fmt.Errorf("replay failed for stream %d (port %d): %w", sidx+1, st.port, err)
	// 	}

	// 	// tiny gap between streams so the proxy/app can settle
	// 	time.Sleep(100 * time.Millisecond)
	// }

	return nil
}

func prepareReplayInputs(opts ReplayOptions) (proxyAddr string, proxyPort int, preserveTiming bool, writeDelay time.Duration, err error) {
	proxyAddr = DefaultProxyAddr
	proxyPort = DefaultProxyPort
	preserveTiming = false
	writeDelay = opts.WriteDelay
	return
}
