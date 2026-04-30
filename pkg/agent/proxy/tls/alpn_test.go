package tls

import (
	"reflect"
	"testing"
)

func TestNegotiateNextProtos(t *testing.T) {
	tests := []struct {
		name   string
		client []string
		want   []string
	}{
		{"http1 only", []string{"http/1.1"}, []string{"http/1.1"}},
		{"http1 and h2", []string{"http/1.1", "h2"}, []string{"http/1.1"}},
		{"h2 first then http1", []string{"h2", "http/1.1"}, []string{"http/1.1"}},
		{"h2 only", []string{"h2"}, []string{"h2", "http/1.1"}},
		{"postgresql only", []string{"postgresql"}, nil},
		{"mongodb only", []string{"mongodb"}, nil},
		{"redis only", []string{"redis"}, nil},
		{"empty client list", []string{}, nil},
		{"nil client list", nil, nil},
		{"unknown protocol mix", []string{"foo", "bar"}, nil},
		{"http1 and unknown", []string{"foo", "http/1.1", "bar"}, []string{"http/1.1"}},
		{"h2 and unknown but no http1", []string{"foo", "h2", "bar"}, []string{"h2", "http/1.1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := negotiateNextProtos(tc.client)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("negotiateNextProtos(%v) = %v, want %v", tc.client, got, tc.want)
			}
		})
	}
}
