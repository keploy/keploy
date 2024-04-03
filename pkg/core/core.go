// Package core provides functionality for managing core functionalities in Keploy.
package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Core struct {
	Proxy         // embedding the Proxy interface to transfer the proxy methods to the core object
	Hooks         // embedding the Hooks interface to transfer the hooks methods to the core object
	logger        *zap.Logger
	id            utils.AutoInc
	apps          sync.Map
	proxyStarted  bool
	hostConfigStr string // hosts string in the nsswitch.conf of linux system. To restore the system hosts configuration after completion of test
}

func New(logger *zap.Logger, hook Hooks, proxy Proxy) *Core {
	return &Core{
		logger: logger,
		Hooks:  hook,
		Proxy:  proxy,
	}
}

func (c *Core) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	// create a new app and store it in the map
	id := uint64(c.id.Next())
	a := app.NewApp(c.logger, id, cmd, app.Options{
		DockerNetwork: opts.DockerNetwork,
		Container:     opts.Container,
		DockerDelay:   opts.DockerDelay,
	})
	c.apps.Store(id, a)

	err := a.Setup(ctx)
	if err != nil {
		utils.LogError(c.logger, err, "failed to setup app")
		return 0, err
	}
	return id, nil
}

func (c *Core) getApp(id uint64) (*app.App, error) {
	a, ok := c.apps.Load(id)
	if !ok {
		return nil, fmt.Errorf("app with id:%v not found", id)
	}

	// type assertion on the app
	h, ok := a.(*app.App)
	if !ok {
		return nil, fmt.Errorf("failed to type assert app with id:%v", id)
	}

	return h, nil
}

func (c *Core) Hook(ctx context.Context, id uint64, opts models.HookOptions) error {
	hookErr := errors.New("failed to hook into the app")

	a, err := c.getApp(id)
	if err != nil {
		utils.LogError(c.logger, err, "failed to get app")
		return hookErr
	}

	isDocker := false
	appKind := a.Kind(ctx)
	//check if the app is docker/docker-compose or native
	if appKind == utils.Docker || appKind == utils.DockerCompose {
		isDocker = true
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	// create a new error group for the hooks
	hookErrGrp, _ := errgroup.WithContext(ctx)
	hookCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the hookCtx to control the lifecycle of the hooks
	hookCtx, hookCtxCancel := context.WithCancel(hookCtx)
	hookCtx = context.WithValue(hookCtx, models.ErrGroupKey, hookErrGrp)

	// create a new error group for the proxy
	proxyErrGrp, _ := errgroup.WithContext(ctx)
	proxyCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the proxyCtx to control the lifecycle of the proxy
	proxyCtx, proxyCtxCancel := context.WithCancel(proxyCtx)
	proxyCtx = context.WithValue(proxyCtx, models.ErrGroupKey, proxyErrGrp)

	g.Go(func() error {
		<-ctx.Done()

		proxyCtxCancel()
		err = proxyErrGrp.Wait()
		if err != nil {
			utils.LogError(c.logger, err, "failed to stop the proxy")
		}

		hookCtxCancel()
		err := hookErrGrp.Wait()
		if err != nil {
			utils.LogError(c.logger, err, "failed to unload the hooks")
		}

		// reset the hosts config in nsswitch.conf of the system (in test mode)
		if opts.Mode == models.MODE_TEST && c.hostConfigStr != "" {
			err := c.resetNsSwitchConfig()
			if err != nil {
				utils.LogError(c.logger, err, "")
			}
		}
		return nil
	})

	//load hooks
	err = c.Hooks.Load(hookCtx, id, HookCfg{
		AppID:      id,
		Pid:        0,
		IsDocker:   isDocker,
		KeployIPV4: a.KeployIPv4Addr(),
	})
	if err != nil {
		utils.LogError(c.logger, err, "failed to load hooks")
		return hookErr
	}

	if c.proxyStarted {
		c.logger.Debug("Proxy already started")
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// TODO: Hooks can be loaded multiple times but proxy should be started only once
	// if there is another containerized app, then we need to pass new (ip:port) of proxy to the eBPF
	// as the network namespace is different for each container and so is the keploy/proxy IP to communicate with the app.
	// start proxy
	err = c.Proxy.StartProxy(proxyCtx, ProxyOptions{
		DNSIPv4Addr: a.KeployIPv4Addr(),
		//DnsIPv6Addr: ""
	})
	if err != nil {
		utils.LogError(c.logger, err, "failed to start proxy")
		return hookErr
	}

	c.proxyStarted = true

	if opts.Mode == models.MODE_TEST {
		// setting up the dns routing in test mode (helpful in fedora distro)
		err = c.setupNsswitchConfig()
		if err != nil {
			return err
		}
	}

	// For keploy test bench
	if opts.EnableTesting {
		c.logger.Info("🧪 setting up environment for testing keploy with itself")
		// enable testing in the app
		a.EnableTesting = true
		a.Mode = opts.Mode

		if opts.Mode == models.MODE_TEST {
			err := c.setUpReplayTesting(ctx)
			if err != nil {
				return err
			}
			return nil
		}

		err := c.setUpRecordTesting(ctx)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Core) Run(ctx context.Context, id uint64, _ models.RunOptions) models.AppError {
	a, err := c.getApp(id)
	if err != nil {
		utils.LogError(c.logger, err, "failed to get app")
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	}

	runAppErrGrp, runAppCtx := errgroup.WithContext(ctx)

	inodeErrCh := make(chan error, 1)
	appErrCh := make(chan models.AppError, 1)
	inodeChan := make(chan uint64, 1) //send inode to the hook

	defer func() {
		err := runAppErrGrp.Wait()
		defer close(inodeErrCh)
		defer close(inodeChan)
		if err != nil {
			utils.LogError(c.logger, err, "failed to stop the app")
		}
	}()

	runAppErrGrp.Go(func() error {
		defer utils.Recover(c.logger)
		if a.Kind(ctx) == utils.Native {
			return nil
		}
		select {
		case inode := <-inodeChan:
			err := c.Hooks.SendInode(ctx, id, inode)
			if err != nil {
				utils.LogError(c.logger, err, "")

				inodeErrCh <- errors.New("failed to send inode to the kernel")
			}
		case <-ctx.Done():
			return nil
		}
		return nil
	})

	runAppErrGrp.Go(func() error {
		defer utils.Recover(c.logger)
		defer close(appErrCh)
		appErr := a.Run(runAppCtx, inodeChan)
		if appErr.Err != nil {
			utils.LogError(c.logger, appErr.Err, "error while running the app")
			appErrCh <- appErr
		}
		return nil
	})

	select {
	case <-runAppCtx.Done():
		return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: nil}
	case appErr := <-appErrCh:
		return appErr
	case inodeErr := <-inodeErrCh:
		return models.AppError{AppErrorType: models.ErrInternal, Err: inodeErr}
	}
}

func (c *Core) GetAppIP(_ context.Context, id uint64) (string, error) {

	a, err := c.getApp(id)
	if err != nil {
		utils.LogError(c.logger, err, "failed to get app")
		return "", err
	}

	return a.ContainerIPv4Addr(), nil
}

// setting up the dns routing for the linux system
func (c *Core) setupNsswitchConfig() error {
	nsSwitchConfig := "/etc/nsswitch.conf"

	// Check if the nsswitch.conf present for the system
	if _, err := os.Stat(nsSwitchConfig); err == nil {
		// Read the current nsswitch.conf
		data, err := os.ReadFile(nsSwitchConfig)
		if err != nil {
			utils.LogError(c.logger, err, "failed to read the nsswitch.conf file from system")
			return errors.New("failed to setup the nsswitch.conf file to redirect the DNS queries to proxy")
		}

		// Replace the hosts field value if it exists
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "hosts:") {
				c.hostConfigStr = lines[i]
				lines[i] = "hosts: files dns"
			}
		}

		// Write the modified nsswitch.conf back to the file
		err = os.WriteFile("/etc/nsswitch.conf", []byte(strings.Join(lines, "\n")), 0644)
		if err != nil {
			utils.LogError(c.logger, err, "failed to write the configuration to the nsswitch.conf file to redirect the DNS queries to proxy")
			return errors.New("failed to setup the nsswitch.conf file to redirect the DNS queries to proxy")
		}

		c.logger.Debug("Successfully written to nsswitch config of linux")
	}
	return nil
}

// resetNsSwitchConfig resets the hosts config of nsswitch of the system
func (c *Core) resetNsSwitchConfig() error {
	nsSwitchConfig := "/etc/nsswitch.conf"
	data, err := os.ReadFile(nsSwitchConfig)
	if err != nil {
		c.logger.Error("failed to read the nsswitch.conf file from system", zap.Error(err))
		return errors.New("failed to reset the nsswitch.conf back to the original state")
	}

	// Replace the hosts field value if it exists with the actual system hosts value
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "hosts:") {
			lines[i] = c.hostConfigStr
		}
	}

	// Write the modified nsswitch.conf back to the file
	err = os.WriteFile(nsSwitchConfig, []byte(strings.Join(lines, "\n")), 0644)
	if err != nil {
		c.logger.Error("failed to write the configuration to the nsswitch.conf file to redirect the DNS queries to proxy", zap.Error(err))
		return errors.New("failed to reset the nsswitch.conf back to the original state")
	}

	c.logger.Debug("Successfully reset the nsswitch config of linux")
	return nil
}

const (
	keployTestPort   = 56789
	keployRecordPort = 36789
)

func (c *Core) setUpReplayTesting(ctx context.Context) error {
	setUpErr := errors.New("failed to setup the keploy replay testing")

	keployRecordPid, err := utils.GetPIDByPort(ctx, c.logger, keployRecordPort)
	if err != nil {
		c.logger.Error("failed to get the keployRecord pid", zap.Error(err))
		utils.LogError(c.logger, err, "failed to get the keployRecord pid from port", zap.Any("port", keployRecordPort))
		return setUpErr
	}
	c.logger.Debug(fmt.Sprintf("keployRecord pid:%v", keployRecordPid))

	err = c.TransmitTestBenchKeployPIDs(0, keployRecordPid)
	if err != nil {
		return setUpErr
	}

	err = c.TransmitTestBenchKeployPorts(0, uint32(keployRecordPort))
	if err != nil {
		return setUpErr
	}

	err = c.TransmitTestBenchKeployPorts(1, uint32(keployTestPort))
	if err != nil {
		return setUpErr
	}

	// to get the pid of keployTest binary in keployRecord binary, we have to wait for some time till the proxy server is started
	// TODO: find other way to filter child process (keployTest) pid in parent process binary (keployRecord)
	time.Sleep(30 * time.Second) // just for test bench.

	return nil
}

func (c *Core) setUpRecordTesting(ctx context.Context) error {

	go func() {
		timeout := 30 * time.Second
		startTime := time.Now()

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				keployTestPid, err := utils.GetPIDByPort(ctx, c.logger, keployTestPort)
				if err != nil {
					c.logger.Debug("failed to get the keploytest pid", zap.Error(err))
					continue
				}

				if keployTestPid == 0 {
					continue
				}

				c.logger.Debug("keploytest pid", zap.Uint32("pid", keployTestPid))

				// sending keploytest binary pid in keployrecord binary to filter out ingress/egress calls related to keploytest binary.
				_ = c.Hooks.TransmitTestBenchKeployPIDs(1, keployTestPid)

				return

			case <-time.After(timeout - time.Since(startTime)):
				c.logger.Debug("Timeout reached, exiting loop from setupRecordTesting")
				return // Exit the goroutine

			case <-ctx.Done():
				c.logger.Debug("Context cancelled, exiting loop from setupRecordTesting")
				return // Exit the goroutine
			}
		}
	}()

	return nil
}
