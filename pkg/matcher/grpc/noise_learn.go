package grpc

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// Learning noise from a gRPC body difference. Shared by every learner of
// noise (enterprise's auto-noise, k8s-proxy's auto-replay), so each reads the
// same bodies and keeps the same paths.

// NoiseBodies are the bodies a gRPC body difference is learned from: the
// decoded JSON the replay showed them as (Result.GrpcDecode). why, with its
// refusal reason, says why none can be:
//   - a stream of several messages, or one whose message count differs:
//     GrpcDecode holds one message, so the others' differences cannot be
//     read, and no body noise answers a count;
//   - bodies the test case's assertions did not compare (they replaced the
//     comparison): no body noise answers an assertion's failure;
//   - bodies not decoded (no schema): the recorded protoscope is no JSON;
//   - each side decoded with another schema: a field renamed, or defined on
//     one side only, yields a name the replay cannot translate (it
//     translates through the recorded side's schema), which the matcher
//     would still accept here, a false "resolved".
func NoiseBodies(r models.Result) (expected, actual, why string, reason models.NoiseFailureReason) {
	messages := 0
	for _, b := range r.BodyResult {
		if b.NotCompared {
			return "", "", "the test case's assertions replaced the comparison, so its bodies were not compared and no body noise answers its failure", models.NoiseFailureUnresolved
		}
		if b.Type == models.GrpcData {
			messages++
		}
		// The stream's message count differs (the matcher's count row, beside
		// the rows of the messages both sides have): no body noise answers it.
		if !b.Normal && IsMessageCountRow(b) {
			return "", "", fmt.Sprintf("the stream carries %s, recorded %s: a count no body noise excuses", b.Actual, b.Expected), models.NoiseFailureUnresolved
		}
	}
	if messages > 1 {
		return "", "", fmt.Sprintf("the response is a stream of %d messages, and only one of its messages is decoded", messages), models.NoiseFailureUnresolved
	}
	d := r.GrpcDecode
	if d == nil || d.ExpectedJSON == "" || d.ActualJSON == "" {
		return "", "", "the bodies were not decoded with a schema, so they have no field names (give the run --proto-dir, or upload the release's protos with keploy proto upload)", models.NoiseFailureNonJSONBody
	}
	if d.Expected == "" || d.Expected != d.Actual {
		return "", "", fmt.Sprintf("the two sides were decoded with different schemas (%q and %q), so a field name may not translate to the recorded field", d.Expected, d.Actual), models.NoiseFailureUnresolved
	}
	return d.ExpectedJSON, d.ActualJSON, "", ""
}

// TranslatablePaths keeps the learned paths the replay can translate to
// field numbers: none reaching a field the schema does not define (shown as
// "#<number>"), and none whose first field is absent from the recorded
// message (expected), which its schema may not name. The others are returned
// apart.
//
// opaque are the fields a path may name whole but not step past (a map, a
// Struct, an Any: GrpcDecodeInfo.OpaqueFields). When the replay could not
// list them all (opaqueComplete false: a recursive or very large schema), a
// path of more than one field might step past one it did not list, so only
// top-level fields are kept.
func TranslatablePaths(paths []string, expected string, opaque []string, opaqueComplete bool) (kept, dropped []string) {
	var top map[string]any
	_ = json.Unmarshal([]byte(expected), &top)
	present := make(map[string]bool, len(top))
	for k := range top {
		present[strings.ToLower(k)] = true
	}
	for _, p := range paths {
		segs := strings.Split(p, ".")
		ok := present[strings.ToLower(segs[0])]
		for _, seg := range segs {
			if strings.HasPrefix(seg, "#") {
				ok = false
				break
			}
		}
		// A step past a map, Struct or Any names a key, not a field: the
		// replay translates the field whole and refuses the step.
		for _, o := range opaque {
			if ok && strings.HasPrefix(strings.ToLower(p), strings.ToLower(o)+".") {
				ok = false
			}
		}
		if !opaqueComplete && len(segs) > 1 {
			ok = false
		}
		if ok {
			kept = append(kept, p)
		} else {
			dropped = append(dropped, p)
		}
	}
	return kept, dropped
}
