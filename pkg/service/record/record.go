package record

import (
	"context"

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
	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	var stopReason string
	defer func() {
		select {
		case <-ctx.Done():
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				r.logger.Error("failed to stop recording", zap.Error(err))
			}
		}
		err := g.Wait()
		if err != nil {
			r.logger.Error("failed to stop recording", zap.Error(err))
		}
	}()

	var runAppError models.AppError
	var appErrChan = make(chan models.AppError)
	var incomingChan <-chan *models.TestCase
	var outgoingChan <-chan *models.Mock
	var incomingErrChan <-chan error
	var outgoingErrChan <-chan error
	var insertTestErrChan = make(chan error)
	var insertMockErrChan = make(chan error)
	var appId uint64

	testSetIds, err := r.testDB.GetAllTestSetIds(ctx)
	if err != nil {
		stopReason = "failed to get testSetIds"
		r.logger.Error(stopReason, zap.Error(err))
		return nil
	}

	newTestSetId := pkg.NewId(testSetIds, models.TestSetPattern)

	appId, err = r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		stopReason = "failed to exeute record due to error while setting up the environment"
		r.logger.Error(stopReason, zap.Error(err))
		return nil
	}

	err = r.instrumentation.Hook(ctx, appId, models.HookOptions{})
	if err != nil {
		stopReason = "failed to start the hooks and proxy"
		r.logger.Error(stopReason, zap.Error(err))
		return nil
	}

	incomingChan, incomingErrChan = r.instrumentation.GetIncoming(ctx, appId, models.IncomingOptions{})
	g.Go(func() error {
		for testCase := range incomingChan {
			testCase := testCase // capture range variable
			g.Go(func() error {
				err := r.testDB.InsertTestCase(ctx, testCase, newTestSetId)
				if err != nil {
					insertTestErrChan <- err
				}
				return nil
			})
		}
		return nil
	})

	outgoingChan, outgoingErrChan = r.instrumentation.GetOutgoing(ctx, appId, models.OutgoingOptions{})
	g.Go(func() error {
		for mock := range outgoingChan {
			mock := mock // capture range variable
			g.Go(func() error {
				err := r.mockDB.InsertMock(ctx, mock, newTestSetId)
				if err != nil {
					insertMockErrChan <- err
				}
				return nil
			})
		}
		return nil
	})

	g.Go(func() error {
		runAppError = r.instrumentation.Run(ctx, appId, models.RunOptions{})
		if runAppError.AppErrorType == models.ErrCtxCanceled {
			return nil
		}
		appErrChan <- runAppError
		return nil
	})

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
	case err = <-incomingErrChan:
		stopReason = "error while fetching incoming frame, hence stopping keploy"
	case err = <-outgoingErrChan:
		stopReason = "error while fetching outgoing frame, hence stopping keploy"
	case err = <-insertTestErrChan:
		stopReason = "error while inserting test case into db, hence stopping keploy"
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	r.logger.Error(stopReason, zap.Error(err))
	return nil
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
				r.logger.Error("failed to stop recording", zap.Error(err))
			}
		}
		err := g.Wait()
		if err != nil {
			r.logger.Error("failed to stop recording", zap.Error(err))
		}
	}()
	var outgoingChan <-chan *models.Mock
	var outgoingErrChan <-chan error
	var insertMockErrChan = make(chan error)

	appId, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		stopReason = "failed to exeute mock record due to error while setting up the environment"
		r.logger.Error(stopReason, zap.Error(err))
		return nil
	}
	err = r.instrumentation.Hook(ctx, appId, models.HookOptions{})
	if err != nil {
		stopReason = "failed to start the hooks and proxy"
		r.logger.Error(stopReason, zap.Error(err))
		return nil
	}

	outgoingChan, outgoingErrChan = r.instrumentation.GetOutgoing(ctx, appId, models.OutgoingOptions{})
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
	r.logger.Error(stopReason, zap.Error(err))
	return nil
}
