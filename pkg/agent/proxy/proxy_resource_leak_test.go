package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

func TestStopProxyServer_NoResourceLeak(t *testing.T) {
	logger := zaptest.NewLogger(t)
	
	proxy := &Proxy{
		logger:            logger,
		clientConnections: []net.Conn{},
		connMutex:         &sync.Mutex{},
		errChannel:        make(chan error, 10),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan bool, 1)

	go func() {
		proxy.StopProxyServer(ctx)
		done <- true
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("StopProxyServer did not complete")
	}

	if !proxy.connMutex.TryLock() {
		t.Error("mutex is still locked")
		return
	}
	proxy.connMutex.Unlock()
}

func TestStopProxyServer_WithFailingConnections(t *testing.T) {
	logger := zaptest.NewLogger(t)
	
	proxy := &Proxy{
		logger:            logger,
		clientConnections: []net.Conn{},
		connMutex:         &sync.Mutex{},
		errChannel:        make(chan error, 10),
	}

	conn1, _ := net.Pipe()
	conn1.Close()
	conn2, _ := net.Pipe()
	conn2.Close()

	proxy.clientConnections = append(proxy.clientConnections, conn1, conn2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan bool, 1)

	go func() {
		proxy.StopProxyServer(ctx)
		done <- true
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("StopProxyServer did not complete")
	}

	if !proxy.connMutex.TryLock() {
		t.Error("mutex is still locked after cleanup")
		return
	}
	proxy.connMutex.Unlock()
}

