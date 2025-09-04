//go:build linux

package orchestrator

import (
	"context"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/orchestrator/packet"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	ReplayedMocksFolder = "replay-mocks"
)

func (o *Orchestrator) StartNetworkPacketReplay(ctx context.Context) error {

	session := core.NewSessions()
	proxy := proxy.New(o.logger, packet.NewFakeDestInfo(o.config.PacketReplay.DestPort), o.config, session)

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
		err := o.record.InsertMocks(ctx, ReplayedMocksFolder, mockCh)
		if err != nil {
			o.logger.Error("failed to store mocks", zap.Error(err))
		}
	}()

	// create a new error group for the proxy
	proxyErrGrp, _ := errgroup.WithContext(ctx)
	proxyCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the proxyCtx to control the lifecycle of the proxy
	proxyCtx, proxyCtxCancel := context.WithCancel(proxyCtx)
	proxyCtx = context.WithValue(proxyCtx, models.ErrGroupKey, proxyErrGrp)

	if err := proxy.StartProxy(proxyCtx, core.ProxyOptions{}); err != nil {
		o.logger.Error("failed to start proxy", zap.Error(err))
	}

	defer func() {
		o.logger.Info("shutting down proxy server")
		proxyCtxCancel()
	}()

	o.logger.Info("proxy started", zap.Uint32("port", o.config.ProxyPort))

	err := packet.StartReplay(
		ctx,
		o.logger,
		packet.ReplayOptions{
			PreserveTiming: packet.PreserveTiming,
			WriteDelay:     packet.WriteDelay,
			DestPort:       uint16(o.config.PacketReplay.DestPort),
		},
		o.config.PacketReplay.PcapPath,
	)
	if err != nil {
		o.logger.Error("failed to start replay", zap.Error(err))
	}

	o.logger.Info("Replay finished, stopping proxy server")

	return nil
}
