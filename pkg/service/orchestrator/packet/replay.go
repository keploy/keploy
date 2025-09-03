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

// LogFields contains common structured logging fields
type LogFields struct {
	Component string
	Operation string
	SrcPort   uint16
	DstPort   uint16
	EventID   int
	ConnID    string
}

// ToZapFields converts LogFields to zap.Field slice
func (lf LogFields) ToZapFields() []zap.Field {
	fields := make([]zap.Field, 0, 6)

	if lf.Component != "" {
		fields = append(fields, zap.String("component", lf.Component))
	}
	if lf.Operation != "" {
		fields = append(fields, zap.String("operation", lf.Operation))
	}
	if lf.SrcPort != 0 {
		fields = append(fields, zap.Uint16("src_port", lf.SrcPort))
	}
	if lf.DstPort != 0 {
		fields = append(fields, zap.Uint16("dst_port", lf.DstPort))
	}
	if lf.EventID != 0 {
		fields = append(fields, zap.Int("event_id", lf.EventID))
	}
	if lf.ConnID != "" {
		fields = append(fields, zap.String("conn_id", lf.ConnID))
	}

	return fields
}

// ServerMetrics tracks server-level metrics for logging
type ServerMetrics struct {
	ActiveConnections int64
	TotalConnections  int64
	BytesWritten      int64
	BytesRead         int64
	Errors            int64
}

// ReplayMetrics tracks replay-level metrics for logging
type ReplayMetrics struct {
	EventsProcessed    int64
	ToProxyEvents      int64
	FromProxyEvents    int64
	BytesToProxy       int64
	BytesFromProxy     int64
	ConnectionsCreated int64
}

/*
startAppSideServer starts a TCP server on :16790.
Protocol:
  - For each incoming connection, it will read a request (any bytes) from the client.
  - After a read (or even a zero-length read if client just connects and writes later),
    it pops the next "fromProxy" payload and writes it back as the response.
  - It keeps doing this (read -> respond) in sequence until the feeder is closed.
*/
func startAppSideServer(ctx context.Context, logger *zap.Logger, listenPort int, mgr *FeederManager) (stop func(), err error) {
	baseFields := LogFields{
		Component: "app_server",
		Operation: "startup",
		DstPort:   uint16(listenPort),
	}

	listenAddr := fmt.Sprintf(":%d", listenPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		logger.Error("failed to start TCP listener",
			append(baseFields.ToZapFields(),
				zap.String("listen_addr", listenAddr),
				zap.Error(err),
			)...)
		return nil, fmt.Errorf("listen %s: %w", listenAddr, err)
	}

	logger.Info("app side server started successfully",
		append(baseFields.ToZapFields(),
			zap.String("listen_addr", listenAddr),
		)...)

	var wg sync.WaitGroup
	shutdown := make(chan struct{})

	// Context cancellation handler
	go func() {
		<-ctx.Done()
		logger.Info("app server received context cancellation",
			LogFields{Component: "app_server", Operation: "context_cancel"}.ToZapFields()...)
		close(shutdown)
	}()

	drainUntilEOF := func(c net.Conn, shutdown <-chan struct{}, connID string) {
		drainFields := LogFields{
			Component: "app_server",
			Operation: "drain_connection",
			ConnID:    connID,
		}

		logger.Debug("starting connection drain",
			drainFields.ToZapFields()...)

		buf := make([]byte, 8<<10)
		bytesRead := int64(0)

		for {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Minute))
			select {
			case <-shutdown:
				logger.Debug("drain stopped due to shutdown",
					append(drainFields.ToZapFields(),
						zap.Int64("bytes_drained", bytesRead),
					)...)
				return
			default:
			}

			n, err := c.Read(buf)
			if n > 0 {
				bytesRead += int64(n)
			}

			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				logger.Debug("drain completed with error",
					append(drainFields.ToZapFields(),
						zap.Int64("bytes_drained", bytesRead),
						zap.Error(err),
					)...)
				return
			}
		}
	}

	serveConn := func(c net.Conn) {
		connID := fmt.Sprintf("%s->%s", c.RemoteAddr().String(), c.LocalAddr().String())
		connFields := LogFields{
			Component: "app_server",
			Operation: "serve_connection",
			ConnID:    connID,
		}

		logger.Info("new connection established",
			append(connFields.ToZapFields(),
				zap.String("remote_addr", c.RemoteAddr().String()),
				zap.String("local_addr", c.LocalAddr().String()),
			)...)

		tcp, _ := c.(*net.TCPConn)
		defer func() {
			c.Close()
			logger.Info("connection closed",
				connFields.ToZapFields()...)
		}()

		// Claim a feeder exclusively
		srcPort, feeder, err := mgr.Acquire()
		if err != nil {
			logger.Error("failed to acquire feeder",
				append(connFields.ToZapFields(),
					zap.Error(err),
				)...)
			return
		}

		// Update connection fields with srcPort
		connFields.SrcPort = srcPort
		logger.Info("feeder acquired successfully",
			append(connFields.ToZapFields(),
				zap.Uint16("assigned_src_port", srcPort),
			)...)

		// Ensure cleanup when this conn finishes
		defer func() {
			mgr.Release(srcPort)
			logger.Info("feeder released",
				connFields.ToZapFields()...)
		}()

		bytesWritten := int64(0)
		responsesWritten := int64(0)

		for {
			select {
			case <-shutdown:
				logger.Debug("stopping writes due to shutdown",
					append(connFields.ToZapFields(),
						zap.Int64("bytes_written", bytesWritten),
						zap.Int64("responses_written", responsesWritten),
					)...)
				goto afterWrites
			case <-feeder.done:
				logger.Debug("stopping writes due to feeder completion",
					append(connFields.ToZapFields(),
						zap.Int64("bytes_written", bytesWritten),
						zap.Int64("responses_written", responsesWritten),
					)...)
				goto afterWrites
			default:
				resp, ok := feeder.pop(shutdown)
				if !ok {
					logger.Debug("feeder closed, stopping writes",
						append(connFields.ToZapFields(),
							zap.Int64("bytes_written", bytesWritten),
							zap.Int64("responses_written", responsesWritten),
						)...)
					goto afterWrites
				}

				_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
				n, err := c.Write(resp)
				if err != nil {
					logger.Error("write error occurred",
						append(connFields.ToZapFields(),
							zap.Int("attempted_bytes", len(resp)),
							zap.Int("written_bytes", n),
							zap.Int64("total_bytes_written", bytesWritten),
							zap.Error(err),
						)...)
					goto afterWrites
				}

				bytesWritten += int64(n)
				responsesWritten++

				logger.Debug("response written to connection",
					append(connFields.ToZapFields(),
						zap.Int("response_bytes", len(resp)),
						zap.Int64("total_bytes_written", bytesWritten),
						zap.Int64("total_responses_written", responsesWritten),
					)...)
			}
		}

	afterWrites:
		// Close write side and drain
		if tcp != nil {
			if err := tcp.CloseWrite(); err != nil {
				logger.Warn("failed to close write side",
					append(connFields.ToZapFields(),
						zap.Error(err),
					)...)
			} else {
				logger.Info("write side closed successfully",
					append(connFields.ToZapFields(),
						zap.String("action", "sent_fin"),
					)...)
			}
		}

		drainUntilEOF(c, shutdown, connID)
	}

	acceptLoop := func() {
		acceptFields := LogFields{
			Component: "app_server",
			Operation: "accept_loop",
		}

		logger.Info("accept loop started",
			acceptFields.ToZapFields()...)

		for {
			select {
			case <-shutdown:
				logger.Info("accept loop exiting due to shutdown",
					acceptFields.ToZapFields()...)
				return
			default:
				conn, err := ln.Accept()
				if err != nil {
					select {
					case <-shutdown:
						logger.Info("accept loop exiting due to shutdown after accept error",
							append(acceptFields.ToZapFields(),
								zap.Error(err),
							)...)
						return
					default:
					}
					logger.Error("accept error occurred",
						append(acceptFields.ToZapFields(),
							zap.Error(err),
						)...)
					continue
				}

				logger.Debug("new connection accepted",
					append(acceptFields.ToZapFields(),
						zap.String("remote_addr", conn.RemoteAddr().String()),
					)...)

				wg.Add(1)
				go func() {
					defer func() {
						wg.Done()
					}()
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
		stopFields := LogFields{
			Component: "app_server",
			Operation: "shutdown",
		}

		logger.Info("initiating server shutdown",
			stopFields.ToZapFields()...)

		close(shutdown)
		_ = ln.Close()
		mgr.Close()

		// Wait for all connections to finish with timeout
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			logger.Info("server shutdown completed successfully")
		case <-time.After(30 * time.Second):
			logger.Warn("server shutdown timeout reached")
		}
	}
	return stop, nil
}

func replaySequence(
	ctx context.Context,
	logger *zap.Logger,
	events []flowKeyDup,
	proxyAddr string,
	preserveTiming bool,
	writeDelay time.Duration,
) error {
	replayFields := LogFields{
		Component: "replay_sequence",
		Operation: "startup",
	}

	logger.Info("starting replay sequence",
		append(replayFields.ToZapFields(),
			zap.Int("total_events", len(events)),
			zap.String("proxy_addr", proxyAddr),
			zap.Bool("preserve_timing", preserveTiming),
			zap.Duration("write_delay", writeDelay),
		)...)

	metrics := &ReplayMetrics{}
	startTime := time.Now()

	connTracker := NewConnectionTracker(ctx, logger)
	defer func() {
		shutdownStart := time.Now()
		if err := connTracker.Shutdown(2 * time.Minute); err != nil {
			logger.Error("connection tracker shutdown failed",
				append(replayFields.ToZapFields(),
					zap.Error(err),
					zap.Duration("shutdown_duration", time.Since(shutdownStart)),
				)...)
		} else {
			logger.Info("connection tracker shutdown completed",
				append(replayFields.ToZapFields(),
					zap.Duration("shutdown_duration", time.Since(shutdownStart)),
				)...)
		}
	}()

	// Start the app-side server
	mgr := NewFeederManager()
	stopServer, err := startAppSideServer(ctx, logger, DefaultDestPort, mgr)
	if err != nil {
		logger.Error("failed to start app-side server",
			append(replayFields.ToZapFields(),
				zap.Error(err),
			)...)
		return err
	}
	defer stopServer()

	// Sort events by timestamp
	sort.Slice(events, func(i, j int) bool { return events[i].ts.Before(events[j].ts) })

	logger.Info("events sorted by timestamp, beginning replay",
		append(replayFields.ToZapFields(),
			zap.Time("first_event_time", events[0].ts),
			zap.Time("last_event_time", events[len(events)-1].ts),
			zap.Duration("time_span", events[len(events)-1].ts.Sub(events[0].ts)),
		)...)

	// Process events sequentially
	var prev time.Time
	for i, ev := range events {
		eventFields := LogFields{
			Component: "replay_sequence",
			Operation: "process_event",
			EventID:   i + 1,
			SrcPort:   ev.srcPort,
			DstPort:   ev.dstPt,
		}

		// Handle timing preservation
		if preserveTiming && !prev.IsZero() {
			if d := ev.ts.Sub(prev); d > 0 {
				logger.Debug("preserving timing delay",
					append(eventFields.ToZapFields(),
						zap.Duration("delay", d),
						zap.Time("event_timestamp", ev.ts),
					)...)

				select {
				case <-time.After(d):
				case <-ctx.Done():
					logger.Warn("context cancelled during timing preservation",
						append(eventFields.ToZapFields(),
							zap.Error(ctx.Err()),
						)...)
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
				logger.Warn("context cancelled during write delay",
					append(eventFields.ToZapFields(),
						zap.Duration("write_delay", writeDelay),
						zap.Error(ctx.Err()),
					)...)
				return ctx.Err()
			}
		}

		// Check for context cancellation
		select {
		case <-ctx.Done():
			logger.Warn("replay cancelled by context",
				append(eventFields.ToZapFields(),
					zap.Int64("events_processed", metrics.EventsProcessed),
					zap.Error(ctx.Err()),
				)...)
			return ctx.Err()
		default:
		}

		// Process the event
		switch ev.dir {
		case DirToProxy:
			eventFields.Operation = "write_to_proxy"

			// Get or create connection
			writer := connTracker.GetConnection(ev.srcPort)
			if writer == nil {
				logger.Debug("creating new connection to proxy",
					append(eventFields.ToZapFields(),
						zap.String("proxy_addr", proxyAddr),
					)...)

				newWriter, err := connTracker.CreateConnection(ev.srcPort, proxyAddr)
				if err != nil {
					logger.Error("failed to create connection to proxy",
						append(eventFields.ToZapFields(),
							zap.String("proxy_addr", proxyAddr),
							zap.Error(err),
						)...)
					return fmt.Errorf("failed to create connection for srcPort %d: %w", ev.srcPort, err)
				}
				writer = newWriter
				metrics.ConnectionsCreated++

				// Create/register feeder for this srcPort
				_ = mgr.GetOrCreate(ev.srcPort)

				logger.Info("new connection established to proxy",
					append(eventFields.ToZapFields(),
						zap.String("proxy_addr", proxyAddr),
						zap.Int64("total_connections_created", metrics.ConnectionsCreated),
						zap.String("local_addr", writer.LocalAddr().String()),
					)...)
			}

			// Write payload to proxy
			if err := writer.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
				logger.Warn("failed to set write deadline",
					append(eventFields.ToZapFields(),
						zap.Error(err),
					)...)
			}

			n, err := writer.Write(ev.payload)
			if err != nil {
				logger.Error("failed to write to proxy",
					append(eventFields.ToZapFields(),
						zap.Int("payload_size", len(ev.payload)),
						zap.Int("bytes_written", n),
						zap.Error(err),
					)...)
				return fmt.Errorf("write to proxy (event %d, srcPort %d): %w", i+1, ev.srcPort, err)
			}

			metrics.ToProxyEvents++
			metrics.BytesToProxy += int64(len(ev.payload))

			logger.Debug("payload written to proxy",
				append(eventFields.ToZapFields(),
					zap.Int("payload_bytes", len(ev.payload)),
					zap.Int64("total_to_proxy_events", metrics.ToProxyEvents),
					zap.Int64("total_bytes_to_proxy", metrics.BytesToProxy),
				)...)

		case DirFromProxy:
			eventFields.Operation = "queue_from_proxy"

			// Queue payload for app-side server
			feeder := mgr.GetOrCreate(ev.dstPt)
			feeder.push(ev.payload)

			metrics.FromProxyEvents++
			metrics.BytesFromProxy += int64(len(ev.payload))

			logger.Debug("payload queued for app server",
				append(eventFields.ToZapFields(),
					zap.Int("payload_bytes", len(ev.payload)),
					zap.Int64("total_from_proxy_events", metrics.FromProxyEvents),
					zap.Int64("total_bytes_from_proxy", metrics.BytesFromProxy),
				)...)
		}

		metrics.EventsProcessed++

		// Log progress every 100 events
		if (i+1)%100 == 0 {
			logger.Info("replay progress update",
				append(replayFields.ToZapFields(),
					zap.Int("events_processed", i+1),
					zap.Int("total_events", len(events)),
					zap.Float64("progress_percent", float64(i+1)/float64(len(events))*100),
					zap.Duration("elapsed_time", time.Since(startTime)),
				)...)
		}
	}

	// Close the FeederManager to unblock any waiting operations
	mgr.Close()

	replayDuration := time.Since(startTime)

	logger.Info("replay sequence completed successfully",
		append(replayFields.ToZapFields(),
			zap.Duration("total_duration", replayDuration),
			zap.Int64("events_processed", metrics.EventsProcessed),
			zap.Int64("to_proxy_events", metrics.ToProxyEvents),
			zap.Int64("from_proxy_events", metrics.FromProxyEvents),
			zap.Int64("bytes_to_proxy", metrics.BytesToProxy),
			zap.Int64("bytes_from_proxy", metrics.BytesFromProxy),
			zap.Int64("connections_created", metrics.ConnectionsCreated),
			zap.Int("active_connections", connTracker.GetActiveConnectionCount()),
			zap.Float64("events_per_second", float64(metrics.EventsProcessed)/replayDuration.Seconds()),
		)...)

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
			// log.Debug("duplicate seq, skipping", zap.Uint32("seq", tcp.Seq))
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

	// for index, ev := range packetEvents {
	// 	if ev.dir == DirToProxy {
	// 		logger.Info(fmt.Sprintf("index %d , →proxy srcPort=%d dstPort=%d bytes=%d", index+1, ev.srcPort, ev.dstPt, len(ev.payload)))
	// 	} else {
	// 		logger.Info(fmt.Sprintf("index %d , proxy→app srcPort=%d dstPort=%d bytes=%d", index+1, ev.srcPort, ev.dstPt, len(ev.payload)))
	// 	}
	// }

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
