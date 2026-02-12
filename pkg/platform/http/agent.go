// Package http contains the client side code to communicate with the agent server
package http

import (
	"bytes"
	"context"
	_ "embed" // necessary for embedding
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	ptls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	agentUtils "go.keploy.io/server/v3/pkg/platform/http/utils"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	agentReadyTimeout       = 2 * time.Minute
	agentReadyRetryInterval = 2 * time.Second
)

// TODO: Need to refactor this file
type AgentClient struct {
	logger       *zap.Logger
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	apps         sync.Map
	client       http.Client
	conf         *config.Config
	agentCmd     *exec.Cmd             // Track the agent process
	agentPTY     *agentUtils.PTYHandle // Track the PTY handle for interactive commands
	mu           sync.Mutex
	agentCancel  context.CancelFunc // Function to cancel the agent context
}

// var initStopScript []byte

func New(logger *zap.Logger, client kdocker.Client, c *config.Config) *AgentClient {

	return &AgentClient{
		logger:       logger,
		dockerClient: client,
		client:       http.Client{},
		conf:         c,
	}
}

func (a *AgentClient) GetIncoming(ctx context.Context, opts models.IncomingOptions) (<-chan *models.TestCase, error) {

	a.logger.Debug("Connecting to incoming test cases stream...")

	requestBody := models.IncomingReq{
		IncomingOptions: opts,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for incoming request")
		return nil, fmt.Errorf("error marshaling request body for incoming request: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/incoming", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for incoming request")
		return nil, fmt.Errorf("error creating request for incoming request: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get incoming: %s", err.Error())
	}

	// Ensure response body is closed when we're done
	go func() {
		<-ctx.Done()
		if res.Body != nil {
			err = res.Body.Close()
			if err != nil {
				utils.LogError(a.logger, err, "failed to close response body for incoming request")
			}
		}
	}()

	// Create a channel to stream TestCase data
	tcChan := make(chan *models.TestCase)

	go func() {
		defer func() {
			close(tcChan)

			err := res.Body.Close()
			if err != nil {
				utils.LogError(a.logger, err, "failed to close response body for incoming request")
			}
		}()

		decoder := json.NewDecoder(res.Body)

		for {
			var testCase models.TestCase
			if err := decoder.Decode(&testCase); err != nil {
				if utils.IsShutdownError(err) {
					// End of the stream or connection closed during shutdown
					break
				}
				utils.LogError(a.logger, err, "failed to decode test case from stream")
				break
			}

			select {
			case <-ctx.Done():
				// If the context is done, exit the loop
				return
			case tcChan <- &testCase:
				// Send the decoded test case to the channel
			}
		}
	}()

	a.logger.Debug("Successfully connected to incoming test cases stream.")
	return tcChan, nil
}

func (a *AgentClient) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {

	a.logger.Debug("Connecting to outgoing mocks stream...")

	requestBody := models.OutgoingReq{
		OutgoingOptions: opts,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return nil, fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/outgoing", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
		return nil, fmt.Errorf("error creating request for mock outgoing: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get outgoing response: %s", err.Error())
	}

	mockChan := make(chan *models.Mock)

	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return nil, fmt.Errorf("failed to get errorgroup from the context")
	}

	grp.Go(func() error {
		defer func() {
			close(mockChan)

			err := res.Body.Close()
			if err != nil {
				utils.LogError(a.logger, err, "failed to close response body for getoutgoing")
			}
		}()

		decoder := gob.NewDecoder(res.Body)

		for {
			var mock models.Mock
			if err := decoder.Decode(&mock); err != nil {
				if utils.IsShutdownError(err) {
					// End of the stream or connection closed during shutdown
					break
				}
				utils.LogError(a.logger, err, "failed to decode mock from stream")
				break
			}

			select {
			case <-ctx.Done():
				// If the context is done, exit the loop
				return nil
			case mockChan <- &mock:
				// Send the decoded mock to the channel
			}
		}
		return nil
	})

	a.logger.Debug("Successfully connected to outgoing mocks stream.")

	return mockChan, nil
}

func (a *AgentClient) GetMappings(ctx context.Context, opts models.IncomingOptions) (<-chan models.TestMockMapping, error) {

	a.logger.Debug("Connecting to mappings stream...")

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/mappings", a.conf.Agent.AgentURI), nil)
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mappings")
		return nil, fmt.Errorf("error creating request for mappings: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get mappings response: %s", err.Error())
	}

	mappingChan := make(chan models.TestMockMapping)

	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return nil, fmt.Errorf("failed to get errorgroup from the context")
	}

	grp.Go(func() error {
		defer func() {
			close(mappingChan)
			if err := res.Body.Close(); err != nil {
				utils.LogError(a.logger, err, "failed to close response body for getmappings")
			}
		}()

		decoder := json.NewDecoder(res.Body)

		for {
			var mapping models.TestMockMapping
			if err := decoder.Decode(&mapping); err != nil {
				if utils.IsShutdownError(err) {
					break
				}
				utils.LogError(a.logger, err, "failed to decode mapping from stream")
				break
			}

			select {
			case <-ctx.Done():
				return nil
			case mappingChan <- mapping:
			}
		}
		return nil
	})

	a.logger.Debug("Successfully connected to mappings stream.")
	return mappingChan, nil
}

func (a *AgentClient) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {

	// make a request to the server to mock outgoing
	requestBody := models.OutgoingReq{
		OutgoingOptions: opts,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	// mock outgoing request
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/mock", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
		return fmt.Errorf("error creating request for mock outgoing: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}

	var mockResp models.AgentResp
	err = json.NewDecoder(res.Body).Decode(&mockResp)
	if err != nil {
		return fmt.Errorf("failed to decode response body for mock outgoing: %s", err.Error())
	}

	if mockResp.Error != nil {
		return mockResp.Error
	}

	return nil

}

func (a *AgentClient) BeforeSimulate(ctx context.Context, timestamp *time.Time, testSetID string, tcName string) error {
	if timestamp == nil || timestamp.IsZero() {
		a.logger.Warn("Skipping agent hook: timestamp is zero or nil")
		return nil
	}

	requestBody := models.BeforeSimulateRequest{
		TimeStamp:    *timestamp,
		TestSetID:    testSetID,
		TestCaseName: tcName,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/before-simulate")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 50 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		a.logger.Warn("failed to call agent hook", zap.String("endpoint", "/hooks/before-simulate"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil
}

func (a *AgentClient) AfterSimulate(ctx context.Context, tcName string, testSetID string) error {

	requestBody := models.AfterSimulateRequest{
		TestSetID:    testSetID,
		TestCaseName: tcName,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/after-simulate")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 50 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		a.logger.Warn("failed to call agent hook", zap.String("endpoint", "/hooks/after-simulate"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil
}

func (a *AgentClient) BeforeTestRun(ctx context.Context, testRunID string) error {

	requestBody := models.BeforeTestRunReq{
		TestRunID: testRunID,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/before-test-run")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 50 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		a.logger.Warn("failed to call agent hook", zap.String("endpoint", "/hooks/before-test-run"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil

}

func (a *AgentClient) BeforeTestSetCompose(ctx context.Context, testRunID string, firstRun bool) error {

	requestBody := models.BeforeTestSetCompose{
		TestRunID: testRunID,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/before-test-set-compose")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 50 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		a.logger.Warn("failed to call agent hook", zap.String("endpoint", "/hooks/before-test-set-compose"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil

}

func (a *AgentClient) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {

	requestBody := models.AfterTestRunReq{
		TestRunID:  testRunID,
		TestSetIDs: testSetIDs,
		Coverage:   coverage,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/after-test-run")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 50 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		a.logger.Warn("failed to call agent hook", zap.String("endpoint", "/hooks/after-test-run"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil

}
func (a *AgentClient) StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	requestBody := models.StoreMocksReq{
		Filtered:   filtered,
		UnFiltered: unFiltered,
	}

	// gob-encode the body
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(requestBody); err != nil {
		utils.LogError(a.logger, err, "failed to gob-encode request body for storemocks")
		return fmt.Errorf("gob encode request for storemocks: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/storemocks", a.conf.Agent.AgentURI),
		&buf,
	)
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for storemocks")
		return fmt.Errorf("create request for storemocks: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/x-gob")
	req.Header.Set("Accept", "application/x-gob")

	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request for storemocks: %s", err.Error())
	}
	defer res.Body.Close()

	// Non-200? Try to decode anyway; if that fails, return status text
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Best-effort decode; fall back to status if it fails
		var fail models.AgentResp
		if err := gob.NewDecoder(res.Body).Decode(&fail); err != nil {
			return fmt.Errorf("storemocks http %d", res.StatusCode)
		}
		if fail.Error != nil {
			return fail.Error
		}
		return fmt.Errorf("storemocks http %d", res.StatusCode)
	}

	var mockResp models.AgentResp
	if err := gob.NewDecoder(res.Body).Decode(&mockResp); err != nil {
		return fmt.Errorf("decode gob response for storemocks: %s", err.Error())
	}

	if mockResp.Error != nil {
		return mockResp.Error
	}
	return nil
}

func (a *AgentClient) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	requestBody := models.UpdateMockParamsReq{
		FilterParams: params,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for updatemockparams")
		return fmt.Errorf("error marshaling request body for updatemockparams: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/updatemockparams", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for updatemockparams")
		return fmt.Errorf("error creating request for update mock params: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request for updatemockparams: %s", err.Error())
	}

	var mockResp models.AgentResp
	err = json.NewDecoder(res.Body).Decode(&mockResp)
	if err != nil {
		return fmt.Errorf("failed to decode response body for updatemockparams: %s", err.Error())
	}

	if mockResp.Error != nil {
		return mockResp.Error
	}

	return nil
}

func (a *AgentClient) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	// Create the URL with query parameters
	url := fmt.Sprintf("%s/consumedmocks", a.conf.Agent.AgentURI)
	// Create a new GET request with the query parameter
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %s", err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}

	defer func() {
		err := res.Body.Close()
		if err != nil {
			utils.LogError(a.logger, err, "failed to close response body for getconsumedmocks")
		}
	}()

	var consumedMocks []models.MockState
	err = json.NewDecoder(res.Body).Decode(&consumedMocks)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response body: %s", err.Error())
	}

	return consumedMocks, nil
}

func (a *AgentClient) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	app, err := a.getApp()
	if err != nil {
		utils.LogError(a.logger, err, "failed to get app while running")
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	}

	runAppErrGrp, runAppCtx := errgroup.WithContext(ctx)
	appErrCh := make(chan models.AppError, 1)
	defer func() {
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(a.logger, err, "failed to stop the app")
		}
	}()

	runAppErrGrp.Go(func() error {
		defer utils.Recover(a.logger)
		defer close(appErrCh)
		appErr := app.Run(runAppCtx)
		if appErr.Err != nil {
			utils.LogError(a.logger, appErr.Err, "error while running the app")
			appErrCh <- appErr
		}
		return nil
	})

	select {
	case <-runAppCtx.Done():
		return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: nil}
	case appErr := <-appErrCh:
		return appErr
	}
}

// startAgent starts the keploy agent process and handles its lifecycle
func (a *AgentClient) startAgent(ctx context.Context, isDockerCmd bool, opts models.SetupOptions) error {
	// Get the errgroup from context
	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return fmt.Errorf("failed to get errorgroup from the context")
	}

	// Create a context for the agent that can be cancelled independently
	agentCtx, cancel := context.WithCancel(ctx)
	a.agentCancel = cancel
	if a.conf.Record.Synchronous {
		opts.Synchronous = true
	}
	opts.ExtraArgs = agent.StartupAgentHook.GetArgs(ctx)
	if isDockerCmd {
		// Start the agent in Docker container using errgroup
		grp.Go(func() error {
			defer cancel() // Cancel agent context when Docker agent stops
			if err := a.startInDocker(agentCtx, a.logger, opts); err != nil && !errors.Is(agentCtx.Err(), context.Canceled) {
				a.logger.Error("failed to start Docker agent", zap.Error(err))
				return err
			}
			return nil
		})
	} else {
		// Start the agent as a native process
		err := a.startNativeAgent(agentCtx, opts)
		if err != nil {
			cancel()
			return err
		}
	}

	// Monitor agent process and cancel client context if agent stops using errgroup
	grp.Go(func() error {
		a.monitorAgent(ctx, agentCtx)
		return nil
	})

	return nil
}

// startNativeAgent starts the keploy agent as a native process
func (a *AgentClient) startNativeAgent(ctx context.Context, opts models.SetupOptions) error {

	// Get the errgroup from context
	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return fmt.Errorf("failed to get errorgroup from the context")
	}

	keployBin, err := utils.GetCurrentBinaryPath()
	if err != nil {
		utils.LogError(a.logger, err, "failed to get current keploy binary path")
		return err
	}

	// Build args (binary is passed separately to utils)
	args := []string{
		"agent",
		"--port", strconv.Itoa(int(opts.AgentPort)),
		"--proxy-port", strconv.Itoa(int(opts.ProxyPort)),
		"--dns-port", strconv.Itoa(int(opts.DnsPort)),
		"--client-pid", strconv.Itoa(int(os.Getpid())),
		"--mode", string(opts.Mode),
	}

	extraArgs := opts.ExtraArgs
	if len(extraArgs) > 0 {
		args = append(args, extraArgs...)
	}
	if a.conf.Debug {
		args = append(args, "--debug")
	}
	if a.conf.Record.Synchronous {
		args = append(args, "--sync")
	}
	if opts.EnableTesting {
		args = append(args, "--enable-testing")
	}
	if opts.GlobalPassthrough {
		args = append(args, "--global-passthrough")
	}
	if opts.BuildDelay > 0 {
		args = append(args, "--build-delay", strconv.FormatUint(opts.BuildDelay, 10))
	}
	if models.IsAnsiDisabled == true {
		args = append(args, "--disable-ansi")
	}
	if len(opts.PassThroughPorts) > 0 {
		// Convert []uint32 to []string
		portStrings := make([]string, len(opts.PassThroughPorts))
		for i, port := range opts.PassThroughPorts {
			portStrings[i] = strconv.Itoa(int(port))
		}
		// Join them with a comma and add as a single argument
		args = append(args, "--pass-through-ports", strings.Join(portStrings, ","))
	}
	a.logger.Debug("Starting native agent with args", zap.Strings("args", args))

	if opts.ConfigPath != "" && opts.ConfigPath != "." {
		args = append(args, "--config-path", opts.ConfigPath)
	}

	// Check if sudo credentials are already cached (e.g., from permission fix)
	// If cached, we can use sudo -n (non-interactive) and skip PTY
	sudoCached := utils.AreSudoCredentialsCached()

	// Check if we need PTY for interactive input (e.g., sudo password)
	// Skip PTY if credentials are already cached - use non-interactive sudo instead
	if agentUtils.NeedsPTY() && !sudoCached {
		return a.startNativeAgentWithPTY(ctx, keployBin, args, grp)
	}

	// Create OS-appropriate command (handles sudo/process-group on Unix; plain on Windows)
	// If credentials are cached, this will use sudo -n (non-interactive)
	cmd := agentUtils.NewAgentCommand(keployBin, args, sudoCached)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	a.mu.Lock()
	a.agentCmd = cmd // this has been set for proper stopping of the native agent
	a.mu.Unlock()
	// Start (OS-specific tweaks happen inside utils.StartCommand)
	if err := agentUtils.StartCommand(cmd); err != nil {
		utils.LogError(a.logger, err, "failed to start keploy agent")
		return err
	}

	pid := cmd.Process.Pid
	a.logger.Debug("keploy agent started", zap.Int("pid", pid))

	grp.Go(func() error {
		defer utils.Recover(a.logger)

		err := cmd.Wait()
		// If ctx wasn't cancelled, bubble up unexpected exits
		if err != nil && ctx.Err() == nil {
			a.logger.Error("agent process exited with error", zap.Error(err))
			return err
		}
		a.mu.Lock()
		a.agentCmd = nil
		a.mu.Unlock()
		a.logger.Debug("agent process stopped")
		return nil
	})

	grp.Go(func() error {
		defer utils.Recover(a.logger)
		<-ctx.Done()
		if a.conf.Agent.AgentURI == "" {
			return nil
		}
		a.logger.Debug("Keploy agent shutdown requested", zap.Int("pid", pid))
		a.logger.Info("Stopping keploy agent")
		if err := a.requestAgentStop(); err != nil {
			a.logger.Debug("failed to request keploy agent shutdown, sending stop signal", zap.Error(err))
			// Fallback: forcefully stop the agent process
			a.mu.Lock()
			cmd := a.agentCmd
			a.mu.Unlock()
			if cmd != nil {
				if stopErr := agentUtils.StopCommand(cmd, a.logger); stopErr != nil {
					a.logger.Error("failed to forcefully stop agent", zap.Error(stopErr))
				}
			}
			return nil
		}
		a.logger.Info("Keploy agent stopped.")
		return nil
	})

	return nil
}

// startNativeAgentWithPTY starts the agent with PTY support for interactive input (e.g., sudo password)
func (a *AgentClient) startNativeAgentWithPTY(ctx context.Context, keployBin string, args []string, grp *errgroup.Group) error {
	// Create command configured for PTY
	cmd := agentUtils.NewAgentCommandForPTY(keployBin, args)

	a.logger.Debug("Starting native agent with PTY for interactive input")

	// Start with PTY
	ptyHandle, err := agentUtils.StartCommandWithPTY(cmd, a.logger)
	if err != nil {
		utils.LogError(a.logger, err, "failed to start keploy agent with PTY")
		return err
	}

	a.mu.Lock()
	a.agentCmd = cmd
	a.agentPTY = ptyHandle
	a.mu.Unlock()

	pid := cmd.Process.Pid
	a.logger.Debug("keploy agent started with PTY", zap.Int("pid", pid))

	grp.Go(func() error {
		defer utils.Recover(a.logger)

		err := ptyHandle.Wait()
		// If ctx wasn't cancelled, bubble up unexpected exits
		if err != nil && ctx.Err() == nil {
			a.logger.Error("agent process exited with error", zap.Error(err))
			return err
		}
		a.mu.Lock()
		a.agentCmd = nil
		a.agentPTY = nil
		a.mu.Unlock()
		a.logger.Debug("agent process stopped")
		return nil
	})

	grp.Go(func() error {
		defer utils.Recover(a.logger)
		<-ctx.Done()
		if a.conf.Agent.AgentURI == "" {
			return nil
		}
		a.logger.Debug("Keploy agent shutdown requested", zap.Int("pid", pid))
		a.logger.Info("Stopping keploy agent")
		if err := a.requestAgentStop(); err != nil {
			a.logger.Debug("failed to request keploy agent shutdown, sending stop signal", zap.Error(err))
			// Fallback: forcefully stop the agent process
			a.mu.Lock()
			ptyHandle := a.agentPTY
			a.mu.Unlock()
			if ptyHandle != nil {
				if stopErr := agentUtils.StopPTYCommand(ptyHandle, a.logger); stopErr != nil {
					a.logger.Error("failed to forcefully stop agent", zap.Error(stopErr))
				}
			}
			return nil
		}
		a.logger.Info("Keploy agent stopped.")
		return nil
	})

	return nil
}

func (a *AgentClient) requestAgentStop() error {
	if a.conf == nil {
		return fmt.Errorf("agent config is nil")
	}
	if a.conf.Agent.AgentURI == "" {
		return fmt.Errorf("agent URI is not configured")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(stopCtx, http.MethodPost, fmt.Sprintf("%s/stop", a.conf.Agent.AgentURI), nil)
	if err != nil {
		return err
	}

	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("agent stop http %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// stopAgent stops the agent process gracefully
func (a *AgentClient) stopAgent() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentCancel != nil {
		a.agentCancel()
		a.agentCancel = nil
	}

	if a.agentCmd != nil && a.agentCmd.Process != nil {
		pid := a.agentCmd.Process.Pid
		a.logger.Debug("Stopping keploy agent", zap.Int("pid", pid))
	}

	// If PTY is active, close it to unblock stdin/stdout copies
	if a.agentPTY != nil {
		// Use the utils function to gracefully stop PTY
		if err := agentUtils.StopPTYCommand(a.agentPTY, a.logger); err != nil {
			a.logger.Warn("failed to stop PTY command", zap.Error(err))
		}
	}
}

// monitorAgent monitors the agent process and handles cleanup
func (a *AgentClient) monitorAgent(clientCtx context.Context, agentCtx context.Context) {
	select {
	case <-clientCtx.Done():
		// Client context cancelled, stop the agent
		a.logger.Debug("Client context cancelled, stopping agent")
		a.stopAgent()
	case <-agentCtx.Done():
		// Agent context cancelled or agent stopped
		if errors.Is(agentCtx.Err(), context.Canceled) {
			a.logger.Info("Agent was stopped intentionally")
		} else {
			a.logger.Warn("Agent stopped unexpectedly, client operations may be affected")
		}
	}
}

func (a *AgentClient) Setup(ctx context.Context, cmd string, opts models.SetupOptions) error {
	isDockerCmd := utils.IsDockerCmd(utils.CmdType(opts.CommandType))
	opts.IsDocker = isDockerCmd

	agentPort, err := utils.GetAvailablePort()
	if err != nil {
		utils.LogError(a.logger, err, "failed to find available port for agent")
		return err
	}

	// Check and allocate available ports for proxy and DNS
	proxyPort, err := utils.EnsureAvailablePorts(a.conf.ProxyPort) // check if the proxy port provided by user is unused
	if err != nil {
		utils.LogError(a.logger, err, "failed to ensure available ports for proxy")
		return err
	}

	dnsPort, err := utils.EnsureAvailablePorts(a.conf.DNSPort) // check if the dns port provided by user is unused
	if err != nil {
		utils.LogError(a.logger, err, "failed to ensure available ports for DNS")
		return err
	}

	opts.AgentPort = agentPort
	opts.ProxyPort = proxyPort
	opts.DnsPort = dnsPort
	opts.AgentURI = fmt.Sprintf("http://localhost:%d/agent", agentPort)

	// Update the ports in the configuration
	a.conf.Agent.AgentPort = agentPort
	a.conf.Agent.ProxyPort = proxyPort
	a.conf.Agent.DnsPort = dnsPort
	a.conf.ProxyPort = proxyPort
	a.conf.DNSPort = dnsPort
	a.conf.Agent.AgentURI = opts.AgentURI

	a.logger.Debug("Using available ports",
		zap.Uint32("agent-port", agentPort),
		zap.Uint32("proxy-port", proxyPort),
		zap.Uint32("dns-port", dnsPort))

	if isDockerCmd {

		var origCmd = cmd
		a.logger.Debug("Application command provided :", zap.String("cmd", cmd))

		opts.KeployContainer = agentUtils.GenerateRandomContainerName(a.logger, "keploy-v3-")
		a.conf.KeployContainer = opts.KeployContainer

		var appPorts, appNetworks []string
		cmd, appPorts, appNetworks = agentUtils.ExtractDockerFlags(cmd)

		opts.AppPorts = appPorts
		if len(appNetworks) > 0 {
			opts.AppNetworks = appNetworks
			a.logger.Debug("Found docker networks", zap.Strings("networks", opts.AppNetworks))
		}

		if origCmd != cmd {
			a.logger.Info(
				"Updated user command to allow Keploy to serve traffic before the app",
				zap.String("cmd", cmd),
			)
		}
	}

	if opts.CommandType != string(utils.DockerCompose) { // in case of docker compose, we will run the application command (our agent will run along with it)
		opts.ClientNSPID = uint32(os.Getpid())
		err = a.startAgent(ctx, isDockerCmd, opts)
		if err != nil {
			return fmt.Errorf("failed to start agent: %w", err)
		}
		a.logger.Debug("Agent is now running, proceeding with setup")

		agentCtx, cancel := context.WithTimeout(ctx, 60*time.Second) // we will wait for 1 minute for the agent to get ready
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, a.logger, a.conf.Agent.AgentURI, agentReadyCh, 1*time.Second)

		select {
		case <-ctx.Done():
			// Parent context cancelled (user pressed Ctrl+C)
			return ctx.Err()
		case <-agentCtx.Done():
			return fmt.Errorf("keploy-agent did not become ready in time")
		case <-agentReadyCh:
		}
	}

	// Continue with app setup and registration as per normal flow
	usrApp := app.NewApp(a.logger, cmd, a.dockerClient, opts)
	a.apps.Store(uint64(0), usrApp) // key = 0, since there's only one client per agent

	// Set up cleanup on failure
	defer func() {
		if err != nil {
			a.logger.Info("Setup failed, cleaning up agent")
			a.stopAgent()
		}
	}()

	// TODO : Proxy or TLS should not be importes in the agent
	// This is done because to set env variable for TLS
	err = ptls.SetupCaCertEnv(a.logger)
	if err != nil {
		utils.LogError(a.logger, err, "failed to set TLS environment")
		return err
	}

	err = usrApp.Setup(ctx)
	if err != nil {
		utils.LogError(a.logger, err, "failed to setup app")
		return err
	}

	a.logger.Debug("Keploy client setup completed successfully")
	return nil
}

func (a *AgentClient) getApp() (*app.App, error) {
	ap, ok := a.apps.Load(uint64(0))
	if !ok {
		return nil, fmt.Errorf("app not found")
	}

	// type assertion on the app
	h, ok := ap.(*app.App)
	if !ok {
		return nil, fmt.Errorf("failed to type assert app")
	}

	return h, nil
}

func (a *AgentClient) startInDocker(ctx context.Context, logger *zap.Logger, opts models.SetupOptions) error {
	keployAlias, err := kdocker.GetKeployDockerAlias(ctx, logger, &config.Config{
		InstallationID: a.conf.InstallationID,
	}, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to prepare docker command and environment")
		return err
	}

	cmd := kdocker.PrepareDockerCommand(ctx, keployAlias)

	cmd.Cancel = func() error {
		logger.Debug("Context cancelled. Explicitly stopping the 'keploy-v3' Docker container.")

		containerName := opts.KeployContainer

		// Try stopping the container without sudo first (works if user is in docker group)
		stopCmd := exec.Command("docker", "stop", containerName)
		if output, err := stopCmd.CombinedOutput(); err != nil {
			// If that fails on Linux, try with sudo -n (non-interactive, won't prompt for password)
			if runtime.GOOS == "linux" {
				logger.Debug("docker stop without sudo failed, trying with sudo -n", zap.Error(err))
				stopCmd = exec.Command("sudo", "-n", "docker", "stop", containerName)
				if output, err := stopCmd.CombinedOutput(); err != nil {
					logger.Warn("Could not stop the docker container. It may have already stopped.",
						zap.String("container", containerName),
						zap.Error(err),
						zap.String("output", string(output)))
				} else {
					logger.Debug("Successfully sent stop command to the container.", zap.String("container", containerName))
				}
			} else {
				logger.Warn("Could not stop the docker container. It may have already stopped.",
					zap.String("container", containerName),
					zap.Error(err),
					zap.String("output", string(output)))
			}
		} else {
			logger.Debug("Successfully sent stop command to the container.", zap.String("container", containerName))
		}

		if cmd.Process != nil {
			return utils.SendSignal(logger, cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	logger.Debug("running the following command to start agent in docker", zap.String("command", cmd.String()))

	// Check if we need PTY for interactive input (e.g., sudo password on Linux)
	if agentUtils.NeedsPTY() {
		return a.startInDockerWithPTY(ctx, logger, cmd)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.Canceled {
			cmd.Process.Kill()
			logger.Info("Keploy agent in docker stopped gracefully.")
			return nil
		}
		utils.LogError(logger, err, "failed to run keploy agent in docker")
		return err
	}

	return nil
}

// startInDockerWithPTY starts the docker agent with PTY support for interactive input (e.g., sudo password)
func (a *AgentClient) startInDockerWithPTY(ctx context.Context, logger *zap.Logger, cmd *exec.Cmd) error {
	// Configure command for PTY execution (OS-specific)
	agentUtils.ConfigureCommandForPTY(cmd)

	logger.Debug("Starting docker agent with PTY for interactive input")

	// Start with PTY
	ptyHandle, err := agentUtils.StartCommandWithPTY(cmd, logger)
	if err != nil {
		utils.LogError(logger, err, "failed to start keploy agent in docker with PTY")
		return err
	}

	a.mu.Lock()
	a.agentCmd = cmd
	a.agentPTY = ptyHandle
	a.mu.Unlock()

	pid := cmd.Process.Pid
	logger.Debug("keploy agent in docker started with PTY", zap.Int("pid", pid))

	// Wait for the command to finish
	err = ptyHandle.Wait()

	a.mu.Lock()
	a.agentCmd = nil
	a.agentPTY = nil
	a.mu.Unlock()

	if err != nil {
		if ctx.Err() == context.Canceled {
			logger.Info("Keploy agent in docker stopped gracefully.")
			return nil
		}
		utils.LogError(logger, err, "failed to run keploy agent in docker with PTY")
		return err
	}

	return nil
}

// This function should be implemented such that we listen to the mock not found errors on the proxy side and send it back to the client from agent
// Currently, we are sending the nil chan and it is handled for the nil check in the monitorProxyErrors function
func (a *AgentClient) GetErrorChannel() <-chan error {
	return nil
}

// NotifyGracefulShutdown sends a request to the agent to set the graceful shutdown flag.
// This should be called before cancelling contexts during application shutdown.
// When the flag is set, connection errors will be logged as debug instead of error.
func (a *AgentClient) NotifyGracefulShutdown(ctx context.Context) error {
	if a.conf.Agent.AgentURI == "" {
		a.logger.Debug("Agent URI is empty, skipping graceful shutdown notification")
		return nil
	}

	url := fmt.Sprintf("%s/graceful-shutdown", a.conf.Agent.AgentURI)

	// Use a short timeout since this is a best-effort notification
	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", url, nil)
	if err != nil {
		a.logger.Debug("failed to create graceful shutdown request", zap.Error(err))
		return err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		// Don't log as error since this might fail during shutdown
		a.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		return err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		a.logger.Debug("agent returned non-200 status for graceful shutdown", zap.Int("status", resp.StatusCode))
		return fmt.Errorf("graceful shutdown notification failed with status %d", resp.StatusCode)
	}

	a.logger.Debug("Successfully notified agent of graceful shutdown")
	return nil
}

func (a *AgentClient) MakeAgentReadyForDockerCompose(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, agentReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(agentReadyRetryInterval)
	defer ticker.Stop()

	url := fmt.Sprintf("%s/agent/ready", a.conf.Agent.AgentURI)

	for {
		req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
		if err != nil {
			return err
		}

		resp, err := a.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				a.logger.Debug("Successfully marked agent as ready")
				return nil
			}
			a.logger.Warn("Agent returned non-200 status for ready check", zap.Int("status", resp.StatusCode))
		} else {
			a.logger.Debug("Failed to call agent ready endpoint, retrying...", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timeout waiting for agent to become ready")
			}
			return ctx.Err()
		case <-ticker.C:
			// retry
		}
	}
}
