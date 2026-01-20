// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)


type Recorder struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	telemetry       Telemetry
	instrumentation Instrumentation
	testSetConf     TestSetConfig
	config          *config.Config
	globalMockCh    chan<- *models.Mock

	// ✅ used for config persistence (writing into keploy.yml)
	toolsSvc Tools
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, telemetry Telemetry, instrumentation Instrumentation, testSetConf TestSetConfig, config *config.Config) Service {
	return &Recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		testSetConf:     testSetConf,
		config:          config,
	}
}

// SetToolsService sets tools service for persisting config (writing keploy.yml)
func (r *Recorder) SetToolsService(toolsSvc Tools) {
	r.toolsSvc = toolsSvc
}

func (r *Recorder) Start(ctx context.Context, reRecordCfg models.ReRecordCfg) error {

	r.logger.Info("🔴 Starting Keploy recording... Please wait.")

	// ✅ Auto delay calc: record start -> first testcase insert + buffer
	recordStartTime := time.Now()
	autoDelaySaved := false

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	errGrp, _ := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	runAppErrGrp, _ := errgroup.WithContext(ctx)
	runAppCtx := context.WithoutCancel(ctx)
	runAppCtx, runAppCtxCancel := context.WithCancel(runAppCtx)

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	reqErrGrp, _ := errgroup.WithContext(ctx)
	reqCtx := context.WithoutCancel(ctx)
	reqCtx, reqCtxCancel := context.WithCancel(reqCtx)
	reqCtx = context.WithValue(reqCtx, models.ErrGroupKey, reqErrGrp)

	var stopReason string
	// defining all the channels and variables required for the record
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError, 1)
	var insertTestErrChan = make(chan error, 10)
	var insertMockErrChan = make(chan error, 10)
	var newTestSetID string
	var testCount = 0
	var mockCountMap = make(map[string]int)

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
		default:
			if !reRecordCfg.Rerecord {
				err := utils.Stop(r.logger, stopReason)
				if err != nil {
					utils.LogError(r.logger, err, "failed to stop recording")
				}
			}
		}

		r.logger.Info("🔴 Stopping Keploy recording... in the defer")

		runAppCtxCancel()
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop application")
		}

		reqCtxCancel()
		err = reqErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop request processing")
		}

		setupCtxCancel()
		err = setupErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop setup execution, that covers init container")
		}

		err = errGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		}
		r.telemetry.RecordedTestSuite(newTestSetID, testCount, mockCountMap)
	}()

	defer close(appErrChan)
	defer close(insertTestErrChan)
	defer close(insertMockErrChan)

	if reRecordCfg.TestSet != "" {
		// --- TARGETING AN EXISTING TEST SET ---
		newTestSetID = reRecordCfg.TestSet
		r.logger.Info("Starting mocks-only refresh for existing test set.", zap.String("testSet", newTestSetID))

		// Delete ONLY the old mocks.
		err := r.mockDB.DeleteMocksForSet(ctx, newTestSetID)
		if err != nil {
			stopReason = "failed to clear existing mocks for refresh"
			utils.LogError(r.logger, err, stopReason)
			return fmt.Errorf("%s", stopReason)
		}
	} else {
		var err error
		newTestSetID, err = r.GetNextTestSetID(ctx)
		if err != nil {
			stopReason = "failed to get new test-set id"
			utils.LogError(r.logger, err, stopReason)
			return fmt.Errorf("%s", stopReason)
		}
	}

	// Create config.yaml if metadata is provided
	if r.config.Record.Metadata != "" && r.testSetConf != nil {
		r.createConfigWithMetadata(ctx, newTestSetID)
	}

	//checking for context cancellation as we don't want to start the instrumentation if the context is cancelled
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	passPortsUint := config.GetByPassPorts(r.config)

	// Instrument will setup the environment and start the hooks and proxy
	err := r.instrumentation.Setup(setupCtx, r.config.Command, models.SetupOptions{
		Container:          r.config.ContainerName,
		DockerDelay:        r.config.BuildDelay,
		Mode:               models.MODE_RECORD,
		CommandType:         r.config.CommandType,
		EnableTesting:       false,
		GlobalPassthrough:   r.config.Record.GlobalPassthrough,
		BuildDelay:          r.config.BuildDelay,
		PassThroughPorts:    passPortsUint,
		ConfigPath:          r.config.ConfigPath,
	})
	if err != nil {
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	r.logger.Info("Command type:", zap.String("commandType", r.config.CommandType))

	if r.config.CommandType == string(utils.DockerCompose) {

		r.logger.Info("🟢 Waiting for keploy-agent to be ready for docker compose...", zap.String("Agent-uri", r.config.Agent.AgentURI))

		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			if runAppError.AppErrorType == models.ErrCtxCanceled || runAppError == (models.AppError{}) {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})

		agentCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, r.logger, r.config.Agent.AgentURI, agentReadyCh, 1*time.Second)

		select {
		case <-agentCtx.Done():
			return fmt.Errorf("keploy-agent did not become ready in time")
		case <-agentReadyCh:
		}
	}

	r.logger.Info("🟢 Keploy agent is ready to record test cases and mocks.")

	frames, err := r.GetTestAndMockChans(reqCtx)
	if err != nil {
		stopReason = "failed to get data frames"
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		return fmt.Errorf("%s", stopReason)
	}

	r.logger.Info("🟢 Keploy is now recording test cases and mocks for you application...")

	if r.config.CommandType == string(utils.DockerCompose) {
		r.logger.Info("🟢 Making keploy-agent ready for docker compose...")

		err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
		}
	}

	r.mockDB.ResetCounterID()

	errGrp.Go(func() error {
		for testCase := range frames.Incoming {
			testCase.Curl = pkg.MakeCurlCommand(testCase.HTTPReq)

			if reRecordCfg.TestSet == "" {
				err := r.testDB.InsertTestCase(ctx, testCase, newTestSetID, true)
				if err != nil {
					if ctx.Err() == context.Canceled {
						continue
					}
					insertTestErrChan <- err
				} else {

					// ✅ Save delay after FIRST successful testcase insert
					// ✅ Only if user did NOT explicitly provide delay
					// ✅ Persist to keploy.yml silently
					if !autoDelaySaved {
						autoDelaySaved = true

						secondsTaken := int(time.Since(recordStartTime).Seconds())
						autoDelay := secondsTaken + 10 // + buffer

						// clamp
						if autoDelay < 5 {
							autoDelay = 5
						}
						if autoDelay > 120 {
							autoDelay = 120
						}

						if !r.config.Test.DelayExplicit {
							if r.config.Test.Delay == 0 || r.config.Test.Delay == 5 {
								r.config.Test.Delay = uint64(autoDelay)

								r.logger.Info("✅ Auto-set test delay based on record startup time",
									zap.Uint64("delaySeconds", r.config.Test.Delay),
								)

								// Persist to keploy.yml
								if r.toolsSvc != nil {
    r.logger.Info("📝 Persisting delay into keploy.yml",
        zap.String("configPath", r.config.ConfigPath),
        zap.Uint64("delay", r.config.Test.Delay),
    )

    if err := r.toolsSvc.UpdateTestDelayInConfig(ctx, r.config.ConfigPath, r.config.Test.Delay); err != nil {
        r.logger.Error("❌ failed to persist auto delay to config", zap.Error(err))
    }
} else {
    r.logger.Warn("❌ toolsSvc is nil, cannot persist delay")
}

							}
						}
					}

					testCount++
					r.telemetry.RecordedTestAndMocks()
				}
			} else {
				r.logger.Info("🟠 Keploy has re-recorded test case for the user's application.")
			}
		}
		return nil
	})

	errGrp.Go(func() error {
		for mock := range frames.Outgoing {

			if r.globalMockCh != nil {
				currMockID := r.mockDB.GetCurrMockID()

				mockCopy := *mock
				mockCopy.Name = fmt.Sprintf("%s-%d", "mock", currMockID+1)

				select {
				case r.globalMockCh <- &mockCopy:
					r.logger.Debug("Mock sent to correlation manager", zap.String("mockKind", mock.GetKind()))
				default:
					r.logger.Warn("Global mock channel full, dropping mock for correlation", zap.String("mockKind", mock.GetKind()))
				}
			}

			err := r.mockDB.InsertMock(ctx, mock, newTestSetID)
			if err != nil {
				if ctx.Err() == context.Canceled {
					continue
				}
				insertMockErrChan <- err
			} else {
				mockCountMap[mock.GetKind()]++
				r.telemetry.RecordedTestCaseMock(mock.GetKind())
			}
		}
		return nil
	})

	if r.config.CommandType != string(utils.DockerCompose) {
		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			if runAppError.AppErrorType == models.ErrCtxCanceled {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})
	}

	if r.config.Record.RecordTimer != 0 {
		errGrp.Go(func() error {
			r.logger.Info("Setting a timer of " + r.config.Record.RecordTimer.String() + " for recording")
			timer := time.After(r.config.Record.RecordTimer)
			select {
			case <-timer:
				r.logger.Warn("Time up! Stopping keploy")
				err := utils.Stop(r.logger, "Time up! Stopping keploy")
				if err != nil {
					utils.LogError(r.logger, err, "failed to stop recording")
					return errors.New("failed to stop recording")
				}
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}

	select {
	case appErr := <-appErrChan:
		switch appErr.AppErrorType {
		case models.ErrCommandError:
			stopReason = "error in running the user application, hence stopping keploy"
		case models.ErrUnExpected:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
		case models.ErrInternal:
			stopReason = "internal error occurred while hooking into the application, hence stopping keploy"
		case models.ErrAppStopped:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
			r.logger.Warn(stopReason, zap.Error(appErr))
			return nil
		case models.ErrCtxCanceled:
			return nil
		case models.ErrTestBinStopped:
			stopReason = "keploy test mode binary stopped, hence stopping keploy"
			return nil
		default:
			stopReason = "unknown error received from application, hence stopping keploy"
		}

	case err = <-insertTestErrChan:
		stopReason = "error while inserting test case into db, hence stopping keploy"
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}

	utils.LogError(r.logger, err, stopReason)
	return fmt.Errorf("%s", stopReason)
}

func (r *Recorder) GetTestAndMockChans(ctx context.Context) (FrameChan, error) {

	incomingOpts := models.IncomingOptions{
		Filters: r.config.Record.Filters,
	}

	incomingChan := make(chan *models.TestCase)
	outgoingChan := make(chan *models.Mock)
	errChan := make(chan error, 2)

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return FrameChan{}, fmt.Errorf("failed to get error group from context")
	}

	// INCOMING
	g.Go(func() error {
		defer close(incomingChan)

		ch, err := r.instrumentation.GetIncoming(ctx, incomingOpts)
		if err != nil {
			if ctx.Err() == context.Canceled {
				return nil
			}
			errChan <- err
			return fmt.Errorf("failed to get incoming test cases: %w", err)
		}

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case tc, ok := <-ch:
				if !ok {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case incomingChan <- tc:
				}
			}
		}
	})

	// OUTGOING
	g.Go(func() error {
		defer close(outgoingChan)

		mockCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()

		go func() {
			<-ctx.Done()
			cancel()
		}()

		ch, err := r.instrumentation.GetOutgoing(mockCtx, models.OutgoingOptions{
			Rules:          r.config.BypassRules,
			MongoPassword:  r.config.Test.MongoPassword,
			FallBackOnMiss: r.config.Test.FallBackOnMiss,
		})
		if err != nil {
			if ctx.Err() == context.Canceled {
				return nil
			}
			return fmt.Errorf("failed to get outgoing mocks: %w", err)
		}

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case m, ok := <-ch:
				if !ok {
					return nil
				}
				select {
				case <-ctx.Done():
					outgoingChan <- m
					return ctx.Err()
				case outgoingChan <- m:
				}
			}
		}
	})

	_ = errChan

	return FrameChan{
		Incoming: incomingChan,
		Outgoing: outgoingChan,
	}, nil
}

func (r *Recorder) RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, opts)
}

func (r *Recorder) GetNextTestSetID(ctx context.Context) (string, error) {
	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get test set IDs: %w", err)
	}

	if r.config.Record.Metadata == "" {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	r.config.Record.Metadata = utils.TrimSpaces(r.config.Record.Metadata)
	meta, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil || meta == nil {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	nameVal, ok := meta["name"]
	requestedName, isStr := nameVal.(string)
	if !ok || !isStr || requestedName == "" {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	existingIDs := make(map[string]struct{}, len(testSetIDs))
	for _, id := range testSetIDs {
		existingIDs[id] = struct{}{}
	}

	if _, occupied := existingIDs[requestedName]; !occupied {
		return requestedName, nil
	}

	var highestSuffix int
	namePrefix := requestedName + "-"
	for id := range existingIDs {
		if !strings.HasPrefix(id, namePrefix) {
			continue
		}
		suffixPart := id[len(namePrefix):]
		if n, err := strconv.Atoi(suffixPart); err == nil && n > highestSuffix {
			highestSuffix = n
		}
	}

	newSuffix := highestSuffix + 1
	assignedName := fmt.Sprintf("%s-%d", requestedName, newSuffix)

	r.logger.Warn(fmt.Sprintf(
		"Test set name '%s' already exists, using '%s' instead. You can change this name if you want.",
		requestedName, assignedName,
	))

	return assignedName, nil
}

func (r *Recorder) createConfigWithMetadata(ctx context.Context, testSetID string) {
	metadata, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil {
		utils.LogError(r.logger, err, "failed to parse metadata", zap.String("metadata", r.config.Record.Metadata))
		return
	}

	testSet := &models.TestSet{
		PreScript:  "",
		PostScript: "",
		Template:   make(map[string]interface{}),
		Metadata:   metadata,
	}

	err = r.testSetConf.Write(ctx, testSetID, testSet)
	if err != nil {
		utils.LogError(r.logger, err, "Failed to create test-set config file with metadata", zap.String("testSet", testSetID))
		return
	}

	r.logger.Info("Created test-set config file with metadata")
}

// SetGlobalMockChannel sets the global mock channel for sending mocks to correlation manager
func (r *Recorder) SetGlobalMockChannel(mockCh chan<- *models.Mock) {
	r.globalMockCh = mockCh
	r.logger.Info("Global mock channel set for record service")
}
