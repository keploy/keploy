package proxy

import (
	"io"
	"testing"
)

// One look of the wait at a target (pairReached) reads what the proxy's
// side received (targetBase.received: on Linux, tcpi_bytes_received, all the
// application wrote once it arrived). The look notes that as the most seen
// and as progress for the watchdog, and the target is reached at exactly
// what was written: not short of it, and not only one byte past it.
func TestOneLookOfTheWaitCountsWhatTheProxysSideReceived(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := io.ReadFull(reader, buf[:10]); err != nil || n != 10 {
		t.Fatalf("the proxy read %d, %v; want the 10 bytes", n, err)
	}
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 || targets[0].want != 10 || targets[0].seen != 0 {
		t.Fatalf("pairTargets = %v, %v; want one of 10 bytes, none seen yet", targets, err)
	}
	target := targets[0]
	before := p.paired.progress.Load()
	reached, err := p.pairReached(target)
	if err != nil || !reached {
		t.Fatalf("pairReached = %v, %v with all 10 bytes received; want reached", reached, err)
	}
	if moved := p.paired.progress.Load() - before; target.seen != 10 || moved != 1 {
		t.Fatalf("the look saw %d bytes and moved the progress %d; want 10 and 1", target.seen, moved)
	}
}
