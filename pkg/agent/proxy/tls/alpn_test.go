package tls

import (
	"reflect"
	"testing"
)

func TestNegotiateNextProtos(t *testing.T) {
	tests := []struct {
		name       string
		client     []string
		preserveH2 bool
		want       []string
	}{
		// preserveH2=false (record, and replay of non-Http2 targets): the
		// original behaviour — a dual-protocol client is downgraded to http/1.1.
		{"http1 only", []string{"http/1.1"}, false, []string{"http/1.1"}},
		{"http1 and h2 downgrades", []string{"http/1.1", "h2"}, false, []string{"http/1.1"}},
		{"h2 first then http1 downgrades", []string{"h2", "http/1.1"}, false, []string{"http/1.1"}},
		{"h2 only", []string{"h2"}, false, []string{"h2", "http/1.1"}},
		{"postgresql only", []string{"postgresql"}, false, nil},
		{"mongodb only", []string{"mongodb"}, false, nil},
		{"redis only", []string{"redis"}, false, nil},
		{"empty client list", []string{}, false, nil},
		{"nil client list", nil, false, nil},
		{"unknown protocol mix", []string{"foo", "bar"}, false, nil},
		{"http1 and unknown", []string{"foo", "http/1.1", "bar"}, false, []string{"http/1.1"}},
		{"h2 and unknown but no http1", []string{"foo", "h2", "bar"}, false, []string{"h2", "http/1.1"}},

		// preserveH2=true (replay of a target that has kind:Http2 mocks): a
		// dual-protocol client keeps h2 so its request matches the Http2 mock.
		{"preserveH2: http1 and h2 keeps h2", []string{"http/1.1", "h2"}, true, []string{"h2", "http/1.1"}},
		{"preserveH2: h2 then http1 keeps h2", []string{"h2", "http/1.1"}, true, []string{"h2", "http/1.1"}},
		// preserveH2 has no effect when the client can't do h2.
		{"preserveH2: http1 only stays http1", []string{"http/1.1"}, true, []string{"http/1.1"}},
		{"preserveH2: postgres still nil", []string{"postgresql"}, true, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := negotiateNextProtos(tc.client, tc.preserveH2)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("negotiateNextProtos(%v, preserveH2=%v) = %v, want %v", tc.client, tc.preserveH2, got, tc.want)
			}
		})
	}
}
