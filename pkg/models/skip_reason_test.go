package models

import "testing"

func TestSkipReasonText(t *testing.T) {
	for _, c := range []struct {
		class, reason, detail, want string
	}{
		{"flaky", "replay_only", "", "flaky/replay_only"},
		{"flaky", "replay_only", "failed the replay after record with no keploy evidence", "flaky/replay_only: failed the replay after record with no keploy evidence"},
		{"keploy", "unconsumed_mock", "mock-3 (HTTP)", "keploy/unconsumed_mock: mock-3 (HTTP)"},
		{ManualSkipClass, "waits on a fix in payments", "", "manual: waits on a fix in payments"},
		{ManualSkipClass, "waits on a fix", "PAY-12", "manual: waits on a fix: PAY-12"},
		{"", "", "only the detail", "only the detail"},
	} {
		if got := SkipReasonText(c.class, c.reason, c.detail); got != c.want {
			t.Errorf("SkipReasonText(%q, %q, %q) = %q, want %q", c.class, c.reason, c.detail, got, c.want)
		}
	}
}

func TestSkipReasonOneLine(t *testing.T) {
	if got, want := SkipReasonOneLine("manual: first line\r\n  second\tline \n"), "manual: first line second line"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
