package grpc

import (
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// patternReasons are the reasons a gRPC comparison kept with its result.
func patternReasons(res *models.Result) []string {
	if res.FailureInfo.Assessment == nil {
		return nil
	}
	return res.FailureInfo.Assessment.Reasons
}

// wantPatternReason fails t unless one of res's reasons names the field, the
// pattern, the side and the shape, and none of them carries the side's text.
func wantPatternReason(t *testing.T, res *models.Result, field, pattern, side, shape, secret string) {
	t.Helper()
	want := "keploy.yml's noise lets " + field + " vary only within \"" + pattern + "\"; the " + side + " value (" + shape + ") is outside it, so it is compared"
	reasons := patternReasons(res)
	found := false
	for _, r := range reasons {
		if r == want {
			found = true
		}
		if secret != "" && strings.Contains(r, secret) {
			t.Errorf("a reason carries the value %q: %q", secret, r)
		}
	}
	if !found {
		t.Errorf("no reason %q among %q", want, reasons)
	}
}

// TestGrpcNoisePatternHoldsBothValues: a gRPC noise entry with patterns (the
// response's decoded data, its message length, its compression flag, a
// header) lets its field vary only within them. Both the recorded and the
// live value must match one; a value outside them is a real change, and the
// comparison fails, saying why. An entry with no patterns still excuses its
// field whole.
func TestGrpcNoisePatternHoldsBothValues(t *testing.T) {
	type row struct {
		name     string
		recorded string // decoded data
		live     string
		mutate   func(exp, act *models.GrpcResp)
		tcNoise  map[string][]string
		global   map[string]map[string][]string
		pass     bool
		// the reason a failing row keeps: field, pattern, side, shape, and
		// the side's text, which it must not carry
		field, pattern, side, shape, secret string
	}
	rows := []row{
		{
			name: "decoded data, live outside", recorded: "1: 7", live: "1: 9",
			tcNoise: map[string][]string{"body.decoded_data": {`^1: 7$`}},
			field:   "body.decoded_data", pattern: `^1: 7$`, side: "live", shape: "a string of 4 bytes", secret: "1: 9",
		},
		{
			name: "decoded data, recorded outside", recorded: "1: 7", live: "1: 9",
			tcNoise: map[string][]string{"body.decoded_data": {`^1: 9$`}},
			field:   "body.decoded_data", pattern: `^1: 9$`, side: "recorded", shape: "a string of 4 bytes", secret: "1: 7",
		},
		{
			name: "decoded data, both inside", recorded: "1: 7", live: "1: 9",
			tcNoise: map[string][]string{"body.decoded_data": {`^1: \d$`}},
			pass:    true,
		},
		{
			// The pattern is held to the decoded data as recorded and as
			// answered, not as the comparison canonicalizes it.
			name: "decoded data, held as recorded", recorded: "2: 5\n1: 7", live: "2: 5\n1: 9",
			tcNoise: map[string][]string{"body.decoded_data": {`^2: 5\n1: \d$`}},
			pass:    true,
		},
		{
			name: "decoded data, keploy.yml", recorded: "1: 7", live: "1: 9",
			global: map[string]map[string][]string{"body": {"decoded_data": {`^1: 7$`}}},
			field:  "body.decoded_data", pattern: `^1: 7$`, side: "live", shape: "a string of 4 bytes", secret: "1: 9",
		},
		{
			name: "decoded data, no pattern", recorded: "1: 7", live: "1: 9",
			global: map[string]map[string][]string{"body": {"decoded_data": {}}},
			pass:   true,
		},
		{
			// A length difference stands only where the decoded data
			// differs too; an entry with no patterns excuses that.
			name: "message length, live outside", recorded: "1: 7", live: "1: 99",
			tcNoise: map[string][]string{"body.decoded_data": {}, "body.message_length": {`^4$`}},
			field:   "body.message_length", pattern: `^4$`, side: "live", shape: "a number of 1 characters", secret: "",
		},
		{
			name: "message length, recorded outside", recorded: "1: 7", live: "1: 99",
			tcNoise: map[string][]string{"body.decoded_data": {}, "body.message_length": {`^5$`}},
			field:   "body.message_length", pattern: `^5$`, side: "recorded", shape: "a number of 1 characters", secret: "",
		},
		{
			name: "message length, both inside", recorded: "1: 7", live: "1: 99",
			tcNoise: map[string][]string{"body.decoded_data": {}, "body.message_length": {`^[45]$`}},
			pass:    true,
		},
		{
			name: "compression flag, live outside", recorded: "1: 7", live: "1: 7",
			mutate:  func(_, act *models.GrpcResp) { act.Body.CompressionFlag = 1 },
			tcNoise: map[string][]string{"body.compression_flag": {`^0$`}},
			field:   "body.compression_flag", pattern: `^0$`, side: "live", shape: "a number of 1 characters", secret: "",
		},
		{
			name: "compression flag, recorded outside", recorded: "1: 7", live: "1: 7",
			mutate:  func(_, act *models.GrpcResp) { act.Body.CompressionFlag = 1 },
			tcNoise: map[string][]string{"body.compression_flag": {`^1$`}},
			field:   "body.compression_flag", pattern: `^1$`, side: "recorded", shape: "a number of 1 characters", secret: "",
		},
		{
			name: "compression flag, both inside", recorded: "1: 7", live: "1: 7",
			mutate:  func(_, act *models.GrpcResp) { act.Body.CompressionFlag = 1 },
			tcNoise: map[string][]string{"body.compression_flag": {`^[01]$`}},
			pass:    true,
		},
		{
			name: "content-type, keploy.yml, live outside", recorded: "1: 7", live: "1: 7",
			mutate: func(_, act *models.GrpcResp) { act.Headers.OrdinaryHeaders["content-type"] = "application/json" },
			global: map[string]map[string][]string{"header": {"content-type": {`^application/grpc`}}},
			field:  "header.content-type", pattern: `^application/grpc`, side: "live", shape: "a string of 16 bytes", secret: "application/json",
		},
		{
			name: "content-type, recorded outside", recorded: "1: 7", live: "1: 7",
			mutate:  func(_, act *models.GrpcResp) { act.Headers.OrdinaryHeaders["content-type"] = "application/json" },
			tcNoise: map[string][]string{"header.content-type": {`^application/json$`}},
			field:   "header.content-type", pattern: `^application/json$`, side: "recorded", shape: "a string of 16 bytes", secret: "",
		},
		{
			name: "content-type, both inside", recorded: "1: 7", live: "1: 7",
			mutate:  func(_, act *models.GrpcResp) { act.Headers.OrdinaryHeaders["content-type"] = "application/json" },
			tcNoise: map[string][]string{"header.content-type": {`^application/(grpc|json)$`}},
			pass:    true,
		},
		{
			// An absent header has no value for a pattern to hold, even one
			// that admits the empty string.
			name: "content-type absent", recorded: "1: 7", live: "1: 7",
			mutate: func(_, act *models.GrpcResp) { delete(act.Headers.OrdinaryHeaders, "content-type") },
			global: map[string]map[string][]string{"header": {"content-type": {`.*`}}},
			field:  "header.content-type", pattern: `.*`, side: "live", shape: "absent", secret: "",
		},
		{
			name: "content-type absent, no pattern", recorded: "1: 7", live: "1: 7",
			mutate: func(_, act *models.GrpcResp) { delete(act.Headers.OrdinaryHeaders, "content-type") },
			global: map[string]map[string][]string{"header": {"content-type": {}}},
			pass:   true,
		},
		{
			name: ":status, live outside", recorded: "1: 7", live: "1: 7",
			mutate:  func(_, act *models.GrpcResp) { act.Headers.PseudoHeaders[":status"] = "503" },
			tcNoise: map[string][]string{"header.:status": {`^200$`}},
			field:   "header.:status", pattern: `^200$`, side: "live", shape: "a string of 3 bytes", secret: "",
		},
		{
			name: ":status absent", recorded: "1: 7", live: "1: 7",
			mutate:  func(_, act *models.GrpcResp) { delete(act.Headers.PseudoHeaders, ":status") },
			tcNoise: map[string][]string{"header.:status": {`.*`}},
			field:   "header.:status", pattern: `.*`, side: "live", shape: "absent", secret: "",
		},
		{
			name: "message count, live outside", recorded: "1: 7", live: "1: 7",
			mutate: func(_, act *models.GrpcResp) {
				act.SetMessages([]models.GrpcLengthPrefixedMessage{act.Body, act.Body})
			},
			tcNoise: map[string][]string{"body.message_count": {`^1$`}},
			field:   "body.message_count", pattern: `^1$`, side: "live", shape: "a number of 1 characters", secret: "",
		},
		{
			name: "message count, both inside", recorded: "1: 7", live: "1: 7",
			mutate: func(_, act *models.GrpcResp) {
				act.SetMessages([]models.GrpcLengthPrefixedMessage{act.Body, act.Body})
			},
			tcNoise: map[string][]string{"body.message_count": {`^[12]$`}},
			pass:    true,
		},
		{
			// A stream's message is named by its index, and the entry
			// holds each message's values.
			name: "stream message, live outside", recorded: "1: 7", live: "1: 7",
			mutate: func(exp, act *models.GrpcResp) {
				exp.SetMessages([]models.GrpcLengthPrefixedMessage{exp.Body, grpcMsg("1: 3")})
				act.SetMessages([]models.GrpcLengthPrefixedMessage{act.Body, grpcMsg("1: 8")})
			},
			tcNoise: map[string][]string{"body.decoded_data": {`^1: [37]$`}},
			field:   "body.1.decoded_data", pattern: `^1: [37]$`, side: "live", shape: "a string of 4 bytes", secret: "1: 8",
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			tc := grpcTestCase(r.recorded, r.tcNoise)
			act := grpcActual(r.live)
			if r.mutate != nil {
				r.mutate(&tc.GrpcResp, act)
			}
			global := r.global
			if global == nil {
				global = map[string]map[string][]string{}
			}
			pass, res := Match(tc, act, global, false, zap.NewNop(), false)
			if pass != r.pass {
				t.Fatalf("Match = %v, want %v (reasons %q)", pass, r.pass, patternReasons(res))
			}
			if r.pass {
				if len(patternReasons(res)) != 0 {
					t.Errorf("a passing comparison keeps reasons %q", patternReasons(res))
				}
				return
			}
			wantPatternReason(t, res, r.field, r.pattern, r.side, r.shape, r.secret)
		})
	}
}

// TestGrpcJSONFieldPatternHoldsBothValues: a JSON field of gRPC decoded data
// a pattern-guarded entry names varies only within the pattern on both
// sides.
func TestGrpcJSONFieldPatternHoldsBothValues(t *testing.T) {
	for _, r := range []struct {
		name, live string
		pass       bool
	}{
		{"live outside", `{"stock":100,"name":"Laptop"}`, false},
		{"both inside", `{"stock":98,"name":"Laptop"}`, true},
	} {
		t.Run(r.name, func(t *testing.T) {
			tc := grpcTestCase(`{"stock":99,"name":"Laptop"}`, map[string][]string{"body.stock": {`^9\d$`}})
			act := grpcActual(r.live)
			act.Body.MessageLength = tc.GrpcResp.Body.MessageLength
			pass, _ := Match(tc, act, map[string]map[string][]string{}, false, zap.NewNop(), false)
			if pass != r.pass {
				t.Fatalf("Match = %v, want %v", pass, r.pass)
			}
		})
	}
}

// TestGrpcPatternReasonJoinsTheBodyAssessment: a header's pattern reason is
// kept beside the decoded data's own assessment, which it does not replace.
func TestGrpcPatternReasonJoinsTheBodyAssessment(t *testing.T) {
	tc := grpcTestCase(`{"stock":99}`, map[string][]string{"header.content-type": {`^application/grpc$`}})
	act := grpcActual(`{"stock":10}`)
	act.Headers.OrdinaryHeaders["content-type"] = "application/json"
	pass, res := Match(tc, act, map[string]map[string][]string{}, false, zap.NewNop(), false)
	if pass {
		t.Fatal("Match passed a changed body and a header outside its pattern")
	}
	if a := res.FailureInfo.Assessment; a == nil || len(a.ValueChanges) == 0 {
		t.Fatalf("the decoded data's assessment is lost: %+v", a)
	}
	wantPatternReason(t, res, "header.content-type", `^application/grpc$`, "live", "a string of 16 bytes", "application/json")
}

// TestGrpcAFieldAPatternKeptIsGraded: a field a pattern kept is a changed
// value, graded at least Medium, so the failure and its reason reach the
// report (replay keeps a failed test's assessment only with a risk): a
// header as a changed header, a body field the comparison did not grade
// itself as a changed value under an unchanged schema. A body the
// comparison graded keeps its own grade.
func TestGrpcAFieldAPatternKeptIsGraded(t *testing.T) {
	for _, r := range []struct {
		name     string
		live     string
		mutate   func(act *models.GrpcResp)
		noise    map[string][]string
		risk     models.RiskLevel
		category []models.FailureCategory
	}{
		{"a compression flag", "1: 7", func(act *models.GrpcResp) { act.Body.CompressionFlag = 1 },
			map[string][]string{"body.compression_flag": {`^0$`}}, models.Medium, []models.FailureCategory{models.SchemaUnchanged}},
		{"a :status header", "1: 7", func(act *models.GrpcResp) { act.Headers.PseudoHeaders[":status"] = "503" },
			map[string][]string{"header.:status": {`^200$`}}, models.Medium, []models.FailureCategory{models.HeaderChanged}},
		{"a :status header and a compression flag", "1: 7", func(act *models.GrpcResp) {
			act.Headers.PseudoHeaders[":status"] = "503"
			act.Body.CompressionFlag = 1
		}, map[string][]string{"header.:status": {`^200$`}, "body.compression_flag": {`^0$`}}, models.Medium,
			[]models.FailureCategory{models.SchemaUnchanged, models.HeaderChanged}},
		{"decoded data", "1: 9", nil,
			map[string][]string{"body.decoded_data": {`^1: 7$`}}, models.High, []models.FailureCategory{models.SchemaBroken}},
	} {
		t.Run(r.name, func(t *testing.T) {
			tc := grpcTestCase("1: 7", r.noise)
			act := grpcActual(r.live)
			if r.mutate != nil {
				r.mutate(act)
			}
			pass, res := Match(tc, act, map[string]map[string][]string{}, false, zap.NewNop(), false)
			if pass {
				t.Fatal("Match passed a field outside its pattern")
			}
			fi := res.FailureInfo
			if fi.Risk != r.risk || !slices.Equal(fi.Category, r.category) {
				t.Fatalf("graded %s %v, want %s %v", fi.Risk, fi.Category, r.risk, r.category)
			}
			if fi.Assessment == nil || len(fi.Assessment.Reasons) == 0 || fi.Assessment.Risk != r.risk {
				t.Fatalf("assessment %+v, want the reason with the risk %s", fi.Assessment, r.risk)
			}
		})
	}
}

func grpcMsg(decoded string) models.GrpcLengthPrefixedMessage {
	return models.GrpcLengthPrefixedMessage{MessageLength: uint32(len(decoded)), DecodedData: decoded}
}
