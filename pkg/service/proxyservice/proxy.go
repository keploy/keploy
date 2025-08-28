package proxyservice

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

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

func New(logger *zap.Logger, cfg *config.Config, mockDB MockDB) *ProxyService {
	session := core.NewSessions()
	p := proxy.New(logger, NewFakeDestInfo(), cfg, session)

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

	err := s.StartReplay(
		ReplayOptions{
			PreserveTiming: PreserveTiming,
			WriteDelay:     WriteDelay,
		},
	)
	if err != nil {
		s.logger.Error("failed to start replay", zap.Error(err))
	}

	s.logger.Info("Replay finished, stopping proxy server")

	return nil
}

func (s *ProxyService) StartReplay(opts ReplayOptions) error {

	pcapPath, proxyAddr, proxyPort, preserveTiming, writeDelay, err := s.prepareReplayInputs(opts)
	if err != nil {
		return err
	}

	var streams []streamSeq

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

	srcPorts, err := readSrcPortEvents(r, proxyPort, s.logger)
	if err != nil {
		return err
	}

	if len(srcPorts) == 0 {
		return ErrNoProxyRelatedFlows
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

func (s *ProxyService) prepareReplayInputs(opts ReplayOptions) (pcapPath, proxyAddr string, proxyPort int, preserveTiming bool, writeDelay time.Duration, err error) {
	pcapPath = s.cfg.Proxy.PcapPath
	if pcapPath == "" {
		s.logger.Error("missing -pcap")
		return "", "", 0, false, 0, ErrMissingPCAP
	}
	proxyAddr = DefaultProxyAddr
	proxyPort = DefaultProxyPort
	preserveTiming = opts.PreserveTiming
	writeDelay = opts.WriteDelay
	return
}
