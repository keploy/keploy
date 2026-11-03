package supervisor

import (
	"strings"
	"testing"
)

// The recording's stop partway through the response is a cause of its own, of
// class at_stop: at the upstream's end (LeftOutStopCut), with the connection
// still open (LeftOutStopCutOpen), ending the read after the upstream's end
// (LeftOutStopCutAfterEnd), or closing the application with the call in flight
// (LeftOutPartwayAtStop). Its next_step says the stop cut the call and the
// capture lost nothing: not class other, whose next_step points at --debug as
// for a response that does not decode, and no capture loss's knob. The
// client's other ends that record no fault stay class client_closed. A mark on
// the incomplete-mock flag before it is still the cause it is reported under.
func TestAStopCutIsLeftOutWithItsOwnNextStep(t *testing.T) {
	for _, tc := range []struct{ reason, class string }{
		{LeftOutStopCut, leftOutAtStop},
		{LeftOutStopCutOpen, leftOutAtStop},
		{LeftOutStopCutAfterEnd, leftOutAtStop},
		{LeftOutPartwayAtStop, leftOutAtStop},
		{LeftOutPartwayAtExit, leftOutClientClosed},
		{LeftOutPartwayUnjudged, leftOutClientClosed},
		{"per_conn_cap: " + LeftOutStopCut, leftOutPerConnCap},
		{"per_conn_cap: " + LeftOutStopCutOpen, leftOutPerConnCap},
		{"http decode error: read response body: unexpected EOF", leftOutOther},
	} {
		if got := leftOutClass(leftOutCause(tc.reason)); got != tc.class {
			t.Errorf("reason %q is of class %q, want %q", tc.reason, got, tc.class)
		}
	}
	next := leftOutNextStep(leftOutAtStop, true)
	for _, want := range []string{"traffic is unaffected", "reason=the recording's stop cut the response short",
		"in flight as the recording stopped", "partway through the response", "the capture lost nothing",
		"stop the recording once the app's calls have ended"} {
		if !strings.Contains(next, want) {
			t.Errorf("the stop cut's next_step %q does not say %q", next, want)
		}
	}
	for _, knob := range []string{"--debug", "--memory-limit", "maxMemoryPerConnection"} {
		if strings.Contains(next, knob) {
			t.Errorf("the stop cut's next_step %q gives %q, a capture loss's knob", next, knob)
		}
	}
}

// Each of the stop's reasons says how the stop cut the response, and no two
// say it alike: only LeftOutStopCut, whose upstream's end the parser read,
// says that the upstream ended the connection partway through it. The others
// were cut by the stop with the connection still open, by the stop's end of
// the read after the upstream's end, or by the stop's close of the
// application.
func TestTheStopsReasonsSayHowTheStopCutTheResponse(t *testing.T) {
	const upstreamsCut = "the upstream ended the connection partway through"
	reasons := []string{LeftOutStopCut, LeftOutStopCutOpen, LeftOutStopCutAfterEnd, LeftOutPartwayAtStop}
	for i, a := range reasons {
		for _, b := range reasons[i+1:] {
			if a == b {
				t.Errorf("two of the stop's reasons are both %q", a)
			}
		}
	}
	if !strings.Contains(LeftOutStopCut, upstreamsCut) {
		t.Errorf("LeftOutStopCut %q does not say %q", LeftOutStopCut, upstreamsCut)
	}
	for _, r := range reasons[1:] {
		if strings.Contains(r, upstreamsCut) {
			t.Errorf("the reason %q says %q: the upstream's end did not cut that response", r, upstreamsCut)
		}
	}
}
