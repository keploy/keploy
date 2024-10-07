// Package hooks provides functionality for managing hooks.
package hooks

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		logger:    logger,
		sess:      core.NewSessions(),
		m:         sync.Mutex{},
		proxyIP4:  "127.0.0.1",
		proxyIP6:  [4]uint32{0000, 0000, 0000, 0001},
		proxyPort: cfg.ProxyPort,
		dnsPort:   cfg.DNSPort,
	}
}

type Hooks struct {
	logger    *zap.Logger
	sess      *core.Sessions
	proxyIP4  string
	proxyIP6  [4]uint32
	proxyPort uint32
	dnsPort   uint32

	m     sync.Mutex
	appID uint64
}

func (h *Hooks) Load(ctx context.Context, id uint64, opts core.HookCfg) error {

	h.sess.Set(id, &core.Session{
		ID: id,
	})

	err := h.load(ctx, opts)
	if err != nil {
		return err
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer utils.Recover(h.logger)
		<-ctx.Done()
		h.unLoad(ctx)

		//deleting in order to free the memory in case of rerecord.
		h.sess.Delete(id)
		return nil
	})

	return nil
}

func (h *Hooks) load(ctx context.Context, opts core.HookCfg) error {
	// start name pipe
	// start windivert
	// send pid
	// make channels for outgoing
	// make channels for incoming
	// make receive channels for outgoing 
	return nil
}

func (h *Hooks) Record(ctx context.Context, _ uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	// TODO use the session to get the app id
	// and then use the app id to get the test cases chan
	// and pass that to eBPF consumers/listeners
	return nil, nil
}

func (h *Hooks) unLoad(_ context.Context) {
	// stop name pipe
	// stop windivert
}
