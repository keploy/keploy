package provider

import (
	"context"
	"errors"
	"sync"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/service/utgen"
	"go.uber.org/zap"
)

var TeleGlobalMap sync.Map

type ServiceProvider struct {
	logger *zap.Logger
	cfg    *config.Config
	auth   service.Auth
}

func NewServiceProvider(logger *zap.Logger, cfg *config.Config, auth service.Auth) *ServiceProvider {
	return &ServiceProvider{
		logger: logger,
		cfg:    cfg,
		auth:   auth,
	}
}

func (n *ServiceProvider) GetService(ctx context.Context, cmd *cobra.Command) (interface{}, error) {

	tel := telemetry.NewTelemetry(n.logger, telemetry.Options{
		Enabled:        !n.cfg.DisableTele,
		Version:        utils.Version,
		GlobalMap:      TeleGlobalMap,
		InstallationID: n.cfg.InstallationID,
	})
	tel.Ping()

	switch cmd.Name() {
	case "gen":
		return utgen.NewUnitTestGenerator(n.cfg, tel, n.auth, n.logger)
	case "record", "test", "mock", "normalize", "rerecord", "contract", "config", "update", "login", "export", "import", "templatize", "report":
		bigPayload, err := cmd.Flags().GetBool("bigPayload")
		if err != nil {
			bigPayload = false
		}
		n.cfg.Record.BigPayload = bigPayload
		return Get(ctx, cmd.Name(), n.cfg, n.logger, tel, n.auth)
	default:
		return nil, errors.New("invalid command")
	}
}
