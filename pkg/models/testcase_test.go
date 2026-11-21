package models

import "testing"

// AgentName is the name the agent knows a test case by: its capture id when
// it came with one, else its name.
func TestTestCase_AgentName(t *testing.T) {
	for _, tc := range []struct {
		tc   TestCase
		want string
	}{
		{TestCase{CaptureID: CaptureIDPrefix + "n-1"}, CaptureIDPrefix + "n-1"},
		{TestCase{CaptureID: CaptureIDPrefix + "n-1", Name: "get-a-1"}, CaptureIDPrefix + "n-1"},
		{TestCase{Name: "test-3"}, "test-3"},
		{TestCase{}, ""},
	} {
		if got := tc.tc.AgentName(); got != tc.want {
			t.Fatalf("AgentName of %+v = %q, want %q", tc.tc, got, tc.want)
		}
	}
}
