package proxy

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestWaitForIngressTargetReady(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()

	if err := waitForIngressTarget(context.Background(), ln.Addr().String(), 250*time.Millisecond); err != nil {
		t.Fatalf("waitForIngressTarget returned error: %v", err)
	}

	select {
	case <-accepted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("waitForIngressTarget did not dial the ready listener")
	}
}

func TestWaitForIngressTargetTimeout(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if err := waitForIngressTarget(context.Background(), addr, 40*time.Millisecond); err == nil {
		t.Fatal("expected timeout waiting for unused port")
	}
}

func TestWaitForIngressTargetWhenKnownSkipsUnknownPort(t *testing.T) {
	start := time.Now()
	waited, err := waitForIngressTargetWhenKnown(context.Background(), 0, "127.0.0.1:0", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForIngressTargetWhenKnown returned error: %v", err)
	}
	if waited {
		t.Fatal("expected unknown redirected port to skip target wait")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("unknown redirected port should skip immediately, took %s", elapsed)
	}
}
