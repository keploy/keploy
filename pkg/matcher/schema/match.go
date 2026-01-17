// Package schema for schema matching
package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sort"
	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	matcher "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)
// StatusCodeComparison represents the comparison result for a single status code
type StatusCodeComparison struct {
    StatusCode    string  // e.g., "200", "400", "500"
    Exists        bool    // Whether status code exists in both schemas
    IsMatched     bool    // Whether response bodies match
    Score         float64 // Similarity score
    ErrorMessage  string  // Detailed error if not matched
}

// SchemaValidationResult aggregates all status code validations
type SchemaValidationResult struct {
    AllStatusCodesValid bool                    // Overall result
    ComparisonResults   []StatusCodeComparison  // Individual results
    OverallScore        float64                 // Average score
    TotalStatusCodes    int                     // Total status codes checked
    MatchedCount        int                     // How many matched
    ValidStatusCodes    []string                // List of valid status codes
    InvalidStatusCodes  []string                // List of invalid status codes
}

type ValidatedJSONWrapper struct {
	Expected    interface{} `json:"expected"`
	Actual      interface{} `json:"actual"`
	IsIdentical bool        `json:"isIdentical"`
}
type JSONComparisonResultWrapper struct {
	Matches     bool     `json:"matches"`
	IsExact     bool     `json:"isExact"`
	Differences []string `json:"differences"`
}

const NOTCANDIDATE = -1.0

func compareOperationTypes(mockOperationType, testOperationType string) (bool, error) {
	pass := true
	if mockOperationType != testOperationType {
		pass = false
		return pass, nil

	}
	return pass, nil
}
func compareRequestBodies(mockOperation, testOperation *models.Operation, logDiffs matcher.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger, testName, mockName, testSetID, mockSetID string) (bool, error) {
	pass := false
	var score float64
	mockRequestBodyStr, testRequestBodyStr, err := matcher.MarshalRequestBodies(mockOperation, testOperation)
	if err != nil {
		return false, err
	}

	validatedJSON, err := matcher.ValidateAndMarshalJSON(logger, &mockRequestBodyStr, &testRequestBodyStr)
	if err != nil {
		return false, err
	}

	if validatedJSON.IsIdentical() {
		if score, pass, err = handleJSONDiff(validatedJSON, logDiffs, newLogger, logger, testName, mockName, testSetID, mockSetID, mockRequestBodyStr, testRequestBodyStr, "request", 0); err != nil {
			return false, err
		}
		if score == NOTCANDIDATE {
			return false, nil
		}

	} else {
		pass = false
		return pass, nil

	}
	return pass, nil
}

func compareParameters(mockParameters, testParameters []models.Parameter) (bool, error) {
	pass := true

	for _, mockParam := range mockParameters {
		if mockParam.In == "header" {
			continue
		}
		found := false
		for _, testParam := range testParameters {
			if mockParam.Name == testParam.Name && mockParam.In == testParam.In {
				found = true
				if mockParam.Schema.Type != testParam.Schema.Type {
					pass = false
					return pass, nil
				}
			}
		}
		if !found {
			pass = false
			return pass, nil
		}
	}

	return pass, nil
}

func compareResponseBodies(status string, mockOperation, testOperation *models.Operation, logDiffs matcher.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger, testName, mockName, testSetID, mockSetID string, mode models.SchemaMatchMode) (float64, bool, bool, error) {
	pass := true
	overallScore := 0.0
	matched := false
	differencesCount := 0.0
	if _, ok := testOperation.Responses[status]; ok {
		mockResponseBodyStr, testResponseBodyStr, err := matcher.MarshalResponseBodies(status, mockOperation, testOperation)
		if err != nil {
			return differencesCount, false, false, err
		}
		overallScore = float64(len(mockOperation.Responses[status].Content["application/json"].Schema.Properties))
		validatedJSON, err := matcher.ValidateAndMarshalJSON(logger, &mockResponseBodyStr, &testResponseBodyStr)
		if err != nil {
			return differencesCount, false, false, err
		}

		if validatedJSON.IsIdentical() {
			switch mode {
			case models.CompareMode:
				if _, matched, err = handleJSONDiff(validatedJSON, logDiffs, newLogger, logger, testName, mockName, testSetID, mockSetID, mockResponseBodyStr, testResponseBodyStr, "response", mode); err != nil {
					return differencesCount, false, false, err
				}
			case models.IdentifyMode:
				differencesCount, err = calculateSimilarityScore(mockOperation, testOperation, status)
				if err != nil {
					return differencesCount, false, false, err
				}
			}
		} else {
			differencesCount = overallScore

			if mode == models.CompareMode {
				logDiffs.PushTypeDiff(fmt.Sprint(reflect.TypeOf(validatedJSON.Expected())), fmt.Sprint(reflect.TypeOf(validatedJSON.Actual())))
				logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testName, testSetID, mockName, mockSetID)

				if err := printAndRenderLogs(logs, newLogger, logDiffs, logger); err != nil {
					return differencesCount, false, false, err
				}
			}
		}
	} else {
		pass = false
		differencesCount = -1

	}
	return differencesCount / overallScore, pass, matched, nil
}

// compareAllResponseStatusCodes validates ALL status codes in the mock against test
func compareAllResponseStatusCodes(
    mockOperation *models.Operation,
    testOperation *models.Operation,
    logDiffs matcher.DiffsPrinter,
    newLogger *pp.PrettyPrinter,
    logger *zap.Logger,
    testName, mockName, testSetID, mockSetID string,
    mode models.SchemaMatchMode,
) SchemaValidationResult {
    
    result := SchemaValidationResult{
        AllStatusCodesValid: true,
        ComparisonResults:   make([]StatusCodeComparison, 0),
        ValidStatusCodes:    make([]string, 0),
        InvalidStatusCodes:  make([]string, 0),
    }

    // ✅ STEP 1: Extract all status codes from mock
    mockStatusCodes := make([]string, 0, len(mockOperation.Responses))
    for status := range mockOperation.Responses {
        mockStatusCodes = append(mockStatusCodes, status)
    }
    
    // ✅ STEP 2: IMPORTANT - Sort for deterministic behavior
    // Without sorting, Go map iteration is random
    sort.Strings(mockStatusCodes)
    
    logger.Debug("Validating response status codes",
        zap.Strings("statusCodes", mockStatusCodes),
        zap.Int("count", len(mockStatusCodes)))

    totalScore := 0.0
    matchedCount := 0

    // ✅ STEP 3: Validate EACH status code
    for _, statusCode := range mockStatusCodes {
        comparison := StatusCodeComparison{
            StatusCode: statusCode,
        }

        // ✅ STEP 4: Check if status code exists in test schema
        if _, exists := testOperation.Responses[statusCode]; !exists {
            logger.Warn(
                "Status code present in mock but missing in test",
                zap.String("statusCode", statusCode),
                zap.String("mockName", mockName),
                zap.String("testName", testName),
            )
            
            comparison.Exists = false
            comparison.IsMatched = false
            comparison.ErrorMessage = fmt.Sprintf(
                "Status code %s exists in mock schema but not in test schema",
                statusCode,
            )
            
            result.AllStatusCodesValid = false
            result.InvalidStatusCodes = append(result.InvalidStatusCodes, statusCode)
            result.ComparisonResults = append(result.ComparisonResults, comparison)
            continue
        }

        comparison.Exists = true

        // ✅ STEP 5: Compare response bodies for this specific status code
        score, isMatched, _, err := compareResponseBodies(
            statusCode,
            mockOperation,
            testOperation,
            logDiffs,
            newLogger,
            logger,
            testName,
            mockName,
            testSetID,
            mockSetID,
            mode,
        )

        if err != nil {
            comparison.IsMatched = false
            comparison.ErrorMessage = fmt.Sprintf("Error comparing status %s: %v", statusCode, err)
            result.AllStatusCodesValid = false
            result.InvalidStatusCodes = append(result.InvalidStatusCodes, statusCode)
            
            logger.Error("Failed to compare response body",
                zap.String("statusCode", statusCode),
                zap.Error(err))
        } else {
            // ✅ IMPORTANT: A score of 0 or negative means NO MATCH
            // Even if isMatched is true, treat score <= 0 as failed match
            if isMatched && score > 0 {
                comparison.IsMatched = true
                comparison.Score = score
                matchedCount++
                result.ValidStatusCodes = append(result.ValidStatusCodes, statusCode)
                totalScore += score
                
                logger.Debug("Status code validated successfully",
                    zap.String("statusCode", statusCode),
                    zap.Float64("score", score))
            } else {
                // Score is 0 or negative, or isMatched is false
                comparison.IsMatched = false
                comparison.Score = score
                result.AllStatusCodesValid = false
                result.InvalidStatusCodes = append(result.InvalidStatusCodes, statusCode)
                
                logger.Warn("Status code validation failed",
                    zap.String("statusCode", statusCode),
                    zap.Float64("score", score),
                    zap.Bool("matched", isMatched))
            }
        }

        result.ComparisonResults = append(result.ComparisonResults, comparison)
    }

    result.TotalStatusCodes = len(mockStatusCodes)
    result.MatchedCount = matchedCount
    
    // ✅ STEP 6: Calculate overall score
    if result.TotalStatusCodes > 0 {
        result.OverallScore = totalScore / float64(result.TotalStatusCodes)
    } else {
        result.OverallScore = -1.0
    }

    // ✅ STEP 7: Log comprehensive summary
    logger.Info(
        "Schema validation completed",
        zap.String("mockName", mockName),
        zap.String("testName", testName),
        zap.Int("totalStatusCodes", result.TotalStatusCodes),
        zap.Int("matchedCount", matchedCount),
        zap.Float64("overallScore", result.OverallScore),
        zap.Bool("allValid", result.AllStatusCodesValid),
        zap.Strings("validStatusCodes", result.ValidStatusCodes),
        zap.Strings("invalidStatusCodes", result.InvalidStatusCodes),
    )

    return result
}


func Match(mock, test models.OpenAPI, testSetID string, mockSetID string, logger *zap.Logger, mode models.SchemaMatchMode) (float64, bool, error) {
	pass := false

	candidateScore := -1.0
	newLogger := pp.New()
	newLogger.WithLineInfo = false
	newLogger.SetColorScheme(models.GetFailingColorScheme())

	for path, mockItem := range mock.Paths {
		logDiffs := matcher.NewDiffsPrinter(test.Info.Title + "/" + mock.Info.Title)
		var err error
		if testItem, found := test.Paths[path]; found {
			mockOperation, mockOperationType := matcher.FindOperation(mockItem)
			testOperation, testOperationType := matcher.FindOperation(testItem)
			if mode == models.IdentifyMode {
				if pass, err = compareOperationTypes(mockOperationType, testOperationType); err != nil {
					return candidateScore, false, err
				}
				if !pass {
					continue
				}
				if pass, err = compareParameters(mockOperation.Parameters, testOperation.Parameters); err != nil {
					return candidateScore, false, err
				}
				if !pass {
					continue
				}
				if pass, err = compareRequestBodies(mockOperation, testOperation, logDiffs, newLogger, logger, test.Info.Title, mock.Info.Title, testSetID, mockSetID); err != nil {
					return candidateScore, false, err
				}
				if !pass {
					continue
				}
			}
			// var statusCode string
			// for status := range mockOperation.Responses {
			// 	statusCode = status
			// 	break

			// }

			// if candidateScore, pass, _, err = compareResponseBodies(statusCode, mockOperation, testOperation, logDiffs, newLogger, logger, test.Info.Title, mock.Info.Title, testSetID, mockSetID, mode); err != nil {
			// 	return candidateScore, false, err
			// }
			validationResult := compareAllResponseStatusCodes(
    mockOperation,
    testOperation,
    logDiffs,
    newLogger,
    logger,
    test.Info.Title,
    mock.Info.Title,
    testSetID,
    mockSetID,
    mode,
)

// ALL status codes must be valid for schemas to match
if !validationResult.AllStatusCodesValid {
    if mode == models.CompareMode {
        // In CompareMode, log detailed failure information
        logger.Error(
            "Schema validation failed",
            zap.String("mockName", mock.Info.Title),
            zap.String("testName", test.Info.Title),
            zap.Strings("invalidStatusCodes", validationResult.InvalidStatusCodes),
        )
        
        // Log each invalid status code's error
        for _, comp := range validationResult.ComparisonResults {
            if !comp.IsMatched || !comp.Exists {
                logger.Error(
                    "Status code validation details",
                    zap.String("statusCode", comp.StatusCode),
                    zap.Bool("exists", comp.Exists),
                    zap.Bool("matched", comp.IsMatched),
                    zap.String("error", comp.ErrorMessage),
                )
            }
			}
    }
    
    candidateScore = -1.0
    pass = false
} else {
    // All status codes validated successfully
    candidateScore = validationResult.OverallScore
    pass = true
}
		} else {
			pass = false

		}

	}

	return candidateScore, pass, nil
}
func calculateSimilarityScore(mockOperation, testOperation *models.Operation, status string) (float64, error) {
	testParameters := testOperation.Responses[status].Content["application/json"].Schema.Properties
	mockParameters := mockOperation.Responses[status].Content["application/json"].Schema.Properties
	score := 0.0
	for key, testParam := range testParameters {
		if _, ok := mockParameters[key]; ok {
			if testParam["type"] == mockParameters[key]["type"] {
				score++
			}
		}
	}
	return score, nil
}

func handleJSONDiff(validatedJSON matcher.ValidatedJSON, logDiffs matcher.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger, _ string, _ string, _ string, _ string, mockBodyStr string, testBodyStr string, diffType string, mode models.SchemaMatchMode) (float64, bool, error) {
	pass := true
	differencesCount := 0.0
	jsonComparisonResult, err := matcher.JSONDiffWithNoiseControl(validatedJSON, nil, false)
	if err != nil {
		return differencesCount, false, err
	}
	if !jsonComparisonResult.IsExact() {
		pass = false
		// logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testName, testSetID, mockName, mockSetID)
		if json.Valid([]byte(mockBodyStr)) {
			patch, err := jsondiff.Compare(testBodyStr, mockBodyStr)
			if err != nil {
				logger.Warn("failed to compute json diff", zap.Error(err))
				return differencesCount, false, err
			}
			differencesCount = float64(len(patch))
			if diffType == "request" && differencesCount > 1 {
				return -1.0, false, nil
			}
			if diffType == "response" {
				for _, op := range patch {
					if jsonComparisonResult.Matches() {
						logDiffs.SetHasarrayIndexMismatch(true)
						logDiffs.PushFooterDiff(strings.Join(jsonComparisonResult.Differences(), ", "))
					}

					logDiffs.PushBodyDiff(fmt.Sprint(op.OldValue), fmt.Sprint(op.Value), nil)

				}
			}
		}
		if diffType == "response" && mode == models.CompareMode {
			if err := printAndRenderLogs("", newLogger, logDiffs, logger); err != nil {
				return differencesCount, false, err
			}

		}
	}
	return differencesCount, pass, nil
}

func printAndRenderLogs(logs string, newLogger *pp.PrettyPrinter, logDiffs matcher.DiffsPrinter, logger *zap.Logger) error {
	if _, err := newLogger.Printf(logs); err != nil {
		utils.LogError(logger, err, "failed to print the logs")
		return err
	}
	if err := logDiffs.RenderAppender(); err != nil {
		utils.LogError(logger, err, "failed to render the diffs")
		return err
	}
	return nil
}
