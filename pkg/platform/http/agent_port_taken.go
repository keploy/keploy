package http

import (
	"errors"
	"fmt"
	"regexp"
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
// refuses the agent's port, port, in words an engine says of a host port
// another socket holds (takenPortWordings), naming 127.0.0.1:port
// (agentPortPublish). Words keploy does not know for that fail closed: a run
// that exited engineRunFailed with a line naming that address in other words
// is not taken for a taken port, nor started again, and its error shows the
// line. Any other end is err as it is: the refusal of another port (an
// application's, which keploy did not pick) is not a start that would go
// otherwise on another agent port.
func publishRefusedBy(err error, said string, port uint32) error {
	var ee interface{ ExitCode() int }
	if !errors.As(err, &ee) || ee.ExitCode() != engineRunFailed {
		return err
	}
	line, known := publishRefusalLine(said, "127.0.0.1:"+strconv.FormatUint(uint64(port), 10))
	switch {
	case known:
		return &publishRefused{
			msg: fmt.Sprintf("the container engine could not publish the keploy agent's port: %s, saying %q: %v", utils.PortTaken(port), line, err),
			err: err,
		}
	case line != "":
		return fmt.Errorf("%w; the container engine said %q of the keploy agent's port %d, in words keploy does not know for a taken port, so it does not start the agent again", err, line, port)
	}
	return err
}

// takenPortWordings are the words in which the container engines refuse to
// publish a host port another socket holds, each a regular expression of the
// line around the address refused (%s, 127.0.0.1:P quoted), which a fixed
// character other than a digit follows:
//   - Docker, the port published by another container: "Bind for
//     127.0.0.1:P failed: port is already allocated";
//   - Docker 29, a host process holding it: "failed to bind host port
//     127.0.0.1:P/tcp: address already in use";
//   - Docker before 29, without its userland proxy: "failed to bind host port
//     for 127.0.0.1:P:172.17.0.2:P/tcp: address already in use";
//   - a Go listener on the host that could not bind it, in Linux's and macOS's
//     words or in Windows' (WSAEADDRINUSE): Docker's userland proxy ("listen
//     tcp4 127.0.0.1:P: bind: address already in use"), podman's rootlessport
//     ("rootlessport listen tcp 127.0.0.1:P: bind: address already in use"),
//     and Docker Desktop on macOS and on Windows ("Ports are not available:
//     exposing port TCP 127.0.0.1:P -> 0.0.0.0:0: listen tcp 127.0.0.1:P:
//     bind: Only one usage of each socket address (protocol/network
//     address/port) is normally permitted.").
var takenPortWordings = []string{
	`Bind for %s failed: port is already allocated`,
	`failed to bind host port %s/tcp: address already in use`,
	`failed to bind host port for %s:\S+/tcp: address already in use`,
	`listen tcp4? %s: bind: (?:address already in use|Only one usage of each socket address \(protocol/network address/port\) is normally permitted)`,
}

// publishRefusalLine is the last line of said, trimmed, that refuses addr in
// one of takenPortWordings (known), or else the last that names addr, with
// no digit after it, in other words; "" where none names it.
func publishRefusalLine(said, addr string) (line string, known bool) {
	quoted := regexp.QuoteMeta(addr)
	wordings := make([]*regexp.Regexp, len(takenPortWordings))
	for i, w := range takenPortWordings {
		wordings[i] = regexp.MustCompile(fmt.Sprintf(w, quoted))
	}
	names := regexp.MustCompile(quoted + `(?:[^0-9]|$)`)
	lines := strings.Split(said, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		for _, w := range wordings {
			if w.MatchString(l) {
				return l, true
			}
		}
		if line == "" && names.MatchString(l) {
			line = l
		}
	}
	return line, false
}
