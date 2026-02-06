package mockreplay

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func (r *replayer) mockReplay(ctx context.Context, opts models.ReplayOptions) (*models.ReplayResult, error) {
	if r.runtime == nil {
		return nil, errors.New("mock replay runtime is not configured")
	}
	mockDB := r.runtime.MockDB()
	if mockDB == nil {
		return nil, errors.New("mock database is not configured")
	}
	beforeTime := time.Now()
	rootFiltered, err := mockDB.GetFilteredMocks(ctx, "", models.BaseTime, beforeTime)
	if err != nil {
		return nil, fmt.Errorf("failed to load root filtered mocks: %w", err)
	}
	rootUnfiltered, err := mockDB.GetUnFilteredMocks(ctx, "", models.BaseTime, beforeTime)
	if err != nil {
		return nil, fmt.Errorf("failed to load root config mocks: %w", err)
	}
	var mocks []*models.Mock
	mocks = append(mocks, rootFiltered...)
	mocks = append(mocks, rootUnfiltered...)
	if len(mocks) > 0 {
		r.logger.Info("Loaded root mocks before session start", zap.Int("mockCount", len(mocks)))
	}

	command, commandType, cleanup, err := r.prepareMockReplayConfig(opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}

	return r.runMockReplay(ctx, command, commandType, rootFiltered, rootUnfiltered, mocks, opts)
}

func (r *replayer) prepareMockReplayConfig(opts models.ReplayOptions) (string, string, func(), error) {
	if r.cfg == nil {
		return "", "", nil, errors.New("mock replay config is not available")
	}

	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return "", "", nil, errors.New("command is required")
	}
	commandType := string(utils.FindDockerCmd(command))
	if commandType == "" {
		commandType = r.cfg.CommandType
	}

	oldCommand := r.cfg.Command
	oldCommandType := r.cfg.CommandType
	oldProxyPort := r.cfg.ProxyPort
	oldDNSPort := r.cfg.DNSPort
	oldContainerName := r.cfg.ContainerName
	containerNameChanged := false

	r.cfg.Command = command
	r.cfg.CommandType = commandType
	if opts.ProxyPort != 0 {
		r.cfg.ProxyPort = opts.ProxyPort
	}
	if opts.DNSPort != 0 {
		r.cfg.DNSPort = opts.DNSPort
	}
	if r.cfg.ContainerName == "" {
		if inferred := inferContainerName(command, utils.CmdType(commandType)); inferred != "" {
			r.cfg.ContainerName = inferred
			containerNameChanged = true
		}
	}

	cleanup := func() {
		r.cfg.Command = oldCommand
		r.cfg.CommandType = oldCommandType
		r.cfg.ProxyPort = oldProxyPort
		r.cfg.DNSPort = oldDNSPort
		if containerNameChanged {
			r.cfg.ContainerName = oldContainerName
		}
	}

	if utils.CmdType(commandType) == utils.DockerCompose && r.cfg.ContainerName == "" {
		return command, commandType, cleanup, errors.New("missing required container name for docker compose command")
	}
	return command, commandType, cleanup, nil
}

func (r *replayer) runMockReplay(ctx context.Context, command, commandType string, rootFiltered []*models.Mock, rootUnfiltered []*models.Mock, mocks []*models.Mock, opts models.ReplayOptions) (*models.ReplayResult, error) {
	if r.runtime == nil {
		return nil, errors.New("mock replay runtime is not configured")
	}
	instrumentation := r.runtime.Instrumentation()
	if instrumentation == nil {
		return nil, errors.New("instrumentation is not configured")
	}

	replayCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		replayCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	} else {
		replayCtx, cancel = context.WithCancel(ctx)
	}
	errGrp, grpCtx := errgroup.WithContext(replayCtx)
	grpCtx = context.WithValue(grpCtx, models.ErrGroupKey, errGrp)
	defer func() {
		cancel()
		if err := errGrp.Wait(); err != nil {
			utils.LogError(r.logger, err, "failed to stop mock replay")
		}
	}()

	passPortsUint := config.GetByPassPorts(r.cfg)
	err := instrumentation.Setup(grpCtx, command, models.SetupOptions{
		Container:         r.cfg.ContainerName,
		CommandType:       commandType,
		DockerDelay:       r.cfg.BuildDelay,
		Mode:              models.MODE_TEST,
		BuildDelay:        r.cfg.BuildDelay,
		EnableTesting:     true,
		GlobalPassthrough: r.cfg.Record.GlobalPassthrough,
		ConfigPath:        r.cfg.ConfigPath,
		Path:              r.cfg.Path,
		PassThroughPorts:  passPortsUint,
	})
	if err != nil {
		return nil, fmt.Errorf("failed setting up the environment: %w", err)
	}

	cmdType := utils.CmdType(commandType)
	appErrCh := make(chan models.AppError, 1)

	if cmdType == utils.DockerCompose {
		go func() {
			appErrCh <- instrumentation.Run(grpCtx, models.RunOptions{})
		}()

		agentCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, r.logger, string(r.cfg.Agent.AgentURI), agentReadyCh, 1*time.Second)

		select {
		case <-agentCtx.Done():
			return nil, fmt.Errorf("keploy-agent did not become ready in time")
		case <-agentReadyCh:
		}
	}

	if runtime.GOOS == "windows" {
		type incomingProxyStarter interface {
			GetIncoming(ctx context.Context, opts models.IncomingOptions) (<-chan *models.TestCase, error)
		}
		if starter, ok := instrumentation.(incomingProxyStarter); ok {
			go func() {
				incomingCh, err := starter.GetIncoming(grpCtx, models.IncomingOptions{
					Filters: r.cfg.Record.Filters,
				})
				if err != nil {
					r.logger.Debug("failed to start incoming proxy for mock replay", zap.Error(err))
					return
				}
				for range incomingCh {
				}
			}()
		}
	}

	err = instrumentation.MockOutgoing(grpCtx, models.OutgoingOptions{
		Rules:          r.cfg.BypassRules,
		MongoPassword:  r.cfg.Test.MongoPassword,
		SQLDelay:       time.Duration(r.cfg.Test.Delay) * time.Second,
		FallBackOnMiss: opts.FallBackOnMiss,
		Mocking:        true,
	})
	if err != nil {
		return nil, err
	}

	if err := instrumentation.StoreMocks(grpCtx, rootFiltered, rootUnfiltered); err != nil {
		return nil, err
	}
	pkg.InitSortCounter(int64(max(len(rootFiltered), len(rootUnfiltered))))
	if err := instrumentation.UpdateMockParams(grpCtx, models.MockFilterParams{
		AfterTime:  models.BaseTime,
		BeforeTime: time.Now(),
	}); err != nil {
		return nil, err
	}

	r.logger.Info("Mock replay will load mocks on start-session; ensure your tests call /agent/hooks/start-session before any outbound calls")

	if cmdType == utils.DockerCompose {
		err = instrumentation.MakeAgentReadyForDockerCompose(grpCtx)
		if err != nil {
			utils.LogError(r.logger, err, "failed to make the request to make agent ready for the docker compose")
		}
	}

	var appErr models.AppError
	if cmdType == utils.DockerCompose {
		select {
		case appErr = <-appErrCh:
		case <-grpCtx.Done():
			appErr = models.AppError{AppErrorType: models.ErrCtxCanceled, Err: grpCtx.Err()}
		}
	} else {
		appErr = instrumentation.Run(grpCtx, models.RunOptions{})
	}

	switch appErr.AppErrorType {
	case models.ErrCommandError, models.ErrInternal:
		return nil, appErr
	case models.ErrCtxCanceled:
		if appErr.Err != nil {
			return nil, appErr.Err
		}
		return nil, grpCtx.Err()
	}

	consumed, err := instrumentation.GetConsumedMocks(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}

	appExitCode := exitCodeFromAppError(appErr)
	mocksReplayed := len(consumed)
	mocksMissed := len(mocks) - mocksReplayed
	if mocksMissed < 0 {
		mocksMissed = 0
	}

	success := appExitCode == 0
	return &models.ReplayResult{
		Success:       success,
		MocksReplayed: mocksReplayed,
		MocksMissed:   mocksMissed,
		AppExitCode:   appExitCode,
		Output:        "",
		ConsumedMocks: consumed,
	}, nil
}

func exitCodeFromAppError(appErr models.AppError) int {
	if appErr.Err == nil {
		return 0
	}

	if exitErr, ok := appErr.Err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 1
}

func inferContainerName(command string, cmdType utils.CmdType) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}

	switch cmdType {
	case utils.DockerStart:
		re := regexp.MustCompile(`\bstart\s+(?:-[^\s]+\s+)*([^\s]+)`)
		matches := re.FindStringSubmatch(command)
		if len(matches) >= 2 {
			return matches[1]
		}
	case utils.DockerRun:
		re := regexp.MustCompile(`--name\s+([^\s]+)`)
		matches := re.FindStringSubmatch(command)
		if len(matches) >= 2 {
			return matches[1]
		}
	}

	return ""
}
