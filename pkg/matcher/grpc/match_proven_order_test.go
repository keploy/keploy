package grpc

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch a gRPC response array is compared in order even when
// the caller passes ignoreOrdering=true (test.ignoreOrdering defaults to true):
// a reorder is a difference, never forgiven as a multiset.
func TestMatch_ProvenSwitchComparesArraysInOrder(t *testing.T) {
	headers := models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{}}
	trailers := models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}}
	tc := &models.TestCase{Name: "reordered", GrpcResp: models.GrpcResp{
		Body:    models.GrpcLengthPrefixedMessage{DecodedData: `{"items": [{"id": 1}, {"id": 2}]}`, MessageLength: 30},
		Headers: headers, Trailers: trailers,
	}}
	actual := &models.GrpcResp{
		Body:    models.GrpcLengthPrefixedMessage{DecodedData: `{"items": [{"id": 2}, {"id": 1}]}`, MessageLength: 30},
		Headers: headers, Trailers: trailers,
	}

	t.Setenv(provenswitch.Env, "0")
	if ok, _ := Match(tc, actual, map[string]map[string][]string{}, true, zap.NewNop(), false); !ok {
		t.Fatal("control: with the switch off and ignoreOrdering=true a reordered array must still match, as today")
	}
	t.Setenv(provenswitch.Env, "1")
	if ok, _ := Match(tc, actual, map[string]map[string][]string{}, true, zap.NewNop(), false); ok {
		t.Fatal("KEPLOY_PROVEN=1: a reordered response array matched; arrays are compared in order")
	}
}

// protoscopeCase is a gRPC test case whose recorded body is protoscope text,
// the form a response is compared in when no proto files are configured (the
// default): it is canonicalized, not compared as JSON.
func protoscopeCase(body string) (*models.TestCase, func(string) *models.GrpcResp) {
	headers := models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{}}
	trailers := models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}}
	tc := &models.TestCase{Name: "protoscope", GrpcResp: models.GrpcResp{
		Body:    models.GrpcLengthPrefixedMessage{DecodedData: body, MessageLength: 40},
		Headers: headers, Trailers: trailers,
	}}
	return tc, func(b string) *models.GrpcResp {
		return &models.GrpcResp{Body: models.GrpcLengthPrefixedMessage{DecodedData: b, MessageLength: 40}, Headers: headers, Trailers: trailers}
	}
}

// Under the proven switch a repeated field's elements in protoscope text are
// compared in order, at the top level and inside a nested message: a reorder
// is a difference, said as one. Different fields compare whatever order the
// wire put them in, under the switch and without it. With the switch off a
// reorder of a repeated field still matches, as today.
func TestMatch_ProvenSwitchComparesProtoscopeRepeatedFieldsInOrder(t *testing.T) {
	const recorded = "1: {\"ord_A\"}\n1: {\"ord_B\"}\n2: 7\n3: {\n  4: {\"x\"}\n  4: {\"y\"}\n}"
	for _, c := range []struct {
		name, live string
		off, on    bool
	}{
		{"as recorded", recorded, true, true},
		{"fields in another order", "2: 7\n3: {\n  4: {\"x\"}\n  4: {\"y\"}\n}\n1: {\"ord_A\"}\n1: {\"ord_B\"}", true, true},
		{"a repeated field reordered", "1: {\"ord_B\"}\n1: {\"ord_A\"}\n2: 7\n3: {\n  4: {\"x\"}\n  4: {\"y\"}\n}", true, false},
		{"a nested repeated field reordered", "1: {\"ord_A\"}\n1: {\"ord_B\"}\n2: 7\n3: {\n  4: {\"y\"}\n  4: {\"x\"}\n}", true, false},
		{"a value changed", "1: {\"ord_A\"}\n1: {\"ord_C\"}\n2: 7\n3: {\n  4: {\"x\"}\n  4: {\"y\"}\n}", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc, answer := protoscopeCase(recorded)
			t.Setenv(provenswitch.Env, "0")
			if ok, _ := Match(tc, answer(c.live), map[string]map[string][]string{}, true, zap.NewNop(), false); ok != c.off {
				t.Fatalf("switch off: matched %v, want %v", ok, c.off)
			}
			t.Setenv(provenswitch.Env, "1")
			ok, _ := Match(tc, answer(c.live), map[string]map[string][]string{}, true, zap.NewNop(), false)
			if ok != c.on {
				t.Fatalf("KEPLOY_PROVEN=1: matched %v, want %v", ok, c.on)
			}
		})
	}
}

// Under the switch the canonical form keeps a repeated field's order and
// orders fields by number; without it every sibling is sorted, as today.
func TestCanonicalizeKeepingRepeatedOrder(t *testing.T) {
	in := "2: 7\n1: {\"b\"}\n1: {\"a\"}"
	if got, want := CanonicalizeKeepingRepeatedOrder(in), "1: {\"b\"}\n1: {\"a\"}\n2: 7"; got != want {
		t.Fatalf("in order: %q, want %q", got, want)
	}
	if got := CanonicalizeTopLevelBlocks(in); !strings.HasPrefix(got, "1: {\"a\"}") {
		t.Fatalf("today's form must still sort every sibling: %q", got)
	}
}

// A protoscope body that differs only in a repeated field's order fails under
// the switch with that said where the user reads a failure: the result's
// assessment (persisted in the report, rendered in JUnit) carries the reason
// and the proto-files way out for a map, graded as a reordered JSON array is.
// A value change is graded as before.
func TestMatch_ProvenSwitchSaysAProtoscopeReorderIsOne(t *testing.T) {
	const recorded = "1: {\"ord_A\"}\n1: {\"ord_B\"}"
	tc, answer := protoscopeCase(recorded)
	t.Setenv(provenswitch.Env, "1")
	_, res := Match(tc, answer("1: {\"ord_B\"}\n1: {\"ord_A\"}"), map[string]map[string][]string{}, true, zap.NewNop(), false)
	a := res.FailureInfo.Assessment
	if a == nil || len(a.Reasons) != 1 || !strings.Contains(a.Reasons[0], "--proto-file") || res.FailureInfo.Risk != models.Medium || len(res.FailureInfo.Category) != 1 || res.FailureInfo.Category[0] != models.SchemaUnchanged {
		t.Fatalf("assessment %+v, risk %s, categories %v; want the reorder reason with the proto-files hint, MEDIUM, SCHEMA_UNCHANGED", a, res.FailureInfo.Risk, res.FailureInfo.Category)
	}
	_, res = Match(tc, answer("1: {\"ord_A\"}\n1: {\"ord_C\"}"), map[string]map[string][]string{}, true, zap.NewNop(), false)
	if res.FailureInfo.Assessment != nil || res.FailureInfo.Risk != models.High || len(res.FailureInfo.Category) != 1 || res.FailureInfo.Category[0] != models.SchemaBroken {
		t.Fatalf("a value change: assessment %+v, risk %s, categories %v; want HIGH SCHEMA_BROKEN as before", res.FailureInfo.Assessment, res.FailureInfo.Risk, res.FailureInfo.Category)
	}
}

// A stream is graded a reorder only if every message that differs is one: a
// first message only reordered and a second with a changed value is a real
// change (HIGH, SCHEMA_BROKEN), with no reorder reason.
func TestMatch_ProvenSwitchGradesAStreamByEveryMessage(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	msgs := func(bodies ...string) []models.GrpcLengthPrefixedMessage {
		out := make([]models.GrpcLengthPrefixedMessage, 0, len(bodies))
		for _, b := range bodies {
			out = append(out, models.GrpcLengthPrefixedMessage{DecodedData: b, MessageLength: 40})
		}
		return out
	}
	tc, answer := protoscopeCase("")
	tc.GrpcResp.SetMessages(msgs("1: {\"ord_A\"}\n1: {\"ord_B\"}", "2: 100"))
	live := answer("")
	live.SetMessages(msgs("1: {\"ord_B\"}\n1: {\"ord_A\"}", "2: 999"))
	ok, res := Match(tc, live, map[string]map[string][]string{}, true, zap.NewNop(), false)
	if ok || res.FailureInfo.Risk != models.High || res.FailureInfo.Assessment != nil {
		t.Fatalf("matched %v, risk %s, assessment %+v; want a real change: HIGH, no reorder reason", ok, res.FailureInfo.Risk, res.FailureInfo.Assessment)
	}
	live.SetMessages(msgs("1: {\"ord_B\"}\n1: {\"ord_A\"}", "2: 100"))
	if _, res := Match(tc, live, map[string]map[string][]string{}, true, zap.NewNop(), false); res.FailureInfo.Risk != models.Medium || res.FailureInfo.Assessment == nil {
		t.Fatalf("a stream only reordered: risk %s, assessment %+v; want MEDIUM with the reorder reason", res.FailureInfo.Risk, res.FailureInfo.Assessment)
	}
}
