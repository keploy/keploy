package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// The agent runs at debug level in production, and what it logs reaches a
// log store. Every parser it links sees the bytes of the connections it is
// offered: MatchType the first chunk of every outgoing connection, its own
// protocol's or not; the record and replay paths every request and answer of
// its own. Those bytes hold the application's credentials (an Authorization
// header, a cookie, a token in a query string, a database password) and its
// data (bodies, queries, rows). No parser logs any of them, at any level: a
// parser logs facts about the traffic (sizes, kinds, operations, mock names,
// a URL's shape), never the traffic.
//
// This guard drives each parser as the agent does, with a debug-level
// logger, over traffic that carries a made-up secret where the credentials
// and the data go, and fails if any entry logged holds part of the secret,
// in any form bytes are written in (logleak.WindowForms). The HTTP parser
// logged whole request heads, Authorization and all, the MySQL parser every
// packet it served, rows and all.
//
// Every parser keploy registers must have its traffic below (canaryTraffic),
// so a parser added to parsers.go is guarded the day it is.

// canaryStep is one chunk of a connection: the application's (fromClient)
// or the server's.
type canaryStep struct {
	fromClient bool
	bytes      []byte
}

func cli(b []byte) canaryStep { return canaryStep{fromClient: true, bytes: b} }
func srv(b []byte) canaryStep { return canaryStep{bytes: b} }

// canaryExchange is one connection's traffic, in the order it was sent, the
// secret in the application's requests and in the server's answers.
type canaryExchange struct {
	name  string
	port  uint32
	steps []canaryStep
	// broken is an exchange the parser cannot take whole (a malformed
	// head, a connection read from the middle of a message, a command it
	// does not know): its record need make no mock and its replay need not
	// answer, but the parser must have run on it (logged or returned an
	// error), and logged and returned none of it.
	broken bool
	// answered says whether a replay's answer is the recorded one; nil is
	// "it holds the secret" (a compressed answer holds it encoded).
	answered func(got []byte) bool
	// near is the request that differs from the recording by little: nil
	// is every secret written nearSecret (with). An exchange whose secret
	// is a key sets one that keeps the key and drifts a value under it.
	near func(canaryExchange) canaryExchange
	// nearMisses says the near miss is refused even leniently: a drift no
	// noise or learning accepts (a login's user).
	nearMisses bool
	// nearSays is a message the near miss must log: the path it is to
	// drive ran.
	nearSays string
}

// nearMiss is x's near miss (near).
func (x canaryExchange) nearMiss() canaryExchange {
	if x.near != nil {
		return x.near(x)
	}
	return x.with(nearSecret)
}

// replaced is x with old written new in its requests: a value drifted.
func (x canaryExchange) replaced(old, new string) canaryExchange {
	return x.replacedFrom(0, old, new)
}

// replacedFrom is replaced, in the requests from step i on only: a
// connection whose login matches its recording and whose commands drift.
func (x canaryExchange) replacedFrom(from int, old, new string) canaryExchange {
	y := x
	y.steps = make([]canaryStep, len(x.steps))
	for i, s := range x.steps {
		y.steps[i] = s
		if s.fromClient && i >= from {
			y.steps[i].bytes = bytes.ReplaceAll(s.bytes, []byte(old), []byte(new))
		}
	}
	return y
}

// missed reports whether a replay missed: the parser returned an error, or
// logged a line that says no recording matched.
func missed(logs *observer.ObservedLogs, err error) bool {
	if err != nil {
		return true
	}
	for _, e := range logs.All() {
		if missLine.MatchString(e.Message) {
			return true
		}
	}
	return false
}

// proxyMissLine begins the proxy's line for every parser's miss
// (sendMockNotFoundErrorFrom), with a report or without.
const proxyMissLine = "mock mismatch: no matching mock for outgoing call"

// missLine is a message that says no recording matched.
var missLine = regexp.MustCompile(`(?i)\bno matching\b|\bmismatch\b|\bmock miss\b|\bnot matched\b`)

// answeredBy reports whether got is x's recorded answer.
func (x canaryExchange) answeredBy(got []byte) bool {
	if x.answered != nil {
		return x.answered(got)
	}
	return bytes.Contains(got, []byte(canarySecret))
}

func (x canaryExchange) client() [][]byte { return x.side(true) }
func (x canaryExchange) dest() [][]byte   { return x.side(false) }

func (x canaryExchange) side(fromClient bool) [][]byte {
	var out [][]byte
	for _, s := range x.steps {
		if s.fromClient == fromClient {
			out = append(out, s.bytes)
		}
	}
	return out
}

// with is x with every secret in its requests written as other, of the same
// length, so every length a protocol states still holds: the application
// sending what the recording did not.
func (x canaryExchange) with(other string) canaryExchange {
	y := x
	y.steps = make([]canaryStep, len(x.steps))
	for i, s := range x.steps {
		y.steps[i] = s
		if s.fromClient {
			y.steps[i].bytes = bytes.ReplaceAll(s.bytes, []byte(canarySecret), []byte(other))
		}
	}
	return y
}

const canarySecret = logleak.Canary

// nearSecret is another made-up secret, as long as canarySecret: a request
// that holds it where the recording holds canarySecret misses the recording
// by that alone, and drives the parser's mismatch paths, which compare (and
// used to print) the live request and the recorded one.
const nearSecret = "kq9Zp2Lx7Rt8Yb1JmW4cN"

// canaryHTTPRequest is an outgoing HTTP request whose credentials (a bearer
// token, a session cookie, an API key in the query) and body hold the
// secret: what MatchType sees first on every HTTP connection.
func canaryHTTPRequest(method, path, header, body string) []byte {
	return []byte(method + " " + path + " HTTP/1.1\r\nHost: api.example.com\r\n" +
		"Authorization: Bearer " + canarySecret + "\r\n" +
		"Cookie: session=" + canarySecret + "; theme=dark\r\n" +
		"X-Api-Key: " + canarySecret + "\r\n" + header +
		fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)) + body)
}

func canaryHTTPResponse(header, body string) []byte {
	return []byte("HTTP/1.1 200 OK\r\nSet-Cookie: session=" + canarySecret + "; HttpOnly\r\n" + header +
		fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)) + body)
}

// canaryChunkedResponse is a chunked answer whose chunks hold the secret.
func canaryChunkedResponse() []byte {
	part1 := `{"access_token":"` + canarySecret
	part2 := `","refresh_token":"` + canarySecret + `"}`
	return []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" +
		fmt.Sprintf("%x\r\n%s\r\n%x\r\n%s\r\n0\r\n\r\n", len(part1), part1, len(part2), part2))
}

func sortedTypes[V any](m map[integrations.IntegrationType]V) []integrations.IntegrationType {
	out := make([]integrations.IntegrationType, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// registered is every parser this build registers (parsers.go), by name.
func registered(t *testing.T) map[integrations.IntegrationType]*integrations.Parsers {
	t.Helper()
	out := map[integrations.IntegrationType]*integrations.Parsers{}
	for name, p := range integrations.Registered {
		if p != nil && p.Initializer != nil {
			out[name] = p
		}
	}
	if len(out) == 0 {
		t.Fatal("no parser is registered; parsers.go must link them")
	}
	return out
}

// Every parser keploy registers has traffic in canaryTraffic: a parser
// without any would go unguarded.
func TestEveryParserHasCanaryTraffic(t *testing.T) {
	traffic := canaryTraffic(t)
	parsers := registered(t)
	for _, name := range sortedTypes(parsers) {
		if len(traffic[name]) == 0 {
			t.Errorf("parser %q has no traffic in canaryTraffic: add an exchange of its protocol that carries the secret where its credentials and data go, so this guard drives it", name)
		}
	}
	for _, name := range sortedTypes(traffic) {
		if _, ok := parsers[name]; !ok {
			t.Errorf("canaryTraffic has traffic for %q, which no parser registers", name)
		}
		for _, x := range traffic[name] {
			if !holdsSecret(x.client()) || !holdsSecret(x.dest()) {
				t.Errorf("%s/%s: the secret must be in both the requests and the answers", name, x.name)
			}
		}
	}
}

// MatchType is offered the first chunk of every outgoing connection, its own
// protocol's or not, so every parser is offered every chunk of every
// protocol here, both directions, and HTTP requests (what the agent sees
// most), and logs none of them.
func TestNoParserLogsTheChunksMatchTypeIsOffered(t *testing.T) {
	chunks := [][]byte{
		canaryHTTPRequest("POST", "/v1/login", "Content-Type: application/json\r\n", `{"password":"`+canarySecret+`"}`),
		[]byte("GET /v1/me?access_token=" + canarySecret + " HTTP/1.1\r\nHost: api.example.com\r\nCookie: session=" + canarySecret + "\r\n\r\n"),
		// A head split across reads: MatchType's first chunk ends mid-header.
		[]byte("GET /v1/me HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Basic " + canarySecret),
	}
	traffic := canaryTraffic(t)
	for _, name := range sortedTypes(traffic) {
		for _, x := range traffic[name] {
			for _, s := range x.steps {
				chunks = append(chunks, s.bytes)
			}
		}
	}
	for _, name := range sortedTypes(registered(t)) {
		t.Run(string(name), func(t *testing.T) {
			logger, logs := logleak.Observe()
			p := integrations.Registered[name].Initializer(logger)
			took := 0
			for _, c := range chunks {
				if p.MatchType(context.Background(), append([]byte(nil), c...)) {
					took++
				}
			}
			logleak.Check(t, logs, canarySecret, "MatchType")
			// The guard drove each parser's own detection: it took a chunk
			// of its own protocol. The generic parser takes none by design
			// (the proxy falls back to it).
			if took == 0 && name != integrations.GENERIC {
				t.Errorf("MatchType of %s took none of the chunks, its own protocol's included: the guard drove nothing there", name)
			}
		})
	}
}

// Each parser records its own protocol's traffic (from the supervisor's
// chunk streams, and from the sockets where it still can), then replays it
// from what it recorded, replays it with nothing recorded (every request a
// miss), and replays a request that differs from the recording only in its
// secret (a near miss), and logs none of it. Each step must do what it does
// in the agent, or the guard drives nothing: the recording makes mocks, and
// the replay from them answers.
func TestNoParserLogsTheTrafficItRecordsOrReplays(t *testing.T) {
	traffic := canaryTraffic(t)
	parsers := registered(t)
	// What a parser writes past the logger it is given: through zap's
	// global logger and through the standard library's log.
	global, globalLogs := logleak.Observe()
	t.Cleanup(zap.ReplaceGlobals(global))
	var stdlog lockedBuffer
	log.SetOutput(&stdlog)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	t.Cleanup(func() {
		logleak.Check(t, globalLogs, canarySecret, "zap's global logger")
		logleak.Check(t, globalLogs, nearSecret, "zap's global logger")
		for _, s := range []string{canarySecret, nearSecret} {
			if logleak.Holds(stdlog.String(), s) {
				t.Errorf("the standard library's log was written the secret (%d bytes written in all)", len(stdlog.String()))
			}
		}
	})
	for _, name := range sortedTypes(parsers) {
		for _, x := range traffic[name] {
			t.Run(string(name)+"/"+x.name, func(t *testing.T) {
				init := parsers[name].Initializer
				// ran is whether a step drove the parser: it logged, or it
				// returned an error.
				ran := false
				check := func(what string, logs *observer.ObservedLogs, err error, secrets ...string) {
					t.Helper()
					ran = ran || logs.Len() > 0 || err != nil
					for _, s := range secrets {
						logleak.Check(t, logs, s, what)
						if err != nil && logleak.Holds(err.Error(), s) {
							t.Errorf("%s returned an error that holds the secret it was given (%d bytes of error text)", what, len(err.Error()))
						}
					}
					// A line a goroutine of the step wrote after the step
					// returned is looked at once the exchange ends, against
					// this step's logger.
					seen := logs.Len()
					t.Cleanup(func() {
						late := logs.All()[seen:]
						for _, s := range secrets {
							for _, line := range logleak.Find(late, s) {
								t.Errorf("%s logged the secret it was given after it returned: %s", what, line)
							}
						}
					})
				}

				logger, logs := logleak.Observe()
				mocks, err := recordCanary(t, init(logger), logger, x)
				check("the record path", logs, err, canarySecret)
				if len(mocks) == 0 && !x.broken {
					t.Fatal("the record path made no mock of the exchange: the guard drove nothing; fix the exchange")
				}

				logger, logs = logleak.Observe()
				if done, made, err := recordLegacyCanary(t, init(logger), logger, x); done {
					check("the socket record path", logs, err, canarySecret)
					if made == 0 && !x.broken {
						t.Error("the socket record path made no mock of the exchange: the guard drove nothing; fix the exchange")
					}
				}

				logger, logs = logleak.Observe()
				answer, err := replayCanary(t, init(logger), logger, x, mocks, false)
				check("the replay path", logs, err, canarySecret)
				if !x.broken && !x.answeredBy(answer) {
					t.Errorf("the replay path answered %d bytes from the exchange's own recording, not the recorded answer: the guard drove nothing; fix the exchange", len(answer))
				}

				logger, logs = logleak.Observe()
				_, err = replayCanary(t, init(logger), logger, x, nil, false)
				check("the replay path, with no recording to answer from", logs, err, canarySecret)
				if !x.broken && !missed(logs, err) {
					t.Error("the replay path with no recording neither returned nor logged a miss: the guard drove nothing")
				}
				if !x.broken && logs.FilterMessageSnippet(proxyMissLine).Len() == 0 {
					t.Error("the replay path's miss with no recording did not reach the proxy's mismatch line: its logging went unchecked")
				}

				// A near miss, lenient (a drifted value may be served:
				// the drift paths ran), then strict (refused: the strict
				// miss's paths and advice ran).
				for _, strict := range []bool{false, true} {
					near, _ := recordCanary(t, init(zap.NewNop()), zap.NewNop(), x)
					logger, logs = logleak.Observe()
					_, err = replayCanary(t, init(logger), logger, x.nearMiss(), near, strict)
					what := map[bool]string{false: "the replay path, near-missing a recording", true: "the strict replay path, near-missing a recording"}[strict]
					check(what, logs, err, canarySecret, nearSecret)
					mustMiss := (strict || x.nearMisses) && !x.broken
					if mustMiss && !missed(logs, err) {
						t.Errorf("%s served the near miss: its mismatch paths ran for nothing; make the exchange's near miss one the parser refuses", what)
					}
					if x.nearSays != "" && logs.FilterMessageSnippet(x.nearSays).Len() == 0 {
						t.Errorf("%s did not log %q: the path the near miss is to drive did not run", what, x.nearSays)
					}
					if mustMiss && logs.FilterMessageSnippet(proxyMissLine).Len() == 0 {
						t.Errorf("%s: the miss did not reach the proxy's mismatch line: its logging went unchecked", what)
					}
				}

				if !ran {
					t.Error("no step of the exchange logged or returned anything: the guard drove nothing; fix the exchange")
				}
			})
		}
	}
}

func holdsSecret(frames [][]byte) bool {
	for _, b := range frames {
		if bytes.Contains(b, []byte(canarySecret)) {
			return true
		}
	}
	return false
}

var canaryStart = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

type canaryAddr struct{}

func (canaryAddr) Network() string { return "tcp" }
func (canaryAddr) String() string  { return "127.0.0.1:0" }

// recordCanary runs p's record path, the supervisor's (RecordSession.V2),
// over x: step i read at i ms. It returns the mocks p made and the error it
// returned.
func recordCanary(t *testing.T, p integrations.Integrations, logger *zap.Logger, x canaryExchange) ([]*models.Mock, error) {
	t.Helper()
	at := func(ms int) time.Time { return canaryStart.Add(time.Duration(ms) * time.Millisecond) }
	clientCh := make(chan fakeconn.Chunk, len(x.steps))
	destCh := make(chan fakeconn.Chunk, len(x.steps))
	for i, s := range x.steps {
		c := fakeconn.Chunk{ConnSeq: fakeconn.ConnSeqOf(uint64(i + 1)), Bytes: append([]byte(nil), s.bytes...), ReadAt: at(i), WrittenAt: at(i)}
		if s.fromClient {
			c.Dir = fakeconn.FromClient
			clientCh <- c
		} else {
			c.Dir = fakeconn.FromDest
			destCh <- c
		}
	}
	close(clientCh)
	close(destCh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mocks := make(chan *models.Mock, 64)
	dst := &models.ConditionalDstCfg{Addr: fmt.Sprintf("10.0.0.9:%d", x.port), Port: uint(x.port)}
	sess := &supervisor.Session{
		ClientStream: fakeconn.New(clientCh, canaryAddr{}, canaryAddr{}),
		DestStream:   fakeconn.New(destCh, canaryAddr{}, canaryAddr{}),
		Mocks:        mocks,
		Mgr:          syncMock.New(logger),
		Logger:       logger,
		Ctx:          ctx,
		ClientConnID: "canary-1",
		DestConnID:   "canary-1-dst",
		Opts:         models.OutgoingOptions{DstCfg: dst},
		// What the relay says of a connection it has streamed whole: every
		// byte of dir captured before the chunk numbered seq is on dir's
		// stream (relay.Relay.StreamedBefore), so a parser that merges the
		// two directions places each chunk in capture order.
		Order: func(dir fakeconn.Direction, seq uint32) (int64, bool, <-chan struct{}) {
			var end int64
			for i, st := range x.steps {
				if !(fakeconn.Chunk{ConnSeq: fakeconn.ConnSeqOf(uint64(i + 1))}).CapturedBefore(seq) {
					break
				}
				if st.fromClient == (dir == fakeconn.FromClient) {
					end += int64(len(st.bytes))
				}
			}
			return end, true, nil
		},
	}
	done := make(chan error, 1)
	var err error
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("the record path %w: %v", errPanicked, r)
			}
		}()
		done <- p.RecordOutgoing(ctx, &integrations.RecordSession{
			V2:           sess,
			Mocks:        mocks,
			Logger:       logger,
			ClientConnID: sess.ClientConnID,
			DestConnID:   sess.DestConnID,
			Opts:         sess.Opts,
		})
	}()
	select {
	case err = <-done:
		if errors.Is(err, errPanicked) {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the record path did not return once both streams ended")
	}
	close(mocks)
	var got []*models.Mock
	for m := range mocks {
		got = append(got, m)
	}
	return got, err
}

var errPanicked = errors.New("panicked")

// recordLegacyCanary runs p's record path from the sockets (a RecordSession
// with no V2): x's requests come from the application's socket and its
// answers from the server's, each step once the one before it was read. It
// says whether p records that way, how many mocks it made, and the error it
// returned; one that declines (it records only from the supervisor's
// streams) did not run.
func recordLegacyCanary(t *testing.T, p integrations.Integrations, logger *zap.Logger, x canaryExchange) (bool, int, error) {
	t.Helper()
	// Loopback TCP, as the agent's sockets are: the socket path reads
	// their addresses as TCP ones.
	app, ingress := canaryTCPPair(t)
	egress, server := canaryTCPPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	// The agent's connection context carries its error group: the socket
	// path forwards on a goroutine of it.
	group, gctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(gctx, models.ErrGroupKey, group)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "canary-2")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "canary-2-dst")
	// The socket path hands its mocks to the connection's sync manager, as
	// the agent's are; it logs on the observed logger too.
	mgr := syncMock.New(logger)
	ctx = syncMock.NewContext(ctx, mgr)
	mocks := make(chan *models.Mock, 64)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("the socket record path %w: %v", errPanicked, r)
			}
		}()
		done <- p.RecordOutgoing(ctx, &integrations.RecordSession{
			Ingress:      ingress,
			Egress:       egress,
			Mocks:        mocks,
			Logger:       logger,
			ClientConnID: "canary-2",
			DestConnID:   "canary-2-dst",
			Opts:         models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: fmt.Sprintf("10.0.0.9:%d", x.port), Port: uint(x.port)}},
		})
	}()
	appGot, appGrew, appRead := drain(app)
	srvGot, srvGrew, srvRead := drain(server)
	ended := false
	wait := func(got func() []byte, grew <-chan struct{}, want int) {
		bound := time.After(5 * time.Second)
		for len(got()) < want && !ended {
			select {
			case <-grew:
			case err := <-done:
				done <- err
				ended = true
			case <-bound:
				return
			}
		}
	}
	appWant, srvWant := 0, 0
	for _, s := range x.steps {
		if ended {
			break
		}
		if s.fromClient {
			_ = app.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := app.Write(s.bytes); err != nil {
				break
			}
			srvWant += len(s.bytes)
			wait(srvGot, srvGrew, srvWant)
		} else {
			_ = server.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := server.Write(s.bytes); err != nil {
				break
			}
			appWant += len(s.bytes)
			wait(appGot, appGrew, appWant)
		}
	}
	_ = app.Close()
	_ = server.Close()
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the socket record path did not return once both sockets closed")
	}
	waited := make(chan error, 1)
	go func() { waited <- group.Wait() }()
	select {
	case gerr := <-waited:
		if err == nil {
			err = gerr
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the socket record path's goroutines did not end once both sockets closed")
	}
	cancel()
	_ = ingress.Close()
	_ = egress.Close()
	<-appRead
	<-srvRead
	if errors.Is(err, errPanicked) {
		t.Fatal(err)
	}
	close(mocks)
	made := 0
	for range mocks {
		made++
	}
	// A socket path may make its mocks on a goroutine of its own after it
	// returned (generic's createGenericMocksAsync): wait, bounded, for them,
	// so that what it logs while making them is in before the check.
	added := int64(0)
	for bound := time.Now().Add(5 * time.Second); time.Now().Before(bound); time.Sleep(10 * time.Millisecond) {
		if _, _, added, _ = mgr.GetDropStats(); added > 0 || made > 0 || x.broken || errors.Is(err, integrations.ErrParserDeclined) {
			break
		}
	}
	made += int(added)
	return !errors.Is(err, integrations.ErrParserDeclined), made, err
}

// canaryTCPPair is the two ends of a loopback TCP connection.
func canaryTCPPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	other := <-accepted
	if other == nil {
		_ = dialed.Close()
		t.Fatal("the loopback listener accepted nothing")
	}
	t.Cleanup(func() { _ = dialed.Close(); _ = other.Close() })
	return dialed, other
}

// drain reads c until it fails: what it read so far, a signal each time it
// grew, and a channel closed once it stopped.
func drain(c net.Conn) (func() []byte, <-chan struct{}, <-chan struct{}) {
	var mu sync.Mutex
	var got []byte
	grew := make(chan struct{}, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		buf := make([]byte, 64<<10)
		for {
			n, err := c.Read(buf)
			mu.Lock()
			got = append(got, buf[:n]...)
			mu.Unlock()
			select {
			case grew <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
		}
	}()
	return func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), got...)
	}, grew, stopped
}

// replayCanary runs p's replay path over x's requests, from recorded (staged
// as one test's mocks, as the agent stages a test window), and returns what
// p answered. Each request is sent once the answer to the one before it came
// (or a bound passed); with a recording it then waits, up to a bound, for the
// answer to hold the secret before it hangs up.
func replayCanary(t *testing.T, p integrations.Integrations, logger *zap.Logger, x canaryExchange, recorded []*models.Mock, strict bool) ([]byte, error) {
	t.Helper()
	// The mock manager logs on the parser's logger: what it logs of the
	// mocks it serves is checked too.
	mm := NewMockManager(nil, nil, logger)
	defer mm.Close()
	// The proxy takes each miss as the agent's does, reported or returned,
	// and logs its mismatch line on the parser's logger too.
	prx := &Proxy{logger: logger, errChannel: make(chan error, 1024)}
	prx.mockManager = mm
	dst := &models.ConditionalDstCfg{Addr: fmt.Sprintf("10.0.0.9:%d", x.port), Port: uint(x.port)}
	// Named as the agent's mock store names what it saves.
	for i, m := range recorded {
		m.Name = fmt.Sprintf("mock-%d", i)
		m.DeriveLifetime()
	}
	stage := func(from, to time.Time) {
		first := mm.FirstTestWindowStart()
		if first.IsZero() && !from.Equal(models.BaseTime) {
			first = from
		}
		perTest, promoted := pkg.FilterPerTestAndLaxPromotedTierAware(context.Background(), logger, recorded, from, to, false, first)
		mm.SetMocksWithWindow(perTest, promoted, from, to)
	}
	stage(models.BaseTime, time.Now())
	stage(canaryStart.Add(-time.Second), canaryStart.Add(time.Minute))

	app, agent := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "canary-1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "canary-1-dst")
	ctx = models.WithMockMismatchReporter(ctx, func(err error) { prx.sendMockNotFoundErrorFrom(err, 0, 0, dst) })
	served := make(chan struct{})
	var mockErr error
	go func() {
		defer close(served)
		defer func() { _ = agent.Close() }()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("the replay path panicked: %v", r)
			}
		}()
		mockErr = p.MockOutgoing(ctx, agent, dst, mm, models.OutgoingOptions{DstCfg: dst, MockNoiseStrict: strict})
		// As handleConnection does with what a parser returns.
		if mockErr != nil && mockErr != io.EOF && !errors.Is(mockErr, context.Canceled) && !isNetworkClosedErr(mockErr) {
			mockErr = prx.mockFailedFrom(logger, mockErr, 0, 0, dst)
		}
	}()
	got, grew, read := drain(app)

	waitFor := func(ok func([]byte) bool, d time.Duration) {
		bound := time.After(d)
		for !ok(got()) {
			select {
			case <-grew:
			case <-served:
				return
			case <-bound:
				return
			}
		}
	}
	// A parser that stops reading (a miss can end the connection) ends the
	// sends; one that hangs is bounded.
	for i, s := range x.steps {
		if !s.fromClient {
			continue
		}
		n := len(got())
		_ = app.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := app.Write(s.bytes); err != nil {
			break
		}
		if i+1 < len(x.steps) && !x.steps[i+1].fromClient {
			waitFor(func(b []byte) bool { return len(b) > n }, 5*time.Second)
			// A step's answer can come in more than one write: let
			// the rest of it come before the next request.
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(recorded) > 0 && !x.broken {
		waitFor(x.answeredBy, 20*time.Second)
	}
	_ = app.Close()
	select {
	case <-served:
	case <-time.After(20 * time.Second):
		t.Fatal("the replay path did not return once the application hung up")
	}
	<-read
	return got(), mockErr
}

// canaryTraffic is, for each parser keploy registers, traffic of its
// protocol with the secret where its credentials, its keys and its data go,
// and where a server echoes what it refuses; and traffic the parser cannot
// take whole (broken), whose error paths must log none of it either.
func canaryTraffic(t *testing.T) map[integrations.IntegrationType][]canaryExchange {
	t.Helper()
	s := canarySecret
	login := canaryHTTPRequest("POST", "/v1/login?api_key="+s+"&lang=en", "Content-Type: application/json\r\n", `{"user":"app","password":"`+s+`"}`)
	return map[integrations.IntegrationType][]canaryExchange{
		integrations.HTTP: {
			{
				name: "a JSON login with a bearer token, a cookie, an API key and a password, answered with a session cookie and a token",
				port: 443,
				steps: []canaryStep{
					cli(login),
					srv(canaryHTTPResponse("Content-Type: application/json\r\n", `{"token":"`+s+`","ok":true}`)),
				},
			},
			{
				name: "a form post of a secret, answered in chunks",
				port: 80,
				steps: []canaryStep{
					cli(canaryHTTPRequest("POST", "/oauth/token", "Content-Type: application/x-www-form-urlencoded\r\n", "grant_type=password&username=app&password="+s)),
					srv(canaryChunkedResponse()),
				},
				// A form body's values are not checked even strictly: the
				// near miss is at another path too, which nothing serves.
				near: func(x canaryExchange) canaryExchange {
					return x.with(nearSecret).replaced("/oauth/token ", "/oauth/tokex ")
				},
			},
			{
				name: "a GET whose path, query value and query name hold the secret",
				port: 80,
				steps: []canaryStep{
					cli([]byte("GET /v1/reset/" + s + "?access_token=" + s + "&" + s + "=1 HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Basic " + s + "\r\n\r\n")),
					srv(canaryHTTPResponse("Content-Type: text/plain\r\n", s)),
				},
			},
			{
				name: "a session store keyed by the secret, in the body's keys",
				port: 80,
				steps: []canaryStep{
					cli(canaryHTTPRequest("PUT", "/sessions/"+s, "Content-Type: application/json\r\n", `{"sessions":{"`+s+`":{"user":"app"}}}`)),
					srv(canaryHTTPResponse("Content-Type: application/json\r\n", `{"`+s+`":{"saved":true}}`)),
				},
				// The key stays and the value under it drifts: the
				// matcher's advice names the drifted field's path.
				near: func(x canaryExchange) canaryExchange { return x.replaced(`"user":"app"`, `"user":"bob"`) },
			},
			{
				name: "a request head split inside its Authorization header",
				port: 80,
				steps: []canaryStep{
					cli(login[:bytes.Index(login, []byte(s))+8]),
					cli(login[bytes.Index(login, []byte(s))+8:]),
					srv(canaryHTTPResponse("Content-Type: application/json\r\n", `{"token":"`+s+`"}`)),
				},
			},
			{
				name: "a server error that echoes the request's token",
				port: 80,
				steps: []canaryStep{
					cli(login),
					srv([]byte(fmt.Sprintf("HTTP/1.1 401 Unauthorized\r\nContent-Type: text/plain\r\nSet-Cookie: session=%s\r\nContent-Length: %d\r\n\r\ninvalid token %s", s, len("invalid token ")+len(s), s))),
				},
			},
			{
				name: "gzip-compressed bodies that hold the secret",
				port: 80,
				steps: []canaryStep{
					cli(canaryHTTPRequest("POST", "/v1/events", "Content-Type: application/json\r\nContent-Encoding: gzip\r\n", string(gzipped(t, `{"token":"`+s+`"}`)))),
					srv(canaryHTTPResponse("Content-Type: application/json\r\nContent-Encoding: gzip\r\n", string(gzipped(t, `{"refresh_token":"`+s+`"}`)))),
				},
				answered: func(got []byte) bool { return bytes.HasPrefix(got, []byte("HTTP/1.1 200")) },
			},
			{
				name: "a chunked request whose chunks hold the secret",
				port: 80,
				steps: []canaryStep{
					cli([]byte("POST /v1/upload HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer " + s + "\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" +
						fmt.Sprintf("%x\r\n%s\r\n", len(`{"token":"`+s+`"}`), `{"token":"`+s+`"}`) + "0\r\n\r\n")),
					srv(canaryHTTPResponse("Content-Type: application/json\r\n", `{"stored":"`+s+`"}`)),
				},
			},
			{
				name: "a request that waits for 100 Continue before its body",
				port: 80,
				steps: []canaryStep{
					cli([]byte(fmt.Sprintf("POST /v1/upload HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", s, len(`{"token":"`+s+`"}`)))),
					srv([]byte("HTTP/1.1 100 Continue\r\n\r\n")),
					cli([]byte(`{"token":"` + s + `"}`)),
					srv(canaryHTTPResponse("Content-Type: application/json\r\n", `{"stored":"`+s+`"}`)),
				},
			},
			{
				name:   "a malformed response that holds the secret",
				port:   80,
				broken: true,
				steps: []canaryStep{
					cli(login),
					srv([]byte("HTTP/1.1 2O0 " + s + "\r\nSet-Cookie: session=" + s + "\r\n\r\n" + s)),
				},
			},
			{
				name:   "a malformed header line that holds the secret",
				port:   80,
				broken: true,
				steps: []canaryStep{
					cli([]byte("GET /v1/me HTTP/1.1\r\nHost: api.example.com\r\nAuthorization Bearer " + s + "\r\n\r\n")),
					srv([]byte(fmt.Sprintf("HTTP/1.1 400 Bad Request\r\nContent-Length: %d\r\n\r\nbad header %s", len("bad header ")+len(s), s))),
				},
			},
			{
				name:   "a connection read from the middle of a request's body",
				port:   80,
				broken: true,
				steps: []canaryStep{
					cli([]byte("access_token=" + s + "&refresh=1\r\n\r\n")),
					srv(canaryHTTPResponse("Content-Type: text/plain\r\n", s)),
				},
			},
		},
		integrations.MYSQL: {
			mysqlCanary(t, "a login, a query of the secret and the row that holds it",
				"SELECT token FROM sessions WHERE token = '"+s+"'", mysqlRows(t)),
			mysqlCanary(t, "an insert the server refuses, echoing the secret it holds",
				"INSERT INTO sessions (token) VALUES ('"+s+"')", mysqlErr(1, 1062, "23000", "Duplicate entry '"+s+"' for key 'PRIMARY'")),
			mysqlBroken(t),
			mysqlPrepared(t),
			mysqlLoginDrift(t),
		},
		integrations.GENERIC: {
			{
				name: "a text protocol's AUTH, a SET under a key that is the secret and a GET of it",
				port: 7000,
				steps: []canaryStep{
					cli([]byte("AUTH app " + s + "\r\n")),
					srv([]byte("+OK\r\n")),
					cli([]byte("SET session:" + s + " " + s + "\r\n")),
					srv([]byte("+OK\r\n")),
					cli([]byte("GET session:" + s + "\r\n")),
					srv([]byte("$" + fmt.Sprint(len(s)) + "\r\n" + s + "\r\n")),
				},
			},
			{
				name: "a server error that echoes the password it refused",
				port: 7000,
				steps: []canaryStep{
					cli([]byte("AUTH app " + s + "\r\n")),
					srv([]byte("-ERR invalid password '" + s + "'\r\n")),
				},
			},
		},
	}
}

// gzipped is body, gzip-compressed.
func gzipped(t *testing.T, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// mysqlCaps are the capabilities the canary connections agree on.
const mysqlCaps = uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_CONNECT_WITH_DB)

// mysqlLogin is a MySQL connection's start: a greeting, a login whose auth
// response holds the secret, and the server's OK.
func mysqlLogin(t *testing.T) []canaryStep {
	t.Helper()
	ctx := context.Background()
	greeting, err := connphase.EncodeHandshakeV10(ctx, zap.NewNop(), &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a,
		ServerVersion:   "8.0.36-canary",
		ConnectionID:    9,
		AuthPluginData:  append(bytes.Repeat([]byte{0x2a}, 20), 0x00),
		CapabilityFlags: mysqlCaps,
		CharacterSet:    0x21,
		StatusFlags:     0x02,
		AuthPluginName:  string(mysql.Native),
	})
	if err != nil {
		t.Fatal(err)
	}
	login, err := connphase.EncodeHandshakeResponse41(ctx, zap.NewNop(), &mysql.HandshakeResponse41Packet{
		CapabilityFlags: mysqlCaps,
		MaxPacketSize:   1 << 24,
		CharacterSet:    0x21,
		// The user is half of the login: the secret stands for it too.
		Username: canarySecret,
		// A native password's scramble is as good as the password to a
		// dictionary attack: the secret's first 20 bytes stand for it.
		AuthResponse:   []byte(canarySecret[:20]),
		Database:       "shop",
		AuthPluginName: string(mysql.Native),
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := phase.EncodeOk(ctx, &mysql.OKPacket{Header: mysql.OK, StatusFlags: 2}, mysqlCaps)
	if err != nil {
		t.Fatal(err)
	}
	return []canaryStep{srv(mysqlPacket(greeting, 0)), cli(mysqlPacket(login, 1)), srv(mysqlPacket(ok, 2))}
}

// mysqlLoginDrift is a login and a query whose near miss changes the login's
// user alone: the handshake's mismatch path runs, and its miss must reach
// the proxy's mismatch line, in both modes.
func mysqlLoginDrift(t *testing.T) canaryExchange {
	t.Helper()
	x := mysqlCanary(t, "a login whose user drifted from the recorded one", "SELECT 1", mysqlRows(t))
	x.nearMisses = true
	x.nearSays = "no mysql mocks matched the HandshakeResponse41"
	x.near = func(x canaryExchange) canaryExchange {
		y := x
		y.steps = append([]canaryStep(nil), x.steps...)
		const login = 1 // mysqlLogin's client step
		y.steps[login].bytes = bytes.ReplaceAll(x.steps[login].bytes, []byte(canarySecret), []byte(nearSecret))
		return y
	}
	return x
}

// mysqlLoginSteps is how many steps mysqlLogin is.
const mysqlLoginSteps = 3

// mysqlCanary is a MySQL connection: a login (mysqlLogin), then query and
// the server's answer to it.
func mysqlCanary(t *testing.T, name, query string, answer []byte) canaryExchange {
	t.Helper()
	steps := append(mysqlLogin(t),
		cli(mysqlPacket(append([]byte{mysql.COM_QUERY}, query...), 0)),
		srv(answer))
	return canaryExchange{name: name, port: 3306, steps: steps, near: mysqlNearMiss}
}

// mysqlNearMiss is a MySQL exchange whose login is the recorded one and
// whose commands hold another secret: the query and prepared-statement
// mismatch paths run, not the handshake's.
func mysqlNearMiss(x canaryExchange) canaryExchange {
	return x.replacedFrom(mysqlLoginSteps, canarySecret, nearSecret)
}

// mysqlRows is a text result set of one column whose one row is the secret.
func mysqlRows(t *testing.T) []byte {
	t.Helper()
	col, err := rowscols.EncodeColumn(context.Background(), zap.NewNop(), &mysql.ColumnDefinition41{Catalog: "def", Schema: "shop", Table: "sessions", OrgTable: "sessions",
		Name: "token", OrgName: "token", FixedLength: 0x0c, CharacterSet: 0x21, ColumnLength: 1020, Type: byte(mysql.FieldTypeVarString)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	eof := []byte{0xfe, 0x00, 0x00, 0x02, 0x00}
	var rows []byte
	rows = append(rows, mysqlPacket([]byte{1}, 1)...)
	rows = append(rows, mysqlPacket(col[4:], 2)...)
	rows = append(rows, mysqlPacket(eof, 3)...)
	rows = append(rows, mysqlPacket(append([]byte{byte(len(canarySecret))}, canarySecret...), 4)...)
	rows = append(rows, mysqlPacket(eof, 5)...)
	return rows
}

// mysqlErr is an ERR packet: its code, SQL state and message.
func mysqlErr(seq byte, code uint16, state, msg string) []byte {
	p := []byte{0xff, byte(code), byte(code >> 8), '#'}
	p = append(p, state...)
	return mysqlPacket(append(p, msg...), seq)
}

// mysqlPrepared is a login, a statement prepared with a literal that is the
// secret, executed with the secret bound to its parameter, answered with a
// binary row that holds it.
func mysqlPrepared(t *testing.T) canaryExchange {
	t.Helper()
	ctx := context.Background()
	column := func(name string) []byte {
		col, err := rowscols.EncodeColumn(ctx, zap.NewNop(), &mysql.ColumnDefinition41{Catalog: "def", Schema: "shop", Table: "sessions", OrgTable: "sessions",
			Name: name, OrgName: name, FixedLength: 0x0c, CharacterSet: 0x21, ColumnLength: 1020, Type: byte(mysql.FieldTypeVarString)}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return col[4:]
	}
	eof := []byte{0xfe, 0x00, 0x00, 0x02, 0x00}
	query := "SELECT token FROM sessions WHERE token = ? AND note = '" + canarySecret + "'"
	prepare := append([]byte{mysql.COM_STMT_PREPARE}, query...)
	// PREPARE_OK: statement 1, one column, one parameter.
	var prepared []byte
	prepared = append(prepared, mysqlPacket([]byte{0x00, 1, 0, 0, 0, 1, 0, 1, 0, 0x00, 0, 0}, 1)...)
	prepared = append(prepared, mysqlPacket(column("?"), 2)...)
	prepared = append(prepared, mysqlPacket(eof, 3)...)
	prepared = append(prepared, mysqlPacket(column("token"), 4)...)
	prepared = append(prepared, mysqlPacket(eof, 5)...)
	// EXECUTE of statement 1, no cursor, once; the parameter's NULL bitmap,
	// its type sent (VAR_STRING), then its value.
	execute := []byte{mysql.COM_STMT_EXECUTE, 1, 0, 0, 0, 0x00, 1, 0, 0, 0, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00}
	execute = append(execute, byte(len(canarySecret)))
	execute = append(execute, canarySecret...)
	// A binary result set of one row: its OK header, NULL bitmap, value.
	var rows []byte
	rows = append(rows, mysqlPacket([]byte{1}, 1)...)
	rows = append(rows, mysqlPacket(column("token"), 2)...)
	rows = append(rows, mysqlPacket(eof, 3)...)
	rows = append(rows, mysqlPacket(append([]byte{0x00, 0x00, byte(len(canarySecret))}, canarySecret...), 4)...)
	rows = append(rows, mysqlPacket(eof, 5)...)
	steps := append(mysqlLogin(t),
		cli(mysqlPacket(prepare, 0)), srv(prepared),
		cli(mysqlPacket(execute, 0)), srv(rows))
	return canaryExchange{name: "a statement prepared with the secret, executed with it bound, and the row that holds it", port: 3306, steps: steps, near: mysqlNearMiss}
}

// mysqlBroken is a login, then a command keploy does not know
// (COM_REGISTER_SLAVE, whose body holds a replica's password), refused by the
// server with an error that echoes it.
func mysqlBroken(t *testing.T) canaryExchange {
	t.Helper()
	cmd := []byte{0x15, 1, 0, 0, 0}
	cmd = append(cmd, byte(len("replica")))
	cmd = append(cmd, "replica"...)
	cmd = append(cmd, byte(len("repl")))
	cmd = append(cmd, "repl"...)
	cmd = append(cmd, byte(len(canarySecret)))
	cmd = append(cmd, canarySecret...)
	steps := append(mysqlLogin(t),
		cli(mysqlPacket(cmd, 0)),
		srv(mysqlErr(1, 1045, "28000", "Access denied for replica password '"+canarySecret+"'")))
	return canaryExchange{name: "a command keploy does not know, holding a replica's password", port: 3306, steps: steps, broken: true}
}

// mysqlPacket is payload with its packet header: its length and seq.
func mysqlPacket(payload []byte, seq byte) []byte {
	n := len(payload)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, payload...)
}

// lockedBuffer is a bytes.Buffer the standard library's log may write from
// any goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
