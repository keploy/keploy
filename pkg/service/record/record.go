package record

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type recorder struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          config.Config
}

func NewRecorder(logger *zap.Logger, testDB TestDB, mockDB MockDB, telemetry Telemetry, instrumentation Instrumentation, config config.Config) *recorder {
	return &recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
	}
}

func (r *recorder) Record(ctx context.Context) error {
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError)
	var incomingFrameChan = make(chan models.Frame)
	var outgoingFrameChan = make(chan models.Frame)
	var incomingErrChan = make(chan models.IncomingError)
	var outgoingErrChan = make(chan models.OutgoingError)

	hookCtx, hookCancel := context.WithCancel(context.Background())
	runAppCtx, runAppCancel := context.WithCancel(context.Background())
	incomingCtx, incomingCancel := context.WithCancel(context.Background())
	outgoingCtx, outgoingCancel := context.WithCancel(context.Background())

	defer hookCancel()
	defer runAppCancel()
	defer incomingCancel()
	defer outgoingCancel()

	err := r.instrumentation.Hook(hookCtx, models.HookOptions{})
	if err != nil {
		return fmt.Errorf("failed to start the hooks and proxy: %w", err)
	}

	go func() {
		runAppError = r.instrumentation.Run(runAppCtx, r.config.Command)
		appErrChan <- runAppError
	}()

	go func() {
		incomingFrameChan, incomingErrChan = r.instrumentation.GetIncoming(incomingCtx, models.IncomingOptions{})
	}()

	go func() {
		outgoingFrameChan, outgoingErrChan = r.instrumentation.GetOutgoing(outgoingCtx, models.OutgoingOptions{})
	}()

	for {
		select {
		case appErr := <-appErrChan:
			switch appErr.AppErrorType {
			case models.ErrCommandError:
				r.logger.Error("error in running the user application, hence stopping keploy", zap.Error(appErr))
			case models.ErrUnExpected:
				r.logger.Warn("user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected", zap.Error(appErr))
			default:
				r.logger.Error("unknown error recieved from application, hence stopping keploy", zap.Error(appErr))
			}
			hookCancel()
			incomingCancel()
			outgoingCancel()
			return errors.New("failed to execute record due to error in running the user application")
		case frame := <-incomingFrameChan:
			r.logger.Info("Incoming frame", zap.Any("frame", frame))
		case frame := <-outgoingFrameChan:
			r.logger.Info("Outgoing frame", zap.Any("frame", frame))
		case err := <-incomingErrChan:
			r.logger.Error("error while fetching incoming frame", zap.Error(err))
			runAppCancel()
			hookCancel()
			outgoingCancel()
			return errors.New("failed to execute record due to error in fetching incoming frame")
		case err := <-outgoingErrChan:
			r.logger.Error("error while fetching outgoing frame", zap.Error(err))
			runAppCancel()
			hookCancel()
			incomingCancel()
			return errors.New("failed to execute record due to error in fetching outgoing frame")
		case <-ctx.Done():
			return nil
		}
	}
}

func (r *recorder) MockRecord(ctx context.Context) error {
	var outgoingFrameChan = make(chan models.Frame)
	var outgoingErrChan = make(chan models.OutgoingError)

	hookCtx, hookCancel := context.WithCancel(context.Background())
	outgoingCtx, outgoingCancel := context.WithCancel(context.Background())

	defer hookCancel()
	defer outgoingCancel()

	err := r.instrumentation.Hook(hookCtx, models.HookOptions{})
	if err != nil {
		return fmt.Errorf("failed to execute mock-record due to error while loading hooks and proxy: %w", err)
	}

	go func() {
		outgoingFrameChan, outgoingErrChan = r.instrumentation.GetOutgoing(outgoingCtx, models.OutgoingOptions{})
	}()

	for {
		select {
		case frame := <-outgoingFrameChan:
			r.logger.Info("Outgoing frame", zap.Any("frame", frame))
		case err := <-outgoingErrChan:
			r.logger.Error("error while fetching outgoing frame", zap.Error(err))
			hookCancel()
			return errors.New("failed to execute record due to error in fetching outgoing frame")
		case <-ctx.Done():
			return nil
		}
	}

}
