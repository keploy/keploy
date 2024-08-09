package contract

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	"go.keploy.io/server/v2/pkg/models"
	replaySvc "go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type ValidatedJSON struct {
	Expected    interface{} `json:"expected"`
	Actual      interface{} `json:"actual"`
	IsIdentical bool        `json:"isIdentical"`
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

func findOperation(item models.PathItem) (*models.Operation, string) {
	if item.Get != nil {
		return item.Get, "GET"
	}
	if item.Post != nil {
		return item.Post, "POST"
	}
	if item.Put != nil {
		return item.Put, "PUT"
	}
	if item.Delete != nil {
		return item.Delete, "DELETE"
	}
	if item.Patch != nil {
		return item.Patch, "PATCH"
	}
	return nil, ""
}
func compareOperationTypes(mockOperationType, testOperationType, testTitle, testSetID, mockTitle, mockSetID string, logDiffs replaySvc.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger) (bool, error) {
	pass := true
	if mockOperationType != testOperationType {
		pass = false
		logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testTitle, testSetID, mockTitle, mockSetID)
		logDiffs.PushMethodDiff(fmt.Sprint(testOperationType), fmt.Sprint(mockOperationType))
		if err := printAndRenderLogs(logs, newLogger, logDiffs, logger); err != nil {
			return false, err
		}
	}
	return pass, nil
}
func compareRequestBodies(mockOperation, testOperation *models.Operation, logDiffs replaySvc.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger, testName, mockName, testSetID, mockSetID string) (bool, error) {
	pass := false
	mockRequestBodyStr, testRequestBodyStr, err := marshalRequestBodies(mockOperation, testOperation)
	if err != nil {
		return false, err
	}

	validatedJSON, err := replaySvc.ValidateAndMarshalJSON(logger, &mockRequestBodyStr, &testRequestBodyStr)
	if err != nil {
		return false, err
	}

	if validatedJSON.IsIdentical() {
		if pass, err = handleJSONDiff(validatedJSON, logDiffs, newLogger, logger, testName, mockName, testSetID, mockSetID, mockRequestBodyStr, testRequestBodyStr, "request"); err != nil {
			return false, err
		}

	} else {
		pass = false
		logDiffs.PushTypeDiff(fmt.Sprint(reflect.TypeOf(validatedJSON.Expected())), fmt.Sprint(reflect.TypeOf(validatedJSON.Actual())))
		logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testName, testSetID, mockName, mockSetID)

		if err := printAndRenderLogs(logs, newLogger, logDiffs, logger); err != nil {
			return false, err
		}
	}
	return pass, nil
}
func compareResponseBodies(status string, mockOperation, testOperation *models.Operation, logDiffs replaySvc.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger, testName, mockName, testSetID, mockSetID string) (bool, error) {
	pass := false
	if _, ok := testOperation.Responses[status]; ok {
		mockResponseBodyStr, testResponseBodyStr, err := marshalResponseBodies(status, mockOperation, testOperation)
		if err != nil {
			return false, err
		}

		validatedJSON, err := replaySvc.ValidateAndMarshalJSON(logger, &mockResponseBodyStr, &testResponseBodyStr)
		if err != nil {
			return false, err
		}

		if validatedJSON.IsIdentical() {
			if pass, err = handleJSONDiff(validatedJSON, logDiffs, newLogger, logger, testName, mockName, testSetID, mockSetID, mockResponseBodyStr, testResponseBodyStr, "response"); err != nil {
				return false, err
			}
		} else {
			pass = false
			logDiffs.PushTypeDiff(fmt.Sprint(reflect.TypeOf(validatedJSON.Expected())), fmt.Sprint(reflect.TypeOf(validatedJSON.Actual())))
			logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testName, testSetID, mockName, mockSetID)

			if err := printAndRenderLogs(logs, newLogger, logDiffs, logger); err != nil {
				return false, err
			}
		}
	} else {
		pass = false
		var testStatusCode string
		for status := range testOperation.Responses {
			testStatusCode = status
			break
		}
		logDiffs.PushStatusDiff(fmt.Sprint(testStatusCode), fmt.Sprint(status))
		logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testName, testSetID, mockName, mockSetID)

		if err := printAndRenderLogs(logs, newLogger, logDiffs, logger); err != nil {
			return false, err
		}
	}
	return pass, nil
}
func match2(mock, test models.OpenAPI, testSetID string, mockSetID string, logger *zap.Logger) (bool, error) {
	pass := false
	newLogger := pp.New()
	newLogger.WithLineInfo = false
	newLogger.SetColorScheme(models.GetFailingColorScheme())

	for path, mockItem := range mock.Paths {
		logDiffs := replaySvc.NewDiffsPrinter(test.Info.Title + "/" + mock.Info.Title)
		var err error
		if testItem, found := test.Paths[path]; found {
			mockOperation, mockOperationType := findOperation(mockItem)
			testOperation, testOperationType := findOperation(testItem)
			if pass, err = compareOperationTypes(mockOperationType, testOperationType, test.Info.Title, testSetID, mock.Info.Title, mockSetID, logDiffs, newLogger, logger); err != nil {
				return false, err
			}
			if !pass {
				continue
			}

			if pass, err = compareRequestBodies(mockOperation, testOperation, logDiffs, newLogger, logger, test.Info.Title, mock.Info.Title, testSetID, mockSetID); err != nil {
				return false, err
			}
			if !pass {
				continue
			}
			var statusCode string
			for status := range mockOperation.Responses {
				statusCode = status
				break

			}
			if pass, err = compareResponseBodies(statusCode, mockOperation, testOperation, logDiffs, newLogger, logger, test.Info.Title, mock.Info.Title, testSetID, mockSetID); err != nil {
				return false, err
			}

		} else {
			if err := handleMissingPath(path, test, mock, testSetID, mockSetID, logDiffs, newLogger, logger); err != nil {
				return false, err
			}
		}

	}
	if pass {
		log2 := newLogger.Sprintf("Contract Check passed for test: %s / mock: %s \n\n--------------------------------------------------------------------\n\n", test.Info.Title, mock.Info.Title)
		_, err := newLogger.Printf(log2)
		if err != nil {
			utils.LogError(logger, err, "failed to print the logs")
			return false, err
		}

	}

	return pass, nil
}
func marshalRequestBodies(mockOperation, testOperation *models.Operation) (string, string, error) {
	var mockRequestBody []byte
	var testRequestBody []byte
	var err error
	if mockOperation.RequestBody != nil {
		mockRequestBody, err = json.Marshal(mockOperation.RequestBody.Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling mock RequestBody: %v", err)
		}
	}
	if testOperation.RequestBody != nil {
		testRequestBody, err = json.Marshal(testOperation.RequestBody.Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling test RequestBody: %v", err)
		}
	}
	return string(mockRequestBody), string(testRequestBody), nil
}

func marshalResponseBodies(status string, mockOperation, testOperation *models.Operation) (string, string, error) {
	var mockResponseBody []byte
	var testResponseBody []byte
	var err error
	if mockOperation.Responses[status].Content != nil {
		mockResponseBody, err = json.Marshal(mockOperation.Responses[status].Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling mock ResponseBody: %v", err)
		}
	}
	if testOperation.Responses[status].Content != nil {
		testResponseBody, err = json.Marshal(testOperation.Responses[status].Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling test ResponseBody: %v", err)
		}
	}
	return string(mockResponseBody), string(testResponseBody), nil
}

func handleJSONDiff(validatedJSON replaySvc.ValidatedJSON, logDiffs replaySvc.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger, testName string, mockName string, testSetID string, mockSetID string, mockBodyStr string, testBodyStr string, diffType string) (bool, error) {
	pass := true
	jsonComparisonResult, err := replaySvc.JSONDiffWithNoiseControl(validatedJSON, nil, false)
	if err != nil {
		return false, err
	}
	if !jsonComparisonResult.IsExact() {
		pass = false
		logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testName, testSetID, mockName, mockSetID)
		if json.Valid([]byte(mockBodyStr)) {
			patch, err := jsondiff.Compare(testBodyStr, mockBodyStr)
			if err != nil {
				logger.Warn("failed to compute json diff", zap.Error(err))
				return false, err
			}
			for _, op := range patch {
				if jsonComparisonResult.Matches() {
					logDiffs.HasarrayIndexMismatch(true)
					logDiffs.PushFooterDiff(strings.Join(jsonComparisonResult.Differences(), ", "))
				}
				if diffType == "request" {
					logDiffs.PushRequestDiff(fmt.Sprint(op.OldValue), fmt.Sprint(op.Value))

				} else if diffType == "response" {
					logDiffs.PushBodyDiff(fmt.Sprint(op.OldValue), fmt.Sprint(op.Value), nil)
				}
			}
		}
		if err := printAndRenderLogs(logs, newLogger, logDiffs, logger); err != nil {
			return false, err
		}
	}
	return pass, nil
}

func handleMissingPath(path string, test, mock models.OpenAPI, testSetID, mockSetID string, logDiffs replaySvc.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger) error {
	logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", test.Info.Title, testSetID, mock.Info.Title, mockSetID)
	var testPath string
	for path := range test.Paths {
		testPath = path
		break
	}
	logDiffs.PushPathDiff(fmt.Sprint(testPath), fmt.Sprint(path))
	return printAndRenderLogs(logs, newLogger, logDiffs, logger)
}
func printAndRenderLogs(logs string, newLogger *pp.PrettyPrinter, logDiffs replaySvc.DiffsPrinter, logger *zap.Logger) error {
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
