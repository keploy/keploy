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

	"regexp"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func (o *Orchestrator) ReRecord(ctx context.Context) error {

	o.mockCorrelationManager = NewMockCorrelationManager(ctx, o.globalMockCh, o.logger)

	// Start the mock routing goroutine
	go o.mockCorrelationManager.routeMocks()

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

	var SelectedTests, ReRecordedTests []string
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

		mappingTestSet := testSet // Test set to store the mapping , have to get a new test set id if we are creating a new test set

		cfg := models.ReRecordCfg{
			Rerecord: true,
			TestSet:  testSet,
		}

		if !o.config.ReRecord.AmendTestSet {
			cfg.TestSet = ""
			mappingTestSet, err = o.record.GetNextTestSetID(recordCtx)
			if err != nil {
				errMsg := "failed to get next testset id"
				utils.LogError(o.logger, err, errMsg)
			}
		}

		isMappingEnabled := !o.config.DisableMapping

		//Keeping two back-to-back selects is used to not do blocking operation if parent ctx is done
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
				allRecorded, err := o.replayTests(recordCtx, testSet, mappingTestSet, isMappingEnabled)

				if allRecorded && err == nil {
					ReRecordedTests = append(ReRecordedTests, mappingTestSet)
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

		var errRecord error
		select {
		case errRecord = <-errCh:
			if errRecord != nil {
				stopReason = "error while starting the recording"
				utils.LogError(o.logger, errRecord, stopReason, zap.String("testset", testSet))
			}
		case errRecord = <-replayErrCh:
			if errRecord != nil {
				stopReason = "error while replaying the testcases"
				utils.LogError(o.logger, errRecord, stopReason, zap.String("testset", testSet))
			}
		case <-ctx.Done():
		}

		if errRecord == nil || ctx.Err() == nil {
			// Sleep for 3 seconds to ensure that the recording has completed
			time.Sleep(3 * time.Second)
		}

		recordCtxCancel()

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

	if !o.config.Test.DisableMockUpload {
		o.replay.UploadMocks(ctx, ReRecordedTests)
	} else {
		o.logger.Warn("To enable storing mocks in cloud, please use --disableMockUpload=false flag or test:disableMockUpload:false in config file")
	}

	if !o.config.InCi && !o.config.ReRecord.AmendTestSet {
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

func (o *Orchestrator) replayTests(ctx context.Context, testSet string, mappingTestSet string, isMappingEnabled bool) (bool, error) {

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
	delay := o.config.Test.Delay
	time.Sleep(time.Duration(delay) * time.Second)
	if utils.IsDockerCmd(cmdType) {
		host = o.config.ContainerName
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

	// ------------------------------------------------------------
	// Build usage tracking: which template keys are referenced by which testcases.
	// This allows us to update only the affected testcases when a template value changes.
	// Tracks both placeholder usage ({{type .key}}) and literal usage (raw current value in URL/header/body).
	usageMap := make(map[string]map[*models.TestCase]struct{})
	placeholderRe := regexp.MustCompile(`\{\{[^{}]*?\.([a-zA-Z0-9_]+)\}\}`)
	// Initialize set for each existing template key
	for k := range utils.TemplatizedValues {
		usageMap[k] = make(map[*models.TestCase]struct{})
	}

	// Track which template keys appear in any response body (treat as potential producers to avoid overwriting)
	producerKeys := make(map[string]struct{})

	for _, tc := range tcs {
		// Scan for placeholder occurrences in URL, headers, body
		// URL
		for _, m := range placeholderRe.FindAllStringSubmatch(tc.HTTPReq.URL, -1) {
			key := m[1]
			if _, ok := usageMap[key]; !ok {
				usageMap[key] = make(map[*models.TestCase]struct{})
			}
			usageMap[key][tc] = struct{}{}
		}
		// Headers
		for _, hv := range tc.HTTPReq.Header {
			for _, m := range placeholderRe.FindAllStringSubmatch(hv, -1) {
				key := m[1]
				if _, ok := usageMap[key]; !ok {
					usageMap[key] = make(map[*models.TestCase]struct{})
				}
				usageMap[key][tc] = struct{}{}
			}
		}
		// Body
		for _, m := range placeholderRe.FindAllStringSubmatch(tc.HTTPReq.Body, -1) {
			key := m[1]
			if _, ok := usageMap[key]; !ok {
				usageMap[key] = make(map[*models.TestCase]struct{})
			}
			usageMap[key][tc] = struct{}{}
		}

		// Response body placeholders -> mark as producer
		for _, m := range placeholderRe.FindAllStringSubmatch(tc.HTTPResp.Body, -1) {
			producerKeys[m[1]] = struct{}{}
		}

		// Literal usages: check each template key's current value appears without placeholders.
		for key, val := range utils.TemplatizedValues {
			lit := fmt.Sprintf("%v", val)
			if lit == "" { // skip empty
				continue
			}
			addIfLiteral := func(s string) {
				if s == "" || strings.Contains(s, "{{") { // skip if already templated
					return
				}
				if strings.Contains(s, lit) { // simple containment; over-match risk accepted
					if _, ok := usageMap[key]; !ok {
						usageMap[key] = make(map[*models.TestCase]struct{})
					}
					usageMap[key][tc] = struct{}{}
				}
			}
			addIfLiteral(tc.HTTPReq.URL)
			addIfLiteral(tc.HTTPReq.Body)
			for _, hv := range tc.HTTPReq.Header {
				addIfLiteral(hv)
			}
		}
	}
	// ------------------------------------------------------------
	var simErr bool
	for _, tc := range tcs {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		// Register test case with correlation manager
		testCaseCtx := TestContext{
			TestID:  tc.Name,
			TestSet: testSet,
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
				case <-ctx.Done():
					return
				}
			}
		}(tc.Name)

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
		// Snapshot current template values before simulating request; SimulateHTTP may update them.
		prevVals := make(map[string]interface{}, len(utils.TemplatizedValues))
		for k, v := range utils.TemplatizedValues {
			prevVals[k] = v
		}

		// Create a rendered copy of testcase with current template/secret values
		// so that comparison uses concrete expected values (not the templatized form)
		renderedTC, renderErr := pkg.RenderTestCaseWithTemplates(tc)
		if renderErr != nil {
			utils.LogError(o.logger, renderErr, "failed to render testcase with templates")
			// fallback to using the original testcase
			renderedTC = tc
		}

		// Store the original test case response for comparison using the rendered copy
		originalTestCase := *renderedTC
		// Detect noise fields introduced by templating and mark them on the testcase
		if len(utils.TemplatizedValues) > 0 {
			detected := pkg.DetectNoiseFieldsInResp(&models.HTTPResp{
				StatusCode: renderedTC.HTTPResp.StatusCode,
				Body:       renderedTC.HTTPResp.Body,
				Header:     renderedTC.HTTPResp.Header,
			})
			o.logger.Debug("Detected noise fields", zap.Any("fields", detected))
			// merge detected into originalTestCase.Noise
			if originalTestCase.Noise == nil {
				originalTestCase.Noise = map[string][]string{}
			}
			for k := range detected {
				if _, ok := originalTestCase.Noise[k]; !ok {
					originalTestCase.Noise[k] = []string{}
				}
			}
		}

		resp, err := pkg.SimulateHTTP(ctx, tc, testSet, o.logger, o.config.Test.APITimeout, o.config.ReRecord.Port)
		if err != nil {
			utils.LogError(o.logger, err, "failed to simulate HTTP request")
			if resp == nil {
				allTcRecorded = false
			}
			simErr = true
			continue
		}
		o.logger.Debug("Response received for testcase",
			zap.String("testcase", tc.Name),
			zap.Int("response_code", resp.StatusCode),
		)
		// Compare the new response with the original test case response and show diff (optional via flag)
		if o.config.ReRecord.ShowDiff && resp != nil {
			o.showResponseDiff(&originalTestCase, resp, testSet)
		}

		if resp != nil && resp.Body != "" && len(utils.TemplatizedValues) > 0 {
			// SimulateHTTP already updated templates; now perform propagation if any changed.
			for key, newVal := range utils.TemplatizedValues {
				oldVal, existed := prevVals[key]
				if !existed { // newly introduced key -> skip propagation (no literal users yet tracked)
					continue
				}
				if fmt.Sprintf("%v", oldVal) == fmt.Sprintf("%v", newVal) {
					continue
				}
				oldStr := fmt.Sprintf("%v", oldVal)
				newStr := fmt.Sprintf("%v", newVal)
				base := strings.TrimRightFunc(key, func(r rune) bool { return r >= '0' && r <= '9' })
				if base == "" {
					base = key
				}
				for sibling, val := range utils.TemplatizedValues {
					if sibling == key || !strings.HasPrefix(sibling, base) {
						continue
					}
					if _, isProducer := producerKeys[sibling]; isProducer {
						continue
					}
					if fmt.Sprintf("%v", val) == oldStr {
						utils.TemplatizedValues[sibling] = newVal
					}
				}
				for future := range usageMap[key] {
					if future.Name == tc.Name {
						continue
					}
					if future.HTTPReq.URL != "" && !strings.Contains(future.HTTPReq.URL, "{{") && strings.Contains(future.HTTPReq.URL, oldStr) {
						future.HTTPReq.URL = strings.ReplaceAll(future.HTTPReq.URL, oldStr, newStr)
					}
					for hk, hv := range future.HTTPReq.Header {
						if hv == oldStr {
							future.HTTPReq.Header[hk] = newStr
						}
					}
					if body := future.HTTPReq.Body; body != "" && !strings.Contains(body, "{{") && strings.Contains(body, oldStr) {
						future.HTTPReq.Body = strings.ReplaceAll(body, oldStr, newStr)
					}
				}
			}
			// Persist any template changes (best-effort) after propagation
			if err := o.replay.UpdateTestSetTemplate(ctx, testSet, utils.TemplatizedValues); err != nil {
				o.logger.Warn("failed to persist updated template values during rerecord", zap.String("testSet", testSet), zap.Error(err))
			} else {
				o.logger.Debug("updated template values during rerecord", zap.String("testSet", testSet), zap.Any("template", utils.TemplatizedValues))
			}
		}

		o.logger.Info("Re-recorded the testcase successfully", zap.String("testcase", tc.Name), zap.String("of testset", testSet))

		time.Sleep(100 * time.Millisecond)

		// Unregister test case and collect mocks
		o.mockCorrelationManager.UnregisterTest(tc.Name)

		// Wait a bit for any remaining mocks to be collected
		<-mockCollectionDone

		// Store collected mocks for this test case
		mockMutex.Lock()
		finalMocks := make([]*models.Mock, len(collectedMocks))
		copy(finalMocks, collectedMocks)
		mockMutex.Unlock()
		if len(finalMocks) > 0 {
			mappings[tc.Name] = make([]string, 0)
			for _, mock := range finalMocks {
				mappings[tc.Name] = append(mappings[tc.Name], mock.Name)
			}
		}
	}

	// Save the test-mock mappings to YAML file
	if len(mappings) > 0 && isMappingEnabled {
		mapping := &models.Mapping{
			Version:   string(models.GetVersion()), // or models.GetVersion() casted
			Kind:      models.MappingKind,
			TestSetID: mappingTestSet,
		}
		for tcID, mocks := range mappings {
			mapping.Tests = append(mapping.Tests, models.Test{
				ID:    tcID,
				Mocks: models.FromSlice(mocks),
			})
		}

		err := o.replay.StoreMappings(ctx, mapping)
		if err != nil {
			o.logger.Error("Error saving test-mock mappings to YAML file", zap.Error(err))
		} else {
			o.logger.Info("Successfully saved test-mock mappings",
				zap.String("testSetID", mappingTestSet),
				zap.Int("numTests", len(mappings)))
		}
	}

	if simErr {
		return allTcRecorded, fmt.Errorf("got error while simulating HTTP request. Please make sure the related services are up and running")
	}

	return allTcRecorded, nil
}

// checkForTemplates checks if the testcases are already templatized. If not, it asks the user if they want to templatize the testcases before re-recording
// showResponseDiff compares the original test case response with the newly recorded response
// and displays the differences using the existing replay service functions
func (o *Orchestrator) showResponseDiff(originalTC *models.TestCase, newResp *models.HTTPResp, testSetID string) {
	// Use the existing replay service comparison functions
	switch originalTC.Kind {
	case models.HTTP:
		// Use the HTTP matcher to compare responses - this will automatically show diffs
		matched, _ := o.replay.CompareHTTPResp(originalTC, newResp, testSetID, true)
		if !matched {
			o.logger.Info("Response differences detected during re-record",
				zap.String("testcase", originalTC.Name),
				zap.String("testset", testSetID))
		} else {
			o.logger.Debug("No response differences detected during re-record",
				zap.String("testcase", originalTC.Name),
				zap.String("testset", testSetID))
		}
	case models.GRPC_EXPORT:
		// For gRPC, we need to handle the case where SimulateHTTP returns HTTP response
		// but we want to compare with gRPC response. We'll log this limitation.
		o.logger.Info("gRPC response comparison during re-record",
			zap.String("testcase", originalTC.Name),
			zap.String("testset", testSetID),
			zap.String("note", "gRPC test cases are simulated as HTTP during re-record, comparison limited"))

		// For gRPC, we'll do a basic comparison since SimulateHTTP returns HTTP response
		o.logger.Info("gRPC Response differences detected during re-record",
			zap.String("testcase", originalTC.Name),
			zap.String("testset", testSetID),
			zap.String("note", "Detailed gRPC comparison not available during re-record"))
	}
}

func (o *Orchestrator) checkForTemplates(ctx context.Context, testSets []string) {
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
