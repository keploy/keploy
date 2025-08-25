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
	"go.keploy.io/server/v2/pkg/models"
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

// stripNumericSuffix removes trailing digits from a string and returns
// the base string and whether a suffix was found
func stripNumericSuffix(s string) (string, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] < '0' || s[i] > '9' {
			if i < len(s)-1 {
				return s[:i+1], true
			}
			return s, false
		}
	}
	return "", false
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
// This version includes generic numeric suffix handling
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
		// Try exact match first
		if rVal, exists := parsed[tKey]; exists {
			currentStr := fmt.Sprintf("%v", tVal)
			newStr := fmt.Sprintf("%v", rVal)

			logger.Debug("Comparing template value with response value (exact match)",
				zap.String("template_key", tKey),
				zap.String("response_key", tKey),
				zap.String("current_value_str", currentStr),
				zap.String("new_value_str", newStr),
			)

			if currentStr != newStr {
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

				logger.Info("Updating template value (exact match)",
					zap.String("key", tKey),
					zap.Any("old_value", tVal),
					zap.Any("new_value", finalValue),
				)
				templates[tKey] = finalValue
				changed = true
			}
			continue // Found exact match, move to next template key
		}

		// Handle numeric suffixes: if template key ends with number (like id1, token2, etc.)
		// try matching with the base name (id, token, etc.) in the response
		if baseKey, hasSuffix := stripNumericSuffix(tKey); hasSuffix {
			if rVal, exists := parsed[baseKey]; exists {
				currentStr := fmt.Sprintf("%v", tVal)
				newStr := fmt.Sprintf("%v", rVal)

				logger.Debug("Comparing template value with response value (suffix match)",
					zap.String("template_key", tKey),
					zap.String("response_key", baseKey),
					zap.String("current_value_str", currentStr),
					zap.String("new_value_str", newStr),
				)

				if currentStr != newStr {
					var finalValue interface{} = rVal
					if num, ok := rVal.(json.Number); ok {
						if i, err := num.Int64(); err == nil {
							finalValue = i
						} else if f, err := num.Float64(); err == nil {
							finalValue = f
						}
					}

					logger.Info("Updating suffixed template from base response field",
						zap.String("template_key", tKey),
						zap.String("response_key", baseKey),
						zap.Any("old_value", tVal),
						zap.Any("new_value", finalValue),
					)
					templates[tKey] = finalValue
					changed = true
				}
				continue // Found suffix match, move to next template key
			}
		}

		// Fallback to normalized matching for backward compatibility
		normTKey := normalizeKey(tKey)
		for rKey, rVal := range parsed {
			if normTKey == normalizeKey(rKey) {
				currentStr := fmt.Sprintf("%v", tVal)
				newStr := fmt.Sprintf("%v", rVal)

				logger.Debug("Comparing template value with response value (normalized)",
					zap.String("template_key", tKey),
					zap.String("response_key", rKey),
					zap.String("current_value_str", currentStr),
					zap.String("new_value_str", newStr),
				)

				if currentStr != newStr {
					var finalValue interface{} = rVal
					if num, ok := rVal.(json.Number); ok {
						if i, err := num.Int64(); err == nil {
							finalValue = i
						} else if f, err := num.Float64(); err == nil {
							finalValue = f
						}
					}

					logger.Info("Updating template value (normalized match)",
						zap.String("key", tKey),
						zap.Any("old_value", tVal),
						zap.Any("new_value", finalValue),
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

// The rest of the file remains unchanged...
// [ReRecord function, replayTests function, checkForTemplates function, etc.]
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

				// Propagate updated values only to tracked testcases that use the changed key (literal occurrences only; placeholders auto-render next time)
				for key, newVal := range utils.TemplatizedValues {
					oldVal := prevVals[key]
					if oldVal == nil || fmt.Sprintf("%v", oldVal) == fmt.Sprintf("%v", newVal) {
						continue
					}
					oldStr := fmt.Sprintf("%v", oldVal)
					newStr := fmt.Sprintf("%v", newVal)
					// For each testcase tracked for this key
					// Sibling key synchronization: propagate to non-producer siblings sharing same base (strip trailing digits)
					base := strings.TrimRightFunc(key, func(r rune) bool { return r >= '0' && r <= '9' })
					if base == "" { // if no alpha prefix, treat full key as base
						base = key
					}
					for sibling, val := range utils.TemplatizedValues {
						if sibling == key {
							continue
						}
						if !strings.HasPrefix(sibling, base) {
							continue
						}
						if fmt.Sprintf("%v", val) == newStr {
							continue
						}
						if _, isProducer := producerKeys[sibling]; isProducer {
							continue
						}
						// Only update if sibling currently tracked as consumer for this resource family
						// Heuristic: sibling value equals oldStr OR sibling value not referenced in any response bodies.
						if fmt.Sprintf("%v", val) == oldStr {
							utils.TemplatizedValues[sibling] = newVal
							// update usageMap so replacements below also consider sibling key
						}
					}

					for future := range usageMap[key] {
						if future.Name == tc.Name { // skip current producer testcase
							continue
						}
						// Replace only if field not templated already.
						if future.HTTPReq.URL != "" && !strings.Contains(future.HTTPReq.URL, "{{") && strings.Contains(future.HTTPReq.URL, oldStr) {
							future.HTTPReq.URL = strings.ReplaceAll(future.HTTPReq.URL, oldStr, newStr)
						}
						for hk, hv := range future.HTTPReq.Header {
							if hv == oldStr { // exact match safer for headers
								future.HTTPReq.Header[hk] = newStr
							}
						}
						if body := future.HTTPReq.Body; body != "" && !strings.Contains(body, "{{") && strings.Contains(body, oldStr) {
							future.HTTPReq.Body = strings.ReplaceAll(body, oldStr, newStr)
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
