//go:build linux

package proxyservice

import (
	"context"
	"os"
	"os/signal"
	"syscall"

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

func (s *ProxyService) StartProxy(ctx context.Context) {

	// Create channels for testcases and mocks
	testcaseCh := make(chan *models.TestCase, 10)
	mockCh := make(chan *models.Mock, 10)

	s.session.Set(12345, &core.Session{
		ID:   12345,
		Mode: models.MODE_RECORD,
		TC:   testcaseCh,
		MC:   mockCh,
	})

	// Listen for testcases
	go func() {
		for tc := range testcaseCh {
			s.logger.Info("received testcase", zap.Any("testcase", tc))
			// Handle testcase as needed
		}
	}()
	// Listen for mocks
	go func() {
		for mock := range mockCh {
			err := s.mockDB.InsertMock(ctx, mock, "test-set-1")
			if err != nil {
				s.logger.Error("failed to insert mock into database", zap.Error(err))
			}
		}
	}()

	// create a new error group for the proxy
	proxyErrGrp, _ := errgroup.WithContext(ctx)
	proxyCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the proxyCtx to control the lifecycle of the proxy
	proxyCtx, _ = context.WithCancel(proxyCtx)
	proxyCtx = context.WithValue(proxyCtx, models.ErrGroupKey, proxyErrGrp)

	go func() {
		//TODO: should get the proxy options from config
		if err := s.proxy.StartProxy(proxyCtx, core.ProxyOptions{}); err != nil {
			s.logger.Error("failed to start proxy", zap.Error(err))
		}
		// proxyCtxCancel() // cancel the context when proxy stops
	}()

	s.logger.Info("proxy started", zap.Uint32("port", s.cfg.ProxyPort))

	// Wait for exit signal
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt, syscall.SIGTERM)
	<-exit
	s.logger.Info("shutting down proxy server")
}
