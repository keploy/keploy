package cli

import "testing"

func TestIsDaemonSetAgent(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{name: "empty (equivalent to unset)", val: "", want: false},
		{name: "true", val: "true", want: true},
		{name: "one", val: "1", want: true},
		{name: "false", val: "false", want: false},
		{name: "zero", val: "0", want: false},
		{name: "uppercase TRUE is not accepted", val: "TRUE", want: false},
		{name: "arbitrary", val: "yes", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv restores the previous value after the test; an empty value
			// is treated the same as unset by isDaemonSetAgent.
			t.Setenv("KEPLOY_DAEMONSET_ENABLED", tc.val)
			if got := isDaemonSetAgent(); got != tc.want {
				t.Errorf("isDaemonSetAgent() with KEPLOY_DAEMONSET_ENABLED=%q = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
