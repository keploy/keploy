package memoryguard

import (
	"testing"
)

func TestParseForcedPauseTakesTwoOrderedRequestNumbers(t *testing.T) {
	for _, tc := range []struct {
		in       string
		from, to uint64
		ok       bool
	}{
		{"3:5", 3, 5, true},
		{" 1 : 2 ", 1, 2, true},
		{"", 0, 0, false},
		{"3", 0, 0, false},
		{"0:5", 0, 0, false},
		{"5:5", 0, 0, false},
		{"5:3", 0, 0, false},
		{"a:5", 0, 0, false},
		{"3:b", 0, 0, false},
		{"-1:5", 0, 0, false},
	} {
		from, to, err := parseForcedPause(tc.in)
		if (err == nil) != tc.ok || from != tc.from || to != tc.to {
			t.Errorf("parseForcedPause(%q) = %d, %d, %v; want %d, %d, ok=%v", tc.in, from, to, err, tc.from, tc.to, tc.ok)
		}
	}
}

// The knob pauses recording as the from-th ingress request is counted and ends
// the pause as the to-th is, once, through the guard's own pause: the flag the
// ingress reads and the mock managers' pressure signal move together. Each
// request reads the pause after it is counted, as the ingress does.
func TestTheForcedPausePausesFromItsFromRequestToItsToRequest(t *testing.T) {
	var signals []bool
	unregister := RegisterPressureHook(func(paused bool) { signals = append(signals, paused) })
	t.Cleanup(unregister)
	t.Cleanup(SetForcedPauseForTest("2:4"))

	var pausedAt []int
	for n := 1; n <= 6; n++ {
		IngressRequest()
		if IsRecordingPaused() {
			pausedAt = append(pausedAt, n)
		}
	}
	if len(pausedAt) != 2 || pausedAt[0] != 2 || pausedAt[1] != 3 {
		t.Fatalf("requests %v read the pause, want 2 and 3: from its from-th request to the one before its to-th", pausedAt)
	}
	if len(signals) != 2 || !signals[0] || signals[1] {
		t.Fatalf("the mock managers were told %v, want [true false]: the pause and its end, once each", signals)
	}
}

// Unset, the knob counts nothing and never pauses; a value it cannot read
// sets nothing either.
func TestTheForcedPauseIsANoOpUnlessSet(t *testing.T) {
	for _, v := range []string{"", "4:2"} {
		restore := SetForcedPauseForTest(v)
		for n := 0; n < 5; n++ {
			IngressRequest()
			if IsRecordingPaused() {
				restore()
				t.Fatalf("knob %q paused recording at request %d", v, n+1)
			}
		}
		if forcedPause.Load() != nil {
			restore()
			t.Fatalf("knob %q is set", v)
		}
		restore()
	}
}

// Start reads the knob from the environment with its count at zero, whether
// or not the guard itself runs (here it does not: no limit).
func TestStartReadsTheForcedPauseFromTheEnvironment(t *testing.T) {
	t.Setenv(ForcePauseEnv, "1:2")
	t.Cleanup(SetForcedPauseForTest(""))
	if err := Start(t.Context(), nil, false, 0); err != nil {
		t.Fatalf("Start: %v", err)
	}
	IngressRequest()
	if !IsRecordingPaused() {
		t.Fatal("the first ingress request after Start did not pause recording")
	}
	IngressRequest()
	if IsRecordingPaused() {
		t.Fatal("the second ingress request after Start did not end the pause")
	}
}
