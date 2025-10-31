//go:build windows

// Package hooks provides functionality for managing hooks.
package windows

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/utils"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/conn"
	windows_comm "go.keploy.io/server/v3/pkg/agent/hooks/windows/ipc/windows"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		logger:         logger,
		sess:           agent.NewSessions(),
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
	sess      *agent.Sessions
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

func (h *Hooks) Load(ctx context.Context, cfg agent.HookCfg, setupOpts config.Agent) error {

	h.sess.Set(uint64(0), &agent.Session{
		ID: uint64(0), // need to check this one
	})

	err := h.load(ctx, setupOpts)
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
		h.sess.Delete(uint64(0))
		return nil
	})

	return nil
}

func (h *Hooks) load(ctx context.Context, setupOpts config.Agent) error {

	unixSocket := windows_comm.UnixSocket{
		Path:   `C:\my.sock`,
		Logger: h.logger,
	}

	connChan := make(chan net.Conn, 1)
	errChan := make(chan error, 1)

	go func() {
		conn, err := unixSocket.Start(ctx)
		if err != nil {
			h.logger.Error("Unable to start commuciation with redirector", zap.Error(err))
			errChan <- err
			return
		}
		connChan <- conn
	}()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return errors.New("unable to get the current filename")
	}
	dirname := filepath.Dir(filename)

	// Join the current directory with the relative path to the executable
	exePath := filepath.Join(dirname, "windows-redirector.exe")
	exePath = filepath.Clean(exePath)

	cmd := exec.CommandContext(ctx, exePath, `C:\my.sock`)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start the executable: %w", err)
	}

	select {
	case conn := <-connChan:
		h.conn = conn
		clientInfo := structs.ClientInfo{
			ClientNSPID: setupOpts.ClientNSPID,
		}
		err := h.SendClientInfo(clientInfo)
		if err != nil {
			h.logger.Error("failed to send client info", zap.Error(err))
			return fmt.Errorf("failed to load hooks")
		}
	case <-errChan:
		return fmt.Errorf("failed to load hooks")
	}

	go func() {
		h.GetEvents(ctx)
	}()
	return nil
}

func (h *Hooks) unLoad(_ context.Context) {
}

func (h *Hooks) Record(ctx context.Context, _ uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	return conn.ListenSocket(ctx, h.logger, h.openEventChan, h.dataEventChan, h.closeEventChan, opts)
}

func (h *Hooks) WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error) {
	return nil, nil
}
