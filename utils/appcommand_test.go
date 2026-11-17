package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	yamlv2 "gopkg.in/yaml.v2"
	"gopkg.in/yaml.v3"
)

// A line at INFO or above names the command keploy runs for the user by its
// program and how many words follow it, never an argument: in every form a
// password, a host or a user takes in it, whether the redactor reads that
// form or not, and in a plain command. A first word that is no program's
// name (a logon, a userinfo, a flag) is written by its shape and tag.
func TestAnAppCommandIsNamedAtInfoByItsProgramAlone(t *testing.T) {
	const spaced = "/opt/my app/bin/python3"
	tag := (*models.LogKeySession)(nil).LogKey
	for _, tc := range []struct {
		cmd, program string
		args         int64
	}{
		{"python3 -m pytest -q", "python3", 3},
		{"/home/alice/venv/bin/python app.py", "python", 1},
		{"docker compose up --build", "docker", 3},
		{"sqlplus scott/S3CR3T@//db.keploy.test:1521/ORCL", "sqlplus", 1},
		{"./app --db=scott/S3CR3T@db.keploy.test:1521/XEPDB1", "app", 1},
		{"python app.py scott/S3CR3T@db.keploy.test/orclpdb", "python", 2},
		{"python app.py alice/S3CR3T@orcl", "python", 2},
		{"sqlcmd -S db.keploy.test -U sa -P S3CR3T", "sqlcmd", 6},
		{"lftp -u alice,S3CR3T sftp://files.keploy.test", "lftp", 3},
		{"smbclient //fs.keploy.test/share -U alice%S3CR3T", "smbclient", 3},
		{"curl -x alice@corp:S3CR3T@proxy.keploy.test:3128 http://dep.keploy.test", "curl", 3},
		{"curl --user=alice:S3CR3T http://dep.keploy.test", "curl", 2},
		{"./app --opaque-thing S3CR3T", "app", 2},
		{"TOKEN=S3CR3T DB_URL=postgres://alice:S3CR3T@db.keploy.test/shop ./server --port 8080", "server", 2},
		{`C:\Users\alice\app.exe --x`, "app.exe", 1},
		{"c++ -o a a.cc", "c++", 3},
		{`'` + spaced + `' alice/S3CR3T@orcl`, "/{}/{} {}/{}/{} " + tag(spaced), 1},
		// A first word that is no program.
		{"--token=S3CR3T ./app", "--{}=<redacted> " + tag("--token=<redacted>"), 1},
		{"scott/S3CR3T@orcl", "{}/{}@{} " + tag("scott/S3CR3T@orcl"), 0},
		{"alice:S3CR3T@db.keploy.test:5432 ./app", "{}:<redacted>@{}.{}.{}:{} " + tag("alice:<redacted>@db.keploy.test:5432"), 1},
		{"-pS3CR3T ./app", "-{} " + tag("-pS3CR3T"), 1},
		{"", "", 0},
	} {
		cmd := NewAppCommand(tc.cmd)
		core, logs := observer.New(zapcore.InfoLevel)
		zap.New(core).Info(StartingApplication, zap.Inline(cmd))
		e := logs.All()[0]
		if got, want := e.ContextMap(), map[string]any{"program": tc.program, "args": tc.args}; !reflect.DeepEqual(got, want) {
			t.Errorf("zap.Inline(NewAppCommand(%q)) = %v, want %v", tc.cmd, got, want)
		}
		if line := fmt.Sprint(e.ContextMap()); strings.Contains(line, "S3CR3T") || strings.Contains(line, "keploy.test") || strings.Contains(line, "alice") {
			t.Errorf("NewAppCommand(%q) names an argument: %s", tc.cmd, line)
		}
		if got := cmd.Program(); got != tc.program {
			t.Errorf("NewAppCommand(%q).Program() = %q, want %q", tc.cmd, got, tc.program)
		}
		if got := int64(cmd.ArgCount()); got != tc.args {
			t.Errorf("NewAppCommand(%q).ArgCount() = %d, want %d", tc.cmd, got, tc.args)
		}
	}
}

// The whole command goes to a DEBUG line alone, its secrets redacted; with
// debug off it is not written.
func TestTheWholeCommandIsLoggedAtDebugAlone(t *testing.T) {
	const cmd = "sqlplus scott/S3CR3T@orcl --password S3CR3T"
	core, logs := observer.New(zapcore.DebugLevel)
	LogWholeCommand(zap.New(core), "cmd", NewAppCommand(cmd), zap.String("composeProject", "shop"))
	LogWholeCmdArgs(zap.New(core), "executing_cmd", []string{"sh", "-c", cmd})
	if got := logs.Len(); got != 2 {
		t.Fatalf("logged %d lines, want 2", got)
	}
	for i, want := range []map[string]any{
		{"cmd": "sqlplus scott/<redacted>@orcl --password <redacted>", "composeProject": "shop"},
		{"executing_cmd": "sh -c 'sqlplus scott/<redacted>@orcl --password <redacted>'"},
	} {
		e := logs.All()[i]
		if e.Level != zapcore.DebugLevel || e.Message != WholeCommandMessage || !reflect.DeepEqual(e.ContextMap(), want) {
			t.Errorf("line %d: %v %q %v, want debug %q %v", i, e.Level, e.Message, e.ContextMap(), WholeCommandMessage, want)
		}
	}
	core, logs = observer.New(zapcore.InfoLevel)
	LogWholeCommand(zap.New(core), "cmd", NewAppCommand(cmd))
	LogWholeCmdArgs(zap.New(core), "executing_cmd", []string{"sh", "-c", cmd})
	if logs.Len() != 0 {
		t.Errorf("with debug off, logged %v", logs.All())
	}
}

// Every way keploy's code can print an AppCommand writes its program and how
// many arguments follow it, and none of its arguments: fmt's verbs, a struct
// or a list that holds it, JSON and YAML, and zap through its JSON and
// console encoders, among them the seven forms the guard before it missed
// (a variable, a field built from the command, a list of it, zap.Any,
// Check(...).Write, Log at a level, an error that formats it). zap.String,
// a string's concatenation and the strings package do not take one: the
// code does not compile.
func TestEveryWayToPrintAnAppCommandNamesItsProgramAlone(t *testing.T) {
	cmd := NewAppCommand("DB_URL=postgres://alice:S3CR3T@db.keploy.test/shop python3 app.py --password S3CR3T")
	const program = "python3 (3 args)"
	type exported struct{ Cmd AppCommand }
	type unexported struct{ cmd AppCommand }
	logged := func(log func(*zap.Logger)) string {
		var b bytes.Buffer
		for _, enc := range []zapcore.Encoder{zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())} {
			log(zap.New(zapcore.NewCore(enc, zapcore.AddSync(&b), zapcore.DebugLevel)))
		}
		return b.String()
	}
	jsonOf := func(v any) string { b, err := json.Marshal(v); return fmt.Sprint(string(b), err) }
	yamlOf := func(v any) string { b, err := yaml.Marshal(v); return fmt.Sprint(string(b), err) }
	yaml2Of := func(v any) string { b, err := yamlv2.Marshal(v); return fmt.Sprint(string(b), err) }
	type form struct {
		name, out string
		named     bool // the output names the program
	}
	var forms []form
	for _, verb := range []string{"%v", "%s", "%q", "%#v", "%+v", "%x", "%d", "%T"} {
		forms = append(forms, form{verb, fmt.Sprintf(verb, cmd), verb != "%T"})
	}
	// A verb fmt reports as bad prints its operand without its methods.
	bad := "%w"
	forms = append(forms, form{"%w, a bad verb", fmt.Sprintf(bad, cmd), false})
	_ = unexported{cmd}.cmd
	for _, tc := range append(forms, []form{
		{"Sprint", fmt.Sprint(cmd), true},
		{"a pointer", fmt.Sprintf("%v", &cmd), true},
		{"a list", fmt.Sprintf("%v", []AppCommand{cmd}), true},
		{"a map's value", fmt.Sprintf("%v", map[string]AppCommand{"cmd": cmd}), true},
		{"an exported field, %+v", fmt.Sprintf("%+v", exported{cmd}), true},
		{"an exported field, %#v", fmt.Sprintf("%#v", exported{cmd}), true},
		{"an unexported field, %v", fmt.Sprintf("%v", unexported{cmd}), false},
		{"an unexported field, %+v", fmt.Sprintf("%+v", unexported{cmd}), false},
		{"an unexported field, %#v", fmt.Sprintf("%#v", unexported{cmd}), false},
		{"an error that formats it (7)", fmt.Errorf("could not start %v", cmd).Error(), true},
		{"json", jsonOf(cmd), true},
		{"json of an exported field", jsonOf(exported{cmd}), true},
		{"json of an unexported field", jsonOf(unexported{cmd}), false},
		{"yaml.v3", yamlOf(exported{cmd}), true},
		{"yaml.v2", yaml2Of(exported{cmd}), true},
		{"a variable (1)", logged(func(l *zap.Logger) { s := cmd; l.Info("starting the app", zap.Any("cmd", s)) }), true},
		{"zap.Stringer (2)", logged(func(l *zap.Logger) { l.Info("starting the app", zap.Stringer("cmd", cmd)) }), true},
		{"a list of it (3)", logged(func(l *zap.Logger) { l.Warn("starting the app", zap.Any("args", []AppCommand{cmd})) }), true},
		{"zap.Any (4)", logged(func(l *zap.Logger) { l.Error("starting the app", zap.Any("cmd", cmd)) }), true},
		{"Check(...).Write (5)", logged(func(l *zap.Logger) {
			if ce := l.Check(zap.InfoLevel, "starting the app"); ce != nil {
				ce.Write(zap.Any("cmd", cmd))
			}
		}), true},
		{"Log at a level (6)", logged(func(l *zap.Logger) { l.Log(zap.InfoLevel, "starting the app", zap.Any("cmd", cmd)) }), true},
		{"zap.Error of an error that formats it (7)", logged(func(l *zap.Logger) { l.Error("failed", zap.Error(fmt.Errorf("could not start %v", cmd))) }), true},
		{"zap.Object", logged(func(l *zap.Logger) { l.Info("m", zap.Object("cmd", cmd)) }), true},
		{"zap.Inline", logged(func(l *zap.Logger) { l.Info("m", zap.Inline(cmd)) }), true},
		{"zap.Reflect", logged(func(l *zap.Logger) { l.Info("m", zap.Reflect("cmd", cmd)) }), true},
		{"zap.Reflect of a struct", logged(func(l *zap.Logger) { l.Info("m", zap.Reflect("v", exported{cmd})) }), true},
		{"With", logged(func(l *zap.Logger) { l.With(zap.Any("cmd", cmd)).Info("m") }), true},
		// k0kubun/pp prints a struct's fields by reflection, unexported ones
		// too, and calls no method: AppCommand's text is a field it skips.
		{"pp", pp.Sprint(cmd), false},
		{"pp of a pointer", pp.Sprint(&cmd), false},
		{"pp's %v", pp.Sprintf("%v", cmd), false},
		{"pp of an exported field", pp.Sprint(exported{cmd}), false},
		{"pp of an unexported field", pp.Sprint(unexported{cmd}), false},
		{"the sugared logger", logged(func(l *zap.Logger) {
			l.Sugar().Infow("m", "cmd", cmd)
			l.Sugar().Infof("running %v", cmd)
			l.Sugar().Info(cmd)
		}), true},
	}...) {
		for _, word := range []string{"S3CR3T", "alice", "keploy.test", "postgres", "app.py", "--password"} {
			if strings.Contains(tc.out, word) {
				t.Errorf("%s names an argument (%s): %s", tc.name, word, tc.out)
			}
		}
		if named := strings.Contains(tc.out, "python3"); named != tc.named {
			t.Errorf("%s names the program: %v, want %v: %s", tc.name, named, tc.named, tc.out)
		}
		if tc.named && !strings.Contains(tc.out, program) && !strings.Contains(tc.out, `"program":"python3","args":3`) &&
			!strings.Contains(tc.out, `{"program": "python3", "args": 3}`) && !strings.Contains(tc.out, "program: python3") {
			t.Errorf("%s: want %q or its fields: %s", tc.name, program, tc.out)
		}
	}
	if got := cmd.WholeRedacted(); got != "DB_URL=<redacted> python3 app.py --password <redacted>" {
		t.Errorf("WholeRedacted() = %q", got)
	}
	var zero AppCommand
	if !zero.Empty() || zero.String() != "(no command)" || zero.WholeUnredacted() != "" || zero.Program() != "" {
		t.Errorf("the zero AppCommand: %v %q %q %q", zero.Empty(), zero.String(), zero.WholeUnredacted(), zero.Program())
	}
	if got := NewAppCommand("go test").String(); got != "go (1 arg)" {
		t.Errorf("one argument: %q", got)
	}
}

// An AppCommand's whole text is read only where the command is run, parsed
// or rewritten, logged whole at DEBUG (its secrets redacted), or written to
// keploy mock's local receipt; what each of these does with it the flow
// test follows (internal/cmdflow). A new reader is added here, by name.
func TestTheWholeTextOfAnAppCommandIsReadOnlyWhereListed(t *testing.T) {
	want := map[string]string{
		"utils.AppCommand.WholeRedacted":            "redacts it",
		"utils.LogWholeCommand":                     "logs it at DEBUG, redacted",
		"pkg/client/app.App.Setup":                  "parses it (-d)",
		"pkg/client/app.App.SetupDocker":            "rewrites it (the before-setup hook), logs it at DEBUG, redacted",
		"pkg/client/app.App.modifyDockerRun":        "rewrites it (keploy's flags), logs it at DEBUG, redacted",
		"pkg/client/app.App.SetupCompose":           "parses it (-f) and rewrites it",
		"pkg/client/app.App.setupComposeInMemory":   "rewrites it",
		"pkg/client/app.App.SetAppCommand":          "logs it at DEBUG, redacted",
		"pkg/client/app.App.GetAppCommand":          "hands it to its caller, which runs it",
		"pkg/client/app.App.composeChildTee":        "parses it (project, env files)",
		"pkg/client/app.App.composeDownProject":     "parses it (project)",
		"pkg/client/app.App.composeProjectArgs":     "parses it (project flags)",
		"pkg/client/app.App.run":                    "runs it",
		"pkg/client/app.App.newLibraryComposeStack": "parses it (project, env files)",
		"pkg/client/app.App.runComposeInProcess":    "parses it (compose up's flags)",
		"pkg/client/app.App.checkAgentTokenHandoff": "parses it (sudo)",
		"pkg/service/mock.mockService.Replay":       "writes keploy mock's local receipt; parses it (coverage report)",
		"pkg/service/mock.mockService.readCoverage": "parses it (coverage report)",
	}
	// The scanner itself: a method value, a method expression and the
	// field, in an unlisted function, are each a read; and so are a
	// package-level variable's method expression, method value, and
	// function literal that calls one, each under the variable's name (a
	// blank one's as _).
	fset := token.NewFileSet()
	probe, err := parser.ParseFile(fset, "probe.go", `package utils
var wholeOf = AppCommand.WholeUnredacted
var textOf = func(c AppCommand) string { return c.WholeUnredacted() }
var cmd0 AppCommand
var redactedOf, _ = cmd0.WholeRedacted, 0
var _ = AppCommand.WholeRedacted
func probe(cmd AppCommand) string {
	whole := cmd.WholeUnredacted
	_ = AppCommand.WholeRedacted(cmd)
	return whole() + cmd.c.unredacted
}`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var probeKeys []string
	for _, r := range wholeTextReads(fset, probe, "utils") {
		probeKeys = append(probeKeys, r.key+" "+r.what)
	}
	if want := []string{"utils.wholeOf WholeUnredacted", "utils.textOf WholeUnredacted", "utils.redactedOf WholeRedacted", "utils._ WholeRedacted",
		"utils.probe WholeUnredacted", "utils.probe WholeRedacted", "utils.probe unredacted"}; !slices.Equal(probeKeys, want) {
		t.Fatalf("the scanner read %q in the probe, want %q", probeKeys, want)
	}
	got := map[string]bool{}
	err = filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != ".." && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir, _ := filepath.Rel("..", filepath.Dir(path))
		for _, r := range wholeTextReads(fset, f, filepath.ToSlash(dir)) {
			if r.field {
				if r.key != "utils.AppCommand.WholeUnredacted" && r.key != "utils.AppCommand.Empty" {
					t.Errorf("%s: %s reads an AppCommand's whole text from its field, past its methods", r.pos, r.key)
				}
				continue
			}
			got[r.key] = true
			if _, listed := want[r.key]; !listed {
				t.Errorf("%s: %s reads an AppCommand's whole text (%s), and is not listed", r.pos, r.key, r.what)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var gone []string
	for key := range want {
		if !got[key] {
			gone = append(gone, key)
		}
	}
	sort.Strings(gone)
	for _, key := range gone {
		t.Errorf("%s is listed and reads no AppCommand's whole text", key)
	}
}

// wholeTextRead is a place a function reads an AppCommand's whole text.
type wholeTextRead struct {
	pos   token.Position
	key   string // the function, dir.Func or dir.Type.Method
	what  string // the method, or the field
	field bool
}

// wholeTextReads are the places f's declarations read an AppCommand's whole
// text: any use of its two methods (a call, a method value, a method
// expression), and in package utils (dir), whose alone it is, of the
// unexported field that holds it. A function's are keyed by the function; a
// package-level declaration's (a variable's initializer, a function literal
// or a method expression it holds) by the names it declares (a blank one,
// var _ = ..., as "_"). Reflection and a template that calls a method by
// its name ({{.WholeUnredacted}} executed on an AppCommand) read the text
// past it: a stated limit. Forms no code writes, e.g. fmt printing the
// field's pointer in package utils (fmt.Sprint(c.c), which names no
// unredacted), are not read; a real one is a bug to fix.
func wholeTextReads(fset *token.FileSet, f *ast.File, dir string) []wholeTextRead {
	var reads []wholeTextRead
	for _, d := range f.Decls {
		var key string
		var node ast.Node = d
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			key = dir + "." + d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv := d.Recv.List[0].Type
				if s, ok := recv.(*ast.StarExpr); ok {
					recv = s.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					key = dir + "." + id.Name + "." + d.Name.Name
				}
			}
			node = d.Body
		case *ast.GenDecl:
			var names []string
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						if n.Name != "_" {
							names = append(names, n.Name)
						}
					}
				case *ast.TypeSpec:
					names = append(names, sp.Name.Name)
				}
			}
			if len(names) == 0 {
				names = []string{"_"}
			}
			key = dir + "." + strings.Join(names, ",")
		}
		ast.Inspect(node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch name := sel.Sel.Name; {
			case name == "WholeUnredacted" || name == "WholeRedacted":
				reads = append(reads, wholeTextRead{fset.Position(sel.Pos()), key, name, false})
			case name == "unredacted" && dir == "utils":
				reads = append(reads, wholeTextRead{fset.Position(sel.Pos()), key, name, true})
			}
			return true
		})
	}
	return reads
}
