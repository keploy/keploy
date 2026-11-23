package grpc

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func newTestGrpcResp(body, grpcStatus string) *models.GrpcResp {
	trailers := map[string]string{}
	if grpcStatus != "" {
		trailers["grpc-status"] = grpcStatus
	}
	return &models.GrpcResp{
		Body: models.GrpcLengthPrefixedMessage{
			DecodedData:   body,
			MessageLength: uint32(len(body)),
		},
		Headers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{":status": "200"},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
		},
		Trailers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{},
			OrdinaryHeaders: trailers,
		},
	}
}

func TestAssertionsReplaceComparison(t *testing.T) {
	cases := []struct {
		name string
		tc   *models.TestCase
		want bool
	}{
		{name: "nil testcase", tc: nil, want: false},
		{name: "empty assertions", tc: &models.TestCase{}, want: false},
		{name: "noise only map", tc: &models.TestCase{Assertions: map[models.AssertionType]interface{}{models.NoiseAssertion: map[string][]string{}}}, want: false},
		{name: "noise only nil value", tc: &models.TestCase{Assertions: map[models.AssertionType]interface{}{models.NoiseAssertion: nil}}, want: false},
		{name: "single real assertion", tc: &models.TestCase{Assertions: map[models.AssertionType]interface{}{models.StatusCode: 0}}, want: true},
		{name: "noise plus real assertion", tc: &models.TestCase{Assertions: map[models.AssertionType]interface{}{models.NoiseAssertion: nil, models.StatusCode: 0}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AssertionsReplaceComparison(tc.tc); got != tc.want {
				t.Fatalf("AssertionsReplaceComparison() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMatch_Assertions drives Match (not AssertionMatch directly) so it proves
// the wiring — the defect in #4609 was that Match never consulted tc.Assertions.
func TestMatch_Assertions(t *testing.T) {
	tests := []struct {
		name          string
		recorded      *models.GrpcResp
		actual        *models.GrpcResp
		assertions    map[models.AssertionType]interface{}
		expectedMatch bool
	}{
		{
			name:     "issue repro: status_code and json_contains both fail despite identical response",
			recorded: newTestGrpcResp(`{"name": "test", "value": 123}`, "0"),
			actual:   newTestGrpcResp(`{"name": "test", "value": 123}`, "0"),
			assertions: map[models.AssertionType]interface{}{
				models.StatusCode:   500,
				models.JsonContains: map[string]interface{}{"missing_field": "x"},
			},
			expectedMatch: false,
		},
		{
			name:          "status_code matches grpc-status trailer",
			recorded:      newTestGrpcResp(`{"ok": true}`, "5"),
			actual:        newTestGrpcResp(`{"ok": true}`, "5"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCode: 5},
			expectedMatch: true,
		},
		{
			name:          "status_code mismatch fails",
			recorded:      newTestGrpcResp(`{"ok": true}`, "0"),
			actual:        newTestGrpcResp(`{"ok": true}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCode: 5},
			expectedMatch: false,
		},
		{
			name:          "status_code rejects non-integer float",
			recorded:      newTestGrpcResp(`{}`, "1"),
			actual:        newTestGrpcResp(`{}`, "1"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCode: 1.5},
			expectedMatch: false,
		},
		{
			name:          "status_code rejects out-of-range float64 2^63",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCode: float64(1 << 63)},
			expectedMatch: false,
		},
		{
			name:          "non-numeric grpc-status fails status_code 0",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "abc"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCode: 0},
			expectedMatch: false,
		},
		{
			name:          "non-numeric grpc-status cannot match status_code -1",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "abc"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCode: -1},
			expectedMatch: false,
		},
		{
			name:          "status_code_in matches slice",
			recorded:      newTestGrpcResp(`{}`, "7"),
			actual:        newTestGrpcResp(`{}`, "7"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCodeIn: []interface{}{"3", 7, "13"}},
			expectedMatch: true,
		},
		{
			name:          "status_code_in matches scalar",
			recorded:      newTestGrpcResp(`{}`, "7"),
			actual:        newTestGrpcResp(`{}`, "7"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCodeIn: 7},
			expectedMatch: true,
		},
		{
			name:          "status_code_in fails on empty or malformed value",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCodeIn: []interface{}{}},
			expectedMatch: false,
		},
		{
			name:          "status_code_class fails closed on gRPC",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.StatusCodeClass: "2xx"},
			expectedMatch: false,
		},
		{
			name:          "json_contains passes with map[string]interface{}",
			recorded:      newTestGrpcResp(`{"name": "test", "value": 123}`, "0"),
			actual:        newTestGrpcResp(`{"name": "test", "value": 123}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.JsonContains: map[string]interface{}{"name": "test"}},
			expectedMatch: true,
		},
		{
			name:          "json_contains passes with valid JSON string value",
			recorded:      newTestGrpcResp(`{"name": "test", "value": 123}`, "0"),
			actual:        newTestGrpcResp(`{"name": "test", "value": 123}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.JsonContains: `{"name":"test"}`},
			expectedMatch: true,
		},
		{
			name:     "json_contains passes with YAML-decoded map[models.AssertionType]interface{} and nested map[interface{}]interface{}",
			recorded: newTestGrpcResp(`{"user": {"id": 9007199254740993}}`, "0"),
			actual:   newTestGrpcResp(`{"user": {"id": 9007199254740993}}`, "0"),
			assertions: map[models.AssertionType]interface{}{
				models.JsonContains: map[models.AssertionType]interface{}{
					"user": map[interface{}]interface{}{"id": int64(9007199254740993)},
				},
			},
			expectedMatch: true,
		},
		{
			name:     "json_contains distinguishes >2^53 integers (UseNumber precision)",
			recorded: newTestGrpcResp(`{"user": {"id": 9007199254740993}}`, "0"),
			actual:   newTestGrpcResp(`{"user": {"id": 9007199254740992}}`, "0"),
			assertions: map[models.AssertionType]interface{}{
				models.JsonContains: map[models.AssertionType]interface{}{
					"user": map[interface{}]interface{}{"id": int64(9007199254740993)},
				},
			},
			expectedMatch: false,
		},
		{
			name:          "json_contains fails closed on invalid JSON string",
			recorded:      newTestGrpcResp(`{"name": "test"}`, "0"),
			actual:        newTestGrpcResp(`{"name": "test"}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.JsonContains: "{not valid json"},
			expectedMatch: false,
		},
		{
			name:          "json_contains fails closed on null JSON string",
			recorded:      newTestGrpcResp(`{"name": "test"}`, "0"),
			actual:        newTestGrpcResp(`{"name": "test"}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.JsonContains: "null"},
			expectedMatch: false,
		},
		{
			name:          "json_contains fails closed on non-JSON actual body",
			recorded:      newTestGrpcResp(`1: "hello"`, "0"),
			actual:        newTestGrpcResp(`1: "hello"`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.JsonContains: map[string]interface{}{"a": 1}},
			expectedMatch: false,
		},
		{
			name:     "json_contains fails closed when actualResp.NoMessages is true despite stale body field",
			recorded: newTestGrpcResp(`{"name": "test"}`, "0"),
			actual: func() *models.GrpcResp {
				r := newTestGrpcResp(`{"name": "test"}`, "0")
				r.NoMessages = true
				return r
			}(),
			assertions:    map[models.AssertionType]interface{}{models.JsonContains: map[string]interface{}{"name": "test"}},
			expectedMatch: false,
		},
		{
			name:          "json_equal matches semantically equal JSON with different formatting and numeric representations",
			recorded:      newTestGrpcResp(`{"b":2,"a":1,"c":100,"d":0}`, "0"),
			actual:        newTestGrpcResp(`{ "a": 1.0, "b": 2.00, "c": 1e2, "d": -0.0 }`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.JsonEqual: true},
			expectedMatch: true,
		},
		{
			name:          "json_equal distinguishes >2^53 integers (UseNumber precision)",
			recorded:      newTestGrpcResp(`{"id": 9007199254740993}`, "0"),
			actual:        newTestGrpcResp(`{"id": 9007199254740992}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.JsonEqual: true},
			expectedMatch: false,
		},
		{
			name:          "json_equal mismatch fails",
			recorded:      newTestGrpcResp(`{"name": "test"}`, "0"),
			actual:        newTestGrpcResp(`{"name": "other"}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.JsonEqual: map[string]interface{}{"name": "other"}},
			expectedMatch: false,
		},
		{
			name:     "json_equal fails when NoMessages differs",
			recorded: newTestGrpcResp(`{}`, "0"),
			actual: func() *models.GrpcResp {
				r := newTestGrpcResp(`{}`, "0")
				r.NoMessages = true
				return r
			}(),
			assertions:    map[models.AssertionType]interface{}{models.JsonEqual: true},
			expectedMatch: false,
		},
		{
			name:     "json_equal fails closed on streaming response",
			recorded: newTestGrpcResp(`{"a":1}`, "0"),
			actual: func() *models.GrpcResp {
				r := newTestGrpcResp(`{"a":1}`, "0")
				r.SetMessages([]models.GrpcLengthPrefixedMessage{
					{DecodedData: `{"a":1}`},
					{DecodedData: `{"a":2}`},
				})
				return r
			}(),
			assertions:    map[models.AssertionType]interface{}{models.JsonEqual: true},
			expectedMatch: false,
		},
		{
			name:     "header_equal is case-insensitive and lets trailers override headers",
			recorded: newTestGrpcResp(`{}`, "0"),
			actual: func() *models.GrpcResp {
				r := newTestGrpcResp(`{}`, "0")
				r.Headers.OrdinaryHeaders["X-Custom"] = "from-header"
				r.Trailers.OrdinaryHeaders["x-custom"] = "from-trailer"
				return r
			}(),
			assertions: map[models.AssertionType]interface{}{
				models.HeaderEqual: map[string]interface{}{
					"GRPC-STATUS": "0",
					"X-CUSTOM":    "from-trailer",
				},
			},
			expectedMatch: true,
		},
		{
			name:          "header_contains matches case-insensitively",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.HeaderContains: map[string]interface{}{"Content-Type": "grpc"}},
			expectedMatch: true,
		},
		{
			name:          "header_exists matches case-insensitively",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.HeaderExists: []interface{}{"Content-Type", "GRPC-Status"}},
			expectedMatch: true,
		},
		{
			name:          "header_matches evaluates regex case-insensitively",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.HeaderMatches: map[string]interface{}{"Content-Type": `^application/grpc`}},
			expectedMatch: true,
		},
		{
			name:          "header_matches fails closed on invalid regex",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.HeaderMatches: map[string]interface{}{"Content-Type": `[unclosed`}},
			expectedMatch: false,
		},
		{
			name:          "malformed header_equal fails closed",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.HeaderEqual: "not-a-map"},
			expectedMatch: false,
		},
		{
			name:          "malformed header_contains fails closed",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.HeaderContains: "not-a-map"},
			expectedMatch: false,
		},
		{
			name:          "malformed header_matches fails closed",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.HeaderMatches: 123},
			expectedMatch: false,
		},
		{
			name:          "malformed header_exists fails closed",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.HeaderExists: 42},
			expectedMatch: false,
		},
		{
			name:          "unknown assertion type fails closed",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        newTestGrpcResp(`{}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.AssertionType("nonexistent_rule"): true},
			expectedMatch: false,
		},
		{
			name:          "noise-only nil value stays on ordinary comparison and fails on differing body",
			recorded:      newTestGrpcResp(`{"name": "expected"}`, "0"),
			actual:        newTestGrpcResp(`{"name": "actual"}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.NoiseAssertion: nil},
			expectedMatch: false,
		},
		{
			name:          "noise-only empty map stays on ordinary comparison and fails on differing body",
			recorded:      newTestGrpcResp(`{"name": "expected"}`, "0"),
			actual:        newTestGrpcResp(`{"name": "actual"}`, "0"),
			assertions:    map[models.AssertionType]interface{}{models.NoiseAssertion: map[string][]string{}},
			expectedMatch: false,
		},
		{
			name:          "synthetic empty response fails status_code 0",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        &models.GrpcResp{},
			assertions:    map[models.AssertionType]interface{}{models.StatusCode: 0},
			expectedMatch: false,
		},
		{
			name:          "synthetic empty response cannot match status_code -1",
			recorded:      newTestGrpcResp(`{}`, "0"),
			actual:        &models.GrpcResp{},
			assertions:    map[models.AssertionType]interface{}{models.StatusCode: -1},
			expectedMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &models.TestCase{
				Name:       "grpc-assertion-test",
				GrpcResp:   *tt.recorded,
				Assertions: tt.assertions,
			}

			matched, result := Match(tc, tt.actual, map[string]map[string][]string{}, false, zap.NewNop(), false)
			if matched != tt.expectedMatch {
				t.Errorf("Match() = %v, want %v", matched, tt.expectedMatch)
			}
			if result == nil {
				t.Fatal("result should not be nil")
			}
		})
	}
}

func TestMatch_Assertions_ConflictingStatusRulesKeepNormalFalse(t *testing.T) {
	tc := &models.TestCase{
		Name:     "grpc-mixed-status-rules",
		GrpcResp: *newTestGrpcResp(`{}`, "0"),
		Assertions: map[models.AssertionType]interface{}{
			models.StatusCode:   0,
			models.StatusCodeIn: []interface{}{5, 7},
		},
	}
	matched, res := Match(tc, newTestGrpcResp(`{}`, "0"), nil, false, zap.NewNop(), false)
	if matched {
		t.Fatalf("expected Match to fail when status_code_in fails")
	}
	if res.StatusCode.Normal {
		t.Fatalf("expected res.StatusCode.Normal=false when any status assertion fails")
	}
}

func TestMatch_Assertions_ProvenSwitchAbsentStatusFailsClosed(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")

	respWithoutStatus := newTestGrpcResp(`{}`, "")
	for _, wantCode := range []int{0, -1} {
		tc := &models.TestCase{
			Name:       "grpc-proven-absent-status",
			GrpcResp:   *newTestGrpcResp(`{}`, "0"),
			Assertions: map[models.AssertionType]interface{}{models.StatusCode: wantCode},
		}
		matched, res := Match(tc, respWithoutStatus, nil, false, zap.NewNop(), false)
		if matched {
			t.Fatalf("expected Match to fail under KEPLOY_PROVEN=1 when grpc-status is absent (asserted %d)", wantCode)
		}
		if res.StatusCode.Actual != -1 {
			t.Fatalf("res.StatusCode.Actual = %d, want -1", res.StatusCode.Actual)
		}
	}
}

func TestMatch_Assertions_TrailersOnlyRecordedExpectedStatus(t *testing.T) {
	trailersOnlyRecorded := models.GrpcResp{
		Headers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{":status": "200"},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc", "grpc-status": "5"},
		},
	}
	tc := &models.TestCase{
		Name:       "grpc-trailers-only",
		GrpcResp:   trailersOnlyRecorded,
		Assertions: map[models.AssertionType]interface{}{models.StatusCodeIn: []interface{}{5, 7}},
	}
	actual := newTestGrpcResp(`{}`, "5")
	matched, res := Match(tc, actual, nil, false, zap.NewNop(), false)
	if !matched {
		t.Fatalf("expected Match to pass")
	}
	if res.StatusCode.Expected != 5 || res.StatusCode.Actual != 5 || !res.StatusCode.Normal {
		t.Fatalf("unexpected StatusCode result: %+v", res.StatusCode)
	}
}

func TestMatch_Assertions_ConsoleAndWithoutPassLine(t *testing.T) {
	// Passing assertion emits only one pass line (never a fail line from ordinary comparison)
	// even when the body differs from the recording.
	tcPass := &models.TestCase{
		Name:       "tc-assert-pass-%s",
		GrpcResp:   *newTestGrpcResp(`{"recorded": true}`, "0"),
		Assertions: map[models.AssertionType]interface{}{models.StatusCode: 0},
	}
	actualDiffBody := newTestGrpcResp(`{"recorded": false}`, "0")

	var out bytes.Buffer
	matched, _ := Match(tcPass, actualDiffBody, nil, false, zap.NewNop(), true, WithConsole(&out))
	if !matched {
		t.Fatalf("expected assertion match to pass")
	}
	if !strings.Contains(out.String(), "Testrun passed") || strings.Contains(out.String(), "Testrun failed") {
		t.Fatalf("unexpected console output on assertion pass: %q", out.String())
	}

	// WithoutPassLine suppresses the pass line.
	out.Reset()
	matched, _ = Match(tcPass, actualDiffBody, nil, false, zap.NewNop(), true, WithoutPassLine(), WithConsole(&out))
	if !matched {
		t.Fatalf("expected assertion match to pass")
	}
	if out.Len() != 0 {
		t.Fatalf("WithoutPassLine should suppress pass output, got %q", out.String())
	}

	// Failing assertion emits only one fail line (never a pass line from ordinary comparison)
	// even when the response matches the recording byte-for-byte.
	tcFail := &models.TestCase{
		Name:       "tc-assert-fail-%s",
		GrpcResp:   *newTestGrpcResp(`{"ok": true}`, "0"),
		Assertions: map[models.AssertionType]interface{}{models.StatusCode: 5},
	}
	out.Reset()
	matched, _ = Match(tcFail, newTestGrpcResp(`{"ok": true}`, "0"), nil, false, zap.NewNop(), true, WithConsole(&out))
	if matched {
		t.Fatalf("expected assertion match to fail")
	}
	if !strings.Contains(out.String(), "Testrun failed") || strings.Contains(out.String(), "Testrun passed") {
		t.Fatalf("unexpected console output on assertion fail: %q", out.String())
	}

	// emitFailureLogs=false produces zero console output AND zero zap error logs.
	core, logs := observer.New(zapcore.DebugLevel)
	out.Reset()
	matched, _ = Match(tcFail, newTestGrpcResp(`{"ok": true}`, "0"), nil, false, zap.New(core), false, WithConsole(&out))
	if matched {
		t.Fatalf("expected assertion match to fail")
	}
	if out.Len() != 0 {
		t.Fatalf("emitFailureLogs=false should emit no console output, got %q", out.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("emitFailureLogs=false should emit no zap logs, got %d", logs.Len())
	}
}

func TestAssertionMatch_DoesNotLogSensitiveValues(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	const (
		secretHeaderVal   = "SUPER_SECRET_BEARER_TOKEN_12345"
		secretExpectedVal = "EXPECTED_SECRET_TOKEN_98765"
		secretPattern     = "^SECRET_REGEX_PATTERN_XYZ$"
		secretBadRegex    = "[SECRET_UNCLOSED_REGEX_999"
		secretUnreadable  = "SECRET_UNREADABLE_PAYLOAD_777"
		secretBody        = `{"token":"BODY_SECRET_VALUE_ABC"}`
	)

	actual := newTestGrpcResp(secretBody, "0")
	actual.Headers.OrdinaryHeaders["authorization"] = secretHeaderVal
	actual.Headers.OrdinaryHeaders["x-regex-test"] = secretHeaderVal

	tc := &models.TestCase{
		Name:     "secret-log-check",
		GrpcResp: *newTestGrpcResp(`{"token":"DIFFERENT_SECRET_BODY"}`, "0"),
		Assertions: map[models.AssertionType]interface{}{
			models.HeaderEqual:     map[string]interface{}{"authorization": secretExpectedVal},
			models.HeaderContains:  secretUnreadable,
			models.HeaderMatches:   map[string]interface{}{"authorization": secretPattern, "x-regex-test": secretBadRegex},
			models.JsonEqual:       true,
			models.JsonContains:    map[string]interface{}{"token": secretExpectedVal},
			models.StatusCodeClass: secretUnreadable,
		},
	}

	matched, _ := Match(tc, actual, nil, false, logger, true)
	if matched {
		t.Fatalf("expected assertions to fail")
	}
	if logs.Len() == 0 {
		t.Fatalf("expected failure log entries")
	}

	forbidden := []string{
		secretHeaderVal,
		secretExpectedVal,
		secretPattern,
		secretBadRegex,
		secretUnreadable,
		"BODY_SECRET_VALUE_ABC",
		"DIFFERENT_SECRET_BODY",
	}
	for _, entry := range logs.All() {
		parts := []string{entry.Message, fmt.Sprint(entry.ContextMap())}
		for _, f := range entry.Context {
			parts = append(parts, f.Key+"="+f.String+" "+fmt.Sprint(f.Interface))
		}
		rendered := strings.Join(parts, " ")
		for _, secret := range forbidden {
			if strings.Contains(rendered, secret) {
				t.Fatalf("log entry leaked sensitive value %q: %s", secret, rendered)
			}
		}
	}
}
