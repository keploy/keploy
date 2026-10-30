package models

import "testing"

// A call keploy has no shape for is named by its protocol, and a call of no
// protocol, or of one keploy did not tell, is "a call": the line read "a
// unknown call".
func TestLoggedCallNameNamesACallOfNoKnownProtocolACall(t *testing.T) {
	for protocol, want := range map[string]string{"": "a call", "unknown": "a call", "Unknown": "a call", "Redis": "a Redis call"} {
		if got := LoggedCallName(UnmatchedCall{Protocol: protocol, ActualSummary: `find users {"email":"ada@example.com"}`}); got != want {
			t.Errorf("LoggedCallName(protocol %q) = %q, want %q", protocol, got, want)
		}
	}
}
