//go:build linux

package orchestrator

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"encoding/json"
	"regexp"
	"strings"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// normalizeKey converts a key to a canonical form for comparison.
func normalizeKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "-", "")
	k = strings.ReplaceAll(k, "_", "")
	return k
}

// *** NEW: Generic function to extract all primitive fields (string, number, bool) ***
// This replaces the old string-only `extractStringFields`.
func extractAllPrimitiveFields(v interface{}, out map[string]interface{}) {
	switch val := v.(type) {
	case map[string]interface{}:
		for k, inner := range val {
			normKey := normalizeKey(k)
			switch innerTyped := inner.(type) {
			// Found a primitive value, store it with its normalized key.
			case string, float64, bool:
				// We only store the first occurrence of a key to keep it simple.
				if _, exists := out[normKey]; !exists {
					out[normKey] = innerTyped
				}
			default:
				// Recurse into nested objects/arrays.
				extractAllPrimitiveFields(innerTyped, out)
			}
		}
	case []interface{}:
		for _, elem := range val {
			extractAllPrimitiveFields(elem, out)
		}
	}
}

// *** REWRITTEN & SIMPLIFIED: A robust function to update templates from any primitive type in a JSON response ***
// This version is non-recursive and focuses on top-level fields for stability.
// *** FINAL REWRITE: This version correctly converts json.Number to standard Go types ***
func updateTemplatesFromJSON(body []byte, templates map[string]interface{}, logger *zap.Logger) bool {
	if len(templates) == 0 || len(body) == 0 {
		return false
	}

	var parsed map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber() // Important: preserves numbers as strings for accurate conversion
	if err := decoder.Decode(&parsed); err != nil {
		// Fallback for non-JSON bodies (e.g., raw JWT token)
		jwtRe := regexp.MustCompile(`eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+`)
		token := jwtRe.FindString(string(body))
		if token == "" {
			return false
		}

		changed := false
		for k := range templates {
			if strings.Contains(normalizeKey(k), "token") && templates[k] != token {
				logger.Debug("Updating template from non-JSON response (JWT)", zap.String("key", k), zap.Any("new_value", token))
				templates[k] = token
				changed = true
			}
		}
		return changed
	}

	logger.Debug("Attempting to update templates from response", zap.Any("current_templates", templates))

	changed := false
	for tKey, tVal := range templates {
		normTKey := normalizeKey(tKey)
		for rKey, rVal := range parsed {
			if normTKey == normalizeKey(rKey) {
				currentStr := fmt.Sprintf("%v", tVal)
				newStr := fmt.Sprintf("%v", rVal)

				logger.Debug("Comparing template value with response value",
					zap.String("template_key", tKey),
					zap.String("response_key", rKey),
					zap.String("current_value_str", currentStr),
					zap.String("new_value_str", newStr),
				)

				if currentStr != newStr {
					// *** CRITICAL FIX STARTS HERE ***
					// Convert the new value to a standard Go type before storing it.
					var finalValue interface{} = rVal
					if num, ok := rVal.(json.Number); ok {
						// Try to convert to int64 first
						if i, err := num.Int64(); err == nil {
							finalValue = i
						} else if f, err := num.Float64(); err == nil {
							// If int64 fails, try float64
							finalValue = f
						}
						// If both fail, it remains a string, which is fine.
					}
					// *** CRITICAL FIX ENDS HERE ***

					logger.Info("Updating template value",
						zap.String("key", tKey),
						zap.Any("old_value", tVal),
						zap.Any("new_value", finalValue), // Log the converted value
					)
					templates[tKey] = finalValue
					changed = true
				}
				break // Found match for this template key, move to the next one
			}
		}
	}

	if changed {
		logger.Debug("Final templates after update", zap.Any("updated_templates", templates))
	}
	return changed
}

func (o *Orchestrator) ReRecord(ctx context.Context) error {
	// This function remains the same as your version.
	// ... (rest of the file is identical to your provided code) ...
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
		errGrp, _ := errgroup.WithContext(ctx)
		recordCtx := context.WithoutCancel(ctx)
		recordCtx, recordCtxCancel := context.WithCancel(recordCtx)

		var errCh = make(chan error, 1)
		var replayErrCh = make(chan error, 1)

		select {
		case <-ctx.Done():
		default:
			errGrp.Go(func() error {
				defer utils.Recover(o.logger)
				err := o.record.Start(recordCtx, true)
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
			time.Sleep(3 * time.Second)
		}

		recordCtxCancel()

		err = errGrp.Wait()
		if err != nil {
			utils.LogError(o.logger, err, "failed to stop re-recording")
		}

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
	if !o.config.InCi {
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

	o.logger.Debug("", zap.String("host", host), zap.String("port", port), zap.Any("WaitTimeout", timeout), zap.Any("CommandType", cmdType))

	if err := pkg.WaitForPort(ctx, host, port, timeout); err != nil {
		utils.LogError(o.logger, err, "Waiting for port failed", zap.String("host", host), zap.String("port", port))
		return false, err
	}

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
		if utils.IsDockerCmd(cmdType) {
			tc.HTTPReq.URL, err = utils.ReplaceHost(tc.HTTPReq.URL, userIP)
			if err != nil {
				utils.LogError(o.logger, err, "failed to replace host to docker container's IP")
				break
			}
			o.logger.Debug("", zap.Any("replaced URL in case of docker env", tc.HTTPReq.URL))
		}

		if o.config.ReRecord.Host != "" {
			tc.HTTPReq.URL, err = utils.ReplaceHost(tc.HTTPReq.URL, o.config.ReRecord.Host)
			if err != nil {
				utils.LogError(o.logger, err, "failed to replace host to provided host by the user")
				break
			}
		}

		if o.config.ReRecord.Port != 0 {
			tc.HTTPReq.URL, err = utils.ReplacePort(tc.HTTPReq.URL, strconv.Itoa(int(o.config.ReRecord.Port)))
			if err != nil {
				utils.LogError(o.logger, err, "failed to replace port to provided port by the user")
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
			continue
		}

		if resp != nil && resp.Body != "" && len(utils.TemplatizedValues) > 0 {
			// Keep a snapshot to detect which keys changed
			prevVals := make(map[string]interface{}, len(utils.TemplatizedValues))
			for k, v := range utils.TemplatizedValues {
				prevVals[k] = v
			}
			if updateTemplatesFromJSON([]byte(resp.Body), utils.TemplatizedValues, o.logger) {
				// Persist template changes
				if err := o.replay.UpdateTestSetTemplate(ctx, testSet, utils.TemplatizedValues); err != nil {
					o.logger.Warn("failed to persist updated template values during rerecord", zap.String("testSet", testSet), zap.Error(err))
				} else {
					o.logger.Debug("updated template values during rerecord", zap.String("testSet", testSet), zap.Any("template", utils.TemplatizedValues))
				}

				// Propagate updated dynamic values into subsequent test case URLs (and headers/bodies if plain occurrence exists) when they are still literal (not templated)
				for key, newVal := range utils.TemplatizedValues {
					oldVal := prevVals[key]
					if oldVal == nil || fmt.Sprintf("%v", oldVal) == fmt.Sprintf("%v", newVal) {
						continue
					}
					oldStr := fmt.Sprintf("%v", oldVal)
					newStr := fmt.Sprintf("%v", newVal)
					for _, future := range tcs { // safe to scan all; earlier ones won't run again
						// Skip already executed testcases by comparing timestamps or names ordering; simplest skip if future.Name == tc.Name then continue afterwards
						if future.Name == tc.Name {
							continue
						}
						// Update URL path parameter occurrences
						if strings.Contains(future.HTTPReq.URL, oldStr) && !strings.Contains(future.HTTPReq.URL, "{{") {
							future.HTTPReq.URL = strings.ReplaceAll(future.HTTPReq.URL, oldStr, newStr)
						}
						// Update headers if any value exactly matches oldStr
						for hk, hv := range future.HTTPReq.Header {
							if hv == oldStr {
								future.HTTPReq.Header[hk] = newStr
							}
						}
						// Update body (simple string replacement) only if appears and body not templated yet
						if future.HTTPReq.Body != "" && strings.Contains(future.HTTPReq.Body, oldStr) && !strings.Contains(future.HTTPReq.Body, "{{") {
							future.HTTPReq.Body = strings.ReplaceAll(future.HTTPReq.Body, oldStr, newStr)
						}
					}
				}
			}
		}

		o.logger.Info("Re-recorded the testcase successfully", zap.String("testcase", tc.Name), zap.String("of testset", testSet))
	}

	if simErr {
		return allTcRecorded, fmt.Errorf("got error while simulating HTTP request. Please make sure the related services are up and running")
	}

	return allTcRecorded, nil
}

func (o *Orchestrator) checkForTemplates(ctx context.Context, testSets []string) {
	// This function remains the same as your version.
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
	o.logger.Warn("The following testSets are not templatized. Do you want to templatize them to handle noisy fields?(y/n)", zap.Any("testSets:", nonTemplatized))
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
		// You might need to change this call to o.tools.ProcessTestCasesV2 if you haven't renamed it.
		if err := o.tools.Templatize(ctx); err != nil {
			utils.LogError(o.logger, err, "failed to templatize test cases, skipping templatization")
		}
	}
}
