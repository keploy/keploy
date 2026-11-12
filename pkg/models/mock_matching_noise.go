package models

// mockMatchingBuckets are the buckets of keploy.yml's test.globalNoise that
// the agent's mock matchers read from OutgoingOptions.NoiseConfig:
//   - "header":      HTTP request-header matching.
//   - "body":        outgoing-payload matchers (e.g. the Pulsar SEND matcher
//     in the integrations repo) strip these field names.
//   - "requestbody": HTTP request-body matching, kept apart from "body" so
//     response-assertion noise cannot soften request matching
//     (http/decode.go).
//   - "url":         HTTP URL-path and query-value noise: regexes that mask
//     the part of a path or query value that changes per run
//     (NewRequestNoise in http/decode.go, MatchURLPath and QueryParamsMatch).
var mockMatchingBuckets = []string{"header", "body", "requestbody", "url"}

// MockMatchingNoise returns the buckets of noise that the agent's mock
// matchers read (mockMatchingBuckets), for OutgoingOptions.NoiseConfig. Every
// path that starts mock serving (keploy test, keploy mock replay and the
// runner) builds its NoiseConfig here, so a bucket a matcher reads reaches
// it on each of them. The result is never nil, and holds only the buckets
// that noise has.
func MockMatchingNoise(noise GlobalNoise) GlobalNoise {
	out := GlobalNoise{}
	for _, bucket := range mockMatchingBuckets {
		if fields, ok := noise[bucket]; ok {
			out[bucket] = fields
		}
	}
	return out
}
