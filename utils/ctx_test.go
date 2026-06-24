package utils

import (
	"testing"
	"time"
)

// TestSidecarDrainDuration covers the parsing of KEPLOY_SIDECAR_DRAIN_SECONDS,
// the env var the k8s injecting webhook sets in app-managed-drain mode. Only an
// explicit positive integer may delay shutdown; everything else (unset,
// non-numeric, zero, negative) must yield 0 so the historical
// cancel-immediately-on-signal behaviour is preserved for every other caller.
func TestSidecarDrainDuration(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want time.Duration
	}{
		{name: "unset", set: false, want: 0},
		{name: "empty", set: true, val: "", want: 0},
		{name: "positive", set: true, val: "15", want: 15 * time.Second},
		{name: "one", set: true, val: "1", want: time.Second},
		{name: "zero", set: true, val: "0", want: 0},
		{name: "negative", set: true, val: "-5", want: 0},
		{name: "non-numeric", set: true, val: "abc", want: 0},
		{name: "float-rejected", set: true, val: "1.5", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("KEPLOY_SIDECAR_DRAIN_SECONDS", tc.val)
			} else {
				// t.Setenv can't unset; ensure the loop's prior value can't
				// leak by explicitly clearing for this subtest.
				t.Setenv("KEPLOY_SIDECAR_DRAIN_SECONDS", "")
			}
			if got := sidecarDrainDuration(); got != tc.want {
				t.Fatalf("sidecarDrainDuration() = %v, want %v", got, tc.want)
			}
		})
	}
}
