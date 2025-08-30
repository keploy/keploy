//go:build linux

package proxyservice

import (
	"context"
	"fmt"
	"io"
	"net"
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
func startAppSideServer(logger *zap.Logger, listenPort int, feeder *responseFeeder) (stop func(), err error) {

	listenAddr := fmt.Sprintf(":%d", listenPort)

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	logger.Info("app side server listening", zap.String("addr", listenAddr))

	var wg sync.WaitGroup
	shutdown := make(chan struct{})

	// drainUntilEOF keeps reading until EOF or shutdown
	drainUntilEOF := func(c net.Conn, shutdown <-chan struct{}) {
		buf := make([]byte, 8<<10)
		for {
			// Keep a soft deadline so we don't hang forever if peer misbehaves.
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Minute))
			select {
			case <-shutdown:
				return
			default:
			}
			_, err := c.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Keep looping; extend grace period.
					continue
				}
				// io.EOF (peer FIN) or any other read error -> stop draining.
				return
			}
		}
	}

	serveConn := func(c net.Conn) {
		// If we can, get TCPConn to use half-close
		tcp, _ := c.(*net.TCPConn)
		defer c.Close()

		// Indicate that the TCP connection is established
		logger.Info("server: tcp connection established")

		// Push responses as they become available.
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
				logger.Debug("server: proxy->app wrote response", zap.Int("bytes", len(resp)))
			}
		}

	afterWrites:
		// === Graceful close sequence ===
		// 1) Half-close write side to signal "no more bytes coming".
		if tcp != nil {
			if err := tcp.CloseWrite(); err != nil {
				logger.Warn("server: CloseWrite error", zap.Error(err))
			} else {
				logger.Info("server: CloseWrite done (sent FIN)")
			}
		} else {
			// Fallback: set a tiny read deadline to avoid hanging forever.
			logger.Info("server: non-TCPConn, skipping CloseWrite")
		}

		// 2) Drain until the peer closes their write side (we see EOF) or shutdown.
		drainUntilEOF(c, shutdown)
		// 3) defer will Close() the socket now.
	}

	acceptLoop := func() {
		for {
			select {
			case <-shutdown: // This ensures that shutdown signal is processed
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
					defer func() {
						logger.Debug("server: goroutine for serveConn(conn) exiting, calling wg.Done()")
						wg.Done()
					}()
					serveConn(conn)
				}()
			}
		}
	}

	wg.Add(1)
	go func() {
		defer func() {
			logger.Debug("server: goroutine for acceptLoop() exiting, calling wg.Done()")
			wg.Done()
		}()
		acceptLoop()
	}()

	stop = func() {
		close(shutdown)
		// Wake any handlers waiting in pop()
		feeder.close()
		err = ln.Close()
		if err != nil {
			logger.Error("server close error", zap.Error(err))
		}
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
	logger *zap.Logger,
	events []flowKeyDup,
	proxyAddr string,
	preserveTiming bool,
	writeDelay time.Duration,
) error {
	feeder := newResponseFeeder()
	stopServer, err := startAppSideServer(logger, DefaultDestPort, feeder)
	if err != nil {
		return err
	}
	defer func() { stopServer() }()

	// Connect to proxy
	if proxyAddr == "" {
		proxyAddr = DefaultProxyAddr
	}
	proxyConn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial proxy %s: %w", proxyAddr, err)
	}

	defer func() { proxyConn.Close() }()

	logger.Info("replay: connected to proxy", zap.String("addr", proxyAddr))

	// For graceful half-close later
	var tcpC *net.TCPConn
	if tc, ok := proxyConn.(*net.TCPConn); ok {
		tcpC = tc
	}

	// Sort by original time to preserve order
	sort.Slice(events, func(i, j int) bool { return events[i].ts.Before(events[j].ts) })

	readDone := make(chan struct{})

	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		for {
			_ = proxyConn.SetReadDeadline(time.Now().Add(2 * time.Minute))
			n, err := proxyConn.Read(buf)
			if n > 0 {
				logger.Debug("drained", zap.Int("bytes", n))
			}
			if err != nil {
				return // EOF or error stops the reader
			}
		}
	}()

	var prev time.Time
	for i, ev := range events {
		// Timing controls
		if preserveTiming {
			if !prev.IsZero() {
				if d := ev.ts.Sub(prev); d > 0 {
					time.Sleep(d)
				}
			}
			prev = ev.ts
		}
		if writeDelay > 0 {
			time.Sleep(writeDelay)
		}

		switch ev.dir {
		case dirToProxy:
			_, err := proxyConn.Write(ev.payload)
			if err != nil {
				return fmt.Errorf("write to proxy (event %d): %w", i+1, err)
			}
			logger.Debug(fmt.Sprintf("[REPLAY %03d] →proxy wrote %d bytes", i+1, len(ev.payload)))

		case dirFromProxy:
			// Enqueue for :16790 server so proxy can fetch it
			feeder.push(ev.payload)
			logger.Debug(fmt.Sprintf("[REPLAY %03d] proxy→app queued %d bytes (server will respond on next request)", i+1, len(ev.payload)))
		}
	}

	feeder.close()

	if tcpC != nil {
		if err := tcpC.CloseWrite(); err != nil {
			logger.Warn("replay: CloseWrite error", zap.Error(err))
		} else {
			logger.Info("replay: CloseWrite done (sent FIN to proxy)")
		}
	} else {
		logger.Info("replay: non-TCPConn, cannot CloseWrite, will Close after wait")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	select {
	case <-readDone:
		logger.Info("replay: proxy closed its write side (EOF observed)")
	case <-waitCtx.Done():
		logger.Warn("replay: timeout waiting for proxy to finish reading/sending", zap.Error(waitCtx.Err()))
	}

	_ = proxyConn.Close()
	logger.Info("replay sequence finished")

	return nil
}

// readSrcPortEvents iterates packets, filters on proxy port, dedups by TCP seq,
// and groups events by the peer (non-proxy) port.
func readSrcPortEvents(r *pcapgo.Reader, proxyPort int, log *zap.Logger) (map[uint16][]flowKeyDup, error) {
	srcPorts := make(map[uint16][]flowKeyDup)
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
			ev.dir = dirToProxy
		} else {
			ev.dir = dirFromProxy
		}

		// group by the peer (non-proxy) port
		if ev.srcPort != uint16(proxyPort) {
			srcPorts[ev.srcPort] = append(srcPorts[ev.srcPort], ev)
		} else {
			srcPorts[ev.dstPt] = append(srcPorts[ev.dstPt], ev)
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
