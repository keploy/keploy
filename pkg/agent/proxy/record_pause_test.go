package proxy

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The proxy is what the agent service asks to pause and resume recording.
var _ interface {
	PauseRecording(ctx context.Context, reason string) error
	ResumeRecording(ctx context.Context) error
	RecordingPaused() (paused bool, reason string)
} = (*Proxy)(nil)

// endRecordPause ends whatever pause a test left on.
func endRecordPause(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		util.DefaultPausePassthroughs.ResetAll()
		memoryguard.SetExternalPause(false, "")
		syncMock.ResetPauseCausesForTest()
	})
}

// While the control plane has paused recording, a new connection of a
// recording session is relayed to its destination unread: bytes that open a
// TLS handshake, which the session would intercept, reach the destination as
// they were sent, and its answer reaches the application. The resume resets
// both of the connection's sockets, so the application reconnects under
// capture, and only then ends the pause.
func TestAPausedRecordingPassesNewConnectionsThroughAndResetsThemAtTheResume(t *testing.T) {
	endRecordPause(t)
	upstream, _, upstreamEnded := echoUpstream(t)
	app, src := acceptedConn(t)
	p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, upstream)}, agentMode: models.MODE_RECORD}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	ctx := context.Background()

	if err := p.PauseRecording(ctx, "rollout"); err != nil {
		t.Fatal(err)
	}
	if on, reason := p.RecordingPaused(); !on || reason != "rollout" {
		t.Fatalf("RecordingPaused() = %v, %q after the pause", on, reason)
	}
	if !memoryguard.IsRecordingPaused() {
		t.Fatal("the pause did not reach the flag every consumer reads")
	}

	done := make(chan error, 1)
	go func() { done <- p.handleConnection(ctx, src) }()

	// The first bytes of a TLS ClientHello: a recording session reads these
	// and answers with its own handshake. A passthrough hands them on.
	hello := "\x16\x03\x01\x00\x05hello"
	if _, err := app.Write([]byte(hello)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("handleConnection returned (%v) without passing the connection through", err)
	case got := <-readN(app, len(hello)):
		if got != hello {
			t.Fatalf("the application read %q through the paused proxy, want its own bytes echoed by the destination", got)
		}
	}
	if n := util.DefaultPausePassthroughs.Len(); n != 1 {
		t.Fatalf("%d passthroughs are tracked for the resume, want this connection", n)
	}

	appEnded := readErr(app)
	if err := p.ResumeRecording(ctx); err != nil {
		t.Fatal(err)
	}
	if err := awaitEvent(t, appEnded, "the application's connection ending at the resume"); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the application's connection ended with %v at the resume; want a reset, so it reconnects under capture", err)
	}
	if err := awaitEvent(t, upstreamEnded, "the destination's connection ending at the resume"); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the destination's connection ended with %v at the resume; want a reset: the exchange may be mid-message", err)
	}
	if err := awaitEvent(t, done, "handleConnection returning after the resume"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	if src.closes.Load() == 0 {
		t.Fatal("handleConnection returned without closing the application's connection")
	}
	if on, reason := p.RecordingPaused(); on || reason != "" {
		t.Fatalf("RecordingPaused() = %v, %q after the resume", on, reason)
	}
	if memoryguard.IsRecordingPaused() {
		t.Fatal("recording is still paused after the resume")
	}
	if n := util.DefaultPausePassthroughs.Len(); n != 0 {
		t.Fatalf("%d passthroughs are still tracked after the resume", n)
	}
	if _, ok := util.DefaultPausePassthroughs.Begin(src); ok {
		t.Fatal("a connection was tracked as a pause's passthrough once recording had resumed")
	}
}

// Pausing and resuming are idempotent, and the control plane's resume leaves
// a pause the memory guard holds in place.
func TestPauseAndResumeRecordingAreIdempotentAndLeaveAMemoryPauseAlone(t *testing.T) {
	endRecordPause(t)
	p := &Proxy{logger: zap.NewNop()}
	ctx := context.Background()

	if err := p.ResumeRecording(ctx); err != nil {
		t.Fatalf("a resume with no pause on: %v", err)
	}
	if memoryguard.IsRecordingPaused() {
		t.Fatal("a resume with no pause on paused recording")
	}
	for _, reason := range []string{"rollout", "rollout, still"} {
		if err := p.PauseRecording(ctx, reason); err != nil {
			t.Fatal(err)
		}
		if on, got := p.RecordingPaused(); !on || got != reason {
			t.Fatalf("RecordingPaused() = %v, %q, want paused for %q", on, got, reason)
		}
	}

	endMemory := memoryguard.SetRecordingPausedForTest(true)
	for i := 0; i < 2; i++ {
		if err := p.ResumeRecording(ctx); err != nil {
			t.Fatal(err)
		}
		if on, _ := p.RecordingPaused(); on {
			t.Fatal("the control plane's pause is still on after its resume")
		}
		if !memoryguard.IsRecordingPaused() {
			t.Fatal("the control plane's resume ended a pause the memory guard holds")
		}
	}
	endMemory()
	// A test's pause does not tell the mock managers of its end, as the
	// memory guard's does: say it, so the manager's span closes.
	memoryguard.SetExternalPause(false, "")
	if memoryguard.IsRecordingPaused() {
		t.Fatal("recording is still paused with no reason left")
	}
}

// A name resolved while recording is paused is answered and not cached: its
// next lookup, after the resume, must reach the recorder, or the name would
// have no mock for as long as the cache held its answer.
func TestADNSAnswerResolvedWhilePausedIsNotCached(t *testing.T) {
	if !shouldCacheDNSResponse(models.MODE_RECORD, dnsCacheEntry{FromUpstream: true}) {
		t.Fatal("a recorded upstream answer is no longer cached")
	}
	if shouldCacheDNSResponse(models.MODE_RECORD, dnsCacheEntry{FromUpstream: true, Unrecorded: true}) {
		t.Fatal("an answer resolved while recording was paused, of which no mock was made, was cached")
	}
}
