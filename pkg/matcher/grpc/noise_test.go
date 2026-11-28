// Package grpc noise tests.
package grpc

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func grpcTestCase(decoded string, noise map[string][]string) *models.TestCase {
	return &models.TestCase{
		Name: "grpc-noise",
		GrpcResp: models.GrpcResp{
			Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":status": "200"},
				OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
			},
			Body: models.GrpcLengthPrefixedMessage{
				CompressionFlag: 0,
				MessageLength:   uint32(len(decoded)),
				DecodedData:     decoded,
			},
			Trailers: models.GrpcHeaders{
				OrdinaryHeaders: map[string]string{"grpc-status": "0"},
			},
		},
		Noise: noise,
	}
}

func grpcActual(decoded string) *models.GrpcResp {
	return &models.GrpcResp{
		Headers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{":status": "200"},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
		},
		Body: models.GrpcLengthPrefixedMessage{
			CompressionFlag: 0,
			MessageLength:   uint32(len(decoded)),
			DecodedData:     decoded,
		},
		Trailers: models.GrpcHeaders{
			OrdinaryHeaders: map[string]string{"grpc-status": "0"},
		},
	}
}

// TestMatch_SectionedBodyNoise covers the gRPC matcher's half of the sectioned
// noise contract. It had no noise coverage at all before.
func TestMatch_SectionedBodyNoise(t *testing.T) {
	const recorded = `{"stock":99,"name":"Laptop"}`

	t.Run("listed path is ignored", func(t *testing.T) {
		tc := grpcTestCase(recorded, map[string][]string{"body": {"stock"}})
		actual := grpcActual(`{"stock":100,"name":"Laptop"}`)
		// MessageLength is derived from the payload and differs by design here;
		// only the decoded-body verdict is under test.
		_, res := Match(tc, actual, map[string]map[string][]string{}, false, zap.NewNop(), false)
		if !decodedDataNormal(res) {
			t.Errorf("expected the decoded body to match: only the listed noisy path differs")
		}
	})

	t.Run("unlisted field is still compared", func(t *testing.T) {
		tc := grpcTestCase(recorded, map[string][]string{"body": {"stock"}})
		actual := grpcActual(`{"stock":100,"name":"Tablet"}`)
		_, res := Match(tc, actual, map[string]map[string][]string{}, false, zap.NewNop(), false)
		if decodedDataNormal(res) {
			t.Errorf("expected a decoded-body mismatch: name differs and is not noise")
		}
	})
}

// TestMatch_DoesNotMutateSharedNoiseConfig is the regression test for the worst
// instance of the shared-map leak: replay passes its long-lived global noise
// config straight into the gRPC matcher without copying, so a test case's own
// noise used to persist for the remainder of the run.
func TestMatch_DoesNotMutateSharedNoiseConfig(t *testing.T) {
	shared := map[string]map[string][]string{"body": {}, "header": {}}

	tc := grpcTestCase(`{"total":1}`, map[string][]string{
		"body":       {"total"},
		"body.stock": {},
	})
	Match(tc, grpcActual(`{"total":2}`), shared, false, zap.NewNop(), false)

	if len(shared["body"]) != 0 {
		t.Errorf("shared global noise config was mutated: %v", shared["body"])
	}

	// A later test case declaring no noise must still catch the difference.
	clean := grpcTestCase(`{"total":1}`, nil)
	_, res := Match(clean, grpcActual(`{"total":2}`), shared, false, zap.NewNop(), false)
	if decodedDataNormal(res) {
		t.Errorf("noise from an earlier test case leaked into a later one")
	}
}

// decodedDataNormal returns the verdict for the gRPC decoded-body result.
func decodedDataNormal(res *models.Result) bool {
	for _, b := range res.BodyResult {
		if b.Type == models.GrpcData {
			return b.Normal
		}
	}
	return false
}

// TestMatch_NoiseExcusedBodyRowsReadAsMatched: a body difference noise
// excuses leaves its rows matched, as an excused HTTP body's, so a reader of
// the rows (the report, the decoded view) never takes it for what failed --
// here the status.
func TestMatch_NoiseExcusedBodyRowsReadAsMatched(t *testing.T) {
	tc := grpcTestCase(`1: {"o-1"}`, nil)
	act := grpcActual(`1: {"o-1"}` + "\n" + `3: {""}`)
	act.Trailers.OrdinaryHeaders = map[string]string{"grpc-status": "5"}
	noiseCfg := map[string]map[string][]string{"body": {"decoded_data": {}, "message_length": {}}}
	pass, res := Match(tc, act, noiseCfg, false, zap.NewNop(), false)
	if pass {
		t.Fatal("the status changed; the test passed")
	}
	for _, b := range res.BodyResult {
		if (b.Type == models.GrpcData || b.Type == models.GrpcLength) && !b.Normal {
			t.Fatalf("an excused body row reads as a difference: %+v", res.BodyResult)
		}
	}
}

// TestMatch_NoiseExcusedBodyIsNoPartOfTheFailure: a test failing on its
// status, its body noised whole, is graded on the status alone: the body
// noise excused is never called a broken schema.
func TestMatch_NoiseExcusedBodyIsNoPartOfTheFailure(t *testing.T) {
	tc := grpcTestCase(`1: {"o-1"}`, nil)
	act := grpcActual(`1: {"o-2"}`)
	act.Trailers.OrdinaryHeaders = map[string]string{"grpc-status": "5"}
	noiseCfg := map[string]map[string][]string{"body": {"decoded_data": {}}}
	pass, res := Match(tc, act, noiseCfg, false, zap.NewNop(), false)
	if pass {
		t.Fatal("the status changed; the test passed")
	}
	if hasCategory(res, models.SchemaBroken) || res.FailureInfo.Assessment != nil {
		t.Fatalf("the excused body was graded: %+v", res.FailureInfo)
	}
}

// TestMatch_NoiseExcusedMessageCountReadsAsMatched: a message count noise
// excuses leaves its row matched and is no part of the failure.
func TestMatch_NoiseExcusedMessageCountReadsAsMatched(t *testing.T) {
	var exp models.GrpcResp
	exp.Headers, exp.Trailers = emptyHdrs(), okTrailers()
	exp.SetMessages([]models.GrpcLengthPrefixedMessage{msg("alpha"), msg("beta")})
	var act models.GrpcResp
	act.Headers = emptyHdrs()
	act.Trailers = models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "5"}}
	act.SetMessages([]models.GrpcLengthPrefixedMessage{msg("alpha")})
	tc := &models.TestCase{Name: "count-noise", GrpcResp: exp}
	noiseCfg := map[string]map[string][]string{"body": {"message_count": {}}}
	pass, res := Match(tc, &act, noiseCfg, false, zap.NewNop(), false)
	if pass {
		t.Fatal("the status changed; the test passed")
	}
	for _, b := range res.BodyResult {
		if !b.Normal {
			t.Fatalf("an excused body row reads as a difference: %+v", res.BodyResult)
		}
	}
	if hasCategory(res, models.SchemaBroken) {
		t.Fatalf("the excused count was graded: %+v", res.FailureInfo)
	}
}

// TestMatch_AStreamsLaterExcusedMessageReadsAsMatched: noise excuses every
// message of a stream, each in its own row.
func TestMatch_AStreamsLaterExcusedMessageReadsAsMatched(t *testing.T) {
	var exp models.GrpcResp
	exp.Headers, exp.Trailers = emptyHdrs(), okTrailers()
	exp.SetMessages([]models.GrpcLengthPrefixedMessage{msg("alpha"), msg("beta")})
	var act models.GrpcResp
	act.Headers = emptyHdrs()
	act.Trailers = models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "5"}}
	act.SetMessages([]models.GrpcLengthPrefixedMessage{msg("alpha"), msg("brav")})
	tc := &models.TestCase{Name: "stream-excused", GrpcResp: exp}
	noiseCfg := map[string]map[string][]string{"body": {"decoded_data": {}}}
	_, res := Match(tc, &act, noiseCfg, false, zap.NewNop(), false)
	for _, b := range res.BodyResult {
		if b.Type == models.GrpcData && !b.Normal {
			t.Fatalf("message 1, excused, reads as a difference: %+v", res.BodyResult)
		}
	}
}

// TestMatch_APatternThatKeepsADifferenceLeavesItsRowUnmatched: noise with
// a pattern neither value matches excuses nothing: the row stays a difference.
func TestMatch_APatternThatKeepsADifferenceLeavesItsRowUnmatched(t *testing.T) {
	tc := grpcTestCase(`1: {"o-1"}`, nil)
	act := grpcActual(`1: {"o-2"}`)
	noiseCfg := map[string]map[string][]string{"body": {"decoded_data": {"^never$"}}}
	pass, res := Match(tc, act, noiseCfg, false, zap.NewNop(), false)
	if pass {
		t.Fatal("the pattern matches neither value; the test passed")
	}
	for _, b := range res.BodyResult {
		if b.Type == models.GrpcData && b.Normal {
			t.Fatalf("a kept difference reads as matched: %+v", res.BodyResult)
		}
	}
}

// TestMatch_NoisedDataExcusesItsLength: a body noised whole passes when its
// length changed with it: the length follows the data. Before, the noise
// entry left the test failing on the length, graded as nothing.
func TestMatch_NoisedDataExcusesItsLength(t *testing.T) {
	tc := grpcTestCase(`1: {"o-1"}`, nil)
	act := grpcActual(`1: {"o-1"}` + "\n" + `3: {""}`)
	noiseCfg := map[string]map[string][]string{"body": {"decoded_data": {}}}
	pass, res := Match(tc, act, noiseCfg, false, zap.NewNop(), false)
	if !pass {
		t.Fatalf("the body is noise; the test failed on its length: %+v", res.BodyResult)
	}
	for _, b := range res.BodyResult {
		if !b.Normal {
			t.Fatalf("an excused body row reads as a difference: %+v", res.BodyResult)
		}
	}
}

// TestMatch_AStreamIsGradedOnTheMessageThatFailed: the failure is graded on
// the first message whose difference noise left standing, never on one it
// excused.
func TestMatch_AStreamIsGradedOnTheMessageThatFailed(t *testing.T) {
	var exp models.GrpcResp
	exp.Headers, exp.Trailers = emptyHdrs(), okTrailers()
	exp.SetMessages([]models.GrpcLengthPrefixedMessage{msg(`{"a":"alp1"}`), msg(`{"b":1}`)})
	var act models.GrpcResp
	act.Headers, act.Trailers = emptyHdrs(), okTrailers()
	act.SetMessages([]models.GrpcLengthPrefixedMessage{msg(`{"a":"alp2"}`), msg(`{"b":"x"}`)})
	tc := &models.TestCase{Name: "stream-graded", GrpcResp: exp}
	// Message 0's values both hold "alp": excused. Message 1's do not: kept.
	noiseCfg := map[string]map[string][]string{"body": {"decoded_data": {"alp"}}}
	pass, res := Match(tc, &act, noiseCfg, false, zap.NewNop(), false)
	if pass {
		t.Fatal("message 1 changed type; the test passed")
	}
	if !hasCategory(res, models.SchemaBroken) {
		t.Fatalf("graded on the excused message 0, not message 1's type change: %+v", res.FailureInfo)
	}
}

// TestMatch_ACompressionFlagThatDiffersIsGraded: a test failing on its
// compression flag alone, or with its data noised, is graded, never a
// failure with no risk (replay keeps no reason for one).
func TestMatch_ACompressionFlagThatDiffersIsGraded(t *testing.T) {
	for _, c := range []struct {
		name  string
		live  string
		noise map[string]map[string][]string
	}{
		{name: "compression flag alone", live: `1: {"o-1"}`},
		{name: "data noised", live: `1: {"o-2"}`, noise: map[string]map[string][]string{"body": {"decoded_data": {}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := grpcTestCase(`1: {"o-1"}`, nil)
			act := grpcActual(c.live)
			act.Body.CompressionFlag = 1
			pass, res := Match(tc, act, c.noise, false, zap.NewNop(), false)
			if pass {
				t.Fatal("the compression flag changed; the test passed")
			}
			if res.FailureInfo.Risk == models.None || !hasCategory(res, models.SchemaUnchanged) {
				t.Fatalf("not graded: %+v", res.FailureInfo)
			}
		})
	}
}

// TestMatch_ACompressionFlagNeverContradictsTheBodysGrade: a stream short of
// a message, its flag changed too, is a broken schema, never also an
// unchanged one.
func TestMatch_ACompressionFlagNeverContradictsTheBodysGrade(t *testing.T) {
	var exp models.GrpcResp
	exp.Headers, exp.Trailers = emptyHdrs(), okTrailers()
	exp.SetMessages([]models.GrpcLengthPrefixedMessage{msg("alpha"), msg("beta")})
	flagged := msg("alpha")
	flagged.CompressionFlag = 1
	var act models.GrpcResp
	act.Headers, act.Trailers = emptyHdrs(), okTrailers()
	act.SetMessages([]models.GrpcLengthPrefixedMessage{flagged})
	tc := &models.TestCase{Name: "count-and-flag", GrpcResp: exp}
	pass, res := Match(tc, &act, nil, false, zap.NewNop(), false)
	if pass {
		t.Fatal("a message is missing; the test passed")
	}
	if !hasCategory(res, models.SchemaBroken) || hasCategory(res, models.SchemaUnchanged) {
		t.Fatalf("graded %+v, want a broken schema alone", res.FailureInfo.Category)
	}
}

// TestMatch_AStatusHeaderThatDiffersIsGraded: a proxy's :status, alone,
// is a changed header, never a failure with no grade.
func TestMatch_AStatusHeaderThatDiffersIsGraded(t *testing.T) {
	tc := grpcTestCase(`1: {"o-1"}`, nil)
	act := grpcActual(`1: {"o-1"}`)
	act.Headers.PseudoHeaders[":status"] = "503"
	pass, res := Match(tc, act, nil, false, zap.NewNop(), false)
	if pass {
		t.Fatal(":status changed; the test passed")
	}
	if res.FailureInfo.Risk == models.None || !hasCategory(res, models.HeaderChanged) {
		t.Fatalf("not graded: %+v", res.FailureInfo)
	}
}

// TestMatch_AnExcusedContentTypeIsNoPartOfTheFailure: a content-type noise
// excuses reads as matched and is not graded, whether the test then passes or
// fails on its status; one noise leaves standing is a changed header.
func TestMatch_AnExcusedContentTypeIsNoPartOfTheFailure(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   string
		noise    map[string]map[string][]string
		wantPass bool
		wantHdr  bool
	}{
		{name: "excused, passes", status: "0", noise: map[string]map[string][]string{"header": {"content-type": {}}}, wantPass: true},
		{name: "excused, fails on status", status: "5", noise: map[string]map[string][]string{"header": {"content-type": {}}}},
		{name: "standing", status: "0", wantHdr: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := grpcTestCase(`1: {"o-1"}`, nil)
			act := grpcActual(`1: {"o-1"}`)
			act.Headers.OrdinaryHeaders["content-type"] = "application/json"
			act.Trailers.OrdinaryHeaders = map[string]string{"grpc-status": c.status}
			pass, res := Match(tc, act, c.noise, false, zap.NewNop(), false)
			if pass != c.wantPass {
				t.Fatalf("pass = %v, want %v", pass, c.wantPass)
			}
			if got := hasCategory(res, models.HeaderChanged); got != c.wantHdr {
				t.Fatalf("header changed graded = %v, want %v: %+v", got, c.wantHdr, res.FailureInfo)
			}
			for _, h := range res.HeadersResult {
				if h.Expected.Key == "content-type" && h.Normal == c.wantHdr {
					t.Fatalf("content-type row Normal = %v, want %v", h.Normal, !c.wantHdr)
				}
			}
		})
	}
}

// TestMatch_AStreamShortOfAMessageIsABrokenSchemaAlone: a stream short of a
// message whose first message only changed a value is a broken schema, never
// also an unchanged one.
func TestMatch_AStreamShortOfAMessageIsABrokenSchemaAlone(t *testing.T) {
	var exp models.GrpcResp
	exp.Headers, exp.Trailers = emptyHdrs(), okTrailers()
	exp.SetMessages([]models.GrpcLengthPrefixedMessage{msg(`{"a":1}`), msg(`{"b":2}`)})
	var act models.GrpcResp
	act.Headers, act.Trailers = emptyHdrs(), okTrailers()
	act.SetMessages([]models.GrpcLengthPrefixedMessage{msg(`{"a":3}`)})
	tc := &models.TestCase{Name: "short-stream", GrpcResp: exp}
	_, res := Match(tc, &act, nil, false, zap.NewNop(), false)
	if !hasCategory(res, models.SchemaBroken) || hasCategory(res, models.SchemaUnchanged) || hasCategory(res, models.SchemaAdded) {
		t.Fatalf("graded %+v, want a broken schema alone", res.FailureInfo.Category)
	}
}

// TestMatch_AnExcusedStatusHeaderReadsAsMatched: a :status noise excuses
// passes, its row matched; a content-type a pattern keeps is a changed
// header, graded High.
func TestMatch_AnExcusedStatusHeaderReadsAsMatched(t *testing.T) {
	tc := grpcTestCase(`1: {"o-1"}`, nil)
	act := grpcActual(`1: {"o-1"}`)
	act.Headers.PseudoHeaders[":status"] = "503"
	pass, res := Match(tc, act, map[string]map[string][]string{"header": {":status": {}}}, false, zap.NewNop(), false)
	if !pass {
		t.Fatalf(":status is noise; the test failed: %+v", res.FailureInfo)
	}
	for _, h := range res.HeadersResult {
		if !h.Normal {
			t.Fatalf("an excused header row reads as a difference: %+v", res.HeadersResult)
		}
	}

	kept := grpcTestCase(`1: {"o-1"}`, nil)
	ct := grpcActual(`1: {"o-1"}`)
	ct.Headers.OrdinaryHeaders["content-type"] = "application/json"
	pass, res = Match(kept, ct, map[string]map[string][]string{"header": {"content-type": {"^never$"}}}, false, zap.NewNop(), false)
	if pass || res.FailureInfo.Risk != models.High || !hasCategory(res, models.HeaderChanged) {
		t.Fatalf("a content-type a pattern kept: pass=%v %+v", pass, res.FailureInfo)
	}
}
