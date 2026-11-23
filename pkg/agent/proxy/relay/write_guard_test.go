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
// trackedPeerConn (and therefore peerWriteState), failing closed on both
// direct and indirect writes:
//   - Relay.src and Relay.dst must both have type atomic.Pointer[trackedPeerConn],
//     and every Store to them must wrap via wrapPeerConn.
//   - No non-test file may call .Write, .CloseWrite, or .ReadFrom on a raw
//     net.Conn outside the single tracked chokepoints and low-level wrappers.
//   - No non-test file may call the package-local closeWrite helper outside
//     (*peerWriteState).closeWrite.
//   - No non-test file may call indirect writer helpers (io.Copy*, io.WriteString,
//     fmt.Fprint*, bufio.NewWriter*) or convert/assign a raw conn to io.Writer.
//   - No non-test file may pass a raw net.Conn, *tls.Conn, *net.TCPConn, or
//     unwrapped trackedPeerConn.Conn / NetConn() / Unwrap() to any function
//     outside wrapPeerConn and the explicitly audited non-writing allowlist.
func TestNoRawNetConnWriteInRelayPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read relay package dir: %v", err)
	}

	fset := token.NewFileSet()
	foundSrcField := false
	foundDstField := false

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		errs, hasSrc, hasDst := checkRelayFileAST(fset, file)
		if hasSrc {
			foundSrcField = true
		}
		if hasDst {
			foundDstField = true
		}
		for _, msg := range errs {
			t.Error(msg)
		}
	}

	if !foundSrcField || !foundDstField {
		t.Fatalf("did not locate Relay.src (%v) and Relay.dst (%v) in relay package", foundSrcField, foundDstField)
	}
}

// TestRawConnWriteGuardRejectsDirectAndIndirectWrites verifies that
// checkRelayFileAST fails closed on both direct raw writes and indirect raw
// writes (such as closeWrite(&raw), fmt.Fprint(raw, ...), io.Copy(raw, ...),
// io.WriteString(raw, ...), converting a raw conn to io.Writer, or passing a
// raw net.Conn / *tls.Conn to an unallowlisted helper).
func TestRawConnWriteGuardRejectsDirectAndIndirectWrites(t *testing.T) {
	for _, tc := range []struct {
		name       string
		src        string
		wantReject bool
		wantSubstr string
	}{
		{
			name: "direct Write on raw net.Conn parameter is rejected",
			src: `package relay
import "net"
func bad(c net.Conn, p []byte) { _, _ = c.Write(p) }`,
			wantReject: true,
			wantSubstr: "raw Write",
		},
		{
			name: "unwrapping trackedPeerConn.Conn to Write is rejected",
			src: `package relay
func (r *Relay) bad(p []byte) {
	dst := r.dst.Load()
	_, _ = dst.Conn.Write(p)
}`,
			wantReject: true,
			wantSubstr: "raw Write",
		},
		{
			name: "indirect closeWrite(&raw) outside peerWriteState is rejected",
			src: `package relay
import "net"
func bad(raw net.Conn) error { return closeWrite(&raw) }`,
			wantReject: true,
			wantSubstr: "closeWrite",
		},
		{
			name: "indirect fmt.Fprint on raw net.Conn is rejected",
			src: `package relay
import (
	"fmt"
	"net"
)
func bad(raw net.Conn) { _, _ = fmt.Fprint(raw, "hi") }`,
			wantReject: true,
			wantSubstr: "indirect writer helper",
		},
		{
			name: "indirect io.Copy to raw net.Conn is rejected",
			src: `package relay
import (
	"io"
	"net"
)
func bad(raw net.Conn, r io.Reader) { _, _ = io.Copy(raw, r) }`,
			wantReject: true,
			wantSubstr: "indirect writer helper",
		},
		{
			name: "indirect io.WriteString to raw net.Conn is rejected",
			src: `package relay
import (
	"io"
	"net"
)
func bad(raw net.Conn) { _, _ = io.WriteString(raw, "hi") }`,
			wantReject: true,
			wantSubstr: "indirect writer helper",
		},
		{
			name: "converting raw net.Conn to io.Writer is rejected",
			src: `package relay
import (
	"io"
	"net"
)
func bad(raw net.Conn) io.Writer {
	var w io.Writer = raw
	return w
}`,
			wantReject: true,
			wantSubstr: "io.Writer",
		},
		{
			name: "passing raw net.Conn to unallowlisted helper is rejected",
			src: `package relay
import "net"
func bad(raw net.Conn) { someExternalWriter(raw) }`,
			wantReject: true,
			wantSubstr: "raw conn argument",
		},
		{
			name: "passing raw *tls.Conn to unallowlisted helper is rejected",
			src: `package relay
import "crypto/tls"
func bad(raw *tls.Conn) { someExternalWriter(raw) }`,
			wantReject: true,
			wantSubstr: "raw conn argument",
		},
		{
			name: "passing unwrapped dst.NetConn() to unallowlisted helper is rejected",
			src: `package relay
func (r *Relay) bad() {
	dst := r.dst.Load()
	someExternalWriter(dst.NetConn())
}`,
			wantReject: true,
			wantSubstr: "raw conn argument",
		},
		{
			name: "embedding raw net.Conn in unallowlisted struct literal is rejected",
			src: `package relay
import "net"
func bad(raw net.Conn) any {
	return &untrackedWrapper{c: raw}
}`,
			wantReject: true,
			wantSubstr: "raw conn stored in unallowlisted composite literal",
		},
		{
			name: "storing unwrapped trackedPeerConn into r.dst is rejected",
			src: `package relay
func (r *Relay) bad(tc *trackedPeerConn) {
	r.dst.Store(tc)
}`,
			wantReject: true,
			wantSubstr: "must wrap via wrapPeerConn",
		},
		{
			name: "trackedPeerConn Write via r.dst.Load is allowed",
			src: `package relay
func (r *Relay) good(p []byte) {
	dst := r.dst.Load()
	_, _ = dst.Write(p)
}`,
			wantReject: false,
		},
		{
			name: "wrapping raw net.Conn via wrapPeerConn and storing is allowed",
			src: `package relay
import "net"
func (r *Relay) good(raw net.Conn) {
	r.dst.Store(wrapPeerConn(raw, &r.dstWrites))
}`,
			wantReject: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "synthetic.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse synthetic source: %v", err)
			}
			errs, _, _ := checkRelayFileAST(fset, file)
			if tc.wantReject {
				if len(errs) == 0 {
					t.Fatalf("guard accepted forbidden code, want rejection matching %q", tc.wantSubstr)
				}
				joined := strings.Join(errs, "\n")
				if !strings.Contains(joined, tc.wantSubstr) {
					t.Fatalf("guard errors %q do not mention %q", joined, tc.wantSubstr)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("guard rejected valid code: %v", errs)
			}
		})
	}
}

func checkRelayFileAST(fset *token.FileSet, file *ast.File) (errs []string, foundSrc, foundDst bool) {
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

	// Audited functions allowed to receive a raw net.Conn / *tls.Conn / *net.TCPConn:
	// wrapPeerConn itself, read-only / inspection / socket-state helpers, and
	// TLS-upgrade wrappers whose output is wrapped via wrapPeerConn before Store.
	allowRawConnCallee := map[string]bool{
		"wrapPeerConn":                true,
		"connseq.Of":                  true,
		"clientSocketOf":              true,
		"tcpStateSocketOf":            true,
		"countFromAccept":             true,
		"countedClientUnder":          true,
		"reflect.TypeOf":              true,
		"observedRead":                true,
		"observedReadAt":              true,
		"proxyStamped":                true,
		"pTls.PassTLSLayer":           true,
		"newPrependingConn":           true,
		"newPrependingConnWithReadAt": true,
		"newReadTrackingConn":         true,
		"newReadTimeReportingConn":    true,
		"r.cfg.TLSUpgradeFn":          true,
		"publishRealCertFromUpgraded": true,
		"unwrapToTLSConn":             true,
		"resetClientOnClose":          true,
		"resetClientAsking":           true,
		"resetOnceTaken":              true,
		"resetCloser":                 true,
		"tcpConnUnder":                true,
		"r.noteClientFINSent":         true,
		"readFullPreamble":            true,
		"errors.Is":                   true,
		"errors.As":                   true,
	}

	// Audited wrapper struct types allowed to hold a raw net.Conn field.
	allowRawConnStruct := map[string]bool{
		"trackedPeerConn":       true,
		"readTrackingConn":      true,
		"readTimeReportingConn": true,
		"prependingConn":        true,
		"countedClient":         true,
	}

	// Exact (enclosingFunc, callee) pairs for the single write/half-close
	// delegation chokepoints.
	allowRawConnDelegation := map[string]string{
		"(*trackedPeerConn).Write":      "c.writes.write",
		"(*trackedPeerConn).CloseWrite": "c.writes.closeWrite",
		"(*peerWriteState).closeWrite":  "closeWrite",
		"(*countedClient).Write":        "c.count.write",
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
							foundSrc = true
							if got := exprString(field.Type); got != "atomic.Pointer[trackedPeerConn]" {
								errs = append(errs, fmt.Sprintf("%s: Relay.src has type %q, want atomic.Pointer[trackedPeerConn]",
									fset.Position(field.Pos()), got))
							}
						case "dst":
							foundDst = true
							if got := exprString(field.Type); got != "atomic.Pointer[trackedPeerConn]" {
								errs = append(errs, fmt.Sprintf("%s: Relay.dst has type %q, want atomic.Pointer[trackedPeerConn]",
									fset.Position(field.Pos()), got))
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
		trackedIdents, rawIdents := collectConnIdents(fn)

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			// Reject explicit typed var declarations as io.Writer / io.ReadWriter / io.ReadWriteCloser.
			if vs, ok := n.(*ast.ValueSpec); ok && vs.Type != nil {
				if isWriterInterfaceType(exprString(vs.Type)) {
					errs = append(errs, fmt.Sprintf("%s: in %s: explicit %s binding can carry a raw conn for indirect writes",
						fset.Position(vs.Pos()), fnKey, exprString(vs.Type)))
				}
			}

			// Reject storing a raw conn in any composite literal outside the
			// audited wrapper structs.
			if cl, ok := n.(*ast.CompositeLit); ok {
				litType := exprString(cl.Type)
				if !allowRawConnStruct[litType] {
					for _, elt := range cl.Elts {
						val := elt
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							val = kv.Value
						}
						if isRawConnExpr(val, trackedIdents, rawIdents) {
							errs = append(errs, fmt.Sprintf("%s: in %s: raw conn stored in unallowlisted composite literal %s",
								fset.Position(cl.Pos()), fnKey, litType))
						}
					}
				}
			}

			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos())

			// Reject type conversions to io.Writer / io.ReadWriter / io.ReadWriteCloser.
			if isWriterInterfaceType(exprString(call.Fun)) {
				errs = append(errs, fmt.Sprintf("%s: in %s: conversion to %s can carry a raw conn for indirect writes",
					pos, fnKey, exprString(call.Fun)))
				return true
			}

			// Direct identifier calls (package-local helpers like closeWrite(...)).
			if id, ok := call.Fun.(*ast.Ident); ok {
				if id.Name == "closeWrite" && fnKey != "(*peerWriteState).closeWrite" {
					errs = append(errs, fmt.Sprintf("%s: in %s: indirect closeWrite call bypasses *trackedPeerConn / peerWriteState",
						pos, fnKey))
					return true
				}
				checkRawConnArgs(pos, fnKey, id.Name, call.Args, trackedIdents, rawIdents, allowRawConnCallee, allowRawConnDelegation, &errs)
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				// Anonymous func or function-valued call: still reject raw conn args.
				checkRawConnArgs(pos, fnKey, exprString(call.Fun), call.Args, trackedIdents, rawIdents, allowRawConnCallee, allowRawConnDelegation, &errs)
				return true
			}

			// Enforce r.src.Store / r.dst.Store always wrap via wrapPeerConn.
			if sel.Sel.Name == "Store" {
				if xSel, ok := sel.X.(*ast.SelectorExpr); ok && (xSel.Sel.Name == "src" || xSel.Sel.Name == "dst") {
					if len(call.Args) != 1 || !isWrapPeerConnCall(call.Args[0]) {
						errs = append(errs, fmt.Sprintf("%s: in %s: %s.Store must wrap via wrapPeerConn",
							pos, fnKey, exprString(sel.X)))
					}
				}
			}

			switch sel.Sel.Name {
			case "Write":
				if !allowRawWrite[fnKey] && !isTrackedPeerConnExpr(sel.X, trackedIdents) {
					errs = append(errs, fmt.Sprintf("%s: in %s: raw Write on %q bypasses *trackedPeerConn / peerWriteState",
						pos, fnKey, exprString(sel.X)))
				}
				return true
			case "CloseWrite":
				if !allowRawCloseWrite[fnKey] && !isTrackedPeerConnExpr(sel.X, trackedIdents) {
					errs = append(errs, fmt.Sprintf("%s: in %s: raw CloseWrite on %q bypasses *trackedPeerConn / peerWriteState",
						pos, fnKey, exprString(sel.X)))
				}
				return true
			case "ReadFrom":
				errs = append(errs, fmt.Sprintf("%s: in %s: ReadFrom on %q may bypass trackedPeerConn.Write",
					pos, fnKey, exprString(sel.X)))
				return true
			}

			// Reject stdlib indirect writer helpers anywhere in non-test relay code.
			if pkg, ok := sel.X.(*ast.Ident); ok && isIndirectWriterHelper(pkg.Name, sel.Sel.Name) {
				errs = append(errs, fmt.Sprintf("%s: in %s: indirect writer helper %s.%s bypasses *trackedPeerConn audit",
					pos, fnKey, pkg.Name, sel.Sel.Name))
				return true
			}

			callee := exprString(call.Fun)
			checkRawConnArgs(pos, fnKey, callee, call.Args, trackedIdents, rawIdents, allowRawConnCallee, allowRawConnDelegation, &errs)
			return true
		})
	}
	return errs, foundSrc, foundDst
}

func checkRawConnArgs(
	pos token.Position,
	fnKey, callee string,
	args []ast.Expr,
	trackedIdents, rawIdents map[string]bool,
	allowCallee map[string]bool,
	allowDelegation map[string]string,
	errs *[]string,
) {
	if allowCallee[callee] || allowDelegation[fnKey] == callee {
		return
	}
	for _, arg := range args {
		if isRawConnExpr(arg, trackedIdents, rawIdents) {
			*errs = append(*errs, fmt.Sprintf("%s: in %s: raw conn argument %q passed to %s outside wrapPeerConn/audited non-writing helpers",
				pos, fnKey, exprString(arg), callee))
		}
	}
}

func isIndirectWriterHelper(pkg, fn string) bool {
	switch pkg {
	case "io":
		switch fn {
		case "Copy", "CopyBuffer", "CopyN", "WriteString", "MultiWriter":
			return true
		}
	case "fmt":
		switch fn {
		case "Fprint", "Fprintf", "Fprintln":
			return true
		}
	case "bufio":
		switch fn {
		case "NewWriter", "NewWriterSize", "NewReadWriter":
			return true
		}
	}
	return false
}

func isWriterInterfaceType(ts string) bool {
	switch ts {
	case "io.Writer", "io.ReadWriter", "io.ReadWriteCloser", "io.StringWriter":
		return true
	}
	return false
}

func isRawConnType(ts string) bool {
	switch ts {
	case "net.Conn", "*net.Conn", "*tls.Conn", "*net.TCPConn", "closeWriter":
		return true
	}
	return isWriterInterfaceType(ts)
}

func isWrapPeerConnCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "wrapPeerConn"
}

func funcDeclKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return fmt.Sprintf("(%s).%s", exprString(fn.Recv.List[0].Type), fn.Name.Name)
}

// collectConnIdents returns:
//   - tracked: identifiers statically known to hold *trackedPeerConn
//   - raw: identifiers holding a raw net.Conn, *tls.Conn, *net.TCPConn, or io.Writer
func collectConnIdents(fn *ast.FuncDecl) (tracked, raw map[string]bool) {
	tracked = make(map[string]bool)
	raw = make(map[string]bool)
	trackedPtrs := map[string]bool{}

	recordParams := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, f := range fields.List {
			ts := exprString(f.Type)
			for _, id := range f.Names {
				switch {
				case ts == "*trackedPeerConn":
					tracked[id.Name] = true
				case ts == "*atomic.Pointer[trackedPeerConn]":
					trackedPtrs[id.Name] = true
				case isRawConnType(ts):
					raw[id.Name] = true
				}
			}
		}
	}

	recordParams(fn.Recv)
	recordParams(fn.Type.Params)
	recordParams(fn.Type.Results)

	// Capture parameters of nested func literals and typed var declarations.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			recordParams(x.Type.Params)
			recordParams(x.Type.Results)
		case *ast.ValueSpec:
			if x.Type != nil {
				ts := exprString(x.Type)
				for _, id := range x.Names {
					switch {
					case ts == "*trackedPeerConn":
						tracked[id.Name] = true
					case isRawConnType(ts):
						raw[id.Name] = true
					}
				}
			}
		}
		return true
	})

	// Capture short variable declarations `x := <trackedPtr>.Load()`.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != len(assign.Rhs) {
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

	// Fixed-point propagation for local aliases of raw conns (e.g., `inner := c.NetConn()`,
	// `u := raw`, `tc, ok := c.(*tls.Conn)`, `upgraded, err := r.cfg.TLSUpgradeFn(...)`).
	changed := true
	for changed {
		changed = false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range assign.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" || tracked[id.Name] || raw[id.Name] {
					continue
				}
				var rhs ast.Expr
				if len(assign.Rhs) == len(assign.Lhs) {
					rhs = assign.Rhs[i]
				} else if len(assign.Rhs) == 1 && i == 0 {
					rhs = assign.Rhs[0]
				}
				if rhs != nil && isRawConnExpr(rhs, tracked, raw) {
					raw[id.Name] = true
					changed = true
				}
			}
			return true
		})
	}

	return tracked, raw
}

func isRawConnExpr(expr ast.Expr, tracked, raw map[string]bool) bool {
	switch x := expr.(type) {
	case *ast.Ident:
		return raw[x.Name]
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return isRawConnExpr(x.X, tracked, raw)
		}
	case *ast.StarExpr:
		return isRawConnExpr(x.X, tracked, raw)
	case *ast.ParenExpr:
		return isRawConnExpr(x.X, tracked, raw)
	case *ast.TypeAssertExpr:
		if x.Type != nil && isRawConnType(exprString(x.Type)) {
			return true
		}
		return isRawConnExpr(x.X, tracked, raw)
	case *ast.SelectorExpr:
		// Unwrapping .Conn or .tcp from a wrapper (including *trackedPeerConn).
		if x.Sel.Name == "Conn" || x.Sel.Name == "tcp" {
			return true
		}
	case *ast.CallExpr:
		callee := exprString(x.Fun)
		if strings.HasSuffix(callee, ".NetConn") || strings.HasSuffix(callee, ".Unwrap") {
			return true
		}
		switch callee {
		case "newPrependingConn", "newPrependingConnWithReadAt", "newReadTrackingConn",
			"newReadTimeReportingConn", "r.cfg.TLSUpgradeFn", "tcpConnUnder",
			"resetCloser", "CountClientWrites":
			return true
		}
	}
	return false
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
	case *ast.UnaryExpr:
		return x.Op.String() + exprString(x.X)
	default:
		return fmt.Sprintf("%T", e)
	}
}
