//go:build linux || darwin

package http

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap/zapcore"
)

// plantedAgentValue is what agentCutter cuts; never a real secret.
const plantedAgentValue = "planted-agent-s3cr3t"

type agentCutter struct{}

func (agentCutter) RedactEntry(*zapcore.Entry)       {}
func (agentCutter) RedactField(*zapcore.Field)       {}
func (agentCutter) RedactEncoded(text string) string { return text }
func (agentCutter) CutsChildOutput() bool            { return true }
func (agentCutter) HoldChildOutput(string) int       { return 0 }
func (agentCutter) RedactChildOutput(line string) string {
	return strings.ReplaceAll(line, plantedAgentValue, "***")
}

// TestStartWithConsoleCutsTheAgentsLines: the native agent and the docker-run
// agent both start through startWithConsole, so what they print reaches
// keploy's console cut, its last unended line too.
func TestStartWithConsoleCutsTheAgentsLines(t *testing.T) {
	keployLog.SetRedactor(agentCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			target := &os.Stdout
			redirect := ""
			if stream == "stderr" {
				target, redirect = &os.Stderr, " >&2"
			}
			old := *target
			*target = w
			defer func() { *target = old }()
			cmd := exec.Command("sh", "-c", `printf 'agent %s\nlast %s' '`+plantedAgentValue+`' '`+plantedAgentValue+`'`+redirect)
			console, err := startWithConsole(cmd, (*exec.Cmd).Start)
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			console.Exited()
			*target = old
			_ = w.Close()
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if want := "agent ***\nlast ***"; string(got) != want {
				t.Fatalf("the agent's %s reached keploy's console as %q, want %q", stream, got, want)
			}
		})
	}
}

func TestStartWithConsoleReportsAFailedStart(t *testing.T) {
	keployLog.SetRedactor(agentCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	cmd := exec.Command("/nonexistent/keploy-agent")
	if console, err := startWithConsole(cmd, (*exec.Cmd).Start); err == nil || console != nil {
		t.Fatalf("startWithConsole = %v, %v; want the start's error", console, err)
	}
}
