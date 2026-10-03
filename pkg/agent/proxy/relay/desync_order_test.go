package relay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The owner of a tee stops the connection's recording when the tee reports its
// hole (Config.OnCaptureDesync), and the span of what the connection carries
// from then on opens there. So the tee reports it first, as the chunk is lost:
// before it writes the hole's WARN. Reported after it, what the connection
// carried while the WARN was written, on a starved agent for a while, was in
// no span.
func TestTee_ReportsItsHoleBeforeAnythingElseSeesIt(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zapcore.DebugLevel)
	tt := newTee(fakeconn.FromClient, 4, 1, testStallGrace, nil, nil, zap.New(core))
	tt.start(make(chan struct{}))
	t.Cleanup(func() {
		tt.close()
		tt.waitDone()
	})
	var warned []bool
	tt.onDesync = func(string) bool {
		warned = append(warned, logs.FilterLevelExact(zapcore.WarnLevel).Len() > 0)
		return true
	}

	if tt.push(mkChunk("0123456789")) {
		t.Fatal("a chunk over the cap was admitted")
	}
	if tt.push(mkChunk("x")) {
		t.Fatal("a chunk after the hole was admitted")
	}
	if len(warned) != 1 {
		t.Fatalf("the hole was reported %d times, want once", len(warned))
	}
	if warned[0] {
		t.Error("the hole was reported after its WARN was written")
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 1 {
		t.Errorf("%d WARNs for the hole, want one", n)
	}
	if !tt.desynced.Load() {
		t.Error("the stream is not marked desynced after its hole")
	}
}

// A hole as the owner's recording stops costs it nothing: the connection is
// torn down with the recording, and the owner opens no span for it. Its WARN
// would say that every test case recorded from then on is left out, when none
// is, so the tee warns of a hole only when its owner says it costs something
// (Config.OnCaptureDesync). The hole is still the end of the stream's
// capture: reported once, and nothing after it admitted.
func TestTee_WarnsOfAHoleOnlyWhenItCostsTheRecordingSomething(t *testing.T) {
	t.Parallel()
	for _, costs := range []bool{true, false} {
		core, logs := observer.New(zapcore.DebugLevel)
		tt := newTee(fakeconn.FromClient, 4, 1, testStallGrace, nil, nil, zap.New(core))
		tt.start(make(chan struct{}))
		reported := 0
		tt.onDesync = func(string) bool {
			reported++
			return costs
		}

		if tt.push(mkChunk("0123456789")) {
			t.Fatal("a chunk over the cap was admitted")
		}
		if tt.push(mkChunk("x")) {
			t.Fatal("a chunk after the hole was admitted")
		}
		tt.close()
		tt.waitDone()
		if reported != 1 {
			t.Errorf("costs=%v: the hole was reported %d times, want once", costs, reported)
		}
		if !tt.desynced.Load() {
			t.Errorf("costs=%v: the stream is not marked desynced after its hole", costs)
		}
		want := 0
		if costs {
			want = 1
		}
		if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != want {
			t.Errorf("costs=%v: %d WARNs for the hole, want %d", costs, n, want)
		}
	}
}
