package relay

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoRawNetConnWriteInRelayPackage enforces by construction that every
// peer socket Write and CloseWrite in pkg/agent/proxy/relay routes through
// trackedPeerConn (and therefore peerWriteState):
//   - Relay.src and Relay.dst must both have type atomic.Pointer[trackedPeerConn].
//   - No non-test source file in pkg/agent/proxy/relay may call .Write,
//     .CloseWrite, .ReadFrom, or io.Copy/io.WriteString on a raw net.Conn
//     outside the single tracked chokepoints and low-level socket wrappers.
func TestNoRawNetConnWriteInRelayPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read relay package dir: %v", err)
	}

	fset := token.NewFileSet()
	foundSrcField := false
	foundDstField := false

	// Low-level chokepoints that legitimately invoke an underlying Write or
	// CloseWrite on a raw net.Conn / *net.TCPConn / zapcore.CheckedEntry.
	allowRawWrite := map[string]bool{
		"(*peerWriteState).write":     true, // the single relay write chokepoint
		"(*writeCount).write":         true, // accept-time byte counter beneath Relay
		"publishRealCertFromUpgraded": true, // zapcore.CheckedEntry.Write
	}
	allowRawCloseWrite := map[string]bool{
		"closeWrite":                          true, // the single relay half-close chokepoint
		"(*countedClient).CloseWrite":         true, // accept-time wrapper beneath Relay
		"(*readTrackingConn).CloseWrite":      true, // TLS read-time wrapper delegation
		"(*readTimeReportingConn).CloseWrite": true, // TLS read-time wrapper delegation
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}

		for _, decl := range file.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name.Name != "Relay" {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, field := range st.Fields.List {
						for _, ident := range field.Names {
							switch ident.Name {
							case "src":
								foundSrcField = true
								if got := exprString(field.Type); got != "atomic.Pointer[trackedPeerConn]" {
									t.Errorf("%s: Relay.src has type %q, want atomic.Pointer[trackedPeerConn]",
										fset.Position(field.Pos()), got)
								}
							case "dst":
								foundDstField = true
								if got := exprString(field.Type); got != "atomic.Pointer[trackedPeerConn]" {
									t.Errorf("%s: Relay.dst has type %q, want atomic.Pointer[trackedPeerConn]",
										fset.Position(field.Pos()), got)
								}
							}
						}
					}
				}
			}

			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fnKey := funcDeclKey(fn)
			trackedIdents := collectTrackedPeerConnIdents(fn)

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pos := fset.Position(call.Pos())

				switch sel.Sel.Name {
				case "Write":
					if allowRawWrite[fnKey] {
						return true
					}
					if !isTrackedPeerConnExpr(sel.X, trackedIdents) {
						t.Errorf("%s: in %s: raw Write on %q bypasses *trackedPeerConn / peerWriteState",
							pos, fnKey, exprString(sel.X))
					}
				case "CloseWrite":
					if allowRawCloseWrite[fnKey] {
						return true
					}
					if !isTrackedPeerConnExpr(sel.X, trackedIdents) {
						t.Errorf("%s: in %s: raw CloseWrite on %q bypasses *trackedPeerConn / peerWriteState",
							pos, fnKey, exprString(sel.X))
					}
				case "ReadFrom":
					t.Errorf("%s: in %s: ReadFrom on %q may bypass trackedPeerConn.Write",
						pos, fnKey, exprString(sel.X))
				case "Copy", "CopyBuffer", "CopyN", "WriteString":
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "io" {
						t.Errorf("%s: in %s: io.%s in relay package must be audited against trackedPeerConn",
							pos, fnKey, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}

	if !foundSrcField || !foundDstField {
		t.Fatalf("did not locate Relay.src (%v) and Relay.dst (%v) in relay package", foundSrcField, foundDstField)
	}
}

func funcDeclKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return fmt.Sprintf("(%s).%s", exprString(fn.Recv.List[0].Type), fn.Name.Name)
}

// collectTrackedPeerConnIdents collects every identifier inside fn (including
// nested closures) statically known to hold a *trackedPeerConn: parameters of
// type *trackedPeerConn, or short-assigned identifiers initialized directly
// from .Load() on a *atomic.Pointer[trackedPeerConn] (r.src, r.dst, srcPtr,
// dstPtr, writeSide).
func collectTrackedPeerConnIdents(fn *ast.FuncDecl) map[string]bool {
	tracked := make(map[string]bool)
	trackedPtrs := map[string]bool{}

	recordParams := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, f := range fields.List {
			ts := exprString(f.Type)
			for _, id := range f.Names {
				switch ts {
				case "*trackedPeerConn":
					tracked[id.Name] = true
				case "*atomic.Pointer[trackedPeerConn]":
					trackedPtrs[id.Name] = true
				}
			}
		}
	}

	recordParams(fn.Recv)
	recordParams(fn.Type.Params)

	// First pass: capture parameters of any func literals inside fn.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok {
			recordParams(fl.Type.Params)
		}
		return true
	})

	// Second pass: capture short variable declarations `x := <trackedPtr>.Load()`.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE {
			return true
		}
		if len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, rhs := range assign.Rhs {
			id, ok := assign.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			if isTrackedLoadCall(rhs, trackedPtrs) {
				tracked[id.Name] = true
			}
		}
		return true
	})

	return tracked
}

func isTrackedLoadCall(expr ast.Expr, trackedPtrs map[string]bool) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Load" {
		return false
	}
	switch x := sel.X.(type) {
	case *ast.Ident:
		return trackedPtrs[x.Name]
	case *ast.SelectorExpr:
		return x.Sel.Name == "src" || x.Sel.Name == "dst"
	}
	return false
}

func isTrackedPeerConnExpr(expr ast.Expr, trackedIdents map[string]bool) bool {
	id, ok := expr.(*ast.Ident)
	return ok && trackedIdents[id.Name]
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.IndexExpr:
		return exprString(x.X) + "[" + exprString(x.Index) + "]"
	case *ast.IndexListExpr:
		parts := make([]string, len(x.Indices))
		for i, idx := range x.Indices {
			parts[i] = exprString(idx)
		}
		return exprString(x.X) + "[" + strings.Join(parts, ",") + "]"
	default:
		return fmt.Sprintf("%T", e)
	}
}
