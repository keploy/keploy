package http

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/utils"
)

// The agent's `docker run` that the engine ends with 125 because the agent's
// published port (127.0.0.1:P) is taken is utils.ErrPortTaken, in Docker's and
// podman's words, and keeps its exit code; another port's refusal, another
// exit code, an end with no exit code, or a port P is only a prefix of is not.
func TestAnAgentPortTheEngineCouldNotPublishIsATakenPort(t *testing.T) {
	// 125 is the code docker run and podman run exit with when the engine
	// itself could not run the container: written out, not engineRunFailed.
	const port, engineCode = 41234, 125
	refusals := []string{
		"docker: Error response from daemon: driver failed programming external connectivity on endpoint keploy-v3-x (abc): Bind for 127.0.0.1:41234 failed: port is already allocated.",
		"docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-v3-x (abc): failed to bind host port for 127.0.0.1:41234:172.17.0.2:41234/tcp: address already in use",
		"docker: Error response from daemon: driver failed programming external connectivity on endpoint keploy-v3-x (abc): Error starting userland proxy: listen tcp4 127.0.0.1:41234: bind: address already in use.",
		"Error: rootlessport listen tcp 127.0.0.1:41234: bind: address already in use",
	}
	a := &AgentClient{logger: zap.NewNop()}
	for _, refusal := range refusals {
		// After the agent's own lines, or the only line printed.
		for _, said := range []string{"agent lines\n" + refusal + "\n", refusal} {
			end := &agentContainerExit{container: "keploy-v3-x", code: engineCode}
			err := publishRefusedBy(end, said, port)
			var ee interface{ ExitCode() int }
			if !errors.Is(err, utils.ErrPortTaken) || !errors.As(err, &ee) || ee.ExitCode() != engineCode || !strings.Contains(err.Error(), "port 41234 is taken") {
				t.Fatalf("publishRefusedBy(%q) = %v; want utils.ErrPortTaken naming port %d, exit code kept", said, err, port)
			}
			if got := a.agentStoppedBeforeReady(err, true); !errors.Is(got, utils.ErrPortTaken) {
				t.Fatalf("agentStoppedBeforeReady = %v; want utils.ErrPortTaken", got)
			}
		}
	}
	for name, c := range map[string]struct {
		code int
		said string
	}{
		"another exit code":           {1, refusals[0]},
		"an app's port":               {engineCode, strings.ReplaceAll(refusals[0], "41234", "8080")},
		"a longer port":               {engineCode, strings.ReplaceAll(refusals[1], "41234", "412345")},
		"no refusal at all":           {engineCode, "docker: Error response from daemon: No such image: keploy"},
		"the port named, not refused": {engineCode, "publishing 127.0.0.1:41234 for the agent\ndocker: Error response from daemon: No such image: keploy"},
		"a refusal of 0.0.0":          {engineCode, strings.ReplaceAll(refusals[0], "127.0.0.1", "0.0.0.0")},
	} {
		end := &agentContainerExit{container: "keploy-v3-x", code: c.code}
		if err := publishRefusedBy(end, c.said, port); errors.Is(err, utils.ErrPortTaken) || err != error(end) {
			t.Fatalf("%s: publishRefusedBy = %v; want the run's end as it is", name, err)
		}
	}
	// An end with no exit code (the run was not waited for to its end) is not
	// the engine's refusal, whatever the output said.
	noCode := errors.New("waiting for docker run: context canceled")
	if err := publishRefusedBy(noCode, refusals[0], port); err != noCode {
		t.Fatalf("publishRefusedBy of an end with no exit code = %v; want it as it is", err)
	}
}

// outputTail keeps the last outputTailSize bytes written to it, from a byte
// past that size on.
func TestOutputTailKeepsTheLastBytes(t *testing.T) {
	for _, last := range []string{"refusal", "!"} {
		var tl outputTail
		_, _ = tl.Write([]byte(strings.Repeat("a", outputTailSize)))
		_, _ = tl.Write([]byte(last))
		if s := tl.String(); len(s) != outputTailSize || !strings.HasSuffix(s, last) {
			t.Fatalf("tail of %d bytes ends %q; want %d ending with the last write %q", len(s), s[len(s)-10:], outputTailSize, last)
		}
	}
}

// A native agent that failed its start on a taken port is a start to make
// again, whether its report reaches waitForAgent (agentReportedStartFailure)
// or its exit does first: its last report is read as it exits (noteBootEnd).
func TestANativeAgentsTakenPortIsAStartToMakeAgain(t *testing.T) {
	a := &AgentClient{logger: zap.NewNop()}
	failure := "the proxy's port 16789 is taken by pid 7 (x): bind: address already in use"
	if err := a.agentReportedStartFailure(utils.BootProgress{Failed: true, ExitCode: 1, PortTaken: true}, failure, false); !errors.Is(err, utils.ErrPortTaken) || !strings.Contains(err.Error(), failure) {
		t.Fatalf("agentReportedStartFailure = %v; want utils.ErrPortTaken saying %q", err, failure)
	}
	if err := a.agentReportedStartFailure(utils.BootProgress{Failed: true, ExitCode: 1}, failure, false); errors.Is(err, utils.ErrPortTaken) {
		t.Fatalf("agentReportedStartFailure without PortTaken = %v; want no ErrPortTaken", err)
	}
	// Only a report of a failed start counts: one that does not say the start
	// failed is no start to make again, whatever port it names.
	for _, c := range []struct{ failed, taken bool }{{true, true}, {true, false}, {false, true}} {
		dir := t.TempDir()
		run := &agentRun{bootReport: filepath.Join(dir, utils.BootProgressFileName)}
		b, _ := json.Marshal(utils.BootProgress{Failed: c.failed, ExitCode: 1, Failure: failure, PortTaken: c.taken})
		if err := os.WriteFile(run.bootReport, b, 0o600); err != nil {
			t.Fatal(err)
		}
		run.noteBootEnd()
		exit := errors.New("exit status 1")
		err := a.agentExited(run, exit, false)
		want := c.failed && c.taken
		if errors.Is(err, utils.ErrPortTaken) != want || (want && !strings.Contains(err.Error(), failure)) || (!want && !errors.Is(err, exit)) {
			t.Fatalf("agentExited with a report Failed=%v PortTaken=%v: %v", c.failed, c.taken, err)
		}
	}
	if err := a.agentExited(nil, errors.New("exit status 1"), false); errors.Is(err, utils.ErrPortTaken) {
		t.Fatalf("agentExited with no run: %v", err)
	}
}
