package cmdflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keploy is where keploy reads the command it runs for the user, and what a
// value made of it is.
var keploy = Rules{
	Module: "go.keploy.io/server/v3",
	Fields: map[string]Kind{
		// The command given with -c (or keploy.yml's command).
		"config.Config.Command": Raw,
		// The command a run starts (instrumentation's Run).
		"pkg/models.RunOptions.AppCommand": Raw,
		// A test set's own: its app's command and the scripts run before and
		// after it (keploy/<set>/config.yaml).
		"pkg/models.TestSet.AppCommand": Raw,
		"pkg/models.TestSet.PreScript":  Raw,
		"pkg/models.TestSet.PostScript": Raw,
		// keploy's own command line, -c and all, as a test report records it
		// (utils.GetFullCommandUsed): redacted.
		"config.Test.CmdUsed":           Redacted,
		"pkg/models.TestReport.CmdUsed": Redacted,
	},
	Methods: map[string]Kind{
		"WholeUnredacted": Raw,
		"WholeRedacted":   Redacted,
	},
	Redactors: map[string]bool{
		"utils.RedactCommandLine": true,
		"utils.RedactCmdArgs":     true,
		"utils.RedactArgv":        true,
	},
	Sanitizers: map[string]string{
		// What keploy parses out of a docker or compose command, which its
		// lines name: none is a credential.
		"pkg/platform/engine.Detect":                 "the container engine a command runs (docker, podman)",
		"pkg/platform/engine.Invocation#0":           "the container engine a command runs",
		"pkg/platform/docker.ParseDockerCmd#0":       "the container a docker command names (--name)",
		"pkg/platform/docker.ParseDockerCmd#1":       "the network a docker command names (--network)",
		"pkg/platform/http.runFlagsOf#2":             "the networks a docker command names (--network)",
		"pkg/platform/http.runFlagsOf#3":             "the --add-host entries a docker command names",
		"utils.ContainerNameFromDockerRun":           "the container a docker command names (--name)",
		"utils.NewAppCommand":                        "an AppCommand prints its program and how many words follow it",
		"pkg/platform/docker.FindComposeFiles":       "the compose files a compose command names (-f, --file, COMPOSE_FILE, or default files)",
		"pkg/client/app.composeProjectFlag":          "the project compose's own options name (-p, before its subcommand)",
		"pkg/client/app.composeProjectEnvAssignment": "the project a compose command names (COMPOSE_PROJECT_NAME=)",
		"pkg/client/app.composeUpSemantics":          "the services a compose command names (--exit-code-from)",
		"pkg/client/app.composeUserAbort":            "the abort flags and services a compose up names (--abort-on-container-exit, --exit-code-from, --attach)",
		"utils.DetectLanguage#1":                     "the program's path",
		"pkg/platform/coverage/report.FromCommand":   "the coverage report a test command names (-coverprofile, --cov-report)",
		// keploy's own command line (os.Args, through a package-level
		// variable's function literal) holds the agent's log, which its CLI
		// names to it.
		"utils/log.clientLogFileArg#0": "the log keploy's CLI names to its agent (keploy agent --client-log-file)",
	},
	Allowed: map[string]string{
		"utils.CommandContext: string(cmdStr[i])":                                                          "the one shell operator the command needs a shell for (shellOperators)",
		`utils.isGoBinary: fmt.Sprintf("failed to open file %s", filePath)`:                                "the program's path (DetectLanguage's executable)",
		`pkg/platform/coverage/golang.checkForCoverFlag: zap.String("file", binary)`:                       "the program's path",
		`utils.isGoBinary: zap.String("file", filePath)`:                                                   "the program's path (DetectLanguage's executable)",
		`pkg/platform/docker.Impl.ExtractNetworksForContainer: zap.String("containerName", containerName)`: "the container a docker start command names (ParseDockerCmd)",
	},
	AllowedRedacted: map[string]string{
		"utils.JSONWriter.Write: string(data)": "a test report printed with --json, its cmdUsed redacted as the report's file holds it",
	},
	Sinks: map[string]string{
		"utils.LogError": "ERROR",
	},
	Skip: []string{"tests", "internal/cmdflow"},
}

func TestKeploysCommandReachesNoLineItMayNot(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal(err)
	}
	n, err := Files(root, keploy.Skip)
	if err != nil {
		t.Fatal(err)
	}
	if n < 500 {
		t.Fatalf("read %d Go files, want the module's", n)
	}
	found, err := Trace(root, keploy)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Errorf("%s", strings.TrimPrefix(f.String(), root+string(filepath.Separator)))
	}
}

// Every field, function, method and type keploy's rules name is in the
// module: a rename that takes one away fails here, not by a rule that no
// longer holds.
func TestKeploysRulesNameWhatTheModuleHas(t *testing.T) {
	unknown, err := Unknown(filepath.Join("..", ".."), keploy)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range unknown {
		t.Errorf("the rule names nothing in the module: %s", u)
	}
}

// fixture is a module of its own, read by Trace alone (never built): every
// line marked FOUND must be found, and no other.
var fixture = map[string]string{
	"go.mod": "module example.com/fx\n",
	"config/config.go": `package config

type Config struct {
	Command string
	Name    string
}
`,
	"util/util.go": `package util

import "go.uber.org/zap"

func Redact(s string) string { return "<redacted>" }

// Safe carries a command; its methods but Whole and WholeRedacted print its
// program.
type Safe struct{ c *string }

func NewSafe(s string) Safe          { return Safe{&s} }
func (s Safe) Whole() string         { return *s.c }
func (s Safe) WholeRedacted() string { return Redact(*s.c) }
func (s Safe) String() string        { return "prog (2 args)" }

func Program(s string) string { return s }

// Parse returns a name it reads in s, and s rewritten.
func Parse(s string) (string, string) { return s, s + " --x" }

func LogError(logger *zap.Logger, err error, msg string, fields ...zap.Field) {
	logger.Error(msg, append(fields, zap.Error(err))...)
}
`,
	"run/run_unix.go": `//go:build !windows

package run

import "go.uber.org/zap"

func Start(logger *zap.Logger, cmd string) {
	logger.Info("starting", zap.String("cmd", cmd)) // FOUND
}
`,
	"run/run_windows.go": `//go:build windows

package run

import "go.uber.org/zap"

func Start(logger *zap.Logger, cmd string) {
	logger.Info("starting (windows)", zap.String("cmd", cmd)) // FOUND
}
`,
	"app/app.go": `package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"example.com/fx/config"
	"example.com/fx/run"
	"example.com/fx/util"
	"go.uber.org/zap"
)

type App struct {
	logger *zap.Logger
	cmd    string
	safe   util.Safe
}

type Runner interface{ Start(cmd string) }

type impl struct{ logger *zap.Logger }

func (i *impl) Start(cmd string) {
	i.logger.Warn("starting", zap.String("cmd", cmd)) // FOUND
}

func Run(logger *zap.Logger, cfg *config.Config, r Runner) error {
	cmd := cfg.Command
	args := strings.Fields(cmd)
	// The seven forms a log guard that reads calls alone missed.
	s := util.Redact(cmd)
	logger.Info("starting the app", zap.String("cmd", s))   // FOUND
	logger.Info("starting the app", zap.String("cmd", cmd)) // FOUND
	logger.Warn("starting the app", zap.Strings("args", args)) // FOUND
	logger.Error("starting the app", zap.Any("cmd", args))   // FOUND
	if ce := logger.Check(zap.InfoLevel, "starting the app"); ce != nil {
		ce.Write(zap.String("cmd", util.Redact(cmd))) // FOUND
	}
	logger.Log(zap.InfoLevel, "starting the app", zap.String("cmd", util.Redact(strings.Join(args, " ")))) // FOUND
	_ = fmt.Errorf("could not start %v", args) // FOUND

	// A struct's field, a result, a method by its name, every sink.
	a := &App{logger: logger, cmd: cmd, safe: util.NewSafe(cmd)}
	a.run()
	newJob(logger, cmd).start()
	run.Start(logger, cmd)
	logger.Info("rewritten", zap.String("cmd", rewrite(cmd))) // FOUND
	r.Start(cfg.Command)
	logger.Debug("raw at debug", zap.String("cmd", cmd)) // FOUND
	fmt.Println("running", cmd)                         // FOUND
	_ = errors.New(cmd)                                 // FOUND
	logger.With(zap.String("cmd", cmd)).Info("with")    // FOUND
	util.LogError(logger, nil, "failed", zap.String("cmd", cmd)) // FOUND
	var b strings.Builder
	b.WriteString(cmd)
	logger.Info(b.String())                                // FOUND
	logger.Info("argv", zap.Strings("argv", os.Args))      // FOUND
	for _, w := range args {
		logger.Info(w) // FOUND
	}
	c := exec.Command("sh", "-c", cmd)
	logger.Info("exec", zap.String("cmd", c.String())) // FOUND
	logger.Info("exec", zap.Strings("args", c.Args))   // FOUND
	_, rest := util.Parse(cmd)
	logger.Info(rest) // FOUND
	if cmd == "" {
		panic(cmd) // FOUND
	}

	// None of these names the command.
	safe := util.NewSafe(cmd)
	logger.Info("starting", zap.Any("cmd", safe))
	logger.Info("starting", zap.Inline(safe))
	logger.Info("starting", zap.Stringer("cmd", safe))
	_ = fmt.Errorf("could not start %v", safe)
	logger.Info("program", zap.String("program", util.Program(cmd)))
	name, _ := util.Parse(cmd)
	logger.Info("parsed", zap.String("name", name))
	logger.Debug("whole", zap.String("cmd", util.Redact(cmd)))
	logger.Debug("whole", zap.String("cmd", safe.WholeRedacted()))
	logger.Info("size", zap.Int("n", len(cmd)), zap.Bool("docker", strings.Contains(cmd, "docker")))
	logger.Info("name", zap.String("name", cfg.Name))
	if err := doThing(cmd); err != nil {
		logger.Error("failed", zap.Error(err))
	}
	return nil
}

func (a *App) run() {
	a.logger.Info("running", zap.String("cmd", a.cmd))           // FOUND
	a.logger.Info("running", zap.String("cmd", a.safe.Whole()))  // FOUND
	a.logger.Info("running", zap.String("cmd", a.safe.WholeRedacted())) // FOUND
	a.logger.Info("running", zap.Stringer("cmd", a.safe))
}

func rewrite(c string) string { return c + " --flag" }

// Service holds the config: its command is read through a field of the
// receiver's, whose type the struct's declaration tells.
type Service struct {
	logger *zap.Logger
	cfg    *config.Config
}

func (s *Service) Start() {
	s.logger.Info("service", zap.String("cmd", s.cfg.Command)) // FOUND
}

type Job struct {
	logger *zap.Logger
	line   string
}

// newJob's fields hold what it is given, from the literal it returns.
func newJob(l *zap.Logger, c string) *Job { return &Job{logger: l, line: c} }

func (j *Job) start() {
	j.logger.Info("job", zap.String("line", j.line)) // FOUND
}

func doThing(cmd string) error { return nil }

// unrelated is never given the command: a parameter's name is no source.
func unrelated(logger *zap.Logger, cmd string) {
	logger.Info("unrelated", zap.String("cmd", cmd))
}
`,
	"logs/logs.go": `package logs

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"

	"example.com/fx/config"
	"example.com/fx/util"
	"go.uber.org/zap"
)

// Package-level initializers: what they give a variable, and a line one
// writes.
var argv = strings.Join(os.Args, " ")
var wholeOf = util.Safe.Whole
var textOf = func(s util.Safe) string { return s.Whole() }
var started = fmt.Errorf("started as %v", os.Args) // FOUND
var argc = len(os.Args)

// Logs writes the command with log/slog and a *log.Logger.
func Logs(logger *zap.Logger, cfg *config.Config) {
	cmd := cfg.Command
	ctx := context.Background()
	slog.Info("starting", "cmd", cmd)                             // FOUND
	slog.Warn("starting", slog.String("cmd", cmd))                // FOUND
	slog.Debug("starting", "cmd", cmd)                            // FOUND
	slog.Log(ctx, slog.LevelError, "x", "cmd", util.Redact(cmd)) // FOUND
	sl := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sl.InfoContext(ctx, "x", "cmd", cmd) // FOUND
	l := log.New(os.Stderr, "keploy: ", 0)
	l.Println(cmd)              // FOUND
	l.Printf("ran %s", cmd)     // FOUND
	p := log.New(os.Stderr, cmd, 0) // FOUND
	p.SetPrefix(cmd)            // FOUND
	log.SetPrefix(cmd)          // FOUND
	logger.Info("argv", zap.String("argv", argv))                       // FOUND
	logger.Info("whole", zap.String("cmd", wholeOf(util.NewSafe(cmd)))) // FOUND
	logger.Info("text", zap.String("cmd", textOf(util.NewSafe(cmd))))   // FOUND
	sugar := logger.Sugar()
	sugar.Infoln("cmd", cmd)                                         // FOUND
	sugar.Warnln(cmd)                                                // FOUND
	sugar.Errorln(cmd)                                               // FOUND
	sugar.DPanicln(cmd)                                              // FOUND
	logger.Named(cmd).Info("x")                                      // FOUND
	logger.WithOptions(zap.Fields(zap.String("cmd", cmd))).Info("x") // FOUND
	logger.Named("svc").WithOptions(zap.AddCallerSkip(1)).Info("x", zap.String("cmd", cmd)) // FOUND

	// None of these names it.
	slog.Debug("starting", "cmd", util.Redact(cmd))
	slog.Log(ctx, slog.LevelDebug, "x", "cmd", util.Redact(cmd))
	sl.DebugContext(ctx, "x", "cmd", util.Redact(cmd))
	sl.Log(ctx, slog.LevelDebug, "x", "cmd", util.Redact(cmd))
	logger.Info("argc", zap.Int("n", argc))
	l.Println(len(cmd))
	sugar.Debugln("cmd", util.Redact(cmd))
	logger.Named("svc").WithOptions(zap.AddCallerSkip(1)).Info("x")
}
`,
	"more/more.go": `package more

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

	"example.com/fx/config"
	"example.com/fx/util"
	"go.uber.org/zap"
)

type holder struct {
	C string
	N int
}

type counts struct{ N int }

// Line is a type of the module's declared as a string.
type Line string

func wrap(l Line) Line { return l + " !" }

var last string

// More takes the command to a line in the forms a trace that read only
// calls, plain values and a console's name missed.
func More(logger *zap.Logger, cfg *config.Config) {
	cmd := cfg.Command
	f := func(given string) { logger.Info("closure", zap.String("cmd", given)) } // FOUND
	f(cmd)
	go func(started string) { logger.Info("goroutine", zap.String("cmd", started)) }(cmd) // FOUND
	keys := map[string]bool{cmd: true}
	for k := range keys {
		logger.Info("key", zap.String("cmd", k)) // FOUND
	}
	seen := map[string]int{}
	seen[cmd] = 1
	for word := range seen {
		logger.Info("key", zap.String("cmd", word)) // FOUND
	}
	h := holder{C: cmd, N: 1}
	logger.Info("struct", zap.Any("h", h))                          // FOUND
	logger.Info("a literal", zap.Any("h", holder{C: cmd, N: 2}))  // FOUND
	var h2 holder
	h2.C = cmd
	logger.Info("struct", zap.Any("h", h2))           // FOUND
	var e exec.Error
	e.Name = cmd
	logger.Info("a library's struct", zap.Any("e", e)) // FOUND
	logger.Info("config", zap.Any("cfg", cfg))        // FOUND
	_ = fmt.Errorf("config %+v", *cfg)                // FOUND
	logger.Info("named", zap.String("cmd", string(wrap(Line(cmd))))) // FOUND
	js, _ := json.Marshal(map[string]string{"c": cmd})
	logger.Info("json", zap.String("cmd", string(js)))                  // FOUND
	logger.Error("join", zap.Error(errors.Join(&exec.Error{Name: cmd}))) // FOUND
	var b strings.Builder
	_, _ = io.WriteString(&b, cmd)                        // FOUND
	logger.Info("builder", zap.String("cmd", b.String())) // FOUND
	_, _ = os.Stdout.WriteString(cmd)                     // FOUND
	var w io.Writer = os.Stdout
	fmt.Fprintln(w, cmd) // FOUND
	log.Fatalln(cmd)     // FOUND
	info := logger.Info
	info("a method value", zap.String("cmd", cmd)) // FOUND
	safe := util.NewSafe(cmd)
	whole := safe.Whole
	logger.Info("a method value", zap.String("cmd", whole()))               // FOUND
	logger.Info("a method expression", zap.String("cmd", util.Safe.Whole(safe))) // FOUND
	last = cmd
	logLast(logger)

	// None of these names it.
	logger.Info("struct", zap.Any("n", counts{N: len(cmd)}))
	redacted := *cfg
	redacted.Command = util.Redact(cfg.Command)
	logger.Debug("config", zap.Any("cfg", redacted))
	logger.Info("value", zap.Any("safe", safe))
	other := map[string]bool{"x": len(cmd) > 0}
	for name := range other {
		logger.Info("key", zap.String("name", name))
	}
}

func logLast(logger *zap.Logger) {
	logger.Info("a package-level variable", zap.String("cmd", last)) // FOUND
}

// Shown and ShownRedacted print what they are given where the rules allow
// the command redacted alone: show is given it raw too, showRedacted never.
func Shown(cfg *config.Config) {
	show(util.Redact(cfg.Command))
	show(cfg.Command)
	showRedacted(util.Redact(cfg.Command))
}

func show(s string)         { fmt.Println(s) } // FOUND
func showRedacted(s string) { fmt.Println(s) }
`,
}

var fixtureRules = Rules{
	Module:          "example.com/fx",
	Fields:          map[string]Kind{"config.Config.Command": Raw},
	Methods:         map[string]Kind{"Whole": Raw, "WholeRedacted": Redacted},
	Redactors:       map[string]bool{"util.Redact": true},
	Sanitizers:      map[string]string{"util.NewSafe": "", "util.Program": "", "util.Parse#0": ""},
	SanitizerTypes:  map[string]string{"util.Safe": ""},
	Sinks:           map[string]string{"util.LogError": "ERROR"},
	AllowedRedacted: map[string]string{"more.show: s": "", "more.showRedacted: s": ""},
}

// Trace finds every line of the fixture that takes the command, in each form
// the guard before it missed, and none that holds only what a sanitizer
// gives.
func TestTraceFindsEveryLineThatTakesTheCommand(t *testing.T) {
	dir := t.TempDir()
	want := map[string]bool{}
	for name, src := range fixture {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "// FOUND") {
				want[fmt.Sprintf("%s:%d", name, i+1)] = true
			}
		}
	}
	found, err := Trace(dir, fixtureRules)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range found {
		rel, _ := filepath.Rel(dir, f.Pos.Filename)
		at := fmt.Sprintf("%s:%d", filepath.ToSlash(rel), f.Pos.Line)
		got[at] = true
		if !want[at] {
			t.Errorf("found a line that does not take the command: %s", f)
		}
	}
	for at := range want {
		if !got[at] {
			t.Errorf("%s takes the command, and Trace did not find it", at)
		}
	}
	if len(want) != 72 {
		t.Errorf("the fixture marks %d lines, want 72", len(want))
	}
}
