package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Structural pins for the no-intercept branch in handleConnection, in the same
// spirit as TestEveryLegacyDispatchFunctionConsultsTheGate: both properties
// regressed silently once, and neither is reachable by a unit harness (the
// branch sits behind the eBPF destination lookup), so the shape itself is
// pinned.

func handleConnectionDecl(t *testing.T) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy.go: %v", err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleConnection" {
			return fset, fd
		}
	}
	t.Fatal("handleConnection not found in proxy.go")
	return nil, nil
}

// firstPos returns the position of the first node inside fn for which match
// returns true, or token.NoPos.
func firstPos(fn *ast.FuncDecl, match func(ast.Node) bool) token.Pos {
	pos := token.NoPos
	ast.Inspect(fn, func(n ast.Node) bool {
		if pos != token.NoPos {
			return false
		}
		if n != nil && match(n) {
			pos = n.Pos()
			return false
		}
		return true
	})
	return pos
}

func isSelector(n ast.Node, sel string) bool {
	s, ok := n.(*ast.SelectorExpr)
	return ok && s.Sel.Name == sel
}

// A destination the client pins a trust store for must never be MITM'd in ANY
// mode. The opportunistic sniff-and-hijack branch returns before every dispatch
// below it, so the no-intercept decision has to come first — with it placed
// after, one per-recording config flag silently reintroduced the HTTPS-by-IP
// crash-loop for apiserver clients.
func TestNoInterceptDecisionOutranksOpportunisticTLS(t *testing.T) {
	_, fn := handleConnectionDecl(t)
	noIntercept := firstPos(fn, func(n ast.Node) bool { return isSelector(n, "matches") })
	opportunistic := firstPos(fn, func(n ast.Node) bool { return isSelector(n, "OpportunisticTLSIntercept") })
	if noIntercept == token.NoPos || opportunistic == token.NoPos {
		t.Fatal("could not locate both dispatch sites in handleConnection")
	}
	if noIntercept > opportunistic {
		t.Fatal("the no-intercept check sits below the OpportunisticTLSIntercept dispatch, which returns first: " +
			"a pinned-trust destination gets hijacked into MITM whenever that flag is on")
	}
}

// The no-intercept relay carries indefinitely-lived streams (API-server
// watches). RelayRawPassthrough's own contract assumes the caller started the
// connCloser so parserCtx cancellation severs the pair; without it, recording
// stop and agent shutdown leak both sockets and two goroutines per watch.
func TestNoInterceptRelayStartsTheConnCloser(t *testing.T) {
	_, fn := handleConnectionDecl(t)
	var branch *ast.IfStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		if branch != nil {
			return false
		}
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		found := false
		ast.Inspect(ifs.Cond, func(c ast.Node) bool {
			if isSelector(c, "matches") {
				found = true
				return false
			}
			return true
		})
		if found {
			branch = ifs
			return false
		}
		return true
	})
	if branch == nil {
		t.Fatal("no-intercept branch not found in handleConnection")
	}
	closer := token.NoPos
	relay := token.NoPos
	ast.Inspect(branch.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == "startConnCloser" && closer == token.NoPos {
				closer = call.Pos()
			}
		case *ast.SelectorExpr:
			if strings.Contains(fun.Sel.Name, "RelayRawPassthrough") && relay == token.NoPos {
				relay = call.Pos()
			}
		}
		return true
	})
	if relay == token.NoPos {
		t.Fatal("RelayRawPassthrough call not found inside the no-intercept branch")
	}
	if closer == token.NoPos {
		t.Fatal("the no-intercept branch never starts the connCloser: its relay cannot be severed by parserCtx cancellation")
	}
	if closer > relay {
		t.Fatal("startConnCloser must run before the relay begins, not after")
	}
}
