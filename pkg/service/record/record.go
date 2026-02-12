// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/telemetry"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Recorder struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	mappingDb       MappingDb
	telemetry       Telemetry
	instrumentation Instrumentation
	testSetConf     TestSetConfig
	config          *config.Config
	globalMockCh    chan<- *models.Mock
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, mappingDB MappingDb, telemetry Telemetry, instrumentation Instrumentation, testSetConf TestSetConfig, config *config.Config) Service {
	return &Recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDb:       mappingDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		testSetConf:     testSetConf,
		config:          config,
	}
}

func (r *Recorder) Start(ctx context.Context, reRecordCfg models.ReRecordCfg) error {

	r.logger.Debug("Starting Keploy recording... Please wait.")

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

	// Propagate parent context cancellation to setupCtx
	// This ensures that when Ctrl+C is pressed, setupCtx is cancelled immediately
	go func() {
		<-ctx.Done()
		setupCtxCancel()
	}()

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
	domainSet := telemetry.NewDomainSet()
	var recordingStarted bool

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

		r.logger.Info("Stopping Keploy recording...")

		// Notify the agent that we are shutting down gracefully
		// This will cause connection errors to be logged as debug instead of error
		if err := r.instrumentation.NotifyGracefulShutdown(context.Background()); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}

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
		if recordingStarted {
			r.telemetry.RecordedTestSuite(newTestSetID, testCount, mockCountMap, map[string]interface{}{
				"host-domains": domainSet.ToSlice(),
			})
		}
		if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
			s.Shutdown()
		}
	}()

	defer close(appErrChan)
	defer close(insertTestErrChan)
	defer close(insertMockErrChan)

	if reRecordCfg.TestSet != "" {
		// --- TARGETING AN EXISTING TEST SET ---
		newTestSetID = reRecordCfg.TestSet
		r.logger.Info("Starting mocks-only refresh for existing test set.", zap.String("testSet", newTestSetID))

		// Delete ONLY the old mocks.
		err := r.mockDB.DeleteMocksForSet(ctx, newTestSetID) // We will create this new function
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
	passPortsUint32 := make([]uint32, len(passPortsUint)) // slice type of uint32
	for i, port := range passPortsUint {
		passPortsUint32[i] = uint32(port)
	}

	// Instrument will setup the environment and start the hooks and proxy
	err := r.instrumentation.Setup(setupCtx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerDelay: r.config.BuildDelay, Mode: models.MODE_RECORD, CommandType: r.config.CommandType, EnableTesting: false, GlobalPassthrough: r.config.Record.GlobalPassthrough, BuildDelay: r.config.BuildDelay, PassThroughPorts: passPortsUint, ConfigPath: r.config.ConfigPath})

	if err != nil {
		// If context was cancelled (user pressed Ctrl+C), return gracefully without error
		if ctx.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	r.logger.Debug("Command type:", zap.String("commandType", r.config.CommandType))

	if r.config.CommandType == string(utils.DockerCompose) {

		r.logger.Info("Waiting for keploy-agent to be ready for docker compose...", zap.String("Agent-uri", r.config.Agent.AgentURI))

		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			if (runAppError.AppErrorType == models.ErrCtxCanceled || runAppError == models.AppError{}) {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})

		agentCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, r.logger, r.config.Agent.AgentURI, agentReadyCh, 1*time.Second)

		select {
		case <-ctx.Done():
			// Parent context cancelled (user pressed Ctrl+C)
			return ctx.Err()
		case <-agentCtx.Done():
			return fmt.Errorf("keploy-agent did not become ready in time")
		case <-agentReadyCh:
		}
	}

	r.logger.Debug("Agent is ready. Starting to fetch test cases and mocks...")

	var correlationMap sync.Map
	// fetching test cases and mocks from the application and inserting them into the database
	frames, err := r.GetTestAndMockChans(reqCtx)
	if err != nil {
		stopReason = "failed to get data frames"
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		return fmt.Errorf("%s", stopReason)
	}
	recordingStarted = true
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if r.config.CommandType == string(utils.DockerCompose) {

		r.logger.Debug("Making keploy-agent ready for docker compose...")

		err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
		}
	}

	r.logger.Info("Keploy agent is ready to record test cases and mocks.")

	r.mockDB.ResetCounterID() // Reset mock ID counter for each recording session
	errGrp.Go(func() error {
		for testCase := range frames.Incoming {
			testCase.Curl = pkg.MakeCurlCommand(testCase.HTTPReq)
			domainSet.AddAll(telemetry.ExtractDomainsFromTestCase(testCase))
			if reRecordCfg.TestSet == "" {
				err := r.testDB.InsertTestCase(ctx, testCase, newTestSetID, true)
				if err != nil {
					if ctx.Err() == context.Canceled {
						continue
					}
					insertTestErrChan <- err
				} else {
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
			domainSet.AddAll(telemetry.ExtractDomainsFromMock(mock))
			tempID := mock.Name
			// Send a copy to global mock channel for correlation manager if available
			if r.globalMockCh != nil {
				currMockID := r.mockDB.GetCurrMockID()
				// Create a deep copy of the mock to avoid race conditions
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
				if tempID != "" && mock.Name != "" {
					correlationMap.Store(tempID, mock.Name)
				}
				mockCountMap[mock.GetKind()]++
				r.telemetry.RecordedTestCaseMock(mock.GetKind())
			}
		}
		return nil
	})

	errGrp.Go(func() error {
		for mapping := range frames.Mappings {
			var realMockNames []string

			for _, tempID := range mapping.MockIDs {

				var realName string
				found := false

				// Simple retry loop (fast spin) to wait for the Mock Loop
				for i := 0; i < 50; i++ { // Wait up to ~500ms
					if val, ok := correlationMap.Load(tempID); ok {
						realName = val.(string)
						found = true
						break
					}
					time.Sleep(10 * time.Millisecond)
				}

				if found {
					realMockNames = append(realMockNames, realName)
					correlationMap.Delete(tempID)
				} else {
					r.logger.Warn("Failed to correlate mock mapping",
						zap.String("test", mapping.TestName),
						zap.String("tempMockID", tempID))
				}
			}

			// Write to mappings.yaml
			if len(realMockNames) > 0 {
				err := r.mappingDb.Upsert(ctx, newTestSetID, mapping.TestName, realMockNames)
				if err != nil {
					utils.LogError(r.logger, err, "failed to save mapping")
				}
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

	// setting a timer for recording
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

	// Waiting for the error to occur in any of the go routines
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

	// Create channels to receive incoming and outgoing data
	incomingChan := make(chan *models.TestCase)
	outgoingChan := make(chan *models.Mock)
	mappingChan := make(chan models.TestMockMapping)

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return FrameChan{}, fmt.Errorf("failed to get error group from context")
	}

	// INCOMING
	incomingStream, err := r.instrumentation.GetIncoming(ctx, incomingOpts)
	if err != nil {
		if ctx.Err() != nil || utils.IsShutdownError(err) {
			r.logger.Debug("Context cancelled or shutdown error while getting incoming test cases")
			// Close channels to prevent callers from hanging when ranging over them
			close(incomingChan)
			close(outgoingChan)
			return FrameChan{Incoming: incomingChan, Outgoing: outgoingChan}, nil
		}
		return FrameChan{}, fmt.Errorf("failed to get incoming test cases: %w", err)
	}

	g.Go(func() error {
		defer close(incomingChan)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case tc, ok := <-incomingStream:
				if !ok {
					return nil
				}
				// forward but remain cancelable
				select {
				case <-ctx.Done():
					return ctx.Err()
				case incomingChan <- tc:
				}
			}
		}
	})

	// OUTGOING
	// Create a cancelable child that we always cancel when ctx is done.
	mockCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var tlsPrivateKey string
	if r.config.Record.TLSPrivateKeyPath != "" {
		keyBytes, err := os.ReadFile(r.config.Record.TLSPrivateKeyPath)
		if err != nil {
			r.logger.Error("failed to read tls private key", zap.Error(err))
			cancel()
			return FrameChan{}, err
		}
		tlsPrivateKey = string(keyBytes)
	}

	outgoingStream, err := r.instrumentation.GetOutgoing(mockCtx, models.OutgoingOptions{
		Rules:          r.config.BypassRules,
		MongoPassword:  r.config.Test.MongoPassword,
		TLSPrivateKey:  tlsPrivateKey,
		FallBackOnMiss: r.config.Test.FallBackOnMiss,
	})
	if err != nil {

		cancel()
		if ctx.Err() != nil || utils.IsShutdownError(err) {
			r.logger.Debug("Context cancelled or shutdown error while getting outgoing mocks")
			// Close outgoingChan to prevent callers from hanging
			// Note: incomingChan will be closed by the goroutine started above when ctx is done
			close(outgoingChan)
			return FrameChan{Incoming: incomingChan, Outgoing: outgoingChan}, nil
		}
		return FrameChan{}, fmt.Errorf("failed to get outgoing mocks: %w", err)
	}
	g.Go(func() error {
		defer close(outgoingChan)
		defer cancel()

		// Also cancel mockCtx when parent ctx is done
		// This is done inside the goroutine to avoid goroutine leaks
		go func() {
			<-ctx.Done()
			cancel()
		}()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case m, ok := <-outgoingStream:
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

	// MAPPINGS
	g.Go(func() error {
		defer close(mappingChan)

		// Create context that cancels with parent
		mapCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()
		go func() {
			<-ctx.Done()
			cancel()
		}()

		// Call the new AgentClient method
		ch, err := r.instrumentation.GetMappings(mapCtx, incomingOpts)
		if err != nil {
			if ctx.Err() != nil || utils.IsShutdownError(err) {
				return nil
			}
			return fmt.Errorf("failed to get mappings: %w", err)
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
					return ctx.Err()
				case mappingChan <- m:
				}
			}
		}
	})

	return FrameChan{
		Incoming: incomingChan,
		Outgoing: outgoingChan,
		Mappings: mappingChan,
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
	// Parse metadata from the config
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
