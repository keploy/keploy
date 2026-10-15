package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/models"
)

// A tee drop that loses captured bytes is a capture loss, and still marks the
// mock incomplete as before: a parser that stopped taking its queue (W8), and
// bytes dropped for want of room. A drop while paused, or after a desync
// already counted, is not.
func TestATeeDropThatLosesBytesIsACaptureLoss(t *testing.T) {
	p := &Proxy{}
	var marked []string
	dst := func() string { return "10.0.0.5:5432" }
	drop := p.teeDropped(func(r string) { marked = append(marked, r) }, dst, func() bool { return false })
	for _, r := range []string{relay.DropConsumerGone, relay.DropPerConnCap, relay.DropMemoryPressure, relay.DropPaused, relay.DropDesynced} {
		drop(r)
	}
	if len(marked) != 5 {
		t.Fatalf("marked %v, want every drop passed on to the session", marked)
	}
	got := p.CaptureLosses()
	want := models.CaptureLosses{
		{Kind: models.CaptureLossDropped, Count: 2, What: relay.DropMemoryPressure + " on a connection to 10.0.0.5:5432"},
		{Kind: models.CaptureLossParserStall, Count: 1, What: relay.DropConsumerGone + " on a connection to 10.0.0.5:5432"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("capture losses %+v, want %+v", got, want)
	}
}

// Once the recording is stopping, what the retired parsers still had queued is
// abandoned as the stop's: still passed on to the session, never counted as a
// test's capture loss.
func TestATeeDropWhileTheRecordingStopsIsNoCaptureLoss(t *testing.T) {
	p := &Proxy{}
	stopping := false
	var marked []string
	drop := p.teeDropped(func(r string) { marked = append(marked, r) }, func() string { return "10.0.0.5:5432" }, func() bool { return stopping })
	drop(relay.DropConsumerGone)
	stopping = true
	drop(relay.DropConsumerGone)
	drop(relay.DropPerConnCap)
	if len(marked) != 3 {
		t.Fatalf("marked %v, want every drop passed on to the session", marked)
	}
	if got := p.CaptureLosses(); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("capture losses %+v, want only the one before the stop", got)
	}
}
