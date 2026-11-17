// Package cmdflow follows the command keploy runs for the user through
// keploy's Go code, from where keploy reads it to every log line, error and
// console write it reaches, and says where a line at INFO or above, an
// error, or a DEBUG line with no redactor between, takes it. Its tests run it
// over the module. It reads the code's syntax alone (go/parser, no type
// check), so it runs in a few seconds; what that leaves out is its stated
// limits (Trace). Only _test.go files import it, so no keploy binary links
// it.
package cmdflow

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Kind is what a value holds of the command: none, its text with its
// secrets redacted (RedactCommandLine's: its hosts, paths and users still
// whole), or its raw text.
type Kind int

const (
	None Kind = iota
	Redacted
	Raw
)

// Rules name, by a package's directory relative to the module's root (""
// for the root's own package) and a type's or function's name, where the
// command comes from and what a value made of it is.
type Rules struct {
	// Module is the module's path (go.mod's module line).
	Module string
	// Fields are the struct fields that hold the command, "dir.Type.Field":
	// the config's Command, a run's AppCommand.
	Fields map[string]Kind
	// Methods are the methods, by name, whose result holds it
	// (AppCommand's WholeUnredacted, WholeRedacted).
	Methods map[string]Kind
	// Redactors are the functions, "dir.Func", whose result is a command's
	// text with its secrets redacted, whatever they were given.
	Redactors map[string]bool
	// Sanitizers are the functions, "dir.Func" or "dir.Type.Method", whose
	// results hold nothing of the command a line may not name, each with
	// why: AppCommand (its program and how many words follow it), and the
	// names keploy parses out of a docker or compose command. "dir.Func#i"
	// names its result i alone (a parser that also returns the command it
	// rewrote).
	Sanitizers map[string]string
	// Allowed are places a line may take what it is given, "dir.Func: expr"
	// (Finding's Func and Expr), each with why: a parser's own error that
	// names the value it could not read.
	Allowed map[string]string
	// AllowedRedacted are places, as Allowed's, a line may take the command
	// redacted (a Redactor's result), never raw: a test report printed as
	// its file holds it.
	AllowedRedacted map[string]string
	// Sinks are the module's own logging helpers, "dir.Func", with the
	// level they log at ("DEBUG", or a level above it): a call of one is a
	// sink, its arguments read there, and it is not followed into.
	Sinks map[string]string
	// SanitizerTypes are types, "dir.Type", whose methods' results hold
	// nothing of the command a line may not name, but for the Methods'.
	SanitizerTypes map[string]string
	// Skip are directories, relative to the root, that are no part of
	// keploy's binaries (e2e programs).
	Skip []string
	// MaxMethods is how many methods of one name a call by that name is
	// followed into, when the receiver's type is not known.
	MaxMethods int
}

// Finding is a place the command reaches that it may not.
type Finding struct {
	Pos  token.Position
	Func string // the function the place is in
	Sink string // what takes it: INFO+, DEBUG, error, console
	Kind Kind
	Expr string // the argument that holds it
}

func (f Finding) String() string {
	what := "the command"
	if f.Kind == Redacted {
		what = "the command whole (redacted)"
	}
	return fmt.Sprintf("%s: %s: a %s line takes %s: %s", f.Pos, f.Func, f.Sink, what, f.Expr)
}

type file struct {
	dir     string
	imports map[string]string // name -> import path
	ast     *ast.File
}

type fn struct {
	key     string // dir.Func or dir.Type.Method
	name    string
	recv    string
	dir     string
	decl    *ast.FuncDecl
	file    *file
	params  []string
	ptypes  []ast.Expr
	results int
}

type tracer struct {
	r     Rules
	fset  *token.FileSet
	funcs []*fn
	// byDir are a package's functions by name, dir|Func: each file's under
	// its build tags (signal_others.go's and signal_windows.go's
	// ExecuteCommand), every one of which a call is followed into.
	byDir   map[string][]*fn
	methods map[string][]*fn // Method name
	structs map[string]*structDecl
	// strings are the module's types declared as a string, "dir.Type"
	// (type Line string): a parameter of one may hold the command.
	strings map[string]bool
	// vars are the module's package-level variables, "dir|Name": what one
	// holds is a field's, under "var:dir|Name".
	vars map[string]bool
	// The state the fixed point grows: a parameter's kind, a function's
	// result's (from its sources, and from its parameters), a field's.
	params  map[*fn][]Kind
	fromSrc map[*fn][]Kind
	// fromArg[f][p] are f's results' kinds when its parameter p is given
	// the command raw.
	fromArg  map[*fn]map[int][]Kind
	fields   map[string]Kind
	changed  bool
	findings []Finding
}

type structDecl struct {
	st   *ast.StructType
	file *file
}

// Trace follows the command through every non-test Go file under root, and
// returns where a line it may not reach takes it:
//   - a log call at INFO, WARN, ERROR, FATAL or PANIC (zap's Info, Warn,
//     Error, Fatal, Panic, DPanic and their f, w and ln forms, With, Named,
//     WithOptions (a logger they return is a logger still), Log and
//     Check(...).Write at any level but debug, a variable that holds one of
//     these methods; log/slog's Info, Warn, Error, their Context forms, Log
//     and LogAttrs at any level but debug, and the same methods of a
//     *slog.Logger; and the Sinks the rules name, such as keploy's
//     LogError), an error (fmt.Errorf, errors.New, pkg/errors' Wrap, Wrapf,
//     WithMessage), a write (fmt.Print*, log's Print*, Fatal* and Panic*,
//     a *log.Logger's Print*, Fatal*, Panic* and Output, the prefix of
//     log.New, log.SetPrefix and a logger's SetPrefix, pp's Print*, and
//     fmt.Fprint*, io.WriteString and pp's Fprint* to any writer, and Write
//     or WriteString of os.Stdout, os.Stderr or a writer named console,
//     stdout, stderr or tty) or panic, that takes the command, raw or
//     redacted, or a struct printed whole whose type holds it in a field
//     (zap.Any("cfg", cfg), %+v of *cfg);
//   - a log call at DEBUG (zap's, slog's) that takes it raw (not through a
//     Redactor).
//
// It starts at the Rules' Fields, the Methods' results (a call, a method
// value, a method expression) and os.Args, and follows the command through
// assignments (a variable by its name in its function, a package-level
// variable by its name in its package, and its initializer: what it gives
// the variable, and a line it writes; one that holds a function literal or
// a method value or expression of the Methods is followed as a function of
// its name), a struct's fields (by the struct's
// type where it can tell it, else an unexported field by its package and
// name; a struct variable whose field is set holds what it was set to),
// composite literals (a keyed one holds its fields', a map its keys'),
// string operations (+, slicing, indexing, conversions, the module's string
// types, the strings and path/filepath packages, fmt.Sprint*, json.Marshal,
// errors.Join, pp's Sprint*, shlex, os/exec's Command), ranges (a map's keys
// too), a builder written to, function literals called (at once, as a
// goroutine, or through a variable) with their arguments, every call within
// the module (a function's parameter; a method's, by its name, when at most
// MaxMethods methods share it and the receiver's type is unknown), and a
// function's results when they hold what it was given or read. A
// Sanitizer's result, a Redactor's (at DEBUG), and what a function outside
// the module returns (other than the string operations above) are not
// followed.
//
// Stated limits:
//   - not followed: a value passed through a channel; an interface value's
//     dynamic call with more than MaxMethods methods of its name; a
//     function value other than a literal or one of the Methods (a
//     package-level variable that holds a logger's method, var info =
//     logger.Info, is no sink); an explicitly instantiated generic
//     function's call (logAll[string](logger, cmd)); reflection; an
//     exported field of a struct whose type it cannot tell; what a function
//     writes through a pointer it is given (fill(&b, cmd)); a value in a
//     variable named err, ctx, cancel, logger, ok or _ (an error is followed
//     where it is made, into its sink: an err variable built of the command
//     elsewhere, &exec.Error{Name: cmd}, is not);
//   - what a library returns but the string operations above (url.Parse's
//     URL, an error a library returns: shlex's, strconv's, nat's) is not
//     followed;
//   - a writer is a sink only as listed above: a bufio.Writer, a
//     json.Encoder's Encode, a template's Execute and an
//     http.ResponseWriter's Write (to stdout or not) are not read as one,
//     nor is the Write of any writer not named for the console;
//   - a variable is followed by its name in its function, and a struct's
//     field by its type, so two of one name share what they hold (it finds
//     more, not less).
func Trace(root string, r Rules) ([]Finding, error) {
	if r.MaxMethods == 0 {
		r.MaxMethods = 4
	}
	t := &tracer{r: r, fset: token.NewFileSet(), byDir: map[string][]*fn{}, methods: map[string][]*fn{}, structs: map[string]*structDecl{},
		strings: map[string]bool{}, vars: map[string]bool{}, params: map[*fn][]Kind{}, fromSrc: map[*fn][]Kind{}, fromArg: map[*fn]map[int][]Kind{}, fields: map[string]Kind{}}
	if err := t.load(root); err != nil {
		return nil, err
	}
	for i := 0; i < 50; i++ {
		t.changed = false
		for _, f := range t.funcs {
			t.analyze(f, false)
		}
		if !t.changed {
			break
		}
	}
	for _, f := range t.funcs {
		t.analyze(f, true)
	}
	sort.Slice(t.findings, func(i, j int) bool {
		a, b := t.findings[i].Pos, t.findings[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Offset < b.Offset
	})
	return t.findings, nil
}

// Files is how many Go files Trace reads under root, so a test can tell it
// read the module's.
func Files(root string, skip []string) (int, error) {
	n := 0
	err := walk(root, skip, func(string) error { n++; return nil })
	return n, err
}

func walk(root string, skip []string, each func(path string) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			n := d.Name()
			if path != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata" || n == "node_modules") {
				return filepath.SkipDir
			}
			for _, s := range skip {
				if rel == s {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		return each(path)
	})
}

func (t *tracer) load(root string) error {
	return walk(root, t.r.Skip, func(path string) error {
		f, err := parser.ParseFile(t.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		fi := &file{dir: rel, imports: map[string]string{}, ast: f}
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			name := p[strings.LastIndex(p, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			fi.imports[name] = p
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok == token.VAR {
					t.packageVars(d, fi, rel)
				}
				for _, s := range d.Specs {
					if vs, ok := s.(*ast.ValueSpec); ok && d.Tok == token.VAR {
						for _, n := range vs.Names {
							t.vars[rel+"|"+n.Name] = true
						}
					}
					if ts, ok := s.(*ast.TypeSpec); ok {
						if st, ok := ts.Type.(*ast.StructType); ok {
							t.structs[rel+"."+ts.Name.Name] = &structDecl{st, fi}
						}
						if isIdent(ts.Type, "string") {
							t.strings[rel+"."+ts.Name.Name] = true
						}
					}
				}
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				t.addFunc(d, fi, rel)
			}
		}
		return nil
	})
}

// addFunc adds a function to the trace: by its name in its package, or a
// method by its name.
func (t *tracer) addFunc(d *ast.FuncDecl, fi *file, rel string) {
	f := &fn{name: d.Name.Name, dir: rel, decl: d, file: fi}
	if d.Recv != nil && len(d.Recv.List) > 0 {
		f.recv = baseTypeName(d.Recv.List[0].Type)
	}
	f.key = rel + "." + f.name
	if f.recv != "" {
		f.key = rel + "." + f.recv + "." + f.name
	}
	for _, p := range d.Type.Params.List {
		if len(p.Names) == 0 {
			f.params = append(f.params, "_")
			f.ptypes = append(f.ptypes, p.Type)
		}
		for _, n := range p.Names {
			f.params = append(f.params, n.Name)
			f.ptypes = append(f.ptypes, p.Type)
		}
	}
	if d.Type.Results != nil {
		for _, r := range d.Type.Results.List {
			f.results += max(1, len(r.Names))
		}
	}
	t.funcs = append(t.funcs, f)
	if f.recv == "" {
		t.byDir[rel+"|"+f.name] = append(t.byDir[rel+"|"+f.name], f)
	} else {
		t.methods[f.name] = append(t.methods[f.name], f)
	}
}

// packageVars reads a package-level var declaration's initializers as a
// function of their own ("dir.var name,..."), so what they give each
// variable is what the variable holds, and a line one of them writes is
// found. A variable that holds a function literal is also a function by its
// name (var textOf = func(c AppCommand) string { ... }; textOf(cmd)), and one
// that holds a method value or a method expression of the Methods is one
// that returns what that method does (var wholeOf =
// AppCommand.WholeUnredacted).
func (t *tracer) packageVars(d *ast.GenDecl, fi *file, rel string) {
	var stmts []ast.Stmt
	var names []string
	for _, sp := range d.Specs {
		vs, ok := sp.(*ast.ValueSpec)
		if !ok || len(vs.Values) == 0 {
			continue
		}
		for i, n := range vs.Names {
			names = append(names, n.Name)
			if len(vs.Values) != len(vs.Names) || n.Name == "_" {
				continue
			}
			switch v := vs.Values[i].(type) {
			case *ast.FuncLit:
				t.addFunc(&ast.FuncDecl{Name: n, Type: v.Type, Body: v.Body}, fi, rel)
			case *ast.SelectorExpr:
				if _, ok := t.r.Methods[v.Sel.Name]; ok {
					// return _.Method(): the method's kind, whatever its
					// receiver.
					ret := &ast.ReturnStmt{Results: []ast.Expr{&ast.CallExpr{Fun: &ast.SelectorExpr{X: ast.NewIdent("_"), Sel: ast.NewIdent(v.Sel.Name)}}}}
					t.addFunc(&ast.FuncDecl{Name: n, Type: &ast.FuncType{Params: &ast.FieldList{}, Results: &ast.FieldList{List: []*ast.Field{{Type: ast.NewIdent("string")}}}},
						Body: &ast.BlockStmt{List: []ast.Stmt{ret}}}, fi, rel)
				}
			}
		}
		stmts = append(stmts, &ast.DeclStmt{Decl: &ast.GenDecl{Tok: token.VAR, Specs: []ast.Spec{vs}}})
	}
	if len(stmts) > 0 {
		t.addFunc(&ast.FuncDecl{Name: ast.NewIdent("var " + strings.Join(names, ",")), Type: &ast.FuncType{Params: &ast.FieldList{}},
			Body: &ast.BlockStmt{List: stmts}}, fi, rel)
	}
}

func baseTypeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return baseTypeName(x.X)
	case *ast.IndexExpr:
		return baseTypeName(x.X)
	case *ast.IndexListExpr:
		return baseTypeName(x.X)
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// scope is one analysis of a function: its variables' kinds and types.
type scope struct {
	t      *tracer
	f      *fn
	kinds  map[string]Kind
	types  map[string]string       // a variable's type, "dir.Type"
	checks map[string]string       // a variable that holds a Check: its level's sink
	lits   map[string]*ast.FuncLit // a variable that holds a function literal
	maps   map[string]bool         // a variable that holds a map
	// methodValues: a variable that holds a method value of one of the
	// Methods (f := cmd.WholeUnredacted), and what its call returns;
	// logValues: one that holds a logger's method (info := logger.Info),
	// and its sink.
	methodValues map[string]Kind
	logValues    map[string]string
	// fieldSets: what a variable's field was set to in the function
	// (redactedCfg.Command = utils.RedactCommandLine(...)), "var.Field".
	fieldSets map[string]Kind
	dirty     bool
	report    bool
	// hyp: the scope gives every parameter the command, to learn what the
	// function's results hold of its arguments; it stores no field.
	hyp bool
}

// neverHolds are names no analysis gives a kind: an error (its constructor
// is the sink), a context, a logger.
var neverHolds = map[string]bool{"_": true, "err": true, "ctx": true, "cancel": true, "logger": true, "ok": true}

func (t *tracer) analyze(f *fn, report bool) {
	// From its sources alone; then with each parameter that may hold text
	// given the command, one at a time; then with what its callers give it.
	hyp := []int{-1}
	if !report {
		for i, p := range f.params {
			if t.textual(f.file, f.ptypes[i]) && !neverHolds[p] {
				hyp = append(hyp, i)
			}
		}
	}
	for _, p := range hyp {
		s := t.scopeOf(f, false, p >= 0)
		if p >= 0 {
			s.kinds[f.params[p]] = Raw
		}
		res := s.results()
		if p < 0 {
			t.grow(t.fromSrc, f, res)
			continue
		}
		if t.fromArg[f] == nil {
			t.fromArg[f] = map[int][]Kind{}
		}
		cur := t.fromArg[f][p]
		t.growSlice(&cur, res)
		t.fromArg[f][p] = cur
	}
	s := t.scopeOf(f, report, false)
	for i, k := range t.params[f] {
		if i < len(f.params) && !neverHolds[f.params[i]] && k > s.kinds[f.params[i]] {
			s.kinds[f.params[i]] = k
		}
	}
	s.settle()
	ast.Inspect(f.decl.Body, s.calls)
}

// scopeOf is a new analysis of f; a hypothetical one stores no field.
func (t *tracer) scopeOf(f *fn, report, hyp bool) *scope {
	s := &scope{t: t, f: f, kinds: map[string]Kind{}, types: map[string]string{}, checks: map[string]string{},
		lits: map[string]*ast.FuncLit{}, maps: map[string]bool{}, methodValues: map[string]Kind{}, logValues: map[string]string{}, fieldSets: map[string]Kind{},
		report: report, hyp: hyp}
	s.declare()
	return s
}

// settle grows the scope's kinds to a fixed point.
func (s *scope) settle() {
	for n := 0; n < 8; n++ {
		s.dirty = false
		ast.Inspect(s.f.decl.Body, s.flow)
		if !s.dirty {
			return
		}
	}
}

// textual reports whether a parameter of type e, written in file in, may
// hold the command's text: a string, a type of the module's declared as one,
// bytes, a list of them, an exec.Cmd, any value.
func (t *tracer) textual(in *file, e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "string" || x.Name == "any" || t.strings[in.dir+"."+x.Name]
	case *ast.SelectorExpr:
		if p, ok := in.imports[identName(x.X)]; ok {
			if dir, ok := t.moduleDir(p); ok {
				return t.strings[dir+"."+x.Sel.Name]
			}
		}
	case *ast.ArrayType:
		return t.textual(in, x.Elt) || isIdent(x.Elt, "byte")
	case *ast.Ellipsis:
		return t.textual(in, x.Elt)
	case *ast.StarExpr:
		if sel, ok := x.X.(*ast.SelectorExpr); ok {
			return isIdent(sel.X, "exec") && sel.Sel.Name == "Cmd"
		}
	case *ast.InterfaceType:
		return len(x.Methods.List) == 0
	}
	return false
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func (t *tracer) grow(m map[*fn][]Kind, f *fn, ks []Kind) {
	cur := m[f]
	t.growSlice(&cur, ks)
	m[f] = cur
}

func (t *tracer) growSlice(cur *[]Kind, ks []Kind) {
	for len(*cur) < len(ks) {
		*cur = append(*cur, None)
	}
	for i, k := range ks {
		if k > (*cur)[i] {
			(*cur)[i] = k
			t.changed = true
		}
	}
}

// declare gives the function's receiver and parameters their types.
func (s *scope) declare() {
	d := s.f.decl
	if d.Recv != nil {
		for _, r := range d.Recv.List {
			for _, n := range r.Names {
				s.types[n.Name] = s.qualify(r.Type, s.f.file)
			}
		}
	}
	for i, p := range s.f.params {
		s.types[p] = s.qualify(s.f.ptypes[i], s.f.file)
	}
}

// qualify is a type expression's "dir.Type", "" for one outside the module
// or not a named one.
func (s *scope) qualify(e ast.Expr, in *file) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return s.qualify(x.X, in)
	case *ast.Ident:
		if _, ok := s.t.structs[in.dir+"."+x.Name]; ok {
			return in.dir + "." + x.Name
		}
	case *ast.SelectorExpr:
		if p, ok := in.imports[identName(x.X)]; ok {
			if dir, ok := s.t.moduleDir(p); ok {
				return dir + "." + x.Sel.Name
			}
		}
	}
	return ""
}

func (t *tracer) moduleDir(path string) (string, bool) {
	switch {
	case path == t.r.Module:
		return "", true
	case strings.HasPrefix(path, t.r.Module+"/"):
		return strings.TrimPrefix(path, t.r.Module+"/"), true
	}
	return "", false
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// typeOf is e's "dir.Type", where the syntax tells it.
func (s *scope) typeOf(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return s.types[x.Name]
	case *ast.ParenExpr:
		return s.typeOf(x.X)
	case *ast.StarExpr:
		return s.typeOf(x.X)
	case *ast.UnaryExpr:
		return s.typeOf(x.X)
	case *ast.CompositeLit:
		return s.qualify(x.Type, s.f.file)
	case *ast.SelectorExpr:
		if ft, in := s.fieldType(s.typeOf(x.X), x.Sel.Name); ft != nil {
			return s.qualify(ft, in)
		}
	case *ast.CallExpr:
		for _, g := range s.callees(x) {
			if g.decl.Type.Results != nil && len(g.decl.Type.Results.List) > 0 {
				return s.qualify(g.decl.Type.Results.List[0].Type, g.file)
			}
		}
	}
	return ""
}

// fieldType is the type expression of typ's field name, and the file it is
// written in, looking into the structs typ embeds once.
func (s *scope) fieldType(typ, name string) (ast.Expr, *file) {
	sd := s.t.structs[typ]
	if sd == nil {
		return nil, nil
	}
	for _, fl := range sd.st.Fields.List {
		for _, n := range fl.Names {
			if n.Name == name {
				return fl.Type, sd.file
			}
		}
	}
	for _, fl := range sd.st.Fields.List {
		if len(fl.Names) == 0 {
			inner := (&scope{t: s.t, f: s.f}).qualify(fl.Type, sd.file)
			if inner != "" && inner != typ {
				if sub := s.t.structs[inner]; sub != nil {
					for _, g := range sub.st.Fields.List {
						for _, n := range g.Names {
							if n.Name == name {
								return g.Type, sub.file
							}
						}
					}
				}
			}
		}
	}
	return nil, nil
}

// fieldKey is the key of x.name's field in the tracer's state: by its
// struct's type where it is known, else an unexported one by its package.
func (s *scope) fieldKey(x ast.Expr, name string) string {
	if typ := s.typeOf(x); typ != "" {
		return typ + "." + name
	}
	if name != "" && name[0] >= 'a' && name[0] <= 'z' {
		return s.f.dir + "|" + name
	}
	return ""
}

func (s *scope) callees(call *ast.CallExpr) []*fn {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		if gs := s.t.byDir[s.f.dir+"|"+f.Name]; len(gs) > 0 {
			return gs
		}
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			if p, ok := s.f.file.imports[id.Name]; ok && s.types[id.Name] == "" {
				if dir, ok := s.t.moduleDir(p); ok {
					if gs := s.t.byDir[dir+"|"+f.Sel.Name]; len(gs) > 0 {
						return gs
					}
				}
				return nil
			}
		}
		if typ := s.typeOf(f.X); typ != "" {
			for _, g := range s.t.methods[f.Sel.Name] {
				if g.dir+"."+g.recv == typ {
					return []*fn{g}
				}
			}
		}
		if ms := s.t.methods[f.Sel.Name]; len(ms) <= s.t.r.MaxMethods {
			return ms
		}
	}
	return nil
}

// sanitized reports whether the function or method name ("dir.Func",
// "dir.Type.Method") is a Sanitizer, or a method of a SanitizerType that is
// none of the Methods.
func (s *scope) sanitized(name string) bool {
	if _, ok := s.t.r.Sanitizers[name]; ok {
		return true
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return false
	}
	_, typ := s.t.r.SanitizerTypes[name[:dot]]
	_, source := s.t.r.Methods[name[dot+1:]]
	return typ && !source
}

// qualifiedCall is the call's "dir.Func" in the module, or "path.Func" of a
// package outside it.
func (s *scope) qualifiedCall(call *ast.CallExpr) (name string, inModule bool) {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return s.f.dir + "." + f.Name, true
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			if p, ok := s.f.file.imports[id.Name]; ok && s.types[id.Name] == "" {
				if dir, ok := s.t.moduleDir(p); ok {
					return dir + "." + f.Sel.Name, true
				}
				return p + "." + f.Sel.Name, false
			}
		}
		if typ := s.typeOf(f.X); typ != "" {
			return typ + "." + f.Sel.Name, true
		}
	}
	return "", false
}

// textFuncs are functions outside the module whose result holds their
// arguments' text.
var textFuncs = map[string]bool{
	"fmt.Sprintf": true, "fmt.Sprint": true, "fmt.Sprintln": true, "fmt.Appendf": true,
	"github.com/google/shlex.Split": true, "github.com/mattn/go-shellwords.Parse": true,
	"os/exec.Command": true, "os/exec.CommandContext": true, "os.ExpandEnv": true, "strconv.Quote": true,
	"slices.Clone": true, "slices.Concat": true,
	"encoding/json.Marshal": true, "encoding/json.MarshalIndent": true, "errors.Join": true,
	"github.com/k0kubun/pp/v3.Sprint": true, "github.com/k0kubun/pp/v3.Sprintf": true, "github.com/k0kubun/pp/v3.Sprintln": true,
}

// textPackages are packages outside the module whose functions that return
// text return their arguments' (strings.Fields, filepath.Join), and zap,
// whose fields hold their values (zap.String, zap.Strings, zap.Any), and
// log/slog, whose attributes hold theirs (slog.String, slog.Any).
var textPackages = map[string]bool{"strings": true, "bytes": true, "path/filepath": true, "path": true, "go.uber.org/zap": true, "log/slog": true}

// noText are functions of textPackages that return no text.
func noText(name string) bool {
	for _, p := range []string{"Contains", "Has", "Index", "LastIndex", "Count", "Equal", "Compare", "Is", "Match", "Rel"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (s *scope) kind(e ast.Expr) Kind { return s.kindAt(e, -1) }

// kindAt is what e holds of the command; for a call, of its result i (any,
// for i < 0).
func (s *scope) kindAt(e ast.Expr, i int) Kind {
	switch x := e.(type) {
	case nil:
		return None
	case *ast.Ident:
		if s.t.vars[s.f.dir+"|"+x.Name] {
			return max(s.kinds[x.Name], s.t.fields["var:"+s.f.dir+"|"+x.Name])
		}
		return s.kinds[x.Name]
	case *ast.ParenExpr:
		return s.kindAt(x.X, i)
	case *ast.StarExpr, *ast.UnaryExpr, *ast.IndexExpr, *ast.SliceExpr, *ast.TypeAssertExpr:
		return s.kind(inner(x))
	case *ast.KeyValueExpr:
		return s.kind(x.Value)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return max(s.kind(x.X), s.kind(x.Y))
		}
		return None
	case *ast.CompositeLit:
		k := None
		typ := s.qualify(x.Type, s.f.file)
		_, isMap := x.Type.(*ast.MapType)
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && typ != "" {
					// The field holds it, and so does the literal printed
					// whole (%v, zap.Any).
					s.store(typ+"."+key.Name, s.kind(kv.Value))
				}
				if isMap {
					k = max(k, s.kind(kv.Key))
				}
			}
			k = max(k, s.kind(el))
		}
		return k
	case *ast.SelectorExpr:
		if p, ok := s.f.file.imports[identName(x.X)]; ok && s.types[identName(x.X)] == "" {
			if p == "os" && x.Sel.Name == "Args" {
				return Raw
			}
			if dir, ok := s.t.moduleDir(p); ok && s.t.vars[dir+"|"+x.Sel.Name] {
				return s.t.fields["var:"+dir+"|"+x.Sel.Name]
			}
			return None
		}
		if typ := s.typeOf(x.X); typ != "" {
			if k, ok := s.t.r.Fields[typ+"."+x.Sel.Name]; ok {
				return k
			}
		}
		if key := s.fieldKey(x.X, x.Sel.Name); key != "" {
			if k := s.t.fields[key]; k > None {
				return k
			}
		}
		if x.Sel.Name == "Args" || x.Sel.Name == "Path" {
			// An exec.Cmd's words.
			return s.kind(x.X)
		}
		return None
	case *ast.CallExpr:
		return s.callKind(x, i)
	}
	return None
}

func inner(e ast.Expr) ast.Expr {
	switch x := e.(type) {
	case *ast.StarExpr:
		return x.X
	case *ast.UnaryExpr:
		return x.X
	case *ast.IndexExpr:
		return x.X
	case *ast.SliceExpr:
		return x.X
	case *ast.TypeAssertExpr:
		return x.X
	}
	return nil
}

func (s *scope) argsKind(call *ast.CallExpr) Kind {
	k := None
	for _, a := range call.Args {
		k = max(k, s.kind(a))
	}
	return k
}

func (s *scope) callKind(call *ast.CallExpr, i int) Kind {
	name, inModule := s.qualifiedCall(call)
	if s.sanitized(name) {
		return None
	}
	if _, ok := s.t.r.Sanitizers[name+"#"+strconv.Itoa(i)]; ok && i >= 0 {
		return None
	}
	if s.t.r.Redactors[name] {
		return min(s.argsKind(call), Redacted)
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		// cmd.WholeUnredacted(), or its method expression,
		// AppCommand.WholeUnredacted(cmd).
		if k, ok := s.t.r.Methods[sel.Sel.Name]; ok && (len(call.Args) == 0 || len(call.Args) == 1 && s.typeName(sel.X)) {
			return k
		}
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		// A method value's call: f := cmd.WholeUnredacted; f().
		if k, ok := s.methodValues[id.Name]; ok {
			return k
		}
	}
	switch fun := call.Fun.(type) {
	case *ast.ArrayType:
		return s.argsKind(call) // []byte(x), []string(x)
	case *ast.Ident:
		switch fun.Name {
		case "string", "append", "min", "max":
			return s.argsKind(call)
		case "len", "cap", "copy", "make", "new", "recover", "complex", "real", "imag":
			return None
		}
	}
	if inModule {
		cs := s.callees(call)
		if len(cs) == 0 {
			// A conversion to a type of the module's (utils.CmdType(x)).
			return s.argsKind(call)
		}
		return s.resultKind(call, cs, i)
	}
	if textFuncs[name] {
		return s.argsKind(call)
	}
	if dot := strings.LastIndex(name, "."); dot > 0 && textPackages[name[:dot]] {
		if noText(name[dot+1:]) {
			return None
		}
		return s.argsKind(call)
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && name == "" {
		// A method of a value whose type is not known: the module's methods
		// of its name, else a value's outside the module (an exec.Cmd's
		// String, a builder's or a regexp's text).
		if cs := s.callees(call); len(cs) > 0 {
			k := s.resultKind(call, cs, i)
			if sel.Sel.Name == "String" {
				k = max(k, s.kind(sel.X))
			}
			return k
		}
		switch m := sel.Sel.Name; {
		case m == "String":
			return s.kind(sel.X)
		case strings.HasPrefix(m, "Replace") || strings.HasPrefix(m, "Find") && strings.Contains(m, "String"):
			return s.argsKind(call)
		}
	}
	return None
}

// resultKind is what the call's result i holds: what its callees read
// themselves, and what they return of each argument the call gives them.
func (s *scope) resultKind(call *ast.CallExpr, cs []*fn, i int) Kind {
	k := None
	for _, g := range cs {
		k = max(k, at(s.t.fromSrc[g], i))
		for a, arg := range call.Args {
			if ak := s.kind(arg); ak > None {
				p := min(a, len(g.params)-1)
				k = max(k, min(ak, at(s.t.fromArg[g][p], i)))
			}
		}
	}
	return k
}

func at(ks []Kind, i int) Kind {
	if i < 0 {
		k := None
		for _, x := range ks {
			k = max(k, x)
		}
		return k
	}
	if i < len(ks) {
		return ks[i]
	}
	return None
}

func (s *scope) set(name string, k Kind) {
	if k > s.kinds[name] && !neverHolds[name] {
		s.kinds[name] = k
		s.dirty = true
	}
}

func (s *scope) store(key string, k Kind) {
	if !s.hyp && key != "" && k > s.t.fields[key] {
		s.t.fields[key] = k
		s.t.changed = true
	}
}

func (s *scope) assign(lhs ast.Expr, rhs ast.Expr, k Kind) {
	switch l := lhs.(type) {
	case *ast.Ident:
		s.set(l.Name, k)
		if s.t.vars[s.f.dir+"|"+l.Name] {
			// A package-level variable (or a local of its name: it finds
			// more, not less).
			s.store("var:"+s.f.dir+"|"+l.Name, k)
		}
		if lit, ok := rhs.(*ast.FuncLit); ok {
			s.lits[l.Name] = lit
		}
		if sel, ok := rhs.(*ast.SelectorExpr); ok {
			if k, ok := s.t.r.Methods[sel.Sel.Name]; ok {
				s.methodValues[l.Name] = k
			}
			if lv, ok := logLevels[sel.Sel.Name]; ok && sel.Sel.Name != "With" {
				s.logValues[l.Name] = lv
			}
		}
		if rhs != nil && s.isMap(rhs) {
			s.maps[l.Name] = true
		}
		if rhs != nil && s.types[l.Name] == "" {
			if typ := s.typeOf(rhs); typ != "" {
				s.types[l.Name] = typ
			}
		}
		if c, ok := rhs.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Check" && len(c.Args) > 0 {
				s.checks[l.Name] = "Check (INFO or above)"
				if debugLevel(c.Args[0]) {
					s.checks[l.Name] = "DEBUG"
				}
			}
		}
	case *ast.SelectorExpr:
		s.store(s.fieldKey(l.X, l.Sel.Name), k)
		// A struct variable whose field is set holds it, printed whole.
		if id, ok := l.X.(*ast.Ident); ok {
			s.set(id.Name, k)
			key := id.Name + "." + l.Sel.Name
			if cur, ok := s.fieldSets[key]; !ok || k > cur {
				s.fieldSets[key] = k
			}
		}
	case *ast.IndexExpr:
		if s.isMap(l.X) {
			k = max(k, s.kind(l.Index)) // a map's key
		}
		s.assign(l.X, nil, k)
	case *ast.StarExpr:
		s.assign(l.X, nil, k)
	case *ast.ParenExpr:
		s.assign(l.X, nil, k)
	case *ast.UnaryExpr:
		s.assign(l.X, nil, k)
	}
}

// flow grows the scope's kinds from one statement.
func (s *scope) flow(n ast.Node) bool {
	switch x := n.(type) {
	case *ast.AssignStmt:
		if len(x.Lhs) == len(x.Rhs) {
			for i := range x.Lhs {
				s.assign(x.Lhs[i], x.Rhs[i], s.kind(x.Rhs[i]))
			}
		} else if len(x.Rhs) == 1 {
			for i := range x.Lhs {
				s.assign(x.Lhs[i], nil, s.kindAt(x.Rhs[0], i))
			}
		}
	case *ast.ValueSpec:
		for i, name := range x.Names {
			if x.Type != nil {
				s.types[name.Name] = s.qualify(x.Type, s.f.file)
				if _, ok := x.Type.(*ast.MapType); ok {
					s.maps[name.Name] = true
				}
			}
			switch {
			case len(x.Values) == len(x.Names):
				s.assign(name, x.Values[i], s.kind(x.Values[i]))
			case len(x.Values) == 1:
				s.assign(name, nil, s.kindAt(x.Values[0], i))
			}
		}
	case *ast.RangeStmt:
		if k := s.kind(x.X); k > None && x.Value != nil {
			s.assign(x.Value, nil, k)
		}
		if k := s.kind(x.X); k > None && x.Key != nil && s.isMap(x.X) {
			s.assign(x.Key, nil, k) // a map's key
		}
	case *ast.CompositeLit:
		// Its fields hold what it is built of, wherever it is (a return, a
		// call's argument).
		s.kind(x)
	case *ast.CallExpr:
		// A function literal called (at once, as a goroutine, or through
		// the variable that holds it): its parameters hold its arguments.
		if lit := s.funcLit(x.Fun); lit != nil {
			s.bindLit(lit, x.Args)
		}
		// A builder or a buffer written to holds what it was given.
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "Write") {
			if _, isPkg := s.f.file.imports[identName(sel.X)]; !isPkg {
				s.assign(sel.X, nil, s.argsKind(x))
			}
		}
		if name, _ := s.qualifiedCall(x); writes(name) && len(x.Args) > 1 {
			k := None
			for _, a := range x.Args[1:] {
				k = max(k, s.kind(a))
			}
			s.assign(x.Args[0], nil, k)
		}
	}
	return true
}

// wholeKind is what a struct printed whole holds of the command, a sink's
// argument whose type the syntax tells (cfg, *cfg, a struct's field of one,
// in a zap field or a text made of it): the most its type's fields hold, by
// the Rules' Fields and what the trace stored in them (a field the function
// set on that variable as it set it), and its embedded structs' and its
// fields' structs', two deep, by the Rules' Fields alone: the trace stores a
// field by its struct's type, not by the value, so a stored field of a
// struct the printed one holds (the options a docker run hands its agent)
// says little of this value. It is read at a sink alone: a value that holds
// a field of the command is not itself followed (cfg.Path holds none of it).
func (s *scope) wholeKind(e ast.Expr) Kind {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	switch x := e.(type) {
	case *ast.CallExpr:
		// A field or a text made of it: zap.Any("cfg", cfg), fmt.Sprint(cfg).
		name, _ := s.qualifiedCall(x)
		if !strings.HasPrefix(name, "go.uber.org/zap.") && !textFuncs[name] {
			return None
		}
		k := None
		for _, a := range x.Args {
			k = max(k, s.wholeKind(a))
		}
		return k
	case *ast.Ident:
		return s.structKind(s.typeOf(x), 2, x.Name)
	case *ast.StarExpr, *ast.SelectorExpr, *ast.ParenExpr:
		return s.structKind(s.typeOf(e), 2, "")
	}
	return None
}

// structKind is what a value of the struct typ holds of the command, its
// variable's (v, "" for none) fields as the function set them where it did.
func (s *scope) structKind(typ string, depth int, v string) Kind {
	sd := s.t.structs[typ]
	if sd == nil || depth < 0 {
		return None
	}
	k := None
	for _, fl := range sd.st.Fields.List {
		names := []string{}
		for _, n := range fl.Names {
			names = append(names, n.Name)
		}
		inner := (&scope{t: s.t, f: s.f}).qualify(fl.Type, sd.file)
		for _, n := range names {
			if set, ok := s.fieldSets[v+"."+n]; ok && v != "" {
				k = max(k, set)
				continue
			}
			if fk, ok := s.t.r.Fields[typ+"."+n]; ok {
				k = max(k, fk)
			}
			if depth == 2 {
				k = max(k, s.t.fields[typ+"."+n])
			}
		}
		if inner != "" && inner != typ {
			k = max(k, s.structKind(inner, depth-1, ""))
		}
	}
	return k
}

// typeName reports whether e names a type, where the syntax tells it: a
// package's exported name (utils.AppCommand), or a struct of the module's.
func (s *scope) typeName(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return false
		}
		_, isPkg := s.f.file.imports[id.Name]
		return isPkg && s.types[id.Name] == "" && x.Sel.Name != "" && x.Sel.Name[0] >= 'A' && x.Sel.Name[0] <= 'Z'
	case *ast.Ident:
		return s.t.structs[s.f.dir+"."+x.Name] != nil
	case *ast.ParenExpr:
		return s.typeName(x.X)
	}
	return false
}

// funcLit is the function literal e calls: e itself, or a variable that
// holds one.
func (s *scope) funcLit(e ast.Expr) *ast.FuncLit {
	switch x := e.(type) {
	case *ast.FuncLit:
		return x
	case *ast.ParenExpr:
		return s.funcLit(x.X)
	case *ast.Ident:
		return s.lits[x.Name]
	}
	return nil
}

// bindLit gives a function literal's parameters what a call passes them; a
// variadic one, what each argument from its place on holds.
func (s *scope) bindLit(lit *ast.FuncLit, args []ast.Expr) {
	i := 0
	for _, p := range lit.Type.Params.List {
		_, variadic := p.Type.(*ast.Ellipsis)
		for _, n := range p.Names {
			k := None
			for j := i; j < len(args) && (j == i || variadic); j++ {
				k = max(k, s.kind(args[j]))
			}
			s.set(n.Name, k)
			i++
		}
		if len(p.Names) == 0 {
			i++
		}
	}
}

// isMap reports whether e is a map, where the syntax tells it: a map
// literal, make of a map, or a variable that holds one.
func (s *scope) isMap(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.CompositeLit:
		_, ok := x.Type.(*ast.MapType)
		return ok
	case *ast.CallExpr:
		if isIdent(x.Fun, "make") && len(x.Args) > 0 {
			_, ok := x.Args[0].(*ast.MapType)
			return ok
		}
	case *ast.Ident:
		return s.maps[x.Name]
	case *ast.ParenExpr:
		return s.isMap(x.X)
	}
	return false
}

// results are the kinds the function's return statements give its results,
// once its kinds settle.
func (s *scope) results() []Kind {
	s.settle()
	ks := make([]Kind, s.f.results)
	var named []string
	if s.f.decl.Type.Results != nil {
		for _, r := range s.f.decl.Type.Results.List {
			for _, n := range r.Names {
				named = append(named, n.Name)
			}
		}
	}
	ast.Inspect(s.f.decl.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		r, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		switch {
		case len(r.Results) == 0:
			for i, name := range named {
				if i < len(ks) {
					ks[i] = max(ks[i], s.kinds[name])
				}
			}
		case len(r.Results) == len(ks):
			for i, e := range r.Results {
				ks[i] = max(ks[i], s.kind(e))
			}
		case len(r.Results) == 1:
			for i := range ks {
				ks[i] = max(ks[i], s.kindAt(r.Results[0], i))
			}
		}
		return true
	})
	return ks
}

func debugLevel(e ast.Expr) bool {
	var b strings.Builder
	_ = printer.Fprint(&b, token.NewFileSet(), e)
	return strings.Contains(b.String(), "Debug")
}

// calls hands the callees what the function gives them, and reports the
// sinks that take the command.
func (s *scope) calls(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return true
	}
	name, _ := s.qualifiedCall(call)
	_, helper := s.t.r.Sinks[name]
	if !s.sanitized(name) && !s.t.r.Redactors[name] && !helper {
		for _, g := range s.callees(call) {
			for i, a := range call.Args {
				p := min(i, len(g.params)-1)
				if p < 0 {
					continue
				}
				if k := s.kind(a); k > None {
					ks := s.t.params[g]
					for len(ks) < len(g.params) {
						ks = append(ks, None)
					}
					if k > ks[p] {
						ks[p] = k
						s.t.changed = true
					}
					s.t.params[g] = ks
				}
			}
		}
	}
	if !s.report {
		return true
	}
	sink, debug := s.sink(call, name)
	if sink == "" {
		return true
	}
	for _, a := range sinkArgs(call, name) {
		k := max(s.kind(a), s.wholeKind(a))
		if k == Raw || k == Redacted && !debug {
			f := Finding{Pos: s.t.fset.Position(a.Pos()), Func: s.f.key, Sink: sink, Kind: k, Expr: text(a)}
			_, allowed := s.t.r.Allowed[f.Func+": "+f.Expr]
			if _, ok := s.t.r.AllowedRedacted[f.Func+": "+f.Expr]; ok && k == Redacted {
				allowed = true
			}
			if !allowed {
				s.t.findings = append(s.t.findings, f)
			}
		}
	}
	return true
}

func sinkArgs(call *ast.CallExpr, name string) []ast.Expr {
	if writes(name) && len(call.Args) > 0 {
		return call.Args[1:]
	}
	return call.Args
}

// writes reports whether name is a function that writes its arguments after
// the first to the writer that is its first.
func writes(name string) bool {
	return strings.HasPrefix(name, "fmt.Fprint") || name == "io.WriteString" || strings.HasPrefix(name, "github.com/k0kubun/pp/v3.Fprint")
}

var logLevels = map[string]string{
	"Info": "INFO", "Infof": "INFO", "Infow": "INFO", "Warn": "WARN", "Warnf": "WARN", "Warnw": "WARN",
	"InfoContext": "INFO", "WarnContext": "WARN", "ErrorContext": "ERROR",
	"Infoln": "INFO", "Warnln": "WARN", "Errorln": "ERROR", "DPanicln": "DPANIC",
	// A logger's name and options are on each line it writes.
	"Named": "INFO (a logger's Named)", "WithOptions": "INFO (a logger's WithOptions)",
	"Error": "ERROR", "Errorf": "ERROR", "Errorw": "ERROR", "Fatal": "FATAL", "Fatalf": "FATAL", "Fatalw": "FATAL",
	"Panic": "PANIC", "Panicf": "PANIC", "Panicw": "PANIC", "DPanic": "DPANIC", "DPanicf": "DPANIC", "DPanicw": "DPANIC",
	"With": "INFO (a logger's With)",
}

// sink is what call is, when it takes what it is given to a line: "" for
// no sink, debug for a DEBUG line.
func (s *scope) sink(call *ast.CallExpr, name string) (sink string, debug bool) {
	if lv, ok := s.t.r.Sinks[name]; ok {
		return lv, lv == "DEBUG"
	}
	switch name {
	case "fmt.Errorf", "errors.New", "github.com/pkg/errors.New", "github.com/pkg/errors.Errorf", "github.com/pkg/errors.Wrap",
		"github.com/pkg/errors.Wrapf", "github.com/pkg/errors.WithMessage", "github.com/pkg/errors.WithMessagef":
		return "error", false
	case "fmt.Print", "fmt.Printf", "fmt.Println", "log.Print", "log.Printf", "log.Println", "log.Fatal", "log.Fatalf", "log.Fatalln",
		"log.Panic", "log.Panicf", "log.Panicln", "github.com/k0kubun/pp/v3.Print", "github.com/k0kubun/pp/v3.Printf", "github.com/k0kubun/pp/v3.Println":
		return "console", false
	case "fmt.Fprint", "fmt.Fprintf", "fmt.Fprintln", "io.WriteString",
		"github.com/k0kubun/pp/v3.Fprint", "github.com/k0kubun/pp/v3.Fprintf", "github.com/k0kubun/pp/v3.Fprintln":
		// To any writer: the console (os.Stdout, a cobra command's
		// writer, a writer variable), a file, a response.
		return "console or a writer", false
	case "log.New", "log.SetPrefix":
		// A logger's prefix starts each of its lines.
		return "*log.Logger prefix", false
	case "log/slog.Info", "log/slog.InfoContext":
		return "INFO", false
	case "log/slog.Warn", "log/slog.WarnContext":
		return "WARN", false
	case "log/slog.Error", "log/slog.ErrorContext":
		return "ERROR", false
	case "log/slog.Debug", "log/slog.DebugContext":
		return "DEBUG", true
	case "log/slog.Log", "log/slog.LogAttrs":
		// slog.Log(ctx, level, msg, ...).
		if len(call.Args) > 1 && debugLevel(call.Args[1]) {
			return "DEBUG", true
		}
		return "Log (INFO or above)", false
	}
	if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "panic" {
		return "panic", false
	}
	if id, ok := call.Fun.(*ast.Ident); ok && s.logValues[id.Name] != "" {
		return s.logValues[id.Name], false // info := logger.Info; info(...)
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	if _, isPkg := s.f.file.imports[identName(sel.X)]; isPkg && s.types[identName(sel.X)] == "" {
		return "", false
	}
	switch m := sel.Sel.Name; m {
	case "Debug", "Debugf", "Debugw", "Debugln", "DebugContext":
		return "DEBUG", true
	case "Log", "LogAttrs":
		// zap's Log(level, msg, ...); slog's Log(ctx, level, msg, ...).
		if debugLevel(call.Args[0]) || len(call.Args) > 1 && contextArg(call.Args[0]) && debugLevel(call.Args[1]) {
			return "DEBUG", true
		}
		return "Log (INFO or above)", false
	case "Print", "Printf", "Println", "Fatalln", "Panicln", "Output", "SetPrefix":
		// A *log.Logger's line (log.New(os.Stderr, "", 0).Println(...)),
		// and its prefix.
		return "*log.Logger", false
	case "WriteString":
		if consoleWriter(sel.X) {
			return "console", false
		}
		return "", false
	case "Write":
		if consoleWriter(sel.X) {
			return "console", false
		}
		if c, ok := sel.X.(*ast.CallExpr); ok {
			if cs, ok := c.Fun.(*ast.SelectorExpr); ok && cs.Sel.Name == "Check" && len(c.Args) > 0 {
				if debugLevel(c.Args[0]) {
					return "DEBUG", true
				}
				return "Check (INFO or above)", false
			}
		}
		if lv := s.checks[identName(sel.X)]; lv != "" {
			return lv, lv == "DEBUG"
		}
		return "", false
	default:
		if lv, ok := logLevels[m]; ok {
			return lv, false
		}
	}
	return "", false
}

// contextArg reports whether e reads as a context.Context argument (ctx,
// context.Background()), as slog's Log takes first.
func contextArg(e ast.Expr) bool {
	t := text(e)
	return t == "ctx" || strings.HasPrefix(t, "context.") || strings.HasSuffix(strings.ToLower(t), "ctx")
}

func consoleWriter(e ast.Expr) bool {
	t := strings.ToLower(text(e))
	for _, w := range []string{"os.stdout", "os.stderr", "console", "stdout", "stderr", "tty"} {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

func text(e ast.Node) string {
	var b strings.Builder
	_ = printer.Fprint(&b, token.NewFileSet(), e)
	s := strings.Join(strings.Fields(b.String()), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// Unknown are the names in r that name nothing under root: a field, a
// function, a method or a type a rename took away, whose rule would hold
// no more.
func Unknown(root string, r Rules) ([]string, error) {
	t := &tracer{r: r, fset: token.NewFileSet(), byDir: map[string][]*fn{}, methods: map[string][]*fn{}, structs: map[string]*structDecl{}, strings: map[string]bool{}, vars: map[string]bool{}}
	if err := t.load(root); err != nil {
		return nil, err
	}
	funcs := map[string]*fn{}
	for _, f := range t.funcs {
		funcs[f.key] = f
	}
	var unknown []string
	check := func(rule, key string) {
		name, result, indexed := strings.Cut(key, "#")
		f := funcs[name]
		if f == nil {
			unknown = append(unknown, rule+" "+key)
			return
		}
		if i, err := strconv.Atoi(result); indexed && (err != nil || i >= f.results) {
			unknown = append(unknown, rule+" "+key+": no such result")
		}
	}
	for key := range r.Fields {
		dot := strings.LastIndex(key, ".")
		sd := t.structs[key[:max(dot, 0)]]
		found := false
		if sd != nil {
			for _, fl := range sd.st.Fields.List {
				for _, n := range fl.Names {
					found = found || n.Name == key[dot+1:]
				}
			}
		}
		if !found {
			unknown = append(unknown, "Fields "+key)
		}
	}
	for name := range r.Methods {
		if len(t.methods[name]) == 0 {
			unknown = append(unknown, "Methods "+name)
		}
	}
	for key := range r.Redactors {
		check("Redactors", key)
	}
	for key := range r.Sinks {
		check("Sinks", key)
	}
	for key := range r.Sanitizers {
		check("Sanitizers", key)
	}
	for key := range r.Allowed {
		name, _, _ := strings.Cut(key, ": ")
		check("Allowed", name)
	}
	for key := range r.AllowedRedacted {
		name, _, _ := strings.Cut(key, ": ")
		check("AllowedRedacted", name)
	}
	for key := range r.SanitizerTypes {
		if t.structs[key] == nil {
			unknown = append(unknown, "SanitizerTypes "+key)
		}
	}
	sort.Strings(unknown)
	return unknown, nil
}
