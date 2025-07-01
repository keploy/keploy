// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"

	"go.keploy.io/server/v2/utils"
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

func (r *Recorder) Start(ctx context.Context, reRecord bool) error {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	errGrp, _ := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	runAppErrGrp, _ := errgroup.WithContext(ctx)
	runAppCtx := context.WithoutCancel(ctx)
	runAppCtx, runAppCtxCancel := context.WithCancel(runAppCtx)

	hookErrGrp, _ := errgroup.WithContext(ctx)
	hookCtx := context.WithoutCancel(ctx)
	hookCtx, hookCtxCancel := context.WithCancel(hookCtx)
	hookCtx = context.WithValue(hookCtx, models.ErrGroupKey, hookErrGrp)
	// reRecordCtx, reRecordCancel := context.WithCancel(ctx)
	// defer reRecordCancel() // Cancel the context when the function returns

	var stopReason string

	// defining all the channels and variables required for the record
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError, 1)
	var insertTestErrChan = make(chan error, 10)
	var insertMockErrChan = make(chan error, 10)
	var appID uint64
	var newTestSetID string
	var testCount = 0
	var mockCountMap = make(map[string]int)

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
		default:
			if !reRecord {
				err := utils.Stop(r.logger, stopReason)
				if err != nil {
					utils.LogError(r.logger, err, "failed to stop recording")
				}
			}
		}
		runAppCtxCancel()
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop application")
		}
		hookCtxCancel()
		err = hookErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop hooks")
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

	newTestSetID, err := r.GetNextTestSetID(ctx)
	if err != nil {
		stopReason = "failed to get new test-set id"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	// Create config.yaml if metadata is provided
	if r.config.Record.Metadata != "" {
		r.config.Record.Metadata = utils.TrimSpaces(r.config.Record.Metadata)
		r.createConfigWithMetadata(ctx, newTestSetID)
	}

	//checking for context cancellation as we don't want to start the instrumentation if the context is cancelled
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	// Instrument will setup the environment and start the hooks and proxy
	appID, err = r.Instrument(hookCtx)
	if err != nil {
		stopReason = "failed to instrument the application"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	r.config.AppID = appID

	// fetching test cases and mocks from the application and inserting them into the database
	frames, err := r.GetTestAndMockChans(ctx, appID)
	if err != nil {
		stopReason = "failed to get data frames"
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		return fmt.Errorf("%s", stopReason)
	}

	errGrp.Go(func() error {
		for testCase := range frames.Incoming {
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
		}
		return nil
	})

	errGrp.Go(func() error {
		for mock := range frames.Outgoing {
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

	if !r.config.E2E {
		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, appID, models.RunOptions{})
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
			stopReason = "internal error occured while hooking into the application, hence stopping keploy"
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

func (r *Recorder) Instrument(ctx context.Context) (uint64, error) {
	var stopReason string
	// setting up the environment for recording
	appID, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerNetwork: r.config.NetworkName, DockerDelay: r.config.BuildDelay})
	if err != nil {
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return 0, fmt.Errorf("%s", stopReason)
	}
	r.config.AppID = appID

	// checking for context cancellation as we don't want to start the hooks and proxy if the context is cancelled
	select {
	case <-ctx.Done():
		return appID, nil
	default:
		// Starting the hooks and proxy
		hooks := models.HookOptions{
			Mode:          models.MODE_RECORD,
			EnableTesting: r.config.EnableTesting,
			Rules:         r.config.BypassRules,
			E2E:           r.config.E2E,
			Port:          r.config.Port,
		}
		err = r.instrumentation.Hook(ctx, appID, hooks)
		if err != nil {
			stopReason = "failed to start the hooks and proxy"
			utils.LogError(r.logger, err, stopReason)
			if ctx.Err() == context.Canceled {
				return appID, err
			}
			return appID, fmt.Errorf("%s", stopReason)
		}
	}
	return appID, nil
}

func (r *Recorder) GetTestAndMockChans(ctx context.Context, appID uint64) (FrameChan, error) {
	incomingOpts := models.IncomingOptions{
		Filters:  r.config.Record.Filters,
		BasePath: r.config.Record.BasePath,
	}
	incomingChan, err := r.instrumentation.GetIncoming(ctx, appID, incomingOpts)
	if err != nil {
		return FrameChan{}, fmt.Errorf("failed to get incoming test cases: %w", err)
	}

	outgoingOpts := models.OutgoingOptions{
		Rules:          r.config.BypassRules,
		MongoPassword:  r.config.Test.MongoPassword,
		FallBackOnMiss: r.config.Test.FallBackOnMiss,
		Backdate:       time.Now(),
	}

	outgoingChan, err := r.instrumentation.GetOutgoing(ctx, appID, outgoingOpts)
	if err != nil {
		return FrameChan{}, fmt.Errorf("failed to get outgoing mocks: %w", err)
	}

	return FrameChan{
		Incoming: incomingChan,
		Outgoing: outgoingChan,
	}, nil
}

func (r *Recorder) RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, appID, opts)
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

func (r *Recorder) GetContainerIP(ctx context.Context, id uint64) (string, error) {
	return r.instrumentation.GetContainerIP(ctx, id)
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
