package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// passed=false means the flag was never given on the command line. Kept
// separate from the value so "--cmd-type=" (explicitly empty) is
// expressible — a real invocation that a value-only helper cannot describe.
func cmdWithCmdTypeFlag(explicit string, passed bool) *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("cmd-type", "", "")
	if passed {
		_ = cmd.Flags().Set("cmd-type", explicit)
	}
	return cmd
}

// TestResolveCommandType_ExplicitValid asserts that an explicitly-provided,
// valid --cmd-type wins over whatever auto-detection would have said for the
// given command string.
func TestResolveCommandType_ExplicitValid(t *testing.T) {
	cmd := cmdWithCmdTypeFlag("docker-compose", true)

	got, err := resolveCommandType(zap.NewNop(), cmd, "sudo -E docker compose up", "docker-compose")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "docker-compose" {
		t.Errorf("got %q, want %q", got, "docker-compose")
	}
}

// TestResolveCommandType_ExplicitInvalid asserts that an explicit but
// unrecognized --cmd-type value (including the old, never-valid "docker")
// is rejected with a clear error rather than silently falling back to
// auto-detection.
func TestResolveCommandType_ExplicitInvalid(t *testing.T) {
	cmd := cmdWithCmdTypeFlag("docker", true)

	_, err := resolveCommandType(zap.NewNop(), cmd, "python app.py", "docker")
	if err == nil {
		t.Fatal("expected an error for an invalid --cmd-type value, got nil")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("error %q does not mention the invalid value", err.Error())
	}
}

// TestResolveCommandType_NotProvided_FallsBackToAutoDetect is the regression
// guard: when the user never passes --cmd-type, behavior must be identical
// to today's auto-detection.
func TestResolveCommandType_NotProvided_FallsBackToAutoDetect(t *testing.T) {
	cmd := cmdWithCmdTypeFlag("", false)

	got, err := resolveCommandType(zap.NewNop(), cmd, "python app.py", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "native" {
		t.Errorf("got %q, want %q (auto-detected from a plain command string)", got, "native")
	}
}

// TestResolveCommandType_ConfigDefaultDoesNotCountAsExplicit is the
// regression this function's Changed() gating exists for, and the one the
// obvious implementation gets wrong. config/default.go ships
// cmdType: "native" in every generated keploy.yml, so it arrives here as a
// non-empty "configured" value on runs where the user never touched the
// flag. Treating that as a deliberate choice would pin every such user to
// native and silently break docker auto-detection for them.
func TestResolveCommandType_ConfigDefaultDoesNotCountAsExplicit(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{"docker compose up", "docker-compose"},
		{"docker run --name app img", "docker-run"},
		{"docker start app", "docker-start"},
		{"python app.py", "native"},
	}
	for _, tc := range cases {
		// Flag never passed; "native" comes from the generated keploy.yml.
		cmd := cmdWithCmdTypeFlag("", false)
		got, err := resolveCommandType(zap.NewNop(), cmd, tc.command, "native")
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", tc.command, err)
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q — the shipped keploy.yml default is being "+
				"treated as an explicit user choice", tc.command, got, tc.want)
		}
	}
}

// A value that only ever came from keploy.yml is not honoured — the
// privilege decision in main.go happens before any config is read, so a
// config-supplied docker type would pick a mode without the root it needs.
// It must warn rather than fail or silently comply, since the silence is
// what let this flag stay inert for so long.
func TestResolveCommandType_ConfigDockerValueWarnsAndIsNotHonoured(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)

	cmd := cmdWithCmdTypeFlag("", false)
	got, err := resolveCommandType(logger, cmd, "make up", "docker-compose")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "native" {
		t.Errorf("got %q, want auto-detected %q", got, "native")
	}
	if logs.FilterMessageSnippet("not honoured").Len() == 0 {
		t.Error("a docker cmdType in config was ignored without warning — the user has no " +
			"way to discover the flag is command-line only")
	}
}

// The shipped keploy.yml default must not warn; it is not a user choice.
func TestResolveCommandType_ConfigNativeDefaultIsSilent(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	cmd := cmdWithCmdTypeFlag("", false)
	if _, err := resolveCommandType(zap.New(core), cmd, "docker compose up", "native"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("the generated keploy.yml default warned: %v", logs.All())
	}
}

// `--cmd-type=""` was silently overwritten by auto-detection before #4399,
// so that is what it must keep doing. Rejecting it breaks scripts that pass
// it; preserving the empty value is worse — GetCommonServices reads "" as
// "not docker" and leaves the docker client nil while resolveKind reads it
// as "detect", so `--cmd-type= -c "docker compose up"` reaches SetupCompose
// with a nil client and panics.
func TestResolveCommandType_ExplicitEmptyFallsBackToAutoDetect(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"python app.py", "native"},
		{"docker compose up", "docker-compose"},
	} {
		cmd := cmdWithCmdTypeFlag("", true)
		got, err := resolveCommandType(zap.NewNop(), cmd, tc.command, "")
		if err != nil {
			t.Fatalf("--cmd-type=\"\" must not be rejected, got: %v", err)
		}
		if got != tc.want {
			t.Errorf("command %q: got %q, want auto-detected %q", tc.command, got, tc.want)
		}
	}
}

// FindDockerCmd lowercases what it matches against, so the flag should not
// be stricter than the auto-detection it overrides.
func TestResolveCommandType_ExplicitIsNormalised(t *testing.T) {
	for _, in := range []string{"Docker-Compose", "  docker-compose  ", "DOCKER-COMPOSE"} {
		cmd := cmdWithCmdTypeFlag(in, true)
		got, err := resolveCommandType(zap.NewNop(), cmd, "sudo -E docker compose up", in)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", in, err)
		}
		if got != "docker-compose" {
			t.Errorf("%q: got %q, want %q", in, got, "docker-compose")
		}
	}
}

// The help text listed (native/docker/docker-compose): "docker" was never a
// valid CmdType and docker-run/docker-start were missing entirely, so the
// flag documented values it would now reject.
func TestCmdTypeFlagHelpListsTheRealValues(t *testing.T) {
	cfg := config.New()
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	cmd := &cobra.Command{Use: "record"}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	flag := cmd.Flags().Lookup("cmd-type")
	if flag == nil {
		t.Fatal("cmd-type flag is not registered")
	}
	for _, want := range []utils.CmdType{utils.Native, utils.DockerRun, utils.DockerStart, utils.DockerCompose} {
		if !strings.Contains(flag.Usage, string(want)) {
			t.Errorf("help text %q does not mention the valid value %q", flag.Usage, want)
		}
	}
}

// End-to-end through ValidateFlags, which is where the value was being
// discarded — resolveCommandType returning the right answer is only useful
// if the caller keeps it.
func TestValidateFlags_HonoursCmdTypeEndToEnd(t *testing.T) {
	cases := []struct {
		name       string
		command    string
		flagValue  string // "" = not passed on the CLI
		configured string
		want       string
	}{
		{"explicit compose over an unprefixed docker command", "sudo -E docker compose up", "docker-compose", "docker-compose", "docker-compose"},
		{"config compose is not honoured", "sudo -E docker compose up", "", "docker-compose", "native"},
		{"generated default + docker command still auto-detects", "docker compose up", "", "native", "docker-compose"},
		{"generated default + plain command stays native", "python app.py", "", "native", "native"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.New()
			c := NewCmdConfigurator(zap.NewNop(), cfg)
			cmd := &cobra.Command{Use: "record"}
			if err := c.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags: %v", err)
			}
			args := []string{"-c", tc.command}
			if tc.flagValue != "" {
				args = append(args, "--cmd-type", tc.flagValue)
			}
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			// Mirror what viper.Unmarshal leaves behind before ValidateFlags.
			cfg.Command = tc.command
			cfg.CommandType = tc.configured

			if err := c.ValidateFlags(context.Background(), cmd); err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			if cfg.CommandType != tc.want {
				t.Errorf("cfg.CommandType = %q, want %q", cfg.CommandType, tc.want)
			}
		})
	}
}

// A docker cmdType only works if keploy can rewrite the command: SetupCompose
// splices `-f <generated>.yaml` in ahead of " up", SetupDocker splices
// `--pid=container:…` into a `docker run`. Point one at a wrapper and you get
// `make -f ./docker-compose-tmp.yaml up --abort-on-container-exit`, which
// fails with an unrecognised option nowhere near its cause. Refusing up front
// is the difference between a usable flag and one that looks like it worked.
func TestResolveCommandType_DockerRunRejectsWrapperCommand(t *testing.T) {
	for _, cmdType := range []string{"docker-run", "docker-start"} {
		for _, command := range []string{"make up", "./start.sh", "npm start"} {
			cmd := cmdWithCmdTypeFlag(cmdType, true)
			_, err := resolveCommandType(zap.NewNop(), cmd, command, cmdType)
			if err == nil {
				t.Errorf("%s with command %q: want an error, got none", cmdType, command)
				continue
			}
			if !strings.Contains(err.Error(), command) {
				t.Errorf("error for %q does not name the command: %v", command, err)
			}
		}
	}
}

// docker-compose does have a way through — keploy hands its compose file over
// via COMPOSE_FILE and runs the command untouched — so a wrapper is accepted
// there. This is the case issue #4399 was opened for.
func TestResolveCommandType_ComposeAcceptsWrapperCommand(t *testing.T) {
	for _, command := range []string{"make up", "./start.sh", "npm start"} {
		cmd := cmdWithCmdTypeFlag("docker-compose", true)
		got, err := resolveCommandType(zap.NewNop(), cmd, command, "docker-compose")
		if err != nil {
			t.Errorf("command %q with --cmd-type docker-compose: unexpected error: %v", command, err)
			continue
		}
		if got != "docker-compose" {
			t.Errorf("command %q: got %q, want %q", command, got, "docker-compose")
		}
	}
}

// The commands the flag exists for: real docker invocations that
// FindDockerCmd's prefix matching does not recognise.
func TestResolveCommandType_DockerTypeAcceptsUnprefixedDockerCommands(t *testing.T) {
	cases := []struct{ command, cmdType string }{
		{"sudo -E docker compose up", "docker-compose"},
		{"env FOO=bar docker compose up", "docker-compose"},
		{"/usr/bin/docker compose up", "docker-compose"},
		{"sudo -E docker run --name app img", "docker-run"},
	}
	for _, tc := range cases {
		// Auto-detection must genuinely miss these, or the test proves nothing.
		if got := utils.FindDockerCmd(tc.command); got != utils.Native {
			t.Fatalf("premise broken: FindDockerCmd(%q) = %q, expected it to miss", tc.command, got)
		}
		cmd := cmdWithCmdTypeFlag(tc.cmdType, true)
		got, err := resolveCommandType(zap.NewNop(), cmd, tc.command, tc.cmdType)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.command, err)
			continue
		}
		if got != tc.cmdType {
			t.Errorf("%q: got %q, want %q", tc.command, got, tc.cmdType)
		}
	}
}

// native against a wrapper is unaffected — nothing gets rewritten.
func TestResolveCommandType_NativeAcceptsAnyCommand(t *testing.T) {
	cmd := cmdWithCmdTypeFlag("native", true)
	got, err := resolveCommandType(zap.NewNop(), cmd, "make up", "native")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "native" {
		t.Errorf("got %q, want %q", got, "native")
	}
}

// Substring matching let "./run-docker.sh" past the docker-run guard — a
// wrapper by any reading, and the naming convention a docker wrapper script
// actually uses. Past the guard it fails much worse: ParseDockerCmd wipes the
// user's --container-name and modifyDockerRun dies on the token count.
func TestMentionsDockerBinary(t *testing.T) {
	yes := []string{
		"docker compose up",
		"sudo -E docker run --name app img",
		"/usr/bin/docker compose up",
		"env FOO=bar docker-compose up",
	}
	for _, cmd := range yes {
		if !mentionsDockerBinary(cmd) {
			t.Errorf("%q: want recognised as a docker invocation", cmd)
		}
	}

	no := []string{
		"./run-docker.sh",
		"make docker-up",
		"npm run docker",
		"podman run --name app img",
		"make up",
	}
	for _, cmd := range no {
		if mentionsDockerBinary(cmd) {
			t.Errorf("%q: want NOT recognised — it does not invoke docker itself", cmd)
		}
	}
}

// The config-file warning must not fire when the config value agrees with
// auto-detection: nothing is being ignored in any way the user can observe,
// and warning on every such run is pure noise.
func TestResolveCommandType_ConfigWarningOnlyWhenItWouldDiffer(t *testing.T) {
	// Agrees with detection -> silent.
	core, logs := observer.New(zapcore.WarnLevel)
	cmd := cmdWithCmdTypeFlag("", false)
	if _, err := resolveCommandType(zap.New(core), cmd, "docker compose up", "docker-compose"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("warned even though the config value matches what was detected: %v", logs.All())
	}

	// Disagrees -> warns.
	core2, logs2 := observer.New(zapcore.WarnLevel)
	cmd2 := cmdWithCmdTypeFlag("", false)
	if _, err := resolveCommandType(zap.New(core2), cmd2, "make up", "docker-compose"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logs2.FilterMessageSnippet("not honoured").Len() == 0 {
		t.Error("config value was silently ignored where it would have changed the outcome")
	}
}
