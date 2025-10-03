// Package http contains the client side code to communicate with the agent server
package http

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed" // necessary for embedding
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	ptls "go.keploy.io/server/v2/pkg/agent/proxy/tls"
	"go.keploy.io/server/v2/pkg/client/app"
	"go.keploy.io/server/v2/pkg/models"
	kdocker "go.keploy.io/server/v2/pkg/platform/docker"
	agentUtils "go.keploy.io/server/v2/pkg/platform/http/utils"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

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

func (a *AgentClient) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	requestBody := models.IncomingReq{
		IncomingOptions: opts,
		ClientID:        id,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for incoming request")
		return nil, fmt.Errorf("error marshaling request body for incoming request: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://localhost:%d/agent/incoming", a.conf.Agent.Port), bytes.NewBuffer(requestJSON))
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
			fmt.Println("Closing the test case channel")
			close(tcChan)
		}()
		defer func() {
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

func (a *AgentClient) GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	requestBody := models.OutgoingReq{
		OutgoingOptions: opts,
		ClientID:        id,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return nil, fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://localhost:%d/agent/outgoing", a.conf.Agent.Port), bytes.NewBuffer(requestJSON))
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
			fmt.Println("closing the mock channel")
			close(mockChan)
		}()
		defer func() {
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

func (a *AgentClient) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {

	fmt.Println("Inside MockOutgoing of agent client")

	// make a request to the server to mock outgoing
	requestBody := models.OutgoingReq{
		OutgoingOptions: opts,
		ClientID:        id,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	// mock outgoing request
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://localhost:%d/agent/mock", a.conf.Agent.Port), bytes.NewBuffer(requestJSON))
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

func (a *AgentClient) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	requestBody := models.SetMocksReq{
		Filtered:   filtered,
		UnFiltered: unFiltered,
		ClientID:   id,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for setmocks")
		return fmt.Errorf("error marshaling request body for setmocks: %s", err.Error())
	}

	// mock outgoing request
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://localhost:%d/agent/setmocks", a.conf.Agent.Port), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for setmocks outgoing")
		return fmt.Errorf("error creating request for set mocks: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request for setmocks: %s", err.Error())
	}

	var mockResp models.AgentResp
	err = json.NewDecoder(res.Body).Decode(&mockResp)
	if err != nil {
		return fmt.Errorf("failed to decode response body for setmocks: %s", err.Error())
	}

	if mockResp.Error != nil {
		return mockResp.Error
	}

	return nil
}

func (a *AgentClient) StoreMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	requestBody := models.StoreMocksReq{
		Filtered:   filtered,
		UnFiltered: unFiltered,
		ClientID:   id,
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
		fmt.Sprintf("http://localhost:%d/agent/storemocks", a.conf.Agent.Port),
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

func (a *AgentClient) UpdateMockParams(ctx context.Context, id uint64, params models.MockFilterParams) error {
	requestBody := models.UpdateMockParamsReq{
		ClientID:     id,
		FilterParams: params,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for updatemockparams")
		return fmt.Errorf("error marshaling request body for updatemockparams: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://localhost:%d/agent/updatemockparams", a.conf.Agent.Port), bytes.NewBuffer(requestJSON))
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

func (a *AgentClient) GetConsumedMocks(ctx context.Context, id uint64) ([]models.MockState, error) {
	// Create the URL with query parameters
	url := fmt.Sprintf("http://localhost:%d/agent/consumedmocks?id=%d", a.conf.Agent.Port, id)

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

func (a *AgentClient) GetContainerIP(_ context.Context, clientID uint64) (string, error) {

	app, err := a.getApp(clientID)
	if err != nil {
		utils.LogError(a.logger, err, "failed to get app")
		return "", err
	}

	ip := app.ContainerIPv4Addr()
	a.logger.Debug("ip address of the target app container", zap.Any("ip", ip))
	if ip == "" {
		return "", fmt.Errorf("failed to get the IP address of the app container. Try increasing --delay (in seconds)")
	}

	return ip, nil
}

// Creating a duplicate function to avoid breaking changes
func (a *AgentClient) GetContainerIP4(ctx context.Context, clientID uint64) (string, error) {

	app, err := a.getApp(clientID)
	if err != nil {
		utils.LogError(a.logger, err, "failed to get app")
		return "", err
	}

	a.logger.Info("Keploy container name", zap.String("container", app.GetKeployContainer()))

	inspect, err := a.dockerClient.ContainerInspect(ctx, app.GetKeployContainer())
	if err != nil {
		utils.LogError(a.logger, nil, fmt.Sprintf("failed to get inspect keploy container:%v", inspect))
		return "", err
	}
	var keployIPv4 string
	keployIPv4 = inspect.NetworkSettings.IPAddress

	// Check if the Networks map is not empty
	if len(inspect.NetworkSettings.Networks) > 0 && keployIPv4 == "" {
		// Iterate over the map to get the first available IP
		for _, network := range inspect.NetworkSettings.Networks {
			keployIPv4 = network.IPAddress
			if keployIPv4 != "" {
				break // Exit the loop once we've found an IP
			}
		}
	}

	return keployIPv4, nil
}

func (a *AgentClient) Run(ctx context.Context, clientID uint64, _ models.RunOptions) models.AppError {
	fmt.Println("Inside Run of agent binary !!.. ")
	app, err := a.getApp(clientID)
	if err != nil {
		utils.LogError(a.logger, err, "failed to get app while running")
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	}

	runAppErrGrp, runAppCtx := errgroup.WithContext(ctx)
	inodeErrCh := make(chan error, 1)
	appErrCh := make(chan models.AppError, 1)
	inodeChan := make(chan uint64, 1) //send inode to the hook
	defer func() {
		err := runAppErrGrp.Wait()
		defer close(inodeErrCh)
		if err != nil {
			utils.LogError(a.logger, err, "failed to stop the app")
		}
	}()

	runAppErrGrp.Go(func() error {
		defer utils.Recover(a.logger)
		defer close(appErrCh)
		appErr := app.Run(runAppCtx, inodeChan)
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
func (a *AgentClient) startAgent(ctx context.Context, clientID uint64, isDockerCmd bool, opts models.SetupOptions) error {
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
			if err := a.StartInDocker(agentCtx, a.logger, opts); err != nil && !errors.Is(agentCtx.Err(), context.Canceled) {
				a.logger.Error("failed to start Docker agent", zap.Error(err))
				return err
			}
			return nil
		})
	} else {
		// Start the agent as a native process
		err := a.startNativeAgent(agentCtx, clientID, opts)
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
func (a *AgentClient) startNativeAgent(ctx context.Context, clientID uint64, opts models.SetupOptions) error {

	// Get the errgroup from context
	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return fmt.Errorf("failed to get errorgroup from the context")
	}

	// Open the log file (truncate to start fresh)
	filepath := fmt.Sprintf("keploy_agent_%d.log", clientID)
	logFile, err := os.OpenFile(filepath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		utils.LogError(a.logger, err, "failed to open log file")
		return err
	}

	keployBin, err := utils.GetCurrentBinaryPath()
	if err != nil {
		if logFile != nil {
			_ = logFile.Close()
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
		"--client-nspid", strconv.Itoa(int(opts.ClientNsPid)),
		"--docker-network", opts.DockerNetwork,
		"--agent-ip", opts.AgentIP,
		"--mode", string(opts.Mode),
		"--app-inode", strconv.FormatUint(opts.AppInode, 10),
		"--debug",
	}

	if opts.EnableTesting {
		args = append(args, "--enable-testing")
	}
	if opts.GlobalPassthrough {
		args = append(args, "--global-passthrough")
	}

	// Create OS-appropriate command (handles sudo/process-group on Unix; plain on Windows)
	cmd := agentUtils.NewAgentCommand(keployBin, args)
	// Redirect output to log
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Keep a reference for other methods
	a.mu.Lock()
	a.agentCmd = cmd
	a.mu.Unlock()
	fmt.Println(cmd)
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

	// 1) Reaper: wait for process exit and close the log
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

	// 2) Cancellation watcher: on ctx cancel, terminate the agent (and children, per-OS)
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

func (a *AgentClient) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {

	clientID := uint64(0)
	isDockerCmd := utils.IsDockerCmd(utils.CmdType(opts.CommandType))
	opts.IsDocker = isDockerCmd

	// Now it will always starts it's own agent

	// Start the keploy agent as a detached process and pipe the logs into a file
	if !isDockerCmd && runtime.GOOS != "linux" {
		return 0, fmt.Errorf("operating system not supported for this feature")
	}

	agentPort, err := utils.GetAvailablePort()
	if err != nil {
		utils.LogError(a.logger, err, "failed to find available port for agent")
		return 0, err
	}

	// Check and allocate available ports for proxy and DNS
	proxyPort, dnsPort, err := utils.EnsureAvailablePorts(a.conf.ProxyPort, a.conf.DNSPort)
	if err != nil {
		utils.LogError(a.logger, err, "failed to ensure available ports for proxy and DNS")
		return 0, err
	}

	opts.AgentPort = agentPort
	opts.ProxyPort = proxyPort
	opts.DnsPort = dnsPort

	// Update the ports in the configuration
	a.conf.Agent.Port = agentPort
	a.conf.ProxyPort = proxyPort
	a.conf.DNSPort = dnsPort

	a.logger.Info("Using available ports",
		zap.Uint32("agent-port", agentPort),
		zap.Uint32("proxy-port", proxyPort),
		zap.Uint32("dns-port", dnsPort))

	if isDockerCmd {
		fmt.Println("HERE IS THE DOCKER COMMAND :", cmd)
		randomBytes := make([]byte, 2)
		// Read cryptographically secure random bytes.
		if _, err := rand.Read(randomBytes); err != nil {
			// Handle the error appropriately in your application.
			log.Fatal("Failed to generate random part for container name:", err)
		}
		// Encode the 2 bytes into a 4-character hexadecimal string.
		uuidSuffix := hex.EncodeToString(randomBytes)

		// Append the random string.
		opts.KeployContainer = "keploy-v2-" + uuidSuffix
		a.conf.KeployContainer = opts.KeployContainer

		fmt.Println("ORIGINAL DOCKER COMMAND:", cmd)

		// Regex to find all port mapping flags (-p or --publish)
		portRegex := regexp.MustCompile(`\s+(-p|--publish)\s+[^\s]+`)

		// Find all port arguments in the command string
		portArgs := portRegex.FindAllString(cmd, -1)

		// Clean and store the extracted port arguments in opts
		cleanedPorts := []string{}
		for _, p := range portArgs {
			cleanedPorts = append(cleanedPorts, strings.TrimSpace(p))
		}
		opts.AppPorts = cleanedPorts // Store the extracted ports

		// Remove the port arguments from the original command string
		cmd = portRegex.ReplaceAllString(cmd, "")

		networkRegex := regexp.MustCompile(`(--network|--net)\s+([^\s]+)`)

		// Find the first match and its submatches (the captured group).
		networkMatches := networkRegex.FindStringSubmatch(cmd)

		if len(networkMatches) > 2 {
			opts.AppNetwork = networkMatches[2] // Store the extracted network name (the 2nd capture group)
			fmt.Println("FOUND APP NETWORK:", opts.AppNetwork)

			// Remove the network argument from the original command string
			cmd = networkRegex.ReplaceAllString(cmd, "")
		}
		fmt.Println("COMMAND AFTER REMOVING PORTS:", cmd)

		fmt.Println("HERE IS THE KEPLOY CONTAINER : ", opts.KeployContainer)
		// inspect, err := a.dockerClient.ContainerInspect(ctx, opts.KeployContainer)
		// if err != nil {
		// 	utils.LogError(a.logger, nil, fmt.Sprintf("failed to get inspect keploy container:%v", inspect))
		// 	return 0, err
		// }
		// var keployIPv4 string
		// keployIPv4 = inspect.NetworkSettings.IPAddress

		// // Check if the Networks map is not empty
		// if len(inspect.NetworkSettings.Networks) > 0 && keployIPv4 == "" {
		// 	// Iterate over the map to get the first available IP
		// 	for _, network := range inspect.NetworkSettings.Networks {
		// 		keployIPv4 = network.IPAddress
		// 		if keployIPv4 != "" {
		// 			break // Exit the loop once we've found an IP
		// 		}
		// 	}
		// }

		// pkg.AgentIP = keployIPv4
		// fmt.Println("here is the agent's IP address in client :", keployIPv4)
		// opts.AgentIP = keployIPv4
	}
	opts.ClientID = clientID
	if opts.CommandType != "docker-compose" {
		// Start the agent process
		err = a.startAgent(ctx, clientID, isDockerCmd, opts)
		if err != nil {
			return 0, fmt.Errorf("failed to start agent: %w", err)
		}

		a.logger.Info("Agent is now running, proceeding with setup")

	}

	if utils.CmdType(opts.CommandType) != utils.DockerCompose {
		time.Sleep(10 * time.Second)
	}

	// a.waitForAgent(ctx, 3000)
	// Continue with app setup and registration as per normal flow
	usrApp := app.NewApp(a.logger, clientID, cmd, a.dockerClient, opts)
	a.apps.Store(clientID, usrApp)

	// Set up cleanup on failure
	defer func() {
		if err != nil {
			a.logger.Info("Setup failed, cleaning up agent")
			a.stopAgent()
		}
	}()

	err = ptls.SetupCaCertEnv(a.logger)
	if err != nil {
		utils.LogError(a.logger, err, "failed to set TLS environment")
		return 0, err
	}

	err = usrApp.Setup(ctx)
	if err != nil {
		utils.LogError(a.logger, err, "failed to setup app")
		return 0, err
	}

	if isDockerCmd && opts.CommandType != "docker-compose" {
		fmt.Println("HERE IS THE KEPLOY CONTAINER : ", opts.KeployContainer)
		inspect, err := a.dockerClient.ContainerInspect(ctx, opts.KeployContainer)
		if err != nil {
			utils.LogError(a.logger, nil, fmt.Sprintf("failed to get inspect keploy container:%v", inspect))
			return 0, err
		}
		var keployIPv4 string
		keployIPv4 = inspect.NetworkSettings.IPAddress

		// Check if the Networks map is not empty
		if len(inspect.NetworkSettings.Networks) > 0 && keployIPv4 == "" {
			// Iterate over the map to get the first available IP
			for _, network := range inspect.NetworkSettings.Networks {
				keployIPv4 = network.IPAddress
				if keployIPv4 != "" {
					break // Exit the loop once we've found an IP
				}
			}
		}

		pkg.AgentIP = keployIPv4
		fmt.Println("here is the agent's IP address in client :", keployIPv4)
		opts.AgentIP = keployIPv4
	}
	a.logger.Info("Client setup completed successfully", zap.Uint64("clientID", clientID))
	return clientID, nil
}

func (a *AgentClient) getApp(clientID uint64) (*app.App, error) {
	ap, ok := a.apps.Load(clientID)
	if !ok {
		return nil, fmt.Errorf("app with id:%v not found", clientID)
	}

	// type assertion on the app
	h, ok := ap.(*app.App)
	if !ok {
		return nil, fmt.Errorf("failed to type assert app with id:%v", clientID)
	}

	return h, nil
}

// RegisterClient registers the client with the server
func (a *AgentClient) RegisterClient(ctx context.Context, opts models.SetupOptions) error {

	isAgent := a.isAgentRunning(ctx)
	if !isAgent {
		return fmt.Errorf("keploy agent is not running, please start the agent first")
	}

	// Register the client with the server
	clientPid := uint32(os.Getpid())

	// start the app container and get the inode number
	// keploy agent would have already runnning,
	var inode uint64
	var err error

	// This is commented out becuase now we do not require the inode at the client side

	// if runtime.GOOS == "linux" {
	// 	// send the network info to the kernel
	// 	inode, err = linuxHooks.GetSelfInodeNumber()
	// 	if err != nil {
	// 		a.logger.Error("failed to get inode number")
	// 	}
	// }

	// Register the client with the server
	requestBody := models.RegisterReq{
		SetupOptions: models.SetupOptions{
			DockerNetwork: opts.DockerNetwork,
			AgentIP:       opts.AgentIP,
			ClientNsPid:   clientPid,
			Mode:          opts.Mode,
			ClientID:      opts.ClientID,
			ClientInode:   inode,
			IsDocker:      a.conf.Agent.IsDocker,
			AppInode:      opts.AppInode,
			ProxyPort:     a.conf.ProxyPort,
		},
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for register client")
		return fmt.Errorf("error marshaling request body for register client: %s", err.Error())
	}

	resp, err := a.client.Post(fmt.Sprintf("http://localhost:%d/agent/register", a.conf.Agent.Port), "application/json", bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to send register client request")
		return fmt.Errorf("error sending register client request: %s", err.Error())
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to register client: %s", resp.Status)
	}

	a.logger.Info("Client registered successfully with clientId", zap.Uint64("clientID", opts.ClientID))

	// TODO: Read the response body in which we return the app id
	var RegisterResp models.AgentResp
	err = json.NewDecoder(resp.Body).Decode(&RegisterResp)
	if err != nil {
		utils.LogError(a.logger, err, "failed to decode response body for register client")
		return fmt.Errorf("error decoding response body for register client: %s", err.Error())
	}

	if RegisterResp.Error != nil {
		return RegisterResp.Error
	}

	return nil
}

func (a *AgentClient) UnregisterClient(_ context.Context, unregister models.UnregisterReq) error {
	// Unregister the client with the server
	isAgentRunning := a.isAgentRunning(context.Background())
	if !isAgentRunning {
		a.logger.Warn("keploy agent is not running, skipping unregister client")
		return io.EOF
	}

	fmt.Println("Unregistering the client with clientID:", unregister.ClientID)
	requestJSON, err := json.Marshal(unregister)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for unregister client")
		return fmt.Errorf("error marshaling request body for unregister client: %s", err.Error())
	}

	// Passed background context as we dont want to cancel the unregister request upon client ctx cancellation
	req, err := http.NewRequestWithContext(context.Background(), "POST", fmt.Sprintf("http://localhost:%d/agent/unregister", a.conf.Agent.Port), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for unregister client")
		return fmt.Errorf("error creating request for unregister client: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil && err != io.EOF {
		return fmt.Errorf("failed to send request for unregister client: %s", err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to unregister client: %s", resp.Status)
	}

	return nil
}

// In platform/http/agent.go
// In platform/http/agent.go

func (a *AgentClient) StartInDocker(ctx context.Context, logger *zap.Logger, opts models.SetupOptions) error {

	fmt.Println("Starting the keploy agent in docker container....")

	// Step 1: Prepare the Docker environment and get the command components.
	// We delegate all Docker-specific setup to the new helper function.
	keployAlias, err := kdocker.GetDockerCommandAndSetup(ctx, logger, &config.Config{
		InstallationID: a.conf.InstallationID,
	}, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to prepare docker command and environment")
		return err
	}

	cmd := kdocker.PrepareDockerCommand(ctx, keployAlias)
	// Step 4: Define the cancellation behavior for graceful shutdown.
	cmd.Cancel = func() error {
		logger.Info("Context cancelled. Explicitly stopping the 'keploy-v2' Docker container.")

		// Define the container name that you set in the getAlias function.
		containerName := opts.KeployContainer

		// Create a new, separate command to stop the container.
		// We use "sudo" here because the original run command also used it.
		stopCmd := exec.Command("sudo", "docker", "stop", containerName)

		// Execute the stop command. We use CombinedOutput to capture any errors.
		if output, err := stopCmd.CombinedOutput(); err != nil {
			logger.Warn("Could not stop the docker container. It may have already stopped.",
				zap.String("container", containerName),
				zap.Error(err),
				zap.String("output", string(output)))
		} else {
			logger.Info("Successfully sent stop command to the container.", zap.String("container", containerName))
		}

		// Finally, forcefully kill the original `sh` process to ensure cleanup.
		// The container is already stopping gracefully via the command above.
		if cmd.Process != nil {
			return utils.SendSignal(logger, cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	// Step 5: Redirect output to the console, as in the original function.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 👈 Step 6: This is the critical fix. Store the command so stopAgent can find it.
	a.mu.Lock()
	a.agentCmd = cmd
	a.mu.Unlock()

	logger.Info("running the following command to start agent in docker", zap.String("command", cmd.String()))

	// Step 7: Run the command. This blocks until the command exits or is cancelled.
	if err := cmd.Run(); err != nil {
		// A "context canceled" error is expected on normal shutdown, so we don't treat it as a failure.
		if ctx.Err() == context.Canceled {
			cmd.Process.Kill()
			logger.Info("Docker agent run cancelled gracefully.")
			a.mu.Lock()
			a.agentCmd = nil
			a.mu.Unlock()
			return nil
		}
		utils.LogError(logger, err, "failed to run keploy agent in docker")
		return err
	}

	return nil
}

func (a *AgentClient) isAgentRunning(ctx context.Context) bool {
	fmt.Println("chekcing on port :", a.conf.Agent.Port)
	clientPID := int32(os.Getpid())
	fmt.Println("SECOND CHECK ON CLIENT PID :", clientPID)
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://localhost:%d/agent/health", a.conf.Agent.Port), nil)
	if err != nil {
		utils.LogError(a.logger, err, "failed to send request to the agent server")
		return false
	}

	resp, err := a.client.Do(req)
	if err != nil {
		a.logger.Debug("Keploy agent health check failed", zap.Error(err))
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		a.logger.Debug("Agent health check returned non-OK status", zap.String("status", resp.Status))
		return false
	}

	a.logger.Debug("Agent health check successful", zap.String("status", resp.Status))
	return true
}

// waitForAgent waits for the agent to become available within the timeout period
func (a *AgentClient) waitForAgent(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for agent to become ready: %w", ctx.Err())
		case <-ticker.C:
			if a.isAgentRunning(ctx) {
				return nil
			}
		}
	}
}

func (a *AgentClient) GetHookUnloadDone(id uint64) <-chan struct{} {
	ch := make(chan struct{})
	close(ch) // Immediately close since no actual hooks are loaded
	return ch
}

func (a *AgentClient) GetErrorChannel() <-chan error {
	return nil
}

// func FindAvailableContainerName(ctx context.Context, cli *kdocker.Client, logger *zap.Logger, baseName string) (string, error) {
// 	// 1. First, check if the base name itself is available.
// 	_, err := cli.ContainerInspect(ctx, baseName)
// 	if err != nil {
// 		if errdefs.IsNotFound(err) {
// 			logger.Info("Base container name is available", zap.String("name", baseName))
// 			// The container was not found, so the name is available.
// 			return baseName, nil
// 		}
// 		// For any other error (e.g., Docker daemon not running), return it.
// 		return "", fmt.Errorf("failed to inspect container '%s': %w", baseName, err)
// 	}

// 	// 2. If the base name is taken (no error from Inspect), start looping.
// 	logger.Warn("Base container name is already in use, finding an alternative.", zap.String("name", baseName))

// 	// Limit attempts to prevent an infinite loop in an edge case.
// 	const maxAttempts = 100
// 	for i := 1; i <= maxAttempts; i++ {
// 		candidateName := fmt.Sprintf("%s-%d", baseName, i)

// 		_, err := cli.ContainerInspect(ctx, candidateName)
// 		if err != nil {
// 			if errdefs.IsNotFound(err) {
// 				logger.Info("Found available container name.", zap.String("name", candidateName))
// 				// Success! This name is not in use.
// 				return candidateName, nil
// 			}
// 			// Another error occurred.
// 			return "", fmt.Errorf("failed to inspect container '%s': %w", candidateName, err)
// 		}
// 		// If err is nil, this name is also taken. The loop will try the next number.
// 	}

// 	// If the loop finishes, we've failed to find a name after all attempts.
// 	return "", fmt.Errorf("could not find an available name for base '%s' after %d attempts", baseName, maxAttempts)
// }
