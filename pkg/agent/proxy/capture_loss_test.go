package proxy

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
		// A memory-guard pause no longer drops a chunk (relay.DropMemoryPressure
		// is not produced), and what a pause costs is the mock manager's count.
		{Kind: models.CaptureLossDropped, Count: 1, What: relay.DropPerConnCap + " on a connection to 10.0.0.5:5432"},
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

// stalledAt is an index whose read of the container at addr stalled: the
// daemon did not answer it (containerkey's stallResolver).
type stalledAt struct{ containerAt }

func (stalledAt) FingerprintsOrStall(context.Context, string) (string, string, bool, string) {
	return "", "", false, "pg (postgres:16)"
}

// Under the proven switch a use of a connection whose container's read
// stalled is a capture loss that leaves its test not judged, naming the
// container; without the switch the proxy hooks nothing into the keyer.
func TestAUseOfAContainerWhoseReadStalledIsACaptureLoss(t *testing.T) {
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	for _, on := range []bool{false, true} {
		t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[on])
		containerkey.Default = containerkey.NewKeyer(stalledAt{containerAt{addr: "172.17.0.7:5432", c: containerkey.Container{ID: "pg", Image: "postgres:16", Port: 5432}}})
		p := &Proxy{}
		p.bindContainerKeys(context.Background()) // as StartProxy does
		containerkey.Default.Name(nil, "1", "172.17.0.7:5432")
		if _, st := containerkey.StateFromContext(containerkey.WithConn(context.Background(), containerkey.Default, "1")); st != containerkey.Withheld {
			t.Fatalf("switch %v: the use = %v, want Withheld", on, st)
		}
		got := p.CaptureLosses()
		if !on {
			if len(got) != 0 {
				t.Fatalf("without the switch the proxy counted %+v", got)
			}
			continue
		}
		if len(got) != 1 || got[0].Kind != models.CaptureLossContainerUnread || got[0].Count != 1 || !strings.Contains(got[0].What, "pg (postgres:16)") || !got[0].Kind.NotJudged() || !got[0].Kind.PerTest() {
			t.Fatalf("the proxy counted %+v, want one container_unread naming pg, not judged, the test's own", got)
		}
	}
}

// lossesOf is p's count of capture losses of kind, 0 when none.
func lossesOf(p *Proxy, kind models.CaptureLossKind) uint64 {
	for _, l := range p.losses.Snapshot() {
		if l.Kind == kind {
			return l.Count
		}
	}
	return 0
}
