package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// While the control plane has paused recording, a new connection to the app
// is spliced to it unread: an HTTP/1 request reaches the app byte for byte,
// and what the app sends back, which is no HTTP response at all here, reaches
// the client as it is, which a connection framed as HTTP could not do. No
// test case comes of it and no request is counted against a memory pause.
// The resume resets the connection, so its client reconnects under capture.
func TestAPausedIngressSplicesNewConnectionsAndResetsThemAtTheResume(t *testing.T) {
	t.Cleanup(func() {
		util.DefaultPausePassthroughs.ResetAll()
		memoryguard.SetExternalPause(false, "")
		syncMock.ResetPauseCausesForTest()
	})
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	appEnded := make(chan error, 1)
	go func() { // the app: echoes what it reads, raw
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, err = io.Copy(c, c)
		appEnded <- err
	}()

	// The ingress's own listener, for a real client socket to reset.
	front, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = front.Close() }()
	client, err := net.Dial("tcp4", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server, err := front.Accept()
	if err != nil {
		t.Fatal(err)
	}

	util.DefaultPausePassthroughs.Open()
	memoryguard.SetExternalPause(true, "rollout")

	mgr := syncMock.New(zap.NewNop())
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	defer cancel()
	pm := &IngressProxyManager{logger: zap.NewNop()}
	tcs := make(chan *models.TestCase, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pm.handleConnection(ctx, server, ln.Addr().String(), pm.logger, tcs, make(chan struct{}, 1), 8080)
	}()

	req := []byte("GET /orders HTTP/1.1\r\nHost: app\r\n\r\n")
	if _, err := client.Write(req); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(req))
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second)) // bounds only a splice that never relays
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("the client read nothing back through the paused ingress: %v", err)
	}
	if string(got) != string(req) {
		t.Fatalf("the client read %q, want the app's raw echo of its request: the connection was framed, not spliced", got)
	}
	if n := util.DefaultPausePassthroughs.Len(); n != 1 {
		t.Fatalf("%d passthroughs are tracked for the resume, want this connection", n)
	}

	// The resume, as the proxy does it: the reset first, then the flag.
	util.DefaultPausePassthroughs.ResetAll()
	memoryguard.SetExternalPause(false, "")
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the client's connection ended with %v at the resume; want a reset, so it reconnects under capture", err)
	}
	select {
	case err := <-appEnded:
		if !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("the app's connection ended with %v at the resume; want a reset", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the app's connection did not end at the resume")
	}
	<-done
	select {
	case tc := <-tcs:
		t.Fatalf("a test case came of a connection spliced while recording was paused: %+v", tc)
	default:
	}
	if n := mgr.NotRecordedUnderPressure().Total; n != 0 {
		t.Fatalf("%d requests of a spliced connection were reported as lost to memory pressure", n)
	}
}

// With the control plane's pause on and its tracking not open (the resume is
// in progress), a connection is not spliced: nothing would reset it. It is
// left to the framed path, untouched.
func TestAPausedIngressDoesNotSpliceWhatItCannotTrack(t *testing.T) {
	t.Cleanup(func() {
		memoryguard.SetExternalPause(false, "")
		syncMock.ResetPauseCausesForTest()
	})
	util.DefaultPausePassthroughs.ResetAll() // not open
	memoryguard.SetExternalPause(true, "rollout")
	client, server := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	pm := &IngressProxyManager{logger: zap.NewNop()}
	if pm.splicePaused(context.Background(), server, "127.0.0.1:1", pm.logger) {
		t.Fatal("a connection that cannot be tracked for the resume was spliced")
	}
}
