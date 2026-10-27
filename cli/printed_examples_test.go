package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/keploy/shlex"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// TestEveryPrintedExampleIsACommandLineKeployAccepts: a command line keploy
// prints for a user to copy -- an "Example:" hint, the root's examples, the
// `keploy example` text, any command's own Example -- has to be one keploy
// accepts, word for word, or the user who follows it is handed an error
// instead of a recording. `keploy record` has had no --delay since v2.1.0
// (#1684 registered it on `keploy test` only, the one command that waits for
// the application before it fires requests; record never read it), yet the
// hint `keploy test` prints when there is nothing to replay went on telling
// the user to run `keploy record ... --delay 6`, and the docs and samples
// copied it: the first command of a new user's walkthrough ended in
// `unknown flag: --delay`.
//
// Each line is split as a shell splits it, cut at the first shell operator
// (`keploy config defaults | grep -i port`), and parsed by the real command
// tree's flags (the configurator's own, aliases included), so a flag the
// command does not have, a value of the wrong type, or a quote that never
// closes fails here rather than in front of the user.
func TestEveryPrintedExampleIsACommandLineKeployAccepts(t *testing.T) {
	examples := map[string]string{
		"provider.RootExamples":           provider.RootExamples,
		"provider.Examples":               provider.Examples,
		"provider.ExampleOneClickInstall": provider.ExampleOneClickInstall,
	}
	for _, kind := range []utils.CmdType{utils.Native, utils.DockerRun, utils.DockerStart, utils.DockerCompose, utils.Empty} {
		examples["provider.RecordExample("+string(kind)+")"] = provider.RecordExample(kind)
		examples["provider.DelayExample("+string(kind)+")"] = provider.DelayExample(kind)
	}
	for _, command := range []string{"record", "test"} {
		examples["provider.NativeExample("+command+")"] = provider.NativeExample(command)
		examples["provider.DockerExample("+command+")"] = provider.DockerExample(command)
		examples["provider.ComposeExample("+command+")"] = provider.ComposeExample(command)
		examples["provider.BuildDelayExample("+command+")"] = provider.BuildDelayExample(command)
	}
	// Every command's own Example, all the way down the tree (`keploy contract
	// download`'s told the user to pass --service, a flag it does not have).
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Example != "" {
			examples["`"+c.CommandPath()+"`'s Example"] = c.Example
		}
		// A Long text is prose: its command lines are the indented ones.
		var indented []string
		for _, line := range strings.Split(c.Long, "\n") {
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				indented = append(indented, line)
			}
		}
		if len(indented) > 0 {
			examples["`"+c.CommandPath()+"`'s Long"] = strings.Join(indented, "\n")
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRealRoot())

	checked := map[string]int{}
	for source, text := range examples {
		for _, line := range commandLines(text) {
			checked[source]++
			if _, err := parseExample(line); err != nil {
				t.Errorf("%s: %v", source, err)
			}
		}
	}
	// The sources whose lines this test is for: if one stops yielding a
	// `keploy ...` line, it changed shape and this test asserts nothing of it.
	for _, source := range []string{
		"provider.RecordExample(native)", "provider.RecordExample(docker-run)", "provider.RecordExample(docker-compose)",
		"provider.DelayExample(docker-run)", "provider.DelayExample(docker-compose)",
		"provider.NativeExample(record)", "provider.NativeExample(test)",
		"provider.DockerExample(record)", "provider.DockerExample(test)",
		"provider.ComposeExample(record)", "provider.ComposeExample(test)",
		"provider.BuildDelayExample(record)", "provider.BuildDelayExample(test)",
		"provider.RootExamples", "provider.Examples", "provider.ExampleOneClickInstall",
		"`keploy record`'s Example", "`keploy test`'s Example",
		"`keploy contract download`'s Example", "`keploy contract test`'s Example",
		"`keploy ca clean`'s Example", "`keploy normalize`'s Example", "`keploy templatize`'s Example",
		"`keploy mock`'s Long",
	} {
		if checked[source] == 0 {
			t.Errorf("%s yielded no `keploy ...` line to check", source)
		}
	}
}

// The `keploy record` line shown to a user whose application starts the way
// cmdType says is one that records such an application: its -c is detected as
// that kind (docker start, which keploy refuses, is shown docker run, and no
// command the native line), and it names the container keploy has to find --
// a docker run line in its own --name, a compose line with --container-name,
// without which keploy cannot tell which of the project's containers is the
// application.
func TestRecordExampleRecordsTheKindOfAppItIsShownFor(t *testing.T) {
	for kind, want := range map[utils.CmdType]utils.CmdType{
		utils.Native:        utils.Native,
		utils.Empty:         utils.Native,
		utils.DockerRun:     utils.DockerRun,
		utils.DockerStart:   utils.DockerRun,
		utils.DockerCompose: utils.DockerCompose,
	} {
		line := provider.RecordExample(kind)
		cmd, err := parseExample(line)
		if err != nil {
			t.Fatalf("RecordExample(%q): %v", kind, err)
		}
		if cmd.Name() != "record" {
			t.Fatalf("RecordExample(%q) = %q runs `%s`, not record", kind, line, cmd.CommandPath())
		}
		app, _ := cmd.Flags().GetString("command")
		if got := utils.FindDockerCmd(app); got != want {
			t.Errorf("RecordExample(%q) = %q: its -c is a %q command, want %q", kind, line, got, want)
		}
		switch want {
		case utils.DockerRun:
			if utils.ContainerNameFromDockerRun(app) == "" {
				t.Errorf("RecordExample(%q) = %q: its docker run names no container (--name)", kind, line)
			}
		case utils.DockerCompose:
			if !cmd.Flags().Changed("container-name") {
				t.Errorf("RecordExample(%q) = %q: no --container-name", kind, line)
			}
		}
	}
}

// A hint printed for both `keploy record` and `keploy test` names the command
// the user ran: the buildDelay and missing --container-name hints, printed for
// either, both showed a `keploy record` line, so a user whose `keploy test`
// stopped there was told to record instead; and the missing -c hint showed a
// `keploy test` line to a user of record.
func TestAHintPrintedForRecordAndTestNamesTheCommandItIsFor(t *testing.T) {
	for _, command := range []string{"record", "test"} {
		for name, line := range map[string]string{
			"NativeExample":     provider.NativeExample(command),
			"DockerExample":     provider.DockerExample(command),
			"ComposeExample":    provider.ComposeExample(command),
			"BuildDelayExample": provider.BuildDelayExample(command),
		} {
			cmd, err := parseExample(line)
			if err != nil {
				t.Fatalf("%s(%q): %v", name, command, err)
			}
			if cmd.Name() != command {
				t.Errorf("%s(%q) = %q runs `%s`", name, command, line, cmd.CommandPath())
			}
		}
	}
}

// parseExample, the check above, refuses what keploy refuses and what keploy
// silently drops, and takes what keploy takes: a test that says every example
// is accepted is only as good as the parse it accepts them with.
func TestParseExampleRefusesALineKeployWouldNotRun(t *testing.T) {
	for _, line := range []string{
		`keploy record -c "./myApp serve" --delay 6`, // a flag record does not have
		`keploy test --delay six`,                    // a value of the wrong type
		`keploy record -c "./myApp serve`,            // a quote that never closes
		`keploy mock recrod`,                         // a group's unknown verb
		`keploy diff a b c`,                          // too many arguments
		`keploy normalize --tests ts1:tc1 tc2`,       // a value that needed quotes
		`keploy bogus`,                               // no such command
		`keploy record -c "docker compose up"`,       // no --container-name
	} {
		if _, err := parseExample(line); err == nil {
			t.Errorf("parseExample accepted %q", line)
		}
	}
	for _, line := range []string{
		`keploy config defaults | grep -i port`,
		`keploy config defaults > keploy.defaults.yml`,
		`keploy config defaults 2>&1 | less`,
		`keploy diff test-run-1 test-run-2 -t "test-set-1,test-set-2"`,
		`keploy normalize --tests "ts1:tc1 tc2"`,
		`keploy record -c "docker run --name <containerName> <image>" --container-name "<containerName>"`,
		`keploy record --help`,
		`keploy -h`,
	} {
		if _, err := parseExample(line); err != nil {
			t.Errorf("parseExample refused %q: %v", line, err)
		}
	}
	text := "Record it:\n  keploy record -c ./app \\\n    --container-name api\n  keploy test --delay 6"
	if got, want := commandLines(text), []string{"keploy record -c ./app --container-name api", "keploy test --delay 6"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("commandLines(%q) = %q, want %q", text, got, want)
	}
	// A line run through sudo, env or an assignment is still keploy's.
	for line, want := range map[string]string{
		`sudo keploy ca clean --fresh`:                         `keploy ca clean --fresh`,
		`sudo -E env PATH="$PATH" keploy record -c ./app`:      `keploy record -c ./app`,
		`KEPLOY_PROVEN=1 keploy test -c ./app`:                 `keploy test -c ./app`,
		`  keploy config defaults    # print every setting`:    `keploy config defaults    # print every setting`,
		`$ keploy test -c ./app --delay 6`:                     `keploy test -c ./app --delay 6`,
		`keploy records the calls your application makes`:      `keploy records the calls your application makes`,
		`- keploy records the calls`:                           "",
		`Run keploy record to record`:                          "",
		`curl -O -L https://keploy.io/install.sh && keploy -h`: "",
	} {
		got, ok := keployCommandLine(line)
		if want == "" {
			if ok {
				t.Errorf("keployCommandLine(%q) = %q, want no keploy command line", line, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("keployCommandLine(%q) = %q, %v; want %q", line, got, ok, want)
		}
	}
}

// commandLines is the keploy command lines in text, printed text: a line
// continued with a trailing backslash is one.
func commandLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(continuation.ReplaceAllString(text, " "), "\n") {
		if line, ok := keployCommandLine(line); ok {
			lines = append(lines, line)
		}
	}
	return lines
}

// keployCommandLine is the keploy command line in line, a line of printed
// text, if it runs one: `keploy ...` itself, after a `$ ` prompt, or with what
// a shell runs it through in front (`sudo -E env PATH="$PATH" keploy ...`,
// `KEPLOY_PROVEN=1 keploy ...`). Only flags without a value are taken for
// sudo's or env's (`-E`): `sudo -u root keploy ...` is not recognised, and is
// not checked. Neither is a keploy command after another one on its line
// (`cd app && keploy record ...`).
func keployCommandLine(line string) (string, bool) {
	line = strings.TrimPrefix(strings.TrimSpace(line), "$ ")
	for wrapped := false; ; {
		rest := line
		switch {
		case strings.HasPrefix(line, "sudo "), strings.HasPrefix(line, "env "):
			rest, wrapped = line[strings.IndexByte(line, ' ')+1:], true
		case wrapped && strings.HasPrefix(line, "-") && strings.Contains(line, " "):
			rest = line[strings.IndexByte(line, ' ')+1:]
		default:
			if m := envAssignment.FindString(line); m != "" {
				rest = line[len(m):]
			}
		}
		rest = strings.TrimSpace(rest)
		if rest == line {
			break
		}
		line = rest
	}
	return line, strings.HasPrefix(line, "keploy ")
}

// continuation is a backslash that continues a command line onto the next.
var continuation = regexp.MustCompile(`[ \t]*\\\n[ \t]*`)

// envAssignment is a NAME=value word at the start of a command line.
var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=("[^"]*"|'[^']*'|\S*)\s+`)

// shellOperator reports whether w, a word of a command line split as a shell
// splits it, ends the command keploy runs: a pipe, a list operator, `&`, or a
// redirection (`>`, `2>&1`). Only as a word of its own, the way keploy's
// examples write them: one run into the word next to it (`>out.yml`, `app;`)
// is taken as an argument, and the line is refused, not passed. Not a word
// that only starts with `<`: the split has dropped the quotes of a
// "<containerName>" placeholder.
func shellOperator(w string) bool {
	switch w {
	case "|", "||", "&&", ";", "&", "|&":
		return true
	}
	return redirection.MatchString(w)
}

// redirection is a redirection operator standing as a word of its own.
var redirection = regexp.MustCompile(`^[0-9]*(<|<<|>|>>)(&[0-9]+)?$`)

// newRealRoot is the command tree the keploy binary runs, flags and all.
func newRealRoot() *cobra.Command {
	return Root(context.Background(), zap.NewNop(), nil, provider.NewCmdConfigurator(zap.NewNop(), config.New()))
}

// parseExample splits line as a shell would, up to its first shell operator,
// and parses it as the command it names would, flags and arguments, in a tree
// of its own (parsing marks flags Changed).
func parseExample(line string) (*cobra.Command, error) {
	words, err := shlex.Split(line)
	if err != nil {
		return nil, fmt.Errorf("%q does not split as a shell command line: %w", line, err)
	}
	for i, w := range words {
		if shellOperator(w) {
			words = words[:i]
			break
		}
	}
	root := newRealRoot()
	cmd, rest, err := root.Find(words[1:])
	if err != nil {
		return nil, fmt.Errorf("%q names no keploy command: %w", line, err)
	}
	// --help: pflag answers ErrHelp for it, which cobra turns into the help.
	if err := cmd.ParseFlags(rest); errors.Is(err, pflag.ErrHelp) {
		return cmd, nil
	} else if err != nil {
		return nil, fmt.Errorf("%q: `%s` rejects it: %w", line, cmd.CommandPath(), err)
	}
	// What cobra checks once the flags parse: the arguments. A command that
	// declares none takes any and reads none, so a word left over there is
	// one keploy drops or refuses: a group's unknown verb (a hardened group
	// refuses it, cli.HardenUnknownSubcommands), a flag value that needed
	// quotes (`--tests a:b c`), or prose.
	args := cmd.Flags().Args()
	if cmd.Args == nil && len(args) > 0 {
		return nil, fmt.Errorf("%q: `%s` takes no arguments and ignores %q", line, cmd.CommandPath(), args)
	}
	if err := cmd.ValidateArgs(args); err != nil {
		return nil, fmt.Errorf("%q: `%s` rejects its arguments: %w", line, cmd.CommandPath(), err)
	}
	// A compose line that does not say which of the project's containers is
	// the application is refused by record and test alike (`missing required
	// --container-name`, or `container name not found` once compose is up).
	if cmd.Name() == "record" || cmd.Name() == "test" {
		if app, _ := cmd.Flags().GetString("command"); utils.FindDockerCmd(app) == utils.DockerCompose && !cmd.Flags().Changed("container-name") {
			return nil, fmt.Errorf("%q runs docker compose and passes no --container-name", line)
		}
	}
	return cmd, nil
}
