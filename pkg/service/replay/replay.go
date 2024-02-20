package replay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/graph/model"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type replayer struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	reportDB        ReportDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          config.Config
}

func NewReplayer(logger *zap.Logger, testDB TestDB, mockDB MockDB, reportDB ReportDB, telemetry Telemetry, instrumentation Instrumentation, config config.Config) Service {
	return &replayer{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		reportDB:        reportDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
	}
}

func (r *replayer) Replay(ctx context.Context) error {
	var stopReason = "User stopped replay"
	testRunId, err := r.BootReplay(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to boot replay: %v", err)
		r.logger.Error(stopReason, zap.Error(err))
		return errors.New("failed to execute replay due to error in booting replay")
	}
	testSetIds, err := r.testDB.GetAllTestSetIds(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get all test set ids: %v", err)
		r.logger.Error(stopReason, zap.Error(err))
		return errors.New("failed to execute replay due to error in getting all test set ids")
	}
	for _, testSetId := range testSetIds {
		testReport, err := r.RunTestSet(ctx, testSetId)
		if err != nil {
			stopReason = fmt.Sprintf("failed to run test set: %v", err)
			r.logger.Error(stopReason, zap.Error(err))
			return errors.New("failed to execute replay due to error in running test set")
		}
		err = r.reportDB.InsertReport(ctx, testRunId, testSetId, testReport)
		if err != nil {
			stopReason = fmt.Sprintf("failed to insert report: %v", err)
			r.logger.Error(stopReason, zap.Error(err))
			return errors.New("failed to execute replay due to error in inserting report")
		}
	}
	utils.Stop(r.logger, "replay completed")
	return nil
}

func (r *replayer) BootReplay(ctx context.Context) (string, int, error) {
	testRunIds, err := r.reportDB.GetAllTestRunIds(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get all test run ids: %w", err)
	}
	testRunId := pkg.NewId(testRunIds, models.TestRunTemplateName)

	appId, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to setup instrumentation: %w", err)
	}

	err = r.instrumentation.Hook(ctx, appId, models.HookOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to start the hooks and proxy: %w", err)
	}

	return testRunId, appId, nil
}

func (r *replayer) GetAllTestSetIds(ctx context.Context) ([]string, error) {
	return r.testDB.GetAllTestSetIds(ctx)
}

func (r *replayer) RunTestSet(ctx context.Context, testSetId string, appId int) (models.TestReport, error) {
	testCases, err := r.testDB.GetTestCases(ctx, testSetId)
	if err != nil {
		return models.TestReport{}, fmt.Errorf("failed to get test cases: %w", err)
	}
	mocks, err := r.mockDB.GetMocks(ctx, testSetId, "", time.Now())
	err = r.instrumentation.SetMocks(ctx, appId, mocks)
	if err != nil {
		return models.TestReport{}, fmt.Errorf("failed to set mocks: %w", err)
	}
	return nil
}

func (r *replayer) GetTestSetStatus(ctx context.Context, testRunId string, testSetId string) (model.TestSetStatus, error) {
	testReport, err := r.reportDB.GetReport(ctx, testRunId, testSetId)
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to get report: %w", err)
	}
	return model.TestSetStatus{
		Status: testReport.Status,
	}, nil
}
