// Package logcontentsites holds the guard that no log line keploy writes
// carries an application's content unless KEPLOY_LOG_CONTENT says it may
// (utils/logcontent): log_content_sites_test.go. This file is the scan the
// guard runs; the test holds what it finds to an allowlist that only shrinks.
//
// A site is a place in keploy's source where a value that is not a constant
// reaches a log line, or the text of an error, without going through
// logcontent or logurl:
//
//   - field: a zap field built from such a value (zap.String, zap.Any,
//     zap.ByteString, zap.Strings, zap.Stringer, zap.Reflect, zap.Object of
//     a marshaler that is not keploy's, ...), or a zapcore encoder's Add or
//     Append of one inside a MarshalLogObject or MarshalLogArray;
//   - error: an error logged whole (zap.Error, zap.NamedError, the error of
//     utils.LogError, an error, or its text, as any other field or as a
//     message), named by where it came from (origin);
//   - message: a log line's message that is not a constant;
//   - errorf: fmt.Errorf of a value that formats as text (a string, a []byte,
//     a struct, a map, a slice, an interface) that is not a constant, and an
//     error's format that is not one;
//   - errors.New: errors.New of a text that is not a constant;
//   - sugar: an argument of a SugaredLogger's method, or of the standard
//     library's log.Printf and its kin, that is not a constant.
//
// fmt.Sprintf, fmt.Sprint and string concatenation feeding a line are read
// through: such a value is safe when each of its parts is, and a site when
// one is not, with the whole expression named. A local string variable is
// read through to what is assigned to it (localAssigns): it is safe when
// every value assigned to it that can reach the line is, and a site at the
// line, named as the variable, when one is not. A variable whose values
// cannot all be seen (a parameter, a range's or a type switch's, one
// assigned from a call's several results or whose address is taken) is a
// site where it is logged. An error variable is followed the same way, to
// every value that can reach the line, each unsafe origin named.
//
// What can reach a line is decided by its function's control flow, and
// only ever overstated (reaches): an assignment every path to the line runs
// replaces what came before it (one in a statement list before the line's
// statement, an if's, a switch's or a for's Init, one inside the closure
// that holds the line), unless a label a goto names lies between them; a
// value assigned after the line reaches it round a loop, or through a
// closure called more than once, and any value assigned in a closure the
// line is not in reaches it; a block that returns after a value, with no
// break or continue between, keeps it from the lines after the block; and
// a deferred closure's value reaches only the deferred closures and those
// that may run after the function returns. In a function with a goto, only
// the replacing assignment counts.
//
// An error's text is the error wherever it goes (taint): put in a local
// string variable, concatenated or formatted into a line's message or a
// field, it is reported as the error it is, by its origin, at the place it
// is put; formatted into another error's text (fmt.Errorf, errors.New, a
// printf wrapper's), the error made is as safe as the errors it formats.
// So an allowlist entry that says an error's format is keploy's words never
// stands for the errors formatted into it.
//
// A log helper of keploy's, an unexported function that hands a parameter of
// its own, unchanged, to a line as its message, as an error it logs whole or
// as a field's value (or to another such helper), is judged at each of its
// calls, by what the call gives it, and not where it logs the parameter: the
// scan reads every call of such a function (logHelpers), so what the
// allowlist could only claim of its callers is checked. A function another
// package, an interface or a function value may call is no helper, and its
// parameter is a site where it logs it.
//
// A value is safe, and no site, when it is:
//
//   - a constant, or nil;
//   - of a kind that cannot hold an application's text: a number or a
//     boolean of any type, time.Duration, time.Time, an IP address, a
//     net.Addr (an address is the connection's, not what was sent over it),
//     an os.FileMode, a signal, a reflect.Type;
//   - of an enum type: a string type declared in keploy that has constants
//     of its own (models.Kind, models.Mode, ...), which keploy names, not
//     the application;
//   - a conversion, a String() or a strconv formatting of a safe value, or a
//     fmt.Sprint/Sprintf or a concatenation of safe values (an argument of
//     %T is safe whatever it is: it is formatted as its type);
//   - keploy's connection id (a field or a variable named ConnectionID,
//     ClientConnID, connID, ...): the agent's own counter;
//   - a value of logcontent or logurl: the gate itself;
//   - an error whose origin cannot hold content (safeOrigin): a sentinel
//     declared as a constant text, errors.New of a constant, fmt.Errorf of
//     safe values wrapping errors whose origins are safe, the context's
//     error, and the error of a call into a package whose errors name only
//     paths, addresses, errnos or the kernel's verdicts (os, syscall, net's
//     sockets, io's readers and writers, os/exec, cilium/ebpf, ...).
//
// Everything else is a site. A site the code gates goes through logcontent
// (Bytes, String, StringLen, Any, Error, ...) or logurl, and is no site any
// more; one that carries no content anyway is listed in
// testdata/log_content_allowlist.txt, with how many times it occurs in its
// file and a line that says why.
package logcontentsites

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/tools/go/packages"
)

// module is keploy's module path, which names are written without.
const module = "go.keploy.io/server/v3"

// Site kinds, in the order the allowlist lists them.
const (
	kindField     = "field"
	kindError     = "error"
	kindMessage   = "message"
	kindErrorf    = "errorf"
	kindErrorsNew = "errors.New"
	kindSugar     = "sugar"
)

var kinds = []string{kindField, kindError, kindMessage, kindErrorf, kindErrorsNew, kindSugar}

// site is one place a value reaches a line or an error's text ungated.
type site struct {
	file string // repo-relative, slash-separated
	line int
	col  int
	kind string
	what string // the expression, as written, with its origin for an error
}

func (s site) key() string { return s.file + " | " + s.kind + " | " + s.what }

func (s site) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", s.file, s.line, s.kind, s.what)
}

// scanner holds what the scan reads across packages: every package-level
// variable's initializer, so a sentinel error is known by what it is.
type scanner struct {
	root  string
	inits map[*types.Var]varInit
	decls map[*types.Func]funcDecl
	named []*types.Named // keploy's named types, for an interface's implementations
	safe  map[*types.Func]int8
	// passes are the parameters a function returns, or wraps, as its error:
	// its errors are as safe as the arguments it is given for them.
	passes map[*types.Func]map[int]bool
	why    map[*types.Func]string // the first unsafe return of each unsafe function (debugging the scan)
	// text and textPasses are keploy's functions that return a text
	// (textSummary): whether what one returns is safe, given safe
	// arguments for the parameters textPasses names, which it puts in it.
	text       map[*types.Func]int8
	textPasses map[*types.Func]map[int]bool
	// parents are each function body's nodes' parents (pathTo).
	parents map[*ast.BlockStmt]map[ast.Node]ast.Node
	// listed are the allowlist's error texts (errorf and errors.New sites)
	// that carry no content: an error built by one is as safe as the
	// errors it wraps.
	listed map[string]bool
	sites  map[string]site // by position and kind: the platforms' loads overlap
	// msgParams and errParams are keploy's log helpers by their parameters:
	// a string parameter a function logs as a line's message, and an error
	// parameter it logs whole, as such, or passes on to a helper that does
	// (logHelpers). Each call of one is judged by what it passes there, and
	// the helper's own use of the parameter is no site.
	msgParams   map[*types.Func]map[int]bool
	errParams   map[*types.Func]map[int]bool
	fieldParams map[*types.Func]map[int]bool // a parameter a function logs as a field's value
}

// varInit is a package-level variable's initializer and where it is
// declared: it is read with its own package's types.
type varInit struct {
	expr ast.Expr
	p    *packages.Package
	file string
}

// funcDecl is a function's declaration and the package it is in.
type funcDecl struct {
	decl *ast.FuncDecl
	p    *packages.Package
	file string
}

func newScanner(root string) *scanner {
	return &scanner{root: root, sites: map[string]site{}}
}

// add scans pkgs, whose files under root but outside excluded are keploy's
// own: one platform's load, of which the scanner keeps the union.
func (s *scanner) add(pkgs []*packages.Package, excluded []string) {
	// Each load has its own types, so what the scan knows of them is the
	// load's.
	s.inits = map[*types.Var]varInit{}
	s.decls = map[*types.Func]funcDecl{}
	s.named = nil
	s.safe = map[*types.Func]int8{}
	s.passes = map[*types.Func]map[int]bool{}
	s.why = map[*types.Func]string{}
	s.text = map[*types.Func]int8{}
	s.parents = map[*ast.BlockStmt]map[ast.Node]ast.Node{}
	s.textPasses = map[*types.Func]map[int]bool{}
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			name := p.Fset.File(f.Pos()).Name()
			rel, _ := filepath.Rel(s.root, name)
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.GenDecl:
					if d.Tok != token.VAR {
						continue
					}
					for _, spec := range d.Specs {
						vs := spec.(*ast.ValueSpec)
						for i, n := range vs.Names {
							if v, ok := p.TypesInfo.Defs[n].(*types.Var); ok && i < len(vs.Values) {
								s.inits[v] = varInit{vs.Values[i], p, filepath.ToSlash(rel)}
							}
						}
					}
				case *ast.FuncDecl:
					if f, ok := p.TypesInfo.Defs[d.Name].(*types.Func); ok && d.Body != nil {
						s.decls[f] = funcDecl{d, p, filepath.ToSlash(rel)}
					}
				}
			}
		}
		for _, name := range p.Types.Scope().Names() {
			if tn, ok := p.Types.Scope().Lookup(name).(*types.TypeName); ok && !tn.IsAlias() {
				if n, ok := tn.Type().(*types.Named); ok && !types.IsInterface(n) {
					s.named = append(s.named, n)
				}
			}
		}
	}
	s.logHelpers(pkgs)
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			name := p.Fset.File(f.Pos()).Name()
			rel, err := filepath.Rel(s.root, name)
			if err != nil || strings.HasPrefix(rel, "..") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel = filepath.ToSlash(rel)
			skip := false
			for _, e := range excluded {
				if strings.HasPrefix(rel, e) {
					skip = true
				}
			}
			if skip {
				continue
			}
			fs := &fileScan{s: s, p: p, info: p.TypesInfo, fset: p.Fset, file: rel}
			fs.walk(f)
		}
	}
}

// logHelpers finds keploy's log helpers (msgParams, errParams,
// fieldParams): a function that hands one of its parameters, unchanged, to a
// line as its message (a *zap.Logger's method, or a helper's message), as an
// error it logs whole (zap.Error, zap.NamedError, utils.LogError's error, or
// a helper's error), or as a field's value (zap.String's, zap.Any's, ..., or
// a helper's). A helper that passes its parameter on to another helper is
// one too, so the scan runs to a fixpoint. A helper's callers are then judged
// by what they pass, which the allowlist could only have claimed.
//
// Only a function every call of which the scan reads is one: unexported
// (no other package, enterprise's included, can call it), with no receiver
// (no interface can call it), and never used but as the callee of a call (no
// callback or function value calls it unseen). Any other's parameter stays a
// site where it is logged.
func (s *scanner) logHelpers(pkgs []*packages.Package) {
	s.msgParams = map[*types.Func]map[int]bool{}
	s.errParams = map[*types.Func]map[int]bool{}
	s.fieldParams = map[*types.Func]map[int]bool{}
	escaped := map[*types.Func]bool{}
	for _, p := range pkgs {
		for _, file := range p.Syntax {
			callees := map[*ast.Ident]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					switch fn := ast.Unparen(c.Fun).(type) {
					case *ast.Ident:
						callees[fn] = true
					case *ast.SelectorExpr:
						callees[fn.Sel] = true
					case *ast.IndexExpr:
						if id, ok := fn.X.(*ast.Ident); ok {
							callees[id] = true
						}
					case *ast.IndexListExpr:
						if id, ok := fn.X.(*ast.Ident); ok {
							callees[id] = true
						}
					}
				}
				if id, ok := n.(*ast.Ident); ok && !callees[id] {
					if f, ok := p.TypesInfo.Uses[id].(*types.Func); ok {
						escaped[f.Origin()] = true
					}
				}
				return true
			})
		}
	}
	helper := func(f *types.Func) bool {
		sig, _ := f.Type().(*types.Signature)
		return sig != nil && sig.Recv() == nil && !f.Exported() && !escaped[f]
	}
	mark := func(m map[*types.Func]map[int]bool, f *types.Func, i int) bool {
		if m[f][i] {
			return false
		}
		if m[f] == nil {
			m[f] = map[int]bool{}
		}
		m[f][i] = true
		return true
	}
	for changed := true; changed; {
		changed = false
		for f, d := range s.decls {
			if !helper(f) {
				continue
			}
			sig := f.Type().(*types.Signature)
			info := d.p.TypesInfo
			param := func(e ast.Expr) (int, bool) {
				id, ok := ast.Unparen(e).(*ast.Ident)
				if !ok {
					return 0, false
				}
				v, ok := info.Uses[id].(*types.Var)
				if !ok {
					return 0, false
				}
				for i := 0; i < sig.Params().Len(); i++ {
					if sig.Params().At(i) == v && !(sig.Variadic() && i == sig.Params().Len()-1) {
						return i, !reassigned(info, d.decl, v)
					}
				}
				return 0, false
			}
			ast.Inspect(d.decl.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := calleeFunc(info, call)
				if callee == nil {
					return true
				}
				msgs, errs, fields := s.helperArgs(callee)
				for _, a := range fields {
					if a < len(call.Args) {
						if i, ok := param(call.Args[a]); ok && !safeType(sig.Params().At(i).Type()) && mark(s.fieldParams, f, i) {
							changed = true
						}
					}
				}
				for _, a := range msgs {
					if a < len(call.Args) {
						if i, ok := param(call.Args[a]); ok && isString(sig.Params().At(i).Type()) && mark(s.msgParams, f, i) {
							changed = true
						}
					}
				}
				for _, a := range errs {
					if a < len(call.Args) {
						if i, ok := param(call.Args[a]); ok && implementsError(sig.Params().At(i).Type()) && mark(s.errParams, f, i) {
							changed = true
						}
					}
				}
				return true
			})
		}
	}
}

// helperArgs are the indices of callee's arguments that a line logs as its
// message, as an error it logs whole, and as a field's value.
func (s *scanner) helperArgs(f *types.Func) (msgs, errs, fields []int) {
	pkg, recv := funcPkg(f), recvName(f)
	switch {
	case pkg == "go.uber.org/zap" && recv == "go.uber.org/zap.Logger":
		if i, ok := loggerMessage[f.Name()]; ok {
			msgs = append(msgs, i)
		}
	case pkg == "go.uber.org/zap" && recv == "":
		if i, ok := zapErrorCtors[f.Name()]; ok {
			errs = append(errs, i)
		}
		if i, ok := zapFieldCtors[f.Name()]; ok && i >= 0 {
			fields = append(fields, i)
		}
	case pkg == module+"/utils" && recv == "":
		if h, ok := helpers[f.Name()]; ok {
			msgs = append(msgs, h.msg)
			if h.err >= 0 {
				errs = append(errs, h.err)
			}
		}
	}
	for i := range s.msgParams[f.Origin()] {
		msgs = append(msgs, i)
	}
	for i := range s.errParams[f.Origin()] {
		errs = append(errs, i)
	}
	for i := range s.fieldParams[f.Origin()] {
		fields = append(fields, i)
	}
	return msgs, errs, fields
}

// isString: t is a string, or a type whose underlying type is one.
func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// helperParam: e is a parameter of a log helper the scan is in, which m
// (msgParams or errParams) names: its callers are judged instead.
func (fs *fileScan) helperParam(m map[*types.Func]map[int]bool, e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return false
	}
	v, ok := fs.info.Uses[id].(*types.Var)
	if !ok {
		return false
	}
	for _, fn := range fs.funcs {
		fd, ok := fn.(*ast.FuncDecl)
		if !ok {
			continue
		}
		f, ok := fs.info.Defs[fd.Name].(*types.Func)
		if !ok {
			continue
		}
		sig := f.Type().(*types.Signature)
		for i := range m[f] {
			if sig.Params().At(i) == v {
				return true
			}
		}
	}
	return false
}

// explain is why each unsafe keploy function's errors are unsafe.
func (s *scanner) explain() map[string]string {
	out := map[string]string{}
	for f, w := range s.why {
		out[qualFunc(f)] = w
	}
	return out
}

// list is every site found, by file and position.
func (s *scanner) list() []site {
	var out []site
	for _, st := range s.sites {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		if out[i].col != out[j].col {
			return out[i].col < out[j].col
		}
		return out[i].kind < out[j].kind
	})
	return out
}

// fileScan scans one file of one package.
type fileScan struct {
	s     *scanner
	p     *packages.Package
	info  *types.Info
	fset  *token.FileSet
	file  string
	funcs []ast.Node // the enclosing FuncDecls and FuncLits, innermost last
	// summarizing is the signature of the function whose returned errors
	// the scan reads (returnsSafe), and passed the indices of its
	// parameters they come from.
	summarizing *types.Signature
	passed      map[int]bool
	// summarizingText: the function summarized returns a text
	// (textSummary), and a string parameter it puts in it is passed, as an
	// error parameter it returns is.
	summarizingText bool
	gated           int // inside if logcontent.On() { ... }
	// errOK: an error, or its text, counts as safe (errSafe), for a value
	// whose errors taint has reported or followed.
	errOK int
	// assigns are the local string variables' values (localAssigns), and
	// following the values safe is reading, against a cycle.
	assigns   map[*types.Var]localValues
	following map[ast.Expr]bool
}

// localValues are the values assigned to a local variable, and whether they
// are all of them; body is the function they are read in, jumps says it
// holds a goto, and labels are the statements its gotos name, where control
// may enter a statement list other than at its start.
type localValues struct {
	set      []assigned
	ok       bool
	declared bool
	opaque   string // why not every value can be seen ("" when they can)
	body     *ast.BlockStmt
	jumps    bool
	labels   []ast.Node // the statements a goto in body names
}

// assigned is one value a local variable is given (rhs, nil for a zero
// value) and the statement that gives it (at); multi: rhs is a call, one of
// whose results it is; kills: it replaces what the variable held (not +=).
type assigned struct {
	at    ast.Node
	rhs   ast.Expr
	multi bool
	kills bool
}

func (fs *fileScan) walk(n ast.Node) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Body == nil {
				return false
			}
			fs.funcs = append(fs.funcs, x)
			fs.walk(x.Body)
			fs.funcs = fs.funcs[:len(fs.funcs)-1]
			return false
		case *ast.FuncLit:
			fs.funcs = append(fs.funcs, x)
			fs.walk(x.Body)
			fs.funcs = fs.funcs[:len(fs.funcs)-1]
			return false
		case *ast.IfStmt:
			// if logcontent.On() { ... }: what the branch logs is the
			// switch's to allow.
			if !onGuard(fs.info, x.Cond) {
				return true
			}
			if x.Init != nil {
				fs.walk(x.Init)
			}
			fs.walk(x.Cond)
			fs.gated++
			fs.walk(x.Body)
			fs.gated--
			if x.Else != nil {
				fs.walk(x.Else)
			}
			return false
		case *ast.CallExpr:
			fs.call(x)
		case *ast.CompositeLit:
			fs.fieldLiteral(x)
		}
		return true
	})
}

// onGuard: cond is logcontent.On(), or a conjunction one of whose terms is.
func onGuard(info *types.Info, cond ast.Expr) bool {
	switch x := ast.Unparen(cond).(type) {
	case *ast.BinaryExpr:
		return x.Op == token.LAND && (onGuard(info, x.X) || onGuard(info, x.Y))
	case *ast.CallExpr:
		f := calleeFunc(info, x)
		return f != nil && f.Name() == "On" && funcPkg(f) == module+"/utils/logcontent"
	}
	return false
}

// at is n's position, for a debugging line.
func (fs *fileScan) at(n ast.Node) string {
	p := fs.fset.Position(n.Pos())
	return fmt.Sprintf("%s:%d: ", fs.file, p.Line)
}

func (fs *fileScan) report(at ast.Node, kind, what string) {
	if fs.gated > 0 {
		return // in a branch only the switch's opt-in takes
	}
	pos := fs.fset.Position(at.Pos())
	st := site{file: fs.file, line: pos.Line, col: pos.Column, kind: kind, what: what}
	fs.s.sites[fmt.Sprintf("%s:%d:%d:%s", st.file, st.line, st.col, kind)] = st
}

// text is an expression as written, on one line.
func (fs *fileScan) text(e ast.Node) string {
	var b strings.Builder
	_ = printer.Fprint(&b, fs.fset, e)
	t := strings.Join(strings.Fields(b.String()), " ")
	return strings.ReplaceAll(t, module+"/", "")
}

// zapFieldCtors are zap's field constructors that take a value which can
// hold text, by the index of that value.
var zapFieldCtors = map[string]int{
	"String": 1, "Stringp": 1, "ByteString": 1, "ByteStrings": 1, "Binary": 1, "Strings": 1,
	"Any": 1, "Reflect": 1, "Stringer": 1, "Stringers": 1,
	"Object": 1, "Objects": 1, "ObjectValues": 1, "Array": 1, "Inline": 0, "Dict": -1,
}

// zapErrorCtors are zap's constructors of an error's field, by the index of
// the error.
var zapErrorCtors = map[string]int{"Error": 0, "NamedError": 1, "Errors": 1}

// loggerMessage: the index of the message of a *zap.Logger's method.
var loggerMessage = map[string]int{"Debug": 0, "Info": 0, "Warn": 0, "Error": 0, "DPanic": 0, "Panic": 0, "Fatal": 0, "Check": 1, "Log": 1}

// helpers are keploy's log helpers: the index of the error each logs whole
// (-1 for none) and of its message.
var helpers = map[string]struct{ err, msg int }{
	"LogError":        {1, 2},
	"LogContentError": {-1, 2}, // logcontent.Error(err)
	"LogErrorWith":    {-1, 2}, // the caller's errFields, built where they are
	"HandleRecovery":  {-1, 2}, // logs the stack, not the panic's value
}

// encoderAdds are a zapcore encoder's methods that write a value which can
// hold text, inside a marshaler's MarshalLogObject or MarshalLogArray.
var encoderAdds = map[string]int{
	"AddString": 1, "AddByteString": 1, "AddBinary": 1, "AddReflected": 1, "AddObject": 1, "AddArray": 1,
	"AppendString": 0, "AppendByteString": 0, "AppendReflected": 0, "AppendObject": 0, "AppendArray": 0,
}

func (fs *fileScan) call(call *ast.CallExpr) {
	f := calleeFunc(fs.info, call)
	if f == nil {
		return
	}
	pkg, recv := funcPkg(f), recvName(f)
	if i, ok := printfWrapper(f); ok {
		// A keploy function that formats its arguments into an error or a
		// line (framingLost(format, args...)): each call is judged as
		// fmt.Errorf's is, by what it formats.
		kind := kindMessage
		if sig := f.Type().(*types.Signature); sig.Results().Len() == 1 && implementsError(sig.Results().At(0).Type()) {
			kind = kindErrorf
		}
		if what, bad := fs.formatText(call.Args[i:], i < len(call.Args) && call.Ellipsis == token.NoPos); bad {
			fs.report(call, kind, f.Name()+"("+what+")")
		}
		if kind == kindMessage {
			// A wrapper that writes a line: the errors it formats are
			// logged, as a message's are. One that makes an error is
			// as safe as they are (callOrigin).
			for _, a := range call.Args[i:] {
				fs.reportErrorParts(f.Name(), a, nil)
			}
		}
		return
	}
	arg := func(i int) ast.Expr {
		if i >= 0 && i < len(call.Args) {
			return call.Args[i]
		}
		return nil
	}
	if strings.HasPrefix(pkg, module) && !(pkg == module+"/utils" && recv == "" && hasHelper(f.Name())) {
		// A log helper of keploy's (logHelpers): what it is given is
		// logged as it was given. Each argument is a site at its own
		// position: a site is kept by its position and kind, so two
		// arguments of one call reported at the call would keep only the
		// one the map's order gave last, a different one each run.
		for i := range fs.s.msgParams[f.Origin()] {
			if m := arg(i); m != nil {
				fs.message(m)
			}
		}
		for i := range fs.s.errParams[f.Origin()] {
			if e := arg(i); e != nil {
				fs.loggedError(e, f.Name(), e)
			}
		}
		for i := range fs.s.fieldParams[f.Origin()] {
			if v := arg(i); v != nil {
				fs.fieldValue(v, f.Name(), v)
			}
		}
	}
	switch {
	case pkg == "go.uber.org/zap" && recv == "":
		if i, ok := zapErrorCtors[f.Name()]; ok {
			if e := arg(i); e != nil {
				fs.loggedError(call, "zap."+f.Name(), e)
			}
			return
		}
		i, ok := zapFieldCtors[f.Name()]
		if !ok {
			return // a number's, a boolean's, a duration's, a time's, a namespace, a stack
		}
		if i < 0 { // zap.Dict(key, fields...): the fields are scanned as they are built
			return
		}
		if v := arg(i); v != nil {
			fs.fieldValue(call, "zap."+f.Name(), v)
		}
	case pkg == "go.uber.org/zap/zapcore" && (recv == "go.uber.org/zap/zapcore.ObjectEncoder" || recv == "go.uber.org/zap/zapcore.ArrayEncoder" || recv == "go.uber.org/zap/zapcore.PrimitiveArrayEncoder"):
		if i, ok := encoderAdds[f.Name()]; ok {
			if v := arg(i); v != nil {
				fs.fieldValue(call, "enc."+f.Name(), v)
			}
		}
	case pkg == "go.uber.org/zap" && recv == "go.uber.org/zap.Logger":
		if i, ok := loggerMessage[f.Name()]; ok {
			if m := arg(i); m != nil {
				fs.message(m)
			}
		}
	case pkg == "go.uber.org/zap/zapcore" && recv == "go.uber.org/zap/zapcore.Core" && f.Name() == "Write":
		// a core's own line: its entry's message is the caller's
	case pkg == "go.uber.org/zap" && recv == "go.uber.org/zap.SugaredLogger":
		for _, a := range call.Args {
			if !fs.safe(a) {
				fs.report(a, kindSugar, "sugar."+f.Name()+"("+fs.text(a)+")")
			}
		}
	case pkg == "log" && (recv == "" || recv == "log.Logger") && stdLogLine(f.Name()):
		// The standard library's logger: a line as a sugared logger's is,
		// written to stderr, its arguments formatted into it.
		for _, a := range call.Args {
			if !fs.safe(a) {
				fs.report(a, kindSugar, "log."+f.Name()+"("+fs.text(a)+")")
			}
		}
	case pkg == module+"/utils" && recv == "":
		h, ok := helpers[f.Name()]
		if !ok {
			return
		}
		if e := arg(h.err); e != nil {
			fs.loggedError(call, "utils."+f.Name(), e)
		}
		if m := arg(h.msg); m != nil {
			fs.message(m)
		}
	case pkg == "fmt" && f.Name() == "Errorf":
		if what, bad := fs.errorfText(call); bad {
			fs.report(call, kindErrorf, what)
		}
	case pkg == "errors" && f.Name() == "New":
		if what, bad := fs.errorsNewText(call); bad {
			fs.report(call, kindErrorsNew, what)
		}
	}
}

// stdLogLine: name is a method of the standard library's log package that
// writes a line (Print, Printf, Fatalln, Panicf, ...).
func stdLogLine(name string) bool {
	for _, p := range []string{"Print", "Fatal", "Panic"} {
		if name == p || name == p+"f" || name == p+"ln" {
			return true
		}
	}
	return false
}

// hasHelper: name is one of utils' log helpers (helpers), which call judges
// by its own table.
func hasHelper(name string) bool {
	_, ok := helpers[name]
	return ok
}

// errorfText is the site a fmt.Errorf call is, and whether it is one: the
// values it formats that are not safe, and its format when that is not a
// constant. The errors it wraps are judged where they are logged.
func (fs *fileScan) errorfText(call *ast.CallExpr) (string, bool) {
	what, bad := fs.formatText(call.Args, call.Ellipsis == token.NoPos)
	return "fmt.Errorf(" + what + ")", bad
}

// formatText is what a format and its arguments (args[0], args[1:]) put in
// a text that is not safe, and whether there is any. spread is false for a
// call that passes its arguments on with "args...": a wrapper's own call,
// whose callers are judged instead.
func (fs *fileScan) formatText(args []ast.Expr, spread bool) (string, bool) {
	var bad []string
	if len(args) == 0 {
		return "", false
	}
	// The errors a format or a value holds the text of are judged where the
	// error made is logged, as the errors it wraps are (callOrigin).
	if !fs.errSafe(args[0]) {
		bad = append(bad, "format "+fs.text(args[0]))
	}
	verbs := formatVerbs(fs.info, args[0])
	var check func(i int, a ast.Expr)
	check = func(i int, a ast.Expr) {
		if i >= 0 && i < len(verbs) && verbs[i] == 'T' {
			return // formatted as its type
		}
		if c, ok := ast.Unparen(a).(*ast.CallExpr); ok && isBuiltin(fs.info, c.Fun, "append") {
			// append([]any{errA, errB}, args...): each value it holds
			for _, x := range c.Args {
				if lit, ok := ast.Unparen(x).(*ast.CompositeLit); ok {
					for _, el := range lit.Elts {
						check(-1, el)
					}
				} else {
					check(-1, x)
				}
			}
			return
		}
		if fs.isError(a) || !formatsAsText(fs.info.TypeOf(a)) || fs.errSafe(a) {
			return
		}
		bad = append(bad, fs.text(a))
	}
	for i, a := range args[1:] {
		check(i, a)
	}
	_ = spread
	return strings.Join(bad, ", "), len(bad) > 0
}

// isBuiltin: fun names the builtin name.
func isBuiltin(info *types.Info, fun ast.Expr, name string) bool {
	id, ok := ast.Unparen(fun).(*ast.Ident)
	if !ok {
		return false
	}
	b, ok := info.Uses[id].(*types.Builtin)
	return ok && b.Name() == name
}

// printfWrapper: f is keploy's and formats as fmt.Sprintf does, its last
// two parameters a format and the values for it (format string, args
// ...any); the index of the format.
func printfWrapper(f *types.Func) (int, bool) {
	if !strings.HasPrefix(funcPkg(f), module) {
		return 0, false
	}
	sig, _ := f.Type().(*types.Signature)
	if sig == nil || !sig.Variadic() || sig.Params().Len() < 2 {
		return 0, false
	}
	n := sig.Params().Len()
	last, format := sig.Params().At(n-1), sig.Params().At(n-2)
	sl, ok := last.Type().(*types.Slice)
	if !ok {
		return 0, false
	}
	if it, ok := sl.Elem().Underlying().(*types.Interface); !ok || !it.Empty() {
		return 0, false
	}
	if b, ok := format.Type().(*types.Basic); !ok || b.Kind() != types.String || format.Name() != "format" {
		return 0, false
	}
	return n - 2, true
}

// wrapperParam: e is the format or the values of the printf wrapper the
// scan is in, which its callers are judged by.
func (fs *fileScan) wrapperParam(e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return false
	}
	v, ok := fs.info.Uses[id].(*types.Var)
	if !ok {
		return false
	}
	for _, fn := range fs.funcs {
		fd, ok := fn.(*ast.FuncDecl)
		if !ok {
			continue
		}
		f, ok := fs.info.Defs[fd.Name].(*types.Func)
		if !ok {
			continue
		}
		if i, ok := printfWrapper(f); ok {
			sig := f.Type().(*types.Signature)
			if v == sig.Params().At(i) || v == sig.Params().At(i+1) {
				return true
			}
		}
	}
	return false
}

func (fs *fileScan) errorsNewText(call *ast.CallExpr) (string, bool) {
	if len(call.Args) != 1 || fs.errSafe(call.Args[0]) {
		return "", false
	}
	return "errors.New(" + fs.text(call.Args[0]) + ")", true
}

// listedText: the error text call builds is a site the allowlist says
// carries no content, so the error it makes is as safe as what it wraps.
func (fs *fileScan) listedText(kind, what string) bool {
	return fs.s.listed[fs.file+" | "+kind+" | "+what]
}

// fieldLiteral: a zap.Field written out, whose String or Interface holds a
// value.
func (fs *fileScan) fieldLiteral(lit *ast.CompositeLit) {
	t := fs.info.TypeOf(lit)
	if t == nil || !isNamed(t, "go.uber.org/zap/zapcore", "Field") {
		return
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && (k.Name == "String" || k.Name == "Interface") && !fs.safe(kv.Value) {
			fs.report(kv.Value, kindField, "zapcore.Field{"+k.Name+": "+fs.text(kv.Value)+"}")
		}
	}
}

// fieldValue: v, the value of a field ctor builds.
func (fs *fileScan) fieldValue(at ast.Node, ctor string, v ast.Expr) {
	if fs.helperParam(fs.s.fieldParams, v) {
		return // the helper's callers are judged by what they give it
	}
	if fs.isError(v) {
		fs.loggedError(at, ctor, v)
		return
	}
	if fs.safe(v) || fs.ownMarshaler(v) {
		return
	}
	// An error's text in the value is the error; the value is a site of
	// its own when the rest of it is not safe.
	if fs.reportErrorParts(ctor, v, nil) && fs.errSafe(v) {
		return
	}
	fs.report(at, kindField, ctor+"("+fs.text(v)+")")
}

// message: a log line's message.
func (fs *fileScan) message(m ast.Expr) {
	if fs.safe(m) || fs.helperParam(fs.s.msgParams, m) {
		return
	}
	// A message that holds an error's text logs the error.
	var errPart ast.Expr
	fs.parts(m, func(e ast.Expr) {
		if errPart == nil && fs.isError(e) {
			errPart = e
		}
	})
	if errPart != nil {
		fs.loggedError(m, "message", errPart)
		if fs.safeExcept(m, errPart) {
			return
		}
	}
	// Every other error whose text it holds (a second one, one put in a
	// local variable first) is reported at its own place.
	if fs.reportErrorParts("message", m, errPart) && fs.errSafe(m) {
		return
	}
	fs.report(m, kindMessage, fs.text(m))
}

// reportErrorParts reports each error whose text v is made of (taint) but
// skip, the one its caller reported, as an error logged whole: one written
// in v at its own place, by what logs it (ctor), and one a local variable
// holds at the place it is put in the variable ("via" the variable, which a
// line or two may log). It is true when v holds any error's text, skip's
// included.
func (fs *fileScan) reportErrorParts(ctor string, v, skip ast.Expr) bool {
	found := false
	fs.taint(v, nil, map[ast.Expr]bool{}, func(part ast.Expr, via *ast.Ident) {
		found = true
		switch {
		case part == skip:
		case via != nil:
			fs.loggedError(part, "via "+via.Name, part)
		default:
			fs.loggedError(part, ctor, part)
		}
	})
	return found
}

// taint calls f for each error, or error's text, that e's text is made of:
// through concatenation, fmt.Sprint*, a conversion, the strings and strconv
// packages' functions, and the values assigned to a local string variable
// that can reach where it is read (localAssigns, reaching); via is the
// variable a part was first reached through, or nil for one written in e
// itself. seen are the values already followed, against a cycle.
func (fs *fileScan) taint(e ast.Expr, via *ast.Ident, seen map[ast.Expr]bool, f func(part ast.Expr, via *ast.Ident)) {
	e = ast.Unparen(e)
	if fs.isError(e) {
		f(e, via)
		return
	}
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			fs.taint(x.X, via, seen, f)
			fs.taint(x.Y, via, seen, f)
		}
	case *ast.CallExpr:
		if tv, ok := fs.info.Types[x.Fun]; ok && tv.IsType() {
			for _, a := range x.Args {
				fs.taint(a, via, seen, f)
			}
			return
		}
		fn := calleeFunc(fs.info, x)
		if fn == nil {
			return
		}
		if p := funcPkg(fn); (p == "fmt" && strings.HasPrefix(fn.Name(), "Sprint")) || p == "strings" || p == "strconv" {
			for _, a := range x.Args {
				fs.taint(a, via, seen, f)
			}
			return
		}
		if strings.HasPrefix(funcPkg(fn), module) {
			// A text of keploy's (textSummary) holds the arguments it is
			// given for the parameters it puts in it.
			if ok, passes := fs.s.textSummary(fn); ok {
				for i := range passes {
					if a := callArg(fs.info, x, fn, i); a != nil {
						fs.taint(a, via, seen, f)
					}
				}
			}
		}
	case *ast.Ident:
		v, ok := fs.info.Uses[x].(*types.Var)
		if !ok || !isString(v.Type()) {
			return
		}
		if via == nil {
			via = x
		}
		for _, r := range fs.reaching(v, x) {
			if r != nil && !seen[r] {
				seen[r] = true
				fs.taint(r, via, seen, f)
			}
		}
	}
}

// taintOrigin: the first error whose text e is made of (taint) whose origin
// can hold content, and false; or true when there is none.
func (fs *fileScan) taintOrigin(e ast.Expr, depth int) (string, bool) {
	bad := ""
	fs.taint(e, nil, map[ast.Expr]bool{}, func(part ast.Expr, _ *ast.Ident) {
		if bad != "" {
			return
		}
		if o, safe := fs.origin(part, depth+1); !safe {
			bad = o
		}
	})
	return bad, bad == ""
}

// errSafe: e is safe but for the errors, or errors' texts, it is made of,
// which the caller judges by their origins.
func (fs *fileScan) errSafe(e ast.Expr) bool {
	fs.errOK++
	defer func() { fs.errOK-- }()
	return fs.safe(e)
}

// varValues are the values assigned to v, a variable declared in a function
// the scan is in, wherever it is assigned in it (v's scope is in that
// function), cached. They are not all of them (opaque) when it is a range's
// variable, or its address is taken (what is written through the pointer is
// not seen); a type switch's variable and a parameter are not declared in
// the body, and a named result starts as its zero value.
func (fs *fileScan) varValues(v *types.Var) localValues {
	if lv, ok := fs.assigns[v]; ok {
		return lv
	}
	lv := localValues{}
	defer func() {
		if fs.assigns == nil {
			fs.assigns = map[*types.Var]localValues{}
		}
		fs.assigns[v] = lv
	}()
	if v.Pkg() == nil || v.Parent() == v.Pkg().Scope() {
		return lv
	}
	var ftype *ast.FuncType
	for i := len(fs.funcs) - 1; i >= 0; i-- {
		fn := fs.funcs[i]
		if fn.Pos() <= v.Pos() && v.Pos() < fn.End() {
			switch f := fn.(type) {
			case *ast.FuncDecl:
				ftype, lv.body = f.Type, f.Body
			case *ast.FuncLit:
				ftype, lv.body = f.Type, f.Body
			}
			break
		}
	}
	if lv.body == nil {
		return lv
	}
	if ftype.Results != nil {
		for _, fl := range ftype.Results.List {
			for _, n := range fl.Names {
				if fs.info.Defs[n] == v {
					lv.declared = true
					lv.set = append(lv.set, assigned{at: ftype, kills: true}) // its zero value
				}
			}
		}
	}
	is := func(e ast.Expr) bool {
		id, ok := ast.Unparen(e).(*ast.Ident)
		return ok && (fs.info.Defs[id] == v || fs.info.Uses[id] == v)
	}
	gotos := map[string]bool{}
	ast.Inspect(lv.body, func(n ast.Node) bool {
		if b, ok := n.(*ast.BranchStmt); ok && b.Tok == token.GOTO && b.Label != nil {
			gotos[b.Label.Name] = true
		}
		return true
	})
	ast.Inspect(lv.body, func(n ast.Node) bool {
		if l, ok := n.(*ast.LabeledStmt); ok && gotos[l.Label.Name] {
			lv.labels = append(lv.labels, l)
		}
		return true
	})
	ast.Inspect(lv.body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.BranchStmt:
			if st.Tok == token.GOTO {
				lv.jumps = true
			}
		case *ast.AssignStmt:
			for j, l := range st.Lhs {
				if !is(l) {
					continue
				}
				if id := ast.Unparen(l).(*ast.Ident); fs.info.Defs[id] == v {
					lv.declared = true
				}
				switch {
				case st.Tok != token.DEFINE && st.Tok != token.ASSIGN && st.Tok != token.ADD_ASSIGN:
					lv.opaque = "assigned by " + st.Tok.String()
				case len(st.Rhs) == len(st.Lhs):
					lv.set = append(lv.set, assigned{at: st, rhs: st.Rhs[j], kills: st.Tok != token.ADD_ASSIGN})
				case len(st.Rhs) == 1:
					lv.set = append(lv.set, assigned{at: st, rhs: st.Rhs[0], multi: true, kills: true}) // v, err := f()
				}
			}
		case *ast.ValueSpec:
			for j, id := range st.Names {
				if fs.info.Defs[id] != v {
					continue
				}
				lv.declared = true
				switch {
				case len(st.Values) == 0:
					lv.set = append(lv.set, assigned{at: st, kills: true}) // its zero value
				case len(st.Values) == len(st.Names):
					lv.set = append(lv.set, assigned{at: st, rhs: st.Values[j], kills: true})
				default:
					lv.set = append(lv.set, assigned{at: st, rhs: st.Values[0], multi: true, kills: true})
				}
			}
		case *ast.RangeStmt:
			if (st.Key != nil && is(st.Key)) || (st.Value != nil && is(st.Value)) {
				lv.opaque = "range"
			}
		case *ast.UnaryExpr:
			if st.Op == token.AND && is(st.X) {
				lv.opaque = "address taken"
			}
		}
		return true
	})
	return lv
}

// localAssigns are the values assigned to v, a local variable of a string
// type (varValues), and whether they are all of them: they are not when it is
// a parameter or a result, a range's or a type switch's variable, assigned
// from a call's several results, or when its address is taken. A string is
// immutable, so what the variable holds where it is read is one of the values
// assigned to it that can reach there (reaching).
func (fs *fileScan) localAssigns(v *types.Var) localValues {
	if !isString(v.Type()) {
		return localValues{}
	}
	lv := fs.varValues(v)
	lv.ok = lv.declared && lv.opaque == ""
	for _, a := range lv.set {
		if a.multi {
			lv.ok = false
		}
		if _, ok := a.at.(*ast.FuncType); ok {
			lv.ok = false // a named result
		}
	}
	return lv
}

// reaching are the values assigned to v (localAssigns) that it can hold
// where use reads it, or nil when they cannot all be seen (lv.ok false,
// which the callers tell apart by localAssigns).
func (fs *fileScan) reaching(v *types.Var, use *ast.Ident) []ast.Expr {
	lv := fs.localAssigns(v)
	if !lv.ok {
		return nil
	}
	var out []ast.Expr
	for _, a := range lv.set {
		if fs.reaches(lv, a, use, v) {
			out = append(out, a.rhs)
		}
	}
	return out
}

// reaches: the value a assigns to v can be what v holds at use, both in
// lv.body. It is false only where control cannot carry it there: another
// assignment of v that every path from a to use runs (one in a statement
// list that holds use further on, after which a does not come before use,
// and no label a goto names lies between it and use) replaces it; or, in a
// function without a goto, a comes after use and no loop holds both, or a
// block that holds a and not use returns (or panics) after it, with no
// break or continue between. Any value assigned in a closure reaches: a closure runs when it
// is called, which may be after any assignment, and more than once. So does
// any value read in one, unless the closure itself replaces it before the
// read (err = c.Close(); if err != nil { ... } in a deferred func): control
// enters the closure only at its start, whenever it is called.
func (fs *fileScan) reaches(lv localValues, a assigned, use ast.Node, v *types.Var) bool {
	pa, pu := fs.pathTo(lv.body, a.at), fs.pathTo(lv.body, use)
	if pa == nil || pu == nil {
		return true
	}
	n := 0
	for n < len(pa) && n < len(pu) && pa[n] == pu[n] {
		n++
	}
	// A closure deferred runs as the function returns, after every line of
	// its body but those of the closures deferred: what one assigns reaches
	// only those, and the closures that may run after the function returns
	// (a goroutine's, a callback's).
	deferredUse := false
	for i := range pu {
		if _, ok := pu[i].(*ast.FuncLit); ok {
			deferredUse = true
		}
	}
	for i := n; i < len(pa); i++ {
		if deferred(pa, i) && !deferredUse {
			return false
		}
	}
	for _, x := range pa[n:] {
		if _, ok := x.(*ast.FuncLit); ok {
			return true
		}
	}
	for i := len(pu) - 1; i >= n; i-- {
		if _, ok := pu[i].(*ast.FuncLit); ok {
			// The use is in a closure a is outside of: a reaches unless
			// an assignment inside the closure replaces it on every path
			// from the closure's start to the use.
			return !fs.replaced(lv, a, pu[i:])
		}
	}
	for i, x := range pa[:n] {
		// A closure around both, of a variable declared outside it: what
		// one call assigns, the next reads, unless the closure replaces
		// it before the use (replaced). A closure called once, where it
		// is written and outside any loop or other closure (defer func()
		// { ... }()), has no next call: it reads as the lines around it
		// do.
		if fl, ok := x.(*ast.FuncLit); ok && !(fl.Pos() <= v.Pos() && v.Pos() < fl.End()) && (lv.jumps || !calledOnce(pa, i)) {
			return !fs.replaced(lv, a, pu)
		}
	}
	if fs.replaced(lv, a, pu) {
		return false
	}
	if lv.jumps {
		// A goto may carry a value to any line, before it or after a
		// return; only an assignment no label lets control round
		// replaces it.
		return true
	}
	if a.at.Pos() >= use.Pos() {
		for _, x := range pa[:n] {
			switch x.(type) {
			case *ast.ForStmt, *ast.RangeStmt:
				return true // round the loop
			}
		}
		return false
	}
	for _, x := range pa[n:] {
		list := stmtList(x)
		for j, st := range list {
			if st.Pos() <= a.at.Pos() && a.at.End() <= st.End() {
				for _, after := range list[j+1:] {
					if terminates(fs.info, after) {
						return false
					}
					if branches(after) {
						break // a break or a continue may leave first
					}
				}
				break
			}
		}
	}
	return true
}

// calledOnce: path[i] is a closure called where it is written, by a
// statement of its own (func() { ... }(), defer or go func() { ... }()), and
// no loop or other closure holds that statement: it runs once each time the
// function does.
func calledOnce(path []ast.Node, i int) bool {
	if _, ok := path[i].(*ast.FuncLit); !ok || i < 2 {
		return false
	}
	c, ok := path[i-1].(*ast.CallExpr)
	if !ok || ast.Unparen(c.Fun) != path[i] {
		return false
	}
	switch path[i-2].(type) {
	case *ast.DeferStmt, *ast.GoStmt, *ast.ExprStmt:
	default:
		return false
	}
	for _, x := range path[:i-2] {
		switch x.(type) {
		case *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
			return false
		}
	}
	return true
}

// deferred: path[i] is a closure a defer statement calls (defer func() {
// ... }()).
func deferred(path []ast.Node, i int) bool {
	if _, ok := path[i].(*ast.FuncLit); !ok || i < 2 {
		return false
	}
	c, ok := path[i-1].(*ast.CallExpr)
	if !ok || ast.Unparen(c.Fun) != path[i] {
		return false
	}
	_, ok = path[i-2].(*ast.DeferStmt)
	return ok
}

// replaced: another assignment of v replaces a's value on every path to the
// use whose path (from lv.body) is pu: a statement of a list around the use,
// before the statement that holds it, assigns v whole (kills), and a is not
// between the two in that list. Control enters a list only at its start, so
// whatever comes from before the assignment, or from after the use round a
// loop, runs it first. So does the Init of an if, a switch or a for
// statement whose body, condition or cases hold the use (if err = f(); err
// != nil { ... }): whatever comes from outside the statement runs its Init
// first.
func (fs *fileScan) replaced(lv localValues, a assigned, pu []ast.Node) bool {
	for i := 0; i+1 < len(pu); i++ {
		if init := stmtInit(pu[i]); init != nil && pu[i+1] != ast.Node(init) &&
			!(pu[i].Pos() <= a.at.Pos() && a.at.Pos() < pu[i].End()) && !labelIn(lv, init.End(), pu[len(pu)-1].Pos()) {
			for _, k := range lv.set {
				if k.kills && k.at == ast.Node(init) {
					return true
				}
			}
		}
		list := stmtList(pu[i])
		if list == nil {
			continue
		}
		u := -1
		for j, st := range list {
			if st == pu[i+1] {
				u = j
			}
		}
		if u < 0 {
			continue
		}
		for _, k := range lv.set {
			if !k.kills || k.at == a.at {
				continue
			}
			for j := 0; j < u; j++ {
				if !holdsDirectly(list[j], k.at) {
					continue
				}
				if !(list[j].Pos() <= a.at.Pos() && a.at.Pos() < list[u].End()) && !labelIn(lv, list[j].End(), pu[len(pu)-1].Pos()) {
					return true
				}
			}
		}
	}
	return false
}

// labelIn: a statement a goto names lies between from and to, where
// control can enter past an assignment without running it.
func labelIn(lv localValues, from, to token.Pos) bool {
	for _, l := range lv.labels {
		if from <= l.Pos() && l.Pos() <= to {
			return true
		}
	}
	return false
}

// branches: st holds a break, a continue or a goto (outside a closure),
// after which the statements that follow it in its list need not run.
func branches(st ast.Stmt) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.BranchStmt:
			found = true
		}
		return !found
	})
	return found
}

// holdsDirectly: st is at, or the declaration statement of at, a spec.
func holdsDirectly(st ast.Stmt, at ast.Node) bool {
	if ast.Node(st) == at {
		return true
	}
	if d, ok := st.(*ast.DeclStmt); ok {
		if g, ok := d.Decl.(*ast.GenDecl); ok {
			for _, sp := range g.Specs {
				if ast.Node(sp) == at {
					return true
				}
			}
		}
	}
	return false
}

// stmtList is the statements n runs in order: a block's, a case's.
func stmtList(n ast.Node) []ast.Stmt {
	switch b := n.(type) {
	case *ast.BlockStmt:
		return b.List
	case *ast.CaseClause:
		return b.Body
	case *ast.CommClause:
		return b.Body
	}
	return nil
}

// stmtInit is the Init statement of an if, a switch or a for statement, or
// nil.
func stmtInit(n ast.Node) ast.Stmt {
	switch s := n.(type) {
	case *ast.IfStmt:
		return s.Init
	case *ast.SwitchStmt:
		return s.Init
	case *ast.TypeSwitchStmt:
		return s.Init
	case *ast.ForStmt:
		return s.Init
	}
	return nil
}

// terminates: st leaves the function, whatever happens before it: a return,
// or a call of panic.
func terminates(info *types.Info, st ast.Stmt) bool {
	switch x := st.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.ExprStmt:
		if c, ok := x.X.(*ast.CallExpr); ok && isBuiltin(info, c.Fun, "panic") {
			return true
		}
	}
	return false
}

// pathTo is the nodes from root down to n, n last, or nil when root does not
// hold n, read from root's parents (built once per root).
func (fs *fileScan) pathTo(root *ast.BlockStmt, n ast.Node) []ast.Node {
	parents, ok := fs.s.parents[root]
	if !ok {
		parents = map[ast.Node]ast.Node{}
		var stack []ast.Node
		ast.Inspect(root, func(x ast.Node) bool {
			if x == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			if len(stack) > 0 {
				parents[x] = stack[len(stack)-1]
			}
			stack = append(stack, x)
			return true
		})
		if fs.s.parents == nil {
			fs.s.parents = map[*ast.BlockStmt]map[ast.Node]ast.Node{}
		}
		fs.s.parents[root] = parents
	}
	var path []ast.Node
	for x := n; x != nil; x = parents[x] {
		path = append(path, x)
		if x == ast.Node(root) {
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			return path
		}
	}
	return nil
}

// localSafe: v is a local string variable every value of which that can
// reach use is safe (reaching). A value still being read (a cycle, s = s +
// "x") is taken as safe, so that the other values decide.
func (fs *fileScan) localSafe(v *types.Var, use *ast.Ident) bool {
	if !fs.localAssigns(v).ok {
		return false
	}
	if fs.following == nil {
		fs.following = map[ast.Expr]bool{}
	}
	for _, r := range fs.reaching(v, use) {
		if r == nil || fs.following[r] {
			continue
		}
		fs.following[r] = true
		safe := fs.safe(r)
		delete(fs.following, r)
		if !safe {
			return false
		}
	}
	return true
}

// loggedError: e, an error (or its text), logged whole by what at at.
func (fs *fileScan) loggedError(at ast.Node, what string, e ast.Expr) {
	if fs.gatedValue(e) || fs.helperParam(fs.s.errParams, e) {
		return
	}
	if tv, ok := fs.info.Types[ast.Unparen(e)]; ok && tv.IsNil() {
		return
	}
	origin, safe := fs.origin(e, 0)
	if safe {
		return
	}
	fs.report(at, kindError, what+"("+fs.text(e)+") <- "+origin)
}

// parts calls f for e and, through concatenation and fmt.Sprint*, each value
// it is made of.
func (fs *fileScan) parts(e ast.Expr, f func(ast.Expr)) {
	e = ast.Unparen(e)
	f(e)
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			fs.parts(x.X, f)
			fs.parts(x.Y, f)
		}
	case *ast.CallExpr:
		if fn := calleeFunc(fs.info, x); fn != nil && funcPkg(fn) == "fmt" && strings.HasPrefix(fn.Name(), "Sprint") {
			for _, a := range x.Args {
				fs.parts(a, f)
			}
		}
	}
}

// safeExcept: m is safe but for the error part, a message such as
// "failed: " + err.Error(), which is reported as the error alone.
func (fs *fileScan) safeExcept(m, part ast.Expr) bool {
	m = ast.Unparen(m)
	if m == part {
		return true
	}
	switch x := m.(type) {
	case *ast.BinaryExpr:
		return x.Op == token.ADD && fs.safeOr(x.X, part) && fs.safeOr(x.Y, part)
	case *ast.CallExpr:
		if fn := calleeFunc(fs.info, x); fn != nil && funcPkg(fn) == "fmt" && strings.HasPrefix(fn.Name(), "Sprint") {
			for _, a := range x.Args {
				if !fs.safeOr(a, part) {
					return false
				}
			}
			return true
		}
	}
	return false
}

func (fs *fileScan) safeOr(e, part ast.Expr) bool {
	return fs.safe(e) || fs.safeExcept(e, part)
}

// isError: e is an error, or an error's text (err.Error()).
func (fs *fileScan) isError(e ast.Expr) bool {
	e = ast.Unparen(e)
	if c, ok := e.(*ast.CallExpr); ok {
		if f := calleeFunc(fs.info, c); f != nil && f.Name() == "Error" && len(c.Args) == 0 {
			if sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr); ok && implementsError(fs.info.TypeOf(sel.X)) {
				return true
			}
		}
	}
	t := fs.info.TypeOf(e)
	return t != nil && implementsError(t) && !safeType(t)
}

// gated: e is a value of logcontent or logurl.
func (fs *fileScan) gatedValue(e ast.Expr) bool {
	c, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok {
		return false
	}
	f := calleeFunc(fs.info, c)
	return f != nil && strings.HasPrefix(funcPkg(f), module+"/utils/logcontent")
}

// ownMarshaler: v is a zap marshaler of keploy's, whose MarshalLogObject or
// MarshalLogArray writes its values through an encoder, where the scan reads
// them.
func (fs *fileScan) ownMarshaler(v ast.Expr) bool {
	t := fs.info.TypeOf(v)
	if t == nil {
		return false
	}
	// A marshaler a keploy function makes (utils.SortedKeys) is keploy's,
	// whatever interface it is returned as.
	if c, ok := ast.Unparen(v).(*ast.CallExpr); ok && types.IsInterface(t) {
		if f := calleeFunc(fs.info, c); f != nil && strings.HasPrefix(funcPkg(f), module) &&
			(isNamed(t, "go.uber.org/zap/zapcore", "ObjectMarshaler") || isNamed(t, "go.uber.org/zap/zapcore", "ArrayMarshaler")) {
			return true
		}
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil || !strings.HasPrefix(n.Obj().Pkg().Path(), module) {
		return false
	}
	for _, m := range []string{"MarshalLogObject", "MarshalLogArray"} {
		if obj, _, _ := types.LookupFieldOrMethod(n, true, n.Obj().Pkg(), m); obj != nil {
			return true
		}
	}
	return false
}

// shapes are pkg/models' shapes of what a call sent, which write every value
// as {} or ? (models.CallShape): the forms the lines a default run prints
// for a miss name a call by.
var shapes = map[string]bool{"CallShape": true, "MissShape": true, "PathShape": true, "SQLShape": true, "URLShape": true}

// connIDName is the name of keploy's connection id, the agent's counter.
var connIDName = regexp.MustCompile(`^(?i)(client|dest|src|server)?_?conn(ection)?_?id$`)

// safe: e cannot carry an application's content.
func (fs *fileScan) safe(e ast.Expr) bool {
	e = ast.Unparen(e)
	tv, ok := fs.info.Types[e]
	if ok && (tv.Value != nil || tv.IsNil() || safeType(tv.Type)) {
		return true
	}
	if fs.gatedValue(e) || fs.wrapperParam(e) {
		return true
	}
	if fs.errOK > 0 && fs.isError(e) {
		return true // judged by its origin, where taint found it
	}
	switch x := e.(type) {
	case *ast.Ident:
		if connIDName.MatchString(x.Name) {
			return true
		}
		v, ok := fs.info.Uses[x].(*types.Var)
		if !ok {
			return false
		}
		if fs.summarizingText {
			if i := fs.summarizedParam(v); i >= 0 && !reassigned(fs.info, fs.funcs[0], v) {
				fs.passed[i] = true // its callers' to judge
				return true
			}
		}
		return fs.localSafe(v, x)
	case *ast.SelectorExpr:
		return connIDName.MatchString(x.Sel.Name)
	case *ast.TypeAssertExpr: // ctx.Value(models.ClientConnectionIDKey).(string)
		if c, ok := ast.Unparen(x.X).(*ast.CallExpr); ok && len(c.Args) == 1 {
			if f := calleeFunc(fs.info, c); f != nil && f.Name() == "Value" && recvName(f) == "context.Context" {
				return connIDKey(c.Args[0])
			}
		}
	case *ast.IndexExpr:
		// metadata["connID"]: the connection id under its name.
		if tv, ok := fs.info.Types[x.Index]; ok && tv.Value != nil {
			if k, err := strconv.Unquote(tv.Value.ExactString()); err == nil && connIDName.MatchString(k) {
				return true
			}
		}
		// dns.TypeToString[q.Qtype]: a library's table, by a safe key.
		if v := tableVar(fs.info, x.X); v != nil && !strings.HasPrefix(v.Pkg().Path(), module) {
			return fs.safe(x.Index)
		}
	case *ast.BinaryExpr:
		return x.Op == token.ADD && fs.safe(x.X) && fs.safe(x.Y)
	case *ast.CallExpr:
		if tv, ok := fs.info.Types[x.Fun]; ok && tv.IsType() && len(x.Args) == 1 {
			return fs.safe(x.Args[0]) // a conversion
		}
		f := calleeFunc(fs.info, x)
		if f == nil {
			return false
		}
		pkg := funcPkg(f)
		if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok && f.Name() == "String" && len(x.Args) == 0 {
			if t := fs.info.TypeOf(sel.X); t != nil && safeType(t) {
				return true
			}
		}
		switch {
		case pkg == "strconv" && (strings.HasPrefix(f.Name(), "Format") || strings.HasPrefix(f.Name(), "Append") || f.Name() == "Itoa"):
			return fs.allSafe(x.Args)
		case pkg == "fmt" && (f.Name() == "Sprint" || f.Name() == "Sprintln"):
			return fs.allSafe(x.Args)
		case pkg == "fmt" && f.Name() == "Sprintf":
			if len(x.Args) == 0 || !fs.safe(x.Args[0]) {
				return false
			}
			verbs := formatVerbs(fs.info, x.Args[0])
			for i, a := range x.Args[1:] {
				if i < len(verbs) && verbs[i] == 'T' {
					continue
				}
				if !fs.safe(a) {
					return false
				}
			}
			return true
		case pkg == "reflect" && f.Name() == "TypeOf":
			return true
		case pkg == "net/http" && f.Name() == "StatusText":
			return true // a status's standard text
		case pkg == module+"/pkg/models" && shapes[f.Name()]:
			return true // a call's, a path's or a statement's shape, which keeps no value
		}
		if strings.HasPrefix(pkg, module) && isString(fs.info.TypeOf(x)) {
			return fs.textSafe(x, f)
		}
	}
	return false
}

// textSafe: c, a call of f, keploy's, returns a safe text: f's own text is
// safe (textSummary), and so is each argument it puts in it. An error among
// them is judged by its origin, or, under errSafe, where taint finds it.
func (fs *fileScan) textSafe(c *ast.CallExpr, f *types.Func) bool {
	ok, passes := fs.s.textSummary(f)
	if !ok {
		return false
	}
	for i := range passes {
		a := callArg(fs.info, c, f, i)
		if a == nil {
			return false
		}
		if fs.isError(a) {
			if fs.errOK > 0 {
				continue
			}
			if _, safe := fs.origin(a, 1); !safe {
				return false
			}
			continue
		}
		if !fs.safe(a) {
			return false
		}
	}
	return true
}

// callArg is the argument c gives f's i-th parameter, or nil when it cannot
// be read apart (f(g()), a variadic's values).
func callArg(info *types.Info, c *ast.CallExpr, f *types.Func, i int) ast.Expr {
	sig, _ := f.Type().(*types.Signature)
	if sig == nil || i >= len(c.Args) || (sig.Variadic() && i >= sig.Params().Len()-1) {
		return nil
	}
	if len(c.Args) == 1 && isTuple(info.TypeOf(c.Args[0])) {
		return nil
	}
	return c.Args[i]
}

// textSummary: what f, keploy's, returns as its one result, a text, is safe
// but for the parameters it puts in it, whose indices it returns: each
// return is safe (errSafe), and so is the origin of every error whose text
// it holds (taintOrigin), a parameter of f's being passed to its callers to
// judge. A method's receiver is read as any value is (its enum and number
// fields are safe, its strings are not), and an interface's method, a
// function value or one still being read (a recursion) is not safe.
func (s *scanner) textSummary(f *types.Func) (bool, map[int]bool) {
	f = f.Origin()
	if v, ok := s.text[f]; ok {
		return v > 0, s.textPasses[f]
	}
	s.text[f] = 0 // in progress: not safe
	if s.computeText(f) {
		s.text[f] = 1
	} else {
		s.text[f] = -1
	}
	return s.text[f] > 0, s.textPasses[f]
}

func (s *scanner) computeText(f *types.Func) bool {
	sig, _ := f.Type().(*types.Signature)
	if sig == nil || sig.Results().Len() != 1 || !isString(sig.Results().At(0).Type()) {
		return false
	}
	d, ok := s.decls[f]
	if !ok {
		return false
	}
	fs := &fileScan{s: s, p: d.p, info: d.p.TypesInfo, fset: d.p.Fset, file: d.file, funcs: []ast.Node{d.decl},
		summarizing: sig, passed: map[int]bool{}, summarizingText: true}
	safe := true
	ast.Inspect(d.decl.Body, func(n ast.Node) bool {
		if !safe {
			return false
		}
		switch r := n.(type) {
		case *ast.FuncLit:
			return false // a closure's returns are its own
		case *ast.ReturnStmt:
			if len(r.Results) != 1 || !fs.errSafe(r.Results[0]) {
				safe = false // a bare return of a named result, or a value that is not safe
			} else if _, ok := fs.taintOrigin(r.Results[0], 1); !ok {
				safe = false
			}
			return false
		}
		return true
	})
	if safe && len(fs.passed) > 0 {
		s.textPasses[f] = fs.passed
	}
	return safe
}

// connIDKey: k names a context's connection id (models.ClientConnectionIDKey).
func connIDKey(k ast.Expr) bool {
	var name string
	switch x := ast.Unparen(k).(type) {
	case *ast.Ident:
		name = x.Name
	case *ast.SelectorExpr:
		name = x.Sel.Name
	}
	return connIDName.MatchString(strings.TrimSuffix(name, "Key"))
}

// tableVar is the package-level variable e names, if it does.
func tableVar(info *types.Info, e ast.Expr) *types.Var {
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	}
	if id == nil {
		return nil
	}
	v, ok := info.Uses[id].(*types.Var)
	if !ok || v.Pkg() == nil || v.Parent() != v.Pkg().Scope() {
		return nil
	}
	return v
}

func (fs *fileScan) allSafe(args []ast.Expr) bool {
	for _, a := range args {
		if !fs.safe(a) {
			return false
		}
	}
	return true
}

// formatVerbs is the verb each argument of a constant format takes, in
// order: 'T' for %T. A format it cannot read gives none.
func formatVerbs(info *types.Info, format ast.Expr) []rune {
	tv, ok := info.Types[ast.Unparen(format)]
	if !ok || tv.Value == nil {
		return nil
	}
	s, err := strconv.Unquote(tv.Value.ExactString())
	if err != nil {
		return nil
	}
	var verbs []rune
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		i++
		for i < len(s) && strings.ContainsRune("+-# 0123456789.*[]", rune(s[i])) {
			if s[i] == '*' {
				verbs = append(verbs, '*')
			}
			i++
		}
		if i < len(s) && s[i] != '%' {
			r, _ := utf8.DecodeRuneInString(s[i:])
			verbs = append(verbs, r)
		}
	}
	return verbs
}

// safeOrigins are the packages whose errors name only what keploy chose or
// the system says: a path, an address, an errno, the kernel's verdict, a
// process's exit. An error from a call into one of them is safe. A package
// whose errors quote what they read (strconv, encoding/json, yaml, net/http,
// net/url, text/template, regexp, the database drivers, ...) is not here.
var safeOrigins = map[string]bool{
	"os": true, "os/exec": true, "os/signal": true, "os/user": true, "io/fs": true, "path/filepath": true,
	"syscall": true, "golang.org/x/sys/unix": true, "golang.org/x/sys/windows": true,
	"net":     true, // sockets: *net.OpError names the op, the addresses and an errno
	"context": true, "time": true, "runtime": true, "runtime/debug": true,
	"github.com/cilium/ebpf": true, "github.com/cilium/ebpf/link": true, "github.com/cilium/ebpf/ringbuf": true,
	"github.com/cilium/ebpf/perf": true, "github.com/cilium/ebpf/rlimit": true, "github.com/cilium/ebpf/features": true,
}

func init() {
	for _, p := range []string{
		"io", "bufio", // a reader's and a writer's errors, and their own sentinels
		"compress/gzip", "compress/flate", "compress/zlib", "github.com/andybalholm/brotli",
		"github.com/klauspost/compress/zstd", "github.com/klauspost/compress/s2", "github.com/golang/snappy",
		"crypto/tls", "crypto/x509", // a handshake's and a certificate's: names and verdicts, never the payload
		"github.com/spf13/pflag", "github.com/spf13/cobra", // a flag's name and type
		"github.com/docker/docker/client", // the daemon's: containers', images' and networks' names
		"github.com/miekg/dns",            // a resolver's: a name, an address, a timeout
		"encoding/gob",                    // a type's name
		"encoding/base64",                 // the byte offset it could not decode
		"github.com/k0kubun/pp/v3",        // a print's I/O
	} {
		safeOrigins[p] = true
	}
}

// safeOriginFuncs are the functions and methods, of packages whose other
// errors can quote what they read, whose errors cannot: encoding's name the
// type they could not encode; a request's or a response's Write fails as the
// connection it writes to does.
var safeOriginFuncs = map[string]bool{
	"encoding/json.Marshal": true, "encoding/json.MarshalIndent": true, "encoding/json.Encoder.Encode": true,
	"gopkg.in/yaml.v3.Marshal": true, "gopkg.in/yaml.v2.Marshal": true, "gopkg.in/yaml.v3.Encoder.Encode": true, "gopkg.in/yaml.v3.Encoder.Close": true,
	"gopkg.in/yaml.v3.Node.Encode": true,
	"net/http.Request.Write":       true, "net/http.Response.Write": true, "net/http.ResponseWriter.Write": true,
	"net/http.Server.Shutdown": true, "net/http.Server.Close": true,
}

// safeOriginTypes are the interfaces whose methods' errors are I/O's:
// a reader's, a writer's or a closer's, a socket's.
var safeOriginTypes = map[string]bool{
	"io.Reader": true, "io.Writer": true, "io.Closer": true, "io.ReadCloser": true, "io.WriteCloser": true,
	"io.ReadWriter": true, "io.ReadWriteCloser": true, "io.ReaderFrom": true, "io.WriterTo": true,
	"net.Conn": true, "net.Listener": true, "net.PacketConn": true, "os.File": true,
	"net.TCPConn": true, "net.UDPConn": true, "net.UnixConn": true, "net.TCPListener": true, "net.UnixListener": true,
	"context.Context": true, "os/exec.Cmd": true, "os.Process": true,
}

// origin names where the error e came from, and whether that origin cannot
// hold content (see safeOrigins), following a variable to the last value
// assigned to it before e in the functions around it.
func (fs *fileScan) origin(e ast.Expr, depth int) (string, bool) {
	e = ast.Unparen(e)
	if tv, ok := fs.info.Types[e]; ok && tv.IsNil() {
		return "nil", true // err = nil: no error, and no text
	}
	if depth > 4 {
		return "?", false
	}
	switch x := e.(type) {
	case *ast.CallExpr:
		f := calleeFunc(fs.info, x)
		if f == nil {
			return "call " + fs.text(x.Fun), false
		}
		if f.Name() == "Error" && len(x.Args) == 0 { // err.Error()
			if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok && implementsError(fs.info.TypeOf(sel.X)) {
				return fs.origin(sel.X, depth)
			}
		}
		return fs.callOrigin(x, f, depth)
	case *ast.Ident:
		v, ok := fs.info.Uses[x].(*types.Var)
		if !ok {
			return "?", false
		}
		if o, safe, ok := fs.packageVar(v, depth); ok {
			return o, safe
		}
		return fs.localOrigin(x, v, depth)
	case *ast.SelectorExpr:
		if v, ok := fs.info.Uses[x.Sel].(*types.Var); ok {
			if o, safe, ok := fs.packageVar(v, depth); ok {
				return o, safe
			}
			if v.IsField() {
				return "field " + fs.text(x), false
			}
		}
	case *ast.TypeAssertExpr:
		return "assert " + fs.text(x), false
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			if lit, ok := ast.Unparen(x.X).(*ast.CompositeLit); ok {
				return fs.origin(lit, depth)
			}
		}
	case *ast.CompositeLit:
		// An error value written out (deadlineError{}, &OpError{Op: "read",
		// Err: err}): as safe as what it holds. The standard library's
		// errors of an op on a file or a socket hold, besides the error
		// they wrap, an op's name, a path and an address (opErrorType),
		// which are no content.
		opErr := opErrorType(fs.info.TypeOf(x))
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			if fs.isError(el) {
				if o, safe := fs.origin(el, depth+1); !safe {
					return fs.text(x.Type) + "{" + o + "}", false
				}
			} else if !opErr && !fs.safe(el) {
				return fs.text(x), false
			}
		}
		return fs.text(x.Type) + "{}", true
	}
	return fs.text(e), false
}

// opErrorType: t is one of the standard library's errors of an op on a file
// or a socket, whose fields other than the error it wraps are the op's name,
// the path and the addresses it was on: fs.PathError (os.PathError),
// os.LinkError, os.SyscallError and net.OpError.
func opErrorType(t types.Type) bool {
	for _, n := range [][2]string{{"io/fs", "PathError"}, {"os", "LinkError"}, {"os", "SyscallError"}, {"net", "OpError"}} {
		if t != nil && isNamed(t, n[0], n[1]) {
			return true
		}
	}
	return false
}

// packageVar is the origin of v when it is a package-level variable: a
// sentinel. keploy's is what its initializer is; another package's error
// variable is its sentinel, whose text is a constant (io.EOF,
// sql.ErrNoRows).
func (fs *fileScan) packageVar(v *types.Var, depth int) (string, bool, bool) {
	if v.Pkg() == nil || v.Parent() != v.Pkg().Scope() {
		return "", false, false
	}
	if init, ok := fs.s.inits[v]; ok {
		// Read in its own package: a sentinel of another package's is
		// typed by that package's load, not this file's.
		in := &fileScan{s: fs.s, p: init.p, info: init.p.TypesInfo, fset: init.p.Fset, file: init.file}
		o, safe := in.origin(init.expr, depth+1)
		return "var " + qualVar(v) + " = " + o, safe, true
	}
	if !strings.HasPrefix(v.Pkg().Path(), module) {
		return "var " + qualVar(v), true, true
	}
	return "var " + qualVar(v), false, true // keploy's, declared without a value
}

func (fs *fileScan) callOrigin(c *ast.CallExpr, f *types.Func, depth int) (string, bool) {
	pkg, name := funcPkg(f), qualFunc(f)
	switch {
	case pkg == "errors" && f.Name() == "New":
		what, bad := fs.errorsNewText(c)
		safe := !bad || fs.listedText(kindErrorsNew, what)
		if safe && len(c.Args) == 1 {
			// errors.New(err.Error()), errors.New(why): the errors whose
			// text it is
			if o, ok := fs.taintOrigin(c.Args[0], depth); !ok {
				return "errors.New(" + o + ")", false
			}
		}
		return "errors.New", safe
	case pkg == "fmt" && f.Name() == "Errorf":
		what, bad := fs.errorfText(c)
		safe := !bad || fs.listedText(kindErrorf, what)
		var wrapped []string
		for _, a := range c.Args[1:] {
			if fs.isError(a) {
				o, s := fs.origin(a, depth+1)
				wrapped = append(wrapped, o)
				safe = safe && s
			}
		}
		if safe {
			// The errors whose text its format or a value holds (a
			// stopReason that formatted one): an entry that lists the
			// format as keploy's words does not stand for them.
			for _, a := range c.Args {
				if fs.isError(a) {
					continue
				}
				if o, ok := fs.taintOrigin(a, depth); !ok {
					wrapped = append(wrapped, o)
					safe = false
					break
				}
			}
		}
		if len(wrapped) == 0 {
			return "fmt.Errorf", safe
		}
		return "fmt.Errorf(" + strings.Join(wrapped, ", ") + ")", safe
	case pkg == "errors" && f.Name() == "Join":
		safe := true
		var parts []string
		for _, a := range c.Args {
			o, s := fs.origin(a, depth+1)
			parts = append(parts, o)
			safe = safe && s
		}
		return "errors.Join(" + strings.Join(parts, ", ") + ")", safe
	case pkg == "context" && (f.Name() == "Cause"):
		return name, true
	}
	if safeOriginFuncs[pkg+"."+f.Name()] && recvName(f) == "" {
		return name, true
	}
	if r := recvName(f); r != "" && (safeOriginTypes[r] || safeOriginFuncs[r+"."+f.Name()]) {
		return name, true
	}
	if safeOrigins[pkg] {
		return name, true
	}
	if strings.HasPrefix(pkg, module) {
		if !fs.s.returnsSafe(f) {
			return name, false
		}
		if i, ok := printfWrapper(f); ok {
			// framingLost(format, args...): the error it makes formats
			// its values (with %v, which wraps nothing), so it is as
			// safe as the errors among them and those whose text one
			// holds. A call that passes its own args... on is its
			// callers' to judge, as fmt.Errorf's is.
			for _, a := range c.Args[i:] {
				o, s := "", true
				if fs.isError(a) {
					o, s = fs.origin(a, depth+1)
				} else {
					o, s = fs.taintOrigin(a, depth)
				}
				if !s {
					return name + "(" + o + ")", false
				}
			}
		}
		// A parameter it returns is as safe as the argument it is given.
		for i := range fs.s.passes[f.Origin()] {
			arg := (ast.Expr)(nil)
			switch {
			case len(c.Args) == 1 && isTuple(fs.info.TypeOf(c.Args[0])):
				arg = c.Args[0] // f(g()): g's error is the one passed
			case i < len(c.Args):
				arg = c.Args[i]
			default:
				return name, false
			}
			if o, safe := fs.origin(arg, depth+1); !safe {
				return name + "(" + o + ")", false
			}
		}
		return name, true
	}
	return name, false
}

// returnsSafe: every error f returns has a safe origin. f is keploy's: a
// function or a method whose declaration the load holds, or an interface's
// method, which is safe when it has implementations in keploy and each of
// them is. A function the scan is still reading (a recursion) is taken as
// safe, so that its other returns decide.
func (s *scanner) returnsSafe(f *types.Func) bool {
	f = f.Origin()
	if v, ok := s.safe[f]; ok {
		return v >= 0
	}
	s.safe[f] = 0 // in progress
	ok := s.computeSafe(f)
	if !ok && s.why != nil && s.why[f] == "" {
		s.why[f] = "?"
	}
	if ok {
		s.safe[f] = 1
	} else {
		s.safe[f] = -1
	}
	return ok
}

func (s *scanner) computeSafe(f *types.Func) bool {
	sig, _ := f.Type().(*types.Signature)
	if sig == nil {
		return false
	}
	if recv := sig.Recv(); recv != nil && types.IsInterface(recv.Type()) {
		iface, _ := recv.Type().Underlying().(*types.Interface)
		if iface == nil {
			return false
		}
		impls := 0
		for _, n := range s.named {
			if testDouble(n) {
				continue // a test's stand-in, which returns what the test tells it to
			}
			var impl types.Type
			switch {
			case types.Implements(n, iface):
				impl = n
			case types.Implements(types.NewPointer(n), iface):
				impl = types.NewPointer(n)
			default:
				continue
			}
			obj, _, _ := types.LookupFieldOrMethod(impl, false, f.Pkg(), f.Name())
			m, ok := obj.(*types.Func)
			if !ok {
				return false
			}
			if _, declared := s.decls[m.Origin()]; !declared {
				s.why[f] = "impl not keploy's: " + qualFunc(m)
				return false // promoted from a type outside keploy, or not loaded
			}
			impls++
			if !s.returnsSafe(m) {
				s.why[f] = "impl " + qualFunc(m)
				return false
			}
			for i := range s.passes[m.Origin()] {
				if s.passes[f] == nil {
					s.passes[f] = map[int]bool{}
				}
				s.passes[f][i] = true
			}
		}
		if impls == 0 {
			s.why[f] = "no implementation in keploy"
		}
		return impls > 0
	}
	d, ok := s.decls[f]
	if !ok {
		return false
	}
	errAt := map[int]bool{}
	for i := 0; i < sig.Results().Len(); i++ {
		if implementsError(sig.Results().At(i).Type()) {
			errAt[i] = true
		}
	}
	if len(errAt) == 0 {
		return true
	}
	fs := &fileScan{s: s, p: d.p, info: d.p.TypesInfo, fset: d.p.Fset, file: d.file, funcs: []ast.Node{d.decl},
		summarizing: sig, passed: map[int]bool{}}
	defer func() {
		if len(fs.passed) > 0 {
			s.passes[f] = fs.passed
		}
	}()
	safe := true
	ast.Inspect(d.decl.Body, func(n ast.Node) bool {
		if !safe {
			return false
		}
		switch r := n.(type) {
		case *ast.FuncLit:
			return false // a closure's returns are its own
		case *ast.ReturnStmt:
			if len(r.Results) == 0 { // a bare return: the named results
				for i := range errAt {
					v := sig.Results().At(i)
					at := ast.NewIdent(v.Name())
					at.NamePos = r.Pos()
					if o, ok := fs.namedResultOrigin(at, v); !ok {
						safe = false
						s.why[f] = fs.at(r) + o
					}
				}
				return false
			}
			if len(r.Results) == 1 && sig.Results().Len() > 1 {
				// return f(): the call's results are f's
				if o, ok := fs.origin(r.Results[0], 1); !ok {
					safe = false
					s.why[f] = fs.at(r) + o
				}
				return false
			}
			for i, e := range r.Results {
				if !errAt[i] {
					continue
				}
				if tv, ok := fs.info.Types[ast.Unparen(e)]; ok && tv.IsNil() {
					continue
				}
				if o, ok := fs.origin(e, 1); !ok {
					safe = false
					s.why[f] = fs.at(r) + o
				}
			}
			return false
		}
		return true
	})
	return safe
}

// testDouble: n is a testify mock (it embeds mock.Mock), which keploy's
// tests stand in for an interface's implementation with: its errors are the
// ones a test gives it, and no binary keploy ships calls it.
func testDouble(n *types.Named) bool {
	st, ok := n.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := 0; i < st.NumFields(); i++ {
		if f := st.Field(i); f.Embedded() && isNamed(f.Type(), "github.com/stretchr/testify/mock", "Mock") {
			return true
		}
	}
	return false
}

// namedResultOrigin: the origin of v, a named result, at a bare return: the
// last value assigned to it before, or nil when nothing is.
func (fs *fileScan) namedResultOrigin(use *ast.Ident, v *types.Var) (string, bool) {
	o, safe := fs.localOrigin(use, v, 1)
	if o == "result "+v.Name() || o == "unassigned" {
		return "nil", true
	}
	return o, safe
}

// localOrigin follows v, a local variable or a parameter, to every value
// assigned to it that can reach use (varValues, reaches): each must have a
// safe origin, and the first that does not is named. A parameter is its
// callers' to judge only while its function is summarized (returnsSafe).
func (fs *fileScan) localOrigin(use *ast.Ident, v *types.Var, depth int) (string, bool) {
	for i := len(fs.funcs) - 1; i >= 0; i-- {
		fn := fs.funcs[i]
		var ftype *ast.FuncType
		switch f := fn.(type) {
		case *ast.FuncDecl:
			ftype = f.Type
		case *ast.FuncLit:
			ftype = f.Type
		}
		if ftype.Params == nil {
			continue
		}
		for _, fl := range ftype.Params.List {
			for _, n := range fl.Names {
				if fs.info.Defs[n] == v {
					if i := fs.summarizedParam(v); i >= 0 && !reassigned(fs.info, fn, v) {
						fs.passed[i] = true
						return "param " + n.Name, true
					}
					return "param " + n.Name, false
				}
			}
		}
	}
	lv := fs.varValues(v)
	if !lv.declared {
		return "unassigned", false
	}
	if lv.opaque != "" {
		return lv.opaque + " " + v.Name(), false
	}
	// Every unsafe value is named, so that what the allowlist says of the
	// error is said of each.
	first := ""
	var bad []string
	for _, a := range lv.set {
		if !fs.reaches(lv, a, use, v) {
			continue
		}
		if a.rhs == nil {
			continue // a zero value: no error
		}
		o, safe := fs.assignedOrigin(a.rhs, use, v, depth)
		if !safe {
			if !slices.Contains(bad, o) {
				bad = append(bad, o)
			}
			continue
		}
		if first == "" {
			first = o
		}
	}
	if len(bad) > 0 {
		return strings.Join(bad, " & "), false
	}
	if first == "" {
		first = "nil"
	}
	return first, true
}

// assignedOrigin is the origin of rhs, a value assigned to v.
func (fs *fileScan) assignedOrigin(rhs ast.Expr, use *ast.Ident, v *types.Var, depth int) (string, bool) {
	if c, ok := ast.Unparen(rhs).(*ast.CallExpr); ok {
		if f := calleeFunc(fs.info, c); f != nil {
			if f.Name() == "Error" && len(c.Args) == 0 {
				return fs.origin(c, depth+1)
			}
			return fs.callOrigin(c, f, depth+1)
		}
		return "call " + fs.text(c.Fun), false
	}
	if id, ok := ast.Unparen(rhs).(*ast.Ident); ok && id != use {
		if v2, ok := fs.info.Uses[id].(*types.Var); ok && v2 == v {
			return "nil", true // v = v: its other values decide
		}
	}
	return fs.origin(rhs, depth+1)
}

func isTuple(t types.Type) bool {
	_, ok := t.(*types.Tuple)
	return ok
}

// summarizedParam is the index of v among the parameters of the function
// being summarized, or -1.
func (fs *fileScan) summarizedParam(v *types.Var) int {
	if fs.summarizing == nil {
		return -1
	}
	for i := 0; i < fs.summarizing.Params().Len(); i++ {
		if fs.summarizing.Params().At(i) == v {
			if fs.summarizing.Variadic() && i == fs.summarizing.Params().Len()-1 {
				return -1 // ...error: judged as unknown
			}
			return i
		}
	}
	return -1
}

// reassigned: v, a parameter, is assigned in fn's body, so what it holds at
// a return need not be the argument.
func reassigned(info *types.Info, fn ast.Node, v *types.Var) bool {
	var body *ast.BlockStmt
	switch f := fn.(type) {
	case *ast.FuncDecl:
		body = f.Body
	case *ast.FuncLit:
		body = f.Body
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, l := range as.Lhs {
				if id, ok := ast.Unparen(l).(*ast.Ident); ok && info.Uses[id] == v {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// calleeFunc is the function a call calls, or nil for a call of a value or
// a conversion.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	var id *ast.Ident
	switch fn := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fn
	case *ast.SelectorExpr:
		id = fn.Sel
	case *ast.IndexExpr:
		switch g := fn.X.(type) {
		case *ast.SelectorExpr:
			id = g.Sel
		case *ast.Ident:
			id = g
		}
	case *ast.IndexListExpr:
		switch g := fn.X.(type) {
		case *ast.SelectorExpr:
			id = g.Sel
		case *ast.Ident:
			id = g
		}
	}
	if id == nil {
		return nil
	}
	f, _ := info.Uses[id].(*types.Func)
	return f
}

func funcPkg(f *types.Func) string {
	if f.Pkg() == nil {
		return ""
	}
	return f.Pkg().Path()
}

// recvName is the package-qualified name of f's receiver's type, or "".
func recvName(f *types.Func) string {
	sig, _ := f.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return ""
	}
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok && n.Obj().Pkg() != nil {
		return n.Obj().Pkg().Path() + "." + n.Obj().Name()
	}
	return t.String()
}

func qualFunc(f *types.Func) string {
	var n string
	if r := recvName(f); r != "" {
		n = "(" + r + ")." + f.Name()
	} else {
		n = funcPkg(f) + "." + f.Name()
	}
	return strings.ReplaceAll(n, module+"/", "")
}

func qualVar(v *types.Var) string {
	if v.Pkg() == nil {
		return v.Name()
	}
	return strings.TrimPrefix(v.Pkg().Path(), module+"/") + "." + v.Name()
}

func isNamed(t types.Type, pkg, name string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if a, ok := t.(*types.Alias); ok {
		t = types.Unalias(a)
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

var errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

func implementsError(t types.Type) bool {
	return t != nil && types.Implements(t, errorType)
}

// formatsAsText: a value of type t, formatted by fmt, can write an
// application's text: a string, a []byte, and anything that holds one or
// may (a struct, a map, a slice, an array, a pointer to one, an interface).
func formatsAsText(t types.Type) bool {
	return t == nil || !safeType(t)
}

// safeType: a value of type t cannot hold an application's text (see the
// package doc).
func safeType(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if b, ok := t.Underlying().(*types.Basic); ok && b.Info()&(types.IsNumeric|types.IsBoolean) != 0 {
		return true
	}
	switch u := t.(type) {
	case *types.Pointer:
		return safeType(u.Elem())
	case *types.Slice: // pids, ports: a list of safe values, but not bytes or runes, which are text
		return !textElem(u.Elem()) && safeType(u.Elem())
	case *types.Array:
		return !textElem(u.Elem()) && safeType(u.Elem())
	}
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	path := n.Obj().Pkg().Path()
	switch path + "." + n.Obj().Name() {
	case "time.Duration", "time.Time", "time.Month", "time.Weekday",
		"net.IP", "net.IPMask", "net.IPNet", "net.TCPAddr", "net.UDPAddr", "net.IPAddr", "net.UnixAddr", "net.Addr",
		"net/netip.Addr", "net/netip.AddrPort", "net/netip.Prefix",
		"os.FileMode", "io/fs.FileMode", "os.Signal", "syscall.Signal", "reflect.Type", "reflect.Kind":
		return true
	}
	if b, ok := n.Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 && strings.HasPrefix(path, module) {
		return hasConsts(n)
	}
	return false
}

// textElem: a slice or an array of t is text ([]byte, []rune).
func textElem(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && (b.Kind() == types.Uint8 || b.Kind() == types.Int32)
}

// hasConsts: n's package declares constants of type n: n is an enum.
func hasConsts(n *types.Named) bool {
	sc := n.Obj().Pkg().Scope()
	for _, name := range sc.Names() {
		if c, ok := sc.Lookup(name).(*types.Const); ok && types.Identical(c.Type(), n) {
			return true
		}
	}
	return false
}
