package log

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Under a run log, keploy's info lines go to the run's file alone, and the
// console keeps its warnings and errors (the file has them too); undoing it
// puts the console back.
func TestRouteToRunLogKeepsInfoOffTheConsole(t *testing.T) {
	LogCfg = defaultLogCfg()
	var console bytes.Buffer
	logger := zap.New(newRedactingCore(consoleCore(LogCfg, zapcore.AddSync(&console), LogCfg.Level)))
	f, err := os.Create(filepath.Join(t.TempDir(), "keploy.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	routed, undo := RouteToRunLog(logger, f, true)
	routed.Info("Proxy started at port:16789")
	routed.Warn("no recorded mock matched an outgoing call")
	undo()
	logger.Info("after the run")

	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	file, con := string(raw), console.String()
	if !strings.Contains(file, "INFO\tProxy started at port:16789") || !strings.Contains(file, "WARN\tno recorded mock matched an outgoing call") {
		t.Fatalf("the run log holds %q", file)
	}
	if strings.Contains(file, "\x1b[") {
		t.Fatalf("the run log holds colour codes: %q", file)
	}
	if strings.Contains(con, "Proxy started") || !strings.Contains(con, "no recorded mock matched an outgoing call") {
		t.Fatalf("the console holds %q; want its warning, not its info line", con)
	}
	if !strings.Contains(con, "after the run") {
		t.Fatalf("undo did not put the console back: %q", con)
	}
	if RunLogWriter(&console) != &console {
		t.Fatal("with no run log, an agent's output goes to the console as it is")
	}
}

// An agent's output, under a run log: its info and debug lines go to the
// run log without colour, its warnings and errors to the console, and a line
// with no level where the line before it went.
func TestRunLogWriterSplitsAnAgentsOutput(t *testing.T) {
	LogCfg = defaultLogCfg()
	f, err := os.Create(filepath.Join(t.TempDir(), "keploy.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, undo := RouteToRunLog(zap.NewNop(), f, true)
	defer undo()
	var console bytes.Buffer
	w := RunLogWriter(&console)
	out := "🐰 Keploy(agent): 2026-10-27T10:30:33Z\t\x1b[34mINFO\x1b[0m\tProxy started at port:16789\n" +
		"🐰 Keploy(agent): 2026-10-27T10:30:33Z\t\x1b[33mWARN\x1b[0m\tthe app's DNS is keploy's\n" +
		"  a second line of that warning\n" +
		"🐰 Keploy(agent): 2026-10-27T10:30:34Z\tDEBUG\tstored mocks\n"
	// In two writes, cut inside a line.
	if _, err := w.Write([]byte(out[:40])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(out[40:])); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f.Name())
	if string(raw) != "🐰 Keploy(agent): 2026-10-27T10:30:33Z\tINFO\tProxy started at port:16789\n🐰 Keploy(agent): 2026-10-27T10:30:34Z\tDEBUG\tstored mocks\n" {
		t.Fatalf("the run log holds %q", raw)
	}
	if console.String() != "🐰 Keploy(agent): 2026-10-27T10:30:33Z\t\x1b[33mWARN\x1b[0m\tthe app's DNS is keploy's\n  a second line of that warning\n" {
		t.Fatalf("the console holds %q", console.String())
	}
}

// Not quiet (keploy's --debug): the run log has keploy's lines, and the
// console keeps them all too.
func TestARunLogThatIsNotQuietKeepsTheConsole(t *testing.T) {
	LogCfg = defaultLogCfg()
	var console bytes.Buffer
	logger := zap.New(newRedactingCore(consoleCore(LogCfg, zapcore.AddSync(&console), LogCfg.Level)))
	f, err := os.Create(filepath.Join(t.TempDir(), "keploy.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	routed, undo := RouteToRunLog(logger, f, false)
	routed.Info("Proxy started at port:16789")
	undo()
	raw, _ := os.ReadFile(f.Name())
	if !strings.Contains(string(raw), "Proxy started") || !strings.Contains(console.String(), "Proxy started") {
		t.Fatalf("file %q, console %q; want the line in both", raw, console.String())
	}
	if RunLogWriter(&console) != &console {
		t.Fatal("an agent's output is split only under a quiet run log")
	}
}
