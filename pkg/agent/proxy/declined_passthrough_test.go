package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const h2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// TestRelayDeclinedConn_RelaysTheWholeStreamIncludingConsumedBytes pins
// the fix for a live user-facing break, and the subtlety that makes a
// naive version of it useless.
//
// A parser that MATCHED and then declined — gRPC claims any HTTP/2
// connection at priority 100, then bails when the destination port is
// outside its allowlist (default "50051,443") — signalled that by
// returning nil. The dispatcher read nil as "handled", logged
// "successfully recorded outgoing message", and handleConnection's
// deferred close tore the socket down. So gRPC on any non-default port had
// its connections DROPPED in record mode.
//
// The subtlety: deciding whether it wants the connection requires the
// parser to READ, and gRPC reads the HTTP/2 preface. It restores those
// bytes by wrapping session.Ingress. Relaying handleConnection's own
// srcConn instead would forward a preface-less stream, and a real server
// answers GOAWAY(PROTOCOL_ERROR) — moving the user's break rather than
// fixing it. This asserts the FULL stream, preface included, reaches
// upstream.
func TestRelayDeclinedConn_RelaysTheWholeStreamIncludingConsumedBytes(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = upstream.Close() }()

	received := make(chan []byte, 1)
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, len(h2Preface)+4)
		n, _ := io.ReadFull(c, buf)
		received <- buf[:n]
	}()

	appSide, proxySide, cleanupPair := tcpConnPair(t)
	defer cleanupPair()
	defer func() { _ = appSide.Close() }()
	dstConn, err := net.DialTimeout("tcp", upstream.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer func() { _ = dstConn.Close() }()

	// The app speaks first, exactly as an h2 client does.
	go func() {
		_, _ = appSide.Write([]byte(h2Preface))
		_, _ = appSide.Write([]byte("POST"))
	}()

	// Reproduce what a declining parser leaves behind: it DRAINED the
	// preface off Ingress, then restored it by wrapping. handleConnection's
	// own srcConn is left positioned past those bytes.
	drained := make([]byte, len(h2Preface))
	if _, err := io.ReadFull(proxySide, drained); err != nil {
		t.Fatalf("simulating the parser's read: %v", err)
	}
	if string(drained) != h2Preface {
		t.Fatalf("drained %q, want the h2 preface", drained)
	}
	// The SAME wrapper production uses (buildRecordSession) — not util.Conn.
	// util.Conn embeds a real net.Conn with real Close and deadlines;
	// SafeConn no-ops both and exposes Unwrap(). A future "unwrap to get at
	// the real socket" edit passes against util.Conn and loses the preface
	// against SafeConn.
	session := &integrations.RecordSession{
		Ingress: util.NewSafeConnWithReader(proxySide, util.NewPrefixReader(drained, proxySide), zap.NewNop()),
		Egress:  util.NewSafeConn(dstConn, zap.NewNop()),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errGrp, gctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(gctx, models.ErrGroupKey, errGrp)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")

	declined := fmt.Errorf("destination port 8080 not in KEPLOY_GRPC_V2_PORTS allowlist: %w",
		integrations.ErrParserDeclined)

	go func() {
		_ = p0().relayDeclinedConn(ctx, declined, zap.NewNop(), integrations.GRPC, session)
	}()

	select {
	case got := <-received:
		if !bytes.Equal(got, []byte(h2Preface+"POST")) {
			t.Fatalf("upstream received %q, want the full stream %q.\nThe parser consumed the "+
				"preface to make its decision and restored it on session.Ingress; relaying "+
				"anything else forwards a truncated stream and the server resets it.",
				got, h2Preface+"POST")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("upstream received nothing within the deadline. Either the declined connection " +
			"was not relayed at all, or it was relayed from a conn positioned PAST the bytes " +
			"the parser consumed — the preface is gone, so the reader below never fills.")
	}

}

// TestDeclineClassification: only a wrapped ErrParserDeclined diverts.
// Classification is deliberately a pure errors.Is at each call site rather
// than folded into relayDeclinedConn — fusing the check with a blocking
// relay meant `false &&` could delete a call site's effect while traffic
// still flowed, so no test could tell an honoured result from an ignored
// one.
func TestDeclineClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"wrapped sentinel is a decline", fmt.Errorf("port: %w", integrations.ErrParserDeclined), true},
		{"nil means the parser handled it", nil, false},
		{"a real error keeps its own handling", errors.New("decode failed"), false},
		{"a wrapped unrelated error is not a decline", fmt.Errorf("ctx: %w", context.Canceled), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errors.Is(tc.err, integrations.ErrParserDeclined); got != tc.want {
				t.Fatalf("errors.Is(%v, ErrParserDeclined) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestEveryLegacyRecordDispatchHandlesDeclines pins EVERY call site, not
// just the one the behavioural test drives.
//
// The behavioural test only exercises recordMySQLOutgoing, so deleting the
// check at the matched-parser site — the path a gRPC parser actually takes,
// and the one carrying the reported bug — left the whole package green. A
// per-function comparison catches it, which is the shape
// dispatch_gate_test.go already converged on for the same class of gap.
func TestEveryLegacyRecordDispatchHandlesDeclines(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	// recordViaSupervisor is exempt: the supervisor turns a parser error
	// into FallthroughToPassthrough itself, with a correctly scoped window.
	const exempt = "recordViaSupervisor"
	exemptSeen := false
	var totalLegacy int

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == exempt {
				exemptSeen = true
				continue
			}
			var legacy, handled int
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if f, ok := call.Fun.(*ast.SelectorExpr); ok && f.Sel.Name == "RecordOutgoing" {
						legacy++
					}
					return true
				}
				ifStmt, ok := n.(*ast.IfStmt)
				if !ok || !bodyCalls(ifStmt.Body, "relayDeclinedConn") {
					return true
				}
				// The call existing is not enough. Counting call
				// EXPRESSIONS is what let `if false && errors.Is(...)`
				// delete the entire fix and stay green — the expression is
				// still there, it just never runs. So require the guard's
				// condition to be EXACTLY errors.Is(x, ErrParserDeclined):
				// a `false &&` or a `!` makes the condition a Binary/Unary
				// expression rather than that call, and stops counting.
				if isDeclineCheck(ifStmt.Cond) {
					handled++
				}
				return true
			})
			// recordMySQLOutgoing also relays a SYNTHESIZED decline (an
			// unregistered parser), guarded by `mysqlParser == nil` rather
			// than by errors.Is. It is a real handled site, so credit it —
			// but only for that one named shape, so the credit cannot
			// silently absorb a deleted errors.Is guard.
			handled += countNilParserDeclines(fn.Body)

			totalLegacy += legacy
			if legacy > 0 && handled < legacy {
				t.Errorf("%s (%s) has %d legacy RecordOutgoing dispatch site(s) but handles a "+
					"decline only %d time(s). A parser that matches and then declines returns "+
					"the sentinel; an unhandled site falls through to the deferred close and "+
					"drops the user's connection. Note a site only counts when its guard is "+
					"exactly errors.Is(err, integrations.ErrParserDeclined) — a `false &&` or a "+
					"negation leaves the call in place but never reaches it.",
					fn.Name.Name, name, legacy, handled)
			}
		}
	}
	if totalLegacy == 0 {
		t.Fatal("found no RecordOutgoing dispatch sites; this pin has stopped looking at anything")
	}
	if !exemptSeen {
		t.Errorf("the %q exemption matched no function — update or drop it", exempt)
	}
}

func p0() *Proxy { return &Proxy{logger: zap.NewNop()} }

// decliningParser matches, then declines — the shape gRPC takes when the
// destination port is outside its allowlist.
type decliningParser struct{ recordingMySQLParser }

func (d *decliningParser) RecordOutgoing(_ context.Context, _ *integrations.RecordSession) error {
	return fmt.Errorf("destination port 8080 not in allowlist: %w", integrations.ErrParserDeclined)
}

// TestRecordMySQLOutgoing_DeclineRelaysRatherThanDropping is the
// BEHAVIOURAL dispatcher pin.
//
// The AST pin below only sees that the call exists — `if false && declined`
// deletes the entire fix and leaves it green, which is the same decoy
// defeat dispatch_gate_test.go already documents. This drives a real
// dispatch function with a parser that declines and asserts the client
// connection is still alive and relaying afterwards, which no source scan
// can express.
func TestRecordMySQLOutgoing_DeclineRelaysRatherThanDropping(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(c, c) // echo
	}()

	parser := &decliningParser{}
	p := &Proxy{
		logger:                     zap.NewNop(),
		Integrations:               map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: parser},
		recordBufferCap:            8 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     5 * time.Second,
		recordBufferHalfCloseGrace: 5 * time.Second,
	}

	appSide, proxySide, cleanupPair := tcpConnPair(t)
	defer cleanupPair()
	defer func() { _ = appSide.Close() }()
	dstConn, err := net.DialTimeout("tcp", upstream.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer func() { _ = dstConn.Close() }()

	src, dst := proxySide, dstConn
	upgrader := util.NewConnTLSUpgrader(&src, &dst, zap.NewNop(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errGrp, gctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(gctx, models.ErrGroupKey, errGrp)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")

	go func() {
		_ = p.recordMySQLOutgoing(ctx, proxySide, dstConn, make(chan *models.Mock, 8), errGrp,
			zap.NewNop(), 1, 2, models.OutgoingOptions{}, upgrader)
	}()

	// If the decline relayed, the echo comes back. If the dispatcher treated
	// the decline as "handled", the caller's deferred close drops this.
	if _, err := appSide.Write([]byte("ping")); err != nil {
		t.Fatalf("app write: %v", err)
	}
	_ = appSide.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(appSide, buf); err != nil {
		t.Fatalf("the declined connection did not relay: %v.\nA parser that declines must leave "+
			"user traffic flowing; dropping the connection is keploy ending one it merely chose "+
			"not to parse.", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("relayed %q, want %q", buf, "ping")
	}
}

// bodyCalls reports whether blk contains a call to the named function.
func bodyCalls(blk *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(blk, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if f, ok := call.Fun.(*ast.SelectorExpr); ok && f.Sel.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// isDeclineCheck reports whether cond is literally
// errors.Is(<anything>, integrations.ErrParserDeclined).
//
// Anything else — a negation, a && with a constant, a different sentinel,
// swapped arguments — is not a decline check, however much it looks like
// one at a glance.
func isDeclineCheck(cond ast.Expr) bool {
	call, ok := cond.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	fn, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || fn.Sel.Name != "Is" {
		return false
	}
	pkg, ok := fn.X.(*ast.Ident)
	if !ok || pkg.Name != "errors" {
		return false
	}
	sentinel, ok := call.Args[1].(*ast.SelectorExpr)
	return ok && sentinel.Sel.Name == "ErrParserDeclined"
}

// countNilParserDeclines counts `if <parser> == nil { ... relayDeclinedConn ... }`
// blocks — the synthesized decline for a build with no parser registered.
func countNilParserDeclines(blk *ast.BlockStmt) int {
	n := 0
	ast.Inspect(blk, func(node ast.Node) bool {
		ifStmt, ok := node.(*ast.IfStmt)
		if !ok || !bodyCalls(ifStmt.Body, "relayDeclinedConn") {
			return true
		}
		bin, ok := ifStmt.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.EQL {
			return true
		}
		if rhs, ok := bin.Y.(*ast.Ident); ok && rhs.Name == "nil" {
			n++
		}
		return true
	})
	return n
}

// TestRelayDeclinedConn_ForwardsTheHalfClose is why the declined relay does
// NOT go through globalPassThrough.
//
// globalPassThrough never calls CloseWrite on either side and returns on
// io.EOF, so a client doing shutdown(SHUT_WR) — the end-of-request signal
// for every EOF-delimited exchange — makes the relay return, and
// handleConnection's deferred close then tears down BOTH sockets. The
// upstream never learns the request ended, never replies, and the user's
// connection dies one round trip later than before. That is the same bug
// this whole change exists to remove, so relaying through a passthrough
// that cannot carry a FIN would have shipped it again.
func TestRelayDeclinedConn_ForwardsTheHalfClose(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = upstream.Close() }()

	replied := make(chan error, 1)
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			replied <- err
			return
		}
		defer func() { _ = c.Close() }()
		// Reads until EOF: it only ever returns if the client's FIN is
		// forwarded through the relay.
		body, err := io.ReadAll(c)
		if err != nil {
			replied <- err
			return
		}
		if string(body) != "request" {
			replied <- fmt.Errorf("upstream got %q, want %q", body, "request")
			return
		}
		_, err = c.Write([]byte("reply"))
		replied <- err
	}()

	appSide, proxySide, cleanupPair := tcpConnPair(t)
	defer cleanupPair()
	defer func() { _ = appSide.Close() }()
	dstConn, err := net.DialTimeout("tcp", upstream.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer func() { _ = dstConn.Close() }()

	session := &integrations.RecordSession{
		Ingress: util.NewSafeConn(proxySide, zap.NewNop()),
		Egress:  util.NewSafeConn(dstConn, zap.NewNop()),
	}

	// A fully populated ctx on purpose. The point of this test is the FIN,
	// so it must not be able to "pass" because some other passthrough
	// implementation panicked on a missing context value before it ever got
	// to the half-close — globalPassThrough does unchecked .(string)
	// assertions on all three of these.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errGrp, gctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(gctx, models.ErrGroupKey, errGrp)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")

	declined := fmt.Errorf("port not in allowlist: %w", integrations.ErrParserDeclined)
	go func() {
		_ = p0().relayDeclinedConn(ctx, declined, zap.NewNop(), integrations.GRPC, session)
	}()

	if _, err := appSide.Write([]byte("request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if tc, ok := appSide.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("half-close: %v", err)
		}
	}

	select {
	case err := <-replied:
		if err != nil {
			t.Fatalf("upstream: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never saw the client's FIN, so it never replied. A declined " +
			"connection relayed through a passthrough that drops half-close ends the user's " +
			"request instead of forwarding it — the same failure this change exists to fix.")
	}

	_ = appSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len("reply"))
	if _, err := io.ReadFull(appSide, got); err != nil {
		t.Fatalf("client never received the reply after half-closing: %v", err)
	}
	if string(got) != "reply" {
		t.Fatalf("client got %q, want %q", got, "reply")
	}
}
