// Package schema for schema matching
package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	matcher "go.keploy.io/server/v2/pkg/matcher"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

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
			if mode == models.CompareMode {
				if _, matched, err = handleJSONDiff(validatedJSON, logDiffs, newLogger, logger, testName, mockName, testSetID, mockSetID, mockResponseBodyStr, testResponseBodyStr, "response", mode); err != nil {
					return differencesCount, false, false, err
				}
			} else if mode == models.IdentifyMode {
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
			var statusCode string
			for status := range mockOperation.Responses {
				statusCode = status
				break

			}

			if candidateScore, pass, _, err = compareResponseBodies(statusCode, mockOperation, testOperation, logDiffs, newLogger, logger, test.Info.Title, mock.Info.Title, testSetID, mockSetID, mode); err != nil {
				return candidateScore, false, err
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
