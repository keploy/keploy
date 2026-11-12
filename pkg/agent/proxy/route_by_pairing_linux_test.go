//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// With the switch off, a proxy whose agent settles no read is counted from
// its start once the kernel counted its canary, whether or not the bytes on
// its paired connections can be read, since nothing of its waits for them:
// each connection goes where the kernel's pairing in its own socket says,
// never where the by-port map says (a source port another connection
// reused), and no paired connection is held for its bytes. One whose agent
// settles reads still needs those bytes to be counted, as before.
func TestAProxyThatSettlesNoReadRoutesByThePairingAndHoldsNothing(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	for _, settles := range []bool{false, true} {
		right := namedUpstream(t, "right")
		wrong := namedUpstream(t, "wrong")
		cfg := config.New()
		cfg.ProxyPort = 0
		cfg.Agent.Mode = models.MODE_RECORD // with no session yet: a call passes through
		cfg.Agent.SettleReads = settles
		core, logs := observer.New(zap.WarnLevel)
		p := New(zap.New(core), fakeDestInfo{dest: ipv4Dest(t, wrong)}, cfg)
		if err := p.InitIntegrations(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.SetListenerCounter(&fakeCounter{dst: ipv4Dest(t, right)})
		p.paired.fail(errors.New("no sock_diag here"))
		ctx, cancel := context.WithCancel(context.Background())
		ready, done := make(chan error, 1), make(chan error, 1)
		go func() { done <- p.Serve(ctx, ready) }()
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		want := "right"
		if settles {
			want = "wrong" // not counted: routed by its source port, as before
		}
		if p.CountedFromStart() == settles || p.usesPairing() == settles || p.keepsConnections() {
			t.Fatalf("settles %v: CountedFromStart %v, usesPairing %v, keepsConnections %v", settles, p.CountedFromStart(), p.usesPairing(), p.keepsConnections())
		}
		for i := 0; i < 2; i++ {
			if got := callThrough(t, p.Listener.Addr().String()); got != want {
				t.Fatalf("settles %v: call %d reached %q, want %q", settles, i, got, want)
			}
		}
		p.paired.mu.Lock()
		held := p.paired.byConn != nil
		p.paired.mu.Unlock()
		if held {
			t.Fatalf("settles %v: a paired connection was held for its bytes by a proxy that keeps none", settles)
		}
		// The proxy's WARN for a call passed through before any session is
		// not the pairing's.
		warns := logs.Filter(func(e observer.LoggedEntry) bool { return !strings.Contains(e.Message, "no session yet") })
		if !settles && warns.Len() != 0 {
			t.Fatalf("a proxy that only routes by the pairing warned: %v", warns.All())
		}
		cancel()
		<-done
	}
}

// What a proxy says when it cannot count, cannot read its paired bytes, or
// finds a NAT'd connection is a WARN only where a read waits for its calls
// (the switch, or an agent that settles reads). A proxy that only routes by
// the pairing says it lower: it goes on routing as it always did.
func TestAProxyThatOnlyRoutesByThePairingSaysWhyItCannotBelowAWarn(t *testing.T) {
	for _, tc := range []struct {
		name, proven string
		settles      bool
		want         zapcore.Level // the lowest level any of the lines may have
	}{
		{"switch off, settles no read", "", false, zapcore.DebugLevel},
		{"switch off, settles reads", "", true, zapcore.WarnLevel},
		{"switch on", "1", false, zapcore.WarnLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", tc.proven)
			cfg := config.New()
			cfg.Agent.SettleReads = tc.settles
			core, logs := observer.New(zap.DebugLevel)
			p := New(zap.New(core), plainDestInfo{}, cfg)
			p.warnUncounted(errors.New("no mark here"))
			p.pairedBytesUnsupported(errors.New("no sock_diag here"))
			if tc.proven == "" {
				p.countedFromStart.Store(true)
				app, _ := net.Pipe()
				defer func() { _ = app.Close() }()
				if !p.dropCounting(app) {
					t.Fatal("a counted proxy did not drop its count at a NAT'd connection")
				}
			}
			if logs.Len() == 0 {
				t.Fatal("nothing said")
			}
			for _, e := range logs.All() {
				if tc.want == zapcore.WarnLevel && e.Level != zapcore.WarnLevel {
					t.Fatalf("%q at %v, want a WARN: a read waits for these calls", e.Message, e.Level)
				}
				if tc.want != zapcore.WarnLevel && e.Level >= zapcore.WarnLevel {
					t.Fatalf("%q at %v, want below a WARN: no read waits for these calls", e.Message, e.Level)
				}
			}
		})
	}
}

// oneNATCounter is fakeCounter whose kernel tied the application's nat-th
// connection to none of the application's (a NAT between them), and paired
// every other one with dst.
type oneNATCounter struct {
	fakeCounter
	nat uint64
}

func (f *oneNATCounter) ProxyConnPair(rc syscall.RawConn) (agent.ProxyConnPairing, error) {
	p, err := f.fakeCounter.ProxyConnPair(rc)
	if err == nil && p.AppSeq == f.nat {
		p.AppSeq, p.Dst = 0, nil
	}
	return p, err
}

// With the switch off, one NAT'd connection among paired ones costs only
// itself its pairing: it is routed by its source port, as it always was, and
// each connection before and after it still goes where its own pairing says,
// never where the by-port map says. Whether or not the agent settles reads:
// one that does loses its count (CountingLost) all the same, with its WARN;
// one that does not says it once, below a WARN.
func TestOneNATdConnectionAmongPairedOnesLosesOnlyItsOwnPairing(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	for _, settles := range []bool{false, true} {
		right := namedUpstream(t, "right")
		wrong := namedUpstream(t, "wrong")
		f := &oneNATCounter{fakeCounter: fakeCounter{dst: ipv4Dest(t, right)}, nat: 2}
		cfg := config.New()
		cfg.ProxyPort = 0
		cfg.Agent.Mode = models.MODE_RECORD // with no session yet: a call passes through
		cfg.Agent.SettleReads = settles
		core, logs := observer.New(zap.InfoLevel)
		p := New(zap.New(core), fakeDestInfo{dest: ipv4Dest(t, wrong)}, cfg)
		if err := p.InitIntegrations(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.SetListenerCounter(f)
		ctx, cancel := context.WithCancel(context.Background())
		ready, done := make(chan error, 1), make(chan error, 1)
		go func() { done <- p.Serve(ctx, ready) }()
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		if !p.CountedFromStart() {
			t.Fatalf("settles %v: not counted from the start", settles)
		}
		for i, want := range []string{"right", "wrong", "right", "right"} {
			if got := callThrough(t, p.Listener.Addr().String()); got != want {
				t.Fatalf("settles %v: call %d of 4 reached %q, want %q (the 2nd is the NAT'd one, routed by its source port)", settles, i+1, got, want)
			}
		}
		if _, lost := p.CountingLost(); lost == nil {
			t.Fatalf("settles %v: the NAT'd connection did not drop the run's count", settles)
		}
		nat := logs.FilterMessageSnippet("could not tie to one of the application's").Len() +
			logs.FilterMessageSnippet("NAT'd connection it cannot attribute").Len()
		warns := logs.FilterLevelExact(zap.WarnLevel).Filter(func(e observer.LoggedEntry) bool {
			return !strings.Contains(e.Message, "no session yet")
		}).Len()
		if nat != 1 || (warns == 1) != settles {
			t.Fatalf("settles %v: said the NAT %d times, %d WARNs; want it said once, a WARN only where a read waits", settles, nat, warns)
		}
		cancel()
		<-done
	}
}
