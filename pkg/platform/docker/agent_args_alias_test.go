package docker

import (
	"bytes"
	"context"
	"go/ast"
	"go/token"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/keploy/shlex"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// agentArgCases are arguments a caller hands the agent (SetupOptions.
// ExtraArgs: the enterprise startup hook's, whose --custom-dedup-fields is
// JSON) that a shell would change: each must reach the agent as it was given.
var agentArgCases = []string{
	`[{"method":"GET","path":"/api/v1/users","statusCode":200,"fields":["requestType","custId"]}]`,
	"/var/tmp/a dir/kclk1/o",
	`it's "quoted"`,
	"$HOME and ${PATH}",
	"`id`",
	"$(touch /tmp/keploy-alias-pwned)",
	"a;b|c&d>e<f",
	`back\slash`,
	"tab\there",
	"%PATH%",
	"line\nbreak",
	"",
}

// The linux docker-run alias goes through `sh -c` (utils.CommandContext), or
// its shlex fallback on a host with no /bin/sh. Every argument the agent is
// handed, and its config path, must come out of both as the one argument it
// went in as: a JSON argument used to lose its quotes to the shell, so the
// agent could not read it and refused to start, and a path with a space
// became two arguments.
func TestGetAlias_EveryAgentArgumentReachesTheAgentAsGiven(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	const configPath = "/srv/my app/keploy.yml"
	var extra []string
	for _, a := range agentArgCases {
		extra = append(extra, "--flag", a)
	}
	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		ClientNSPID:     4242,
		Mode:            models.MODE_RECORD,
		ConfigPath:      configPath,
		ExtraArgs:       extra,
	}, &config.Config{})
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}

	check := func(t *testing.T, how string, words []string) {
		t.Helper()
		if len(words) < len(extra) || !slices.Equal(words[len(words)-len(extra):], extra) {
			t.Errorf("%s: the alias's last %d words are not the agent's arguments as given\n got: %q\nwant: %q", how, len(extra), words[max(0, len(words)-len(extra)):], extra)
		}
		if i := slices.Index(words, "--config-path"); i < 0 || i+1 >= len(words) || words[i+1] != configPath {
			t.Errorf("%s: the config path is not one word %q: %q", how, configPath, words)
		}
	}

	words, err := shlex.Split(alias)
	if err != nil {
		t.Fatalf("shlex.Split(alias): %v\nalias: %s", err, alias)
	}
	check(t, "shlex", words)

	// The shell itself: printf hands back, one by one, the words sh makes of
	// the alias, and runs none of them.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh: %v", err)
	}
	out, err := exec.Command(sh, "-c", `printf '%s\0' `+alias).Output()
	if err != nil {
		t.Fatalf("sh -c printf <alias>: %v", err)
	}
	check(t, "sh -c", strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"))
}

// Each argument round-trips through the shell as one word. On windows, where
// the alias is run by cmd.exe with no quoting layer, a value cmd.exe would
// act on (windowsCmdOperators) is refused, and any other goes in as it is.
func TestAliasWord(t *testing.T) {
	for _, a := range agentArgCases {
		got, err := aliasWord("the argument", a)
		if runtime.GOOS != "windows" {
			if err != nil {
				t.Fatalf("aliasWord(%q): %v", a, err)
			}
			if words, err := shlex.Split("x " + got); err != nil || len(words) != 2 || words[1] != a {
				t.Errorf("aliasWord(%q) = %s, which reads back as %q (%v)", a, got, words, err)
			}
			continue
		}
		operator := strings.ContainsAny(a, "&|<>^%\r\n")
		switch {
		case operator && err == nil:
			t.Errorf("aliasWord(%q) = %q on windows, where cmd.exe would act on it", a, got)
		case !operator && (err != nil || got != a):
			t.Errorf("aliasWord(%q) on windows = %q, %v; want it as it is", a, got, err)
		}
	}
}

// Every platform arm of getAlias hands the agent its config path and its
// arguments as the words aliasWord made of them, and none appends either
// from opts itself, as every arm did before: the arms other than linux's
// cannot run here, so this reads them.
func TestGetAlias_EveryPlatformBranchHandsTheAgentItsArgumentsAsWords(t *testing.T) {
	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()
		var configPath, extra bool
		for _, stmt := range list {
			configPath = configPath || isAliasAppendOf(stmt, "configPathFlag")
			extra = extra || isAliasAppendOf(stmt, "extraArgWords")
			ast.Inspect(stmt, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "opts" && (sel.Sel.Name == "ExtraArgs" || sel.Sel.Name == "ConfigPath") {
						t.Errorf("%s: this getAlias platform branch reads opts.%s itself, not as aliasWord's words", pos, sel.Sel.Name)
					}
				}
				return true
			})
		}
		if !configPath {
			t.Errorf("%s: this getAlias platform branch does not append configPathFlag", pos)
		}
		if !extra {
			t.Errorf("%s: this getAlias platform branch does not append extraArgWords", pos)
		}
	})
}

// The compose agent service's command is a list, which no shell reads, but
// compose interpolates every string of the file it loads: a $ in an argument
// would be taken for a variable of the operator's environment. Loaded as
// compose loads it, the command is the agent's arguments as given.
func TestGenerateKeployAgentService_EveryAgentArgumentReachesTheAgentAsGiven(t *testing.T) {
	var extra []string
	for _, a := range agentArgCases {
		extra = append(extra, "--flag", a)
	}
	svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
		AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_TEST,
		ConfigPath: "/srv/$app/keploy.yml",
		ExtraArgs:  extra,
	})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}
	doc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "services"},
		{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "keploy-agent"}, svc}},
	}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	project, err := loader.LoadWithContext(context.Background(), composetypes.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []composetypes.ConfigFile{{Filename: dir + "/compose.yaml", Content: buf.Bytes()}},
		Environment: composetypes.Mapping{"HOME": "/home/operator", "PATH": "/usr/bin", "app": "other"},
	}, func(o *loader.Options) {
		o.SetProjectName("argsalias", true)
		o.SkipValidation = true
		o.SkipConsistencyCheck = true
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		t.Fatalf("compose does not load the generated agent service: %v\n%s", err, buf.String())
	}
	agent, ok := project.Services["keploy-agent"]
	if !ok {
		t.Fatalf("no keploy-agent service in %v", project.ServiceNames())
	}
	got := []string(agent.Command)
	if i := slices.Index(got, "--config-path"); i < 0 || i+1 >= len(got) || got[i+1] != "/srv/$app/keploy.yml" {
		t.Errorf("compose read the config path as %q", got)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if j := slices.Index(got, extra[i+1]); j < 0 {
			t.Errorf("compose read the argument %q as something else; the command it reads: %q", extra[i+1], got)
		}
	}
}
