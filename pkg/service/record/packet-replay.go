//go:build linux

package record

import (
	"context"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/record/packetreplay"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func (r *Recorder) StartNetworkPacketReplay(ctx context.Context) error {

	session := core.NewSessions()
	proxy := proxy.New(r.logger, packetreplay.NewFakeDestInfo(), r.config, session)

	// Create channels for testcases and mocks
	testcaseCh := make(chan *models.TestCase, 10)
	mockCh := make(chan *models.Mock, 10)

	session.Set(12345, &core.Session{
		ID:   12345,
		Mode: models.MODE_RECORD,
		TC:   testcaseCh,
		MC:   mockCh,
	})

	// Listen for mocks
	go func() {
		for mock := range mockCh {
			err := r.mockDB.InsertMock(ctx, mock, "replay-mocks")
			if err != nil {
				r.logger.Error("failed to insert mock into database", zap.Error(err))
			}
		}
	}()

	// create a new error group for the proxy
	proxyErrGrp, _ := errgroup.WithContext(ctx)
	proxyCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the proxyCtx to control the lifecycle of the proxy
	proxyCtx, proxyCtxCancel := context.WithCancel(proxyCtx)
	proxyCtx = context.WithValue(proxyCtx, models.ErrGroupKey, proxyErrGrp)

	if err := proxy.StartProxy(proxyCtx, core.ProxyOptions{}); err != nil {
		r.logger.Error("failed to start proxy", zap.Error(err))
	}

	defer func() {
		r.logger.Info("shutting down proxy server")
		proxyCtxCancel()
	}()

	r.logger.Info("proxy started", zap.Uint32("port", r.config.ProxyPort))

	err := packetreplay.StartReplay(
		r.logger,
		packetreplay.ReplayOptions{
			PreserveTiming: packetreplay.PreserveTiming,
			WriteDelay:     packetreplay.WriteDelay,
		},
		r.config.Proxy.PcapPath,
	)
	if err != nil {
		r.logger.Error("failed to start replay", zap.Error(err))
	}

	r.logger.Info("Replay finished, stopping proxy server")

	return nil
}
