// Package grpc provides gRPC response matching functionality
package grpc

import (
	"encoding/json"
	"fmt"
	"strconv"
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
	differences := make(map[string]grpcDiff)

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
			// :status is the HTTP/2 transport-layer header (always 200 for gRPC);
			// do not classify its absence as StatusCodeChanged — the real gRPC
			// status is compared below via the grpc-status trailer.
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

	// Handle noise configuration first - needed for JSON comparison
	noise := tc.Noise

	// Copy before merging: noiseConfig is the caller's long-lived global map.
	var (
		bodyNoise   = matcher.CloneNoiseMap(noiseConfig["body"])
		headerNoise = matcher.CloneNoiseMap(noiseConfig["header"]) // need to handle noisy header separately (not implemented yet for grpc)
	)

	// Merge test-case-specific noise with global noise (similar to HTTP matcher).
	// TODO: gRPC has never honoured the whole-body skip sentinel that the HTTP
	// matcher applies, so skipBody is dropped here to preserve that behaviour.
	tcBodyNoise, tcHeaderNoise, _ := matcher.SplitNoise(noise, logger)
	for field, regexArr := range tcBodyNoise {
		bodyNoise[field] = regexArr
	}
	for field, regexArr := range tcHeaderNoise {
		headerNoise[field] = regexArr
	}

	// Compare the body POSITIONALLY, one length-prefixed message at a time.
	//
	// Order is the entire semantic content of a stream, so comparison has to
	// be by position. Concatenating messages into one string and diffing that
	// would let a stream replayed in reverse order match, and so would routing
	// a multi-message body through CanonicalizeTopLevelBlocks, which sorts
	// sibling blocks.
	expMsgs := expectedResp.AllMessages()
	actMsgs := actualResp.AllMessages()

	// The COUNT gets its own verdict. Comparing only min(len(exp), len(act))
	// and calling that a match means a server returning 3 of 5 recorded
	// messages passes — strictly worse than not supporting streams, because
	// the user now believes streams are covered.
	if len(expMsgs) != len(actMsgs) {
		differences["body.message_count"] = grpcDiff{
			Expected: fmt.Sprintf("%d", len(expMsgs)),
			Actual:   fmt.Sprintf("%d", len(actMsgs)),
			Message:  "gRPC message count mismatch",
		}
		result.BodyResult = append(result.BodyResult, models.BodyResult{
			Normal:   false,
			Type:     models.GrpcLength,
			Expected: fmt.Sprintf("%d message(s)", len(expMsgs)),
			Actual:   fmt.Sprintf("%d message(s)", len(actMsgs)),
		})
		currentRisk = models.High
		currentCategories = append(currentCategories, models.SchemaBroken)
	}

	// Keys stay UNPREFIXED while both sides carry a single message, which is
	// every recording made before streams were representable. A user's
	// assertions.noise holds the bare string `body.decoded_data`, and the
	// report reader keys off these too, so unary comparison must emit exactly
	// the keys it always did.
	indexed := useIndexedKeys(expMsgs, actMsgs)

	compareCount := len(expMsgs)
	if len(actMsgs) < compareCount {
		compareCount = len(actMsgs)
	}

	// Carried out for the failure assessment below: the FIRST message that
	// differs, which is the one the user needs to look at.
	decodedDataNormal := true
	expectedDecodedData := ""
	actualDecodedData := ""
	var jsonComparisonResult matcher.JSONComparisonResult

	for msgIdx := 0; msgIdx < compareCount; msgIdx++ {
		cmp := compareGrpcMessage(msgIdx, indexed, expMsgs[msgIdx], actMsgs[msgIdx],
			differences, result, bodyNoise, ignoreOrdering, logger)
		if !cmp.decodedNormal && decodedDataNormal {
			// first mismatch wins
			decodedDataNormal = false
			expectedDecodedData = cmp.expected
			actualDecodedData = cmp.actual
			jsonComparisonResult = cmp.json
		}
	}
	if decodedDataNormal && compareCount > 0 {
		// Nothing differed; surface message 0 for the report's benefit.
		expectedDecodedData = expMsgs[0].DecodedData
		actualDecodedData = actMsgs[0].DecodedData
	}

	// Apply noise configuration to ignore specified differences
	for path := range differences {
		pathParts := strings.Split(path, ".")
		if len(pathParts) > 1 {
			if pathParts[0] == "body" && len(bodyNoise) > 0 {
				// Strip a per-message index before the lookup. A user's
				// assertions.noise holds bare `decoded_data` /
				// `message_length` / `compression_flag`; joining a streaming
				// key's parts would ask for `1.decoded_data`, match nothing,
				// and silently render every recorded gRPC noise entry inert —
				// so previously-suppressed flakiness would reappear as
				// failures with no diagnostic, since this matcher never calls
				// WarnUnmatchableBodyNoise.
				fields := pathParts[1:]
				if len(fields) > 1 && isAllDigits(fields[0]) {
					fields = fields[1:]
				}
				if _, found := bodyNoise[strings.Join(fields, ".")]; found {
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

	var bodyAssessment *models.FailureAssessment
	if !decodedDataNormal {
		if json.Valid([]byte(expectedDecodedData)) && json.Valid([]byte(actualDecodedData)) {
			if assess, err := matcher.ComputeFailureAssessmentJSON(expectedDecodedData, actualDecodedData, bodyNoise, ignoreOrdering); err == nil && assess != nil {
				currentRisk = matcher.MaxRisk(currentRisk, assess.Risk)
				currentCategories = append(currentCategories, assess.Category...)
				bodyAssessment = assess
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

	// Compare grpc-status trailer — this is the canonical gRPC status code.
	// HTTP/2 :status (always 200 for gRPC) is transport framing and must not
	// be used as the gRPC status; grpc-status: 0 = OK, non-zero = error.
	expectedGrpcStatus := parseGrpcStatus(expectedResp.Trailers.OrdinaryHeaders["grpc-status"])
	actualGrpcStatus := parseGrpcStatus(actualResp.Trailers.OrdinaryHeaders["grpc-status"])
	result.StatusCode = models.IntResult{
		Normal:   expectedGrpcStatus == actualGrpcStatus,
		Expected: expectedGrpcStatus,
		Actual:   actualGrpcStatus,
	}
	if !result.StatusCode.Normal {
		differences["trailers.grpc-status"] = grpcDiff{
			Expected: strconv.Itoa(expectedGrpcStatus),
			Actual:   strconv.Itoa(actualGrpcStatus),
			Message:  "grpc-status mismatch",
		}
		currentRisk = models.High
		currentCategories = append(currentCategories, models.StatusCodeChanged)
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
		Risk:       currentRisk,
		Category:   uniqueCategories,
		Assessment: bodyAssessment,
	}

	return matched, result
}

// parseGrpcStatus parses a grpc-status trailer value to int.
// An empty string (trailer absent) is treated as 0 (OK) — the gRPC default.
func parseGrpcStatus(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		// Non-numeric grpc-status trailer — treat as unknown error so it
		// causes a mismatch rather than silently passing as OK (0).
		return -1
	}
	return n
}

// grpcDiff is one entry in the differences map. It was an anonymous struct
// literal repeated at every site; naming it is what lets the per-message
// comparison live in its own function.
type grpcDiff struct {
	Expected string
	Actual   string
	Message  string
}

// msgComparison reports how one length-prefixed message compared.
type msgComparison struct {
	decodedNormal bool
	expected      string
	actual        string
	json          matcher.JSONComparisonResult
}

// isAllDigits reports whether s is a non-empty run of ASCII digits, i.e. a
// message index in a difference key like `body.1.decoded_data`.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// compareGrpcMessage compares ONE length-prefixed message and records its
// differences and BodyResult entries.
//
// This is the body-comparison logic that used to be inline against
// GrpcResp.Body, unchanged in substance — the same compression-flag, length
// and decoded-data checks, the same JSON-versus-canonicalization split, and
// the same rule that a length difference is forgiven when the decoded data is
// identical. What is new is that it runs once per message and that its
// difference keys can carry the message index.
//
// The key shape is deliberate. With a single message on both sides the keys
// are exactly what they have always been (`body.decoded_data`), so existing
// noise entries, reports and UI keep working untouched. Only a real stream
// produces `body.<n>.decoded_data`.
func compareGrpcMessage(
	msgIdx int,
	indexed bool,
	expMsg, actMsg models.GrpcLengthPrefixedMessage,
	differences map[string]grpcDiff,
	result *models.Result,
	bodyNoise map[string][]string,
	ignoreOrdering bool,
	logger *zap.Logger,
) msgComparison {
	key := func(field string) string {
		if !indexed {
			return "body." + field
		}
		return fmt.Sprintf("body.%d.%s", msgIdx, field)
	}

	// Compare compression flag
	compressionFlagNormal := expMsg.CompressionFlag == actMsg.CompressionFlag
	if !compressionFlagNormal {
		differences[key("compression_flag")] = grpcDiff{
			Expected: fmt.Sprintf("%d", expMsg.CompressionFlag),
			Actual:   fmt.Sprintf("%d", actMsg.CompressionFlag),
			Message:  "compression flag mismatch",
		}
	}
	result.BodyResult = append(result.BodyResult, models.BodyResult{
		Normal:   compressionFlagNormal,
		Type:     models.GrpcCompression,
		Expected: fmt.Sprintf("%d", expMsg.CompressionFlag),
		Actual:   fmt.Sprintf("%d", actMsg.CompressionFlag),
	})

	// Compare message length
	messageLengthNormal := expMsg.MessageLength == actMsg.MessageLength
	if !messageLengthNormal {
		differences[key("message_length")] = grpcDiff{
			Expected: fmt.Sprintf("%d", expMsg.MessageLength),
			Actual:   fmt.Sprintf("%d", actMsg.MessageLength),
			Message:  "message length mismatch",
		}
	}
	lengthResultIdx := len(result.BodyResult)
	result.BodyResult = append(result.BodyResult, models.BodyResult{
		Normal:   messageLengthNormal,
		Type:     models.GrpcLength,
		Expected: fmt.Sprintf("%d", expMsg.MessageLength),
		Actual:   fmt.Sprintf("%d", actMsg.MessageLength),
	})

	// Compare decoded data — JSON comparison when both sides are valid JSON,
	// canonicalization otherwise.
	decodedDataNormal := true
	expectedDecodedData := expMsg.DecodedData
	actualDecodedData := actMsg.DecodedData
	var jsonComparisonResult matcher.JSONComparisonResult

	if json.Valid([]byte(expectedDecodedData)) && json.Valid([]byte(actualDecodedData)) {
		logger.Debug("Both gRPC decoded data are valid JSON, using JSON comparison",
			zap.Int("message_index", msgIdx),
			zap.String("expectedDecodedData", expectedDecodedData),
			zap.String("actualDecodedData", actualDecodedData))

		expectedDecodedData = matcher.NormalizeNestedJSONForNoise(expectedDecodedData, bodyNoise, logger)
		actualDecodedData = matcher.NormalizeNestedJSONForNoise(actualDecodedData, bodyNoise, logger)

		validatedJSON, err := matcher.ValidateAndMarshalJSON(logger, &expectedDecodedData, &actualDecodedData)
		if err != nil {
			logger.Error("Failed to validate and marshal JSON for gRPC decoded data", zap.Error(err))
			decodedDataNormal = false
		} else if validatedJSON.IsIdentical() {
			jsonComparisonResult, err = matcher.JSONDiffWithNoiseControl(validatedJSON, bodyNoise, ignoreOrdering, logger)
			decodedDataNormal = jsonComparisonResult.IsExact()
			if err != nil {
				logger.Error("Failed to perform JSON diff with noise control", zap.Error(err))
				decodedDataNormal = false
			}
		} else {
			logger.Debug("JSON structures are not identical, marking as mismatch")
			decodedDataNormal = false
		}
	} else {
		logger.Debug("At least one gRPC decoded data is not valid JSON, using canonicalization",
			zap.Int("message_index", msgIdx),
			zap.Bool("expectedIsJSON", json.Valid([]byte(expectedDecodedData))),
			zap.Bool("actualIsJSON", json.Valid([]byte(actualDecodedData))))

		// Per-message only. Canonicalizing a whole stream would sort across
		// message boundaries and let a reordered stream match.
		expCanon := CanonicalizeTopLevelBlocks(expectedDecodedData)
		actCanon := CanonicalizeTopLevelBlocks(actualDecodedData)
		decodedDataNormal = expCanon == actCanon
		expectedDecodedData = expCanon
		actualDecodedData = actCanon
	}

	if !decodedDataNormal {
		differences[key("decoded_data")] = grpcDiff{
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

	// A length difference is forgiven when the decoded data is identical —
	// mocks can be hand-edited, and CreatePayloadFromLengthPrefixedMessage
	// deliberately re-derives the length from the re-encoded payload. Scoped
	// to THIS message's length result, not the first one in the slice.
	if decodedDataNormal && !messageLengthNormal {
		logger.Debug("Ignoring message length mismatch since decoded data is identical",
			zap.Int("message_index", msgIdx),
			zap.Uint32("expected", expMsg.MessageLength),
			zap.Uint32("actual", actMsg.MessageLength))
		result.BodyResult[lengthResultIdx].Normal = true
		delete(differences, key("message_length"))
	}

	return msgComparison{
		decodedNormal: decodedDataNormal,
		expected:      expectedDecodedData,
		actual:        actualDecodedData,
		json:          jsonComparisonResult,
	}
}

// useIndexedKeys reports whether difference keys should carry a message
// index.
//
// They must NOT while both sides carry a single message. Every recording made
// before streams were representable is unary, users' assertions.noise holds
// the bare string `body.decoded_data`, and the report reader and UI key off
// these names. Indexing unconditionally would rename every unary failure —
// invisibly, because the noise lookup strips the index and would keep
// suppressing correctly either way.
func useIndexedKeys(expMsgs, actMsgs []models.GrpcLengthPrefixedMessage) bool {
	return len(expMsgs) > 1 || len(actMsgs) > 1
}
