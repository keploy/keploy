package utils

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/pflag"
)

// The tag a failure carries must decide the code through every wrap it
// crosses on its way to the exit: the agent's hook load wraps it, Setup wraps
// that, and the CLI that launched the agent wraps it again.
func TestExitCodeFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"privileges", fmt.Errorf("%w: failed to set memlock rlimit: %w", ErrPrivilegeRequired, syscall.EPERM), ExitPrivilegeRequired},
		{"privileges, wrapped again", fmt.Errorf("failed setting up the environment: %w", fmt.Errorf("failed to hook into the app: %w", ErrPrivilegeRequired)), ExitPrivilegeRequired},
		{"environment", fmt.Errorf("%w: neither debugfs nor tracefs are mounted", ErrEnvironmentUnsupported), ExitEnvironmentUnsupported},
		{"environment, wrapped again", fmt.Errorf("failed to hook into the app: %w", fmt.Errorf("%w: could not find a non-loopback IP for the container", ErrEnvironmentUnsupported)), ExitEnvironmentUnsupported},
		// A bare permission error is NOT a privilege failure by itself: a
		// file keploy cannot write is not a capability to grant. Only the
		// code that knows the refused operation was a privileged kernel one
		// tags it.
		{"an untagged permission error", fmt.Errorf("open keploy.yml: %w", syscall.EACCES), ExitKeployError},
		{"anything else", errors.New("address already in use"), ExitKeployError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.err); got != tc.want {
				t.Fatalf("ExitCodeFor(%q) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// Callers switch on these codes (the VS Code extension does), so each must
// mean one thing: distinct from the others, from the enterprise build's 5, from
// Go's own crash exit (2: an unrecovered panic or a fatal error), and from what
// the shell and signals already mean. Each also stays below 10: the enterprise
// build carries these codes, and its `keploy ui` contract keeps 10-125 for its
// own (ui-capture spec/reserved-exit-codes.json), so a code of 10 or more fails
// that build's contract gate.
//
// The codes are read out of exitcodes.go, every `Exit<Name> = <int>` constant
// in it, not listed here: a hand-written list left ExitUsageError out, and let
// it be 64.
func TestKeploysExitCodesAreDistinct(t *testing.T) {
	codes := exitCodeConstants(t)
	if len(codes) == 0 {
		t.Fatal("no Exit<Name> = <int> constant read from exitcodes.go")
	}
	taken := map[int]string{
		0: "success",
		2: "Go's runtime: an unrecovered panic or a fatal error",
		5: "enterprise: a session is required",
		// The enterprise build's `keploy ui` contract still uses 0-7; its
		// overlap with 1 and 3-6 is a documented allowance there, until its
		// codes move to 10-125. A new one would fail that build as 2 did.
		7:   "the enterprise ui contract's usage code",
		126: "shell: not executable",
		127: "shell: command not found",
	}
	for name, code := range codes {
		if other, clash := taken[code]; clash {
			t.Fatalf("%s = %d collides with %s", name, code, other)
		}
		if code < 0 || code >= 10 {
			t.Fatalf("%s = %d is outside 1-9: the enterprise ui contract keeps 10-125 for its own codes, and 128+N is a death by signal", name, code)
		}
		taken[code] = name
	}
	for name, want := range map[string]int{
		"ExitKeployError":            ExitKeployError,
		"ExitPrivilegeRequired":      ExitPrivilegeRequired,
		"ExitUnsupportedPlatform":    ExitUnsupportedPlatform,
		"ExitEnvironmentUnsupported": ExitEnvironmentUnsupported,
		"ExitUsageError":             ExitUsageError,
	} {
		if got, ok := codes[name]; !ok || got != want {
			t.Fatalf("read %s = %d (found %v) from exitcodes.go, want %d: the reader misses constants", name, got, ok, want)
		}
	}
}

// exitCodeConstants is every `Exit<Name> = <int>` constant in exitcodes.go.
func exitCodeConstants(t *testing.T) map[string]int {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "exitcodes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]int{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Exit") {
					continue
				}
				if i >= len(vs.Values) {
					t.Fatalf("%s has no value of its own (it repeats the one above it); give it its own", name.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s is not an integer literal; read it some other way", name.Name)
				}
				v, err := strconv.Atoi(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				codes[name.Name] = v
			}
		}
	}
	return codes
}

// A usage error is one tagged where the command line is parsed, or an
// unknown command; pflag's error types alone are not one: keploy reading a
// flag it never defined returns them too, and that is keploy's bug.
func TestIsUsageError(t *testing.T) {
	parse := func(args ...string) error {
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Int("delay", 0, "")
		return fs.Parse(args)
	}
	misuse := func() error {
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		_, err := fs.GetString("output")
		return err
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"unknown command":          {errors.New(`unknown command "bogus" for "keploy"`), true},
		"unknown verb":             {errors.New(`unknown command "bogus" for "keploy mock"`), true},
		"tagged unknown flag":      {UsageError(parse("--nope")), true},
		"tagged unknown shorthand": {UsageError(parse("-Z")), true},
		"tagged missing value":     {UsageError(parse("--delay")), true},
		"tagged invalid value":     {UsageError(parse("--delay", "abc")), true},
		"tagged bad syntax":        {UsageError(parse("---delay")), true},
		"tagged, then wrapped":     {fmt.Errorf("parsing: %w", UsageError(parse("--nope"))), true},
		"untagged flag error":      {parse("--nope"), false},
		"flag API misuse":          {misuse(), false},
		"keploy failure":           {errors.New("failed to start the agent"), false},
		"look-alike":               {errors.New("unknown command type: CONNECT"), false},
		"privilege":                {ErrPrivilegeRequired, false},
		"nil":                      {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.want && tc.err == nil {
				t.Fatal("the case's error is nil: pflag accepted it")
			}
			if got := IsUsageError(tc.err); got != tc.want {
				t.Fatalf("IsUsageError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	if UsageError(nil) != nil {
		t.Fatal("UsageError(nil) is not nil")
	}
	if err := parse("--nope"); UsageError(err).Error() != err.Error() {
		t.Fatalf("the tag changed the message: %q", UsageError(err).Error())
	}
}
