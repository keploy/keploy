package grpc

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func msg(data string) models.GrpcLengthPrefixedMessage {
	return models.GrpcLengthPrefixedMessage{MessageLength: uint32(len(data)), DecodedData: data}
}

func emptyHdrs() models.GrpcHeaders {
	return models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}}
}

func okTrailers() models.GrpcHeaders {
	return models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{"grpc-status": "0"}}
}

func streamCase(t *testing.T, recorded, actual []models.GrpcLengthPrefixedMessage) (bool, *models.Result) {
	t.Helper()
	var exp models.GrpcResp
	exp.Headers, exp.Trailers = emptyHdrs(), okTrailers()
	exp.SetMessages(recorded)

	var act models.GrpcResp
	act.Headers, act.Trailers = emptyHdrs(), okTrailers()
	act.SetMessages(actual)

	tc := &models.TestCase{Name: "stream", GrpcResp: exp}
	return Match(tc, &act, nil, false, zap.NewNop(), false)
}

// TestMatch_ShortStreamMustFail: a server that returns fewer messages than
// were recorded must NOT pass.
//
// Comparing only min(len(expected), len(actual)) and calling that a match is
// strictly worse than not supporting streams at all — every message compared
// would be identical, the test would go green, and the user would believe
// streams are covered while the RPC silently truncated.
func TestMatch_ShortStreamMustFail(t *testing.T) {
	recorded := []models.GrpcLengthPrefixedMessage{msg("one"), msg("two"), msg("three"), msg("four"), msg("five")}
	actual := recorded[:3] // 3 of 5, each byte-identical

	pass, result := streamCase(t, recorded, actual)
	if pass {
		t.Fatal("a response returning 3 of 5 recorded messages PASSED. Every compared message " +
			"was identical, so only an explicit count verdict can catch this.")
	}
	found := false
	for _, br := range result.BodyResult {
		if !br.Normal && strings.Contains(br.Expected, "message(s)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no message-count entry in BodyResult; the user cannot see WHY it failed: %+v", result.BodyResult)
	}
}

// TestMatch_ReorderedStreamMustFail: order is the semantic content of a
// stream. Concatenating messages before comparing, or canonicalizing across
// message boundaries (CanonicalizeTopLevelBlocks sorts sibling blocks), would
// both let this pass.
func TestMatch_ReorderedStreamMustFail(t *testing.T) {
	recorded := []models.GrpcLengthPrefixedMessage{msg("alpha"), msg("beta"), msg("gamma")}
	reversed := []models.GrpcLengthPrefixedMessage{msg("gamma"), msg("beta"), msg("alpha")}

	if pass, _ := streamCase(t, recorded, reversed); pass {
		t.Fatal("a stream replayed in REVERSE order passed. Comparison must be positional; " +
			"the same messages in a different order is a different stream.")
	}
}

// TestMatch_IdenticalStreamPasses is the positive control — without it the
// two tests above could pass because everything fails.
func TestMatch_IdenticalStreamPasses(t *testing.T) {
	msgs := []models.GrpcLengthPrefixedMessage{msg("alpha"), msg("beta"), msg("gamma")}
	same := []models.GrpcLengthPrefixedMessage{msg("alpha"), msg("beta"), msg("gamma")}

	pass, result := streamCase(t, msgs, same)
	if !pass {
		t.Fatalf("an identical 3-message stream FAILED: %+v", result.BodyResult)
	}
}

// TestMatch_MiddleMessageMismatchIsReportedByPosition: the user has to be
// told WHICH message differs, not just that the body did.
func TestMatch_MiddleMessageMismatchIsReportedByPosition(t *testing.T) {
	recorded := []models.GrpcLengthPrefixedMessage{msg("alpha"), msg("beta"), msg("gamma")}
	actual := []models.GrpcLengthPrefixedMessage{msg("alpha"), msg("CHANGED"), msg("gamma")}

	pass, result := streamCase(t, recorded, actual)
	if pass {
		t.Fatal("a stream whose middle message differs passed")
	}
	// message 0 and 2 must still be reported normal; only 1 abnormal.
	var abnormalData int
	for _, br := range result.BodyResult {
		if br.Type == models.GrpcData && !br.Normal {
			abnormalData++
			if br.Expected != "beta" {
				t.Fatalf("the reported mismatch is %q, want the middle message %q", br.Expected, "beta")
			}
		}
	}
	if abnormalData != 1 {
		t.Fatalf("%d decoded-data mismatches reported, want exactly 1 (only message 1 differs)", abnormalData)
	}
}

// TestMatch_UnaryDifferenceKeysAreUnchanged pins the compatibility promise
// for every recording that exists today.
//
// A user's assertions.noise holds the bare string `body.decoded_data`. If a
// unary comparison started emitting `body.0.decoded_data`, every recorded
// noise entry would go inert and previously-suppressed flakiness would
// reappear as failures — with no diagnostic, because this matcher never calls
// WarnUnmatchableBodyNoise.
func TestMatch_UnaryDifferenceKeysAreUnchanged(t *testing.T) {
	var exp models.GrpcResp
	exp.Headers, exp.Trailers = emptyHdrs(), okTrailers()
	exp.Body = msg("alpha")

	var act models.GrpcResp
	act.Headers, act.Trailers = emptyHdrs(), okTrailers()
	act.Body = msg("brave") // same length as "alpha": only decoded_data differs

	tc := &models.TestCase{Name: "unary", GrpcResp: exp}

	// With the bare noise key, a unary mismatch must be suppressed entirely.
	noiseCfg := map[string]map[string][]string{"body": {"decoded_data": {}}}
	pass, _ := Match(tc, &act, noiseCfg, false, zap.NewNop(), false)
	if !pass {
		t.Fatal("a unary body mismatch was NOT suppressed by the bare `decoded_data` noise key. " +
			"Unary difference keys must stay exactly what they were before streams existed.")
	}
}

// TestMatch_StreamNoiseKeysStripTheMessageIndex: the same bare noise entry
// must keep working when the body is a stream. Joining the key's parts
// verbatim would look up `1.decoded_data` and match nothing.
func TestMatch_StreamNoiseKeysStripTheMessageIndex(t *testing.T) {
	recorded := []models.GrpcLengthPrefixedMessage{msg("alpha"), msg("beta")}
	// same length as "beta": only decoded_data differs, so the noise key is
	// the only thing that can suppress it
	actual := []models.GrpcLengthPrefixedMessage{msg("alpha"), msg("brav")}

	var exp models.GrpcResp
	exp.Headers, exp.Trailers = emptyHdrs(), okTrailers()
	exp.SetMessages(recorded)
	var act models.GrpcResp
	act.Headers, act.Trailers = emptyHdrs(), okTrailers()
	act.SetMessages(actual)

	tc := &models.TestCase{Name: "stream-noise", GrpcResp: exp}
	noiseCfg := map[string]map[string][]string{"body": {"decoded_data": {}}}

	pass, _ := Match(tc, &act, noiseCfg, false, zap.NewNop(), false)
	if !pass {
		t.Fatal("a bare `decoded_data` noise key did not suppress a STREAM body mismatch. " +
			"The per-message index has to be stripped before the bodyNoise lookup, or every " +
			"gRPC noise entry a user already has goes inert the moment a stream is recorded.")
	}
}

// TestCompareGrpcMessage_KeyShape pins the exact difference keys, because
// noise suppression is not the only thing that reads them.
//
// The keys land in the differences map, which drives the diff shown to the
// user and is the vocabulary the report reader and UI key off. Suppression
// alone cannot catch a change here: the index-stripping in the noise lookup
// deliberately makes `body.0.decoded_data` and `body.decoded_data` behave the
// same for noise, so an accidental switch to always-indexed keys would leave
// every noise test green while changing what every unary failure is called.
func TestCompareGrpcMessage_KeyShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		indexed bool
		idx     int
		want    []string
	}{
		{
			name: "unary keeps the pre-streaming keys", indexed: false, idx: 0,
			want: []string{"body.compression_flag", "body.decoded_data"},
		},
		{
			name: "streaming carries the message index", indexed: true, idx: 2,
			want: []string{"body.2.compression_flag", "body.2.decoded_data"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			differences := make(map[string]grpcDiff)
			result := &models.Result{BodyResult: make([]models.BodyResult, 0)}

			// Differ in compression flag and content, same length so the
			// length difference is not the one under test.
			expMsg := models.GrpcLengthPrefixedMessage{CompressionFlag: 0, MessageLength: 5, DecodedData: "alpha"}
			actMsg := models.GrpcLengthPrefixedMessage{CompressionFlag: 1, MessageLength: 5, DecodedData: "brave"}

			compareGrpcMessage(tc.idx, tc.indexed, expMsg, actMsg, differences, result, nil, false, zap.NewNop())

			for _, want := range tc.want {
				if _, ok := differences[want]; !ok {
					got := make([]string, 0, len(differences))
					for k := range differences {
						got = append(got, k)
					}
					t.Fatalf("missing difference key %q; got %v.\nThese strings are the vocabulary "+
						"the report reader, the UI and users' assertions.noise all key off — a "+
						"unary failure must keep the name it has always had.", want, got)
				}
			}
		})
	}
}

// TestUseIndexedKeys pins when a message index appears in a difference key.
// Unary must never be indexed — see useIndexedKeys' own comment for why the
// noise tests cannot catch a regression here.
func TestUseIndexedKeys(t *testing.T) {
	one := []models.GrpcLengthPrefixedMessage{msg("a")}
	two := []models.GrpcLengthPrefixedMessage{msg("a"), msg("b")}

	if useIndexedKeys(one, one) {
		t.Fatal("a unary comparison would use indexed difference keys. That renames every " +
			"existing failure (body.decoded_data -> body.0.decoded_data), which the report " +
			"reader, the UI and users' recorded noise entries all key off.")
	}
	if !useIndexedKeys(two, one) || !useIndexedKeys(one, two) || !useIndexedKeys(two, two) {
		t.Fatal("a stream on either side must produce indexed keys, or two messages' " +
			"differences collide under the same key and one silently overwrites the other")
	}
}
