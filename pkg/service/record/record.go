// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"
	"io"

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
	config          *config.Config
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, telemetry Telemetry, instrumentation Instrumentation, config *config.Config) Service {
	return &Recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
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

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	_, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(ctx, models.ErrGroupKey, setupErrGrp)

	reqErrGrp, _ := errgroup.WithContext(ctx)
	reqCtx := context.WithoutCancel(ctx)
	_, reqCtxCancel := context.WithCancel(reqCtx)
	reqCtx = context.WithValue(ctx, models.ErrGroupKey, reqErrGrp)
	var stopReason string

	// defining all the channels and variables required for the record
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError, 1)
	var insertTestErrChan = make(chan error, 10)
	var insertMockErrChan = make(chan error, 10)
	var clientID uint64
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

		unregister := models.UnregisterReq{
			ClientID: clientID,
			Mode:     models.MODE_RECORD,
		}

		// Dont call the Unregister if there is an error in the running application
		if runAppError.AppErrorType != models.ErrUnExpected {
			err := r.instrumentation.UnregisterClient(ctx, unregister)
			if err != nil && err != io.EOF {
				fmt.Println("error in unregistering client record")
				utils.LogError(r.logger, err, "failed to unregister client")
			}
		}

		runAppCtxCancel()
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop application")
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

		reqCtxCancel()
		// err = reqErrGrp.Wait()
		// if err != nil && err != io.EOF {
		// 	utils.LogError(r.logger, err, "failed to stop request execution")
		// }
		r.telemetry.RecordedTestSuite(newTestSetID, testCount, mockCountMap)
	}()

	defer close(appErrChan)

	newTestSetID, err := r.GetNextTestSetID(ctx)
	if err != nil {
		stopReason = "failed to get new test-set id"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	//checking for context cancellation as we don't want to start the instrumentation if the context is cancelled
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	clientID, err = r.instrumentation.Setup(setupCtx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerNetwork: r.config.NetworkName, DockerDelay: r.config.BuildDelay, Mode: models.MODE_RECORD, CommandType: r.config.CommandType})
	if err != nil {
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	r.config.ClientID = clientID

	// fetching test cases and mocks from the application and inserting them into the database
	frames, err := r.GetTestAndMockChans(reqCtx, clientID)
	if err != nil {
		stopReason = "failed to get data frames"
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	errGrp.Go(func() error {
		for testCase := range frames.Incoming {
			err := r.testDB.InsertTestCase(ctx, testCase, newTestSetID)
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
			if mock == nil || mock.GetKind() == "" {
				continue
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

	// running the user application
	runAppErrGrp.Go(func() error {
		runAppError = r.instrumentation.Run(runAppCtx, clientID, models.RunOptions{})
		if runAppError.AppErrorType == models.ErrCtxCanceled {
			return nil
		}
		appErrChan <- runAppError
		return nil
	})

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

func (r *Recorder) GetTestAndMockChans(ctx context.Context, clientID uint64) (FrameChan, error) {
	incomingOpts := models.IncomingOptions{
		Filters: r.config.Record.Filters,
	}

	outgoingOpts := models.OutgoingOptions{
		Rules:          r.config.BypassRules,
		MongoPassword:  r.config.Test.MongoPassword,
		FallBackOnMiss: r.config.Test.FallBackOnMiss,
	}

	// Create channels to receive incoming and outgoing data
	incomingChan := make(chan *models.TestCase)
	outgoingChan := make(chan *models.Mock)
	errChan := make(chan error, 2)

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return FrameChan{}, fmt.Errorf("failed to get error group from context")
	}

	g.Go(func() error {
		defer close(incomingChan)

		ch, err := r.instrumentation.GetIncoming(ctx, clientID, incomingOpts)
		if err != nil {
			errChan <- err
			return fmt.Errorf("failed to get incoming test cases: %w", err)
		}
		for testCase := range ch {
			incomingChan <- testCase
		}
		return nil
	})

	g.Go(func() error {

		defer close(outgoingChan)
		mockReceived := false
		// create a context without cancel
		// change this name to some mockCtx error group
		mockErrGrp, _ := errgroup.WithContext(ctx)
		mockCtx := context.WithoutCancel(ctx)
		mockCtx, mockCtxCancel := context.WithCancel(mockCtx)

		defer func() {
			fmt.Println("closing reqCtx")
			mockCtxCancel()
			err := mockErrGrp.Wait()
			if err != nil && err != io.EOF {
				utils.LogError(r.logger, err, "failed to stop request execution")
			}
		}()

		// listen for ctx canecllation in a go routine
		go func() {
			select {
			case <-ctx.Done():
				if !mockReceived {
					fmt.Println("context cancelled in go routine")
					mockCtxCancel()
				}
			}
		}()

		ch, err := r.instrumentation.GetOutgoing(mockCtx, clientID, outgoingOpts)
		if err != nil {
			r.logger.Error("failed to get outgoing mocks", zap.Error(err))
			errChan <- err
			return fmt.Errorf("failed to get outgoing mocks: %w", err)
		}

		for mock := range ch {
			mockReceived = true // Set flag if a mock is received
			select {
			case <-ctx.Done():
				if mock != nil {
					fmt.Println("mock is not nil")
					outgoingChan <- mock
				}
				return nil
			default:
				outgoingChan <- mock
			}
		}
		return nil
	})

	return FrameChan{
		Incoming: incomingChan,
		Outgoing: outgoingChan,
	}, nil
}

func (r *Recorder) RunApplication(ctx context.Context, clientID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, clientID, opts)
}

func (r *Recorder) GetNextTestSetID(ctx context.Context) (string, error) {
	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get test set IDs: %w", err)
	}
	return pkg.NextID(testSetIDs, models.TestSetPattern), nil
}

func (r *Recorder) GetContainerIP(ctx context.Context, clientID uint64) (string, error) {
	return r.instrumentation.GetContainerIP(ctx, clientID)
}
