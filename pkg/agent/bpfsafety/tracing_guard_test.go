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
					(fnName == "AttachTracing" || fnName == "attachPinnedReaperNoop")
				if !allowed {
					out = append(out, fmt.Sprintf("%s:%d (%s): direct link.%s outside bpfsafety choke point", rel, fset.Position(x.Pos()).Line, fnName, name))
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
