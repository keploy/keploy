//go:build linux

package http

// Instrumentation package client code,

// send the payload to the server
// enable http chunking/straeming for large payloads

// setup ki call jo agent start karte hi hogi - it will return nothing.
// docker k liye alag se setup hoga (can setup via agent flag)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/models"
	kdocker "go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// agent will implement
type AgentClient struct {
	logger       *zap.Logger
	core.Proxy                  // embedding the Proxy interface to transfer the proxy methods to the core object
	core.Hooks                  // embedding the Hooks interface to transfer the hooks methods to the core object
	core.Tester                 // embedding the Tester interface to transfer the tester methods to the core object
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	id           utils.AutoInc
	apps         sync.Map
	proxyStarted bool
	client       http.Client
}

// this will be the client side implementation
func New(logger *zap.Logger, hook core.Hooks, proxy core.Proxy, tester core.Tester, client kdocker.Client) *AgentClient {
	return &AgentClient{
		logger:       logger,
		Hooks:        hook,
		Proxy:        proxy,
		Tester:       tester,
		dockerClient: client,
		client:       http.Client{},
	}
}

// Listeners will get activated, details will be stored in the map. And connection will be established
func (a *AgentClient) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	requestBody := models.IncomingReq{
		IncomingOptions: opts,
		AppId:           0,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for incoming request")
		return nil, fmt.Errorf("error marshaling request body for incoming request: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:8086/agent/incoming", bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for incoming request")
		return nil, fmt.Errorf("error creating request for incoming request: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}

	// Make the HTTP request
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get incoming: %s", err.Error())
	}

	// Ensure response body is closed when we're done
	go func() {
		<-ctx.Done()
		if res.Body != nil {
			_ = res.Body.Close()
		}
	}()

	// Create a channel to stream TestCase data
	tcChan := make(chan *models.TestCase)

	go func() {
		defer close(tcChan)
		defer func() {
			err := res.Body.Close()
			if err != nil {
				utils.LogError(a.logger, err, "failed to close response body for incoming request")
			}
		}()

		decoder := json.NewDecoder(res.Body)
		fmt.Println("Starting to read from the response body")
		// Read from the response body as a stream
		// have to prevent it from reading
		// print the response buffer -
		// resp, err := io.ReadAll(res.Body)
		// if err != nil {
		// 	fmt.Println("Error reading response body")
		// }
		// fmt.Println("Response body: ", string(resp))

		for {
			var testCase models.TestCase
			if err := decoder.Decode(&testCase); err != nil {
				if err == io.EOF {
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
		AppId:           0,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return nil, fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:8086/agent/outgoing", bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
		return nil, fmt.Errorf("error creating request for mock outgoing: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}

	// Make the HTTP request
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to authenticate: %s", err.Error())
	}

	// Create a channel to stream Mock data
	mockChan := make(chan *models.Mock)

	go func() {
		defer close(mockChan)
		defer func() {
			err := res.Body.Close()
			if err != nil {
				utils.LogError(a.logger, err, "failed to close response body for mock outgoing")
			}
		}()

		decoder := json.NewDecoder(res.Body)

		// Read from the response body as a stream
		for {
			var mock models.Mock
			if err := decoder.Decode(&mock); err != nil {
				if err == io.EOF {
					// End of the stream
					break
				}
				utils.LogError(a.logger, err, "failed to decode mock from stream")
				break
			}

			select {
			case <-ctx.Done():
				// If the context is done, exit the loop
				return
			case mockChan <- &mock:
				// Send the decoded mock to the channel
			}
		}
	}()

	return mockChan, nil
}

func (a *AgentClient) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {
	// make a request to the server to mock outgoing
	requestBody := models.OutgoingReq{
		OutgoingOptions: opts,
		AppId:           0,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	// mock outgoing request
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:8086/agent/mock", bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
		return fmt.Errorf("error creating request for mock outgoing: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}

	// Make the HTTP request
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}

	resp, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body for mock outgoing: %s", err.Error())
	}
	fmt.Println("Response body: ", string(resp))

	return nil

}

func (a *AgentClient) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	requestBody := models.SetMocksReq{
		Filtered:   filtered,
		UnFiltered: unFiltered,
		AppId:      0,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	// mock outgoing request
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:8086/agent/setmocks", bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
		return fmt.Errorf("error creating request for mock outgoing: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}

	// Make the HTTP request
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}

	resp, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body for mock outgoing: %s", err.Error())
	}
	fmt.Println("Response body: ", string(resp))

	return nil
}

func (a *AgentClient) GetConsumedMocks(ctx context.Context, id uint64) ([]string, error) {
	// Create the URL with query parameters
	url := fmt.Sprintf("http://localhost:8086/agent/consumedmocks?id=%d", id)

	// Create a new GET request with the query parameter
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %s", err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}
	defer res.Body.Close()

	var consumedMocks []string
	err = json.NewDecoder(res.Body).Decode(&consumedMocks)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response body: %s", err.Error())
	}

	return consumedMocks, nil
}

func (a *AgentClient) UnHook(ctx context.Context, id uint64) error {
	return nil
}

func (a *AgentClient) GetContainerIP(_ context.Context, id uint64) (string, error) {

	app, err := a.getApp(id)
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

func (a *AgentClient) Run(ctx context.Context, id uint64, _ models.RunOptions) models.AppError {

	app, err := a.getApp(id)
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

func (a *AgentClient) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	// check if the agent is running
	isAgentRunning := false

	// if the agent is not running, start the agent
	clientId := utils.GenerateID()
	clientId = 0 // how can I retrieve the same client Id in the testmode ??

	isDockerCmd := utils.IsDockerCmd(utils.CmdType(opts.CommandType))

	if isDockerCmd {
		opts.IsDocker = true
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost:8086/agent/health", nil)
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
	}

	resp, err := a.client.Do(req)
	if err != nil {
		a.logger.Info("Keploy agent is not running in background, starting the agent")
	} else {
		isAgentRunning = true
		a.logger.Info("Setup request sent to the server", zap.String("status", resp.Status))
	}

	if !isAgentRunning {
		// Start the keploy agent as a detached process and pipe the logs into a file
		if isDockerCmd {
			// run the docker container instead of the agent binary
			go func() {
				if err := a.StartInDocker(ctx, a.logger, clientId); err != nil {
					a.logger.Error("failed to start docker agent", zap.Error(err))
				}
			}()
		} else {
			// Open the log file in append mode or create it if it doesn't exist
			logFile, err := os.OpenFile("keploy_agent.log", os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				utils.LogError(a.logger, err, "failed to open log file")
				return 0, err
			}
			defer logFile.Close()

			agentCmd := exec.Command("sudo", "oss", "agent")
			agentCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // Detach the process

			// Redirect the standard output and error to the log file
			agentCmd.Stdout = logFile
			agentCmd.Stderr = logFile
			agentCmd.Stdin = os.Stdin

			err = agentCmd.Start()
			if err != nil {
				utils.LogError(a.logger, err, "failed to start keploy agent")
				return 0, err
			}

			time.Sleep(1 * time.Second)
			a.logger.Info("keploy agent started", zap.Any("pid", agentCmd.Process.Pid))
		}
	}

	time.Sleep(10 * time.Second)
	// check if its docker then create a init container first
	// and then the app container
	usrApp := app.NewApp(a.logger, clientId, cmd, a.dockerClient, app.Options{
		DockerNetwork: opts.DockerNetwork,
		Container:     opts.Container,
		DockerDelay:   opts.DockerDelay,
	})
	a.apps.Store(clientId, usrApp)

	err = usrApp.Setup(ctx)
	if err != nil {
		utils.LogError(a.logger, err, "failed to setup app")
		return 0, err
	}

	var inode uint64
	if isDockerCmd {

		inode, err = a.Initcontainer(ctx, a.logger, app.Options{
			DockerNetwork: opts.DockerNetwork,
			Container:     opts.Container,
			DockerDelay:   opts.DockerDelay,
		})
		if err != nil {
			utils.LogError(a.logger, err, "failed to setup init container")
			return 0, err
		}
	}

	opts.ClientId = clientId
	opts.AppInode = inode // why its required in case of native ?
	// Register the client with the server
	err = a.RegisterClient(ctx, opts)
	if err != nil {
		utils.LogError(a.logger, err, "failed to register client")
		return 0, err
	}

	return clientId, nil
}

// Doubt: where should I place this getApp method ?
func (ag *AgentClient) getApp(id uint64) (*app.App, error) {
	a, ok := ag.apps.Load(id)
	if !ok {
		fmt.Printf("app with id:%v not found", id)
		return nil, fmt.Errorf("app with id:%v not found", id)
	}

	// type assertion on the app
	h, ok := a.(*app.App)
	if !ok {
		return nil, fmt.Errorf("failed to type assert app with id:%v", id)
	}

	return h, nil
}

func (a *AgentClient) RegisterClient(ctx context.Context, opts models.SetupOptions) error {
	// Register the client with the server
	clientPid := uint32(os.Getpid())

	// start the app container and get the inode number
	// keploy agent would have already runnning,

	inode, err := hooks.GetSelfInodeNumber()
	if err != nil {
		a.logger.Error("failed to get inode number")
	}

	// Register the client with the server
	requestBody := models.RegisterReq{
		SetupOptions: models.SetupOptions{
			DockerNetwork: opts.DockerNetwork,
			ClientNsPid:   clientPid,
			Mode:          opts.Mode,
			ClientId:      0,
			ClientInode:   inode,
			IsDocker:      opts.IsDocker,
			AppInode:      opts.AppInode,
		},
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for register client")
		return fmt.Errorf("error marshaling request body for register client: %s", err.Error())
	}

	resp, err := a.client.Post("http://localhost:8086/agent/register", "application/json", bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to send register client request")
		return fmt.Errorf("error sending register client request: %s", err.Error())
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to register client: %s", resp.Status)
	}

	// TODO: Read the response body in which we return the app id
	var RegisterResp models.RegisterResp
	err = json.NewDecoder(resp.Body).Decode(&RegisterResp)
	if err != nil {
		utils.LogError(a.logger, err, "failed to decode response body for register client")
		return fmt.Errorf("error decoding response body for register client: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost:8086/agent/health", nil)
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
	}

	resp, err = a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}

	return nil
}

func (a *AgentClient) StartInDocker(ctx context.Context, logger *zap.Logger, clientId uint64) error {

	// Start the keploy agent in a Docker container
	agentCtx := context.WithoutCancel(context.Background())

	err := kdocker.StartInDocker(agentCtx, logger, &config.Config{
		InstallationID: "1234", // TODO: CHANGE IT TO ACTUAL INSTALLATION ID
	})
	if err != nil {
		utils.LogError(logger, err, "failed to start keploy agent in docker")
		return err
	}
	return nil
}

func (a *AgentClient) Initcontainer(ctx context.Context, logger *zap.Logger, opts app.Options) (uint64, error) {

	// Initialize Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		a.logger.Error("failed to initialize Docker client", zap.Error(err))
		return 0, err
	}

	fmt.Println("Docker client initialized", opts.DockerNetwork)

	// Define container and network names
	containerName := "keploy-init"
	networkName := "keploy-network"

	// Create network if it does not exist
	networks, err := cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		a.logger.Error("failed to list networks", zap.Error(err))
		return 0, err
	}

	networkExists := false
	for _, net := range networks {
		if net.Name == networkName {
			networkExists = true
			break
		}
	}

	if !networkExists {
		_, err := cli.NetworkCreate(ctx, networkName, network.CreateOptions{})
		if err != nil {
			a.logger.Error("failed to create network", zap.Error(err))
			return 0, err
		}
		a.logger.Info("Network created", zap.String("network_name", networkName))
	}

	// Create container with custom name and network settings
	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: "alpine",
		Cmd:   []string{"sleep", "infinity"},
		Tty:   false,
	}, &container.HostConfig{
		NetworkMode: container.NetworkMode(networkName),
	}, nil, nil, containerName)
	if err != nil {
		a.logger.Error("failed to create init container", zap.Error(err))
		return 0, err
	}

	// Start the container
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		a.logger.Error("failed to start init container", zap.Error(err))
		return 0, err
	}

	a.logger.Info("Container started", zap.String("container_id", resp.ID))

	// Get the PID of the container's first process
	inspect, err := cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		a.logger.Error("failed to inspect container", zap.Error(err))
		return 0, err
	}

	pid := inspect.State.Pid
	a.logger.Info("Container PID", zap.Int("pid", pid))

	// Extract inode from the PID namespace
	pidNamespaceInode, err := kdocker.ExtractPidNamespaceInode(pid)
	if err != nil {
		a.logger.Error("failed to extract PID namespace inode", zap.Error(err))
		return 0, err
	}

	a.logger.Info("PID Namespace Inode", zap.String("inode", pidNamespaceInode))
	iNode, err := strconv.ParseUint(pidNamespaceInode, 10, 64)
	return iNode, nil
}
