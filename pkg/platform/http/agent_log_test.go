package http

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAgentTailRelaysFactsAndCountsWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var term syncBuf
	tail := tailAgentLog(path, &term, time.Millisecond)
	long := "keploy: " + strings.Repeat("x", maxFactLine+10) + "\n"
	for _, chunk := range []string{
		"🐰 Keploy(agent): 2026\tINFO\tstarting DNS\nkeploy: shifted the clo",
		"ck to 2026-10-06\n🐰 Keploy(agent): 2026\t\x1b[33mWARN\x1b[0m\tsomething\n",
		long,
		"panic: boom\nkeploy: tail without newline",
	} {
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if warns := tail.end(); warns != 2 {
		t.Fatalf("warnings = %d, want 2", warns)
	}
	want := "keploy: shifted the clock to 2026-10-06\nkeploy: " + strings.Repeat("x", maxFactLine-len("keploy: ")) + "...\nkeploy: tail without newline\n"
	if got := term.String(); got != want {
		t.Fatalf("terminal = %q", got)
	}
}

func agentWithLog(t *testing.T) *AgentClient {
	t.Helper()
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{}, mock: true}
	if out, _ := a.agentOutput(); out == os.Stdout || a.agentLog == "" {
		t.Fatal("a mock run's agent still writes to the console")
	}
	t.Cleanup(func() { _ = a.agentFile.Close() })
	return a
}

func TestTheAgentLogStaysUnlessACleanStopLeftNoWarnings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stopped bool
		warn    bool
		failed  bool
		kept    bool
	}{
		{name: "asked to stop, nothing to see", stopped: true},
		{name: "exited without being asked", kept: true, failed: false},
		{name: "it warned", stopped: true, warn: true, kept: true},
		{name: "a failure was reported", stopped: true, failed: true, kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := agentWithLog(t)
			if tc.warn {
				_, _ = a.agentFile.WriteString("x\tERROR\ty\n")
			}
			if tc.failed {
				a.sayAgentLog()
			}
			a.endAgentLog(tc.stopped)
			_, err := os.Stat(a.agentLog)
			if kept := err == nil; kept != tc.kept {
				t.Fatalf("kept = %v, want %v", kept, tc.kept)
			}
			a.sayAgentLog()
			if !tc.kept && a.failed {
				t.Fatal("a removed log was named")
			}
		})
	}
}

func TestAgentWritesToTheConsoleOutsideMockRuns(t *testing.T) {
	for _, a := range []*AgentClient{
		{logger: zap.NewNop(), conf: &config.Config{}},
		{logger: zap.NewNop(), conf: &config.Config{Debug: true}, mock: true},
	} {
		out, errOut := a.agentOutput()
		if out != os.Stdout || errOut != os.Stderr || a.agentLog != "" {
			t.Fatalf("mock=%v debug=%v: agent output moved off the console", a.mock, a.conf.Debug)
		}
	}
}

func TestTwoRunsInOneFolderKeepTheirOwnAgentLogs(t *testing.T) {
	a := agentWithLog(t)
	if !strings.HasPrefix(filepath.Base(a.agentLog), "keploy-agent-"+strconv.Itoa(os.Getpid())+"-") {
		t.Fatalf("agent log %s is not named for this run", a.agentLog)
	}
	other := filepath.Join(filepath.Dir(a.agentLog), "keploy-agent-1.log")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.endAgentLog(true)
	if _, err := os.Stat(other); err != nil {
		t.Fatal("another run's agent log was removed")
	}
}

func TestWithoutATerminalASudoThatWantsAPasswordNamesTheUsersCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prevPTY, prevArgs := needsPTY, os.Args
	t.Cleanup(func() { needsPTY, os.Args = prevPTY, prevArgs })
	needsPTY = func() bool { return false }
	os.Args = []string{"keploy", "mock", "record", "-c", "go test ./..."}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a := &AgentClient{mock: true, conf: &config.Config{}}
	_, _, err = a.sudoRoute(true, false)
	want := "run: sudo -E env " + utils.QuoteArgs([]string{"PATH=" + os.Getenv("PATH"), exe}) + " mock record -c 'go test ./...'"
	if err == nil || !strings.HasSuffix(err.Error(), want) || strings.Contains(err.Error(), "agent --") {
		t.Fatalf("err = %v\nwant suffix %s", err, want)
	}
	if pty, cached, err := (&AgentClient{conf: &config.Config{}}).sudoRoute(true, false); pty || cached || err != nil {
		t.Fatalf("outside a mock run: %v %v %v", pty, cached, err)
	}
}

func TestASudoThatCanPromptTakesTheTerminalPath(t *testing.T) {
	prev := needsPTY
	t.Cleanup(func() { needsPTY = prev })
	needsPTY = func() bool { return true }
	for _, mock := range []bool{true, false} {
		pty, cached, err := (&AgentClient{mock: mock, conf: &config.Config{}}).sudoRoute(true, false)
		if !pty || cached || err != nil {
			t.Fatalf("mock=%v: %v %v %v", mock, pty, cached, err)
		}
	}
	if pty, cached, _ := (&AgentClient{mock: true, conf: &config.Config{}}).sudoRoute(true, true); pty || !cached {
		t.Fatal("cached credentials need no terminal")
	}
}

func TestAFactLineWrittenInPiecesIsStillCapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var term syncBuf
	tail := tailAgentLog(path, &term, time.Millisecond)
	_, _ = f.WriteString("keploy: ")
	for range 4 {
		_, _ = f.WriteString(strings.Repeat("y", maxFactLine/2))
		time.Sleep(3 * time.Millisecond)
	}
	_, _ = f.WriteString("\n" + "x\tERROR\t" + strings.Repeat("z", maxFactLine+5) + "\nkeploy: next\n")
	time.Sleep(5 * time.Millisecond)
	warns := tail.end()
	got := term.String()
	if len(got) > maxFactLine+20 || !strings.HasSuffix(got, "...\nkeploy: next\n") {
		t.Fatalf("terminal got %d bytes ending %q", len(got), got[max(0, len(got)-30):])
	}
	if warns != 1 {
		t.Fatalf("warnings = %d, want 1", warns)
	}
}
