package bpfsafety

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	ciliumLinkImportPath = "github.com/cilium/ebpf/link"
)

var forbiddenCiliumTrampolineFuncs = map[string]bool{
	"AttachTracing":  true,
	"AttachLSM":      true,
	"AttachFreplace": true,
	"AttachRawLink":  true,
}

// TestNoUnpinnedAttachTracingOutsideBPFSafety enforces the CVE-2026-98800
// structural guard across the OSS repository:
//   - Resolves github.com/cilium/ebpf/link by import path (catching standard,
//     aliased, and dot imports) and rejects any call or reference to
//     AttachTracing, AttachLSM, AttachFreplace, AttachRawLink, or
//     AttachRawTracepoint with an empty/missing Name outside
//     pkg/agent/bpfsafety/grace_linux.go's single AttachTracing choke point.
func TestNoUnpinnedAttachTracingOutsideBPFSafety(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var violations []string
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".go-mod-cache", ".go-build-cache":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		checked++
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			violations = append(violations, fmt.Sprintf("%s: parse error: %v", rel, err))
			return nil
		}
		violations = append(violations, checkOSSAttachTracingFile(fset, rel, file)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if checked < 50 {
		t.Fatalf("expected to inspect at least 50 OSS Go files, got %d", checked)
	}
	if len(violations) > 0 {
		t.Fatalf("unprotected BPF tracing attach site(s) in OSS:\n  - %s", strings.Join(violations, "\n  - "))
	}
}

type importNames struct {
	names map[string]bool
	dot   bool
}

func resolveImportNames(file *ast.File, targetPath, defaultName string) importNames {
	res := importNames{names: map[string]bool{}}
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != targetPath {
			continue
		}
		if imp.Name == nil {
			res.names[defaultName] = true
			continue
		}
		switch imp.Name.Name {
		case ".":
			res.dot = true
		case "_":
		default:
			res.names[imp.Name.Name] = true
		}
	}
	return res
}

func checkOSSAttachTracingFile(fset *token.FileSet, rel string, file *ast.File) []string {
	linkImps := resolveImportNames(file, ciliumLinkImportPath, "link")

	var out []string
	var walk func(n ast.Node, encl *ast.FuncDecl)
	walk = func(n ast.Node, encl *ast.FuncDecl) {
		if n == nil {
			return
		}
		if fd, ok := n.(*ast.FuncDecl); ok {
			encl = fd
		}
		fnName := "<package>"
		if encl != nil {
			fnName = encl.Name.Name
		}

		switch x := n.(type) {
		case *ast.CallExpr:
			if name, ok := matchImportedFunc(x.Fun, linkImps, forbiddenCiliumTrampolineFuncs); ok {
				allowed := rel == "pkg/agent/bpfsafety/grace_linux.go" &&
					name == "AttachTracing" &&
					(fnName == "AttachTracing" || fnName == "attachPinnedReaperNoop" || fnName == "attachPinnedAnchorNoop")
				if !allowed {
					out = append(out, fmt.Sprintf("%s:%d (%s): direct link.%s outside bpfsafety choke point", rel, fset.Position(x.Pos()).Line, fnName, name))
				} else if fnName == "AttachTracing" && (encl == nil || !hasCallBeforePos(encl.Body, "PinBeforeAttach", x.Pos())) {
					out = append(out, fmt.Sprintf("%s:%d (%s): link.AttachTracing without preceding PinBeforeAttach", rel, fset.Position(x.Pos()).Line, fnName))
				}
				return
			}
			if isIdentOrSel(x.Fun, "attachPinnedReaperNoop") {
				if rel != "pkg/agent/bpfsafety/rebuild_linux.go" || fnName != "forceRebuildTarget" || encl == nil || !hasCallBeforePos(encl.Body, "pinNoop", x.Pos()) {
					out = append(out, fmt.Sprintf("%s:%d (%s): attachPinnedReaperNoop without preceding pinNoop", rel, fset.Position(x.Pos()).Line, fnName))
				}
				return
			}
			if isIdentOrSel(x.Fun, "attachPinnedAnchorNoop") {
				if rel != "pkg/agent/bpfsafety/anchors_linux.go" || fnName != "ensureAnchor" || encl == nil || !hasCallBeforePos(encl.Body, "Pin", x.Pos()) {
					out = append(out, fmt.Sprintf("%s:%d (%s): attachPinnedAnchorNoop without preceding prog.Pin", rel, fset.Position(x.Pos()).Line, fnName))
				}
				return
			}
			if _, ok := matchImportedFunc(x.Fun, linkImps, map[string]bool{"AttachRawTracepoint": true}); ok {
				if rawTracepointHasEmptyName(x) {
					out = append(out, fmt.Sprintf("%s:%d (%s): link.AttachRawTracepoint with empty/missing Name (trampoline attach bypass)", rel, fset.Position(x.Pos()).Line, fnName))
				}
				return
			}
			if isIdentOrSel(x.Fun, "attachTracingFn") {
				out = append(out, fmt.Sprintf("%s:%d (%s): direct attachTracingFn outside bpfsafety choke point", rel, fset.Position(x.Pos()).Line, fnName))
				return
			}
		case *ast.SelectorExpr:
			if name, ok := matchImportedFunc(x, linkImps, forbiddenCiliumTrampolineFuncs); ok {
				out = append(out, fmt.Sprintf("%s:%d (%s): link.%s reference outside bpfsafety choke point", rel, fset.Position(x.Pos()).Line, fnName, name))
				return
			}
		case *ast.Ident:
			if x.Name == "attachTracingFn" {
				out = append(out, fmt.Sprintf("%s:%d (%s): attachTracingFn outside bpfsafety choke point", rel, fset.Position(x.Pos()).Line, fnName))
				return
			}
			if linkImps.dot && forbiddenCiliumTrampolineFuncs[x.Name] {
				out = append(out, fmt.Sprintf("%s:%d (%s): dot-imported link.%s outside bpfsafety choke point", rel, fset.Position(x.Pos()).Line, fnName, x.Name))
				return
			}
		}
		ast.Inspect(n, func(child ast.Node) bool {
			if child == n || child == nil {
				return true
			}
			walk(child, encl)
			return false
		})
	}
	for _, decl := range file.Decls {
		walk(decl, nil)
	}
	// Enforce that forceRebuildTarget actually invokes attachPinnedReaperNoop
	// (killing M5: a no-op rebuild loop that returns nil without updating the trampoline).
	if rel == "pkg/agent/bpfsafety/rebuild_linux.go" {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "forceRebuildTarget" || fd.Body == nil {
				continue
			}
			if !hasCallBeforePos(fd.Body, "attachPinnedReaperNoop", fd.Body.End()) {
				out = append(out, fmt.Sprintf("%s:%d (forceRebuildTarget): missing attachPinnedReaperNoop call (rebuild loop cannot be a no-op)", rel, fset.Position(fd.Pos()).Line))
			}
		}
	}
	// Enforce that unixconn.go passes the resolved AttachTo target obtained
	// directly from unixFexitAttachTarget(spec, ...) with a non-nil spec to
	// bpfsafety.AttachTracing (killing M12, M12b, and M29).
	if rel == "pkg/agent/hooks/linux/unixconn.go" {
		isValidHelperCall := func(ce *ast.CallExpr) bool {
			if !isIdentOrSel(ce.Fun, "unixFexitAttachTarget") || len(ce.Args) < 2 {
				return false
			}
			if arg0, ok := ce.Args[0].(*ast.Ident); ok && arg0.Name == "nil" {
				return false
			}
			return true
		}
		assignedFromHelper := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			if ce, ok := as.Rhs[0].(*ast.CallExpr); ok && isValidHelperCall(ce) {
				assignedFromHelper[lhs.Name] = true
			} else {
				delete(assignedFromHelper, lhs.Name)
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok || !isIdentOrSel(ce.Fun, "AttachTracing") || len(ce.Args) < 3 {
				return true
			}
			if argCall, ok := ce.Args[2].(*ast.CallExpr); ok && isValidHelperCall(argCall) {
				return true
			}
			id, ok := ce.Args[2].(*ast.Ident)
			if !ok || !assignedFromHelper[id.Name] {
				out = append(out, fmt.Sprintf("%s:%d: bpfsafety.AttachTracing 3rd arg must be assigned directly from unixFexitAttachTarget(spec, ...) with non-nil spec", rel, fset.Position(ce.Pos()).Line))
			}
			return true
		})
	}
	// Enforce the never-remove-anchor-last-link invariant in anchors_linux.go:
	//  - CleanupAnchors / cleanupAnchorsInRoot MUST NEVER load-and-close or
	//    unlink any pinned anchor link or anchor prog (released only by reboot).
	//  - ensureAnchor MUST NEVER os.Remove(linkPath) (which would unlink a live
	//    anchor on transient EMFILE/ENOMEM) and MUST NEVER defer closing the
	//    newly attached anchor link before l.Pin succeeds.
	if rel == "pkg/agent/bpfsafety/anchors_linux.go" {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			switch fd.Name.Name {
			case "CleanupAnchors", "cleanupAnchorsInRoot":
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					ce, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if isIdentOrSel(ce.Fun, "LoadPinnedLink") || isIdentOrSel(ce.Fun, "Remove") || isIdentOrSel(ce.Fun, "RemoveAll") || isIdentOrSel(ce.Fun, "Unlink") {
						out = append(out, fmt.Sprintf("%s:%d (%s): anchor cleanup must never unlink or detach pinned anchor links (released only by reboot)", rel, fset.Position(ce.Pos()).Line, fd.Name.Name))
					}
					return true
				})
			case "ensureAnchor":
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CallExpr:
						if isIdentOrSel(x.Fun, "Remove") || isIdentOrSel(x.Fun, "RemoveAll") || isIdentOrSel(x.Fun, "Unlink") {
							// The only allowed removal in ensureAnchor is os.Remove(progPath)
							// when attachPinnedAnchorNoop fails before any link exists. Any
							// other Remove/RemoveAll/Unlink (whether linkPath, filepath.Join,
							// or another path under anchorsDir) could unlink a live anchor on
							// transient EMFILE/ENOMEM (kills M33 in all forms).
							allowedProgCleanup := len(x.Args) == 1 && isIdentOrSel(x.Fun, "Remove")
							if allowedProgCleanup {
								if id, ok := x.Args[0].(*ast.Ident); !ok || id.Name != "progPath" {
									allowedProgCleanup = false
								}
							}
							if !allowedProgCleanup {
								out = append(out, fmt.Sprintf("%s:%d (ensureAnchor): must never remove anchor paths other than progPath (could unlink live anchor on transient EMFILE/ENOMEM)", rel, fset.Position(x.Pos()).Line))
							}
						}
					case *ast.DeferStmt:
						ast.Inspect(x.Call, func(dn ast.Node) bool {
							dce, ok := dn.(*ast.CallExpr)
							if !ok {
								return true
							}
							if sel, ok := dce.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" {
								if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "l" {
									out = append(out, fmt.Sprintf("%s:%d (ensureAnchor): must never defer l.Close() before l.Pin succeeds (would close last link on pin failure)", rel, fset.Position(x.Pos()).Line))
								}
							}
							return true
						})
					}
					return true
				})
			}
		}
	}
	return out
}

func matchImportedFunc(e ast.Expr, imps importNames, funcs map[string]bool) (string, bool) {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if ok && imps.names[id.Name] && funcs[x.Sel.Name] {
			return x.Sel.Name, true
		}
	case *ast.Ident:
		if imps.dot && funcs[x.Name] {
			return x.Name, true
		}
	}
	return "", false
}

// rawTracepointHasEmptyName returns true when AttachRawTracepoint's options
// struct literal omits Name or sets Name to "".
func rawTracepointHasEmptyName(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	cl, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Name" {
			continue
		}
		if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			v, err := strconv.Unquote(lit.Value)
			return err == nil && strings.TrimSpace(v) == ""
		}
		return false
	}
	return true
}

func isIdentOrSel(e ast.Expr, name string) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == name
	case *ast.SelectorExpr:
		return x.Sel.Name == name
	default:
		return false
	}
}

func hasCallBeforePos(body *ast.BlockStmt, fnName string, before token.Pos) bool {
	if body == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		ce, ok := n.(*ast.CallExpr)
		if !ok || ce.Pos() >= before {
			return true
		}
		if isIdentOrSel(ce.Fun, fnName) {
			found = true
			return false
		}
		return true
	})
	return found
}

func TestOSSTracingGuardRejectsDirectAttachAndBypasses(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		src  string
	}{
		{
			name: "direct_AttachTracing",
			rel:  "pkg/agent/hooks/linux/unixconn.go",
			src: `package hooks
import "github.com/cilium/ebpf/link"
func bad(p any) { link.AttachTracing(link.TracingOptions{}) }`,
		},
		{
			name: "aliased_link_import",
			rel:  "pkg/agent/hooks/linux/other.go",
			src: `package hooks
import cl "github.com/cilium/ebpf/link"
func bad() { cl.AttachTracing(cl.TracingOptions{}) }`,
		},
		{
			name: "dot_link_import",
			rel:  "pkg/agent/hooks/linux/other.go",
			src: `package hooks
import . "github.com/cilium/ebpf/link"
func bad() { AttachTracing(TracingOptions{}) }`,
		},
		{
			name: "AttachLSM",
			rel:  "pkg/agent/hooks/linux/other.go",
			src: `package hooks
import "github.com/cilium/ebpf/link"
func bad() { link.AttachLSM(link.LSMOptions{}) }`,
		},
		{
			name: "AttachFreplace",
			rel:  "pkg/agent/hooks/linux/other.go",
			src: `package hooks
import "github.com/cilium/ebpf/link"
func bad() { link.AttachFreplace(nil, "", nil) }`,
		},
		{
			name: "AttachRawLink",
			rel:  "pkg/agent/hooks/linux/other.go",
			src: `package hooks
import "github.com/cilium/ebpf/link"
func bad() { link.AttachRawLink(link.RawLinkOptions{}) }`,
		},
		{
			name: "AttachRawTracepoint_empty_name",
			rel:  "pkg/agent/hooks/linux/other.go",
			src: `package hooks
import "github.com/cilium/ebpf/link"
func bad() { link.AttachRawTracepoint(link.RawTracepointOptions{Name: ""}) }`,
		},
		{
			name: "M2_choke_point_drops_PinBeforeAttach",
			rel:  "pkg/agent/bpfsafety/grace_linux.go",
			src: `package bpfsafety
import "github.com/cilium/ebpf/link"
func AttachTracing(prog any, object, name, role string) (link.Link, error) {
	_ = PinBeforeAttach
	return link.AttachTracing(link.TracingOptions{})
}`,
		},
		{
			name: "M3_reaper_noop_unpinned",
			rel:  "pkg/agent/bpfsafety/rebuild_linux.go",
			src: `package bpfsafety
func (r *graceReaper) forceRebuildTarget(funcName string) error {
	_, err := attachPinnedReaperNoop(nil)
	return err
}`,
		},
		{
			name: "M12_unixconn_passes_prog_name",
			rel:  "pkg/agent/hooks/linux/unixconn.go",
			src: `package linux
func bad(bpfsafety any, name string) {
	bpfsafety.AttachTracing(nil, "unixconn", name, "p")
}`,
		},
		{
			name: "M12b_unixconn_reassigns_targetFunc",
			rel:  "pkg/agent/hooks/linux/unixconn.go",
			src: `package linux
func bad(bpfsafety any, name string) {
	targetFunc := name
	bpfsafety.AttachTracing(nil, "unixconn", targetFunc, "p")
}`,
		},
		{
			name: "M21_anchor_cleanup_removes_last_link",
			rel:  "pkg/agent/bpfsafety/anchors_linux.go",
			src: `package bpfsafety
func cleanupAnchorsInRoot(p string) {
	l, _ := link.LoadPinnedLink(p, nil)
	_ = os.Remove(p)
	_ = l.Close()
}`,
		},
		{
			name: "M29_unixconn_nil_spec",
			rel:  "pkg/agent/hooks/linux/unixconn.go",
			src: `package linux
func bad(bpfsafety any, name string) {
	targetFunc := unixFexitAttachTarget(nil, name)
	bpfsafety.AttachTracing(nil, "unixconn", targetFunc, "p")
}`,
		},
		{
			name: "ensureAnchor_removes_linkPath",
			rel:  "pkg/agent/bpfsafety/anchors_linux.go",
			src: `package bpfsafety
func (g *graceOwner) ensureAnchor(linkPath string) {
	_ = os.Remove(linkPath)
}`,
		},
		{
			name: "M33_ensureAnchor_removes_joined_anchor_link",
			rel:  "pkg/agent/bpfsafety/anchors_linux.go",
			src: `package bpfsafety
import (
	"os"
	"path/filepath"
)
func (g *graceOwner) ensureAnchor(anchorsDir, tok string) {
	_ = os.Remove(filepath.Join(anchorsDir, tok+anchorLinkSuffix))
}`,
		},
		{
			name: "ensureAnchor_defers_link_close",
			rel:  "pkg/agent/bpfsafety/anchors_linux.go",
			src: `package bpfsafety
func (g *graceOwner) ensureAnchor(l any) {
	defer func() { _ = l.Close() }()
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, tc.rel, tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if v := checkOSSAttachTracingFile(fset, tc.rel, file); len(v) == 0 {
				t.Fatalf("case %s: expected AST guard violation, got none", tc.name)
			}
		})
	}
}
