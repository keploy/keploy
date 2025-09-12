//go:build linux

package orchestrator

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func (o *Orchestrator) ReRecord(ctx context.Context) error {
	// Initialize mock correlation manager for this rerecord session
	o.InitializeMockCorrelationManager(ctx)

	// Set the global mock channel on the record service
	o.record.SetGlobalMockChannel(o.GetGlobalMockChannel())

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	var stopReason string
	var err error

	defer func() {
		select {
		case <-ctx.Done():
		default:
			err := utils.Stop(o.logger, stopReason)
			if err != nil {
				utils.LogError(o.logger, err, "failed to stop recording")
			}
		}
	}()

	// Get all the testsets
	testSets, err := o.replay.GetAllTestSetIDs(ctx)
	if err != nil {
		errMsg := "Failed to get all testset IDs"
		utils.LogError(o.logger, err, errMsg)
		return err
	}

	// Check for templates
	o.checkForTemplates(ctx, testSets)
	// Sort the testsets to ensure that the testcases are re-recorded in the same order
	sort.SliceStable(testSets, func(i, j int) bool {
		return testSets[i] < testSets[j]
	})

	var SelectedTests []string

	for _, testSet := range testSets {
		if ctx.Err() != nil {
			break
		}

		if _, ok := o.config.Test.SelectedTests[testSet]; !ok && len(o.config.Test.SelectedTests) != 0 {
			continue
		}

		SelectedTests = append(SelectedTests, testSet)

		o.logger.Info("Re-recording testcases for the given testset", zap.String("testset", testSet))
		// Note: Here we've used child context without cancel to avoid the cancellation of the parent context.
		// When we use errgroup and get an error from any of the go routines spawned by errgroup, it cancels the parent context.
		// We don't want to stop the execution if there is an error in any of the test-set recording sessions, it should just skip that test-set and continue with the next one.
		errGrp, _ := errgroup.WithContext(ctx)
		recordCtx := context.WithoutCancel(ctx)
		recordCtx, recordCtxCancel := context.WithCancel(recordCtx)

		var errCh = make(chan error, 1)
		var replayErrCh = make(chan error, 1)

		//Keeping two back-to-back selects is used to not do blocking operation if parent ctx is done
		cfg := models.ReRecordCfg{
			Rerecord: true,
			TestSet:  testSet,
		}
		if o.config.ReRecord.CreateTestSet {
			cfg.TestSet = ""
		}
		select {
		case <-ctx.Done():
		default:
			errGrp.Go(func() error {
				defer utils.Recover(o.logger)
				err := o.record.Start(recordCtx, cfg)
				errCh <- err
				return nil
			})
		}

		select {
		case <-ctx.Done():
		default:
			errGrp.Go(func() error {
				defer utils.Recover(o.logger)
				allRecorded, err := o.replayTests(recordCtx, testSet)

				if allRecorded && err == nil {
					o.logger.Info("Re-recorded testcases successfully for the given testset", zap.String("testset", testSet))
				}
				if !allRecorded {
					o.logger.Warn("Failed to re-record some testcases", zap.String("testset", testSet))
					stopReason = "failed to re-record some testcases"
				}

				replayErrCh <- err
				return nil
			})
		}

		var err error
		select {
		case err = <-errCh:
			if err != nil {
				stopReason = "error while starting the recording"
				utils.LogError(o.logger, err, stopReason, zap.String("testset", testSet))
			}
		case err = <-replayErrCh:
			if err != nil {
				stopReason = "error while replaying the testcases"
				utils.LogError(o.logger, err, stopReason, zap.String("testset", testSet))
			}
		case <-ctx.Done():
		}

		if err == nil || ctx.Err() == nil {
			// Sleep for 3 seconds to ensure that the recording has completed
			time.Sleep(3 * time.Second)
		}

		recordCtxCancel()

		// Wait for the recording to stop
		err = errGrp.Wait()
		if err != nil {
			utils.LogError(o.logger, err, "failed to stop re-recording")
		}

		// Check if the global context is done after each iteration
		if ctx.Err() != nil {
			break
		}
	}

	if stopReason != "" {
		utils.LogError(o.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	if ctx.Err() != nil {
		stopReason = "context cancelled"
		o.logger.Warn("Re-record was cancelled, keploy might have not recorded few test cases")
		return nil
	}
	stopReason = "Re-recorded all the selected testsets successfully"
	if !o.config.InCi && o.config.ReRecord.CreateTestSet {
		o.logger.Info("Re-record was successfull. Do you want to remove the older testsets? (y/n)", zap.Any("testsets", SelectedTests))
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			o.logger.Warn("Failed to read input. The older testsets will be kept.")
			return nil
		}

		if len(input) == 0 {
			o.logger.Warn("Empty input. The older testsets will be kept.")
			return nil
		}
		// Trimming the newline character for cleaner switch statement
		input = input[:len(input)-1]
		switch input {
		case "y", "Y":
			for _, testSet := range SelectedTests {
				err := o.replay.DeleteTestSet(ctx, testSet)
				if err != nil {
					o.logger.Warn("Failed to delete the testset", zap.String("testset", testSet))
				}
			}
			o.logger.Info("Deleted the older testsets successfully")
		case "n", "N":
			o.logger.Info("skipping the deletion of older testsets")
		default:
			o.logger.Warn("Invalid input. The older testsets will be kept.")
		}
	}
	return nil
}

func (o *Orchestrator) replayTests(ctx context.Context, testSet string) (bool, error) {

	var mappings = make(map[string][]string)

	//replay the recorded testcases
	tcs, err := o.replay.GetTestCases(ctx, testSet)
	if err != nil {
		errMsg := "failed to get all testcases"
		utils.LogError(o.logger, err, errMsg, zap.String("testset", testSet))
		return false, fmt.Errorf("%s", errMsg)
	}

	if len(tcs) == 0 {
		o.logger.Warn("No testcases found for the given testset", zap.String("testset", testSet))
		return false, nil
	}

	host, port, err := pkg.ExtractHostAndPort(tcs[0].Curl)
	if err != nil {
		errMsg := "failed to extract host and port"
		utils.LogError(o.logger, err, "")
		o.logger.Debug("", zap.String("curl", tcs[0].Curl))
		return false, fmt.Errorf("%s", errMsg)
	}
	cmdType := utils.CmdType(o.config.CommandType)
	var userIP string
	delay := o.config.Test.Delay
	time.Sleep(time.Duration(delay) * time.Second)
	if utils.IsDockerCmd(cmdType) {
		host = o.config.ContainerName
		userIP, err = o.record.GetContainerIP(ctx, o.config.AppID)
		if err != nil {
			utils.LogError(o.logger, err, "failed to get the app ip")
			return false, err
		}
	}
	timeout := time.Duration(120+delay) * time.Second

	o.logger.Debug("", zap.String("host", host), zap.String("port", port), zap.Duration("WaitTimeout", timeout), zap.String("CommandType", string(cmdType)))

	if err := pkg.WaitForPort(ctx, host, port, timeout); err != nil {
		utils.LogError(o.logger, err, "Waiting for port failed", zap.String("host", host), zap.String("port", port))
		return false, err
	}

	// Read the template and secret values once per test set
	testSetConf, err := o.replay.GetTestSetConf(ctx, testSet)
	if err != nil {
		o.logger.Debug("failed to read template values")
	}

	utils.TemplatizedValues = map[string]interface{}{}
	utils.SecretValues = map[string]interface{}{}

	if testSetConf != nil {
		if testSetConf.Template != nil {
			utils.TemplatizedValues = testSetConf.Template
		}

		if testSetConf.Secret != nil {
			utils.SecretValues = testSetConf.Secret
		}
	}

	allTcRecorded := true
	var simErr bool
	for _, tc := range tcs {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		// Generate unique test case execution ID
		testCaseID := tc.Name

		// Register test case with correlation manager
		testCaseCtx := TestContext{
			TestID:    testCaseID,
			TestName:  tc.Name,
			TestSet:   testSet,
			StartTime: time.Now(),
		}

		o.mockCorrelationManager.RegisterTest(testCaseCtx)

		// Start mock collection for this test case in background
		collectedMocks := make([]*models.Mock, 0)
		mockCollectionDone := make(chan struct{})
		var mockMutex sync.Mutex

		go func(tcID string) {
			defer close(mockCollectionDone)
			mockCh := o.mockCorrelationManager.GetTestMocks(tcID)
			if mockCh == nil {
				return
			}

			for {
				select {
				case mock, ok := <-mockCh:
					if !ok {
						return // Channel closed
					}
					mockMutex.Lock()
					collectedMocks = append(collectedMocks, mock)
					mockMutex.Unlock()
					o.logger.Info("Collected mock for test case",
						zap.String("testCaseID", tcID),
						zap.String("testCaseName", tc.Name),
						zap.String("mockType", mock.GetKind()))
				case <-ctx.Done():
					return
				}
			}
		}(testCaseID)

		if utils.IsDockerCmd(cmdType) {
			tc.HTTPReq.URL, err = utils.ReplaceHost(tc.HTTPReq.URL, userIP)
			if err != nil {
				utils.LogError(o.logger, err, "failed to replace host to docker container's IP")
				break
			}
			o.logger.Debug("", zap.String("replaced_url_in_docker_env", tc.HTTPReq.URL))
		}

		if o.config.ReRecord.Host != "" {
			tc.HTTPReq.URL, err = utils.ReplaceHost(tc.HTTPReq.URL, o.config.ReRecord.Host)
			if err != nil {
				utils.LogError(o.logger, err, "failed to replace host to provided host by the user")
				break
			}
		}

		if o.config.ReRecord.Port != 0 && tc.Kind == models.HTTP {
			tc.HTTPReq.URL, err = utils.ReplacePort(tc.HTTPReq.URL, strconv.Itoa(int(o.config.ReRecord.Port)))
			if err != nil {
				utils.LogError(o.logger, err, "failed to replace http port to provided port by the user")
				break
			}
		}

		if o.config.ReRecord.GRPCPort != 0 && tc.Kind == models.GRPC_EXPORT {
			tc.GrpcReq.Headers.PseudoHeaders[":authority"], err = utils.ReplaceGrpcPort(tc.GrpcReq.Headers.PseudoHeaders[":authority"], strconv.Itoa(int(o.config.ReRecord.GRPCPort)))
			if err != nil {
				utils.LogError(o.logger, err, "failed to replace grpc port to provided grpc port by the user")
				break
			}
		}

		resp, err := pkg.SimulateHTTP(ctx, tc, testSet, o.logger, o.config.Test.APITimeout)
		if err != nil {
			utils.LogError(o.logger, err, "failed to simulate HTTP request")
			if resp == nil {
				allTcRecorded = false
			}
			simErr = true
			continue // Proceed with the next command
		}

		o.logger.Info("Re-recorded the testcase successfully", zap.String("testcase", tc.Name), zap.String("of testset", testSet))

		time.Sleep(100 * time.Millisecond)

		// Unregister test case and collect mocks
		o.mockCorrelationManager.UnregisterTest(testCaseID)

		// Wait a bit for any remaining mocks to be collected
		<-mockCollectionDone

		// Store collected mocks for this test case
		mockMutex.Lock()
		finalMocks := make([]*models.Mock, len(collectedMocks))
		copy(finalMocks, collectedMocks)
		mockMutex.Unlock()
		o.storeMocksForTestCase(testCaseID, tc.Name, testSet, finalMocks)
		mappings[tc.Name] = make([]string, 0)
		for _, mock := range finalMocks {
			mappings[tc.Name] = append(mappings[tc.Name], mock.Name)
		}
	}

	// Save the test-mock mappings to YAML file
	if len(mappings) > 0 {
		err := o.replay.StoreMappings(ctx, testSet, mappings)
		if err != nil {
			o.logger.Error("Error saving test-mock mappings to YAML file", zap.Error(err))
		} else {
			o.logger.Info("Successfully saved test-mock mappings",
				zap.String("testSetID", testSet),
				zap.Int("numTests", len(mappings)))
		}
	}

	if simErr {
		return allTcRecorded, fmt.Errorf("got error while simulating HTTP request. Please make sure the related services are up and running")
	}

	return allTcRecorded, nil
}

// storeMocksForTestCase stores the collected mocks for a specific test case
func (o *Orchestrator) storeMocksForTestCase(testCaseID string, testCaseName string, testSet string, mocks []*models.Mock) {
	if len(mocks) > 0 {
		o.logger.Info("Collected mocks for test case",
			zap.String("testCaseID", testCaseID),
			zap.String("testCaseName", testCaseName),
			zap.String("testSet", testSet),
			zap.Any("mocksCount", len(mocks)))

		// Here you can implement storage logic for the collected mocks
		// For example, save to database, file, or process them
		for i, mock := range mocks {
			o.logger.Info("Mock details",
				zap.String("testCaseID", testCaseID),
				zap.String("testCaseName", testCaseName),
				zap.String("testSet", testSet),
				zap.Int("mockIndex", i),
				zap.String("mockKind", mock.GetKind()),
				zap.String("mockName", mock.Name))
		}
	} else {
		o.logger.Debug("No mocks collected for test case",
			zap.String("testCaseID", testCaseID),
			zap.String("testCaseName", testCaseName),
			zap.String("testSet", testSet))
	}
}

// checkForTemplates checks if the testcases are already templatized. If not, it asks the user if they want to templatize the testcases before re-recording
func (o *Orchestrator) checkForTemplates(ctx context.Context, testSets []string) {
	// Check if the testcases are already templatized.
	var nonTemplatized []string
	for _, testSet := range testSets {
		if _, ok := o.config.Test.SelectedTests[testSet]; !ok && len(o.config.Test.SelectedTests) != 0 {
			continue
		}

		conf, err := o.replay.GetTestSetConf(ctx, testSet)
		if err != nil || conf == nil || conf.Template == nil {
			nonTemplatized = append(nonTemplatized, testSet)
		}
	}

	if len(nonTemplatized) == 0 {
		return
	}

	o.config.Templatize.TestSets = nonTemplatized
	o.logger.Warn("The following testSets are not templatized. Do you want to templatize them to handle noisy fields?(y/n)", zap.Any("testSets", nonTemplatized))
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		o.logger.Warn("failed to read input. Skipping templatization")
	}
	if input == "n\n" || input == "N\n" {
		o.logger.Info("skipping templatization")
		return
	}

	if input == "y\n" || input == "Y\n" {
		if err := o.tools.Templatize(ctx); err != nil {
			utils.LogError(o.logger, err, "failed to templatize test cases, skipping templatization")
		}
	}
}
