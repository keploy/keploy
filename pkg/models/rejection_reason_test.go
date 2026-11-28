package models

import "testing"

func TestRejectionWarmRecordingUnmockedConstant(t *testing.T) {
	if RejectionWarmRecordingUnmocked != "WARM_RECORDING_UNMOCKED" {
		t.Fatalf("RejectionWarmRecordingUnmocked = %q, want WARM_RECORDING_UNMOCKED", RejectionWarmRecordingUnmocked)
	}
	seen := map[RejectionReason]bool{}
	for _, r := range []RejectionReason{
		RejectionObsolete,
		RejectionHighRisk,
		RejectionLowRiskNoNoise,
		RejectionWarmRecordingUnmocked,
	} {
		if seen[r] {
			t.Fatalf("duplicate RejectionReason constant: %q", r)
		}
		seen[r] = true
	}
}
