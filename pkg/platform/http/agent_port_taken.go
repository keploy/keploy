package http

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v3/utils"
)

// A start of the agent that fails because another socket took a port the CLI
// found free and handed it (utils.ErrPortTaken) is one the caller starts
// again at once, on ports Setup picks again (pkg.RetryAgentSetup):
//
//   - a native agent off Linux, whose proxy or DNS server cannot move off its
//     port once its hooks are loaded, says so in its report of its start
//     (utils.BootProgress.PortTaken: agentReportedStartFailure, agentExited);
//   - a Docker agent's `docker run` cannot publish the agent's port on the
//     host, which the engine refuses before the container runs, exiting
//     engineRunFailed (publishRefusedBy).

// agentExited is waitForAgent's error for an agent whose process (or its
// container's client) exited before it was ready: the failure the native
// agent's last report said, where that was a taken port (agentRun.portTaken),
// else what its exit says (agentStoppedBeforeReady).
func (a *AgentClient) agentExited(run *agentRun, exitErr error, isDockerCmd bool) error {
	if run != nil {
		if failure := run.portTaken.Load(); failure != nil {
			return a.agentReportedStartFailure(utils.BootProgress{Failed: true, PortTaken: true, Failure: *failure}, *failure, isDockerCmd)
		}
	}
	return a.agentStoppedBeforeReady(exitErr, isDockerCmd)
}

// engineRunFailed is the code `docker run` (and `podman run`) exits with when
// the engine itself failed to run the container, before it ran: a port it
// could not publish among the reasons.
const engineRunFailed = 125

// outputTailSize is how much of what the agent's `docker run` printed last
// is kept (outputTail): the engine's refusal is its last line.
const outputTailSize = 8 << 10

// outputTail keeps the last outputTailSize bytes written to it: what the
// agent's `docker run` printed, as keploy's console got it.
type outputTail struct {
	mu sync.Mutex
	b  []byte
}

func (t *outputTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - outputTailSize; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *outputTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// publishRefused is the end of an agent's `docker run` (err) whose engine
// could not publish the agent's port on the host because another socket held
// it: utils.ErrPortTaken, and still err (its exit code) unwrapped.
type publishRefused struct {
	msg string
	err error
}

func (e *publishRefused) Error() string        { return e.msg }
func (e *publishRefused) Unwrap() error        { return e.err }
func (e *publishRefused) Is(target error) bool { return target == utils.ErrPortTaken }

// publishRefusedBy is err, the end of the agent's `docker run`, as a
// publishRefused where the run exited engineRunFailed and its output (said)
// has the engine's refusal of the agent's port, port: a line that names
// 127.0.0.1:port (agentPortPublish) and says it is taken, in Docker's words
// ("Bind for 127.0.0.1:P failed: port is already allocated", "failed to bind
// host port for 127.0.0.1:P:...: address already in use", "listen tcp4
// 127.0.0.1:P: bind: address already in use") or podman's ("rootlessport
// listen tcp 127.0.0.1:P: bind: address already in use"). Any other end is
// err as it is: the refusal of another port (an application's, which keploy
// did not pick) is not a start that would go otherwise on another agent port.
func publishRefusedBy(err error, said string, port uint32) error {
	var ee interface{ ExitCode() int }
	if !errors.As(err, &ee) || ee.ExitCode() != engineRunFailed {
		return err
	}
	line, ok := publishRefusalLine(said, port)
	if !ok {
		return err
	}
	return &publishRefused{
		msg: fmt.Sprintf("the container engine could not publish the keploy agent's %s, saying %q: %v", utils.PortTaken(port), line, err),
		err: err,
	}
}

// publishRefusalLine is the last line of said that refuses port on
// 127.0.0.1 as taken (publishRefusedBy), trimmed.
func publishRefusalLine(said string, port uint32) (string, bool) {
	addr := "127.0.0.1:" + strconv.FormatUint(uint64(port), 10)
	lines := strings.Split(said, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.Contains(line, "already allocated") && !strings.Contains(line, "address already in use") {
			continue
		}
		for rest := line; ; {
			at := strings.Index(rest, addr)
			if at < 0 {
				break
			}
			rest = rest[at+len(addr):]
			if rest == "" || rest[0] < '0' || rest[0] > '9' {
				return line, true
			}
		}
	}
	return "", false
}
