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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/pkg/models"
	Docker "go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// agent will implement
type Agent struct {
	logger       *zap.Logger
	core.Proxy                 // embedding the Proxy interface to transfer the proxy methods to the core object
	core.Hooks                 // embedding the Hooks interface to transfer the hooks methods to the core object
	core.Tester                // embedding the Tester interface to transfer the tester methods to the core object
	dockerClient Docker.Client //embedding the docker client to transfer the docker client methods to the core object
	id           utils.AutoInc
	apps         sync.Map
	proxyStarted bool
	client       http.Client
}

// this will be the client side implementation
func New(logger *zap.Logger, hook core.Hooks, proxy core.Proxy, tester core.Tester, client Docker.Client) *Agent {
	return &Agent{
		logger:       logger,
		Hooks:        hook,
		Proxy:        proxy,
		Tester:       tester,
		dockerClient: client,
		client:       http.Client{},
	}
}

// Listeners will get activated, details will be stored in the map. And connection will be established
func (a *Agent) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	requestBody := models.IncomingReq{
		IncomingOptions: opts,
		AppId:           0,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for incoming request")
		return nil, fmt.Errorf("error marshaling request body for incoming request: %s", err.Error())
	}

	fmt.Println("GET INCOMING !!")
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

func (a *Agent) GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
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

func (a *Agent) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {
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

func (a *Agent) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
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

func (a *Agent) GetConsumedMocks(ctx context.Context, id uint64) ([]string, error) {
	// Create the URL with query parameters
	url := fmt.Sprintf("http://localhost:8086/agent/consumedmocks?id=%d", id)

	// Create a new GET request with the query parameter
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %s", err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}

	// Make the HTTP request
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}
	defer res.Body.Close() // Close the response body when done

	// Read the response body
	var consumedMocks []string
	err = json.NewDecoder(res.Body).Decode(&consumedMocks)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response body: %s", err.Error())
	}

	return consumedMocks, nil
}

func (a *Agent) UnHook(ctx context.Context, id uint64) error {
	return nil
}

func (a *Agent) GetContainerIP(_ context.Context, id uint64) (string, error) {

	// a, err := c.getApp(id)
	// if err != nil {
	// 	utils.LogError(c.logger, err, "failed to get app")
	// 	return "", err
	// }

	// ip := a.ContainerIPv4Addr()
	// a.logger.Debug("ip address of the target app container", zap.Any("ip", ip))
	// if ip == "" {
	// 	return "", fmt.Errorf("failed to get the IP address of the app container. Try increasing --delay (in seconds)")
	// }

	// return ip, nil
	return "", nil
}

func (a *Agent) Run(ctx context.Context, id uint64, _ models.RunOptions) models.AppError {
	fmt.Println("Run.....", id)
	app, err := a.getApp(id)

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
		defer close(inodeChan)
		if err != nil {
			utils.LogError(a.logger, err, "failed to stop the app")
		}
	}()

	runAppErrGrp.Go(func() error {
		defer utils.Recover(a.logger)
		if app.Kind(ctx) == utils.Native {
			return nil
		}
		select {
		case inode := <-inodeChan:
			err := a.Hooks.SendInode(ctx, id, inode)
			if err != nil {
				utils.LogError(a.logger, err, "")

				inodeErrCh <- errors.New("failed to send inode to the kernel")
			}
		case <-ctx.Done():
			return nil
		}
		return nil
	})

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
	case inodeErr := <-inodeErrCh:
		return models.AppError{AppErrorType: models.ErrInternal, Err: inodeErr}
	}
}

func (a *Agent) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	// check if the agent is running
	isAgentRunning := false
	// if the agent is not running, start the agent

	httpClient := &http.Client{
		Timeout: 1 * time.Minute,
	}
	clientPid := uint32(os.Getpid())
	fmt.Println("clientPid", clientPid)
	requestBody := models.SetupReq{
		SetupOptions: models.SetupOptions{
			Container:     opts.Container,
			DockerNetwork: opts.DockerNetwork,
			DockerDelay:   opts.DockerDelay,
			Cmd:           cmd,
			ClientPid:     clientPid,
			IsApi:         true,
			Mode:          opts.Mode,
		},
		AppId: 0,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return 0, fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	// where should I place this as its causing problems for the first time ?
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:8086/agent/health", bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		a.logger.Info("Keploy agent is not running in background, starting the agent")
	} else {
		isAgentRunning = true
		a.logger.Info("Setup request sent to the server", zap.String("status", resp.Status))
		time.Sleep(5 * time.Second)
	}

	fmt.Println("isAgentRunning", isAgentRunning)

	if !isAgentRunning {
		// Start the keploy agent as a detached process and pipe the logs into a file

		// Open the log file in append mode or create it if it doesn't exist
		logFile, err := os.OpenFile("keploy_agent.log", os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			utils.LogError(a.logger, err, "failed to open log file")
			return 0, err
		}
		defer logFile.Close()

		agentCmd := exec.Command("oss", "agent")
		agentCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // Detach the process

		// Redirect the standard output and error to the log file
		agentCmd.Stdout = logFile
		agentCmd.Stderr = logFile

		err = agentCmd.Start()
		if err != nil {
			utils.LogError(a.logger, err, "failed to start keploy agent")
			return 0, err
		}

		// a.logger.Info("keploy agent started", zap.Any("pid", agentCmd.Process.Pid))
		time.Sleep(10 * time.Second)

		a.logger.Info("sending request", zap.Any("Reqbody", requestBody))
		resp, err = httpClient.Do(req)
		if err != nil {
			a.logger.Error("failed to send setup request to the server", zap.Error(err))
		}

		a.logger.Info("Registering client after starting agent", zap.String("status", resp.Status))
	}

	// Doubt: this is currently hardcoded, will it be returned from the server ?
	id := uint64(a.id.Next())

	// Doubt: will this be needed in test mode as well or somewhere else we have done this ??
	usrApp := app.NewApp(a.logger, id, cmd, a.dockerClient, app.Options{
		DockerNetwork: opts.DockerNetwork,
		Container:     opts.Container,
		DockerDelay:   opts.DockerDelay,
	})
	a.apps.Store(id, usrApp)

	err = usrApp.Setup(ctx)
	if err != nil {
		utils.LogError(a.logger, err, "failed to setup app")
		return 0, err
	}
	return id, nil
}

// Doubt: where should I place this getApp method ?
func (ag *Agent) getApp(id uint64) (*app.App, error) {
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
