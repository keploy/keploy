//go:build linux

package proxyservice

import (
	"context"
	"errors"
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
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type ProxyService struct {
	logger  *zap.Logger
	proxy   *proxy.Proxy
	cfg     *config.Config
	session *core.Sessions
	mockDB  MockDB
}

func New(logger *zap.Logger, p *proxy.Proxy, mockDB MockDB, cfg *config.Config, session *core.Sessions) *ProxyService {
	return &ProxyService{
		logger:  logger,
		proxy:   p,
		mockDB:  mockDB,
		cfg:     cfg,
		session: session,
	}
}

func (s *ProxyService) StartProxy(ctx context.Context) error {

	// Create channels for testcases and mocks
	testcaseCh := make(chan *models.TestCase, 10)
	mockCh := make(chan *models.Mock, 10)

	s.session.Set(12345, &core.Session{
		ID:   12345,
		Mode: models.MODE_RECORD,
		TC:   testcaseCh,
		MC:   mockCh,
	})

	// Listen for mocks
	go func() {
		for mock := range mockCh {
			err := s.mockDB.InsertMock(ctx, mock, "replay-mocks")
			if err != nil {
				s.logger.Error("failed to insert mock into database", zap.Error(err))
			}
		}
	}()

	// create a new error group for the proxy
	proxyErrGrp, _ := errgroup.WithContext(ctx)
	proxyCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the proxyCtx to control the lifecycle of the proxy
	proxyCtx, proxyCtxCancel := context.WithCancel(proxyCtx)
	proxyCtx = context.WithValue(proxyCtx, models.ErrGroupKey, proxyErrGrp)

	if err := s.proxy.StartProxy(proxyCtx, core.ProxyOptions{}); err != nil {
		s.logger.Error("failed to start proxy", zap.Error(err))
	}

	defer func() {
		s.logger.Info("shutting down proxy server")
		proxyCtxCancel()
	}()

	s.logger.Info("proxy started", zap.Uint32("port", s.cfg.ProxyPort))

	err := s.StartReplay()
	if err != nil {
		s.logger.Error("failed to start replay", zap.Error(err))
	}

	s.logger.Info("Replay finished, stopping proxy server")

	return nil
}

type direction int

const (
	dirToProxy   direction = iota // App -> Proxy  (DstPort == proxyPort)
	dirFromProxy                  // Proxy -> App  (SrcPort == proxyPort)
)

type flowKeyDup struct {
	srcIP   string
	dstIP   string
	srcPort uint16
	dstPt   uint16
	payload []byte
	ts      time.Time
	dir     direction
}

// responseFeeder feeds proxy→app payloads to the :16790 server in order.
type responseFeeder struct {
	mu    sync.Mutex
	queue [][]byte
	cond  *sync.Cond
	// when closed, server should stop
	done chan struct{}
}

type streamSeq struct {
	port    uint16
	events  []flowKeyDup
	firstTS time.Time
}

var streams []streamSeq

func newResponseFeeder() *responseFeeder {
	r := &responseFeeder{done: make(chan struct{})}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *responseFeeder) push(p []byte) {
	r.mu.Lock()
	r.queue = append(r.queue, append([]byte(nil), p...))
	r.mu.Unlock()
	r.cond.Broadcast()
}

func (r *responseFeeder) pop(ctxDone <-chan struct{}) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		if len(r.queue) > 0 {
			p := r.queue[0]
			r.queue = r.queue[1:]
			return p, true
		}
		// If either the server or the stream is shutting down, exit.
		select {
		case <-ctxDone:
			return nil, false
		case <-r.done:
			return nil, false
		default:
		}
		r.cond.Wait() // will be woken by push() or close()
	}
}

func (r *responseFeeder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.done:
		// already closed
	default:
		close(r.done)
	}
	// Wake up any goroutines blocked in cond.Wait()
	r.cond.Broadcast()
}

/*
startAppSideServer starts a TCP server on :16790.
Protocol:
  - For each incoming connection, it will read a request (any bytes) from the client.
  - After a read (or even a zero-length read if client just connects and writes later),
    it pops the next "fromProxy" payload and writes it back as the response.
  - It keeps doing this (read -> respond) in sequence until the feeder is closed.
*/
func startAppSideServer(logger *zap.Logger, listenAddr string, feeder *responseFeeder) (stop func(), err error) {
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
	// Start server on :16790
	feeder := newResponseFeeder()
	stopServer, err := startAppSideServer(logger, ":16790", feeder)
	if err != nil {
		return err
	}
	defer func() { stopServer() }()

	// Connect to proxy
	if proxyAddr == "" {
		proxyAddr = "127.0.0.1:16789"
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

	var prev time.Time
	// lastToProxyIdx := -1
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
			// lastToProxyIdx = i
			logger.Debug(fmt.Sprintf("[REPLAY %03d] →proxy wrote %d bytes", i+1, len(ev.payload)))

		case dirFromProxy:
			// Enqueue for :16790 server so proxy can fetch it,
			// but ALSO read the same number of bytes back from proxyConn,
			// otherwise we might close early and the proxy gets RST.
			feeder.push(ev.payload)
			logger.Debug(fmt.Sprintf("[REPLAY %03d] proxy→app queued %d bytes (server will respond on next request)", i+1, len(ev.payload)))
		}
	}

	// Tell proxy we’re done sending, but keep reading until it closes (if it wants).
	if tcpC != nil {
		_ = tcpC.Close()
	}

	logger.Info("replay sequence finished")
	return nil
}

func (s *ProxyService) StartReplay() error {
	var (
		pcapPath       string        = s.cfg.Proxy.PcapPath
		proxyAddr      string        = "127.0.0.1:16789"
		proxyPort      int           = 16789
		preserveTiming bool          = false
		writeDelay     time.Duration = 10 * time.Millisecond
	)

	if pcapPath == "" {
		s.logger.Error("missing -pcap")
		return errors.New("missing -pcap")
	}

	// Open PCAP
	f, err := os.Open(pcapPath)
	if err != nil {
		s.logger.Error("failed to open pcap file", zap.Error(err))
		return fmt.Errorf("failed to open pcap: %w", err)
	}
	defer f.Close()

	r, err := pcapgo.NewReader(f)
	if err != nil {
		s.logger.Error("pcap reader", zap.Error(err))
		return fmt.Errorf("failed to create pcap reader: %w", err)
	}
	link := r.LinkType()
	s.logger.Info("PCAP link type", zap.String("link", link.String()))

	// srcPorts will contain exactly ONE stream in your case, ordered later by ts.
	srcPorts := make(map[uint16][]flowKeyDup)

	seenSeqNums := make(map[uint32]bool) // checking for duplicates, incase of tcp retransmits

	packetCount := 0
	for {
		data, ci, err := r.ZeroCopyReadPacketData()
		if err != nil {
			if errors.Is(err, os.ErrClosed) || err == io.EOF || strings.Contains(strings.ToLower(err.Error()), "eof") {
				break
			}
			s.logger.Debug("read err", zap.Error(err))
			break
		}
		packetCount++

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

		// Filter to only the proxy side of the world
		if !(uint16(tcp.SrcPort) == uint16(proxyPort) || uint16(tcp.DstPort) == uint16(proxyPort)) {
			continue
		}
		if len(tcp.Payload) == 0 {
			continue
		}

		if seenSeqNums[tcp.Seq] {
			s.logger.Debug("duplicate seq, skipping", zap.Uint32("seq", tcp.Seq))
			continue
		}

		seenSeqNums[tcp.Seq] = true

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
		} else if int(tcp.SrcPort) == proxyPort {
			ev.dir = dirFromProxy
		} else {
			continue
		}

		// Store under the *non-proxy* port to group a stream (as you were doing)
		if ev.srcPort != uint16(proxyPort) {
			srcPorts[ev.srcPort] = append(srcPorts[ev.srcPort], ev)
		} else {
			// For responses, also index by the peer's port so they end up in the same slice
			srcPorts[ev.dstPt] = append(srcPorts[ev.dstPt], ev)
		}

	}

	// For simplicity, pick the first stream (you mentioned there's one)
	if len(srcPorts) == 0 {
		s.logger.Error("no proxy-related payloads found in pcap")
		return errors.New("no proxy-related payloads found in pcap")
	}

	for port, seq := range srcPorts {
		if len(seq) == 0 {
			continue
		}
		// Sort each stream strictly by original packet time
		sort.Slice(seq, func(i, j int) bool { return seq[i].ts.Before(seq[j].ts) })

		first := seq[0].ts
		streams = append(streams, streamSeq{
			port:    port,
			events:  seq,
			firstTS: first,
		})
	}
	// Sort strictly by original packet time
	sort.Slice(streams, func(i, j int) bool { return streams[i].firstTS.Before(streams[j].firstTS) })

	s.logger.Info(fmt.Sprintf("Discovered %d stream(s). Replaying sequentially.", len(streams)))

	for sidx, st := range streams {
		s.logger.Info(fmt.Sprintf("---- Stream %d (peer port %d) ----", sidx+1, st.port))
		s.logger.Info(fmt.Sprintf("Sequence length: %d", len(st.events)))
		for i, ev := range st.events {
			dirStr := "→proxy"
			if ev.dir == dirFromProxy {
				dirStr = "proxy→app"
			}
			s.logger.Info(fmt.Sprintf("  [%03d] %s t=%s bytes=%d", i+1, dirStr, ev.ts.Format(time.RFC3339Nano), len(ev.payload)))
		}

		// REPLAY this stream (sequential as captured)
		if err := replaySequence(s.logger, st.events, proxyAddr, preserveTiming, writeDelay); err != nil {
			s.logger.Error(fmt.Sprintf("replay failed for stream %d (port %d)", sidx+1, st.port), zap.Error(err))
			return fmt.Errorf("replay failed for stream %d (port %d): %w", sidx+1, st.port, err)
		}

		// tiny gap between streams so the proxy/app can settle
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}
