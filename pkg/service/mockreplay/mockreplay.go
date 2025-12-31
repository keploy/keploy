package mockreplay

import (
	"context"
	"fmt"
	"time"

	"facette.io/natsort"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	replaySvc "go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type MockReplayer struct {
	logger          *zap.Logger
	mockDB          MockDB
	testDB          TestDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          *config.Config
}

func New(logger *zap.Logger, mockDB MockDB, testDB TestDB, telemetry Telemetry, instrumentation Instrumentation, cfg *config.Config) Service {
	return &MockReplayer{
		logger:          logger,
		mockDB:          mockDB,
		testDB:          testDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          cfg,
	}
}

func (r *MockReplayer) Start(ctx context.Context) error {
	if r.config.Command == "" {
		return fmt.Errorf("mock replay requires a command to run")
	}

	errGrp, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, errGrp))
	defer func() {
		cancel()
		if err := errGrp.Wait(); err != nil {
			utils.LogError(r.logger, err, "failed to stop mock replay")
		}
	}()

	r.logger.Info("🟢 Starting Keploy mock replay...")

	passPorts := config.GetByPassPorts(r.config)
	err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{
		Container:         r.config.ContainerName,
		CommandType:       r.config.CommandType,
		DockerDelay:       r.config.BuildDelay,
		Mode:              models.MODE_TEST,
		BuildDelay:        r.config.BuildDelay,
		EnableTesting:     true,
		GlobalPassthrough: r.config.Record.GlobalPassthrough,
		PassThroughPorts:  passPorts,
		ConfigPath:        r.config.ConfigPath,
	})
	if err != nil {
		stopReason := "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason := fmt.Sprintf("failed to get mock set ids: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	if len(testSetIDs) == 0 {
		recordCmd := models.HighlightGrayString("keploy mock record")
		errMsg := fmt.Sprintf("No mock sets found in the keploy folder. Please record mocks using %s command", recordCmd)
		utils.LogError(r.logger, nil, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	selectedSets := r.selectMockSets(testSetIDs)
	if len(selectedSets) == 0 {
		errMsg := "No matching mock sets found for the requested test sets"
		utils.LogError(r.logger, nil, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	natsort.Sort(selectedSets)
	r.logger.Info("Mock sets to be replayed", zap.Strings("mockSets", selectedSets))

	isDockerCompose := r.config.CommandType == string(utils.DockerCompose)
	var appErrChan chan models.AppError
	if isDockerCompose {
		appErrChan = make(chan models.AppError, 1)
		go func() {
			appErrChan <- r.instrumentation.Run(ctx, models.RunOptions{})
		}()

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

	filtered, unfiltered, err := r.loadMocks(ctx, selectedSets)
	if err != nil {
		return err
	}

	if len(r.config.Test.SelectedMocks) > 0 {
		filtered = filterMocksByName(filtered, r.config.Test.SelectedMocks)
		unfiltered = filterMocksByName(unfiltered, r.config.Test.SelectedMocks)
		r.logger.Info("Using selected mocks", zap.Strings("selectedMocks", r.config.Test.SelectedMocks))
	}

	if len(filtered) == 0 && len(unfiltered) == 0 {
		errMsg := "No mocks found for the selected mock sets"
		utils.LogError(r.logger, nil, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	r.logger.Info("Loaded mocks", zap.Int("filtered", len(filtered)), zap.Int("unfiltered", len(unfiltered)))

	err = r.instrumentation.StoreMocks(ctx, filtered, unfiltered)
	if err != nil {
		utils.LogError(r.logger, err, "failed to store mocks on agent")
		return err
	}

	headerNoiseConfig := replaySvc.PrepareHeaderNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, selectedSets[0])
	backdate := earliestMockTimestamp(filtered, unfiltered)

	err = r.instrumentation.MockOutgoing(ctx, models.OutgoingOptions{
		Rules:          r.config.BypassRules,
		MongoPassword:  r.config.Test.MongoPassword,
		SQLDelay:       time.Duration(r.config.Test.Delay),
		FallBackOnMiss: r.config.Test.FallBackOnMiss,
		Mocking:        r.config.Test.Mocking,
		Backdate:       backdate,
		NoiseConfig:    headerNoiseConfig,
	})
	if err != nil {
		utils.LogError(r.logger, err, "failed to mock outgoing")
		return err
	}

	err = r.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime:       models.BaseTime,
		BeforeTime:      time.Now(),
		UseMappingBased: false,
	})
	if err != nil {
		utils.LogError(r.logger, err, "failed to update mock params on agent")
		return err
	}

	if isDockerCompose {
		err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
		}
	}

	defer func() {
		consumedMocks, err := r.instrumentation.GetConsumedMocks(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "failed to get consumed mocks for mock replay telemetry")
			return
		}
		r.telemetry.MockTestRun(len(consumedMocks))
	}()

	if !isDockerCompose {
		appErr := r.instrumentation.Run(ctx, models.RunOptions{})
		return r.handleAppError(ctx, appErr)
	}

	select {
	case appErr := <-appErrChan:
		return r.handleAppError(ctx, appErr)
	case <-ctx.Done():
		return nil
	}
}

func (r *MockReplayer) selectMockSets(allSets []string) []string {
	if len(r.config.Test.SelectedTests) == 0 {
		return allSets
	}
	selected := make([]string, 0, len(allSets))
	for _, testSetID := range allSets {
		if _, ok := r.config.Test.SelectedTests[testSetID]; ok {
			selected = append(selected, testSetID)
		}
	}
	return selected
}

func (r *MockReplayer) loadMocks(ctx context.Context, testSetIDs []string) ([]*models.Mock, []*models.Mock, error) {
	var filtered []*models.Mock
	var unfiltered []*models.Mock

	for _, testSetID := range testSetIDs {
		filteredMocks, err := r.mockDB.GetFilteredMocks(ctx, testSetID, models.BaseTime, time.Now())
		if err != nil {
			utils.LogError(r.logger, err, "failed to read filtered mocks", zap.String("mockSet", testSetID))
			return nil, nil, err
		}
		unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(ctx, testSetID, models.BaseTime, time.Now())
		if err != nil {
			utils.LogError(r.logger, err, "failed to read unfiltered mocks", zap.String("mockSet", testSetID))
			return nil, nil, err
		}
		filtered = append(filtered, filteredMocks...)
		unfiltered = append(unfiltered, unfilteredMocks...)
	}

	return filtered, unfiltered, nil
}

func (r *MockReplayer) handleAppError(ctx context.Context, appErr models.AppError) error {
	switch appErr.AppErrorType {
	case models.ErrCtxCanceled:
		return nil
	case models.ErrAppStopped, models.ErrTestBinStopped:
		return nil
	case models.ErrCommandError:
		return fmt.Errorf("error in running the user command")
	case models.ErrInternal:
		return fmt.Errorf("internal error occurred while hooking into the application")
	case models.ErrUnExpected:
		return fmt.Errorf("user command terminated unexpectedly, please check application logs")
	default:
		if ctx.Err() != context.Canceled {
			return fmt.Errorf("unknown error received from application")
		}
		return nil
	}
}

func filterMocksByName(mocks []*models.Mock, selectedNames []string) []*models.Mock {
	if len(selectedNames) == 0 {
		return mocks
	}
	lookup := make(map[string]struct{}, len(selectedNames))
	for _, name := range selectedNames {
		lookup[name] = struct{}{}
	}
	filtered := make([]*models.Mock, 0, len(mocks))
	for _, mock := range mocks {
		if mock == nil {
			continue
		}
		if _, ok := lookup[mock.Name]; ok {
			filtered = append(filtered, mock)
		}
	}
	return filtered
}

func earliestMockTimestamp(filtered []*models.Mock, unfiltered []*models.Mock) time.Time {
	all := make([]*models.Mock, 0, len(filtered)+len(unfiltered))
	all = append(all, filtered...)
	all = append(all, unfiltered...)

	var earliest time.Time
	for _, mock := range all {
		ts := mockTimestamp(mock)
		if ts.IsZero() {
			continue
		}
		if earliest.IsZero() || ts.Before(earliest) {
			earliest = ts
		}
	}
	return earliest
}

func mockTimestamp(mock *models.Mock) time.Time {
	if mock == nil {
		return time.Time{}
	}
	if !mock.Spec.ReqTimestampMock.IsZero() {
		return mock.Spec.ReqTimestampMock
	}
	if mock.Spec.HTTPReq != nil && !mock.Spec.HTTPReq.Timestamp.IsZero() {
		return mock.Spec.HTTPReq.Timestamp
	}
	if mock.Spec.GRPCReq != nil && !mock.Spec.GRPCReq.Timestamp.IsZero() {
		return mock.Spec.GRPCReq.Timestamp
	}
	return time.Time{}
}
