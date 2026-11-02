package models

import (
	"reflect"
	"testing"
)

// MockMatchingNoise keeps exactly the buckets the agent's mock matchers read
// (header, body, requestbody, url), each as the user wrote it, adds none
// the noise does not have, and drops a bucket no matcher reads.
func TestMockMatchingNoiseKeepsOnlyTheBucketsTheMatchersRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		noise GlobalNoise
		want  GlobalNoise
	}{
		{
			name: "every bucket a matcher reads",
			noise: GlobalNoise{
				"header":      {"x-request-id": {}},
				"body":        {"eventTs": {}},
				"requestbody": {"nonce": {`\d+`}},
				"url":         {"/orders/": {`[0-9]+`}},
				"status":      {"code": {}},
			},
			want: GlobalNoise{
				"header":      {"x-request-id": {}},
				"body":        {"eventTs": {}},
				"requestbody": {"nonce": {`\d+`}},
				"url":         {"/orders/": {`[0-9]+`}},
			},
		},
		{
			name:  "only the buckets the noise has",
			noise: GlobalNoise{"url": {"/orders/": {`[0-9]+`}}},
			want:  GlobalNoise{"url": {"/orders/": {`[0-9]+`}}},
		},
		{
			name:  "no noise",
			noise: nil,
			want:  GlobalNoise{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MockMatchingNoise(tc.noise)
			if got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MockMatchingNoise(%v) = %#v, want %#v", tc.noise, got, tc.want)
			}
		})
	}
}
