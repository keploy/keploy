package record

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
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
	var recordErr error
	var appId int

	hookCtx, hookCancel := context.WithCancel(context.Background())
	runAppCtx, runAppCancel := context.WithCancel(context.Background())
	incomingCtx, incomingCancel := context.WithCancel(context.Background())
	outgoingCtx, outgoingCancel := context.WithCancel(context.Background())

	defer hookCancel()
	defer runAppCancel()
	defer incomingCancel()
	defer outgoingCancel()

	stopReason := "User stopped recording"

	testSetIds, err := r.testDB.GetAllTestSetIds(ctx)
	if err != nil {
		stopReason = "failed to get testSetIds"
		utils.Stop(r.logger, stopReason)
		return fmt.Errorf(stopReason+": %w", err)
	}

	newTestSetId, err := newTestSetId(testSetIds)
	if err != nil {
		stopReason = "failed to create new testSetId"
		utils.Stop(r.logger, stopReason)
		return fmt.Errorf(stopReason+": %w", err)
	}

	appId, err = r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})

	err = r.instrumentation.Hook(hookCtx, appId, models.HookOptions{})
	if err != nil {
		return fmt.Errorf("failed to start the hooks and proxy: %w", err)
	}

	incomingFrameChan, incomingErrChan = r.instrumentation.GetIncoming(incomingCtx, appId, models.IncomingOptions{})

	outgoingFrameChan, outgoingErrChan = r.instrumentation.GetOutgoing(outgoingCtx, appId, models.OutgoingOptions{})

	go func() {
		runAppError = r.instrumentation.Run(runAppCtx, appId, r.config.Command)
		appErrChan <- runAppError
	}()

	loop := true
	for loop {
		select {
		case appErr := <-appErrChan:
			switch appErr.AppErrorType {
			case models.ErrCommandError:
				stopReason = "error in running the user application, hence stopping keploy"
				r.logger.Error(stopReason, zap.Error(appErr))
			case models.ErrUnExpected:
				stopReason = "user application terminated unexpectedly hence stopping keploy"
				r.logger.Warn(stopReason+", please check user application logs if this behaviour is not expected", zap.Error(appErr))
			default:
				stopReason = "unknown error recieved from application, hence stopping keploy"
				r.logger.Error("unknown error recieved from user application, hence stopping keploy", zap.Error(appErr))
			}
			recordErr = errors.New("failed to execute record due to error in running the user application")
			loop = false
		case frame := <-incomingFrameChan:
			err := r.testDB.InsertTestCase(context.Background(), frame, newTestSetId)
			if err != nil {
				stopReason = "error while inserting incoming frame into db, hence stopping keploy"
				r.logger.Error(stopReason, zap.Error(err))
				recordErr = errors.New("failed to execute record due to error in inserting incoming frame into db")
				loop = false
			}
		case frame := <-outgoingFrameChan:
			err := r.mockDB.InsertMock(context.Background(), frame, newTestSetId)
			if err != nil {
				stopReason = "error while inserting outgoing frame into db, hence stopping keploy"
				r.logger.Error(stopReason, zap.Error(err))
				recordErr = errors.New("failed to execute record due to error in inserting outgoing frame into db")
				loop = false
			}
		case err := <-incomingErrChan:
			stopReason = "error while fetching incoming frame, hence stopping keploy"
			r.logger.Error(stopReason, zap.Error(err))
			recordErr = errors.New("failed to execute record due to error in fetching incoming frame")
			loop = false
		case err := <-outgoingErrChan:
			stopReason = "error while fetching outgoing frame, hence stopping keploy"
			r.logger.Error(stopReason, zap.Error(err))
			recordErr = errors.New("failed to execute record due to error in fetching outgoing frame")
			loop = false
		case <-ctx.Done():
			return nil
		}
	}
	utils.Stop(r.logger, stopReason)
	return recordErr
}

func (r *recorder) MockRecord(ctx context.Context) error {
	var outgoingFrameChan = make(chan models.Frame)
	var outgoingErrChan = make(chan models.OutgoingError)

	hookCtx, hookCancel := context.WithCancel(context.Background())
	outgoingCtx, outgoingCancel := context.WithCancel(context.Background())

	defer hookCancel()
	defer outgoingCancel()

	appId, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	err = r.instrumentation.Hook(hookCtx, appId, models.HookOptions{})
	if err != nil {
		return fmt.Errorf("failed to execute mock-record due to error while loading hooks and proxy: %w", err)
	}

	go func() {
		outgoingFrameChan, outgoingErrChan = r.instrumentation.GetOutgoing(outgoingCtx, appId, models.OutgoingOptions{})
	}()

	for {
		select {
		case frame := <-outgoingFrameChan:
			err := r.mockDB.InsertMock(context.Background(), frame, "")
			if err != nil {
				r.logger.Error("error while inserting outgoing frame into db", zap.Error(err))
				hookCancel()
				return errors.New("failed to execute record due to error in inserting outgoing frame into db")
			}
		case err := <-outgoingErrChan:
			r.logger.Error("error while fetching outgoing frame", zap.Error(err))
			hookCancel()
			return errors.New("failed to execute record due to error in fetching outgoing frame")
		case <-ctx.Done():
			return nil
		}
	}
}

func newTestSetId(testSetIds []string) (string, error) {
	indx := 0
	for _, testSetId := range testSetIds {
		namePackets := strings.Split(testSetId, "-")
		if len(namePackets) == 3 {
			testSetIndx, err := strconv.Atoi(namePackets[2])
			if err != nil {
				continue
			}
			if indx < testSetIndx+1 {
				indx = testSetIndx + 1
			}
		}
	}
	return fmt.Sprintf("%s%v", models.TestSetPattern, indx), nil
}
