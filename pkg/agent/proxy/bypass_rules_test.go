package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// A connection that shows keploy no host cannot decide a rule with a host for
// its port, nor one that shows no path a rule with a path: it is passed through
// for them, as the kernel passed the rule's whole port before. A rule whose
// host the connection shows and does not match is decided, as is a rule for
// another port or any port, a port-only rule (the kernel's) and an HTTP call
// (its parser's). keploy warns of each rule once, naming it, that it passes
// every such connection to the port through, and the port-only rule.
func TestABypassRuleAConnectionCannotDecidePassesItThroughWithOneWarning(t *testing.T) {
	rules := []models.BypassRule{
		{Port: 5432},
		{Host: "db.internal", Port: 5432},
		{Path: "/v1", Port: 5432},
		{Host: `^api\.example$`, Path: "/v1", Port: 5432},
		{Host: `^api\.example$`, Port: 5432},
		{Host: "[", Port: 5432},
		{Host: "other.internal", Port: 6432},
		{Host: "any.internal"},
	}
	for _, tc := range []struct {
		name string
		host string
		http bool
		want []models.BypassRule
	}{
		{"plain, not HTTP", "", false, []models.BypassRule{rules[1], rules[2], rules[3], rules[4]}},
		{"TLS to the rule's host, not HTTP", "api.example", false, []models.BypassRule{rules[2], rules[3]}},
		{"TLS to another host, not HTTP", "web.example", false, []models.BypassRule{rules[2]}},
		{"HTTP", "api.example", true, nil},
	} {
		got := bypassRulesUndecided(zap.NewNop(), rules, 5432, tc.host, tc.http)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: undecided = %+v, want %+v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: undecided = %+v, want %+v", tc.name, got, tc.want)
			}
		}
	}
	if got := bypassRulesUndecided(zap.NewNop(), rules, 7003, "", false); len(got) != 0 {
		t.Fatalf("port 7003: undecided = %+v, want none (no rule names it)", got)
	}

	resetBypassRulesWarned(t)
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	for i := 0; i < 3; i++ {
		warnBypassRulesUndecided(logger, rules[1:3], "10.0.0.5:5432", "")
	}
	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("warnings = %d, want 2 (one for each rule): %v", len(entries), entries)
	}
	for i, want := range []string{`{host: "db.internal", path: "", port: 5432}`, `{host: "", path: "/v1", port: 5432}`} {
		msg := entries[i].Message
		for _, part := range []string{want, "10.0.0.5:5432", "passes every such connection to port 5432 through", "whichever host", "{port: 5432}"} {
			if !strings.Contains(msg, part) {
				t.Errorf("warning %d = %q, want it to say %q", i, msg, part)
			}
		}
	}
}

// A connection that shows its host (a TLS server name) and no path: the
// warning names that host, not every host on the port.
func TestABypassRuleWithAPathOnATLSServerWarnsOfThatServer(t *testing.T) {
	resetBypassRulesWarned(t)
	core, logs := observer.New(zap.WarnLevel)
	warnBypassRulesUndecided(zap.New(core), []models.BypassRule{{Host: `^api\.example$`, Path: "/v1", Port: 443}}, "10.0.0.5:443", "api.example")
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("warnings = %d, want 1: %v", len(entries), entries)
	}
	msg := entries[0].Message
	for _, part := range []string{`{host: "^api\\.example$", path: "/v1", port: 443}`, "passes every such connection to api.example through whole", "{port: 443}"} {
		if !strings.Contains(msg, part) {
			t.Errorf("warning = %q, want it to say %q", msg, part)
		}
	}
	if strings.Contains(msg, "whichever host") {
		t.Errorf("warning = %q, says every host on the port, but the connection showed its host", msg)
	}
}

// resetBypassRulesWarned forgets the rules warned of, now and when t ends.
func resetBypassRulesWarned(t *testing.T) {
	t.Helper()
	reset := func() { bypassRulesWarned.Range(func(k, _ any) bool { bypassRulesWarned.Delete(k); return true }) }
	reset()
	t.Cleanup(reset)
}

// lineServer is a server on 127.0.0.1 that answers each line with
// "pong:"+line, greeting each connection with "hello\n" first when greet
// (a server-first protocol, as MySQL is), wrapped by wrap (TLS) when not nil.
// dials says how many connections reached it since the last call.
func lineServer(t *testing.T, greet bool, wrap func(net.Listener) net.Listener) (addr string, dials func(*testing.T) int) {
	t.Helper()
	raw, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := raw
	if wrap != nil {
		ln = wrap(raw)
	}
	t.Cleanup(func() { _ = ln.Close() })
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
				if greet {
					if _, err := conn.Write([]byte("hello\n")); err != nil {
						return
					}
				}
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte("pong:" + line))
			}()
		}
	}()
	// The server accepts in the order the connections came: dials counts
	// those before a marker dialled now.
	return raw.Addr().String(), func(t *testing.T) int {
		marker, err := net.Dial("tcp4", raw.Addr().String())
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
}

// keploy test passes a connection that shows no host through to its server
// when a bypassRule with a host names its port, as the kernel passed the
// rule's whole port before: a plain connection that is not HTTP, a TLS one to
// the rule's host that is not HTTP (the rule's path unseen), and a MySQL one
// (server-first, so the server's greeting is relayed). keploy warns of the
// rule once. A rule for another port, or for another TLS server, leaves the
// connection to the mocks: with none, it gets no answer and the server is
// never dialled.
func TestKeployTestPassesAConnectionThatShowsNoHostForARuleThrough(t *testing.T) {
	tlsWrap := func(ln net.Listener) net.Listener {
		return tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{selfSignedForDispatch(t)}})
	}
	for _, tc := range []struct {
		name     string
		tls      bool
		mysql    bool
		rule     func(port uint) models.BypassRule
		through  bool
		warnings int
	}{
		{"plain, a host rule for its port", false, false, func(port uint) models.BypassRule { return models.BypassRule{Host: "db.internal", Port: port} }, true, 1},
		{"plain, a path rule for its port", false, false, func(port uint) models.BypassRule { return models.BypassRule{Path: "/v1", Port: port} }, true, 1},
		{"plain, a host rule for another port", false, false, func(port uint) models.BypassRule { return models.BypassRule{Host: "db.internal", Port: port + 1} }, false, 0},
		{"TLS to the rule's host, its path unseen", true, false, func(port uint) models.BypassRule {
			return models.BypassRule{Host: "^localhost$", Path: "/v1", Port: port}
		}, true, 1},
		{"TLS to another host than the rule's", true, false, func(port uint) models.BypassRule {
			return models.BypassRule{Host: `^other\.example$`, Path: "/v1", Port: port}
		}, false, 0},
		{"MySQL, a host rule for its port", false, true, func(port uint) models.BypassRule { return models.BypassRule{Host: "db.internal", Port: port} }, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetBypassRulesWarned(t)
			var wrap func(net.Listener) net.Listener
			if tc.tls {
				wrap = tlsWrap
			}
			addr, dials := lineServer(t, tc.mysql, wrap)
			_, portStr, _ := net.SplitHostPort(addr)
			port, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				t.Fatal(err)
			}
			app, src := acceptedConn(t)
			p := replayProxyTo(t, addr, models.OutgoingOptions{Rules: []models.BypassRule{tc.rule(uint(port))}})
			core, logs := observer.New(zap.WarnLevel)
			p.logger = zap.New(core)
			if tc.mysql {
				p.mysqlPorts.Learn(uint32(port))
			}
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()

			var client net.Conn = app
			if tc.tls {
				c := tls.Client(app, &tls.Config{ServerName: "localhost", InsecureSkipVerify: true}) //nolint:gosec // keploy's MITM leaf is not under test
				if err := c.Handshake(); err != nil {
					t.Fatalf("client handshake: %v", err)
				}
				client = c
			}
			type answer struct {
				lines string
				err   error
			}
			got := make(chan answer, 1)
			go func() {
				r := bufio.NewReader(client)
				var lines string
				if tc.mysql {
					// The client waits to be greeted.
					line, err := r.ReadString('\n')
					lines += line
					if err != nil {
						got <- answer{lines, err}
						return
					}
				}
				// Not HTTP, so no parser sees a host or a path in it.
				if _, err := client.Write([]byte("ping\n")); err != nil {
					got <- answer{lines, err}
					return
				}
				line, err := r.ReadString('\n')
				got <- answer{lines + line, err}
			}()
			a := awaitEvent(t, got, "the client's read ending")
			want := "pong:ping\n"
			if tc.mysql {
				want = "hello\n" + want
			}
			if tc.through && (a.err != nil || a.lines != want) {
				t.Fatalf("the client got %q, %v; want the server's %q", a.lines, a.err, want)
			}
			if !tc.through && a.err == nil {
				t.Fatalf("the client got %q from a server no rule passes through; want no answer", a.lines)
			}
			_ = client.Close()
			_ = awaitEvent(t, done, "handleConnection returning after the client left")
			wantDials := 0
			if tc.through {
				wantDials = 1
			}
			if n := dials(t); n != wantDials {
				t.Fatalf("the server was dialled %d times, want %d", n, wantDials)
			}
			var warned int
			for _, e := range logs.All() {
				if strings.Contains(e.Message, "passes every such connection") {
					warned++
				}
			}
			if warned != tc.warnings {
				t.Fatalf("warnings of the rule = %d, want %d: %v", warned, tc.warnings, logs.All())
			}
		})
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

// keploy record passes a server-first connection (it shows no host) through
// for a bypassRule with a host for its port, as the kernel passed the rule's
// whole port before: not as a protocol keploy cannot record (no such warning,
// and no loss charged under the proven switch), and with the rule's own
// warning, once.
//
// It is looked at so with MySQL detection turned off, and on a port learned
// not to speak MySQL (a client-first server of the port, or another address
// of the rule's host, reached first): dispatched on its client's bytes, it
// waited for a greeting that never came.
func TestKeployRecordPassesAServerFirstConnectionARuleCannotDecideThrough(t *testing.T) {
	for _, tc := range []struct {
		name         string
		noDetection  bool
		learnedOther bool
	}{
		{"MySQL detection on", false, false},
		{"MySQL detection off", true, false},
		{"the port learned not to speak MySQL", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			resetBypassRulesWarned(t)
			served := make(chan string, 8)
			addr, conns, _ := acceptingServer(t, smtpServer(served))
			_, portStr, _ := net.SplitHostPort(addr)
			port, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zap.WarnLevel)
			p := recordingProxy(t, addr, zap.New(core))
			if tc.learnedOther {
				p.mysqlPorts.LearnNotMysql(uint32(port))
			}
			opts := models.OutgoingOptions{Rules: []models.BypassRule{{Host: "mail.internal", Port: uint(port)}}, DisableMysqlAutoDetect: tc.noDetection}
			if err := p.Record(context.Background(), make(chan *models.Mock, 16), opts); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				callServerFirst(t, p, served, false)
			}
			if n := conns.Load(); n != 2 {
				t.Fatalf("the destination got %d connections for 2 calls, want one each", n)
			}
			if said := logs.FilterMessage("keploy cannot record " + addr + notRecorded).All(); len(said) != 0 {
				t.Fatalf("keploy said it cannot record a destination a bypassRule passes through: %v", said)
			}
			if c := serverFirstLoss(p); c.Count != 0 {
				t.Fatalf("a loss was charged for a connection a bypassRule passes through: %+v", c)
			}
			if n := rulesWarned(logs, `{host: "mail.internal", path: "", port: `+portStr+`}`); n != 1 {
				t.Fatalf("warnings of the rule = %d, want 1: %v", n, logs.All())
			}
		})
	}
}

// rulesWarned counts the warnings of the bypassRule ruleText that it passes
// connections through (warnBypassRulesUndecided).
func rulesWarned(logs *observer.ObservedLogs, ruleText string) int {
	var n int
	for _, e := range logs.All() {
		if strings.Contains(e.Message, ruleText) && strings.Contains(e.Message, "passes every such connection") {
			n++
		}
	}
	return n
}

// keploy test passes a connection whose server speaks first through to it when
// a bypassRule with a host names its port and the recording holds no mock
// there, as keploy record passed it, and the kernel the rule's whole port
// before: an SMTP server, with MySQL detection off (replayProxyTo), as a MySQL
// server on a port keploy does not take for MySQL is. Read for its client's
// bytes, it waited for a greeting keploy, with no mock, never sends. Each
// connection reaches the server once, and keploy warns of the rule once.
func TestKeployTestPassesAServerFirstConnectionARuleNamesThrough(t *testing.T) {
	resetBypassRulesWarned(t)
	served := make(chan string, 8)
	addr, conns, _ := acceptingServer(t, smtpServer(served))
	_, portStr, _ := net.SplitHostPort(addr)
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	p := replayProxyTo(t, addr, models.OutgoingOptions{Rules: []models.BypassRule{{Host: "mail.internal", Port: uint(port)}}})
	core, logs := observer.New(zap.WarnLevel)
	p.logger = zap.New(core)
	p.deriveMysqlPorts() // the test set's mocks are stored: none
	for i := 0; i < 2; i++ {
		callServerFirst(t, p, served, false)
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("the server got %d connections for 2 calls, want one each", n)
	}
	if n := rulesWarned(logs, `{host: "mail.internal", path: "", port: `+portStr+`}`); n != 1 {
		t.Fatalf("warnings of the rule = %d, want 1: %v", n, logs.All())
	}
}

// greetThenRead is a server that sends greeting at accept, then reads until
// its client closes.
func greetThenRead(greeting string) func(c net.Conn) {
	return func(c net.Conn) {
		defer func() { _ = c.Close() }()
		if _, err := io.WriteString(c, greeting); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, c)
	}
}

// keploy test looks at the server of a connection to a port a bypassRule with
// a host names only while its client is silent and the recording holds no
// mock at the port, and whichever end speaks first decides. The server, or its
// closing without a word, passes the connection through to it (Dst). The
// client, or a server whose first bytes are a client-first protocol's (an idle
// HTTP server's 408), sends it on to dispatch, the server reached handed on
// (Predialed), with what it sent. A port at which the recording holds a mock
// is not looked at, nor is a port no such rule names, and a server that
// cannot be reached fails the connection. The mocks may be stored after the
// connection came.
func TestKeployTestLooksAtTheServerOfARulePortWithNoMock(t *testing.T) {
	const (
		dispatched = "dispatched"
		passed     = "passed through"
		failed     = "failed"
	)
	for _, tc := range []struct {
		name      string
		serve     func(c net.Conn) // nil: nothing listens
		mocked    bool             // the recording holds a mock at the port
		otherPort bool             // the rule names another port
		late      bool             // the mocks are stored after the connection came
		speak     bool             // the client speaks once the server has accepted
		want      string
		wantConns int32
		wantFirst string // what the server's connection gives first
	}{
		{"the server greets", greetThenRead("220 ready\r\n"), false, false, false, false, passed, 1, "220 ready\r\n"},
		{"the server greets, the mocks stored later", greetThenRead("220 ready\r\n"), false, false, true, false, passed, 1, "220 ready\r\n"},
		{"the server closes without a word", func(c net.Conn) { _ = c.Close() }, false, false, false, false, passed, 1, ""},
		{"the client speaks first", silentUntilAsked, false, false, false, true, dispatched, 1, ""},
		{"an idle HTTP server's 408 first", greetThenRead("HTTP/1.1 408 Request Timeout\r\n"), false, false, false, true, dispatched, 1, "HTTP/1.1 408 Request Timeout\r\n"},
		{"a mock at the port", greetThenRead("220 ready\r\n"), true, false, false, false, dispatched, 0, ""},
		{"a rule for another port", greetThenRead("220 ready\r\n"), false, true, false, false, dispatched, 0, ""},
		{"the server cannot be reached", nil, false, false, false, false, failed, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				addr     string
				conns    *atomic.Int32
				accepted <-chan struct{}
			)
			if tc.serve != nil {
				addr, conns, accepted = acceptingServer(t, tc.serve)
			} else {
				ln, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr = ln.Addr().String()
				_ = ln.Close()
			}
			_, portStr, _ := net.SplitHostPort(addr)
			port, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				t.Fatal(err)
			}
			rulePort := uint(port)
			if tc.otherPort {
				rulePort++
			}
			rules := []models.BypassRule{{Host: "mail.internal", Port: rulePort}}
			p := replayProxyTo(t, addr, models.OutgoingOptions{Rules: rules})
			var mocks []*models.Mock
			if tc.mocked {
				mocks = append(mocks, &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": addr}}})
			}
			if !tc.late {
				p.deriveMysqlPorts(mocks)
			}
			app, src := acceptedConn(t)
			defer func() { _ = app.Close() }()
			type result struct {
				look replayLook
				err  error
			}
			got := make(chan result, 1)
			go func() {
				look, err := p.lookUndecidedReplay(context.Background(), zap.NewNop(), src, addr, nil, uint(port), rules)
				got <- result{look, err}
			}()
			if tc.late {
				p.deriveMysqlPorts(mocks)
			}
			if tc.speak {
				awaitEvent(t, accepted, "the server accepting the look's connection")
				if _, err := io.WriteString(app, "ping\n"); err != nil {
					t.Fatal(err)
				}
			}
			r := awaitEvent(t, got, "the look ending")
			switch tc.want {
			case failed:
				if r.err == nil || r.look.Dst != nil {
					t.Fatalf("got %+v, %v; want the connection failed", r.look, r.err)
				}
			case passed:
				if r.err != nil || r.look.Dst == nil || r.look.Predialed != nil || len(r.look.Undecided) != 1 {
					t.Fatalf("got %+v, %v; want the connection passed through for the rule", r.look, r.err)
				}
				readFirst(t, r.look.Dst, tc.wantFirst)
				_ = r.look.Dst.Close()
			case dispatched:
				if r.err != nil || r.look.Dst != nil {
					t.Fatalf("got %+v, %v; want the connection dispatched", r.look, r.err)
				}
				if tc.speak {
					line, err := bufio.NewReader(r.look.Src).ReadString('\n')
					if err != nil || line != "ping\n" {
						t.Fatalf("dispatch reads %q, %v from the client; want its ping", line, err)
					}
				}
				if (r.look.Predialed != nil) != (tc.wantConns == 1) {
					t.Fatalf("the server reached handed on: %v, want %v", r.look.Predialed != nil, tc.wantConns == 1)
				}
				if r.look.Predialed != nil {
					server := r.look.Predialed.Look(addr)
					if tc.wantFirst != "" {
						readFirst(t, server, tc.wantFirst)
					} else {
						if _, err := io.WriteString(server, "x\n"); err != nil {
							t.Fatal(err)
						}
						readFirst(t, server, "ok x\n")
					}
					_ = server.Close()
				}
			}
			if conns != nil {
				if n := conns.Load(); n != tc.wantConns {
					t.Fatalf("the server got %d connections, want %d", n, tc.wantConns)
				}
			}
		})
	}
}

// readFirst reads from conn what it gives first, want ("" for its close).
func readFirst(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second)) // only so a hang is reported
	buf := make([]byte, len(want)+1)
	n, err := io.ReadAtLeast(conn, buf, max(len(want), 1))
	if want == "" {
		if err != io.EOF {
			t.Fatalf("the server gave %q, %v; want its close", buf[:n], err)
		}
		return
	}
	if err != nil || string(buf[:n]) != want {
		t.Fatalf("the server gave %q, %v; want %q", buf[:n], err, want)
	}
}
