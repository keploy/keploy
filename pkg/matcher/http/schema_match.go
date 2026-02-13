package http

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// MatchSchema checks if the actual response matches the expected response schema.
func MatchSchema(tc *models.TestCase, actualResponse *models.HTTPResp, logger *zap.Logger) (bool, *models.Result) {
	pass := true
	result := &models.Result{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: tc.HTTPResp.StatusCode,
			Actual:   actualResponse.StatusCode,
		},
		BodyResult: []models.BodyResult{{
			Normal:   false,
			Expected: tc.HTTPResp.Body,
			Actual:   actualResponse.Body,
		}},
	}

	// Status Code Match
	if tc.HTTPResp.StatusCode == actualResponse.StatusCode {
		result.StatusCode.Normal = true
	} else {
		pass = false
	}

	// Body Schema Match
	var expObj, actObj interface{}
	errExp := json.Unmarshal([]byte(tc.HTTPResp.Body), &expObj)
	errAct := json.Unmarshal([]byte(actualResponse.Body), &actObj)

	var schemaErrors []matcher.SchemaError

	if errExp == nil && errAct == nil {
		result.BodyResult[0].Type = models.JSON
		schemaErrors = schemaMatchRecursive(expObj, actObj, "body", logger)
		if len(schemaErrors) > 0 {
			pass = false
		}
		result.BodyResult[0].Normal = len(schemaErrors) == 0
	} else {
		if (errExp == nil) != (errAct == nil) {
			pass = false
			result.BodyResult[0].Normal = false
			schemaErrors = append(schemaErrors, matcher.SchemaError{
				Reason: "One of the body is json and other is not",
			})
		} else {
			// Both non-JSON: fallback to strict equality.
			bodyMatch := tc.HTTPResp.Body == actualResponse.Body
			result.BodyResult[0].Normal = bodyMatch
			if !bodyMatch {
				pass = false
				schemaErrors = append(schemaErrors, matcher.SchemaError{
					Reason: "Body mismatch (non-JSON)",
				})
			}
		}
	}

	// Logging similar to Match() in match.go
	if !pass {
		printer := matcher.NewSchemaDiffPrinter(tc.Name)
		for _, err := range schemaErrors {
			printer.PushError(err.Reason, err.Expected, err.Actual)
		}
		if err := printer.Render(); err != nil {
			utils.LogError(logger, err, "failed to print schema diffs")
		}
	} else {
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

	// Check headers existence
	for k := range tc.HTTPResp.Header {
		if _, ok := actualResponse.Header[k]; !ok {
			pass = false
		}
	}

	return pass, result
}

func schemaMatchRecursive(expected, actual interface{}, path string, logger *zap.Logger) []matcher.SchemaError {
	var errors []matcher.SchemaError

	// Handle Nil Cases
	if expected == nil {
		if actual == nil {
			return errors
		}
		errors = append(errors, matcher.SchemaError{
			Reason:   fmt.Sprintf("mismatch at %s", path),
			Expected: "nil",
			Actual:   fmt.Sprintf("%T", actual),
		})
		return errors
	}

	if actual == nil {
		errors = append(errors, matcher.SchemaError{
			Reason:   fmt.Sprintf("mismatch at %s", path),
			Expected: fmt.Sprintf("%T", expected),
			Actual:   "nil",
		})
		return errors
	}

	expType := reflect.TypeOf(expected)
	actType := reflect.TypeOf(actual)

	// Type Check with Numeric Compatibility
	if expType != actType {
		if !isNumeric(expType.Kind()) || !isNumeric(actType.Kind()) {
			errors = append(errors, matcher.SchemaError{
				Reason:   fmt.Sprintf("type mismatch at %s", path),
				Expected: fmt.Sprintf("%T", expected),
				Actual:   fmt.Sprintf("%T", actual),
			})
			return errors
		}
	}

	// Recursive Check
	expKind := expType.Kind()

	// If expected was an interface, get the underlying kind
	if expKind == reflect.Interface {
		expKind = reflect.ValueOf(expected).Elem().Kind()
	}

	switch expKind {
	case reflect.Map:
		expVal := reflect.ValueOf(expected)
		actVal := reflect.ValueOf(actual)

		if actVal.Kind() != reflect.Map {
			errors = append(errors, matcher.SchemaError{
				Reason:   fmt.Sprintf("type mismatch at %s", path),
				Expected: "Map",
				Actual:   fmt.Sprintf("%v", actVal.Kind()),
			})
			return errors
		}

		// Iterate over EXPECTED keys
		for _, key := range expVal.MapKeys() {
			actValue := actVal.MapIndex(key)

			// Construct new path
			newPath := fmt.Sprintf("%s.%v", path, key)
			if path == "" {
				newPath = fmt.Sprintf("%v", key)
			}

			if !actValue.IsValid() {
				errors = append(errors, matcher.SchemaError{
					Reason:   fmt.Sprintf("missing key at %s", path),
					Expected: fmt.Sprintf("%v", key),
					Actual:   "(missing)",
				})
				continue
			}

			// Recursion - Collect errors from children
			childErrors := schemaMatchRecursive(expVal.MapIndex(key).Interface(), actValue.Interface(), newPath, logger)
			errors = append(errors, childErrors...)
		}

	case reflect.Slice, reflect.Array:
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)

		minLen := expSlice.Len()
		if actSlice.Len() < minLen {
			minLen = actSlice.Len()
		}

		for i := 0; i < minLen; i++ {
			vExp := expSlice.Index(i).Interface()
			vAct := actSlice.Index(i).Interface()
			newPath := fmt.Sprintf("%s[%d]", path, i)

			childErrors := schemaMatchRecursive(vExp, vAct, newPath, logger)
			errors = append(errors, childErrors...)
		}

	default:
		// Primitives - already checked types above
		return errors
	}

	return errors
}

// Helper to handle Go's strict types vs JSON loose numbers
func isNumeric(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
