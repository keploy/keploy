package utils

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// AppCommand is the command keploy runs for the user (the command given with
// -c, a compose command keploy rewrote, a test command) as keploy's code
// carries it to a log line or an error. Every way to print it writes its
// program and how many arguments follow it, never an argument: String and
// Format (%v, %s, %q, %#v, %+v, any verb), zap (zap.Any, zap.Object,
// zap.Inline, zap.Stringer, zap.Reflect), and JSON, YAML or text encoders.
// Its arguments carry what the user passes the app (hosts, users, passwords,
// tokens, in more forms than any redactor reads), and a line at INFO
// reaches CI logs, keploy-logs.txt and support bundles at the default
// level. It is no string, so zap.String, string concatenation and the
// strings package do not take it.
//
// Its whole text comes out of two methods alone: WholeUnredacted, for the
// code that runs, parses or rewrites the command, and WholeRedacted (its
// secrets redacted, RedactCommandLine), for the DEBUG line that names it
// whole (LogWholeCommand). A test lists the functions either is called in
// (TestTheWholeTextOfAnAppCommandIsReadOnlyWhereListed), and the flow test
// (internal/cmdflow) follows what they return.
//
// Its text is held behind a pointer, so a struct that holds an AppCommand in
// a field fmt cannot call methods of (an unexported one) prints the
// pointer's address, not the text. k0kubun/pp, which prints a struct's
// fields by reflection and calls no method, skips the field (its pp tag).
// The zero AppCommand is an empty command.
type AppCommand struct {
	c *appCommandText `pp:"-"`
}

type appCommandText struct {
	unredacted string
	program    string
	args       int
}

// NewAppCommand is cmd as keploy's code carries it to a log line
// (AppCommand).
func NewAppCommand(cmd string) AppCommand {
	program, args := appProgram(cmd)
	return AppCommand{&appCommandText{unredacted: cmd, program: program, args: args}}
}

// Program is the command's program: the base name of its first word that is
// no assignment (TOKEN=x ./app runs ./app) when that word is a program's
// path and its base name a program's name, else that word by its shape and
// tag (appProgram). An empty command's is "".
func (c AppCommand) Program() string {
	if c.c == nil {
		return ""
	}
	return c.c.program
}

// ArgCount is how many words follow the command's program.
func (c AppCommand) ArgCount() int {
	if c.c == nil {
		return 0
	}
	return c.c.args
}

// Empty reports whether the command is "", as no -c gives.
func (c AppCommand) Empty() bool {
	return c.c == nil || c.c.unredacted == ""
}

// String is the command by its program and how many arguments follow it:
// "python3 (3 args)", "go (1 arg)", "(no command)".
func (c AppCommand) String() string {
	switch {
	case c.Empty():
		return "(no command)"
	case c.ArgCount() == 1:
		return c.Program() + " (1 arg)"
	}
	return c.Program() + " (" + strconv.Itoa(c.ArgCount()) + " args)"
}

// Format writes String for every verb, so no verb or flag (%#v, %+v, %x)
// prints the command's text.
func (c AppCommand) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(c.String()))
}

// GoString is String.
func (c AppCommand) GoString() string { return c.String() }

// MarshalLogObject writes the command's program and how many arguments
// follow it (program, args): zap.Inline(c) gives a line
// {"program": "python3", "args": 3}, zap.Any("cmd", c) and zap.Object
// {"cmd": {"program": "python3", "args": 3}}.
func (c AppCommand) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("program", c.Program())
	enc.AddInt64("args", int64(c.ArgCount()))
	return nil
}

// MarshalJSON is {"program": ..., "args": N}, as zap.Reflect and a JSON
// encoder of a struct that holds it write it.
func (c AppCommand) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Program string `json:"program"`
		Args    int    `json:"args"`
	}{c.Program(), c.ArgCount()})
}

// MarshalText is String, as a YAML or text encoder writes it.
func (c AppCommand) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// WholeUnredacted is the command's whole text as the user wrote it: for the
// code that runs it, parses it (a docker command's container and network, a
// compose command's files) or rewrites it, never for a log line, an error
// or a report.
func (c AppCommand) WholeUnredacted() string {
	if c.c == nil {
		return ""
	}
	return c.c.unredacted
}

// WholeRedacted is the command's whole text with its secrets redacted
// (RedactCommandLine), for the DEBUG line that names it whole
// (LogWholeCommand): it keeps the command's hosts, paths and users, so no
// line at INFO or above, and no error, takes it.
func (c AppCommand) WholeRedacted() string {
	return RedactCommandLine(c.WholeUnredacted())
}

// appProgram is cmd's program, as AppCommand names it, and how many words
// follow it. The program is the first word that is no assignment: its base
// name when the word is a program's path (programPath) and that name is a
// program's (programBaseName: letters, digits, '.', '_', '+', '-', not
// first), else the word by its shape and tag, as a process's logged
// argv[0] is. A first word that is no program (scott/<pw>@orcl,
// -p<password>, user:<pw>@host) so names nothing at INFO.
func appProgram(cmd string) (string, int) {
	words := splitCommandLine(cmd)
	i := 0
	for i < len(words) {
		name, _, assigns := strings.Cut(words[i], "=")
		if !assigns || !assignedName.MatchString(name) {
			break
		}
		i++
	}
	if i == len(words) {
		return "", 0
	}
	raw, w := words[i], RedactArgv(words[i : i+1])[0]
	if base := w[strings.LastIndexAny(w, `/\`)+1:]; programPath(raw) && programBaseName.MatchString(base) {
		return base, len(words) - i - 1
	}
	return shapedWord(nil, w), len(words) - i - 1
}

// LogWholeCommand logs cmd, a command keploy runs for the user, whole but
// for its secrets (WholeRedacted), under key, beside fields, at DEBUG
// alone: the line at INFO or above that it follows names cmd by its program
// (zap.Inline(cmd)). With debug off nothing is redacted. RedactArgv says
// what is redacted, and its stated limits what is not.
func LogWholeCommand(logger *zap.Logger, key string, cmd AppCommand, fields ...zap.Field) {
	if ce := logger.Check(zap.DebugLevel, WholeCommandMessage); ce != nil {
		ce.Write(append([]zap.Field{zap.String(key, cmd.WholeRedacted())}, fields...)...)
	}
}

// logAppStart logs msg, that keploy starts cmd as args (exec.Cmd.Args: sh -c
// and the command, or its words where no shell is), at INFO by cmd's
// program and how many words follow it, and at DEBUG args whole but for
// their secrets, under key.
func logAppStart(logger *zap.Logger, msg string, cmd AppCommand, key string, args []string) {
	logger.Info(msg, zap.Inline(cmd))
	LogWholeCmdArgs(logger, key, args)
}
