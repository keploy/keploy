//go:build windows

// Package hooks provides functionality for managing hooks.
package hooks

import (
	"context"
	"errors"
	"net"
	"sync"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/conn"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		logger:         logger,
		sess:           core.NewSessions(),
		m:              sync.Mutex{},
		proxyIP4:       "127.0.0.1",
		proxyIP6:       [4]uint32{0000, 0000, 0000, 0001},
		proxyPort:      cfg.ProxyPort,
		dnsPort:        cfg.DNSPort,
		openEventChan:  make(chan conn.SocketOpenEvent, 1024),
		closeEventChan: make(chan conn.SocketCloseEvent, 1024),
		dataEventChan:  make(chan conn.SocketDataEvent, 1024),
	}
}

type Hooks struct {
	logger    *zap.Logger
	sess      *core.Sessions
	proxyIP4  string
	proxyIP6  [4]uint32
	proxyPort uint32
	dnsPort   uint32
	m         sync.Mutex
	// windows destination info map
	dstMap sync.Map
	// windows incoming traffic streams
	openEventChan  chan conn.SocketOpenEvent
	closeEventChan chan conn.SocketCloseEvent
	dataEventChan  chan conn.SocketDataEvent
	conn           net.Conn
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
