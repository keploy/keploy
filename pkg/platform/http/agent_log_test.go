package http

import (
	"bytes"
	"os"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func TestAgentOutKeepsEverythingAndRelaysOnlyFacts(t *testing.T) {
	var file, term bytes.Buffer
	w := &agentOut{file: &file, term: &term}
	for _, chunk := range []string{"🐰 Keploy(agent): INFO starting DNS\nkeploy: shifted the clo", "ck to 2026-10-06\npanic: boom\n", "keploy: tail without newline"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got, want := term.String(), "keploy: shifted the clock to 2026-10-06\n"; got != want {
		t.Fatalf("terminal = %q, want %q", got, want)
	}
	if got, want := file.String(), "🐰 Keploy(agent): INFO starting DNS\nkeploy: shifted the clock to 2026-10-06\npanic: boom\nkeploy: tail without newline"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestAgentWritesToTheConsoleOutsideMockRuns(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	for _, a := range []*AgentClient{
		{logger: zap.NewNop(), conf: &config.Config{}},
		{logger: zap.NewNop(), conf: &config.Config{Debug: true}, mock: true},
	} {
		out, errOut := a.agentOutput()
		if out != os.Stdout || errOut != os.Stderr || a.agentLog != "" {
			t.Fatalf("mock=%v debug=%v: agent output moved off the console", a.mock, a.conf.Debug)
		}
	}
	if _, err := os.Stat(dir + "/keploy-agent.log"); err == nil {
		t.Fatal("a keploy-agent.log was created outside a quiet mock run")
	}
	a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{}, mock: true}
	if out, _ := a.agentOutput(); out == os.Stdout || a.agentLog == "" {
		t.Fatal("a mock run's agent still writes to the console")
	}
}
