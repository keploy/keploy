//go:build linux

// Package hooks provides functionality for managing hooks.
package hooks

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

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

	m sync.Mutex

	// eBPF C shared maps
	clientRegistrationMap    *ebpf.Map
	agentRegistartionMap     *ebpf.Map
	dockerAppRegistrationMap *ebpf.Map
	redirectProxyMap         *ebpf.Map

	//--------------

	// eBPF C shared objectsobjects
	// ebpf objects and events
	socket      link.Link
	connect4    link.Link
	gp4         link.Link
	udpp4       link.Link
	tcppv4      link.Link
	tcpv4       link.Link
	tcpv4Ret    link.Link
	connect6    link.Link
	gp6         link.Link
	tcppv6      link.Link
	tcpv6       link.Link
	tcpv6Ret    link.Link
	accept      link.Link
	acceptRet   link.Link
	accept4     link.Link
	accept4Ret  link.Link
	read        link.Link
	readRet     link.Link
	write       link.Link
	writeRet    link.Link
	close       link.Link
	closeRet    link.Link
	sendto      link.Link
	sendtoRet   link.Link
	recvfrom    link.Link
	recvfromRet link.Link
	objects     bpfObjects
	writev      link.Link
	writevRet   link.Link
	appID       uint64
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
