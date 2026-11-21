package generic

import (
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
)

// A capture a pause of the control plane's stops is tracked for the resume
// to reset, since nothing captures its connection again; one that cannot be
// tracked (the resume is in progress) goes on capturing; and a memory pause
// stops a capture untracked, as it always did.
func TestACaptureStoppedForTheControlPlanesPauseIsTrackedForTheResume(t *testing.T) {
	t.Cleanup(func() {
		proxyutil.DefaultPausePassthroughs.ResetAll()
		memoryguard.SetExternalPause(false, "")
		syncMock.ResetPauseCausesForTest()
	})
	conns := func() (net.Conn, net.Conn) {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		return a, b
	}
	stopped := func(s *captureStream) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopped
	}

	// The control plane's pause, tracking not open: the capture goes on.
	memoryguard.SetExternalPause(true, "rollout")
	s := newCaptureStream(8, 2)
	s.client, s.dest = conns()
	s.add(true, []byte("PING\r\n"))
	if stopped(s) || len(s.ch) != 1 {
		t.Fatalf("a capture that could not be tracked was stopped for the control plane's pause (stopped=%v, queued=%d)", stopped(s), len(s.ch))
	}

	// Tracking open: the capture stops, and its connection is tracked until
	// its forwarding ends.
	proxyutil.DefaultPausePassthroughs.Open()
	s.add(true, []byte("PING\r\n"))
	if !stopped(s) {
		t.Fatal("the control plane's pause did not stop the capture")
	}
	if n := proxyutil.DefaultPausePassthroughs.Len(); n != 1 {
		t.Fatalf("%d passthroughs tracked, want the connection whose capture stopped", n)
	}
	s.forwardingEnded()
	s.forwardingEnded() // idempotent
	if n := proxyutil.DefaultPausePassthroughs.Len(); n != 0 {
		t.Fatalf("%d passthroughs tracked after the connection's forwarding ended", n)
	}
	proxyutil.DefaultPausePassthroughs.ResetAll()
	memoryguard.SetExternalPause(false, "")

	// A memory pause: stopped, and nothing tracked.
	proxyutil.DefaultPausePassthroughs.Open()
	end := memoryguard.SetRecordingPausedForTest(true)
	defer end()
	m := newCaptureStream(8, 2)
	m.client, m.dest = conns()
	m.add(true, []byte("PING\r\n"))
	if !stopped(m) || proxyutil.DefaultPausePassthroughs.Len() != 0 {
		t.Fatalf("under a memory pause: stopped=%v with %d tracked, want a stop nothing tracks", stopped(m), proxyutil.DefaultPausePassthroughs.Len())
	}
}
