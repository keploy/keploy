package supervisor

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A tunnel's calls are not recorded, and that is never silent: ReportTunnel
// says so at WARN once per interval for each host, naming the host and the
// connection, and what follows from it. One held back is said at Debug, and
// the host's next WARN past the interval says how many it stands for. Another
// host is not held back behind it, nor counted under it. As the recording
// itself stops, the connection is torn down with it: it is said at Debug, and
// the limit is not spent on it.
// Not parallel: the WARN limit is process-wide, and this test sets its clock.
func TestATunnelsCallsNotRecordedAreSaidOncePerHost(t *testing.T) {
	clock := time.Unix(7000, 0)
	saved := tunnelWarns
	tunnelWarns = &WarnLimiters{every: tunnelWarnEvery, now: func() time.Time { return clock }}
	t.Cleanup(func() { tunnelWarns = saved })
	core, logs := observer.New(zapcore.DebugLevel)
	s := &Session{Logger: zap.New(core), ClientConnID: "c1"}
	at := func(level zapcore.Level) []observer.LoggedEntry {
		return logs.FilterMessage(TunnelWarnMsg).FilterLevelExact(level).All()
	}

	s.ReportTunnel("api.example:443")
	w := at(zapcore.WarnLevel)
	if len(w) != 1 {
		t.Fatalf("%d WARN lines for the first tunnel, want 1", len(w))
	}
	f := w[0].ContextMap()
	if f["host"] != "api.example:443" || f["connID"] != "c1" {
		t.Fatalf("the WARN names host %v on connection %v, want api.example:443 on c1", f["host"], f["connID"])
	}
	if _, ok := f["sameWarningsHeldBack"]; ok {
		t.Fatal("the first WARN says warnings were held back before it")
	}
	next, _ := f["next_step"].(string)
	for _, want := range []string{"traffic is unaffected", "no call through the tunnel is recorded as a mock",
		"keploy's proxy leaves out the test cases recorded while the connection carries the tunnel's traffic",
		"saves them without these mocks", "no_mocks", "when the CONNECT is its connection's first request"} {
		if !strings.Contains(next, want) {
			t.Errorf("next_step %q does not say %q", next, want)
		}
	}

	// The same host within the interval: held back, at Debug. Another host:
	// its own WARN, counting none of the first's.
	s.ReportTunnel("api.example:443")
	s.ReportTunnel("api.example:443")
	if n, d := len(at(zapcore.WarnLevel)), len(at(zapcore.DebugLevel)); n != 1 || d != 2 {
		t.Fatalf("%d WARN and %d Debug lines after the same host's tunnel twice more within %v, want 1 and 2", n, d, tunnelWarnEvery)
	}
	s.ReportTunnel("other.example:443")
	w = at(zapcore.WarnLevel)
	if len(w) != 2 || w[1].ContextMap()["host"] != "other.example:443" {
		t.Fatalf("%d WARN lines after another host's tunnel, want its own: it was held back behind the first host's", len(w))
	}
	if held, ok := w[1].ContextMap()["sameWarningsHeldBack"]; ok {
		t.Fatalf("the other host's WARN says %v held back: it counts the first host's", held)
	}

	// As the recording stops: at Debug, with the limit not spent.
	done := make(chan struct{})
	close(done)
	stopping := &Session{Logger: zap.New(core), ClientConnID: "c2", RecordingDone: done}
	clock = clock.Add(tunnelWarnEvery)
	stopping.ReportTunnel("api.example:443")
	if n := len(at(zapcore.WarnLevel)); n != 2 {
		t.Fatalf("%d WARN lines after a tunnel as the recording stops, want it at Debug", n)
	}
	d := at(zapcore.DebugLevel)
	if last := d[len(d)-1].ContextMap(); last["connID"] != "c2" || last["recordingStopping"] != true {
		t.Fatalf("the last Debug line is %v, want the tunnel as the recording stops", last)
	}

	// Past the interval, the host's next WARN says how many were held back:
	// the two, not the one as the recording stopped.
	s.ReportTunnel("api.example:443")
	w = at(zapcore.WarnLevel)
	if len(w) != 3 || w[2].ContextMap()["host"] != "api.example:443" {
		t.Fatalf("%d WARN lines past the interval, want the first host's next one", len(w))
	}
	if held := w[2].ContextMap()["sameWarningsHeldBack"]; held != uint64(2) {
		t.Fatalf("the WARN past the interval says %v held back, want the 2 of its host since its last one", held)
	}

	// Nil-safe, and silent with no logger.
	for name, s := range map[string]*Session{"a nil session": nil, "a session with no logger": {}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ReportTunnel on %s panicked: %v", name, r)
				}
			}()
			s.ReportTunnel("api.example:443")
		}()
	}
}
