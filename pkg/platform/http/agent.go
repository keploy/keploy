//go:build !windows

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

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/models"
	kdocker "go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type AgentClient struct {
	logger       *zap.Logger
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	id           utils.AutoInc
	apps         sync.Map
	proxyStarted bool
	client       http.Client
	conf         *config.Config
}

// this will be the client side implementation
func New(logger *zap.Logger, client kdocker.Client, c *config.Config) *AgentClient {
	fmt.Println("Agent client started::: ", c.Agent.Port, c.Agent.IsDocker)
	return &AgentClient{
		logger:       logger,
		dockerClient: client,
		client:       http.Client{},

		conf: c,
	}
}

// Listeners will get activated, details will be stored in the map. And connection will be established
func (a *AgentClient) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	requestBody := models.IncomingReq{
		IncomingOptions: opts,
		ClientId:        0,
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
		ClientId:        0,
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
		ClientId:        0,
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
		AppId:      0,
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

func (a *AgentClient) GetConsumedMocks(ctx context.Context, id uint64) ([]string, error) {
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

	// if the agent is not running, start the agent
	clientId := utils.GenerateID()
	clientId = 0 // how can I retrieve the same client Id in the testmode ??

	isDockerCmd := utils.IsDockerCmd(utils.CmdType(opts.CommandType))

	isAgentRunning := a.isAgentRunning(ctx)

	if !isAgentRunning {
		// Start the keploy agent as a detached process and pipe the logs into a file
		// if !isDockerCmd && !Linux {
		// 	return 0, fmt.Errorf("keploy agent is not running, please start the agent first")
		// }
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

			defer func() {
				err := logFile.Close()
				if err != nil {
					utils.LogError(a.logger, err, "failed to close agent log file")
				}
			}()

			agentCmd := exec.Command("sudo", "keployv2", "agent")
			agentCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Detach the process

			// Redirect the standard output and error to the log file
			agentCmd.Stdout = logFile
			agentCmd.Stderr = logFile
			agentCmd.Stdin = os.Stdin

			err = agentCmd.Start()
			if err != nil {
				utils.LogError(a.logger, err, "failed to start keploy agent")
				return 0, err
			}

			a.logger.Info("keploy agent started", zap.Int("pid", agentCmd.Process.Pid))
		}
	}

	time.Sleep(5 * time.Second)
	// check if its docker then create a init container first
	// and then the app container
	usrApp := app.NewApp(a.logger, clientId, cmd, a.dockerClient, app.Options{
		DockerNetwork: opts.DockerNetwork,
		Container:     opts.Container,
		DockerDelay:   opts.DockerDelay,
	})
	a.apps.Store(clientId, usrApp)

	err := usrApp.Setup(ctx)
	if err != nil {
		utils.LogError(a.logger, err, "failed to setup app")
		return 0, err
	}

	var inode uint64
	if isDockerCmd {
		// Start the init container to get the pid namespace
		inode, err = a.Initcontainer(ctx, a.logger, app.Options{
			DockerNetwork: opts.DockerNetwork,
			Container:     opts.Container,
			DockerDelay:   opts.DockerDelay,
		})
		if err != nil {
			utils.LogError(a.logger, err, "failed to setup init container")
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

	isAgent := a.isAgentRunning(ctx)
	if !isAgent {
		a.logger.Info("Keploy agent is not running in background, Loggin the agent file")
		err := exec.Command("cat", "keploy_agent.log").Run()
		if err != nil {
			a.logger.Error("failed to read keploy agent log file", zap.Error(err))
		}
		return fmt.Errorf("keploy agent is not running, please start the agent first")
	}

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
			IsDocker:      a.conf.Agent.IsDocker,
			AppInode:      opts.AppInode,
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

func (a *AgentClient) StartInDocker(ctx context.Context, logger *zap.Logger, clientId uint64) error {

	// Start the keploy agent in a Docker container
	agentCtx := context.WithoutCancel(context.Background())

	err := kdocker.StartInDocker(agentCtx, logger, &config.Config{
		InstallationID: a.conf.InstallationID,
	})
	if err != nil {
		utils.LogError(logger, err, "failed to start keploy agent in docker")
		return err
	}
	return nil
}

func (a *AgentClient) Initcontainer(ctx context.Context, logger *zap.Logger, opts app.Options) (uint64, error) {
	// Start the init container to get the PID namespace inode
	cmdCancel := func(cmd *exec.Cmd) func() error {
		return func() error {
			a.logger.Info("sending SIGINT to the container", zap.Any("cmd.Process.Pid", cmd.Process.Pid))
			err := utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGINT)
			return err
		}
	}
	cmd := fmt.Sprintf("docker run --network=%s --name keploy-init --rm alpine sleep infinity", a.conf.NetworkName)

	// execute the command
	go func() {
		cmdErr := utils.ExecuteCommand(ctx, a.logger, cmd, cmdCancel, 25*time.Second)
		if cmdErr.Err != nil {
			utils.LogError(a.logger, cmdErr.Err, "failed to execute init container command")
		}
	}()

	time.Sleep(2 * time.Second)
	// Get the PID of the container's first process
	inspect, err := a.dockerClient.ContainerInspect(ctx, "keploy-init")
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

func (a *AgentClient) isAgentRunning(ctx context.Context) bool {

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://localhost:%d/agent/health", a.conf.Agent.Port), nil)
	if err != nil {
		utils.LogError(a.logger, err, "failed to send request to the agent server")
	}

	resp, err := a.client.Do(req)
	if err != nil {
		a.logger.Info("Keploy agent is not running in background, starting the agent")
		return false
	} else {
		a.logger.Info("Setup request sent to the server", zap.String("status", resp.Status))
		return true
	}
}
