// Package grpc provides gRPC response matching functionality
package grpc

import (
	"fmt"
	"strings"

	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/pkg/matcher"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// Match compares an expected gRPC response with an actual response and returns whether they match
// along with detailed comparison results
func Match(tc *models.TestCase, actualResp *models.GrpcResp, noiseConfig map[string]map[string][]string, logger *zap.Logger) (bool, *models.Result) {
	expectedResp := tc.GrpcResp
	result := &models.Result{
		HeadersResult: make([]models.HeaderResult, 0),
		BodyResult:    make([]models.BodyResult, 0),
		TrailerResult: make([]models.HeaderResult, 0),
	}

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
		Type:     models.BodyTypeGrpcCompression,
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
		Type:     models.BodyTypeGrpcLength,
		Expected: fmt.Sprintf("%d", expectedResp.Body.MessageLength),
		Actual:   fmt.Sprintf("%d", actualResp.Body.MessageLength),
	})

	// Compare decoded data
	decodedDataNormal := expectedResp.Body.DecodedData == actualResp.Body.DecodedData
	if !decodedDataNormal {
		differences["body.decoded_data"] = struct {
			Expected string
			Actual   string
			Message  string
		}{
			Expected: expectedResp.Body.DecodedData,
			Actual:   actualResp.Body.DecodedData,
			Message:  "decoded data mismatch",
		}
	}
	result.BodyResult = append(result.BodyResult, models.BodyResult{
		Normal:   decodedDataNormal,
		Type:     models.BodyTypeGrpcData,
		Expected: expectedResp.Body.DecodedData,
		Actual:   actualResp.Body.DecodedData,
	})

	// Handle noise configuration
	var (
		bodyNoise   = noiseConfig["body"]
		headerNoise = noiseConfig["header"]
	)

	if bodyNoise == nil {
		bodyNoise = map[string][]string{}
	}
	if headerNoise == nil {
		headerNoise = map[string][]string{}
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
		_, err := newLogger.Printf(logs)
		if err != nil {
			utils.LogError(logger, err, "failed to print the logs")
		}

		err = logDiffs.Render()
		if err != nil {
			utils.LogError(logger, err, "failed to render the diffs")
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

	return matched, result
}
