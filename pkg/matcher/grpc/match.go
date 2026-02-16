// Package grpc provides gRPC response matching functionality
package grpc

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Match compares an expected gRPC response with an actual response and returns whether they match
// along with detailed comparison results
func Match(tc *models.TestCase, actualResp *models.GrpcResp, noiseConfig map[string]map[string][]string, ignoreOrdering bool, logger *zap.Logger, emitFailureLogs bool) (bool, *models.Result) {
	expectedResp := tc.GrpcResp
	result := &models.Result{
		HeadersResult: make([]models.HeaderResult, 0),
		BodyResult:    make([]models.BodyResult, 0),
		TrailerResult: make([]models.HeaderResult, 0),
	}
	currentRisk := models.None
	var currentCategories []models.FailureCategory

	// Local variables to track overall match status
	differences := make(map[string]struct {
		Expected string
		Actual   string
		Message  string
	})

	// Only compare :status in pseudo headers
	if expectedStatus, ok := expectedResp.Headers.PseudoHeaders[":status"]; ok {
		actualStatus, exists := actualResp.Headers.PseudoHeaders[":status"]
		headerResult := models.HeaderResult{
			Expected: models.Header{
				Key:   ":status",
				Value: []string{expectedStatus},
			},
			Actual: models.Header{
				Key:   ":status",
				Value: []string{},
			},
		}

		if !exists {
			differences["headers.pseudo_headers.:status"] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedStatus,
				Actual:   "",
				Message:  "missing status header in response",
			}
			headerResult.Normal = false
			currentRisk = models.High
			currentCategories = append(currentCategories, models.StatusCodeChanged)
		} else {
			headerResult.Actual.Value = []string{actualStatus}
			headerResult.Normal = expectedStatus == actualStatus

			if !headerResult.Normal {
				differences["headers.pseudo_headers.:status"] = struct {
					Expected string
					Actual   string
					Message  string
				}{
					Expected: expectedStatus,
					Actual:   actualStatus,
					Message:  "status header value mismatch",
				}
				currentRisk = models.High
				currentCategories = append(currentCategories, models.StatusCodeChanged)
			}
		}

		result.HeadersResult = append(result.HeadersResult, headerResult)
	}

	// Compare 'content-type' in ordinary headers
	if expectedContentType, ok := expectedResp.Headers.OrdinaryHeaders["content-type"]; ok {
		actualContentType, exists := actualResp.Headers.OrdinaryHeaders["content-type"]
		headerResult := models.HeaderResult{
			Expected: models.Header{
				Key:   "content-type",
				Value: []string{expectedContentType},
			},
			Actual: models.Header{
				Key:   "content-type",
				Value: []string{},
			},
		}

		if !exists {
			differences["headers.ordinary_headers.content-type"] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedContentType,
				Actual:   "",
				Message:  "missing content-type header in response",
			}
			headerResult.Normal = false
			currentRisk = models.High
			currentCategories = append(currentCategories, models.HeaderChanged)
		} else {
			headerResult.Actual.Value = []string{actualContentType}

			// Split the header strings by comma to handle potential multi-valued headers
			// represented as a single string. This makes the order-ignoring comparison meaningful.
			expectedParts := strings.Split(expectedContentType, ",")
			for i := range expectedParts {
				expectedParts[i] = strings.TrimSpace(expectedParts[i])
			}

			actualParts := strings.Split(actualContentType, ",")
			for i := range actualParts {
				actualParts[i] = strings.TrimSpace(actualParts[i])
			}

			normalize := func(s string) string {
				return strings.TrimSpace(strings.Split(s, "+")[0])
			}

			headerResult.Normal = normalize(expectedContentType) == normalize(actualContentType)

			if !headerResult.Normal {
				differences["headers.ordinary_headers.content-type"] = struct {
					Expected string
					Actual   string
					Message  string
				}{
					Expected: expectedContentType,
					Actual:   actualContentType,
					Message:  "content-type header value mismatch",
				}
				currentRisk = models.High
				currentCategories = append(currentCategories, models.HeaderChanged)
			}
		}
		result.HeadersResult = append(result.HeadersResult, headerResult)
	}

	// Compare Body - using specialized body types for gRPC
	// Compare compression flag
	compressionFlagNormal := expectedResp.Body.CompressionFlag == actualResp.Body.CompressionFlag
	if !compressionFlagNormal {
		differences["body.compression_flag"] = struct {
			Expected string
			Actual   string
			Message  string
		}{
			Expected: fmt.Sprintf("%d", expectedResp.Body.CompressionFlag),
			Actual:   fmt.Sprintf("%d", actualResp.Body.CompressionFlag),
			Message:  "compression flag mismatch",
		}
	}
	result.BodyResult = append(result.BodyResult, models.BodyResult{
		Normal:   compressionFlagNormal,
		Type:     models.GrpcCompression,
		Expected: fmt.Sprintf("%d", expectedResp.Body.CompressionFlag),
		Actual:   fmt.Sprintf("%d", actualResp.Body.CompressionFlag),
	})

	// Compare message length
	messageLengthNormal := expectedResp.Body.MessageLength == actualResp.Body.MessageLength
	if !messageLengthNormal {
		differences["body.message_length"] = struct {
			Expected string
			Actual   string
			Message  string
		}{
			Expected: fmt.Sprintf("%d", expectedResp.Body.MessageLength),
			Actual:   fmt.Sprintf("%d", actualResp.Body.MessageLength),
			Message:  "message length mismatch",
		}
	}
	result.BodyResult = append(result.BodyResult, models.BodyResult{
		Normal:   messageLengthNormal,
		Type:     models.GrpcLength,
		Expected: fmt.Sprintf("%d", expectedResp.Body.MessageLength),
		Actual:   fmt.Sprintf("%d", actualResp.Body.MessageLength),
	})

	// Handle noise configuration first - needed for JSON comparison
	noise := tc.Noise

	var (
		bodyNoise   = noiseConfig["body"]
		headerNoise = noiseConfig["header"] // need to handle noisy header separately (not implemented yet for grpc)
	)

	if bodyNoise == nil {
		bodyNoise = map[string][]string{}
	}

	if headerNoise == nil {
		headerNoise = map[string][]string{}
	}

	// Merge test-case-specific noise with global noise (similar to HTTP matcher)
	for field, regexArr := range noise {
		a := strings.Split(field, ".")
		if len(a) > 1 && a[0] == "body" {
			x := strings.Join(a[1:], ".")
			bodyNoise[strings.ToLower(x)] = regexArr
		} else if a[0] == "header" {
			headerNoise[strings.ToLower(a[len(a)-1])] = regexArr
		}
	}

	// Compare decoded data - use JSON comparison if both are valid JSON, otherwise use canonicalization
	decodedDataNormal := true
	expectedDecodedData := expectedResp.Body.DecodedData
	actualDecodedData := actualResp.Body.DecodedData

	var jsonComparisonResult matcher.JSONComparisonResult

	// Check if both decoded data are valid JSON
	if json.Valid([]byte(expectedDecodedData)) && json.Valid([]byte(actualDecodedData)) {
		// Both are JSON - use proper JSON comparison like HTTP matcher
		logger.Debug("Both gRPC decoded data are valid JSON, using JSON comparison",
			zap.String("expectedDecodedData", expectedDecodedData),
			zap.String("actualDecodedData", actualDecodedData))

		expectedDecodedData = matcher.NormalizeNestedJSONForNoise(expectedDecodedData, bodyNoise, logger)
		actualDecodedData = matcher.NormalizeNestedJSONForNoise(actualDecodedData, bodyNoise, logger)

		validatedJSON, err := matcher.ValidateAndMarshalJSON(logger, &expectedDecodedData, &actualDecodedData)
		if err != nil {
			logger.Error("Failed to validate and marshal JSON for gRPC decoded data", zap.Error(err))
			decodedDataNormal = false
		} else if validatedJSON.IsIdentical() {
			jsonComparisonResult, err = matcher.JSONDiffWithNoiseControl(validatedJSON, bodyNoise, ignoreOrdering)
			decodedDataNormal = jsonComparisonResult.IsExact()
			if err != nil {
				logger.Error("Failed to perform JSON diff with noise control", zap.Error(err))
				decodedDataNormal = false
			}
			if !decodedDataNormal {
				logger.Debug("JSON comparison found differences",
					zap.Bool("isExact", jsonComparisonResult.IsExact()),
					zap.Bool("matches", jsonComparisonResult.Matches()))
			}
		} else {
			logger.Debug("JSON structures are not identical, marking as mismatch")
			decodedDataNormal = false
		}
	} else {
		// At least one is not JSON - fall back to canonicalization approach
		logger.Debug("At least one gRPC decoded data is not valid JSON, using canonicalization",
			zap.Bool("expectedIsJSON", json.Valid([]byte(expectedDecodedData))),
			zap.Bool("actualIsJSON", json.Valid([]byte(actualDecodedData))))

		expCanon := CanonicalizeTopLevelBlocks(expectedDecodedData)
		actCanon := CanonicalizeTopLevelBlocks(actualDecodedData)
		decodedDataNormal = expCanon == actCanon
		// Update the data for result reporting
		expectedDecodedData = expCanon
		actualDecodedData = actCanon
	}

	if !decodedDataNormal {
		differences["body.decoded_data"] = struct {
			Expected string
			Actual   string
			Message  string
		}{
			Expected: expectedDecodedData,
			Actual:   actualDecodedData,
			Message:  "decoded data mismatch",
		}
	}
	result.BodyResult = append(result.BodyResult, models.BodyResult{
		Normal:   decodedDataNormal,
		Type:     models.GrpcData,
		Expected: expectedDecodedData,
		Actual:   actualDecodedData,
	})

	// If decoded data matches but message length differs, ignore the length difference
	if decodedDataNormal && !messageLengthNormal {
		logger.Warn("Ignoring message length mismatch since decoded data is identical",
			zap.Uint32("expected", expectedResp.Body.MessageLength),
			zap.Uint32("actual", actualResp.Body.MessageLength))
		// Update the message length result to Normal=true
		for i := range result.BodyResult {
			if result.BodyResult[i].Type == models.GrpcLength {
				result.BodyResult[i].Normal = true
				break
			}
		}
		// Remove the message_length difference from differences map
		delete(differences, "body.message_length")
	}

	// Apply noise configuration to ignore specified differences
	for path := range differences {
		pathParts := strings.Split(path, ".")
		if len(pathParts) > 1 {
			if pathParts[0] == "body" && len(bodyNoise) > 0 {
				if _, found := bodyNoise[strings.Join(pathParts[1:], ".")]; found {
					delete(differences, path)
				}
			} else if pathParts[0] == "headers" && len(headerNoise) > 0 {
				if _, found := headerNoise[pathParts[len(pathParts)-1]]; found {
					delete(differences, path)
				}
			}
		}
	}

	// Calculate final match status based on remaining differences
	matched := len(differences) == 0

	if !matched {
		// Display differences to the user, similar to HTTP matcher
		logDiffs := matcher.NewDiffsPrinter(tc.Name)
		newLogger := pp.New()
		newLogger.WithLineInfo = false
		newLogger.SetColorScheme(models.GetFailingColorScheme())
		var logs = ""

		logs = logs + newLogger.Sprintf("Testrun failed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)

		// Display gRPC differences
		if len(differences) > 0 {
			for path, diff := range differences {
				if strings.HasPrefix(path, "headers.") {
					// Header differences
					header := strings.TrimPrefix(path, "headers.")
					logDiffs.PushHeaderDiff(diff.Expected, diff.Actual, header, headerNoise)
				} else if strings.HasPrefix(path, "body.") {
					bodyPart := strings.TrimPrefix(path, "body.")
					switch bodyPart {
					case "message_length":
						// Message length is a good indicator of difference for gRPC
						logDiffs.PushHeaderDiff(diff.Expected, diff.Actual, "message_length (body)", bodyNoise)
					case "compression_flag":
						// Compression flag
						logDiffs.PushHeaderDiff(diff.Expected, diff.Actual, "compression_flag (body)", bodyNoise)
					case "decoded_data":
						// Handle decoded data differences - could be JSON or canonical format
						if jsonComparisonResult.Matches() {
							logDiffs.SetHasarrayIndexMismatch(true)
							logDiffs.PushFooterDiff(strings.Join(jsonComparisonResult.Differences(), ", "))
						}
						logDiffs.PushBodyDiff(diff.Expected, diff.Actual, bodyNoise)
					default:
						// Any other body differences
						logDiffs.PushBodyDiff(diff.Expected, diff.Actual, bodyNoise)
					}
				}
			}
		} else {
			// If there are no specific differences but match still failed, show a generic message
			logDiffs.PushHeaderDiff("See logs for details", "Matching failed", "gRPC", nil)
		}

		// Print the differences
		if emitFailureLogs {
			_, err := newLogger.Printf(logs)
			if err != nil {
				utils.LogError(logger, err, "failed to print the logs")
			}

			err = logDiffs.Render()
			if err != nil {
				utils.LogError(logger, err, "failed to render the diffs")
			}
		}
	} else {
		// Display success message
		newLogger := pp.New()
		newLogger.WithLineInfo = false
		newLogger.SetColorScheme(models.GetPassingColorScheme())
		var log2 = ""
		log2 += newLogger.Sprintf("Testrun passed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)
		_, err := newLogger.Printf(log2)
		if err != nil {
			utils.LogError(logger, err, "failed to print the logs")
		}
	}

	if !decodedDataNormal {
		if json.Valid([]byte(expectedDecodedData)) && json.Valid([]byte(actualDecodedData)) {
			if assess, err := matcher.ComputeFailureAssessmentJSON(expectedDecodedData, actualDecodedData, bodyNoise, ignoreOrdering); err == nil && assess != nil {
				currentRisk = matcher.MaxRisk(currentRisk, assess.Risk)
				currentCategories = append(currentCategories, assess.Category...)
			} else {
				currentRisk = models.High
				currentCategories = append(currentCategories, models.InternalFailure)
			}
		} else {
			// non-JSON payload mismatch → Broken
			currentRisk = models.High
			currentCategories = append(currentCategories, models.SchemaBroken)
		}
	}

	// remove duplicates
	catMap := make(map[models.FailureCategory]bool)
	uniqueCategories := []models.FailureCategory{}
	for _, cat := range currentCategories {
		if !catMap[cat] {
			catMap[cat] = true
			uniqueCategories = append(uniqueCategories, cat)
		}
	}

	result.FailureInfo = models.FailureInfo{
		Risk:     currentRisk,
		Category: uniqueCategories,
	}

	return matched, result
}
