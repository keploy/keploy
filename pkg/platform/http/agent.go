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
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// TODO: Need to refactor this file
type AgentClient struct {
	logger       *zap.Logger
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	apps         sync.Map
	client       http.Client
	conf         *config.Config
	agentCmd     *exec.Cmd // Track the agent process
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
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					// End of the stream
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

	return tcChan, nil
}

func (a *AgentClient) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
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
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					// End of the stream
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

	return mockChan, nil
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

	// Open the log file (truncate to start fresh)
	filepath := "keploy_agent.log"
	logFile, err := os.OpenFile(filepath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		utils.LogError(a.logger, err, "failed to open log file")
		return err
	}

	keployBin, err := utils.GetCurrentBinaryPath()
	if err != nil {
		if logFile != nil {
			logFileCloseErr := logFile.Close()
			if logFileCloseErr != nil {
				utils.LogError(a.logger, logFileCloseErr, "failed to close log file")
			}

			utils.LogError(a.logger, err, "failed to get current keploy binary path")
		}
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

	if a.conf.Debug {
		args = append(args, "--debug")
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
	if len(opts.PassThroughPorts) > 0 {
		// Convert []uint32 to []string
		portStrings := make([]string, len(opts.PassThroughPorts))
		for i, port := range opts.PassThroughPorts {
			portStrings[i] = strconv.Itoa(int(port))
		}
		// Join them with a comma and add as a single argument
		args = append(args, "--pass-through-ports", strings.Join(portStrings, ","))
	}
	a.logger.Info("Starting native agent with args", zap.Strings("args", args))

	if opts.ConfigPath != "" && opts.ConfigPath != "." {
		args = append(args, "--config-path", opts.ConfigPath)
	}

	// Create OS-appropriate command (handles sudo/process-group on Unix; plain on Windows)
	cmd := agentUtils.NewAgentCommand(keployBin, args)

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	a.mu.Lock()
	a.agentCmd = cmd // this has been set for proper stopping of the native agent
	a.mu.Unlock()
	// Start (OS-specific tweaks happen inside utils.StartCommand)
	if err := agentUtils.StartCommand(cmd); err != nil {
		if logFile != nil {
			_ = logFile.Close()
			utils.LogError(a.logger, err, "failed to start keploy agent")
		}
		return err
	}

	pid := cmd.Process.Pid
	a.logger.Info("keploy agent started", zap.Int("pid", pid))

	grp.Go(func() error {
		defer utils.Recover(a.logger)
		defer logFile.Close()

		err := cmd.Wait()
		// If ctx wasn't cancelled, bubble up unexpected exits
		if err != nil && ctx.Err() == nil {
			a.logger.Error("agent process exited with error", zap.Error(err))
			return err
		}
		a.mu.Lock()
		a.agentCmd = nil
		a.mu.Unlock()
		a.logger.Info("agent process stopped")
		return nil
	})

	grp.Go(func() error {
		defer utils.Recover(a.logger)
		<-ctx.Done()
		if stopErr := agentUtils.StopCommand(cmd, a.logger); stopErr != nil {
			utils.LogError(a.logger, stopErr, "failed to stop keploy agent")
		}
		return nil
	})

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
		a.logger.Info("Stopping keploy agent", zap.Int("pid", a.agentCmd.Process.Pid))
		err := a.agentCmd.Process.Kill()
		if err != nil {
			utils.LogError(a.logger, err, "failed to kill keploy agent process")
		} else {
			a.logger.Info("Keploy agent process killed successfully")
		}
		a.agentCmd = nil
	}
}

// monitorAgent monitors the agent process and handles cleanup
func (a *AgentClient) monitorAgent(clientCtx context.Context, agentCtx context.Context) {
	select {
	case <-clientCtx.Done():
		// Client context cancelled, stop the agent
		a.logger.Info("Client context cancelled, stopping agent")
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

	a.logger.Info("Using available ports",
		zap.Uint32("agent-port", agentPort),
		zap.Uint32("proxy-port", proxyPort),
		zap.Uint32("dns-port", dnsPort))

	if isDockerCmd {

		a.logger.Info("Application command provided :", zap.String("cmd", cmd))

		opts.KeployContainer = agentUtils.GenerateRandomContainerName(a.logger, "keploy-v3-")
		a.conf.KeployContainer = opts.KeployContainer

		var appPorts, appNetworks []string
		cmd, appPorts, appNetworks = agentUtils.ExtractDockerFlags(cmd)

		opts.AppPorts = appPorts
		if len(appNetworks) > 0 {
			opts.AppNetworks = appNetworks
			a.logger.Debug("Found docker networks", zap.Strings("networks", opts.AppNetworks))
		}

		a.logger.Info("Application command to execute :", zap.String("cmd", cmd))
	}

	if opts.CommandType != string(utils.DockerCompose) { // in case of docker compose, we will run the application command (our agent will run along with it)
		opts.ClientNSPID = uint32(os.Getpid())
		err = a.startAgent(ctx, isDockerCmd, opts)
		if err != nil {
			return fmt.Errorf("failed to start agent: %w", err)
		}
		a.logger.Info("Agent is now running, proceeding with setup")

		agentCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, a.conf.Agent.AgentURI, agentReadyCh, 1*time.Second)

		select {
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

	a.logger.Info("Client setup completed successfully")
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
		logger.Info("Context cancelled. Explicitly stopping the 'keploy-v3' Docker container.")

		containerName := opts.KeployContainer

		args := []string{"docker", "stop", opts.KeployContainer}
		var stopCmd *exec.Cmd

		// Conditionally add "sudo" only for Linux
		if runtime.GOOS == "linux" {
			stopCmd = exec.Command("sudo", args...)
		} else {
			stopCmd = exec.Command(args[0], args[1:]...)
		}

		if output, err := stopCmd.CombinedOutput(); err != nil {
			logger.Warn("Could not stop the docker container. It may have already stopped.",
				zap.String("container", containerName),
				zap.Error(err),
				zap.String("output", string(output)))
		} else {
			logger.Info("Successfully sent stop command to the container.", zap.String("container", containerName))
		}

		if cmd.Process != nil {
			return utils.SendSignal(logger, cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	logger.Info("running the following command to start agent in docker", zap.String("command", cmd.String()))

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

// This function should be implemented such that we listen to the mock not found errors on the proxy side and send it back to the client from agent
// Currently, we are sending the nil chan and it is handled for the nil check in the monitorProxyErrors function
func (a *AgentClient) GetErrorChannel() <-chan error {
	return nil
}

func (a *AgentClient) MakeAgentReadyForDockerCompose(ctx context.Context) error {

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/agent/ready", a.conf.Agent.AgentURI), nil)
	if err != nil {
		return err
	}

	_, err = a.client.Do(req)
	if err != nil {
		return err
	}

	return nil
}
