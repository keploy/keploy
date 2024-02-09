package core

import (
	"context"

	"github.com/cloudflare/cfssl/log"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/proxy"
	"go.uber.org/zap"
)

type Config struct {
	Port uint32
}

// Init will initialize the core keploy servives
func Init(ctx context.Context, config Config) error {
	// disable init logs from the cfssl library
	log.Level = 0

	// Initiate the hooks
	loadedHooks, err := hooks.NewHook(ys, routineId, s.logger)
	if err != nil {
		s.logger.Error("error while creating hooks", zap.Error(err))
		return
	}

	if err := loadedHooks.LoadHooks("", "", pid, ctx, nil); err != nil {
		return
	}

	// start the proxy
	ps := proxy.BootProxy(s.logger, proxy.Option{Port: proxyPort}, "", "", pid, "", []uint{}, loadedHooks, ctx, 0)
}
