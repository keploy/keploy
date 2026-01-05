package mockreplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	mockdb "go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	yamldoc "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type replayer struct {
	logger *zap.Logger
	cfg    *config.Config
	agent  AgentService
	mockDB MockDB
}

// New creates a new mock replay service.
func New(logger *zap.Logger, cfg *config.Config, agent AgentService, mockDB MockDB) Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &replayer{
		logger: logger,
		cfg:    cfg,
		agent:  agent,
		mockDB: mockDB,
	}
}

// Replay loads mocks and replays them while running the provided command.
func (r *replayer) Replay(ctx context.Context, opts models.ReplayOptions) (*models.ReplayResult, error) {
	if r.agent == nil {
		return nil, errors.New("agent service is not configured")
	}

	if strings.TrimSpace(opts.Command) == "" {
		return nil, errors.New("command is required")
	}
	if strings.TrimSpace(opts.MockFilePath) == "" {
		return nil, errors.New("mock file path is required")
	}

	mockPath, err := resolveMockFilePath(opts.MockFilePath)
	if err != nil {
		return nil, err
	}

	var mocks []*models.Mock
	if r.mockDB != nil {
		mocks, err = r.mockDB.LoadMocks(ctx, mockPath)
	} else {
		mocks, err = r.loadMocksFromFile(mockPath)
	}
	if err != nil {
		return nil, err
	}

	r.prepareAgentConfig(opts)

	replayCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		replayCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	} else {
		replayCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	startCh := make(chan int, 1)
	setupErrCh := make(chan error, 1)
	go func() {
		setupErrCh <- r.agent.Setup(replayCtx, startCh)
	}()

	select {
	case <-startCh:
	case err := <-setupErrCh:
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("agent setup stopped before it was ready")
		}
		return nil, err
	case <-replayCtx.Done():
		return nil, replayCtx.Err()
	}

	if err := r.agent.MockOutgoing(replayCtx, r.outgoingOptions(opts)); err != nil {
		return nil, err
	}
	if err := r.agent.SetMocks(replayCtx, mocks, nil); err != nil {
		return nil, err
	}

	output, exitCode, cmdErr := runCommand(replayCtx, opts.Command)
	if cmdErr != nil {
		return nil, cmdErr
	}

	consumed, err := r.agent.GetConsumedMocks(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}

	cancel()
	if err := <-setupErrCh; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		r.logger.Warn("agent setup stopped with error", zap.Error(err))
	}

	mocksReplayed := len(consumed)
	mocksMissed := len(mocks) - mocksReplayed
	if mocksMissed < 0 {
		mocksMissed = 0
	}

	success := exitCode == 0 && mocksMissed == 0
	return &models.ReplayResult{
		Success:       success,
		MocksReplayed: mocksReplayed,
		MocksMissed:   mocksMissed,
		AppExitCode:   exitCode,
		Output:        output,
		ConsumedMocks: consumed,
	}, nil
}

func (r *replayer) prepareAgentConfig(opts models.ReplayOptions) {
	if r.cfg == nil {
		return
	}

	r.cfg.Agent.Mode = models.MODE_TEST

	if opts.ProxyPort != 0 {
		r.cfg.Agent.ProxyPort = opts.ProxyPort
	} else if r.cfg.Agent.ProxyPort == 0 {
		r.cfg.Agent.ProxyPort = r.cfg.ProxyPort
	}

	if opts.DNSPort != 0 {
		r.cfg.Agent.DnsPort = opts.DNSPort
	} else if r.cfg.Agent.DnsPort == 0 {
		r.cfg.Agent.DnsPort = r.cfg.DNSPort
	}

	if r.cfg.Agent.ProxyPort == 0 {
		r.cfg.Agent.ProxyPort = 16789
	}
	if r.cfg.Agent.DnsPort == 0 {
		r.cfg.Agent.DnsPort = 26789
	}
	// if r.cfg.Agent.ClientNSPID == 0 {
	// 	r.cfg.Agent.ClientNSPID = uint32(utils.GetCurrentProcessGroupID())
	// }

	if len(r.cfg.Agent.PassThroughPorts) == 0 && len(r.cfg.BypassRules) > 0 {
		r.cfg.Agent.PassThroughPorts = config.GetByPassPorts(r.cfg)
	}

	if !r.cfg.Agent.IsDocker {
		if utils.IsDockerCmd(utils.FindDockerCmd(opts.Command)) {
			r.cfg.Agent.IsDocker = true
		}
	}
}

func (r *replayer) outgoingOptions(opts models.ReplayOptions) models.OutgoingOptions {
	base := models.OutgoingOptions{
		FallBackOnMiss: opts.FallBackOnMiss,
		Mocking:        true,
	}

	if r.cfg == nil {
		return base
	}

	delay := time.Duration(r.cfg.Test.Delay) * time.Second
	base.Rules = r.cfg.BypassRules
	base.MongoPassword = r.cfg.Test.MongoPassword
	base.SQLDelay = delay
	return base
}

func (r *replayer) loadMocksFromFile(filePath string) ([]*models.Mock, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	dec := yamlLib.NewDecoder(bytes.NewReader(data))
	var docs []*yamldoc.NetworkTrafficDoc
	for {
		var doc *yamldoc.NetworkTrafficDoc
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}

	if len(docs) == 0 {
		return []*models.Mock{}, nil
	}

	return mockdb.DecodeMocks(docs, r.logger)
}

func resolveMockFilePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("mock file path is required")
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		candidate := filepath.Join(path, "mocks.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		candidate = filepath.Join(path, "mocks.yml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		return "", fmt.Errorf("mocks.yaml not found in %s", path)
	}
	return path, nil
}

func runCommand(ctx context.Context, command string) (string, int, error) {
	cmd := buildCommand(ctx, command)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		return "", -1, err
	}

	err := cmd.Wait()
	outStr := output.String()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return outStr, -1, ctxErr
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return outStr, exitErr.ExitCode(), nil
		}
		return outStr, -1, err
	}
	return outStr, 0, nil
}

func buildCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}
