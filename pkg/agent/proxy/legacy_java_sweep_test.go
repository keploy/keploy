package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// The retired CA's JDK keystore alias is removed off the agent's start
// path: its removal may run keytool, a JVM that takes seconds on a starved
// host. It starts once the proxy listens, never before; the start goes on
// while it runs; and a start whose listener fails removes nothing.
func TestTheRetiredJDKAliasSweepRunsOnlyOnceTheProxyListens(t *testing.T) {
	old := sweepLegacyJavaAlias
	t.Cleanup(func() { sweepLegacyJavaAlias = old })
	swept := make(chan struct{}, 2)
	release := make(chan struct{})
	// A failing row releases the sweep too, so that no row waits on it.
	var once sync.Once
	releaseSweep := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseSweep)
	sweepLegacyJavaAlias = func(context.Context, *zap.Logger) {
		swept <- struct{}{}
		<-release
	}
	p := &Proxy{logger: zap.NewNop()}

	t.Run("the proxy listens", func(t *testing.T) {
		defer releaseSweep()
		g, ctx := errgroup.WithContext(context.Background())
		ready := make(chan error, 1)
		done := make(chan error, 1)
		go func() { done <- p.awaitListen(ctx, g, ready) }()
		select {
		case <-swept:
			t.Fatal("the sweep ran before the proxy listened")
		case <-done:
			t.Fatal("the start went on before the proxy listened")
		case <-time.After(200 * time.Millisecond):
		}
		ready <- nil
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("awaitListen: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the start waited for the sweep")
		}
		select {
		case <-swept:
		case <-time.After(10 * time.Second):
			t.Fatal("the sweep did not run once the proxy listened")
		}
		releaseSweep()
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the listener fails", func(t *testing.T) {
		g, ctx := errgroup.WithContext(context.Background())
		ready := make(chan error, 1)
		bind := errors.New("bind: address already in use")
		ready <- bind
		if err := p.awaitListen(ctx, g, ready); !errors.Is(err, bind) {
			t.Fatalf("awaitListen: %v, want the listener's error", err)
		}
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-swept:
			t.Fatal("the sweep ran for a start whose listener failed")
		default:
		}
	})
}
