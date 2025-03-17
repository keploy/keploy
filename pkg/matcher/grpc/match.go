// Package grpc provides gRPC response matching functionality
package grpc

import (
	"fmt"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
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
	matched := true
	differences := make(map[string]struct {
		Expected string
		Actual   string
		Message  string
	})

	logger.Debug("comparing gRPC headers")
	// Compare pseudo headers
	for k, expectedVal := range expectedResp.Headers.PseudoHeaders {
		actualVal, exists := actualResp.Headers.PseudoHeaders[k]
		if !exists {
			logger.Debug("missing pseudo header in response",
				zap.String("header", k),
				zap.String("expected", expectedVal))
			matched = false
			differences[fmt.Sprintf("headers.pseudo_headers.%s", k)] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedVal,
				Actual:   "",
				Message:  "missing pseudo header in response",
			}
			result.HeadersResult = append(result.HeadersResult, models.HeaderResult{
				Normal: false,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{},
				},
			})
			continue
		}
		if expectedVal != actualVal {
			logger.Debug("pseudo header value mismatch",
				zap.String("header", k),
				zap.String("expected", expectedVal),
				zap.String("actual", actualVal))
			matched = false
			differences[fmt.Sprintf("headers.pseudo_headers.%s", k)] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedVal,
				Actual:   actualVal,
				Message:  "pseudo header value mismatch",
			}
			result.HeadersResult = append(result.HeadersResult, models.HeaderResult{
				Normal: false,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{actualVal},
				},
			})
		} else {
			logger.Debug("pseudo header matched",
				zap.String("header", k),
				zap.String("value", expectedVal))
			result.HeadersResult = append(result.HeadersResult, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{actualVal},
				},
			})
		}
	}

	logger.Debug("comparing gRPC ordinary headers")
	// Compare ordinary headers
	for k, expectedVal := range expectedResp.Headers.OrdinaryHeaders {
		actualVal, exists := actualResp.Headers.OrdinaryHeaders[k]
		if !exists {
			logger.Debug("missing ordinary header in response",
				zap.String("header", k),
				zap.String("expected", expectedVal))
			matched = false
			differences[fmt.Sprintf("headers.ordinary_headers.%s", k)] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedVal,
				Actual:   "",
				Message:  "missing ordinary header in response",
			}
			result.HeadersResult = append(result.HeadersResult, models.HeaderResult{
				Normal: false,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{},
				},
			})
			continue
		}
		if expectedVal != actualVal {
			logger.Debug("ordinary header value mismatch",
				zap.String("header", k),
				zap.String("expected", expectedVal),
				zap.String("actual", actualVal))
			matched = false
			differences[fmt.Sprintf("headers.ordinary_headers.%s", k)] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedVal,
				Actual:   actualVal,
				Message:  "ordinary header value mismatch",
			}
			result.HeadersResult = append(result.HeadersResult, models.HeaderResult{
				Normal: false,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{actualVal},
				},
			})
		} else {
			logger.Debug("ordinary header matched",
				zap.String("header", k),
				zap.String("value", expectedVal))
			result.HeadersResult = append(result.HeadersResult, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{actualVal},
				},
			})
		}
	}

	logger.Debug("comparing gRPC body")
	// Compare Body - using specialized body types for gRPC

	// Compare compression flag
	compressionFlagNormal := expectedResp.Body.CompressionFlag == actualResp.Body.CompressionFlag
	if !compressionFlagNormal {
		logger.Debug("compression flag mismatch",
			zap.Uint("expected", expectedResp.Body.CompressionFlag),
			zap.Uint("actual", actualResp.Body.CompressionFlag))
		matched = false
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
		logger.Debug("message length mismatch",
			zap.Uint32("expected", expectedResp.Body.MessageLength),
			zap.Uint32("actual", actualResp.Body.MessageLength))
		matched = false
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
		logger.Debug("decoded data mismatch",
			zap.String("expected", expectedResp.Body.DecodedData),
			zap.String("actual", actualResp.Body.DecodedData))
		matched = false
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

	logger.Debug("comparing gRPC trailers")
	// Compare trailers - using the new TrailerResult field
	for k, expectedVal := range expectedResp.Trailers.PseudoHeaders {
		actualVal, exists := actualResp.Trailers.PseudoHeaders[k]
		if !exists {
			logger.Debug("missing pseudo trailer in response",
				zap.String("trailer", k),
				zap.String("expected", expectedVal))
			matched = false
			differences[fmt.Sprintf("trailers.pseudo_headers.%s", k)] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedVal,
				Actual:   "",
				Message:  "missing pseudo trailer in response",
			}
			result.TrailerResult = append(result.TrailerResult, models.HeaderResult{
				Normal: false,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{},
				},
			})
			continue
		}
		if expectedVal != actualVal {
			logger.Debug("pseudo trailer value mismatch",
				zap.String("trailer", k),
				zap.String("expected", expectedVal),
				zap.String("actual", actualVal))
			matched = false
			differences[fmt.Sprintf("trailers.pseudo_headers.%s", k)] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedVal,
				Actual:   actualVal,
				Message:  "pseudo trailer value mismatch",
			}
			result.TrailerResult = append(result.TrailerResult, models.HeaderResult{
				Normal: false,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{actualVal},
				},
			})
		} else {
			logger.Debug("pseudo trailer matched",
				zap.String("trailer", k),
				zap.String("value", expectedVal))
			result.TrailerResult = append(result.TrailerResult, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{actualVal},
				},
			})
		}
	}

	// Compare ordinary trailers
	for k, expectedVal := range expectedResp.Trailers.OrdinaryHeaders {
		actualVal, exists := actualResp.Trailers.OrdinaryHeaders[k]
		if !exists {
			logger.Debug("missing ordinary trailer in response",
				zap.String("trailer", k),
				zap.String("expected", expectedVal))
			matched = false
			differences[fmt.Sprintf("trailers.ordinary_headers.%s", k)] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedVal,
				Actual:   "",
				Message:  "missing ordinary trailer in response",
			}
			result.TrailerResult = append(result.TrailerResult, models.HeaderResult{
				Normal: false,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{},
				},
			})
			continue
		}
		if expectedVal != actualVal {
			logger.Debug("ordinary trailer value mismatch",
				zap.String("trailer", k),
				zap.String("expected", expectedVal),
				zap.String("actual", actualVal))
			matched = false
			differences[fmt.Sprintf("trailers.ordinary_headers.%s", k)] = struct {
				Expected string
				Actual   string
				Message  string
			}{
				Expected: expectedVal,
				Actual:   actualVal,
				Message:  "ordinary trailer value mismatch",
			}
			result.TrailerResult = append(result.TrailerResult, models.HeaderResult{
				Normal: false,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{actualVal},
				},
			})
		} else {
			logger.Debug("ordinary trailer matched",
				zap.String("trailer", k),
				zap.String("value", expectedVal))
			result.TrailerResult = append(result.TrailerResult, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: []string{expectedVal},
				},
				Actual: models.Header{
					Key:   k,
					Value: []string{actualVal},
				},
			})
		}
	}

	// Handle noise configuration
	var (
		bodyNoise    = noiseConfig["body"]
		headerNoise  = noiseConfig["header"]
		trailerNoise = noiseConfig["trailer"]
	)

	if bodyNoise == nil {
		bodyNoise = map[string][]string{}
	}
	if headerNoise == nil {
		headerNoise = map[string][]string{}
	}
	if trailerNoise == nil {
		trailerNoise = map[string][]string{}
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
			} else if pathParts[0] == "trailers" && len(trailerNoise) > 0 {
				if _, found := trailerNoise[pathParts[len(pathParts)-1]]; found {
					delete(differences, path)
				}
			}
		}
	}

	// Recalculate match status after applying noise
	matched = len(differences) == 0

	// Log for debugging
	if !matched {
		logger.Debug("gRPC response matching failed", zap.Int("difference_count", len(differences)))
	} else {
		logger.Debug("gRPC response matched successfully")
	}

	return matched, result
}
