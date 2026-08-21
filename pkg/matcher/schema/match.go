// Package schema for schema matching
package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	matcher "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
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

func compareResponseBodies(status string, mockOperation, testOperation *models.Operation, logDiffs matcher.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger, testName, mockName, testSetID, mockSetID string, mode models.SchemaMatchMode) (float64, bool, error) {
	testResponse, ok := testOperation.Responses[status]
	if !ok {
		// The test case never produced this status code, so the mock is not a
		// candidate for it. Returning before any arithmetic is the point: the
		// old code set the sentinel and then divided it by the mock's property
		// count on the way out, turning -1 into -Inf, or into NaN whenever the
		// mock had no properties to divide by.
		return NOTCANDIDATE, false, nil
	}

	mockResponseBodyStr, testResponseBodyStr, err := matcher.MarshalResponseBodies(status, mockOperation, testOperation)
	if err != nil {
		return NOTCANDIDATE, false, err
	}

	// Score from the schemas, in both modes, so a mock's score never depends on
	// which mode computed it.
	score := schemaSimilarity(
		mockOperation.Responses[status].Content["application/json"].Schema,
		testResponse.Content["application/json"].Schema,
	)

	validatedJSON, err := matcher.ValidateAndMarshalJSON(logger, &mockResponseBodyStr, &testResponseBodyStr)
	if err != nil {
		return NOTCANDIDATE, false, err
	}

	// CompareMode does not score; it re-runs the comparison for the chosen
	// candidate purely to render the diff the user sees.
	if mode == models.CompareMode {
		if validatedJSON.IsIdentical() {
			if _, _, err = handleJSONDiff(validatedJSON, logDiffs, newLogger, logger, testName, mockName, testSetID, mockSetID, mockResponseBodyStr, testResponseBodyStr, "response", mode); err != nil {
				return NOTCANDIDATE, false, err
			}
		} else {
			logDiffs.PushTypeDiff(fmt.Sprint(reflect.TypeOf(validatedJSON.Expected())), fmt.Sprint(reflect.TypeOf(validatedJSON.Actual())))
			logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testName, testSetID, mockName, mockSetID)

			if err := printAndRenderLogs(logs, newLogger, logDiffs, logger); err != nil {
				return NOTCANDIDATE, false, err
			}
		}
	}

	return score, true, nil
}

// schemaSimilarity reports the fraction of what the mock's response schema
// requires that the test's response schema provides: 1 means the test covers
// the mock completely, 0 means it shares nothing with it. A test carrying extra
// fields still scores 1 - the mock is what has to be satisfied.
//
// Every return is a literal or a division whose denominator is guaranteed
// non-zero, which is the property that matters. The score used to be computed
// as differencesCount/len(mockSchema.Properties), and an array-root, scalar-root
// or body-less response has no properties at all: 0/0 = NaN. NaN loses every
// comparison, so consumer's `pass && candidateScore > best` gate rejected it and
// the mock was reported MISSED no matter which test it was scored against.
//
// It recurses into Items because that is the only thing an array-root schema
// carries. Returning a flat 1 for "no properties" would make every array-root
// mock score 1 against every array-root test, so ties would be broken by Go's
// map iteration order and []int would match []string.
//
// Property comparison stays top-level and type-only, as it has always been:
// recursing into nested object properties would move the scores of contracts
// that match correctly today, which is a separate change.
func schemaSimilarity(mock, test models.Schema) float64 {
	if !sameSchemaType(mock.Type, test.Type) {
		// An object response and an array response are not the same shape.
		return 0
	}

	if len(mock.Properties) > 0 {
		matches := 0.0
		for key, mockProp := range mock.Properties {
			if testProp, ok := test.Properties[key]; ok && sameSchemaType(mockProp["type"], testProp["type"]) {
				matches++
			}
		}
		return matches / float64(len(mock.Properties))
	}

	if mock.Type == "array" {
		if mock.Items == nil {
			// The mock says nothing about its elements, so there is nothing
			// for the test to disagree with.
			return 1
		}
		if test.Items == nil {
			return 0
		}
		return schemaSimilarity(*mock.Items, *test.Items)
	}

	// A scalar root, or no JSON body on either side (a 204, or a DELETE with an
	// empty response). The types already agreed above and there is nothing else
	// in the contract to compare.
	return 1
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

			if candidateScore, pass, err = compareResponseBodies(statusCode, mockOperation, testOperation, logDiffs, newLogger, logger, test.Info.Title, mock.Info.Title, testSetID, mockSetID, mode); err != nil {
				return candidateScore, false, err
			}

		} else {
			pass = false

		}

	}

	return candidateScore, pass, nil
}

// sameSchemaType reports whether two OpenAPI type names describe the same
// field for matching purposes.
//
// "integer" and "number" are treated as equal because contracts generated by
// different keploy versions disagree on them: until integer inference was
// added, every JSON number was typed "number". `keploy contract download`
// copies another service's already-generated schema verbatim rather than
// regenerating it, so a provider on an older keploy and a consumer on a newer
// one legitimately hold both spellings of the same field. Scoring those as a
// mismatch drops a mock's score to 0 for a single-property schema, which
// reports it as MISSED - a false failure caused purely by version skew.
func sameSchemaType(a, b interface{}) bool {
	if a == b {
		return true
	}
	numeric := func(v interface{}) bool { return v == "integer" || v == "number" }
	return numeric(a) && numeric(b)
}

func handleJSONDiff(validatedJSON matcher.ValidatedJSON, logDiffs matcher.DiffsPrinter, newLogger *pp.PrettyPrinter, logger *zap.Logger, _ string, _ string, _ string, _ string, mockBodyStr string, testBodyStr string, diffType string, mode models.SchemaMatchMode) (float64, bool, error) {
	pass := true
	differencesCount := 0.0
	jsonComparisonResult, err := matcher.JSONDiffWithNoiseControl(validatedJSON, nil, false, logger)
	if err != nil {
		return differencesCount, false, err
	}
	if !jsonComparisonResult.IsExact() {
		pass = false
		// logs := newLogger.Sprintf("Contract Check failed for test: %s (%s) / mock: %s (%s) \n\n--------------------------------------------------------------------\n\n", testName, testSetID, mockName, mockSetID)
		if json.Valid([]byte(mockBodyStr)) {
			patch, err := jsondiff.Compare(testBodyStr, mockBodyStr)
			if err != nil {
				logger.Debug("failed to compute json diff", zap.Error(err))
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
