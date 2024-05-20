// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"

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
	config          config.Config
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, telemetry Telemetry, instrumentation Instrumentation, config config.Config) Service {
	return &Recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
	}
}

func (r *Recorder) Start(ctx context.Context) error {

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
	reRecordCtx, reRecordCancel := context.WithCancel(ctx)
	defer reRecordCancel() // Cancel the context when the function returns

	var stopReason string

	// defining all the channels and variables required for the record
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError, 1)
	var incomingChan <-chan *models.TestCase
	var outgoingChan <-chan *models.Mock
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
			r.telemetry.RecordedTestSuite(newTestSetID, testCount, mockCountMap)
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
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
	}()

	defer close(appErrChan)
	defer close(insertTestErrChan)
	defer close(insertMockErrChan)

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason = "failed to get testSetIds"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	newTestSetID = pkg.NewID(testSetIDs, models.TestSetPattern)

	// setting up the environment for recording
	appID, err = r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerNetwork: r.config.NetworkName, DockerDelay: r.config.BuildDelay})
	if err != nil {
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	// checking for context cancellation as we don't want to start the hooks and proxy if the context is cancelled
	select {
	case <-ctx.Done():
		return nil
	default:
		// Starting the hooks and proxy
		err = r.instrumentation.Hook(hookCtx, appID, models.HookOptions{Mode: models.MODE_RECORD, EnableTesting: r.config.EnableTesting})
		if err != nil {
			stopReason = "failed to start the hooks and proxy"
			utils.LogError(r.logger, err, stopReason)
			if err == context.Canceled {
				return err
			}
			return fmt.Errorf(stopReason)
		}
	}

	incomingOpts := models.IncomingOptions{
		Filters: r.config.Record.Filters,
	}

	// fetching test cases and mocks from the application and inserting them into the database
	incomingChan, err = r.instrumentation.GetIncoming(ctx, appID, incomingOpts)
	if err != nil {
		stopReason = "failed to get incoming frames"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	errGrp.Go(func() error {
		for testCase := range incomingChan {
			err := r.testDB.InsertTestCase(ctx, testCase, newTestSetID)
			if err != nil {
				if err == context.Canceled {
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

	outgoingOpts := models.OutgoingOptions{
		Rules:          r.config.BypassRules,
		MongoPassword:  r.config.Test.MongoPassword,
		FallBackOnMiss: r.config.Test.FallBackOnMiss,
	}

	outgoingChan, err = r.instrumentation.GetOutgoing(ctx, appID, outgoingOpts)
	if err != nil {
		stopReason = "failed to get outgoing frames"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}
	errGrp.Go(func() error {
		for mock := range outgoingChan {
			err := r.mockDB.InsertMock(ctx, mock, newTestSetID)
			if err != nil {
				if err == context.Canceled {
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

	// running the user application
	runAppErrGrp.Go(func() error {
		runAppError = r.instrumentation.Run(runAppCtx, appID, models.RunOptions{})
		if runAppError.AppErrorType == models.ErrCtxCanceled {
			return nil
		}
		appErrChan <- runAppError
		return nil
	})
	go func() {
		if len(r.config.ReRecord) != 0 {
			err = r.ReRecord(reRecordCtx, appID)
			reRecordCancel()

		}
	}()

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
			stopReason = "unknown error recieved from application, hence stopping keploy"
		}

	case err = <-insertTestErrChan:
		stopReason = "error while inserting test case into db, hence stopping keploy"
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	utils.LogError(r.logger, err, stopReason)
	return fmt.Errorf(stopReason)
}

func (r *Recorder) StartMock(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	var stopReason string
	defer func() {
		select {
		case <-ctx.Done():
			break
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
			}
		}
		err := g.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		}
	}()
	var outgoingChan <-chan *models.Mock
	var insertMockErrChan = make(chan error)

	appID, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerNetwork: r.config.NetworkName, DockerDelay: r.config.BuildDelay})
	if err != nil {
		stopReason = "failed to exeute mock record due to error while setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}
	err = r.instrumentation.Hook(ctx, appID, models.HookOptions{Mode: models.MODE_RECORD})
	if err != nil {
		stopReason = "failed to start the hooks and proxy"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	outgoingChan, err = r.instrumentation.GetOutgoing(ctx, appID, models.OutgoingOptions{})
	if err != nil {
		stopReason = "failed to get outgoing frames"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}
	g.Go(func() error {
		for mock := range outgoingChan {
			mock := mock // capture range variable
			g.Go(func() error {
				err := r.mockDB.InsertMock(ctx, mock, "")
				if err != nil {
					insertMockErrChan <- err
				}
				return nil
			})
		}
		return nil
	})

	select {
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	utils.LogError(r.logger, err, stopReason)
	return fmt.Errorf(stopReason)
}

func (r *Recorder) ReRecord(ctx context.Context, appID uint64) error {

	tcs, err := r.testDB.GetTestCases(ctx, r.config.ReRecord)
	if err != nil {
		r.logger.Error("Failed to get testcases", zap.Error(err))
		return nil
	}
	host, port, err := extractHostAndPort(tcs[0].Curl)
	if err != nil {
		r.logger.Error("Failed to extract host and port", zap.Error(err))
		return nil

	}
	cmdType := utils.FindDockerCmd(r.config.Command)
	if cmdType == utils.Docker || cmdType == utils.DockerCompose {
		host = r.config.ContainerName
	}

	if err := waitForPort(ctx, host, port); err != nil {
		r.logger.Error("Waiting for port failed", zap.String("host", host), zap.String("port", port), zap.Error(err))
		return nil
	}

	allTestCasesRecorded := true
	for _, tc := range tcs {
		if cmdType == utils.Docker || cmdType == utils.DockerCompose {

			userIP, err := r.instrumentation.GetContainerIP(ctx, appID)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get the app ip")
				break
			}

			tc.HTTPReq.URL, err = utils.ReplaceHostToIP(tc.HTTPReq.URL, userIP)
			if err != nil {
				utils.LogError(r.logger, err, "failed to replace host to docker container's IP")
				break
			}
			r.logger.Debug("", zap.Any("replaced URL in case of docker env", tc.HTTPReq.URL))
		}

		resp, err := pkg.SimulateHTTP(ctx, *tc, r.config.ReRecord, r.logger, r.config.Test.APITimeout)
		if err != nil {
			r.logger.Error("Failed to simulate HTTP request", zap.Error(err))
			allTestCasesRecorded = false
			continue // Proceed with the next command
		}

		r.logger.Debug("Re-recorded testcases successfully", zap.String("curl", tc.Curl), zap.Any("response", (resp)))

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	if allTestCasesRecorded {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err = utils.Stop(r.logger, "Re-recorded testcases successfully")
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
			}
		}
	} else {
		err = utils.Stop(r.logger, "Failed to re-record some testcases")
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		}
	}

	return nil
}
