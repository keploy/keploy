package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A rule with a host and no path passes a TLS connection through when its host
// names the server the client asked for, on the rule's port or any port. A
// rule with a path is the HTTP parser's, which sees each call's path, and a
// port-only rule is the kernel's.
func TestABypassRuleWithAHostPassesItsTLSServerThrough(t *testing.T) {
	rules := []models.BypassRule{
		{Port: 7003},
		{Host: `^api\.example\.com$`, Port: 443},
		{Host: "auth.example.com"},
		{Host: "files.example.com", Path: "/upload", Port: 443},
		{Host: "[", Port: 443},
	}
	for _, tc := range []struct {
		server string
		port   uint
		want   bool
	}{
		{"api.example.com", 443, true},
		{"api.example.com", 8443, false},
		{"web.example.com", 443, false},
		{"auth.example.com", 9443, true},
		{"files.example.com", 443, false},
		{"", 443, false},
		{"", 7003, false},
	} {
		if _, got := bypassByServerName(zap.NewNop(), rules, tc.server, tc.port); got != tc.want {
			t.Errorf("bypassByServerName(%q, %d) = %v, want %v", tc.server, tc.port, got, tc.want)
		}
	}
}

// A connection that shows no host cannot apply a rule with a host for its
// port, nor one with no path a rule with a path. keploy warns of each such
// rule once, naming the port-only rule that passes the whole port through.
func TestABypassRuleAConnectionCannotApplyIsWarnedOfOnce(t *testing.T) {
	rules := []models.BypassRule{
		{Port: 5432},
		{Host: "db.internal", Port: 5432},
		{Path: "/v1", Port: 5432},
		{Host: "other.internal", Port: 6432},
		{Host: "any.internal"},
	}
	for _, tc := range []struct {
		name               string
		hostSeen, pathSeen bool
		want               []models.BypassRule
	}{
		{"plain, not HTTP", false, false, []models.BypassRule{rules[1], rules[2]}},
		{"TLS, not HTTP", true, false, []models.BypassRule{rules[2]}},
		{"HTTP", true, true, nil},
	} {
		got := bypassRulesUnseen(rules, 5432, tc.hostSeen, tc.pathSeen)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: unseen = %+v, want %+v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: unseen = %+v, want %+v", tc.name, got, tc.want)
			}
		}
	}

	bypassRulesWarned.Range(func(k, _ any) bool { bypassRulesWarned.Delete(k); return true })
	t.Cleanup(func() { bypassRulesWarned.Range(func(k, _ any) bool { bypassRulesWarned.Delete(k); return true }) })
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	for i := 0; i < 3; i++ {
		warnBypassRulesUnseen(logger, rules, 5432, "10.0.0.5:5432", false, false)
	}
	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("warnings = %d, want 2 (one for each rule): %v", len(entries), entries)
	}
	for i, want := range []string{`{host: "db.internal", path: "", port: 5432}`, `{host: "", path: "/v1", port: 5432}`} {
		msg := entries[i].Message
		if !strings.Contains(msg, want) || !strings.Contains(msg, "{port: 5432}") || !strings.Contains(msg, "10.0.0.5:5432") {
			t.Errorf("warning %d = %q, want it to name %s, the port-only rule and the destination", i, msg, want)
		}
	}
}

// keploy test passes a TLS connection through to its server when a rule with
// a host names the server the client asked for (its SNI), whatever the
// protocol inside: the proxy dials the server, as it does to record, and
// relays both ways. A connection to a server no rule names is not passed
// through: with no mock for it, it gets no answer and the server is never
// dialled.
func TestKeployTestPassesATLSConnectionARuleNamesThrough(t *testing.T) {
	ln, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSignedForDispatch(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Each connection the server accepts, by its client's address, in the
	// order the kernel queued them.
	accepted := make(chan string, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn.RemoteAddr().String()
			go func() {
				defer func() { _ = conn.Close() }()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte("pong:" + line))
			}()
		}
	}()
	// dials is how many connections reached the server before a marker the
	// test dials now: the server accepts in the order the connections came.
	dials := func(t *testing.T) int {
		marker, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = marker.Close() }()
		for n := 0; ; n++ {
			if awaitEvent(t, accepted, "the server accepting the marker") == marker.LocalAddr().String() {
				return n
			}
		}
	}

	// The client asks for localhost, which resolves, so a connection passed
	// through by mistake would reach the server too.
	for _, tc := range []struct {
		name, ruleHost string
		through        bool
	}{
		{"the rule names the server", "^localhost$", true},
		{"the rule names another server", `^other\.example$`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, src := acceptedConn(t)
			p := replayProxyTo(t, ln.Addr().String(), models.OutgoingOptions{Rules: []models.BypassRule{{Host: tc.ruleHost}}})
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()

			client := tls.Client(app, &tls.Config{ServerName: "localhost", InsecureSkipVerify: true}) //nolint:gosec // keploy's MITM leaf is not under test
			if err := client.Handshake(); err != nil {
				t.Fatalf("client handshake: %v", err)
			}
			// Not HTTP, so only the server-name rule can pass it through.
			if _, err := client.Write([]byte("ping\n")); err != nil {
				t.Fatal(err)
			}
			// With no mock, keploy ends a connection it does not pass
			// through, so the read ends either way.
			type answer struct {
				line string
				err  error
			}
			got := make(chan answer, 1)
			go func() {
				line, err := bufio.NewReader(client).ReadString('\n')
				got <- answer{line, err}
			}()
			a := awaitEvent(t, got, "the client's read ending")
			if tc.through && (a.err != nil || a.line != "pong:ping\n") {
				t.Fatalf("the client got %q, %v; want the server's answer pong:ping", a.line, a.err)
			}
			if !tc.through && a.err == nil {
				t.Fatalf("the client got %q from a server no rule names; want no answer", a.line)
			}
			_ = client.Close()
			_ = awaitEvent(t, done, "handleConnection returning after the client left")
			want := 0
			if tc.through {
				want = 1
			}
			if n := dials(t); n != want {
				t.Fatalf("the server was dialled %d times, want %d", n, want)
			}
		})
	}
}
