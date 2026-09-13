package docker

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestAgentPortPublish is the behavioural half: one function, one exact string.
// Everything else in this file exists only to prove that every platform arm
// routes through it.
func TestAgentPortPublish(t *testing.T) {
	t.Parallel()

	if got, want := agentPortPublish(16789), " -p 127.0.0.1:16789:16789"; got != want {
		t.Fatalf("agentPortPublish(16789) = %q, want %q — the agent control plane is unauthenticated (/agent/pcap/keylog streams live TLS session keys), so this publish must reach the host's loopback only", got, want)
	}
}

// TestGetAlias_LinuxPublishesAgentPortToHostLoopbackOnly runs the real linux
// arm end to end. Skipped elsewhere: on darwin/windows getAlias takes a
// different arm that shells out to `docker context ls`, so this would assert
// against a command it was not written for — or fail outright where docker is
// not installed.
func TestGetAlias_LinuxPublishesAgentPortToHostLoopbackOnly(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}

	opts := models.SetupOptions{
		KeployContainer: "keploy-test",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		ClientNSPID:     4242,
		Mode:            models.MODE_RECORD,
	}

	alias, err := getAlias(context.Background(), zap.NewNop(), opts, false)
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}

	want := fmt.Sprintf("-p 127.0.0.1:%d:%d", opts.AgentPort, opts.AgentPort)
	if !strings.Contains(alias, want) {
		t.Errorf("linux getAlias must publish the agent control port to the host's loopback only (want %q).\ngot: %s", want, alias)
	}

	// Presence alone is not the invariant: an alias carrying BOTH forms
	// satisfies the check above while still exposing the control plane.
	unrestricted := fmt.Sprintf("-p %d:%d", opts.AgentPort, opts.AgentPort)
	if strings.Contains(alias, unrestricted) {
		t.Errorf("linux getAlias also published the agent control port on all host interfaces (%q).\ngot: %s", unrestricted, alias)
	}
}

// TestGetAlias_EveryPlatformBranchPublishesAgentPortViaHelper covers the four
// arms that cannot execute here. getAlias switches on runtime.GOOS and each arm
// rebuilds the alias from scratch; on linux the windows/colima, windows/default,
// darwin/colima and darwin/default arms are dead code at runtime, and the
// darwin/windows arms shell out to `docker context ls` before they get
// anywhere. Nothing behavioural reaches them.
//
// Two assertions per branch, and the second is the one that took a review to
// get right. Requiring the branch to CALL agentPortPublish catches an arm that
// forgets to publish; it does not catch an arm that calls it and then appends a
// second, unrestricted "-p <port>:<port>" — which re-exposes the control plane
// while leaving a presence check green. So also require that no arm builds a
// "-p" fragment of its own. That is exact here: the only other publishes in
// getAlias (proxy and app ports) are assembled before the switch.
func TestGetAlias_EveryPlatformBranchPublishesAgentPortViaHelper(t *testing.T) {
	t.Parallel()

	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()

		calls := false
		for _, stmt := range list {
			if stmtCallsFunc(stmt, "agentPortPublish") {
				calls = true
			}
		}
		if !calls {
			t.Errorf("%s: this getAlias platform branch returns an alias without calling agentPortPublish — on that platform the unauthenticated agent control plane (/agent/pcap/keylog streams live TLS session keys) would not be scoped to the host's loopback", pos)
		}

		for _, stmt := range list {
			if lit, ok := stmtAssignsAliasFromLiteralContaining(stmt, "-p "); ok {
				t.Errorf("%s: this getAlias platform branch builds its own %q publish fragment instead of using agentPortPublish. Proxy and app ports are assembled before the switch, so an arm has no reason to; a second publish here would re-expose the agent control plane on every host interface while agentPortPublish is still called", pos, lit)
			}
		}
	})
}

// forEachAliasReturningBranch walks getAlias in util.go and invokes fn once per
// statement list that returns an alias, i.e. once per platform arm. Shared with
// TestGetAlias_EveryPlatformBranchForwardsUpstreamTLSFlags so a future getAlias
// restructure needs one edit rather than two that can drift apart.
func forEachAliasReturningBranch(t *testing.T, fn func(*testing.T, token.Position, []ast.Stmt)) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "util.go", nil, 0)
	if err != nil {
		t.Fatalf("parse util.go: %v", err)
	}

	var decl *ast.FuncDecl
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Recv == nil && fd.Name.Name == "getAlias" {
			decl = fd
			break
		}
	}
	if decl == nil {
		t.Fatal("getAlias not found in util.go; this test needs updating alongside the move")
	}

	// A platform arm is either a switch case body ([]ast.Stmt) or the body of
	// the colima `if` (*ast.BlockStmt), so both shapes have to be walked.
	var lists [][]ast.Stmt
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.BlockStmt:
			lists = append(lists, s.List)
		case *ast.CaseClause:
			lists = append(lists, s.Body)
		}
		return true
	})

	returns := 0
	for _, list := range lists {
		for _, stmt := range list {
			if !isReturnAliasNil(stmt) {
				continue
			}
			returns++
			fn(t, fset.Position(stmt.Pos()), list)
		}
	}

	// linux, windows/colima, windows/default, darwin/colima, darwin/default.
	if returns < 5 {
		t.Fatalf("found only %d alias-returning branches in getAlias, expected at least 5 (linux, windows x2, darwin x2) — the walk is not seeing the switch and would pass vacuously", returns)
	}
}

// stmtCallsFunc reports whether stmt contains a call to the named function.
// Unlike the literal checks below this deliberately descends into nested
// blocks: a call is unambiguous, and a nested arm is walked as its own list
// anyway.
func stmtCallsFunc(stmt ast.Stmt, name string) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
			found = true
		}
		return true
	})
	return found
}

// stmtAssignsAliasFromLiteralContaining reports whether stmt assigns to `alias`
// from a string literal containing sub, returning that literal.
//
// Restricted to assignments, and deliberately NOT descending into nested
// blocks: a plain walk would let the colima `if` body's literals answer for the
// enclosing case body, so an arm could lose its scoping undetected. That was a
// real bug in an earlier version of this file, caught by mutating util.go:340
// and :480. Nested blocks are walked as their own lists, so nothing is missed.
func stmtAssignsAliasFromLiteralContaining(stmt ast.Stmt, sub string) (string, bool) {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok {
		return "", false
	}
	isAlias := false
	for _, lhs := range assign.Lhs {
		if ident, ok := lhs.(*ast.Ident); ok && ident.Name == "alias" {
			isAlias = true
		}
	}
	if !isAlias {
		return "", false
	}
	for _, rhs := range assign.Rhs {
		var hit string
		ast.Inspect(rhs, func(n ast.Node) bool {
			if hit != "" {
				return false
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, sub) {
				hit = lit.Value
			}
			return true
		})
		if hit != "" {
			return hit, true
		}
	}
	return "", false
}
