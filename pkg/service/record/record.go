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

type recorder struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          config.Config
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, telemetry Telemetry, instrumentation Instrumentation, config config.Config) Service {
	return &recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
	}
}

func (r *recorder) Start(ctx context.Context) error {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	var stopReason string

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
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

	// defining all the channels and variables required for the record
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError)
	var incomingChan <-chan *models.TestCase
	var outgoingChan <-chan *models.Mock
	var outgoingErrChan <-chan error
	var insertTestErrChan = make(chan error)
	var insertMockErrChan = make(chan error)
	var appID uint64

	defer close(appErrChan)
	defer close(insertTestErrChan)
	defer close(insertMockErrChan)

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason = "failed to get testSetIds"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	newTestSetID := pkg.NewID(testSetIDs, models.TestSetPattern)

	// setting up the environment for recording
	appID, err = r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
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
		err = r.instrumentation.Hook(ctx, appID, models.HookOptions{})
		if err != nil {
			stopReason = "failed to start the hooks and proxy"
			utils.LogError(r.logger, err, stopReason)
			if err == context.Canceled {
				return err
			}
			return fmt.Errorf(stopReason)
		}
	}

	// fetching test cases and mocks from the application and inserting them into the database
	incomingChan, err = r.instrumentation.GetIncoming(ctx, appID, models.IncomingOptions{})
	if err != nil {
		stopReason = "failed to get incoming frames"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	g.Go(func() error {
		for testCase := range incomingChan {
			err := r.testDB.InsertTestCase(ctx, testCase, newTestSetID)
			if err != nil {
				insertTestErrChan <- err
			}
		}
		return nil
	})

	outgoingChan, outgoingErrChan = r.instrumentation.GetOutgoing(ctx, appID, models.OutgoingOptions{})
	g.Go(func() error {
		for mock := range outgoingChan {
			err := r.mockDB.InsertMock(ctx, mock, newTestSetID)
			if err != nil {
				insertMockErrChan <- err
			}
		}
		return nil
	})

	// running the user application
	g.Go(func() error {
		runAppError = r.instrumentation.Run(ctx, appID, models.RunOptions{})
		if runAppError.AppErrorType == models.ErrCtxCanceled {
			return nil
		}
		appErrChan <- runAppError
		return nil
	})

	// setting a timer for recording
	g.Go(func() error {
		if r.config.Record.RecordTimer != 0 {
			g.Go(func() error {
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
		return nil
	})

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
		default:
			stopReason = "unknown error recieved from application, hence stopping keploy"
		}

	case err = <-outgoingErrChan:
		stopReason = "error while fetching outgoing frame, hence stopping keploy"
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

func (r *recorder) StartMock(ctx context.Context) error {
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
	var outgoingErrChan <-chan error
	var insertMockErrChan = make(chan error)

	appID, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		stopReason = "failed to exeute mock record due to error while setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}
	err = r.instrumentation.Hook(ctx, appID, models.HookOptions{})
	if err != nil {
		stopReason = "failed to start the hooks and proxy"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	outgoingChan, outgoingErrChan = r.instrumentation.GetOutgoing(ctx, appID, models.OutgoingOptions{})
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
	case err = <-outgoingErrChan:
		stopReason = "error while fetching outgoing frame, hence stopping keploy"
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	utils.LogError(r.logger, err, stopReason)
	return fmt.Errorf(stopReason)
}
