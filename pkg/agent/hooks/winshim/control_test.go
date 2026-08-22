//go:build windows && amd64

package winshim

import (
	"testing"

	"go.uber.org/zap"
)

// fakeDecider records what the control server asked of it and replies with
// whatever the test set up.
type fakeDecider struct {
	helloPID  uint32
	helloProg string
	helloSeen int

	connectSrc     uint16
	connectVersion uint32
	connectIP      string
	connectPort    uint16
	proxyPort      uint16
	proxyOK        bool

	bindPID  uint32
	bindPort uint16
	newPort  uint16

	listenPID   uint32
	listenOrig  uint16
	listenMoved uint16
	listenSeen  int
}

func (f *fakeDecider) onHello(pid uint32, prog string) {
	f.helloPID, f.helloProg = pid, prog
	f.helloSeen++
}

func (f *fakeDecider) onConnect(srcPort uint16, version uint32, destIP string, destPort uint16) (uint16, bool) {
	f.connectSrc, f.connectVersion, f.connectIP, f.connectPort = srcPort, version, destIP, destPort
	return f.proxyPort, f.proxyOK
}

func (f *fakeDecider) onBind(pid uint32, origPort uint16) uint16 {
	f.bindPID, f.bindPort = pid, origPort
	return f.newPort
}

func (f *fakeDecider) onListen(pid uint32, origPort, movedPort uint16) {
	f.listenPID, f.listenOrig, f.listenMoved = pid, origPort, movedPort
	f.listenSeen++
}

func newTestServer(d controlDecider) *controlServer {
	return &controlServer{logger: zap.NewNop(), decider: d}
}

func TestDispatchConnect(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		proxy uint16
		ok    bool
		want  string
	}{
		{"redirects", "CONNECT 51000 4 93.184.216.34 80", 16789, true, "OK 16789"},
		{"bypasses when the decider declines", "CONNECT 51000 4 93.184.216.34 80", 0, false, ReplyBypass},
		{"bypasses a zero proxy port", "CONNECT 51000 4 93.184.216.34 80", 0, true, ReplyBypass},
		{"bypasses a malformed destination", "CONNECT 51000 4 not-an-ip 80", 16789, true, ReplyBypass},
		{"bypasses an unknown ip version", "CONNECT 51000 5 93.184.216.34 80", 16789, true, ReplyBypass},
		{"bypasses a zero source port", "CONNECT 0 4 93.184.216.34 80", 16789, true, ReplyBypass},
		{"bypasses a zero destination port", "CONNECT 51000 4 93.184.216.34 0", 16789, true, ReplyBypass},
		{"bypasses a short request", "CONNECT 51000 4 93.184.216.34", 16789, true, ReplyBypass},
		{"handles IPv6", "CONNECT 51000 6 2606:2800:220:1:248:1893:25c8:1946 443", 16789, true, "OK 16789"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDecider{proxyPort: tc.proxy, proxyOK: tc.ok}
			if got := newTestServer(d).dispatch(tc.line); got != tc.want {
				t.Fatalf("dispatch(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestDispatchConnectPassesFieldsThrough(t *testing.T) {
	d := &fakeDecider{proxyPort: 16789, proxyOK: true}
	newTestServer(d).dispatch("CONNECT 51000 4 93.184.216.34 8080")

	if d.connectSrc != 51000 || d.connectVersion != 4 || d.connectIP != "93.184.216.34" || d.connectPort != 8080 {
		t.Fatalf("decider saw src=%d version=%d ip=%s port=%d",
			d.connectSrc, d.connectVersion, d.connectIP, d.connectPort)
	}
}

func TestDispatchHello(t *testing.T) {
	d := &fakeDecider{}
	if got := newTestServer(d).dispatch("HELLO 4242 app.exe"); got != ReplyOK {
		t.Fatalf("dispatch(HELLO) = %q, want %q", got, ReplyOK)
	}
	if d.helloPID != 4242 || d.helloProg != "app.exe" {
		t.Fatalf("decider saw pid=%d prog=%q", d.helloPID, d.helloProg)
	}

	// The program name is best-effort and may be absent.
	d2 := &fakeDecider{}
	if got := newTestServer(d2).dispatch("HELLO 7"); got != ReplyOK {
		t.Fatalf("dispatch(HELLO without a program) = %q, want %q", got, ReplyOK)
	}
	if d2.helloPID != 7 {
		t.Fatalf("decider saw pid=%d, want 7", d2.helloPID)
	}
}

func TestDispatchBind(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		newPort uint16
		want    string
	}{
		{"moves the listener", "BIND 4242 8080", 41234, "PORT 41234"},
		{"keeps it when the decider declines", "BIND 4242 8080", 0, ReplyKeep},
		{"keeps it on a zero port", "BIND 4242 0", 41234, ReplyKeep},
		{"keeps it on a short request", "BIND 4242", 41234, ReplyKeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDecider{newPort: tc.newPort}
			if got := newTestServer(d).dispatch(tc.line); got != tc.want {
				t.Fatalf("dispatch(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestDispatchListen(t *testing.T) {
	d := &fakeDecider{}
	if got := newTestServer(d).dispatch("LISTEN 4242 8080 41234"); got != ReplyOK {
		t.Fatalf("dispatch(LISTEN) = %q, want %q", got, ReplyOK)
	}
	if d.listenPID != 4242 || d.listenOrig != 8080 || d.listenMoved != 41234 {
		t.Fatalf("decider saw pid=%d orig=%d moved=%d", d.listenPID, d.listenOrig, d.listenMoved)
	}

	// A malformed LISTEN must still be answered OK — the socket is already
	// listening, and there is nothing the shim could do with a refusal.
	d2 := &fakeDecider{}
	if got := newTestServer(d2).dispatch("LISTEN 4242 8080"); got != ReplyOK {
		t.Fatalf("dispatch(short LISTEN) = %q, want %q", got, ReplyOK)
	}
	if d2.listenSeen != 0 {
		t.Fatal("a malformed LISTEN reached the decider")
	}
}

// An unrecognised verb must degrade to leaving the application's traffic alone,
// so that a version-skewed shim cannot break a run.
func TestDispatchUnknownVerbBypasses(t *testing.T) {
	for _, line := range []string{"", "   ", "WAT", "CONNECTX 1 4 1.2.3.4 80"} {
		if got := newTestServer(&fakeDecider{}).dispatch(line); got != ReplyBypass {
			t.Fatalf("dispatch(%q) = %q, want %q", line, got, ReplyBypass)
		}
	}
}
