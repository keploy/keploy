// Package http contains the client side code to communicate with the agent server
package http

import (
	"bytes"
	"context"
	_ "embed" // necessary for embedding
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/token"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	"github.com/klauspost/compress/zstd"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	ptls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/pkg/platform/agentargs"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/platform/engine"
	agentUtils "go.keploy.io/server/v3/pkg/platform/http/utils"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	keployLog "go.keploy.io/server/v3/utils/log"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	agentReadyRetryInterval = 2 * time.Second
)

// TODO: Need to refactor this file
type AgentClient struct {
	// streamEnds says, for each frame stream (models.FrameStream), whether
	// the agent ended the latest one with its end marker (StreamEnded).
	streamEnds   sync.Map
	logger       *zap.Logger
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	apps         sync.Map
	client       http.Client
	conf         *config.Config
	agentCmd     *exec.Cmd             // Track the agent process
	agentPTY     *agentUtils.PTYHandle // Track the PTY handle for interactive commands
	mu           sync.Mutex
	agentCancel  context.CancelFunc // Function to cancel the agent context

	// agentContainer is the container Setup set the agent up to run in, in a
	// docker mode (docker run, or as a service docker compose starts): the
	// entry its naming made (nameAgentContainer), on which Run and the
	// watches note what they see of it, its name with it; nil natively.
	// agentContainerAt is the agent address it is mapped at in containerAt.
	// Both are what the client forgets, and only that, when it sets up
	// another agent (forgetAgentContainer). Under mu. WatchAgentStart
	// watches its start.
	agentContainer   *namedContainer
	agentContainerAt string

	// startedAgent: this client started the agent it talks to (natively, or
	// in a container it runs), so nothing but this client stops it. Not
	// under docker compose (the project starts it), nor for an agent another
	// process started (a k8s sidecar).
	startedAgent atomic.Bool
	// --from-container only. The user's own container is stopped for the
	// duration of the session, so SOMETHING has to start it again on every way
	// out. restoreOnce makes that safe to call from each of them.
	fromContainer           string
	fromContainerWasRunning bool
	// Everything needed to put the user's container back as it was. Captured
	// before it is stopped, because the container does not survive the session
	// intact: Docker drops its network endpoint while it sits stopped and the
	// agent holds the same network under the app's own alias, and a container
	// with no endpoint can neither publish its ports nor resolve a service
	// name. Re-attaching in place does not stick, so repairing it means
	// re-creating it - which needs all of this.
	fromContainerSpec        *sourceContainerSpec
	restoreFromContainerOnce sync.Once

	// consumedPerTest holds what the agent's last /updatemockparams answer said
	// about how it reads the consumed history (models.ConsumedScopeHeader).
	// StoreMocks clears it: a store addresses an agent, possibly a fresh one,
	// whose answer this client has not seen yet.
	consumedPerTest atomic.Bool
	// consumed is what the agent holds of the consumed history this client
	// sends it (models.ConsumedSync).
	consumed consumedSync
	// stagedBackground holds the background rules the agent's answer to the
	// last staging call said it serves the set by
	// (models.BackgroundRulesHeader): "" until one says the background rules.
	stagedBackground atomic.Pointer[string]
	// mockWindows is what the agent's answers to /updatemockparams since the
	// last StoreMocks said of how it served their windows
	// (models.MockWindowHeader; AgentMockWindow).
	mockWindows mockWindowsHeard
	// clientTestsRead holds whether the agent's answer to the last /mock
	// said its learn step reads the session's tests as keploy mock's client
	// tests (models.ClientTestsReadHeader). Every call clears it first.
	clientTestsRead atomic.Bool
	// agentVersion holds the keploy version the agent's last successful
	// answer to /mock or /updatemockparams named (models.AgentVersionHeader):
	// "" where it named none, and until one is answered.
	agentVersion atomic.Pointer[string]

	// concurrentServing holds what the agent's last /storemocks answer said
	// of serving a test the recordings it shares with the tests that ran
	// beside it (models.ExactConcurrentHeader). Each StoreMocks clears it
	// first: a store addresses an agent, possibly a fresh one.
	concurrentServing atomic.Bool

	// run is the agent's current start (startAgent); guarded by mu. A stop
	// reads how to end the agent from the run it stops, never from a later one.
	run *agentRun
}

// agentRun is one start of the agent.
//
// The agent removes what it attached to the kernel as it shuts down, TLS
// uprobes included, and the kernel abandons a uprobe removal whose thread is
// killed part way ("failed to unregister, leaking uprobe"), leaving the
// breakpoint in every process that maps the probed library. So the CLI stops
// an agent by asking it, or by SIGTERM, and waits for it (gone) while its
// teardown makes progress -- killing it only once that stops, or it has
// detached everything and not exited, or the user presses Ctrl+C once keploy
// has said a press stops it (stopWatch: a press answering the first offer has
// it die once no teardown work in progress is making progress, one answering
// an offer made after a press kills it at once -- an agent sudo runs as root,
// which no kill ends at once, is waited for instead until it has exited or
// halted); never on a timer, never by a hang-up from closing its terminal
// (endAgent, stopAgentContainerFor). That holds before the agent answers too:
// it attaches its uprobes first.
//
// ready is set once the agent answered its readiness check (waitForAgent), so
// its HTTP server was up. It decides two things. A native agent whose stop
// request then fails as if it were going away is waited for before it is
// signalled (endAgent); before it answered, the same failure says only that it
// is not listening yet. And an unanswered agent's container is sent SIGTERM
// again while it is waited for: its PID 1 may still have been the OSS image's
// entrypoint, which drops a SIGTERM (resendSIGTERM). One run per start
// (startAgent): the stop of an earlier start, which a setup retry waits for,
// reads its own.
type agentRun struct {
	ready atomic.Bool
	// scope is the teardown scope of the run that started the agent
	// (utils.TeardownScopeOf of the start's context): its stop's reports
	// that move tick it (stopWatch.moved). Set before the run is shared.
	scope    *utils.TeardownScope
	gone     chan struct{} // closed once the agent process (or its container's client) has exited
	goneOnce sync.Once
	// letGo is closed once its stop killed it at once at the user's Ctrl+C,
	// or found an agent sudo runs as root halted after a press (stopWatch):
	// what waits for its exit waits no more.
	letGo     chan struct{}
	letGoOnce sync.Once
	// bootReport is the file a native agent reports its start's progress in
	// (utils.BootProgressFileName, next to its teardown progress file), set
	// before it starts; "" for a containerised one, whose report is read out
	// of its container. bootPrompt: it is started by a sudo that may prompt
	// for the user's password in front of it (no cached credentials, on a PTY
	// or on the terminal sudo opens itself), which the user answers in their
	// own time; sudoPid is that sudo's process, once started.
	bootReport string
	bootPrompt bool
	sudoPid    atomic.Int64
	// agentComm is the agent's process name as the kernel keeps it (its
	// binary's, cut to 15 bytes), which sudoRanAgent looks for.
	agentComm string
}

// watchBoot sets what waitForAgent watches of a native agent's start, before
// it starts: its report, next to its teardown progress file (progress; none
// when that is ""), whether the sudo that starts it may prompt in front of it
// (prompts), and the name its process runs as, the binary bin's.
func (r *agentRun) watchBoot(progress string, prompts bool, bin string) {
	if progress != "" {
		r.bootReport = filepath.Join(filepath.Dir(progress), utils.BootProgressFileName)
	}
	r.bootPrompt = prompts
	r.agentComm = filepath.Base(bin)
	if len(r.agentComm) > 15 { // TASK_COMM_LEN less its NUL
		r.agentComm = r.agentComm[:15]
	}
}

// sudoMayPrompt reports whether the sudo keploy starts a native agent with may
// prompt for the user's password: it elevates the agent (elevates, as euid,
// not root, needs), and without cached credentials (sudo -n otherwise). It
// prompts on the PTY keploy gives it, or, without one, on the terminal it
// opens itself.
func sudoMayPrompt(elevates, sudoCached bool, euid int) bool {
	return elevates && !sudoCached && euid != 0
}

// sudoRanAgent reports whether the sudo that starts the agent (bootPrompt) has
// run it: its prompt, if it made one, has been answered, and the agent's
// process runs. That is the command sudo runs (agentUtils.CommandUnderSudo),
// once it is the agent (its process name): not a helper sudo runs while the
// user is still at its prompt (a PAM module's).
func (r *agentRun) sudoRanAgent(context.Context) bool {
	sudo := r.sudoPid.Load()
	if sudo <= 0 {
		return false
	}
	pid := agentUtils.CommandUnderSudo(int(sudo))
	if pid <= 0 {
		return false
	}
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return err == nil && strings.TrimSpace(string(comm)) == r.agentComm
}

// newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached and
// needsAgentPTY are how a native agent's command is made (without a PTY, and
// on one), whether sudo has cached credentials, and whether its start needs a
// PTY: variables for tests.
var (
	newNativeAgentCommand    = agentUtils.NewAgentCommand
	newNativeAgentPTYCommand = agentUtils.NewAgentCommandForPTY
	sudoCredentialsCached    = utils.AreSudoCredentialsCached
	needsAgentPTY            = agentUtils.NeedsPTY
)

func newAgentRun() *agentRun { return &agentRun{gone: make(chan struct{}), letGo: make(chan struct{})} }

// stoppedAtOnce says its stop killed it at once at the user's Ctrl+C, or
// found a root agent halted after one (stopWatch.run returned true).
func (r *agentRun) stoppedAtOnce() { r.letGoOnce.Do(func() { close(r.letGo) }) }

// exited marks the agent gone; it may be called more than once.
func (r *agentRun) exited() { r.goneOnce.Do(func() { close(r.gone) }) }

// isGone reports whether the agent has exited.
func (r *agentRun) isGone() bool {
	select {
	case <-r.gone:
		return true
	default:
		return false
	}
}

// currentRun is the agent's current start, or nil before the first.
func (a *AgentClient) currentRun() *agentRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.run
}

// AgentReadsConsumedPerTestOnly reports whether the agent's last
// /updatemockparams answer said it reads the consumed history only for the
// per-test mocks it stages (models.ConsumedScopePerTest). Until an answer has
// said so, and again after every StoreMocks, it is false and the whole history
// must be sent.
func (a *AgentClient) AgentReadsConsumedPerTestOnly() bool {
	return a.consumedPerTest.Load()
}

// ServesConcurrent reports whether the agent's last /storemocks answer said it
// serves a test the recordings it shares with the tests that ran beside it at
// record time (models.ExactConcurrentHeader). False until a store has said so,
// and from the start of every StoreMocks.
func (a *AgentClient) ServesConcurrent() bool {
	return a.concurrentServing.Load()
}

// AgentReadsClientTests reports whether the agent's answer to the last /mock
// said its learn step reads the session's tests as keploy mock's client
// tests (models.ClientTestsReadHeader). False until an answer says so, and
// from the start of every call.
func (a *AgentClient) AgentReadsClientTests() bool {
	return a.clientTestsRead.Load()
}

// AgentVersion is the keploy version the agent's last successful answer to
// /mock or /updatemockparams named (models.AgentVersionHeader;
// pkg.AgentVersionReader): "" where it named none, as an agent older than
// this keploy does not, and until one is answered.
func (a *AgentClient) AgentVersion() string {
	if v := a.agentVersion.Load(); v != nil {
		return *v
	}
	return ""
}

// noteAgentVersion keeps the version a successful answer's header names
// (AgentVersion), "" where it names none.
func (a *AgentClient) noteAgentVersion(h http.Header) {
	v := h.Get(models.AgentVersionHeader)
	a.agentVersion.Store(&v)
}

// SendTestCaseFilter sends params, a filter that opens a test case and
// carries its request, as UpdateMockParams does, and reports whether the
// agent's answer to this call said its learn step took the request
// (models.TestRequestReadHeader; pkg.TestCaseFilterSender): false for an
// answer that says nothing, and for a call that fails.
func (a *AgentClient) SendTestCaseFilter(ctx context.Context, params models.MockFilterParams) (bool, error) {
	header, err := a.updateMockParams(ctx, params)
	if err != nil {
		return false, err
	}
	return header.Get(models.TestRequestReadHeader) == models.TestRequestRead, nil
}

// AgentServesBackground is the background rules the agent's answer to the
// last staging call said it serves the staged set by
// (models.BackgroundRulesHeader): "" for today's, and until a staging is
// answered.
func (a *AgentClient) AgentServesBackground() string {
	if r := a.stagedBackground.Load(); r != nil {
		return *r
	}
	return ""
}

// mockWindowsHeard folds what the agent's answers to /updatemockparams said
// of how it served their windows (models.MockWindowHeader) since the last
// StoreMocks, the set's mocks: said, what they said (models.MockWindowsSaid);
// unsaid, that some answer said nothing.
type mockWindowsHeard struct {
	mu            sync.Mutex
	said          models.MockWindowsSaid
	heard, unsaid bool
}

func (h *mockWindowsHeard) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.said, h.heard, h.unsaid = models.MockWindowsSaid{}, false, false
}

func (h *mockWindowsHeard) note(v string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := models.ParseMockWindowMode(v)
	if !ok {
		h.unsaid = true
		return
	}
	h.heard = true
	h.said.Add(m)
}

// AgentMockWindow is what the agent's answers to /updatemockparams since the
// set's mocks were stored said of how it served their windows
// (pkg.MockWindowReader): ok is false when none was answered, or some answer
// said nothing, as an agent older than models.MockWindowHeader does.
func (a *AgentClient) AgentMockWindow() (models.MockWindowsSaid, bool) {
	h := &a.mockWindows
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.heard || h.unsaid {
		return models.MockWindowsSaid{}, false
	}
	return h.said, true
}

// var initStopScript []byte

func New(logger *zap.Logger, client kdocker.Client, c *config.Config) *AgentClient {

	return &AgentClient{
		logger:       logger,
		dockerClient: client,
		client:       agentHTTPClient(token.Session()),
		conf:         c,
	}
}

// markDrainsAfterStop says, on a frame stream's request, that its client
// drains the stream after its stop and closes it before it stops the agent
// (models.WithDrainsAfterStop): the agent holds a stop signal for it. Only
// when this client started the agent: otherwise something else stops it
// (docker compose stops it with the project, which the client waits on;
// kubelet stops a sidecar with its pod), and an agent that held for the
// client would only be killed at that stop's timeout. Every request that
// opens the stream carries it, a reconnection too.
//
// The stream carries its recording's token too (models.StreamEndTokenHeader),
// whoever started the agent: the recording's stop asks the agent to end the
// streams of that token (EndRecordingStreams).
//
// A stream whose client watches for the agent's own word of a stop
// (models.WithAgentStopNotice, WatchAgentStop) says so instead, when this
// client did not start the agent: the agent holds a stop signal for it while
// it has something for it to save, and tells it (models.DrainsOnAgentStopHeader).
func (a *AgentClient) markDrainsAfterStop(ctx context.Context, req *http.Request) {
	switch {
	case models.DrainsAfterStop(ctx) && a.startedAgent.Load():
		req.Header.Set(models.DrainsAfterStopHeader, "true")
	case models.AgentStopNotice(ctx) && !a.startedAgent.Load():
		req.Header.Set(models.DrainsOnAgentStopHeader, "true")
	}
	if tok := models.StreamEndToken(ctx); tok != "" {
		req.Header.Set(models.StreamEndTokenHeader, tok)
	}
}

func (a *AgentClient) GetIncoming(ctx context.Context, opts models.IncomingOptions) (<-chan *models.TestCase, error) {

	a.logger.Debug("Connecting to incoming test cases stream...")

	requestBody := models.IncomingReq{
		IncomingOptions: opts,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for incoming request")
		return nil, fmt.Errorf("error marshaling request body for incoming request: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/incoming", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for incoming request")
		return nil, fmt.Errorf("error creating request for incoming request: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	a.markDrainsAfterStop(ctx, req)

	// Registered before it connects; it outlives a restarted agent (see
	// agentSession).
	stream := a.session().openStream()
	conn := stream.connect(ctx)
	res, err := a.client.Do(req.WithContext(conn.ctx))
	if err != nil {
		stream.close()
		return nil, fmt.Errorf("failed to get incoming: %s", err.Error())
	}
	if err := agentStreamStatus("get incoming", res); err != nil {
		stream.close()
		return nil, err
	}
	res.Body = conn.body(res.Body)

	// Create a channel to stream TestCase data
	tcChan := make(chan *models.TestCase)
	ended := a.newStreamEnd(models.TestCaseStream)

	// The first response's framing is checked here, where it can still fail
	// the call; see readIncoming for the rest.
	if mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err == nil &&
		strings.HasPrefix(mediaType, "multipart/") && strings.TrimSpace(params["boundary"]) == "" {
		stream.close()
		if res.Body != nil {
			res.Body.Close()
		}
		return nil, fmt.Errorf("missing multipart boundary in content-type: %s", res.Header.Get("Content-Type"))
	}

	go func() {
		defer func() {
			close(tcChan)
			stream.close()
		}()
		for {
			done := a.readIncoming(ctx, res, tcChan, ended)
			if res.Body != nil {
				if err := res.Body.Close(); err != nil {
					utils.LogError(a.logger, err, "failed to close response body for incoming request")
				}
			}
			if done {
				return
			}
			var ok bool
			if res, ok = a.resumeStream(ctx, stream, "get incoming", http.MethodPost, "/incoming", requestJSON); !ok {
				return
			}
		}
	}()

	a.logger.Debug("Successfully connected to incoming test cases stream.")
	return tcChan, nil
}

// readIncoming hands the test cases of one incoming stream to tcChan until the
// stream ends, in the stream's framing (multipart, or a legacy JSON stream).
// It reports whether the stream must not go on: ctx is done, or the response
// cannot be read at all.
//
// A stream the agent ends with its end marker sets ended, and is done.
func (a *AgentClient) readIncoming(ctx context.Context, res *http.Response, tcChan chan<- *models.TestCase, ended *atomic.Bool) (done bool) {
	contentType := res.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		utils.LogError(a.logger, err, "failed to parse content type", zap.String("content-type", contentType))
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			utils.LogError(a.logger, nil, "missing multipart boundary in content-type", zap.String("content-type", contentType))
			return true
		}
		return a.readIncomingParts(ctx, res, boundary, tcChan, ended)
	}
	return a.readIncomingJSON(ctx, res, tcChan)
}

// unnamedCallerConnsSaid is what the client says of the session's connections
// to the app whose callers the agent could not name
// (models.StreamEndSummary.UnnamedCallerConns).
const unnamedCallerConnsSaid = "the keploy agent recorded no test case for the calls some connections to the app carried: it could not name their callers in its PID namespace, so it could not tell whether a test made them"

// tlsRelayedConnsSaid is what the client says of the session's connections to
// the app that opened with a TLS handshake, which the agent relayed to the
// app's own TLS unread (models.StreamEndSummary.TLSRelayedConns).
const tlsRelayedConnsSaid = "the keploy agent recorded no test case for the calls some connections to the app carried: they were TLS (HTTPS), which it relayed to the app's own TLS without reading"

// sayAgentSummary says, in this client's own log, what the agent's summary of
// the session says it lost (models.StreamEndSummary): for an agent this client
// did not start (docker compose's, kubelet's), whose own log reaches the user
// through compose or kubelet.
func (a *AgentClient) sayAgentSummary(s models.StreamEndSummary) {
	if s.LeftOutInFlight > 0 {
		a.logger.Warn("the keploy agent left some test cases out of the recording: each stayed in flight (a slow endpoint, a long poll, a stream) while mocks it may have made had to be let go",
			zap.Uint64("test_cases", s.LeftOutInFlight))
	}
	if s.NotHandedOn > 0 {
		a.logger.Warn("the keploy agent lost some test cases: the app served their requests, and the agent's ingress stopped before their test cases reached the recording",
			zap.Uint64("test_cases", s.NotHandedOn))
	}
	if s.UnnamedCallerConns > 0 {
		a.logger.Warn(unnamedCallerConnsSaid,
			zap.Uint64("connections", s.UnnamedCallerConns),
			zap.String("next_step", "run keploy in the PID namespace of the app's tests, or in the host's (--pid=host, or hostPID in Kubernetes)"))
	}
	if s.TLSRelayedConns > 0 {
		a.logger.Warn(tlsRelayedConnsSaid,
			zap.Uint64("connections", s.TLSRelayedConns),
			zap.String("next_step", "serve the app's API over plain HTTP (or h2c gRPC) to the tests while recording, for example with TLS ended in front of the app, so keploy can read and record these calls"))
	}
	if s.Unsettled > 0 {
		a.logger.Warn("the keploy agent saved some test cases before it had parsed all the traffic they overlap; if a connection's parser failed on it afterwards, they lack mocks",
			zap.Int("test_cases", s.Unsettled))
	}
	if p := s.NotRecordedUnderPressure; p.Requests > 0 {
		a.logger.Warn("the keploy agent did not record some requests: it was short of memory when they came, and its memory guard had paused recording; each connection's requests after the pause are recorded",
			zap.Uint64("requests", p.Requests), zap.Int("connections", p.Connections))
	}
	if s.MassSuppressed {
		a.logger.Warn("the keploy agent left out a large share of this recording's test cases: it could not capture their mocks",
			zap.Int("suppressed", s.TestCasesSuppressed), zap.Int("recorded", s.TestCasesSent))
	}
}

// readIncomingParts reads a multipart incoming stream; see readIncoming.
func (a *AgentClient) readIncomingParts(ctx context.Context, res *http.Response, boundary string, tcChan chan<- *models.TestCase, ended *atomic.Bool) (done bool) {
	mr := multipart.NewReader(res.Body, boundary)
	var pendingTestCase *models.TestCase

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil ||
				strings.Contains(err.Error(), "closed network connection") ||
				errors.Is(err, io.ErrUnexpectedEOF) ||
				strings.Contains(err.Error(), "unexpected EOF") {
				a.logger.Debug("multipart stream ended", zap.Error(err))
				break
			}
			utils.LogError(a.logger, err, "error reading stream part")
			break
		}

		if part.FormName() == models.StreamEndPart {
			// The agent ended the stream on this client's request, after all
			// it had (EndRecordingStreams): no more to read, nor to resume.
			// Its marker says what the session lost: said here for an agent
			// this client did not start, whose own log reaches the user
			// through compose or kubelet, and can be cut off as it stops.
			var sum models.StreamEndSummary
			if err := json.NewDecoder(part).Decode(&sum); err == nil && !a.startedAgent.Load() {
				a.sayAgentSummary(sum)
			}
			ended.Store(true)
			return true
		}
		if part.FormName() == "metadata" {
			var tc models.TestCase
			if err := json.NewDecoder(part).Decode(&tc); err != nil {
				utils.LogError(a.logger, err, "failed to decode metadata json")
				continue
			}
			if tc.HasBinaryFile {
				pendingTestCase = &tc
			} else {
				select {
				case <-ctx.Done():
					return true
				case tcChan <- &tc:
					pendingTestCase = nil
				}
			}
		} else if part.FormName() == "file" {
			if pendingTestCase == nil {
				utils.LogError(a.logger, nil, "Received file part without preceding metadata, skipping...")
				continue
			}
			fileName := part.FileName()
			sanitizedFileName := filepath.Base(fileName)
			if sanitizedFileName == "." || sanitizedFileName == string(filepath.Separator) || sanitizedFileName == "" {
				sanitizedFileName = "testcase_blob"
			}
			a.logger.Debug("Received binary file part", zap.String("file_name", fileName), zap.String("sanitized_name", sanitizedFileName))

			savePath := filepath.Join(os.TempDir(), fmt.Sprintf("keploy_%d_%s", time.Now().UnixNano(), sanitizedFileName))
			outFile, err := os.Create(savePath)
			if err != nil {
				utils.LogError(a.logger, err, "failed to create temp file for stream")
				continue
			}

			_, err = io.Copy(outFile, part)
			outFile.Close()
			if err != nil {
				utils.LogError(a.logger, err, "failed to write file stream to disk")
				continue
			}
			a.logger.Debug("Successfully wrote binary file to temp storage", zap.String("path", savePath))
			// Link matching file path logic
			updated := false
			for i := range pendingTestCase.HTTPReq.Form {
				form := &pendingTestCase.HTTPReq.Form[i]
				for j, fname := range form.FileNames {
					if (fname == fileName || fname == sanitizedFileName || filepath.Base(fname) == sanitizedFileName) && j < len(form.Paths) {
						form.Paths[j] = savePath
						updated = true
					}
				}
			}
			if !updated {
				for i := range pendingTestCase.HTTPReq.Form {
					form := &pendingTestCase.HTTPReq.Form[i]
					if len(form.Paths) > 0 {
						form.Paths[0] = savePath
						break
					}
				}
			}

		} else if part.FormName() == "delimiter" {
			if pendingTestCase == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return true
			case tcChan <- pendingTestCase:
				pendingTestCase = nil
			}
		}
	}
	return false
}

// readIncomingJSON reads a legacy JSON incoming stream; see readIncoming.
func (a *AgentClient) readIncomingJSON(ctx context.Context, res *http.Response, tcChan chan<- *models.TestCase) (done bool) {
	decoder := json.NewDecoder(res.Body)

	for {
		var testCase models.TestCase
		if err := decoder.Decode(&testCase); err != nil {
			if utils.IsShutdownError(err) {
				// End of the stream or connection closed during shutdown
				break
			}
			utils.LogError(a.logger, err, "failed to decode test case from stream")
			break
		}

		select {
		case <-ctx.Done():
			// If the context is done, exit the loop
			return true
		case tcChan <- &testCase:
			// Send the decoded test case to the channel
		}
	}
	return false
}

// mockHandoffBuffer sizes the decoder -> consumer hand-off. Deep enough that a
// slow InsertMock cannot park the decoder behind a mock it has already read off
// the wire.
const mockHandoffBuffer = 1024

// mockHandoffGrace bounds how long the decoder waits for the consumer to take a
// mock before giving up on it, in the time this process could run
// (utils/stall), not wall time: a CLI starved of CPU has a consumer that could
// not run either. Only reachable if the consumer has stopped draining
// entirely; a busy one drains in milliseconds.
const mockHandoffGrace = 30 * time.Second

// handOff offers m to ch, and reports whether it was taken before ch's reader
// took nothing for grace of the time the process could run (on clk; nil: the
// process's own).
func handOff[T any](ch chan<- T, m T, clk stall.Clock, grace time.Duration) bool {
	select {
	case ch <- m:
		return true
	default:
	}
	meter := stall.NewMeter(clk, grace)
	defer meter.Stop()
	for {
		select {
		case ch <- m:
			return true
		case <-meter.Wait():
			if meter.Expired() {
				return false
			}
		}
	}
}

func (a *AgentClient) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {

	a.logger.Debug("Connecting to outgoing mocks stream...")
	a.pushContainers(ctx)

	// Mirror the mock-noise pair onto BOTH spellings before the struct is
	// marshalled. This is the send-side twin of the normalise Proxy.Record and
	// Proxy.Mock do on receipt, and it is the half that protects a NEW client
	// talking to an OLD agent.
	//
	// OutgoingOptions carries no json tags, so it goes on the wire under Go
	// field names. A pre-rename agent decodes only SchemaNoise*, and every
	// producer in this repo now sets the canonical MockNoise* pair — so without
	// this the deprecated fields marshal as false and the toggle is dead on any
	// agent image built before the rename. Agent images are pinned separately
	// from the CLI (k8s-proxy sets proxy.keployAgentImage; enterprise is still
	// on an older keploy), so that skew is the normal state during rollout, not
	// an edge case.
	//
	// Done here rather than at the three call sites because this is the actual
	// wire boundary: a future producer that forgets is covered automatically.
	// NormalizeMockNoise is idempotent and nil-safe, so pairing it with the
	// receive-side call costs nothing.
	opts.NormalizeMockNoise()

	requestBody := models.OutgoingReq{
		OutgoingOptions: opts,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return nil, fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/outgoing", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
		return nil, fmt.Errorf("error creating request for mock outgoing: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	a.markDrainsAfterStop(ctx, req)

	// Registered before it connects; it outlives a restarted agent (see
	// agentSession).
	stream := a.session().openStream()
	conn := stream.connect(ctx)
	res, err := a.client.Do(req.WithContext(conn.ctx))
	if err != nil {
		stream.close()
		return nil, fmt.Errorf("failed to get outgoing response: %s", err.Error())
	}
	if err := agentStreamStatus("get outgoing", res); err != nil {
		stream.close()
		return nil, err
	}
	res.Body = conn.body(res.Body)

	// Buffered. An unbuffered channel parks the decoder for as long as the
	// consumer spends inside InsertMock - and the first insert does mkdir +
	// create + YAML encode + flush, which is exactly when the last mock of a
	// recording arrives. A decoded mock waiting there was the widest of the
	// three places a trailing mock could be lost.
	mockChan := make(chan *models.Mock, mockHandoffBuffer)
	ended := a.newStreamEnd(models.MockStream)

	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		stream.close()
		_ = res.Body.Close()
		return nil, fmt.Errorf("failed to get errorgroup from the context")
	}

	grp.Go(func() error {
		defer func() {
			close(mockChan)
			stream.close()
		}()
		for {
			dropped := a.readMocks(res, mockChan, ended)
			if err := res.Body.Close(); err != nil {
				utils.LogError(a.logger, err, "failed to close response body for getoutgoing")
			}
			if dropped {
				return nil
			}
			var ok bool
			if res, ok = a.resumeStream(ctx, stream, "get outgoing", http.MethodPost, "/outgoing", requestJSON); !ok {
				return nil
			}
		}
	})

	a.logger.Debug("Successfully connected to outgoing mocks stream.")

	return mockChan, nil
}

// readMocks hands the mocks of one outgoing stream to mockChan until the
// stream ends. It reports whether it dropped one because the consumer stopped
// taking them, or the agent ended the stream with its end marker (ended is
// set), after which the stream must not go on.
func (a *AgentClient) readMocks(res *http.Response, mockChan chan<- *models.Mock, ended *atomic.Bool) (stop bool) {
	decoder := gob.NewDecoder(res.Body)
	for {
		var mock models.Mock
		if err := decoder.Decode(&mock); err != nil {
			if utils.IsShutdownError(err) {
				// End of the stream or connection closed during shutdown
				break
			}
			utils.LogError(a.logger, err, "failed to decode mock from stream")
			break
		}
		if mock.Kind == models.StreamEndKind {
			// The agent ended the stream on this client's request, after all
			// it had (EndRecordingStreams).
			ended.Store(true)
			return true
		}

		// Offer the mock rather than racing the context for it.
		//
		// This used to be `select { case <-ctx.Done(): return nil; case
		// mockChan <- &mock }`, which threw away a mock already decoded
		// into our own address space the moment the capture context was
		// cancelled - silently, with agent-side accounting still reporting
		// success. Cancellation is the wrong thing to consult here: the
		// consumer persists on an uncancellable context, so it can always
		// accept, and the only reason it might not have yet is that it is
		// busy writing the previous mock.
		//
		// So always try to deliver, bounded so a genuinely dead consumer
		// cannot hang the stream. Anything still dropped is data loss and
		// is logged as an error, because the alternative - a mock set
		// indistinguishable from one where the call never happened - is
		// what made this class of bug take an investigation to find.
		if !handOff(mockChan, &mock, nil, mockHandoffGrace) {
			utils.LogError(a.logger, nil, "dropping a decoded mock: the consumer did not accept it in time",
				zap.String("mock", mock.Name), zap.String("kind", string(mock.Kind)),
				zap.Duration("waited", mockHandoffGrace))
			return true
		}
	}
	return false
}

func (a *AgentClient) GetMappings(ctx context.Context, opts models.IncomingOptions) (<-chan models.TestMockMapping, error) {

	a.logger.Debug("Connecting to mappings stream...")

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/mappings", a.conf.Agent.AgentURI), nil)
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mappings")
		return nil, fmt.Errorf("error creating request for mappings: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	a.markDrainsAfterStop(ctx, req)

	// Registered before it connects; it outlives a restarted agent (see
	// agentSession).
	stream := a.session().openStream()
	conn := stream.connect(ctx)
	res, err := a.client.Do(req.WithContext(conn.ctx))
	if err != nil {
		stream.close()
		return nil, fmt.Errorf("failed to get mappings response: %s", err.Error())
	}
	// An agent predating test-mock mapping (keploy #3715, Feb 2026) has no
	// /mappings route and answers 404. That is version skew, not a failure, and
	// it must NOT be reported as one: this call sits inside the record errgroup
	// (pkg/service/record/record.go), so any other error here aborts the entire
	// RECORDING. Before this status check existed the decoder goroutine simply
	// logged "failed to decode mapping from stream" and returned nil, and
	// recording completed without mappings — that degraded-but-working
	// behaviour is what a missing endpoint should still produce. It is said
	// here, and returned as models.ErrNoMappingStream, which the recorder takes
	// for a recording without mappings. Not as a channel closed at once: the
	// recorder reads a mapping stream that closes without the agent's marker
	// as the agent going away with mappings unsaved.
	//
	// Same tolerance BeginTestErrorCapture, GetScopeWindows, GetScopeTable and
	// DrainCapturedMocks already apply, and for the same reason StoreMocks
	// carries its legacy-framing fallback: the CLI ships ahead of the agent
	// image, so a lagging agent is the normal state during a rollout rather than
	// an edge case. /incoming and /outgoing need no such guard — both routes
	// predate #3016 and every agent that can serve a session has them.
	if res.StatusCode == http.StatusNotFound {
		stream.close()
		if closeErr := res.Body.Close(); closeErr != nil {
			utils.LogError(a.logger, closeErr, "failed to close response body for getmappings")
		}
		a.logger.Warn("agent has no /mappings endpoint — recording without test-mock mappings",
			zap.String("consequence", "replay falls back to timestamp-based mock filtering for this recording"),
			zap.String("remedy", "upgrade the agent image to one built from keploy #3715 or later"))
		return nil, models.ErrNoMappingStream
	}
	if err := agentStreamStatus("get mappings", res); err != nil {
		stream.close()
		return nil, err
	}
	res.Body = conn.body(res.Body)

	mappingChan := make(chan models.TestMockMapping)
	ended := a.newStreamEnd(models.MappingStream)

	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		stream.close()
		_ = res.Body.Close()
		return nil, fmt.Errorf("failed to get errorgroup from the context")
	}

	grp.Go(func() error {
		defer func() {
			close(mappingChan)
			stream.close()
		}()
		for {
			done := a.readMappings(ctx, res, mappingChan, ended)
			if err := res.Body.Close(); err != nil {
				utils.LogError(a.logger, err, "failed to close response body for getmappings")
			}
			if done {
				return nil
			}
			var ok bool
			if res, ok = a.resumeStream(ctx, stream, "get mappings", http.MethodPost, "/mappings", nil); !ok {
				return nil
			}
		}
	})

	a.logger.Debug("Successfully connected to mappings stream.")
	return mappingChan, nil
}

// readMappings hands the mappings of one mappings stream to mappingChan until
// the stream ends. It reports whether ctx is done, after which the stream must
// not go on.
func (a *AgentClient) readMappings(ctx context.Context, res *http.Response, mappingChan chan<- models.TestMockMapping, ended *atomic.Bool) (done bool) {
	decoder := json.NewDecoder(res.Body)
	for {
		var mapping models.TestMockMapping
		if err := decoder.Decode(&mapping); err != nil {
			if !utils.IsShutdownError(err) {
				utils.LogError(a.logger, err, "failed to decode mapping from stream")
			}
			return false
		}
		if mapping.StreamEnd {
			// The agent ended the stream on this client's request, after all
			// it had (EndRecordingStreams).
			ended.Store(true)
			return true
		}
		select {
		case <-ctx.Done():
			return true
		case mappingChan <- mapping:
		}
	}
}

func (a *AgentClient) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) (retErr error) {
	defer func() {
		if retErr == nil && a.session().recording() {
			a.session().record(stepMockOutgoing, func(c context.Context) error { return a.MockOutgoing(c, opts) })
		}
	}()
	a.pushContainers(ctx)
	a.pushNetworkNames(ctx)
	// Only a successful answer that says so (models.ClientTestsReadHeader)
	// sets it again below.
	a.clientTestsRead.Store(false)

	// See GetOutgoing: mirror both spellings before the marshal so a pre-rename
	// agent still sees the toggle.
	opts.NormalizeMockNoise()

	// make a request to the server to mock outgoing
	requestBody := models.OutgoingReq{
		OutgoingOptions: opts,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for mock outgoing")
		return fmt.Errorf("error marshaling request body for mock outgoing: %s", err.Error())
	}

	// mock outgoing request
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/mock", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mock outgoing")
		return fmt.Errorf("error creating request for mock outgoing: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		if err := res.Body.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to close response body for mockOutgoing")
		}
	}()

	// Read the full body so we can include it in the error message when
	// decode fails — the original `json.NewDecoder(...).Decode` consumed
	// only the first token, which made "404 page not found\n" responses
	// from a wrongly-prefixed AgentURI look like a "cannot unmarshal
	// number into models.AgentResp" error and masked the real misroute.
	rawBody, readErr := readAgentBody(res)
	if readErr != nil {
		return fmt.Errorf("failed to read response body for mock outgoing: %s", readErr.Error())
	}
	if err := agentRespErr("mock outgoing", res, rawBody); err != nil {
		return err
	}
	a.clientTestsRead.Store(res.Header.Get(models.ClientTestsReadHeader) == models.ClientTestsRead)
	a.noteAgentVersion(res.Header)
	return nil
}

// agentRespErr turns an agent reply into the caller's error, and is the ONE
// place that decides what a failure looks like. The order matters: the STATUS
// is checked before the body is decoded, because a failing agent (or an
// intermediary, or a 404 from a mis-prefixed AgentURI) does not owe us a
// well-formed AgentResp, and decoding first turns every one of those into a
// "cannot unmarshal ..." message that hides the real failure.
//
// It returns nil only when the status is 2xx AND the agent said IsSuccess.
func agentRespErr(op string, res *http.Response, rawBody []byte) error {
	var resp models.AgentResp
	decodeErr := json.Unmarshal(rawBody, &resp)

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if msg := resp.ErrorMsg; msg != "" {
			return fmt.Errorf("%s failed (status %d): %s", op, res.StatusCode, msg)
		}
		// Either an old agent (whose message never survived its own
		// marshal) or a non-AgentResp body such as a proxy error page.
		// The raw body is the best evidence available; bound it so a
		// runaway page cannot be pasted whole into a log line.
		return fmt.Errorf("%s failed (status %d, body: %q)", op, res.StatusCode, snippet(rawBody))
	}

	if decodeErr != nil {
		return fmt.Errorf("failed to decode response body for %s: %s (raw body: %q, status: %d, contentType: %q)",
			op, decodeErr.Error(), snippet(rawBody), res.StatusCode, res.Header.Get("Content-Type"))
	}
	if err := resp.Err(); err != nil {
		return fmt.Errorf("%s failed: %w", op, err)
	}
	if !resp.IsSuccess {
		// 2xx but the agent disowned the result. An old agent reaches
		// here on failure too (its message was destroyed before it was
		// sent), so quote the body rather than claiming success.
		return fmt.Errorf("%s returned failure (status %d, body: %q)", op, res.StatusCode, snippet(rawBody))
	}
	return nil
}

// snippet bounds an agent body for inclusion in an error message.
func snippet(b []byte) string {
	const max = 4096
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "...(truncated)"
	}
	return s
}

// agentStreamStatus rejects a non-2xx reply on the long-lived streaming
// endpoints (/incoming, /outgoing, /mappings) BEFORE a decoder is pointed at
// the body, and closes the body when it does.
//
// These handlers answer failures with http.Error — a text/plain line such as
// "failed to get outgoing: <reason>" — and the client used to hand that
// straight to a gob/JSON decoder. The decode failed, was logged as "failed to
// decode mock from stream", the channel was closed empty, and replay carried on
// with ZERO mocks: a silent pass-through recorded as a normal run. The agent's
// reason was in the body the whole time.
func agentStreamStatus(op string, res *http.Response) error {
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	// net/http guarantees a non-nil Body on a client response, so this reads
	// and closes unconditionally. An earlier revision guarded the Close with
	// `if res.Body != nil` AFTER already dereferencing it in the ReadAll above,
	// which would have panicked first -- a guard that read as protection and
	// was not.
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	_ = res.Body.Close()
	return fmt.Errorf("%s failed (status %d): %s", op, res.StatusCode, snippet(body))
}

// readAgentBody drains and bounds a response body for the AgentResp paths.
func readAgentBody(res *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}

func (a *AgentClient) BeforeSimulate(ctx context.Context, timestamp *time.Time, testSetID string, tcName string) error {
	if timestamp == nil || timestamp.IsZero() {
		a.logger.Debug("Skipping agent hook: timestamp is zero or nil")
		return nil
	}
	return a.callHook(ctx, "/hooks/before-simulate", models.BeforeSimulateRequest{
		TimeStamp:    *timestamp,
		TestSetID:    testSetID,
		TestCaseName: tcName,
	})
}

func (a *AgentClient) AfterSimulate(ctx context.Context, tcName string, testSetID string) error {
	return a.callHook(ctx, "/hooks/after-simulate", models.AfterSimulateRequest{
		TestSetID:    testSetID,
		TestCaseName: tcName,
	})
}

func (a *AgentClient) BeforeTestRun(ctx context.Context, testRunID string) error {
	return a.callHook(ctx, "/hooks/before-test-run", models.BeforeTestRunReq{
		TestRunID: testRunID,
	})
}

func (a *AgentClient) BeforeTestSetCompose(ctx context.Context, testRunID string, testSetID string, firstRun bool) error {
	// Made again on a restarted agent, it is no first run: what a first run
	// cleans up was cleaned up (see replay's repair, which re-registers the
	// same way). This set's hook is the one a restarted agent needs, whether
	// or not this agent answered it, so it replaces the previous set's.
	if a.session().recording() {
		a.session().record(stepTestSetHook, func(c context.Context) error { return a.BeforeTestSetCompose(c, testRunID, testSetID, false) })
	}
	return a.callHook(ctx, "/hooks/before-test-set-compose", models.BeforeTestSetCompose{
		TestRunID: testRunID,
		TestSetID: testSetID,
	})
}

func (a *AgentClient) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {
	return a.callHook(ctx, "/hooks/after-test-run", models.AfterTestRunReq{
		TestRunID:  testRunID,
		TestSetIDs: testSetIDs,
		Coverage:   coverage,
	})
}

// errAgentExited is the cause a hook call ends with when the agent this client
// started exits before it answers (whileAgentRuns).
var errAgentExited = errors.New("keploy's agent exited")

// hookNoticeAfter is how long a hook call goes unanswered before keploy says it
// is waiting for the agent, and hookStillWaitingEvery how often it says so
// again. Notices only: neither ends the wait. Variables for tests.
var (
	hookNoticeAfter       = 5 * time.Second
	hookStillWaitingEvery = 30 * time.Second
)

// callHook posts payload to the agent's hook endpoint and waits for its answer
// for as long as the agent serves it: until it answers, the agent this client
// started exits (whileAgentRuns), the connection ends (an agent that dies
// closes it; a host that is gone stops answering its keepalives), or ctx ends
// (the run is stopped, or the test set's app exited). There is no deadline: on
// a starved host a hook takes what it takes, and an agent that is alive but
// never answers is for the user or the job's own limit to stop, as an app that
// never answers a test is; while it waits keploy says so. The 50 s deadline
// there was returned nil while the agent still served the call, so the run went
// on as if the hook were done, and the next hook call overlapped it. A call
// that ends without an answer is an error, as a refusal is.
func (a *AgentClient) callHook(ctx context.Context, endpoint string, payload any) error {
	if a.conf.Agent.AgentURI == "" {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	hctx, stop := a.whileAgentRuns(ctx)
	defer stop()
	req, err := http.NewRequestWithContext(hctx, http.MethodPost, a.conf.Agent.AgentURI+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	ended := make(chan bool, 1)
	go a.sayHookWait(endpoint, ended, hookNoticeAfter, hookStillWaitingEvery)
	resp, err := a.client.Do(req)
	ended <- err == nil
	if err != nil {
		if errors.Is(context.Cause(hctx), errAgentExited) {
			return fmt.Errorf("%w before it answered %s", errAgentExited, endpoint)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("stopped waiting for keploy's agent to answer %s: %w", endpoint, context.Cause(ctx))
		}
		return fmt.Errorf("keploy's agent did not answer %s: %w", endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if b := strings.TrimSpace(string(respBody)); b != "" {
			return fmt.Errorf("agent hook failed: %d: %s", resp.StatusCode, b)
		}
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil
}

// sayHookWait says, while a hook call waits for the agent's answer (until
// ended gets whether an answer came), that it is waiting: once after
// noticeAfter, then every stillEvery, and once more when an answer comes
// after that. A call that ended without one says why itself.
func (a *AgentClient) sayHookWait(endpoint string, ended <-chan bool, noticeAfter, stillEvery time.Duration) {
	began := time.Now()
	notice := time.NewTimer(noticeAfter)
	defer notice.Stop()
	noticed := false
	for {
		select {
		case answered := <-ended:
			if noticed && answered {
				a.logger.Info("keploy's agent answered the hook", zap.String("endpoint", endpoint), zap.Duration("after", time.Since(began)))
			}
			return
		case <-notice.C:
			if !noticed {
				a.logger.Info("waiting for keploy's agent to answer a hook; the run goes on once it has", zap.String("endpoint", endpoint))
			} else {
				a.logger.Info("still waiting for keploy's agent to answer a hook", zap.String("endpoint", endpoint), zap.Duration("waited", time.Since(began)))
			}
			noticed = true
			notice.Reset(stillEvery)
		}
	}
}

// whileAgentRuns is ctx, ended with errAgentExited as its cause once the agent
// this client started (currentRun) has exited; ctx itself when it started
// none, as for an agent in a Kubernetes pod. stop releases it.
func (a *AgentClient) whileAgentRuns(ctx context.Context) (context.Context, func()) {
	run := a.currentRun()
	if run == nil {
		return ctx, func() {}
	}
	hctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		select {
		case <-run.gone:
			cancel(errAgentExited)
		case <-done:
		}
	}()
	return hctx, func() {
		close(done)
		cancel(nil)
	}
}

// StoreMocks sends the mock corpus to the agent as one stream (a gob
// MockStreamHeader over an io.Pipe, then filtered then unfiltered mocks, one gob
// frame each): the agent never buffers the whole corpus (#4327), and neither
// does this side. A mock whose payload the replay's read left in its mock file
// (models.Mock.LeavePayloadOnDisk) is read back as it is encoded, and dropped
// once it is (models.Mock.WithPayload), so the corpus is never whole in memory
// here either. A payload that cannot be read back fails the call, and the
// agent, whose stream ends early, keeps the mocks it held.
func (a *AgentClient) StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) (retErr error) {
	// The callers strip what the agent does not hold (models.AgentBound) at
	// the source, so their counts agree with what is sent; this is the same
	// rule again, for a caller that did not.
	filtered, unFiltered = models.AgentBound(filtered), models.AgentBound(unFiltered)
	defer func() {
		if retErr == nil {
			a.session().record(stepStoreMocks, func(c context.Context) error { return a.StoreMocks(c, filtered, unFiltered) })
		}
	}()
	a.consumedPerTest.Store(false)
	a.concurrentServing.Store(false)
	a.mockWindows.reset()
	// A dedicated cancelable context for the stream request: cancelling it tears
	// down the transport's read of the io.Pipe body, which unblocks the encoder
	// goroutine's pw.Write if the agent responded before consuming the whole
	// stream, so that goroutine can't leak.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	encoding := a.storeMocksEncoding(ctx)
	start := time.Now()
	res, sent, err := a.storeMocksStream(streamCtx, filtered, unFiltered, encoding)
	if err != nil {
		if sendErr := sent.failure(); sendErr != nil {
			return sendErr
		}
		return err
	}
	defer res.Body.Close()
	respErr := decodeStoreMocksResp(res)
	// The agent answers a stream that ended early with an error of its
	// own; the reason it ended is this side's.
	if sendErr := sent.failure(); sendErr != nil {
		return sendErr
	}
	if respErr != nil {
		return respErr
	}
	a.concurrentServing.Store(res.Header.Get(models.ExactConcurrentHeader) == models.ExactConcurrentServes)
	a.logger.Info("stored the mocks on the agent",
		zap.Int("mocks", len(filtered)+len(unFiltered)),
		// How many of them were read back from their mock file as they were
		// sent: a set read whole has none.
		zap.Int64("payloadsReadBack", sent.readBack.Load()),
		zap.Int64("bytes", sent.raw.Load()),
		zap.Int64("sentBytes", sent.wire.Load()),
		zap.String("encoding", encodingName(encoding)),
		zap.Duration("took", time.Since(start)))
	return nil
}

// storeMocksEncodingProbeTimeout bounds the /health request that asks the agent
// whether its /storemocks takes a compressed stream. Running out of it is not an
// error: the mocks then go uncompressed, as to an agent that cannot decompress.
// It is long because the agent can be a port-forward away over a slow link,
// where a new connection alone costs several round trips.
const storeMocksEncodingProbeTimeout = 30 * time.Second

// storeMocksEncoding returns the Content-Encoding to send the mock stream with:
// zstd when the agent's /health advertises it (Accept-Encoding, RFC 7694), and
// "" (uncompressed) otherwise, including when the agent cannot be asked. An
// agent that predates compression has no such header, so it keeps getting the
// stream it can read.
func (a *AgentClient) storeMocksEncoding(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, storeMocksEncodingProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/health", a.conf.Agent.AgentURI), nil)
	if err != nil {
		return ""
	}
	res, err := a.client.Do(req)
	if err != nil {
		a.logger.Debug("could not ask the agent whether it takes compressed mocks; sending them uncompressed", zap.Error(err))
		return ""
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode == http.StatusOK && acceptsEncoding(res.Header.Values("Accept-Encoding"), models.MockStreamEncodingZstd) {
		return models.MockStreamEncodingZstd
	}
	return ""
}

// acceptsEncoding reports whether Accept-Encoding header values list coding,
// ignoring case, spaces and parameters. A coding listed with q=0 is refused
// (RFC 9110, section 12.5.3).
func acceptsEncoding(values []string, coding string) bool {
	for _, v := range values {
		for _, item := range strings.Split(v, ",") {
			name, params, _ := strings.Cut(item, ";")
			if !strings.EqualFold(strings.TrimSpace(name), coding) {
				continue
			}
			for _, p := range strings.Split(params, ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(k, "q") {
					if q, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil && q == 0 {
						return false
					}
				}
			}
			return true
		}
	}
	return false
}

func encodingName(encoding string) string {
	if encoding == "" {
		return "identity"
	}
	return encoding
}

// mockStreamBytes counts a /storemocks stream before compression (raw) and as
// sent (wire), and keeps why the encoder could not read a payload back (err).
// The encoder goroutine writes them while the caller may read them, hence
// atomics.
type mockStreamBytes struct {
	raw  atomic.Int64
	wire atomic.Int64
	err  atomic.Pointer[error]
	// readBack counts the mocks whose payloads were read back from their
	// mock file as they were encoded (models.Mock.WithPayload).
	readBack atomic.Int64
}

// failure is why the encoder ended the stream before its last mock when the
// reason is this side's: a payload it could not read back. nil otherwise.
func (s *mockStreamBytes) failure() error {
	if s == nil {
		return nil
	}
	if p := s.err.Load(); p != nil {
		return *p
	}
	return nil
}

// countingWriter adds what it writes to n.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// storeMocksStream POSTs the corpus as a gob stream (MockStreamHeader + one
// frame per mock), zstd-compressed when encoding says so, and returns the raw
// response. Each mock whose payload is on disk is read back as it is encoded.
func (a *AgentClient) storeMocksStream(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock, encoding string) (*http.Response, *mockStreamBytes, error) {
	pr, pw := io.Pipe()
	sent := &mockStreamBytes{}
	go func() {
		var out io.Writer = countingWriter{w: pw, n: &sent.wire}
		var zw *zstd.Encoder
		if encoding == models.MockStreamEncodingZstd {
			var err error
			// One encoder goroutine: the link, not the CPU, bounds a remote
			// upload, and a local one is fast either way. SpeedFastest, not
			// SpeedDefault: a k8s-proxy pod's replayer is held to one CPU,
			// and there SpeedFastest cut zstd's CPU by 0.6 to 2.5 seconds
			// per 0.52 GB of gob (six interleaved runs), for 2.4% more bytes
			// on the wire (191.05 MB against 186.56 MB).
			zw, err = zstd.NewWriter(out,
				zstd.WithEncoderLevel(zstd.SpeedFastest),
				zstd.WithWindowSize(models.MockStreamZstdWindow),
				zstd.WithEncoderConcurrency(1))
			if err != nil {
				_ = pw.CloseWithError(fmt.Errorf("start the zstd encoder: %w", err))
				return
			}
			out = zw
		}
		// The pipe is closed first, so the encoder's Close writes nothing that
		// could block and only releases its resources.
		fail := func(err error) {
			_ = pw.CloseWithError(err)
			if zw != nil {
				_ = zw.Close()
			}
		}
		enc := gob.NewEncoder(countingWriter{w: out, n: &sent.raw})
		if err := enc.Encode(models.MockStreamHeader{
			FilteredCount:   len(filtered),
			UnfilteredCount: len(unFiltered),
		}); err != nil {
			fail(err)
			return
		}
		i := 0
		encodeAll := func(mocks []*models.Mock) error {
			for _, m := range mocks {
				if i%1024 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				i++
				if m.PayloadOnDisk() {
					sent.readBack.Add(1)
				}
				whole, err := m.WithPayload(ctx)
				if err != nil {
					// Kept for the caller: the agent sees only a stream that
					// ended early.
					err = fmt.Errorf("failed to send the mocks to the agent: %w", err)
					sent.err.Store(&err)
					return err
				}
				if err := enc.Encode(whole); err != nil {
					return err
				}
			}
			return nil
		}
		if err := encodeAll(filtered); err != nil {
			fail(err)
			return
		}
		if err := encodeAll(unFiltered); err != nil {
			fail(err)
			return
		}
		if zw != nil {
			// Flushes the last block and ends the zstd frame.
			if err := zw.Close(); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		_ = pw.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/storemocks", a.conf.Agent.AgentURI), pr)
	if err != nil {
		// The transport never took ownership of pr, so nothing else will ever
		// close it — close the pipe so the encoder goroutine's blocked pw.Write
		// returns instead of leaking. (Do() closes req.Body itself on its own
		// error path per the RoundTripper contract, but here Do was never
		// reached.)
		_ = pw.CloseWithError(err)
		utils.LogError(a.logger, err, "failed to create request for storemocks")
		return nil, nil, fmt.Errorf("create request for storemocks: %s", err.Error())
	}
	req.Header.Set("Content-Type", models.StoreMocksStreamContentType)
	req.Header.Set("Accept", "application/x-gob")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}

	res, err := a.client.Do(req)
	if err != nil {
		return nil, sent, fmt.Errorf("send request for storemocks: %s", err.Error())
	}
	return res, sent, nil
}

// maxStoreMocksFailBody bounds what of a failed /storemocks reply is read to
// tell whether it is the pre-stream agent's, which is one type definition of
// some 50 bytes. A longer reply is still decoded, but is not taken for it.
const maxStoreMocksFailBody = 64 << 10

// agentGobMediaType is the media type of an agent's gob replies.
const agentGobMediaType = "application/x-gob"

// preStreamAgentReply reports whether a failed /storemocks reply is the one an
// agent from before the mock stream gives: under the agent's gob media type,
// one or more whole gob messages that each define a type, and nothing after
// them, so the decoder read them all and then met the end where the value
// should be (decodeErr is io.ErrUnexpectedEOF). That agent's encoder sends the
// AgentResp type whole and fails on the value's error, which it cannot encode.
func preStreamAgentReply(contentType string, body []byte, decodeErr error) bool {
	if !errors.Is(decodeErr, io.ErrUnexpectedEOF) {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != agentGobMediaType || len(body) > maxStoreMocksFailBody {
		return false
	}
	return gobTypeDefinitionsOnly(body)
}

// gobTypeDefinitionsOnly reports whether b is one or more whole gob messages
// (a byte count, then that many bytes), each opening with a negative type id,
// which marks a type definition, and ends where the last one ends. A value's
// message opens with its type's positive id.
func gobTypeDefinitionsOnly(b []byte) bool {
	messages := 0
	for len(b) > 0 {
		size, rest, ok := gobUint(b)
		if !ok || size > uint64(len(rest)) {
			return false
		}
		// An empty message has no type id, and fails here.
		id, _, ok := gobUint(rest[:size])
		// gob sends a signed int as a uint whose low bit is the sign: an
		// odd one is negative.
		if !ok || id&1 == 0 {
			return false
		}
		b = rest[size:]
		messages++
	}
	return messages > 0
}

// gobUint reads one gob unsigned integer off the front of b: a byte below 128
// is its value, and a higher one is minus the count (at most 8) of the
// big-endian bytes that follow and hold it.
func gobUint(b []byte) (uint64, []byte, bool) {
	if len(b) == 0 {
		return 0, nil, false
	}
	if b[0] < 0x80 {
		return uint64(b[0]), b[1:], true
	}
	n := -int(int8(b[0]))
	if n > 8 || len(b) < 1+n {
		return 0, nil, false
	}
	var x uint64
	for _, c := range b[1 : 1+n] {
		x = x<<8 | uint64(c)
	}
	return x, b[1+n:], true
}

// decodeStoreMocksResp reads the agent's single AgentResp gob reply.
func decodeStoreMocksResp(res *http.Response) error {
	// Non-2xx? Try to decode anyway; if that fails, return status text.
	// An OLD agent reaches the decode failure here on every error: its gob
	// encode aborted on the `error` interface field, having already written
	// the AgentResp type's definition but none of the value, so the decode
	// fails with "unexpected EOF" rather than on an empty body — hence the bare
	// "storemocks http 500" this used to give. Unchanged for an old agent;
	// a new one now carries its reason in ErrorMsg.
	//
	// A 400 whose reply is that type with no value is the agent from before
	// the stream: it fails to read the stream's header on every call. The
	// call is exact, since a caller may take the error as permanent and stop
	// retrying the agent: the reply is application/x-gob, as every agent's
	// /storemocks sets it, and it holds whole type definitions and then ends,
	// where the value would be (preStreamAgentReply). A text 400, from a
	// proxy or middleware in front of the agent (http.Error) or from net/http
	// itself, keeps the plain error: a short one fails with
	// io.ErrUnexpectedEOF too, since gob reads its first byte as a message
	// length. An empty 400 and a reply cut short keep it too. An agent that
	// reads the stream but not its reason in ErrorMsg (keploy v3.5.85 to v3.6.61)
	// sends the same type with no value when its service fails, as a 500,
	// which the next call may pass; it answers 400 only to a header it cannot
	// read, which this side sends compressed only to an agent that says it
	// reads zstd (v3.6.99 on).
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(res.Body, maxStoreMocksFailBody+1))
		var fail models.AgentResp
		// The decode goes on past the bound, so a reason of any length is
		// still read; the bytes past it are not judged.
		if err := gob.NewDecoder(io.MultiReader(bytes.NewReader(body), res.Body)).Decode(&fail); err != nil {
			if res.StatusCode == http.StatusBadRequest && readErr == nil && preStreamAgentReply(res.Header.Get("Content-Type"), body, err) {
				return fmt.Errorf("storemocks http %d: %w", res.StatusCode, models.ErrAgentPredatesMockStream)
			}
			return fmt.Errorf("storemocks http %d", res.StatusCode)
		}
		if msg := fail.ErrorMsg; msg != "" {
			return fmt.Errorf("storemocks failed (status %d): %s", res.StatusCode, msg)
		}
		return fmt.Errorf("storemocks http %d", res.StatusCode)
	}

	var mockResp models.AgentResp
	if err := gob.NewDecoder(res.Body).Decode(&mockResp); err != nil {
		return fmt.Errorf("decode gob response for storemocks: %s", err.Error())
	}
	if err := mockResp.Err(); err != nil {
		return fmt.Errorf("storemocks failed: %w", err)
	}
	// IsSuccess is checked too, so this path cannot disagree with
	// agentRespErr about what a failure is. A 2xx carrying IsSuccess:false
	// and no message is not produced by any agent -- respondAgent derives
	// both from the same error, and the pre-rename agent's storemocks
	// success wrote IsSuccess:true just the same -- so this rejects nothing
	// real today and stops a 200-shaped failure reading as success.
	if !mockResp.IsSuccess {
		return fmt.Errorf("storemocks failed: agent reported failure with status %d and no message", res.StatusCode)
	}
	return nil
}

func (a *AgentClient) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	_, err := a.updateMockParams(ctx, params)
	return err
}

// updateMockParams is UpdateMockParams, with the headers of the agent's
// answer to this call (nil for a call that fails).
//
// A filter whose consumed history is a models.ConsumedHistory carries, to an
// agent that holds an earlier state of it (models.ConsumedSync), only the
// entries written since: a replay no longer sends, and the agent no longer
// decodes, everything its tests consumed so far before every test.
func (a *AgentClient) updateMockParams(ctx context.Context, params models.MockFilterParams) (_ http.Header, retErr error) {
	defer func() {
		if retErr == nil && a.session().recording() {
			recorded, rebuild := copyMockParams(params)
			a.session().record(mockParamsStep(recorded), func(c context.Context) error { return a.UpdateMockParams(c, rebuild(recorded)) })
		}
	}()
	// What the agent's last answer said decides how this call carries the
	// consumed history; it is read before the answer to this call replaces
	// it.
	perTest := a.consumedPerTest.Load()
	if params.ConsumedHistory != nil {
		a.consumed.mu.Lock()
		defer a.consumed.mu.Unlock()
	}
	hdr, resend, err := a.sendMockParams(ctx, params, perTest)
	if resend {
		// The agent does not hold the state this send built on (an agent
		// started again): the same filter, with the whole history, as the
		// send that was refused left the client believing the agent holds
		// none of it.
		hdr, _, err = a.sendMockParams(ctx, params, perTest)
	}
	return hdr, err
}

// sendMockParams makes one /updatemockparams call for params. resend reports
// a call the agent did not apply as asked because it does not hold the
// consumed history the send built on: the caller sends it again, which then
// carries the history whole.
func (a *AgentClient) sendMockParams(ctx context.Context, params models.MockFilterParams, perTest bool) (_ http.Header, resend bool, _ error) {
	// Only a successful answer that says so sets it again below; a call that
	// fails at any point leaves the client believing nothing.
	a.consumedPerTest.Store(false)
	// Nor does it believe a staging's background rules (stagedBackground)
	// until the agent's answer to it says them.
	if ex := params.Exact; ex != nil && ex.Stage {
		none := ""
		a.stagedBackground.Store(&none)
	}
	requestBody, send := a.consumed.request(params, perTest)
	if send.sync != nil {
		// Until the answer says what the agent holds, it is believed to
		// hold nothing: a call that fails, or is refused, leaves the next
		// send whole.
		a.consumed.forget()
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for updatemockparams")
		return nil, false, fmt.Errorf("error marshaling request body for updatemockparams: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/updatemockparams", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for updatemockparams")
		return nil, false, fmt.Errorf("error creating request for update mock params: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("failed to send request for updatemockparams: %s", err.Error())
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		if err := res.Body.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to close response body for updatemockparams")
		}
	}()

	rawBody, readErr := readAgentBody(res)
	if readErr != nil {
		return nil, false, fmt.Errorf("failed to read response body for updatemockparams: %s", readErr.Error())
	}

	if send.sync != nil && !send.sync.Full && res.StatusCode == http.StatusConflict && res.Header.Get(models.ConsumedSyncHeader) == models.ConsumedSyncResync {
		return nil, true, nil
	}
	if err := agentRespErr("update mock params", res, rawBody); err != nil {
		return nil, false, err
	}
	if !a.consumed.answered(send, res.Header.Get(models.ConsumedSyncHeader)) {
		// An agent that keeps no history filtered with the entries since
		// alone (another binary answers at the address now).
		return nil, true, nil
	}
	a.consumedPerTest.Store(res.Header.Get(models.ConsumedScopeHeader) == models.ConsumedScopePerTest)
	a.noteAgentVersion(res.Header)
	a.mockWindows.note(res.Header.Get(models.MockWindowHeader))
	if ex := params.Exact; ex != nil && ex.Stage {
		rules := res.Header.Get(models.BackgroundRulesHeader)
		a.stagedBackground.Store(&rules)
	}
	return res.Header, false, nil
}

func (a *AgentClient) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	// Create the URL with query parameters
	url := fmt.Sprintf("%s/consumedmocks", a.conf.Agent.AgentURI)
	// Create a new GET request with the query parameter
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %s", err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request for mockOutgoing: %s", err.Error())
	}

	defer func() {
		err := res.Body.Close()
		if err != nil {
			utils.LogError(a.logger, err, "failed to close response body for getconsumedmocks")
		}
	}()

	// Status BEFORE decode. The agent answers a failure with an error
	// OBJECT, and decoding that straight into the slice produced
	// "json: cannot unmarshal object into Go value of type
	// []models.MockState" — the decoder's complaint about the shape,
	// never the agent's reason.
	if res.StatusCode != http.StatusOK {
		rawBody, _ := readAgentBody(res)
		return nil, agentRespErr("get consumed mocks", res, rawBody)
	}

	var consumedMocks []models.MockState
	if err := json.NewDecoder(res.Body).Decode(&consumedMocks); err != nil {
		return nil, fmt.Errorf("failed to decode response body for getconsumedmocks: %s", err.Error())
	}

	return consumedMocks, nil
}

// GetServedMocks returns the mocks served so far this session, keyed by name.
//
// Unlike GetConsumedMocks this is safe to call repeatedly while the run is in
// flight: the agent answers from its never-drained map, so polling it does not
// consume the state that the end-of-run outcome report and --strict depend on.
func (a *AgentClient) GetServedMocks(ctx context.Context) (map[string]models.MockState, error) {
	url := fmt.Sprintf("%s/mock/served", a.conf.Agent.AgentURI)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %s", err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request for served mocks: %s", err.Error())
	}

	defer func() {
		if err := res.Body.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to close response body for getservedmocks")
		}
	}()

	// Status before decode, for the reason spelled out in GetConsumedMocks: a
	// failure arrives as an error object and decoding it into the map would
	// report the decoder's confusion instead of the agent's reason.
	if res.StatusCode != http.StatusOK {
		rawBody, _ := readAgentBody(res)
		return nil, agentRespErr("get served mocks", res, rawBody)
	}

	served := map[string]models.MockState{}
	if err := json.NewDecoder(res.Body).Decode(&served); err != nil {
		return nil, fmt.Errorf("failed to decode response body for getservedmocks: %s", err.Error())
	}

	return served, nil
}

// GetMockStats fetches the agent's non-draining mock-session snapshot
// (GET /mock/stats): the number of mocks stored on this agent process, plus
// the running consumed/missed totals. The replay setup calls it after the
// docker-compose bring-up to verify the agent still holds what the session
// stored before any test fires: a replacement agent (one compose started again
// in the bring-up retry, had it not been set up again) reports a loaded count
// of zero.
func (a *AgentClient) GetMockStats(ctx context.Context) (models.MockStats, error) {
	url := fmt.Sprintf("%s/mock/stats", a.conf.Agent.AgentURI)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return models.MockStats{}, fmt.Errorf("failed to create request: %s", err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return models.MockStats{}, fmt.Errorf("failed to send request for mock stats: %s", err.Error())
	}

	defer func() {
		if err := res.Body.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to close response body for getmockstats")
		}
	}()

	// Status before decode, for the same reason as GetServedMocks: a failure
	// arrives as an error object and decoding it into the struct would report
	// the decoder's confusion instead of the agent's reason.
	// An agent that CANNOT answer is not an agent that answered "nothing
	// stored". 404 is an agent older than this route; 501 is one whose service
	// does not implement the reader. Both are reported as a distinct, typed
	// condition so the caller can skip its check instead of concluding the
	// agent was replaced — otherwise ordinary version skew fails every
	// docker-compose test set.
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusNotImplemented {
		return models.MockStats{}, fmt.Errorf("%w: agent returned %d", models.ErrMockStatsUnsupported, res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		rawBody, _ := readAgentBody(res)
		return models.MockStats{}, agentRespErr("get mock stats", res, rawBody)
	}

	var stats models.MockStats
	if err := json.NewDecoder(res.Body).Decode(&stats); err != nil {
		return models.MockStats{}, fmt.Errorf("failed to decode response body for getmockstats: %s", err.Error())
	}

	return stats, nil
}

// GetPIDMapRefusals fetches GET /pidmaps: the inserts into the agent's PID maps
// the kernel has refused since its hooks loaded, each of them a process they
// could not track (models.PIDMapRefusals). An agent older than the
// route (404) or one that cannot count them (501) gives
// models.ErrPIDMapRefusalsUnsupported, which is "unknown", not "none".
func (a *AgentClient) GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error) {
	url := fmt.Sprintf("%s/pidmaps", a.conf.Agent.AgentURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return models.PIDMapRefusals{}, fmt.Errorf("failed to create request: %w", err)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return models.PIDMapRefusals{}, fmt.Errorf("failed to ask the agent for refused PID map inserts: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to close response body for pidmaps")
		}
	}()
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusNotImplemented {
		return models.PIDMapRefusals{}, fmt.Errorf("%w: agent returned %d", models.ErrPIDMapRefusalsUnsupported, res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		rawBody, _ := readAgentBody(res)
		return models.PIDMapRefusals{}, agentRespErr("get refused PID map inserts", res, rawBody)
	}
	var refusals models.PIDMapRefusals
	if err := json.NewDecoder(res.Body).Decode(&refusals); err != nil {
		return models.PIDMapRefusals{}, fmt.Errorf("failed to decode the refused PID map inserts: %w", err)
	}
	return refusals, nil
}

// GetCaptureLosses fetches GET /capturelosses: every kind of capture loss the
// agent has counted since it started (models.CaptureLosses). An agent older
// than the route (404) or one that cannot count (501) gives
// models.ErrCaptureLossesUnsupported, which is "unknown", not "none".
func (a *AgentClient) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	losses, _, err := a.GetCaptureLossesAndAgent(ctx)
	return losses, err
}

// GetCaptureLossesAndAgent is GetCaptureLosses, and the agent process that
// answered (models.AgentInstanceHeader): its counts are that process's, from
// its start, and an agent started again in its place counts afresh. "" where
// the answer names none (an agent with the switch off, or older than the
// header).
//
// The read ends at the agent's answer, the end of ctx, the interrupt that
// gives a stop's waits up, or once keploy had captureLossStall of its own
// runnable time with no answer (boundedAgentRead): an error then, never a
// count.
func (a *AgentClient) GetCaptureLossesAndAgent(ctx context.Context) (models.CaptureLosses, string, error) {
	got, err := boundedAgentRead(ctx, "the read of the agent's capture losses", func(ctx context.Context) (lossesAnswer, error) {
		losses, agent, err := a.getCaptureLossesAndAgent(ctx)
		return lossesAnswer{losses, agent}, err
	})
	return got.losses, got.agent, err
}

// lossesAnswer is an answer of GET /capturelosses: the counts, and the agent
// process that gave them.
type lossesAnswer struct {
	losses models.CaptureLosses
	agent  string
}

// getCaptureLossesAndAgent is GetCaptureLossesAndAgent's exchange, unbounded.
func (a *AgentClient) getCaptureLossesAndAgent(ctx context.Context) (models.CaptureLosses, string, error) {
	url := fmt.Sprintf("%s/capturelosses", a.conf.Agent.AgentURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request: %w", err)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to ask the agent for its capture losses: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to close response body for capturelosses")
		}
	}()
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusNotImplemented {
		return nil, "", fmt.Errorf("%w: agent returned %d", models.ErrCaptureLossesUnsupported, res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		rawBody, _ := readAgentBody(res)
		return nil, "", agentRespErr("get capture losses", res, rawBody)
	}
	var losses models.CaptureLosses
	if err := json.NewDecoder(res.Body).Decode(&losses); err != nil {
		return nil, "", fmt.Errorf("failed to decode the capture losses: %w", err)
	}
	return losses, res.Header.Get(models.AgentInstanceHeader), nil
}

// GetUnchargedCaptureLosses asks the agent what it could not capture since
// the run began that no test was charged with: what was lost after the last
// test ended (GET /agent/capturelosses/uncharged). An agent that cannot say
// is ErrCaptureLossesUnsupported. The read is bounded as
// GetCaptureLossesAndAgent's is (boundedAgentRead).
func (a *AgentClient) GetUnchargedCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	return boundedAgentRead(ctx, "the read of the agent's capture losses no test was charged with", a.getUnchargedCaptureLosses)
}

// getUnchargedCaptureLosses is GetUnchargedCaptureLosses's exchange,
// unbounded.
func (a *AgentClient) getUnchargedCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	url := fmt.Sprintf("%s/capturelosses/uncharged", a.conf.Agent.AgentURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to ask the agent for the capture losses no test was charged with: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to close response body for capturelosses/uncharged")
		}
	}()
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusNotImplemented {
		return nil, fmt.Errorf("%w: agent returned %d", models.ErrCaptureLossesUnsupported, res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		rawBody, _ := readAgentBody(res)
		return nil, agentRespErr("get the capture losses no test was charged with", res, rawBody)
	}
	var losses models.CaptureLosses
	if err := json.NewDecoder(res.Body).Decode(&losses); err != nil {
		return nil, fmt.Errorf("failed to decode the capture losses no test was charged with: %w", err)
	}
	return losses, nil
}

// captureLossStall is how long, in time keploy could run (utils/stall), a
// read of the agent's capture losses may go with no answer before keploy
// gives it up: stall.HangBudget, as for a read of a container's
// configuration the Docker daemon does not answer. The agent client's
// http.Client has no timeout of its own, and a wedged agent that took the
// read and never answered it held the run, and a CI job with it, for good:
// record's per-test read is made on a context the stop does not end. A
// starved keploy accrues it only as fast as it gets to run, so load is never
// taken for a silent agent. captureLossClock is the clock it is counted on
// (nil: keploy's own runnable time); tests drive one of their own.
var (
	captureLossStall = stall.HangBudget
	captureLossClock stall.Clock
)

// errAgentSilent is a read of the agent given up on: it made no answer while
// keploy had the read's whole bound of its own time to run.
var errAgentSilent = errors.New("the keploy agent made no answer")

// boundedAgentRead is read, an exchange with the agent, made on a context it
// ends. It returns read's answer, or an error, never an answer, when one of
// these comes first: ctx ends; the interrupt that gives a stop's waits up
// (utils.StopNow, "press Ctrl+C again to stop now": the Ctrl+C escape of a
// read made on a context the stop does not end, as record's per-test read
// is); or keploy had captureLossStall of its own runnable time with no
// answer (errAgentSilent; what names the read). The count it would have
// given is then unknown, and every caller fails closed on an error: no
// count, no verdict that relies on one. A read given up on ends as its
// context does, on its own goroutine.
func boundedAgentRead[T any](ctx context.Context, what string, read func(context.Context) (T, error)) (T, error) {
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type answer struct {
		v   T
		err error
	}
	got := make(chan answer, 1) // a read given up on does not block
	go func() {
		v, err := read(readCtx)
		got <- answer{v, err}
	}()
	m := stall.NewMeter(captureLossClock, captureLossStall)
	defer m.Stop()
	var zero T
	for {
		select {
		case a := <-got:
			return a.v, a.err
		case <-ctx.Done():
			return zero, fmt.Errorf("%s ended: %w", what, ctx.Err())
		case <-utils.StopNow():
			return zero, fmt.Errorf("keploy gave up %s at the interrupt", what)
		case <-m.Wait():
			if m.Expired() {
				return zero, fmt.Errorf("%w to %s while keploy had %s of its own time to run", errAgentSilent, what, m.Grace())
			}
		}
	}
}

func (a *AgentClient) Run(ctx context.Context, _ models.RunOptions) (ret models.AppError) {
	// The agent's container this run is to create, under docker compose: a
	// release that waits for it ends once the run is over without it
	// (containerExited). Its end is noted last, once Run returns, whichever
	// way it ends: after the app's run has returned (and with it compose's up
	// and the down after it), or before the app's run began. Both are noted
	// on this client's entry of the container, not by its name, which
	// another client's Setup may have drawn too.
	a.mu.Lock()
	container := a.agentContainer
	a.mu.Unlock()
	container.noteRun(false)
	defer container.noteRun(true)
	app, err := a.getApp()
	if err != nil {
		utils.LogError(a.logger, err, "failed to get app while running")
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	}
	// Once this run of the application is over, no stream waits for its
	// agent to come back, and what was recorded for it is let go.
	defer a.session().end()

	runAppErrGrp, runAppCtx := errgroup.WithContext(ctx)
	appErrCh := make(chan models.AppError, 1)
	// stopKilled is why the stop at a test set's end, or at a recording's
	// end, had to kill the app (models.AppError.StopKilled), and leftRunning
	// what of its daemons the stop left running (models.AppError.LeftRunning),
	// known once the app's run has returned.
	var stopKilled, leftRunning string
	defer func() {
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(a.logger, err, "failed to stop the app")
		}
		if ret.AppErrorType == models.ErrCtxCanceled && stopKilled != "" {
			ret.StopKilled = stopKilled
		}
		// On a run that ended with a type (a stop's, an exit's): a clean
		// return carries no error to read it from. Not a stop's alone: an
		// application that exited on its own has its daemons stopped too,
		// and its set's end says what of them was left (the set is not
		// judged on its tests then, and no set follows it).
		if ret.AppErrorType != "" {
			ret.LeftRunning = leftRunning
		}
	}()

	runAppErrGrp.Go(func() error {
		defer utils.Recover(a.logger)
		appErr := app.Run(runAppCtx)
		stopKilled = appErr.StopKilled
		// The daemons the app left are part of its run, and end with it
		// (adopt_linux.go): none is judged in the next test set, and the app
		// started again can bind their ports. An app whose shell died on
		// keploy's interrupt at once (`app & wait`) is one of them, still
		// stopping, and is not asked again when it acts on the interrupt.
		// When the stop at a set's end had to kill one that made no
		// progress on its way out, the run says so, as for the app.
		why, left := stopAdopted(a.logger, app.TakeStopReached(), utils.TeardownScopeOf(runAppCtx))
		leftRunning = left
		if why != "" && stopKilled == "" {
			switch {
			case errors.Is(context.Cause(ctx), models.ErrAppStoppedForSetEnd):
				stopKilled = "keploy interrupted the application at the set's end, and " + why
			case ctx.Err() != nil && app.StopsARecording():
				// The recording lacks what it would have done as it stopped
				// (P101-b-k).
				stopKilled = "keploy stopped the application at the recording's end, and " + why
			}
		}
		if appErr.Err != nil {
			utils.LogError(a.logger, appErr.Err, "error while running the app")
		}
		if appErr.AppErrorType != models.ErrCtxCanceled && appErr != (models.AppError{}) {
			appErrCh <- appErr
		}
		return nil
	})

	select {
	case <-runAppCtx.Done():
		return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: nil}
	case appErr, ok := <-appErrCh:
		if !ok {
			return models.AppError{AppErrorType: models.ErrAppStopped, Err: nil}
		}
		return appErr
	}
}

func (a *AgentClient) GetRecentAppLogs(ctx context.Context) string {
	app, err := a.getApp()
	if err != nil {
		a.logger.Debug("failed to get app for recent logs", zap.Error(err))
		return ""
	}

	return app.RecentLogs(ctx)
}

// StopComposeApp stops the app's container of a docker compose stack, its
// stack (keploy's agent among it) running on, under the proven switch, and
// says why it had to kill it (app.App.StopComposeApp, P101-c).
func (a *AgentClient) StopComposeApp(ctx context.Context) (string, error) {
	app, err := a.getApp()
	if err != nil {
		return "", err
	}
	return app.StopComposeApp(ctx)
}

// StartComposeApp starts the app's container of a docker compose stack again
// after StopComposeApp (app.App.StartComposeApp, P101-c).
func (a *AgentClient) StartComposeApp(ctx context.Context) (models.ComposeAppStart, error) {
	app, err := a.getApp()
	if err != nil {
		return models.ComposeAppUnread, err
	}
	return app.StartComposeApp(ctx)
}

// startAgent starts the keploy agent process and handles its lifecycle.
//
// The returned channel receives the agent process's exit exactly once — nil
// for a clean exit — so the readiness wait in Setup can stop the moment the
// agent is gone instead of polling a dead port for the whole ready budget.
func (a *AgentClient) startAgent(ctx context.Context, isDockerCmd bool, opts models.SetupOptions) (<-chan error, error) {
	// Get the errgroup from context
	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return nil, fmt.Errorf("failed to get errorgroup from the context")
	}
	// Buffered: the exit is reported whether or not anyone is still waiting.
	exited := make(chan error, 1)

	agentCtx, cancel, run, err := a.beginRun(ctx)
	if err != nil {
		return nil, err
	}
	if a.conf.Record.Synchronous {
		opts.Synchronous = true
	}
	opts.ExtraArgs = agent.StartupAgentHook.GetArgs(ctx)
	if isDockerCmd {
		// This process's own privileges were checked before Setup changed
		// anything (checkDockerModePrivileges).
		// Start the agent in Docker container using errgroup
		grp.Go(func() error {
			defer cancel() // Cancel agent context when Docker agent stops
			err := a.startInDocker(agentCtx, a.logger, opts, run)
			// `docker run` has returned: the container has exited -- unless the
			// stop could not reach it (the engine refused `kill`, or sudo -n),
			// in which case cmd.Cancel killed the client and it may still run.
			run.exited()
			// `docker run` exits with the agent's own status, so this carries
			// the reason an agent that could not start gave for it.
			exited <- err
			if err != nil && !errors.Is(agentCtx.Err(), context.Canceled) {
				a.logger.Error("failed to start Docker agent", zap.Error(err))
				return err
			}
			return nil
		})
	} else {
		// Start the agent as a native process
		err := a.startNativeAgent(agentCtx, opts, exited, run)
		if err != nil {
			run.exited() // nothing was started, or nothing waits for it
			cancel()
			return nil, err
		}
	}
	a.startedAgent.Store(true)

	// Monitor agent process and cancel client context if agent stops using errgroup
	grp.Go(func() error {
		a.monitorAgent(ctx, agentCtx)
		return nil
	})

	return exited, nil
}

// nativeAgentArgs builds the `keploy agent` argv for a natively spawned agent,
// the control-plane token file included, and records the launch
// (token.RecordLaunch) so the client holds that agent to the token.
func (a *AgentClient) nativeAgentArgs(opts models.SetupOptions) []string {
	// Build args (binary is passed separately to utils)
	args := []string{
		"agent",
		"--port", strconv.Itoa(int(opts.AgentPort)),
		"--proxy-port", strconv.Itoa(int(opts.ProxyPort)),
		"--dns-port", strconv.Itoa(int(opts.DnsPort)),
		"--client-pid", strconv.Itoa(int(os.Getpid())),
		"--mode", string(opts.Mode),
	}
	// The pid namespace the pid is this process's in, so the agent takes it
	// only where it names this process (utils.ClientPIDHere).
	if ns := utils.OwnPIDNamespace(); ns != "" {
		args = append(args, "--client-pid-ns", ns)
	}
	// This run's log, for the agent to append its lines to: one run, one file,
	// which this process removes at its end (keployLog.ClientLogFileFlag).
	// Absolute, as a relative TMPDIR would make it.
	if utils.LogFile != nil {
		if p, err := filepath.Abs(utils.LogFile.Name()); err == nil {
			args = append(args, "--"+keployLog.ClientLogFileFlag+"="+p)
		}
	}

	extraArgs := opts.ExtraArgs
	if len(extraArgs) > 0 {
		args = append(args, extraArgs...)
	}
	// The CLI's root-level settings (--debug, --disable-tele, --disable-ansi,
	// --disable-mapping), each with its value: the native agent reads the same
	// keploy.yml (--config-path), and only an explicit value keeps the
	// flag > yaml > default the CLI resolved.
	args = append(args, agentargs.Root(a.conf)...)
	if a.conf.Record.Synchronous {
		args = append(args, "--sync")
	}
	if a.conf.Record.EnableSampling > 0 {
		args = append(args, fmt.Sprintf("--enable-sampling=%d", a.conf.Record.EnableSampling))
	}
	if opts.EnableTesting {
		args = append(args, "--enable-testing")
	}
	if opts.GlobalPassthrough {
		args = append(args, "--global-passthrough")
	}
	for _, e := range opts.AllowEgress {
		args = append(args, "--allow-egress="+e)
	}
	if opts.DisableHandshakeHold {
		args = append(args, "--disable-handshake-hold")
	}
	if opts.CapturePackets {
		args = append(args, "--capture-packets")
	}
	if opts.OpportunisticTLSIntercept {
		args = append(args, "--opportunistic-tls-intercept")
	}
	if opts.ChannelBindingShim {
		args = append(args, "--channel-binding-shim")
	}
	if opts.MockMode {
		args = append(args, "--mock-mode")
	}
	if opts.RecordRequests {
		args = append(args, "--record-requests")
	}
	if opts.SettleReads {
		args = append(args, "--settle-reads")
	}
	// The proven model's staging switch: sudo drops KEPLOY_PROVEN on the way to
	// the agent, so it travels as a flag.
	args = append(args, provenswitch.AgentArgs()...)
	// And the Docker endpoint the tests' containers run on, which sudo drops
	// too, so the agent can name container destinations (containerkey).
	args = append(args, containerkey.AgentArgs()...)
	// Upstream TLS verification. Forwarded UNCONDITIONALLY as =%t, the same
	// pattern (and for the same reason) as the root settings above: the
	// orchestrator has already applied flag > yaml > default, and the native
	// agent inherits the very same keploy.yml through --config-path below. If
	// we only sent the flag when it was true, an explicit
	// `--upstream-tls-verify=false` over a keploy.yml `verify: true` would be
	// indistinguishable from "not specified" and the agent would re-derive
	// true from the file — while the identical command under docker (no
	// keploy.yml in the container) would correctly record with verification
	// off. The CA path travels the same way so that clearing it is expressible
	// too; `--flag=value` keeps an empty or space-bearing path a single argv
	// element (no shell is involved on this path).
	args = append(args, fmt.Sprintf("--upstream-tls-verify=%t", opts.UpstreamTLSVerify))
	args = append(args, fmt.Sprintf("--upstream-tls-ca-cert=%s", opts.UpstreamTLSCACert))
	if opts.BuildDelay > 0 {
		args = append(args, "--build-delay", strconv.FormatUint(opts.BuildDelay, 10))
	}
	if len(opts.PassThroughPorts) > 0 {
		// Convert []uint32 to []string
		portStrings := make([]string, len(opts.PassThroughPorts))
		for i, port := range opts.PassThroughPorts {
			portStrings[i] = strconv.Itoa(int(port))
		}
		// Join them with a comma and add as a single argument
		args = append(args, "--pass-through-ports", strings.Join(portStrings, ","))
	}
	// Forward record-buffer tuning to the native agent. Only set when
	// non-zero so the agent's own keploy.yml / env-var / default
	// resolution still wins when the orchestrator hasn't overridden.
	if opts.RecordBufferMaxMemoryPerConn > 0 {
		args = append(args, "--max-memory-per-conn", strconv.FormatUint(opts.RecordBufferMaxMemoryPerConn, 10))
	}
	if opts.RecordBufferQueueSize > 0 {
		args = append(args, "--queue-size", strconv.Itoa(opts.RecordBufferQueueSize))
	}
	if opts.RecordBufferConsumerStallGrace > 0 {
		args = append(args, "--consumer-stall-grace", opts.RecordBufferConsumerStallGrace.String())
	}
	// != 0, not > 0: a NEGATIVE half-close grace disables half-close and
	// must reach the agent.
	if opts.RecordBufferHalfCloseGrace != 0 {
		args = append(args, "--half-close-grace", opts.RecordBufferHalfCloseGrace.String())
	}
	a.logger.Debug("Starting native agent with args", zap.Strings("args", args))

	if opts.ConfigPath != "" && opts.ConfigPath != "." {
		args = append(args, "--config-path", opts.ConfigPath)
	}

	// Recorded apart from the handoff, so an agent that did not get its token
	// is still one this process launched, and still checked for it.
	token.RecordLaunch(opts.AgentURI)
	return appendTokenFileArg(a.logger, args)
}

// startNativeAgent starts the keploy agent as a native process. The process's
// exit is sent on exited.
func (a *AgentClient) startNativeAgent(ctx context.Context, opts models.SetupOptions, exited chan<- error, run *agentRun) error {

	// Get the errgroup from context
	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return fmt.Errorf("failed to get errorgroup from the context")
	}

	keployBin, err := utils.GetCurrentBinaryPath()
	if err != nil {
		utils.LogError(a.logger, err, "failed to get current keploy binary path")
		return err
	}

	args, progress, removeProgress := a.nativeAgentLaunch(opts)

	// The PTY and the cached-credentials probe both exist for one reason: sudo
	// may prompt for a password. On a platform where the agent is never
	// elevated (darwin) there is no prompt, and taking the PTY path anyway is
	// pure cost — startNativeAgentWithPTY puts the user's real terminal into
	// raw mode for the whole run and swallows their keystrokes into a pty
	// nothing reads.
	elevates := agentUtils.AgentNeedsElevation(runtime.GOOS)

	sudoCached := false
	if elevates {
		// Cached credentials mean we can use sudo -n and skip the prompt.
		sudoCached = sudoCredentialsCached()
	}
	run.watchBoot(progress, sudoMayPrompt(elevates, sudoCached, os.Geteuid()), keployBin)

	if elevates && needsAgentPTY() && !sudoCached {
		err := a.startNativeAgentWithPTY(ctx, keployBin, args, grp, exited, run, progress, removeProgress)
		if err != nil {
			removeProgress()
		}
		return err
	}

	// Create OS-appropriate command (handles sudo/process-group on Unix; plain on Windows)
	// If credentials are cached, this will use sudo -n (non-interactive)
	cmd := newNativeAgentCommand(keployBin, args, sudoCached)

	a.mu.Lock()
	a.agentCmd = cmd // this has been set for proper stopping of the native agent
	a.mu.Unlock()
	// Start (OS-specific tweaks happen inside utils.StartCommand), its lines
	// reaching keploy's console cut (startWithConsole)
	console, err := startWithConsole(cmd, agentUtils.StartCommand)
	if err != nil {
		removeProgress()
		utils.LogError(a.logger, err, "failed to start keploy agent")
		return err
	}

	pid := cmd.Process.Pid
	run.sudoPid.Store(int64(pid))
	a.logger.Debug("keploy agent started", zap.Int("pid", pid))

	// Reaped by a goroutine of its own, outside the group: a caller waiting on
	// the group is let go once the stop has killed the agent at once at the
	// user's Ctrl+C (stopWatch, awaitAgentExit), while it finishes dying.
	reaped := make(chan error, 1)
	go func() {
		defer utils.Recover(a.logger)
		err := cmd.Wait()
		console.Exited()
		run.exited()
		removeProgress()
		// Reported BEFORE the group goroutine below can return it: returning
		// it cancels the group, and the readiness wait must find the exit
		// already here when it sees that cancellation.
		exited <- err
		reaped <- err
	}()

	grp.Go(func() error {
		defer utils.Recover(a.logger)
		err, ok := a.awaitAgentExit(ctx, reaped, run, pid)
		if !ok {
			return nil
		}
		// If ctx wasn't cancelled, bubble up unexpected exits
		if err != nil && ctx.Err() == nil {
			a.logger.Error("agent process exited with error", zap.Error(err))
			return err
		}
		a.mu.Lock()
		if a.agentCmd == cmd { // not a later start's
			a.agentCmd = nil
		}
		a.mu.Unlock()
		a.logger.Debug("agent process stopped")
		return nil
	})

	grp.Go(func() error {
		defer utils.Recover(a.logger)
		<-ctx.Done()
		if a.conf.Agent.AgentURI == "" {
			return nil
		}
		a.logger.Debug("Keploy agent shutdown requested", zap.Int("pid", pid))
		a.logger.Info("Stopping keploy agent")
		a.stopNativeAgent(run, pid, cmd, progress)
		return nil
	})

	return nil
}

// nativeAgentLaunch is the argv of a native agent this client starts
// (nativeAgentArgs), telling it where to report its teardown's progress once
// it is stopped, which its stop reads (endAgent); and that path, with how to
// remove it once the agent has exited.
func (a *AgentClient) nativeAgentLaunch(opts models.SetupOptions) (args []string, progress string, removeProgress func()) {
	args = a.nativeAgentArgs(opts)
	progress, removeProgress = teardownProgressPath(a.logger)
	if progress != "" {
		args = append(args, "--teardown-progress-file", progress)
	}
	return args, progress, removeProgress
}

// teardownProgressPath returns where a native agent this client starts
// reports its teardown's progress (utils.ReportTeardownProgress), in a
// directory of that start's own, and how to remove the directory once the
// agent has exited. "" when no directory could be made: the agent then reports
// nothing, and is never killed for want of progress.
func teardownProgressPath(logger *zap.Logger) (string, func()) {
	dir, err := os.MkdirTemp("", "keploy-agent-stop-")
	if err != nil {
		logger.Debug("could not make a directory for the agent's teardown progress", zap.Error(err))
		return "", func() {}
	}
	return filepath.Join(dir, utils.TeardownProgressFileName), func() { _ = os.RemoveAll(dir) }
}

// StartedAgent reports whether this client started the agent it talks to
// (natively, or in a container it runs): nothing but this client stops it, and
// it holds its frame streams open for this client's drain. Not under docker
// compose, nor for an agent another process started (a k8s sidecar).
func (a *AgentClient) StartedAgent() bool { return a.startedAgent.Load() }

// agentStoppingGrace bounds how long the CLI waits for an agent that is
// already going away (agentGoingAway) to exit before it sends it SIGTERM
// (endAgent). A signal that reaches an agent on its way out is a second one to
// it, which it reports as such at the end of a recording that lost nothing, so
// the signal is kept for an agent that has reported no teardown by the end of
// the grace. A signal is not a kill: the CLI then waits for the exit while the
// teardown makes progress (stopWatch). The stop request before it
// (agentStopRequestTimeout) and it stay within the 30 s the recorders'
// teardown waits on the goroutine that stops the agent (DrainErrGroup:
// "record-setup", "mock-record"), so such an agent is signalled before that
// wait could give up on it -- which it does not while the agent reports the
// steps of its teardown (stopWatch).
var agentStoppingGrace = 15 * time.Second

// agentStopRequestTimeout bounds the stop request itself (requestAgentStop),
// which comes before the grace.
const agentStopRequestTimeout = 5 * time.Second

// agentGoingAway reports whether a request to an agent that was up failed
// because the agent is already going away: its HTTP server closed the
// connection, or no longer listens. A signal that reached the agent too stops
// it once its recording client has closed its streams (the agent's signal
// hold), and the client's own stop request then finds it on its way out. The
// same errors from an agent that never answered (agentRun.ready) say only that
// it is not listening yet.
func agentGoingAway(err error) bool {
	return err != nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		neterr.IsConnRefused(err) || neterr.IsConnReset(err))
}

// startNativeAgentWithPTY starts the agent with PTY support for interactive input (e.g., sudo password)
func (a *AgentClient) startNativeAgentWithPTY(ctx context.Context, keployBin string, args []string, grp *errgroup.Group, exited chan<- error, run *agentRun, progress string, removeProgress func()) error {
	// Create command configured for PTY
	cmd := newNativeAgentPTYCommand(keployBin, args)

	a.logger.Debug("Starting native agent with PTY for interactive input")

	// Start with PTY
	ptyHandle, err := agentUtils.StartCommandWithPTY(cmd, a.logger)
	if err != nil {
		utils.LogError(a.logger, err, "failed to start keploy agent with PTY")
		return err
	}

	a.mu.Lock()
	a.agentCmd = cmd
	a.agentPTY = ptyHandle
	a.mu.Unlock()

	pid := cmd.Process.Pid
	run.sudoPid.Store(int64(pid))
	a.logger.Debug("keploy agent started with PTY", zap.Int("pid", pid))

	// Reaped outside the group, for the reason given in startNativeAgent.
	reaped := make(chan error, 1)
	go func() {
		defer utils.Recover(a.logger)
		err := ptyHandle.Wait()
		run.exited()
		removeProgress()
		// Before the group goroutine returns it, as in startNativeAgent.
		exited <- err
		reaped <- err
	}()

	grp.Go(func() error {
		defer utils.Recover(a.logger)
		err, ok := a.awaitAgentExit(ctx, reaped, run, pid)
		if !ok {
			return nil
		}
		// If ctx wasn't cancelled, bubble up unexpected exits
		if err != nil && ctx.Err() == nil {
			a.logger.Error("agent process exited with error", zap.Error(err))
			return err
		}
		a.mu.Lock()
		if a.agentPTY == ptyHandle { // not a later start's
			a.agentCmd = nil
			a.agentPTY = nil
		}
		a.mu.Unlock()
		a.logger.Debug("agent process stopped")
		return nil
	})

	grp.Go(func() error {
		defer utils.Recover(a.logger)
		<-ctx.Done()
		if a.conf.Agent.AgentURI == "" {
			return nil
		}
		a.logger.Debug("Keploy agent shutdown requested", zap.Int("pid", pid))
		a.logger.Info("Stopping keploy agent")
		a.stopPTYAgent(run, pid, ptyHandle, progress)
		return nil
	})

	return nil
}

func (a *AgentClient) requestAgentStop() error {
	if a.conf == nil {
		return fmt.Errorf("agent config is nil")
	}
	if a.conf.Agent.AgentURI == "" {
		return fmt.Errorf("agent URI is not configured")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), agentStopRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(stopCtx, http.MethodPost, fmt.Sprintf("%s/stop", a.conf.Agent.AgentURI), nil)
	if err != nil {
		return err
	}

	// On a connection of its own. Sent on one the client kept from an
	// earlier request, it could reach the agent as the agent closed that
	// connection for being idle: a POST is not resent, so the request failed
	// ("server closed idle connection") with an agent that was not going
	// away, and that agent was signalled at once.
	client := a.client
	client.Transport = ownConnections(client.Transport)
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("agent stop http %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// stopAgent stops the agent process gracefully.
func (a *AgentClient) stopAgent() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentCancel != nil {
		a.agentCancel()
		a.agentCancel = nil
	}

	if a.agentCmd != nil && a.agentCmd.Process != nil {
		pid := a.agentCmd.Process.Pid
		a.logger.Debug("Stopping keploy agent", zap.Int("pid", pid))
	}

	// The PTY is never closed here: closing it hangs the agent up, and a
	// hung-up agent dies where it stands. The agent's stop goroutine ends it
	// (stopPTYAgent) -- SIGTERM to sudo, which also ends sudo at its password
	// prompt -- and the PTY is closed once it has exited (PTYHandle.Wait). The
	// terminal is the user's again at once, though.
	if a.agentPTY != nil {
		a.agentPTY.RestoreTerminal()
	}
}

// nativeStopPoll is how often the CLI reads a stopping native agent's
// teardown progress. It only spaces the reads: what counts is the agent's own
// beats (stopWatch).
const nativeStopPoll = 250 * time.Millisecond

// stopNativeAgent stops a natively started agent on the CLI's stop: asked
// over its API, or sent SIGTERM (its leader: sudo hands it on), and waited for
// while its teardown makes progress, which it reports at progress (endAgent).
func (a *AgentClient) stopNativeAgent(run *agentRun, pid int, cmd *exec.Cmd, progress string) {
	var reach killReach
	reach.firstAtOnce, reach.noAtOnce = agentUtils.KillAgentReach(cmd)
	a.endAgent(run, pid, a.requestAgentStop,
		func() error { return agentUtils.TerminateCommand(cmd, a.logger) },
		func(now bool) error { return agentUtils.KillAgentCommand(cmd, a.logger, now) }, reach,
		readProgressFile(progress))
}

// stopPTYAgent stops an agent started on a PTY on the CLI's stop: the user's
// terminal is restored at once (keploy may exit before the agent does), then
// it is stopped as stopNativeAgent stops one, its SIGTERM going to sudo with
// the PTY left open. The same SIGTERM ends sudo when it is still at its
// password prompt: sudo exits on it there.
func (a *AgentClient) stopPTYAgent(run *agentRun, pid int, h *agentUtils.PTYHandle, progress string) {
	h.RestoreTerminal()
	var reach killReach
	reach.firstAtOnce, reach.noAtOnce = agentUtils.KillPTYReach(h)
	a.endAgent(run, pid, a.requestAgentStop,
		func() error { return agentUtils.TerminatePTYCommand(h, a.logger) },
		func(now bool) error { return agentUtils.KillPTYCommand(h, a.logger, now) }, reach,
		readProgressFile(progress))
}

// endAgent stops a native agent on the CLI's stop, and returns once it has
// exited, or been killed at once at the user's Ctrl+C.
//
// It asks the agent over its API (ask). If that fails, the agent is sent
// SIGTERM (terminate) -- whether or not it has answered its readiness check
// yet: an agent attaches its TLS uprobes before it answers (the proxy's
// AfterStart), so one that has not answered may already be removing them on
// that signal -- except one that answered and is already going away
// (agentGoingAway), which got its stop some other way: it is sent SIGTERM only
// if it has reported no teardown within agentStoppingGrace. Then it is waited
// for while its teardown makes progress, and killed (kill) only once it has
// stopped making any, or has detached everything and not exited -- kill(false),
// on which it dies once no teardown work in progress is making progress -- or
// at the user's Ctrl+C once the stop has said a press stops it, as that offer
// says: kill(false) at a press answering the first offer, kill(true), at
// once, at one answering an offer made after a press (stopWatch). reach is
// what kill does where that differs (Windows, sudo): the offers say so. An
// agent sudo runs as root (reach.noAtOnce), which no kill ends at once, is
// waited for after a press until it has exited or halted. An agent already
// reaped is never signalled: its pid may be another process's by now.
func (a *AgentClient) endAgent(run *agentRun, pid int, ask, terminate func() error, kill func(now bool) error, reach killReach, read func(context.Context) (utils.TeardownProgress, bool)) {
	ifRunning := func(f func() error) func() error {
		return func() error {
			if run.isGone() {
				return nil
			}
			return f()
		}
	}
	terminate = ifRunning(terminate)
	killIfRunning := func(now bool) error {
		if run.isGone() {
			return nil
		}
		return kill(now)
	}
	// Only a Ctrl+C once the stop has said one stops the agent now forces it:
	// one before may be a copy of the stop itself (stopWatch).
	watch := stopWatch{logger: a.logger.With(zap.Int("pid", pid)),
		gone: run.gone, read: read, kill: killIfRunning, poll: nativeStopPoll, reach: reach,
		settled: sudoAgentSettled(pid), moved: run.scope}

	err := ask()
	switch {
	case err == nil:
		a.logger.Info("Asked the keploy agent to stop", zap.Int("pid", pid))
	case run.isGone():
		return
	case run.ready.Load() && agentGoingAway(err):
		a.logger.Debug("the keploy agent is already stopping", zap.Error(err))
		// Joined before endAgent returns: no signal outlives the stop.
		watched, nudged := make(chan struct{}), make(chan struct{})
		defer func() { close(watched); <-nudged }()
		after := agentStoppingGrace
		go func() {
			defer close(nudged)
			grace := time.NewTimer(after)
			defer grace.Stop()
			select {
			case <-watched:
				return
			case <-grace.C:
			}
			if _, reported := read(context.Background()); reported {
				a.logger.Debug("the keploy agent is reporting its teardown; it is not signalled")
				return
			}
			a.logger.Warn("the keploy agent was stopping but has reported no teardown; sending it a stop signal",
				zap.Duration("waited", agentStoppingGrace))
			if err := terminate(); err != nil {
				a.logger.Debug("failed to signal the keploy agent to stop", zap.Error(err))
			}
		}()
	default:
		a.logger.Debug("failed to request keploy agent shutdown, sending stop signal", zap.Error(err))
		if err := terminate(); err != nil {
			a.logger.Debug("failed to signal the keploy agent to stop", zap.Error(err))
		}
	}
	if watch.run() {
		run.stoppedAtOnce()
	}
}

// beginRun begins a start of the agent: one agent at a time, so a retried
// start (RetryAgentSetup) first waits for the agent it replaces to finish its
// shutdown, which is waited for while it makes progress (agentRun) -- it
// would otherwise run beside the new one, its hooks still attached for the
// same client. The new start gets a run of its own, not ready until its agent
// answers, and a context the CLI's stop cancels independently of ctx's other
// users.
func (a *AgentClient) beginRun(ctx context.Context) (context.Context, context.CancelFunc, *agentRun, error) {
	if err := a.awaitPreviousAgent(ctx); err != nil {
		return nil, nil, nil, err
	}
	agentCtx, cancel := context.WithCancel(ctx)
	run := newAgentRun()
	run.scope = utils.TeardownScopeOf(ctx)
	a.mu.Lock()
	a.agentCancel = cancel
	a.run = run
	a.mu.Unlock()
	return agentCtx, cancel, run, nil
}

// agentExitBound is how long a step that needs a stopped agent gone waits
// for its exit while no stop is holding the drains (utils.AwaitAgentExit): the
// drains' own bound.
var agentExitBound = 30 * time.Second

// awaitPreviousAgent returns once the agent of the previous start, if any, has
// exited -- or with an error when ctx ends, the user interrupts again, or the
// wait has run agentExitBound with no stop holding the drains
// (utils.AwaitAgentExit): a new agent beside the old one would hook the same
// client twice.
func (a *AgentClient) awaitPreviousAgent(ctx context.Context) error {
	prev := a.currentRun()
	if prev == nil || prev.isGone() {
		return nil
	}
	a.logger.Info("Waiting for the previous keploy agent to exit before starting a new one (Ctrl+C stops waiting)")
	again := utils.InterruptedAfter(utils.RepeatInterrupts())
	stop, waited := make(chan struct{}), make(chan struct{})
	defer close(waited)
	go func() {
		select {
		case <-ctx.Done():
		case <-again:
		case <-waited:
			return
		}
		close(stop)
	}()
	if utils.AwaitAgentExit(prev.gone, agentExitBound, stop) {
		return nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return fmt.Errorf("the previous keploy agent has not exited")
}

// awaitAgentExit is the group's wait for a native agent's exit (reaped). Once
// the stop has killed the agent at once at the user's Ctrl+C, or found an
// agent sudo runs as root halted after a press (run.letGo: stopWatch hears
// only one given once it said that one stops the agent now, never a copy of
// the stop's own signal), it lets the group go -- and with it a caller
// waiting on the group -- while the agent finishes dying on its own; ok is
// false then.
func (a *AgentClient) awaitAgentExit(ctx context.Context, reaped <-chan error, run *agentRun, pid int) (err error, ok bool) {
	select {
	case err = <-reaped:
		return err, true
	case <-ctx.Done():
	}
	select {
	case err = <-reaped:
		return err, true
	case <-run.letGo:
		a.logger.Warn("Stopped waiting for the keploy agent; it is still shutting down on its own",
			zap.Int("pid", pid))
		return nil, false
	}
}

// logAgentContainerDiagnostics dumps the agent container's own logs and state
// when it never became ready. Without it a readiness timeout is a black box:
// the CLI prints the whole wait window of silence and then a generic error,
// with no way to tell a docker-run/container-start stall from an in-agent hang
// (eBPF load, OOM, panic). Best-effort and bounded — never blocks teardown.
func (a *AgentClient) logAgentContainerDiagnostics(container string) {
	if strings.TrimSpace(container) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cli := engine.Active().CLI
	logs, _ := exec.CommandContext(ctx, cli, "logs", "--tail", "200", container).CombinedOutput()
	state, _ := exec.CommandContext(ctx, cli, "inspect", "-f",
		"status={{.State.Status}} exitCode={{.State.ExitCode}} oomKilled={{.State.OOMKilled}} error={{.State.Error}}",
		container).CombinedOutput()
	a.logger.Warn("keploy-agent did not become ready; captured agent container diagnostics",
		zap.String("container", container),
		zap.String("state", strings.TrimSpace(string(state))),
		zap.String("agent_logs", strings.TrimSpace(string(logs))))
}

// monitorAgent monitors the agent process and handles cleanup
func (a *AgentClient) monitorAgent(clientCtx context.Context, agentCtx context.Context) {
	select {
	case <-clientCtx.Done():
		// Client context cancelled, stop the agent
		a.logger.Debug("Client context cancelled, stopping agent")
		a.stopAgent()
	case <-agentCtx.Done():
		// Agent context cancelled or agent stopped
		if errors.Is(agentCtx.Err(), context.Canceled) {
			a.logger.Info("Agent was stopped intentionally")
		} else {
			a.logger.Error("Agent stopped unexpectedly, client operations may be affected", zap.String("next_step", "check agent logs for panic/oom, ensure required capabilities/permissions, or rerun with --debug and verify the agent binary is compatible with the CLI"))
		}
	}
}

// agentStoppedBeforeReady is Setup's error for an agent process that ended
// before it ever reported ready: nothing will answer, so there is nothing
// left to wait for.
//
// The agent's exit status says why when it knows (cli/agent.go arms it from
// utils.ExitCodeFor), and it is re-armed here as this process's own code: the
// agent is a separate process, and its status is the only thing that crosses
// back. Without that, an agent that could not start for want of privileges
// and one that could not start for want of tracefs both ended the run with the
// same bare failure -- and before this wait watched the process at all, with
// a readiness timeout five and a half minutes later.
func (a *AgentClient) agentStoppedBeforeReady(exitErr error, isDockerCmd bool) error {
	var cause error
	// An *exec.ExitError for an agent keploy started itself, an
	// *agentContainerExit for one compose started.
	var ee interface{ ExitCode() int }
	if errors.As(exitErr, &ee) {
		cause = agentExitCause(ee.ExitCode())
	}
	if cause == nil {
		if exitErr == nil {
			return errors.New("the keploy agent exited before it became ready")
		}
		return fmt.Errorf("the keploy agent exited before it became ready: %w", exitErr)
	}
	a.armAgentStartCause(cause, isDockerCmd)
	return fmt.Errorf("%w: the keploy agent could not start (%v)", cause, exitErr)
}

// agentReportedStartFailure is Setup's error for an agent that reported its
// start failed (utils.FailBoot) with failure, and with code, the code it exits
// with for it: what its exit would say (agentStoppedBeforeReady), at once, and
// also of an agent that lives on after the failure (its server's hooks, or its
// server, failed). The specific code is armed, as for its exit.
func (a *AgentClient) agentReportedStartFailure(code int, failure string, isDockerCmd bool) error {
	cause := agentExitCause(code)
	if cause == nil {
		return fmt.Errorf("the keploy agent could not start: %s", failure)
	}
	a.armAgentStartCause(cause, isDockerCmd)
	return fmt.Errorf("%w: the keploy agent could not start: %s", cause, failure)
}

// agentExitCause is the cause an agent's exit code names (cli/agent.go arms
// it from utils.ExitCodeFor), or nil for one that names none.
func agentExitCause(code int) error {
	switch code {
	case utils.ExitPrivilegeRequired:
		return utils.ErrPrivilegeRequired
	case utils.ExitEnvironmentUnsupported:
		return utils.ErrEnvironmentUnsupported
	}
	return nil
}

// armAgentStartCause makes cause, why the agent could not start, this
// process's own exit code, and tells the user what to do about it.
func (a *AgentClient) armAgentStartCause(cause error, isDockerCmd bool) {
	utils.SetExitCodeOnce(utils.ExitCodeFor(cause))
	a.logger.Error("the keploy agent could not start", zap.Error(cause), zap.String("next_step", agentStartRemedy(cause, isDockerCmd, runtime.GOOS)))
}

// agentStartRemedy is what the user can do about an agent that could not start
// for cause, which depends on where it ran.
//
// It ran as root either way. Only the Linux agent tags a privilege failure
// (its eBPF load), and the native one is elevated before it starts (see
// agentUtils.NewAgentCommand), while the agent container runs as root with the
// capabilities keploy starts it with. So a privilege the kernel still refused
// is not one sudo or setcap can give: it is the container's, or the Docker
// daemon's, to grant -- or, on a machine that is not a container, kernel
// lockdown or a security policy refused root itself. All this process has is
// the agent's exit status, which does not say which of those refused it, so
// the remedy names the candidates rather than picking one.
//
// Tracefs is a Linux agent's to need, and all a native one can report
// missing: it is reached over loopback, so it needs no network, and no remedy
// may send the user to connect one. No macOS or Windows agent reports anything
// missing today; the branch for them (goos, the runtime.GOOS it ran on) is
// there so that one that ever does is not told to mount tracefs, and can only
// point at its log. The agent container is the one agent that needs a
// network address, because its hooks send the application's connections to
// its container's own address: keploy starts it on the network the
// application's command names, and --network none leaves it none.
func agentStartRemedy(cause error, isDockerCmd bool, goos string) string {
	switch {
	case errors.Is(cause, utils.ErrPrivilegeRequired) && isDockerCmd:
		return "keploy starts its agent container as root with --cap-add BPF, PERFMON, NET_ADMIN, SYS_RESOURCE and SYS_PTRACE, and the kernel still refused it eBPF. A rootless Docker or Podman cannot grant those capabilities at all; with a rootful daemon, look for what else denies it: an SELinux or AppArmor policy, a Docker whose default seccomp profile predates CAP_BPF, or kernel lockdown. The agent's log above names the operation the kernel refused"
	case errors.Is(cause, utils.ErrPrivilegeRequired):
		return "the agent already ran as root and the kernel still refused it eBPF, as it does inside a container that does not grant it: start that container with --privileged, or with --cap-add BPF --cap-add PERFMON --cap-add NET_ADMIN --cap-add SYS_RESOURCE --cap-add SYS_PTRACE, and with tracefs mounted into it (-v /sys/kernel/tracing:/sys/kernel/tracing). A rootless container runtime cannot grant these at all. Outside a container, look for what denies root eBPF: kernel lockdown, or an SELinux or AppArmor policy. The agent's log above names the operation the kernel refused"
	case isDockerCmd:
		return "mount debugfs on the machine Docker runs on (mount -t debugfs nodev /sys/kernel/debug): keploy's agent container reaches tracefs through it. Or, if the log says it found no non-loopback IP: keploy starts its agent container on the network the application's command names, and --network none leaves it no address. The agent's log above names which one is missing"
	case goos != "linux":
		return "the agent's log above names what is missing"
	default:
		return "mount tracefs (mount -t tracefs nodev /sys/kernel/tracing; into a container, -v /sys/kernel/tracing:/sys/kernel/tracing). The agent's log above names what is missing"
	}
}

// waitForAgent blocks until the agent started by startAgent answers its health
// check (nil, or a refusal wrapping provenswitch.ErrAgentSwitch when that
// answer reports a proven switch that is not this keploy's), its process ends
// (exited), it reports its start failed, its start stops making progress
// (pkg.ErrAgentNotReady: agent_boot_watch.go), or ctx is cancelled. No clock
// bounds the wait: a start that makes progress is waited for however slowly
// the host runs it.
func (a *AgentClient) waitForAgent(ctx context.Context, exited <-chan error, isDockerCmd bool, opts models.SetupOptions) error {
	agentCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The run this wait is for: startAgent's, made just before it.
	run := a.currentRun()

	// The headers of the health answer that finds the agent ready, which its
	// proven switch is read from (below).
	answers := make(chan http.Header, 1)
	go pkg.AgentHealthTickerWithAnswer(agentCtx, a.logger, a.conf.Agent.AgentURI, answers, 1*time.Second)

	// What the agent reports of its start, and whether its process runs:
	// judged once a bootLook (watchStart).
	read, running := readBootFile(""), alwaysRunning
	switch {
	case isDockerCmd:
		// The container this Setup named: what its looks see is noted on
		// the client's entry of it.
		read, running = containerBootLook(a.logger, a.containerNamed(opts.KeployContainer))
	case run != nil:
		read = readBootFile(run.bootReport)
		if run.bootPrompt {
			// sudo's prompt may be in front of it until sudo runs it: the
			// user answers it in their own time.
			running = run.sudoRanAgent
		}
	}
	// A container that exits ends the wait through docker run's own exit
	// (exited), which says why: the watch is not asked.
	judged := watchStart(agentCtx, a.logger, read, running, nil, isDockerCmd, false)

	for {
		select {
		case exitErr := <-exited:
			return a.agentStoppedBeforeReady(exitErr, isDockerCmd)
		case answer := <-answers:
			if answer == nil {
				// The ticker ended with ctx, which the case below decides.
				answers = nil
				continue
			}
			// It answered: running (agentRun).
			if run != nil {
				run.ready.Store(true)
			}
			// The agent's proven switch must be this keploy's (provenswitch.
			// CheckAgent), and it is taken from the very answer that found the
			// agent ready, not from another request: nothing more is asked of
			// the agent, so a run with the switch off asks it exactly what it
			// always did, and an agent whose answer has been taken is not
			// waited on again, whether it has since exited or stopped
			// answering. A refusal names both sides; the agent is stopped
			// rather than left running.
			if err := provenswitch.CheckAgent(answer.Get(provenswitch.HealthHeader)); err != nil {
				a.stopAgent()
				return err
			}
			return nil
		case <-ctx.Done():
			// The caller's Ctrl+C -- or a goroutine in the caller's errgroup
			// failing, which cancels ctx too. The one waiting on the agent
			// process is such a goroutine, and it reports the exit before it
			// fails the group, so when that is what happened the exit is
			// already here. Otherwise the cause says which it was: ctx.Err()
			// is "context canceled" for both, and a caller could not tell
			// keploy failing from the user stopping it.
			select {
			case exitErr := <-exited:
				return a.agentStoppedBeforeReady(exitErr, isDockerCmd)
			default:
			}
			return context.Cause(ctx)
		case j := <-judged:
			switch j.verdict {
			case bootFailed:
				// The agent says its start has failed, as a retry would: it
				// is stopped (it may live on after the failure), and the run
				// fails with that failure and the code it exits with for it.
				if isDockerCmd {
					a.logAgentContainerDiagnostics(opts.KeployContainer)
				}
				a.stopAgent()
				return a.agentReportedStartFailure(j.p.ExitCode, j.why, isDockerCmd)
			case bootStalled:
				// Setup's cleanup defer is not in scope yet, so without this
				// the wedged agent and its goroutine leak, and any retry
				// inherits the mess. The container's own logs first: the CLI
				// otherwise shows a bare error, with nothing to root-cause
				// from.
				if isDockerCmd {
					a.logAgentContainerDiagnostics(opts.KeployContainer)
				}
				a.stopAgent()
				// Sentinel so the caller can retry a fresh bring-up on this
				// nondeterministic stall without retrying deterministic
				// failures.
				return fmt.Errorf("%w: %s", pkg.ErrAgentNotReady, j.why)
			}
		}
	}
}

// readAgentSwitch reads the agent's report of its proven switch from /health:
// the provenswitch.HealthHeader value, "" when a healthy agent sends none. A
// transport failure or a status other than 200 is an error: the header of
// such an answer says nothing about the agent.
func (a *AgentClient) readAgentSwitch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/health", a.conf.Agent.AgentURI), nil)
	if err != nil {
		return "", fmt.Errorf("failed to build the agent health request: %w", err)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the agent's health check answered %d", res.StatusCode)
	}
	return res.Header.Get(provenswitch.HealthHeader), nil
}

// perfEventParanoidPath is the kernel knob docker mode relaxes for its agent's
// tracepoints. It is not namespaced: read or written from inside a container,
// it is the host's.
//
// A variable so a test can point it at a file of its own.
var perfEventParanoidPath = "/proc/sys/kernel/perf_event_paranoid"

// The remedies checkDockerModePrivileges logs, one for each thing it can be
// refused. Both are for a process that may already be root: a CI job
// container runs keploy as root, and /proc/sys is read-only to root in any
// container that is not --privileged, whatever capabilities it was given.
const (
	dockerModeCapabilitiesRemedy = "keploy's docker mode needs the capabilities named above in the process that runs it. Run keploy as root on the machine Docker runs on, or, inside a container, start that container with --privileged. --cap-add for each of them works too, but leaves /proc/sys read-only, so then set kernel.perf_event_paranoid to 2 or lower on the host first (sysctl -w kernel.perf_event_paranoid=2), which keploy would otherwise do itself"
	perfEventParanoidRemedy      = "keploy's docker mode lowers kernel.perf_event_paranoid to 2 so its agent can attach to syscall tracepoints, and this process may not write it. Set it on the host first (sysctl -w kernel.perf_event_paranoid=2) and keploy leaves it as it is; or run keploy as root on the host, or inside a container started with --privileged"
)

// checkDockerModePrivileges fails, with ErrPrivilegeRequired and the privilege
// exit code armed, when this process cannot run keploy's docker mode: it lacks
// the capabilities the check below asks for, or it may not relax
// perf_event_paranoid for the agent's tracepoints. That relaxing is the one
// change it makes, and the first one docker mode makes.
func (a *AgentClient) checkDockerModePrivileges(opts models.SetupOptions) error {
	// docker run, docker start and --from-container start the agent container
	// from this process, and have always been held to the capability check;
	// docker compose starts it as a service of the user's own project, and
	// never was.
	if utils.CmdType(opts.CommandType) != utils.DockerCompose {
		if err := utils.CheckRequiredPermissions(); err != nil {
			return a.dockerModeRefused(fmt.Errorf("%w: %w", utils.ErrPrivilegeRequired, err), dockerModeCapabilitiesRemedy)
		}
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	err := relaxPerfEventParanoid()
	if errors.Is(err, utils.ErrPrivilegeRequired) {
		return a.dockerModeRefused(err, perfEventParanoidRemedy)
	}
	if err != nil {
		a.logger.Error("Failed to relax host perf_event_paranoid. Tracepoints may fail.", zap.Error(err))
	}
	return err
}

// dockerModeRefused arms the privilege exit code for err and logs it with the
// remedy. Distinct exit code: a caller must be able to tell "Keploy needs
// kernel privileges" from "your tests failed", both of which used to be a bare
// 1. See utils/exitcodes.go.
func (a *AgentClient) dockerModeRefused(err error, remedy string) error {
	utils.SetExitCodeOnce(utils.ExitPrivilegeRequired)
	a.logger.Error("keploy cannot run in docker mode here", zap.Error(err), zap.String("next_step", remedy))
	return err
}

// relaxPerfEventParanoid makes sure perf_event_paranoid is at most 2, which
// lets the agent's eBPF programs attach to syscall tracepoints like sys_socket
// via perf_event_open: the Debian and Ubuntu defaults (3 and 4) block this for
// unprivileged users. The value is written straight to the procfs knob instead
// of shelling out to the `sysctl` binary, which isn't guaranteed to be present
// in minimal images.
//
// A value that already allows it is left as it is. Writing 2 over it would
// tighten a host set lower, and the write needs what a container that is not
// --privileged never has, a writable /proc/sys, so a host set up in advance
// is how such a container gets to run keploy's docker mode at all. A write
// this process is not allowed to make is tagged ErrPrivilegeRequired.
func relaxPerfEventParanoid() error {
	if current, err := os.ReadFile(perfEventParanoidPath); err == nil {
		if level, err := strconv.Atoi(strings.TrimSpace(string(current))); err == nil && level <= 2 {
			return nil
		}
	}
	err := os.WriteFile(perfEventParanoidPath, []byte("2\n"), 0644)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("%w: %w", utils.ErrPrivilegeRequired, err)
	}
	return err
}

// fetchAgentCA retrieves the running agent's MITM CA public certificate over the
// token-authenticated control-plane API. a.client already carries the bearer
// token, so this inherits the same guard as every other control-plane call. The
// returned bytes are a PEM certificate — public material, never the private key.
func (a *AgentClient) fetchAgentCA(ctx context.Context) ([]byte, error) {
	url := fmt.Sprintf("%s/ca", a.conf.Agent.AgentURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build CA request: %w", err)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to request the agent CA: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return nil, fmt.Errorf("agent returned %d fetching the CA: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	// Cap the read: a CA PEM is a few KB; a megabyte ceiling is generous and
	// bounds a misbehaving endpoint.
	certPEM, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read the agent CA: %w", err)
	}
	if len(certPEM) == 0 {
		return nil, fmt.Errorf("agent returned an empty CA certificate")
	}
	return certPEM, nil
}

// runFlagsOf takes the app's published ports, networks and host entries out
// of its `docker run` (or `docker start`) command: the agent container
// publishes and joins them on the app's behalf, and the app joins the
// agent's network namespace. A `docker compose` command has none of these
// flags, and its -p is its project name (--project-name), not a port: its
// ports, networks and hosts are its compose file's, which App.SetupCompose
// reads. Taking -p out of it ran the user's stack as the working
// directory's project instead of the one they named, and every tool that
// resolved the project from the command, the user's own `docker compose -p`
// included, then missed the stack keploy started.
func runFlagsOf(cmd string, kind utils.CmdType) (cleaned string, ports, networks, hosts []string, dns models.AppDNS) {
	if kind == utils.DockerCompose {
		return cmd, nil, nil, nil, models.AppDNS{}
	}
	cmd, ports, networks = agentUtils.ExtractDockerFlags(cmd)
	cmd, hosts = agentUtils.ExtractAddHosts(cmd)
	cmd, dns = agentUtils.ExtractDNSFlags(cmd)
	return cmd, ports, networks, hosts, dns
}

// withAppRunSettings hands the agent container what keploy took out of the
// app's own docker command (runFlagsOf) or read off the --from-container
// container: the app joins the agent container's network namespace, where
// docker refuses or ignores its own ports, networks, host entries and
// resolver settings, so the agent container publishes, joins and takes them
// on its behalf (GetKeployDockerAlias).
func withAppRunSettings(opts models.SetupOptions, ports, networks, hosts []string, dns models.AppDNS) models.SetupOptions {
	opts.AppPorts = ports
	opts.AppExtraHosts = hosts
	opts.AppDNS = dns
	if len(networks) > 0 {
		opts.AppNetworks = networks
	}
	return opts
}

func (a *AgentClient) Setup(ctx context.Context, cmd string, opts models.SetupOptions) error {
	isDockerCmd := utils.IsDockerCmd(utils.CmdType(opts.CommandType))
	opts.IsDocker = isDockerCmd
	// A Setup sets up a new agent: the container the client named for the
	// last one is not this one's. A docker Setup names its own below; a
	// native one has none, and its watches watch its process, not that
	// container.
	a.forgetAgentContainer()

	agentPort, err := utils.GetAvailablePort()
	if err != nil {
		utils.LogError(a.logger, err, "failed to find available port for agent")
		return err
	}

	// Check and allocate available ports for proxy and DNS
	// Exclude the ports already handed out in this setup: none of them is bound
	// yet, so a later draw could otherwise legitimately return one of them.
	proxyPort, err := utils.EnsureAvailablePorts(a.conf.ProxyPort, agentPort) // check if the proxy port provided by user is unused
	if err != nil {
		utils.LogError(a.logger, err, "failed to ensure available ports for proxy")
		return err
	}

	dnsPort, err := utils.EnsureAvailablePorts(a.conf.DNSPort, agentPort, proxyPort) // check if the dns port provided by user is unused
	if err != nil {
		utils.LogError(a.logger, err, "failed to ensure available ports for DNS")
		return err
	}

	opts.AgentPort = agentPort
	opts.ProxyPort = proxyPort
	opts.DnsPort = dnsPort
	opts.AgentURI = fmt.Sprintf("http://localhost:%d/agent", agentPort)

	// Update the ports in the configuration
	a.conf.Agent.AgentPort = agentPort
	a.conf.Agent.ProxyPort = proxyPort
	a.conf.Agent.DnsPort = dnsPort
	a.conf.ProxyPort = proxyPort
	a.conf.DNSPort = dnsPort
	a.conf.Agent.AgentURI = opts.AgentURI
	forgetContainers(opts.AgentURI)
	forgetNetworkNames(opts.AgentURI)

	a.logger.Debug("Using available ports",
		zap.Uint32("agent-port", agentPort),
		zap.Uint32("proxy-port", proxyPort),
		zap.Uint32("dns-port", dnsPort))

	if isDockerCmd {
		// Whether docker mode can run here at all is settled before anything
		// is changed for it: the user's --from-container container stopped,
		// the host's perf_event_paranoid written. Both used to come first, so
		// keploy run as root in an unprivileged container -- a CI job's, with
		// the host's Docker socket -- stopped the user's container and then
		// failed with a bare 1 on the read-only /proc/sys, never reaching the
		// capability check that says why.
		if err := a.checkDockerModePrivileges(opts); err != nil {
			return err
		}

		var origCmd = cmd
		a.logger.Debug("Application command provided :", zap.String("cmd", cmd))

		opts.KeployContainer = agentUtils.GenerateRandomContainerName(a.logger, "keploy-v3-")
		a.conf.KeployContainer = opts.KeployContainer
		a.nameAgentContainer(opts.KeployContainer)
		// The agent container maps host.docker.internal to the Docker host
		// (host-gateway) where the daemon resolves it (extra_hosts.go). A
		// daemon that does not answer its version would not start the agent
		// either: the question is bounded as a stall, by keploy's own
		// runnable time (withRunnableBound), and going unanswered fails this
		// bring-up as a stalled one (retried), rather than hanging setup with
		// nothing said, or answering no.
		versionCtx, cancelVersion := withRunnableBound(ctx, daemonAnswerStall, errDaemonSilent)
		opts.HostGateway, err = kdocker.HostGatewaySupported(versionCtx, a.dockerClient)
		cancelVersion()
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return fmt.Errorf("%w: the container engine's daemon at %s did not answer its version (GET /version) for %s of keploy's runnable time: %v", pkg.ErrAgentNotReady, a.dockerClient.DaemonHost(), daemonAnswerStall, err)
		}

		var appPorts, appNetworks, appHosts []string
		var appDNS models.AppDNS
		if utils.CmdType(opts.CommandType) == utils.FromContainer {
			// No command to scrape. The container itself is the source of
			// truth, and it answers with published ports and networks the
			// regex below cannot see: ExtractDockerFlags matches only the
			// space-separated spelling, so `--network=foo` and `-p=8080:80`
			// slip past it today and then collide with the `--network=container:`
			// keploy splices in.
			appPorts, appNetworks, appHosts, appDNS, err = a.appNetworkingFromContainer(ctx, opts.FromContainer)
			if err != nil {
				return fmt.Errorf("failed to read the networking of %q: %w", opts.FromContainer, err)
			}
			// The agent publishes those ports on the app's behalf, so the
			// original has to let go of them first - otherwise the agent's own
			// `docker run` fails with "port is already allocated" before
			// anything else happens. This is why the stop lives here and not in
			// App.Setup, which runs after the agent is already up.
			opts.FromContainerWasRunning, err = a.releaseSourceContainer(ctx, opts.FromContainer)
			// Registered before the error is acted on, because the stop has
			// already been issued by the time this returns one: the only thing
			// that can put the container back is gated on these fields.
			a.fromContainer = opts.FromContainer
			a.fromContainerWasRunning = opts.FromContainerWasRunning
			if err != nil {
				return fmt.Errorf("failed to stop %q: %w", opts.FromContainer, err)
			}
			// Nothing downstream has taken responsibility for the container
			// yet: App.RestoreSource only exists once the app has been built,
			// which is after the agent starts. Until then this is the only
			// thing that can put it back.
			//
			// Gated on an explicit success flag rather than on the returned
			// error: this function's return is unnamed, so a deferred closure
			// reading `err` sees the local of that name, which several returns
			// here never touch - a Ctrl+C during the readiness wait returns
			// ctx.Err() and an unhealthy agent returns ErrAgentNotReady, both
			// with the local still nil.
			defer func() {
				if err := recover(); err != nil {
					a.RestoreFromContainer()
					panic(err)
				}
			}()
		} else {
			cmd, appPorts, appNetworks, appHosts, appDNS = runFlagsOf(cmd, utils.CmdType(opts.CommandType))
		}
		opts = withAppRunSettings(opts, appPorts, appNetworks, appHosts, appDNS)
		if len(appNetworks) > 0 {
			a.logger.Debug("Found docker networks", zap.Strings("networks", opts.AppNetworks))
		}

		if origCmd != cmd {
			a.logger.Info(
				"Updated user command to allow Keploy to serve traffic before the app",
				zap.String("cmd", cmd),
			)
		}
	}

	if opts.CommandType != string(utils.DockerCompose) { // in case of docker compose, we will run the application command (our agent will run along with it)
		opts.ClientNSPID = uint32(os.Getpid())
		if !isDockerCmd && provenswitch.On() {
			// The run's members are this process's descendants: keep a daemon
			// the app starts among them (adopt_linux.go, P146).
			adoptDaemons(a.logger)
		}
		var agentExited <-chan error
		agentExited, err = a.startAgent(ctx, isDockerCmd, opts)
		if err != nil {
			return fmt.Errorf("failed to start agent: %w", err)
		}
		a.logger.Debug("Agent is now running, proceeding with setup")

		if err = a.waitForAgent(ctx, agentExited, isDockerCmd, opts); err != nil {
			return err
		}
	}

	// Continue with app setup and registration as per normal flow
	usrApp := app.NewApp(a.logger, cmd, a.dockerClient, opts)
	a.apps.Store(uint64(0), usrApp) // key = 0, since there's only one client per agent
	// Under compose the agent is the user's project's to start again: the
	// session puts back what the run set up on it (see agentSession).
	a.session().configure(utils.CmdType(opts.CommandType) == utils.DockerCompose)
	usrApp.SetOnAgentRestart(a.setUpRestartedAgent)

	// Set up cleanup on failure
	defer func() {
		if err != nil {
			a.logger.Info("Setup failed, cleaning up agent")
			a.stopAgent()
		}
	}()

	// Native mode only: the application runs as a child of this client and
	// inherits its environment, so the client points the language-runtime trust
	// env vars at the agent's CA. The CA is generated per run and lives in the
	// agent process, so the client fetches its PUBLIC certificate over the
	// token-authenticated control-plane API rather than carrying an embedded copy
	// of a shared CA. In docker-run, compose and k8s the shared /tmp/keploy-tls
	// volume delivers trust to the app container instead, and this call would only
	// leak an unused host temp file — so it is skipped there.
	if !isDockerCmd && opts.CommandType != string(utils.DockerCompose) {
		certPEM, caErr := a.fetchAgentCA(ctx)
		if caErr != nil {
			utils.LogError(a.logger, caErr, "failed to fetch the agent's CA certificate; the app would not trust keploy's MITM leaves",
				zap.String("next_step", "ensure the agent started cleanly (check the agent log for a SetupCA error) and retry"))
			return caErr
		}
		// TODO : Proxy or TLS should not be imported in the agent
		// This is done because to set env variable for TLS
		if err = ptls.SetupCaCertEnv(a.logger, certPEM); err != nil {
			utils.LogError(a.logger, err, "failed to set TLS environment")
			return err
		}
	}

	exportMockScopeEnv(a.logger, opts, agentPort)

	err = usrApp.Setup(ctx)
	if err != nil {
		utils.LogError(a.logger, err, "failed to setup app")
		return err
	}

	a.logger.Debug("Keploy client setup completed successfully")
	return nil
}

// exportMockScopeEnv exports the agent's scope-API address, and the token it
// needs, into the wrapped command's environment in mock mode, so a test-runner
// plugin / glue-code can mark per-test boundaries
// (POST {KEPLOY_MOCK_AGENT}/agent/scope/begin|end). The child inherits this
// process's environment. Native + docker-run put the child in the agent's
// netns, so localhost:<agentPort> reaches it; docker-compose shares the host
// loopback. Harmless when unused.
//
// Nothing at all outside mock mode: there the wrapped command is the
// application under test, and it is handed nothing.
func exportMockScopeEnv(logger *zap.Logger, opts models.SetupOptions, agentPort uint32) {
	if !opts.MockMode {
		return
	}
	if err := os.Setenv("KEPLOY_MOCK_AGENT", fmt.Sprintf("http://localhost:%d", agentPort)); err != nil {
		logger.Debug("failed to export KEPLOY_MOCK_AGENT", zap.Error(err))
	}
	// The scope API is guarded like every other control-plane route, and
	// this caller is not keploy — it is the user's test runner. Without a
	// credential the advertised integration would simply 401.
	//
	// os.Setenv is process-wide, so strictly every child started after
	// this inherits it, not only the wrapped runner. That is the same
	// trust domain: in mock mode the wrapped process is the intended API
	// client, and the incidental children are keploy's own docker and
	// shell teardown commands. What matters is the guard above: only in
	// mock mode, so the application under test in a record or test run is
	// still handed nothing.
	//
	// A separate variable from token.Env, so a process that inherits it
	// presents the token rather than adopting it as one to enforce.
	if tok := token.Session(); tok != "" {
		if err := os.Setenv(token.MockAgentTokenEnv, tok); err != nil {
			logger.Debug("failed to export "+token.MockAgentTokenEnv, zap.Error(err))
		}
	}
	session := "record"
	if opts.Mode == models.MODE_TEST {
		session = "replay"
	}
	if err := os.Setenv("KEPLOY_MOCK_SESSION", session); err != nil {
		logger.Debug("failed to export KEPLOY_MOCK_SESSION", zap.Error(err))
	}
	// A claim marker inherited from an enclosing keploy run (this keploy was
	// started by a test of another run) names a process of that run: left
	// in place, this run's test processes would take themselves for its
	// helpers and never claim, and their tests would pass unjudged. Only
	// the proven wire has claims; with the switch off the environment is
	// left as it was.
	if provenswitch.On() {
		if err := os.Unsetenv(models.TestPIDEnv); err != nil {
			logger.Debug("failed to remove "+models.TestPIDEnv, zap.Error(err))
		}
	}
}

func (a *AgentClient) getApp() (*app.App, error) {
	ap, ok := a.apps.Load(uint64(0))
	if !ok {
		return nil, fmt.Errorf("app not found")
	}

	// type assertion on the app
	h, ok := ap.(*app.App)
	if !ok {
		return nil, fmt.Errorf("failed to type assert app")
	}

	return h, nil
}

// ComposeAgentOutcome is what the keploy-agent compose service served and
// missed in a mock replay, as it wrote it on being stopped.
//
// Under docker compose it is the only way to learn either: compose stops the
// agent service the moment the app exits, so the agent is gone before
// GetConsumedMocks or GetMockErrors could reach it. The App reads the account
// out of the stopped container before keploy's teardown removes it.
func (a *AgentClient) ComposeAgentOutcome() (models.MockOutcome, error) {
	ap, err := a.getApp()
	if err != nil {
		return models.MockOutcome{}, err
	}
	stopped, ok := ap.StoppedAgent()
	if !ok {
		return models.MockOutcome{}, errors.New("keploy did not read the keploy-agent container before it was removed")
	}
	return stopped.MockOutcome()
}

// agentContainerExit is a keploy-agent container that stopped, as docker
// reports it. Its ExitCode is the agent's own status: the agent is the
// container's process.
type agentContainerExit struct {
	container string
	code      int
	oomKilled bool
}

func (e *agentContainerExit) Error() string {
	msg := fmt.Sprintf("the keploy-agent container %s exited with code %d", e.container, e.code)
	if e.oomKilled {
		msg += ", killed for running out of memory"
	}
	return msg
}

func (e *agentContainerExit) ExitCode() int { return e.code }

// ComposeAgentFailure is the keploy-agent compose service stopping while the
// app still needed it, as keploy's own failure -- and nil when it did not,
// which is every run in which compose stopped the agent after the app exited.
//
// Under compose the agent is a service in the project, and when it stops
// first, compose aborts the project over it: the exit that reaches keploy is
// compose reporting the dependency it lost, or the app's code after compose
// stopped it (the test command's, under keploy mock), and neither is the
// app's own verdict. The agent's container, read before keploy's teardown
// removes it, says which it was. keploy mock and keploy record both ask.
//
// An agent that stopped before the app ever started could not start at all,
// and its exit status says why, exactly as a native agent's does
// (agentStoppedBeforeReady): the specific code is armed, and the remedy
// logged.
func (a *AgentClient) ComposeAgentFailure() error {
	ap, err := a.getApp()
	if err != nil {
		return nil
	}
	stopped, ok := ap.StoppedAgent()
	if !ok {
		return nil
	}
	return a.composeAgentFailure(stopped)
}

// composeAgentFailure is ComposeAgentFailure for what keploy read from the
// stopped agent's container.
func (a *AgentClient) composeAgentFailure(stopped app.StoppedAgent) error {
	if !stopped.AgentFailed() {
		return nil
	}
	exit := &agentContainerExit{container: stopped.Container, code: stopped.Agent.ExitCode, oomKilled: stopped.Agent.OOMKilled}
	if !stopped.AppStarted() {
		return a.agentStoppedBeforeReady(exit, true)
	}
	return fmt.Errorf("the keploy agent stopped while the app was running: %w", exit)
}

// ComposeEndAfterReady is what ended a docker compose run keploy saw its
// agent ready in, when the app's own end does not stand: the keploy agent
// having crashed -- under the running app, or before compose started it --
// as keploy's own failure; or the project stopping before compose ever
// started the app, though compose reported 0, as the project's failure, not
// a clean end. nil when the app's own end stands. keploy record asks it once
// its recording is under way; appExit is the app's exit as keploy got it,
// which stands in for the app's container when docker could not say how that
// ended (StoppedAgent.AgentDiedAfterReady).
func (a *AgentClient) ComposeEndAfterReady(appExit int) error {
	ap, err := a.getApp()
	if err != nil {
		return nil
	}
	stopped, ok := ap.StoppedAgent()
	if !ok {
		return nil
	}
	return composeEndAfterReady(stopped, appExit)
}

// composeEndAfterReady is ComposeEndAfterReady for what keploy read from the
// stopped agent's container.
func composeEndAfterReady(stopped app.StoppedAgent, appExit int) error {
	if stopped.AgentDiedAfterReady(appExit) {
		exit := &agentContainerExit{container: stopped.Container, code: stopped.Agent.ExitCode, oomKilled: stopped.Agent.OOMKilled}
		if stopped.AppNeverStarted() {
			return fmt.Errorf("the keploy agent stopped before compose started the app: %w", exit)
		}
		return fmt.Errorf("the keploy agent stopped while the app was running: %w", exit)
	}
	// Compose reports 0 for an app it never started, in a race (see
	// App.run): another of the project's services failed first, and nothing
	// ran to be recorded.
	if appExit == 0 && stopped.AppNeverStarted() {
		return errors.New("compose never started the app (another of the project's services ended it), though it reported 0")
	}
	return nil
}

// appContainerExit is the app's container no longer running, as docker
// reports it.
type appContainerExit struct {
	container string
	status    string
	code      int
	oomKilled bool
	// restarting says its restart policy is starting it again.
	restarting bool
}

func (e *appContainerExit) Error() string {
	var msg string
	switch {
	case e.restarting:
		msg = fmt.Sprintf("the application container %s exited with code %d and is restarting", e.container, e.code)
	case e.status == "dead":
		msg = fmt.Sprintf("the application container %s is dead (exit code %d)", e.container, e.code)
	default:
		msg = fmt.Sprintf("the application container %s exited with code %d", e.container, e.code)
	}
	if e.oomKilled {
		msg += ", killed for running out of memory"
	}
	return msg
}

// AppContainerExit implements replay.AppContainerExitReader: the app's
// container no longer running -- exited, dead, or restarting after an exit --
// as an error naming it and how it ended, and nil while it runs or when keploy
// cannot tell.
func (a *AgentClient) AppContainerExit(ctx context.Context) error {
	ap, err := a.getApp()
	if err != nil {
		return nil
	}
	name, state, exited := ap.ExitedState(ctx)
	if !exited {
		return nil
	}
	return &appContainerExit{container: name, status: state.Status, code: state.ExitCode, oomKilled: state.OOMKilled, restarting: state.Restarting}
}

// ComposeDownOnSetupFailure tears down the managed docker-compose stack so a
// retry after a per-test-set setup failure (e.g. agent-readiness timeout) does
// not hit a "container name already in use" conflict from the dependency
// containers/network left behind. No-op when there is no managed app or it is
// not a compose app (App.ComposeDown self-guards on kind == DockerCompose).
func (a *AgentClient) ComposeDownOnSetupFailure(_ context.Context) error {
	ap, err := a.getApp()
	if err != nil {
		a.logger.Debug("ComposeDownOnSetupFailure: no managed app to tear down")
		return nil
	}
	ap.ComposeDown()
	return nil
}

func (a *AgentClient) startInDocker(ctx context.Context, logger *zap.Logger, opts models.SetupOptions, run *agentRun) error {
	// The CLI's own config: the alias forwards the root flags the agent
	// container must share with the CLI (agentargs.Root) from it.
	keployAlias, err := kdocker.GetKeployDockerAlias(ctx, logger, a.conf, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to prepare docker command and environment")
		return err
	}

	cmd, err := kdocker.PrepareDockerCommand(ctx, keployAlias)
	if err != nil {
		utils.LogError(logger, err, "failed to prepare docker command")
		return err
	}

	cmd.Cancel = dockerAgentCancel(logger, cmd, run, opts.KeployContainer)

	logger.Debug("running the following command to start agent in docker", zap.String("command", redactToken(cmd.String())))

	// Check if we need PTY for interactive input (e.g., sudo password on Linux)
	if agentUtils.NeedsPTY() {
		return a.startInDockerWithPTY(ctx, logger, cmd)
	}

	// As the native agent's (startWithConsole).
	console, err := startWithConsole(cmd, (*exec.Cmd).Start)
	if err == nil {
		err = cmd.Wait()
		console.Exited()
	}
	if err != nil {
		if ctx.Err() == context.Canceled {
			// A start a cancelled context refused left no process.
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			logger.Info("Keploy agent in docker stopped gracefully.")
			return nil
		}
		utils.LogError(logger, err, "failed to run keploy agent in docker")
		return err
	}

	return nil
}

// dockerAgentCancel is the `docker run` command's Cancel: it stops the agent's
// container (stopAgentContainerFor) and then kills the client -- through
// cmd.Process, not its raw pid, which cmd.Wait may have reaped already.
func dockerAgentCancel(logger *zap.Logger, cmd *exec.Cmd, run *agentRun, name string) func() error {
	return func() error {
		stopAgentContainerFor(logger, run, name)
		if cmd.Process == nil {
			return nil
		}
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	}
}

// stopAgentContainerFor stops the agent's container on the CLI's stop: it is
// sent SIGTERM and waited for while its teardown makes progress
// (stopAgentContainer), answered or not -- the agent attaches its TLS uprobes
// before it answers (agentRun). The signal is sent synchronously, as `stop`
// always was, so it has gone out before keploy can exit. An agent that never
// answered is sent SIGTERM again until it reports its teardown
// (resendSIGTERM).
func stopAgentContainerFor(logger *zap.Logger, run *agentRun, name string) {
	logger.Debug("Context cancelled. Stopping the keploy agent container and waiting for it to exit.")
	if stopAgentContainer(logger, name, !run.ready.Load(), run.scope) {
		run.stoppedAtOnce()
	}
}

// containerEngineCommand is the container engine's CLI running args under
// ctx, as name (the engine's CLI, or sudo). It runs in a process group of its
// own, so a terminal's Ctrl+C does not interrupt it.
func containerEngineCommand(ctx context.Context, name string, argv ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, argv...)
	agentUtils.SetOwnProcessGroup(c)
	// Once ctx is cancelled the process is killed -- but under sudo that is
	// sudo, and the root engine client it ran keeps the output pipe open until
	// it exits by itself. Do not wait for that pipe.
	c.WaitDelay = time.Second
	return c
}

// engineCLI runs the container engine's CLI for one stop of an agent's
// container: as root or in the docker group without sudo, or -- on Linux,
// when that is refused -- with sudo -n (non-interactive). Which of the two is
// decided by the first command that succeeds and kept for the rest of the
// stop, so a later command that fails for reasons of its own (a `cp` of a file
// the agent has not written yet) does not try sudo again.
type engineCLI struct {
	logger *zap.Logger
	mode   atomic.Int32 // engineUndecided, engineDirect or engineSudo
}

const (
	engineUndecided int32 = iota
	engineDirect
	engineSudo
)

func newEngineCLI(logger *zap.Logger) *engineCLI { return &engineCLI{logger: logger} }

// run runs the engine's CLI with args under ctx (containerEngineCommand), and
// returns what output takes from it.
func (e *engineCLI) run(ctx context.Context, output func(*exec.Cmd) ([]byte, error), args ...string) ([]byte, error) {
	cli := engine.Active().CLI
	direct := func() ([]byte, error) { return output(containerEngineCommand(ctx, cli, args...)) }
	sudo := func() ([]byte, error) {
		return output(containerEngineCommand(ctx, "sudo", append([]string{"-n", cli}, args...)...))
	}
	switch e.mode.Load() {
	case engineDirect:
		return direct()
	case engineSudo:
		return sudo()
	}
	out, err := direct()
	if err == nil {
		e.mode.CompareAndSwap(engineUndecided, engineDirect)
		return out, nil
	}
	if ctx.Err() != nil || runtime.GOOS != "linux" {
		return out, err
	}
	e.logger.Debug("container engine command without sudo failed, trying with sudo -n",
		zap.Strings("args", args), zap.Error(err))
	out, err = sudo()
	if err == nil {
		e.mode.CompareAndSwap(engineUndecided, engineSudo)
	}
	return out, err
}

// combined runs args and returns their output, stderr included.
func (e *engineCLI) combined(ctx context.Context, args ...string) ([]byte, error) {
	return e.run(ctx, (*exec.Cmd).CombinedOutput, args...)
}

// stdout runs args and returns their stdout alone.
func (e *engineCLI) stdout(ctx context.Context, args ...string) ([]byte, error) {
	return e.run(ctx, (*exec.Cmd).Output, args...)
}

// dockerStopPoll is how often the CLI reads a stopping agent container's
// teardown progress (a `cp` out of the container). It only spaces the reads:
// what counts is the agent's own beats (stopWatch).
var dockerStopPoll = time.Second

// stopAgentContainer stops an agent's container, and returns once it has
// exited, or once it killed it at once at the user's Ctrl+C (killedAtOnce).
// It sends the agent SIGTERM
// (`kill --signal SIGTERM`, synchronously) and waits for the container to stop
// (`wait`) while the agent's teardown makes progress, which it reports in its
// container (readContainerProgress). It kills it only once it has stopped
// making any, or has detached everything and not exited -- with the
// escalation signal (`kill --signal SIGUSR2`), on which the agent dies once no
// teardown work in progress is making progress -- or at the user's Ctrl+C
// once the stop has said a press stops it, as that offer says: the escalation
// signal at a press answering the first offer, and at once (`kill`, SIGKILL)
// at one answering an offer made after a press (stopWatch). Not `stop`, whose
// grace period (10 s by default) ends in a SIGKILL however the teardown is
// doing (agentRun says why that must not land in it). unanswered (the agent
// never answered its readiness check) has SIGTERM sent again until the agent
// reports its teardown (resendSIGTERM).
func stopAgentContainer(logger *zap.Logger, name string, unanswered bool, moved *utils.TeardownScope) (killedAtOnce bool) {
	logger = logger.With(zap.String("container", name))
	cli := newEngineCLI(logger)
	if out, err := cli.combined(context.Background(), "kill", "--signal", "SIGTERM", name); err != nil {
		logger.Debug("Could not signal the agent container. It may have already stopped.",
			zap.Error(err), zap.String("output", strings.TrimSpace(string(out))))
		return
	}
	return awaitAgentContainer(logger, cli, name, unanswered, moved)
}

// awaitAgentContainer waits for an agent's container that has been told to
// stop to exit, while the agent's teardown makes progress, which it reports
// in its container (readContainerProgress), and kills it only once it has
// stopped making any, or has detached everything and not exited, or at the
// user's Ctrl+C, as stopAgentContainer says. It returns once the container
// has exited, or once it killed it at once (killedAtOnce).
func awaitAgentContainer(logger *zap.Logger, cli *engineCLI, name string, unanswered bool, moved *utils.TeardownScope) (killedAtOnce bool) {
	// kill(false) sends the agent the escalation signal, on which it dies
	// once no teardown work in progress is making progress
	// (utils.DieOnEscalation); kill(true) is SIGKILL, at once.
	kill := func(now bool) error {
		args := []string{"kill", "--signal", "SIGUSR2", name}
		if now {
			args = []string{"kill", name}
		}
		out, err := cli.combined(context.Background(), args...)
		if err != nil {
			logger.Debug("Could not kill the agent container. It may have already stopped.",
				zap.Error(err), zap.String("output", strings.TrimSpace(string(out))))
		}
		return err
	}
	// The wait returns when the container has stopped; at once, with an
	// error, when it is already gone (--rm removed it).
	waitCtx, stopWaiting := context.WithCancel(context.Background())
	defer stopWaiting()
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		if out, err := cli.combined(waitCtx, "wait", name); err != nil {
			logger.Debug("stopped waiting for the agent container",
				zap.Error(err), zap.String("output", strings.TrimSpace(string(out))))
		}
	}()
	reported := make(chan struct{})
	resent := make(chan struct{})
	if unanswered {
		// Joined before the stop returns: no resend outlives it.
		first, most := sigtermResendFirst, sigtermResendMax
		go func() {
			defer close(resent)
			resendSIGTERM(waitCtx, reported, logger, cli, name, first, most)
		}()
	} else {
		close(resent)
	}
	// Only a Ctrl+C once the stop has said one stops the agent now forces it:
	// one before may be a copy of the stop itself (stopWatch).
	killedAtOnce = stopWatch{logger: logger, gone: gone, read: readContainerProgress(cli, name),
		kill: kill, poll: dockerStopPoll, reported: reported, moved: moved}.run()
	stopWaiting()
	<-gone
	<-resent
	return killedAtOnce
}

// EndComposeAgent sees the agent docker compose runs to its end, once a
// recording has drained it: compose stopped it with the app's project, and it
// waited for the drain (WatchAgentStop), and goes once the recording's streams
// close. Its container is waited for while its teardown makes progress
// (awaitAgentContainer), not for compose's grace: one that stops making
// progress, or has detached everything and not exited, is wedged, and is
// killed, keploy saying so at WARN (stopWatch). Compose then finishes stopping
// the project. Nothing is sent to an agent that is already going: compose's
// stop signal is its stop. But the agent waits, before it goes, for the
// recording's streams to close (its stop's hold), and release says that a
// drain was given up on a stall, whose stream may stay open: the agent is then
// sent the escalation signal, which ends that wait (utils.DieOnEscalation),
// so that its teardown begins, and is judged as any other. Otherwise it would
// report no teardown, which is waited for until it exits.
func (a *AgentClient) EndComposeAgent(release bool) {
	name := a.conf.KeployContainer
	if name == "" {
		return
	}
	logger := a.logger.With(zap.String("container", name))
	cli := newEngineCLI(logger)
	if release {
		logger.Warn("a drain of the keploy agent's streams was given up on a stall; telling the agent to stop waiting for it")
		if out, err := cli.combined(context.Background(), "kill", "--signal", "SIGUSR2", name); err != nil {
			logger.Debug("Could not signal the agent container. It may have already stopped.",
				zap.Error(err), zap.String("output", strings.TrimSpace(string(out))))
		}
	}
	// No teardown scope: a compose agent is compose's (this client began no
	// run of it, beginRun), and this wait runs before the stop's drains begin
	// (drainComposeAgent), so none of them could wait on its movements.
	awaitAgentContainer(logger, cli, name, false, nil)
}

// sigtermResendFirst is how long stopAgentContainer's resendSIGTERM waits
// before its first resend; each wait after it is twice the one before, up to
// sigtermResendMax.
var (
	sigtermResendFirst = time.Second
	sigtermResendMax   = 30 * time.Second
)

// resendSIGTERM sends the agent's container SIGTERM again, first after first
// and then at intervals doubling up to most, until the agent reports its
// teardown (reported: it got a SIGTERM) or ctx ends (the container has
// exited, or the stop is over). An agent that never answered may have been
// signalled while its PID 1 was still the OSS image's entrypoint (sh, then
// sudo before it handles signals), and a pid namespace's init is sent only
// the signals it handles: that SIGTERM was dropped. The intervals space the
// resends out; nothing is decided by them.
func resendSIGTERM(ctx context.Context, reported <-chan struct{}, logger *zap.Logger, cli *engineCLI, name string, first, most time.Duration) {
	every := first
	for {
		next := time.NewTimer(every)
		select {
		case <-ctx.Done():
			next.Stop()
			return
		case <-reported:
			next.Stop()
			logger.Debug("the agent has reported its teardown: it got a SIGTERM, and is not sent another")
			return
		case <-next.C:
		}
		if out, err := cli.combined(ctx, "kill", "--signal", "SIGTERM", name); err != nil && ctx.Err() == nil {
			logger.Debug("could not signal the agent container again",
				zap.Error(err), zap.String("output", strings.TrimSpace(string(out))))
		}
		if every *= 2; every > most {
			every = most
		}
	}
}

// startInDockerWithPTY starts the docker agent with PTY support for interactive input (e.g., sudo password)
func (a *AgentClient) startInDockerWithPTY(ctx context.Context, logger *zap.Logger, cmd *exec.Cmd) error {
	// Configure command for PTY execution (OS-specific)
	agentUtils.ConfigureCommandForPTY(cmd)

	logger.Debug("Starting docker agent with PTY for interactive input")

	// Start with PTY
	ptyHandle, err := agentUtils.StartCommandWithPTY(cmd, logger)
	if err != nil {
		utils.LogError(logger, err, "failed to start keploy agent in docker with PTY")
		return err
	}

	a.mu.Lock()
	a.agentCmd = cmd
	a.agentPTY = ptyHandle
	a.mu.Unlock()

	pid := cmd.Process.Pid
	logger.Debug("keploy agent in docker started with PTY", zap.Int("pid", pid))

	// Wait for the command to finish
	err = ptyHandle.Wait()

	a.mu.Lock()
	if a.agentPTY == ptyHandle { // not a later start's
		a.agentCmd = nil
		a.agentPTY = nil
	}
	a.mu.Unlock()

	if err != nil {
		if ctx.Err() == context.Canceled {
			logger.Info("Keploy agent in docker stopped gracefully.")
			return nil
		}
		utils.LogError(logger, err, "failed to run keploy agent in docker with PTY")
		return err
	}

	return nil
}

// GetErrorChannel returns nil for HTTP transport — use GetMockErrors() instead.
func (a *AgentClient) GetErrorChannel() <-chan error {
	return nil
}

// GetMockErrors fetches mock-not-found errors from the agent via HTTP.
func (a *AgentClient) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	url := fmt.Sprintf("%s/mockerrors", a.conf.Agent.AgentURI)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get mock errors: %s", err.Error())
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil {
			utils.LogError(a.logger, closeErr, "failed to close response body for getmockerrors; safe to ignore once, but check agent/proxy logs if repeated")
		}
	}()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("get mock errors returned status %d: %s", res.StatusCode, string(body))
	}

	var mockErrors []models.UnmatchedCall
	if err := json.NewDecoder(res.Body).Decode(&mockErrors); err != nil {
		return nil, fmt.Errorf("failed to decode mock errors response: %s", err.Error())
	}
	// The agent read incompletely: the list is what it read, and the error
	// says it may be missing calls.
	if cause := res.Header.Get(models.UnmatchedCallsUnreadHeader); cause != "" {
		return mockErrors, fmt.Errorf("%w (agent: %s)", models.ErrUnmatchedCallsUnread, cause)
	}
	return mockErrors, nil
}

// BeginTestErrorCapture asks the agent to open a per-test mock-error capture
// window so the next GetMockErrors returns only this test's misses. A missing
// endpoint (older agent) returns 404 and is treated as a no-op, preserving the
// legacy global-queue behaviour.
func (a *AgentClient) BeginTestErrorCapture(ctx context.Context) error {
	return a.openTestErrorCapture(ctx, false)
}

// ContinueTestErrorCapture is BeginTestErrorCapture for a test after its set's
// first: the window carries in the misses made since the previous test's
// window closed. An agent that predates it ignores ?carry and opens the window
// as Begin does.
func (a *AgentClient) ContinueTestErrorCapture(ctx context.Context) error {
	return a.openTestErrorCapture(ctx, true)
}

func (a *AgentClient) openTestErrorCapture(ctx context.Context, carry bool) error {
	url := fmt.Sprintf("%s/test-capture/begin", a.conf.Agent.AgentURI)
	if carry {
		url += "?carry=1"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to begin test error capture: %s", err.Error())
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil {
			utils.LogError(a.logger, closeErr, "failed to close response body for begin-test-error-capture; safe to ignore once")
		}
	}()
	if res.StatusCode == http.StatusNotFound {
		return nil // older agent without the endpoint — fall back to legacy behaviour
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("begin test error capture returned status %d: %s", res.StatusCode, string(body))
	}
	return nil
}

// agentBacklogProbeTimeout bounds one look at the agent's backlog
// (BacklogBefore). It decides nothing: a look that runs out is
// BacklogCannotTell, never "nothing pending", and the stop looks again.
const agentBacklogProbeTimeout = 2 * time.Second

// BacklogBefore asks the agent whether it may still hand over a test case or a
// mock captured before `before` (GET /agent/record/pending). The verdict has
// three values and a fourth: pending, clear, cannot tell (no answer came: a
// timeout, a transport error, an error status, a body that does not decode;
// err says why), and unsupported (404: an agent that predates the route; 501:
// a capture without a watermark), when the stop can only go by its streams'
// quiet. A look that cannot tell is never read as clear.
func (a *AgentClient) BacklogBefore(ctx context.Context, before time.Time) (models.AgentBacklog, error) {
	if a.conf.Agent.AgentURI == "" {
		return models.AgentBacklog{Verdict: models.BacklogUnsupported}, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, agentBacklogProbeTimeout)
	defer cancel()
	url := fmt.Sprintf("%s/record/pending?before=%d", a.conf.Agent.AgentURI, before.UnixNano())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return models.AgentBacklog{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return models.AgentBacklog{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusNotImplemented:
		return models.AgentBacklog{Verdict: models.BacklogUnsupported}, nil
	default:
		return models.AgentBacklog{}, fmt.Errorf("the agent answered %s", resp.Status)
	}
	var out struct {
		Pending    bool   `json:"pending"`
		Progress   uint64 `json:"progress"`
		WaitingOn  string `json:"waitingOn"`
		EndMarkers bool   `json:"endMarkers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return models.AgentBacklog{}, err
	}
	b := models.AgentBacklog{Verdict: models.BacklogClear, Progress: out.Progress, WaitingOn: out.WaitingOn, EndMarkers: out.EndMarkers}
	if out.Pending {
		b.Verdict = models.BacklogPending
	}
	return b, nil
}

// EndRecordingStreams asks the agent to end the frame streams of the
// recording whose token is token (models.StreamEndPath, StreamEndTokenHeader):
// each puts on its stream what it still has, then its end marker
// (StreamEnded), and ends; one this client reopens after that ends at once. A
// recording's stop asks once the agent has said it holds nothing more from
// before it, if the agent said it can (models.AgentBacklog.EndMarkers).
func (a *AgentClient) EndRecordingStreams(ctx context.Context, token string) error {
	if a.conf.Agent.AgentURI == "" {
		return errors.New("no agent to end the streams of")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.conf.Agent.AgentURI+models.StreamEndPath+"?token="+neturl.QueryEscape(token), nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the agent answered %s", resp.Status)
	}
	return nil
}

// agentStopNoticeRetry is how long WatchAgentStop waits before it asks again
// after a request that failed: an agent compose starts again, or one that
// answers again. It paces the asking; nothing is judged by it.
var agentStopNoticeRetry = time.Second

// WatchAgentStop returns what closes once the agent says a stop signal has
// reached it and it waits for this client to drain its streams
// (models.StopNoticePath): the frame streams opened on a ctx marked
// models.WithAgentStopNotice ask the agent for that. It never closes for an
// agent that predates the route (404), or once ctx is done. A request that
// fails (the agent restarting, going away) is made again, of the agent at
// this client's address, until ctx is done.
func (a *AgentClient) WatchAgentStop(ctx context.Context) <-chan struct{} {
	stopping := make(chan struct{})
	if a.conf.Agent.AgentURI == "" {
		return stopping
	}
	go func() {
		for {
			answered, err := a.awaitStopNotice(ctx)
			switch {
			case answered:
				close(stopping)
				return
			case err == nil || ctx.Err() != nil:
				// No route (an agent from before it), or the watch is over.
				return
			}
			a.logger.Debug("could not wait on the keploy agent's word of a stop; asking again", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(agentStopNoticeRetry):
			}
		}
	}()
	return stopping
}

// awaitStopNotice makes one request of models.StopNoticePath: answered when
// the agent said a stop reached it; neither answered nor an error from an
// agent without the route.
func (a *AgentClient) awaitStopNotice(ctx context.Context) (answered bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.conf.Agent.AgentURI+models.StopNoticePath, nil)
	if err != nil {
		return false, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("the agent answered %s", resp.Status)
}

// StreamEnded reports whether the agent ended the latest stream of kind with
// its end marker (EndRecordingStreams): every frame it had for this client is
// read. A stream that ended any other way (the agent went away) did not.
func (a *AgentClient) StreamEnded(kind models.FrameStream) bool {
	if v, ok := a.streamEnds.Load(kind); ok {
		return v.(*atomic.Bool).Load()
	}
	return false
}

// newStreamEnd is the end flag of a new stream of kind (StreamEnded).
func (a *AgentClient) newStreamEnd(kind models.FrameStream) *atomic.Bool {
	ended := new(atomic.Bool)
	a.streamEnds.Store(kind, ended)
	return ended
}

// NotifyGracefulShutdown sends a request to the agent to set the graceful shutdown flag.
// This should be called before cancelling contexts during application shutdown.
// When the flag is set, connection errors will be logged as debug instead of error.
func (a *AgentClient) NotifyGracefulShutdown(ctx context.Context) error {
	// Every service calls this from its teardown defer, on every path out -
	// including the ones that return between Setup and Run, which App.run's own
	// defer never sees. Under --from-container those paths would otherwise
	// leave the user's container stopped and keploy's copy of it behind.
	defer a.RestoreFromContainer()

	if a.conf.Agent.AgentURI == "" {
		a.logger.Debug("Agent URI is empty, skipping graceful shutdown notification")
		return nil
	}

	url := fmt.Sprintf("%s/graceful-shutdown", a.conf.Agent.AgentURI)

	// Use a short timeout since this is a best-effort notification
	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", url, nil)
	if err != nil {
		a.logger.Debug("failed to create graceful shutdown request", zap.Error(err))
		return err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		// Don't log as error since this might fail during shutdown
		a.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		return err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		a.logger.Debug("agent returned non-200 status for graceful shutdown", zap.Int("status", resp.StatusCode))
		return fmt.Errorf("graceful shutdown notification failed with status %d", resp.StatusCode)
	}

	a.logger.Debug("Successfully notified agent of graceful shutdown")
	return nil
}

func (a *AgentClient) MakeAgentReadyForDockerCompose(ctx context.Context) (retErr error) {
	defer func() {
		if retErr == nil {
			a.session().record(stepAgentReady, func(c context.Context) error { return a.MakeAgentReadyForDockerCompose(c) })
		}
	}()
	// The wait follows the agent, not a clock: it goes on while the agent
	// answers that it is not ready yet, or while its start makes progress
	// (watchAgent), and ends at once on an answer that says it never will be
	// (a CA setup failure, which the agent latches until it is started again),
	// once its start fails or stops making progress, or once its container
	// has exited.
	//
	// The watch's verdict ends a request in flight too: an agent that takes
	// a connection and never answers it holds the request, not the release.
	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if gone := a.watchAgent(ctx, true); gone != nil {
		go func() {
			select {
			case err := <-gone:
				cancel(fmt.Errorf("the keploy agent did not take its release of the application (/agent/ready): %w", err))
			case <-ctx.Done():
			}
		}()
	}

	ticker := time.NewTicker(agentReadyRetryInterval)
	defer ticker.Stop()
	next := func() error {
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return parent.Err()
			}
			return context.Cause(ctx)
		case <-ticker.C:
			return nil
		}
	}

	url := fmt.Sprintf("%s/agent/ready", a.conf.Agent.AgentURI)

	for {
		// With the switch on, the agent's proven switch must be read and be
		// this keploy's before the agent is told to release the app: a
		// definite disagreement ends the wait, while an agent whose switch
		// cannot be read yet is not released and the wait goes on. With it
		// off nothing is read, and the agent is told to release the app
		// exactly as before the switch existed. Nothing is lost by that: the
		// agent compose starts is the one this keploy wrote into the file
		// (AddKeployAgentToCompose, which refuses a compose file that already
		// has a keploy-agent), and its command carries --proven only when the
		// switch is on here (GenerateKeployAgentService, provenswitch.AgentArgs).
		// The runner calls this for a native or docker-run agent too, whose
		// switch its bring-up already checked, with the switch on or off
		// (waitForAgent).
		if provenswitch.On() {
			reported, err := a.readAgentSwitch(ctx)
			if err == nil {
				if err := provenswitch.CheckAgent(reported); err != nil {
					return err
				}
			} else {
				a.logger.Debug("the agent's proven switch is not readable yet, retrying...", zap.Error(err))
				if err := next(); err != nil {
					return err
				}
				continue
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
		if err != nil {
			return err
		}

		resp, err := a.client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				a.logger.Debug("Successfully marked agent as ready")
				// A docker-mode agent is given the names its replay's
				// networks answer once it takes its release, not only at a
				// test set's start (MockOutgoing): a launcher that never
				// calls MockOutgoing, as enterprise's in-Pod replay stack
				// (whose agent k8s-proxy drives), would otherwise leave it
				// answering its own network's names itself.
				a.pushNetworkNames(parent)
				return nil
			}
			// A CA setup failure is latched in the agent (pTls.MarkCAFailed):
			// it answers so until it is started again, and no wait changes it.
			if msg := strings.TrimSpace(string(body)); resp.StatusCode == http.StatusServiceUnavailable && strings.HasPrefix(msg, agentCASetupFailed) {
				return fmt.Errorf("the keploy agent cannot release the application: %s", msg)
			}
			a.logger.Debug("Agent returned non-200 status for ready check", zap.Int("status", resp.StatusCode))
		} else {
			a.logger.Debug("Failed to call agent ready endpoint, retrying...", zap.Error(err))
		}
		if err := next(); err != nil {
			return err
		}
	}
}

// agentCASetupFailed begins the agent's answer to /agent/ready when its CA
// setup has failed (pkg/agent/routes, MakeAgentReady): a failure it latches.
const agentCASetupFailed = "CA setup failed:"

// StreamPcapArtifacts opens long-lived chunked GETs against the
// agent's /pcap/traffic and /pcap/keylog endpoints and writes the
// streamed bytes into destDir under traffic.pcap and sslkeys.log
// respectively. Blocks until ctx is cancelled (recording stop) or
// the agent's HTTP server closes the response. Best-effort: a
// transport failure on either stream is logged via the returned
// error but never fatal to the recording. Designed for k8s live
// recording where the session has no defined end — bytes flow into
// the local files continuously instead of being fetched on stop.
func (a *AgentClient) StreamPcapArtifacts(ctx context.Context, destDir string) error {
	if destDir == "" {
		return errors.New("destDir is required")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create dest dir %s: %w", destDir, err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return a.streamPcapArtifact(gctx, "/pcap/traffic", filepath.Join(destDir, "traffic.pcap"), 0o644, false)
	})
	g.Go(func() error {
		return a.streamPcapArtifact(gctx, "/pcap/keylog", filepath.Join(destDir, "sslkeys.log"), 0o644, true)
	})
	return g.Wait()
}

// streamPcapArtifact holds a single GET open and copies its body
// into dstFile until the server closes the stream or ctx is done.
// 503 means "capture not active" — silently skipped so the recorder
// stays a no-op for non-capture sessions. Other non-200s surface as
// errors. Context cancellation returns nil so callers don't see
// spurious errors at recording stop.
func (a *AgentClient) streamPcapArtifact(ctx context.Context, urlPath, dstFile string, mode os.FileMode, appendable bool) error {
	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, urlPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build pcap stream request: %w", err)
	}
	// Registered before it connects; it outlives a restarted agent (see
	// agentSession), going on in a file of its own (a capture), or in the
	// same file (appendable, as the key log is).
	stream := a.session().openStream()
	defer stream.close()
	conn := stream.connect(ctx)
	resp, err := a.client.Do(req.WithContext(conn.ctx))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return fmt.Errorf("dial %s: %w", url, err)
	}
	resp.Body = conn.body(resp.Body)
	for n := 1; ; n++ {
		path, flags := dstFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC
		if n > 1 && appendable {
			flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		} else if n > 1 {
			ext := filepath.Ext(dstFile)
			path = fmt.Sprintf("%s.%d%s", strings.TrimSuffix(dstFile, ext), n, ext)
		}
		copyErr, err := a.copyPcapStream(resp, url, path, flags, mode)
		if errors.Is(err, errNoPcapStream) {
			return nil
		}
		if err != nil {
			return err
		}
		var ok bool
		if resp, ok = a.resumeStream(ctx, stream, "pcap stream "+urlPath, http.MethodGet, urlPath, nil); !ok {
			if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
				return copyErr
			}
			return nil
		}
	}
}

// errNoPcapStream is an agent with no pcap stream to serve: capture is off.
var errNoPcapStream = errors.New("the agent serves no pcap stream")

// copyPcapStream writes one pcap stream response to path. It returns how the
// copy ended, and an error when there was nothing to copy into a file
// (errNoPcapStream when capture is off).
func (a *AgentClient) copyPcapStream(resp *http.Response, url, path string, flags int, mode os.FileMode) (copyErr, err error) {
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && !errors.Is(cerr, context.Canceled) {
			a.logger.Debug("failed to close pcap stream body", zap.String("url", url), zap.Error(cerr))
		}
	}()

	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusServiceUnavailable:
		a.logger.Debug("agent reports no pcap stream available; capture likely off",
			zap.String("url", url))
		return nil, errNoPcapStream
	case http.StatusOK:
		// fall through to the long copy below
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("stream %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	out, err := os.OpenFile(path, flags, mode)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil {
			a.logger.Warn("failed to close pcap dst file",
				zap.String("path", path), zap.Error(cerr))
		}
	}()

	a.logger.Debug("pcap stream connected",
		zap.String("url", url), zap.String("path", path))
	written, copyErr := io.Copy(out, resp.Body)
	a.logger.Debug("pcap stream ended",
		zap.String("url", url),
		zap.String("path", path),
		zap.Int64("bytes", written),
		zap.Error(copyErr))
	return copyErr, nil
}

// GetScopeWindows fetches the per-test scope windows the wrapped runner reported
// during a `keploy mock record` session (via /agent/scope/*), so the CLI can
// correlate captured mocks into mappings.yaml. A missing endpoint (older agent)
// returns nil, degrading to suite-level recording; under the proven switch it
// is an error. Under the switch the read is the run's last (?final=1), which
// closes the run: call it only once the test command has exited.
func (a *AgentClient) GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	url := fmt.Sprintf("%s/scope/windows", a.conf.Agent.AgentURI)
	if provenswitch.On() {
		// Every read the CLI makes is the run's last, once its command
		// exited: the agent fails a test still open. With the switch off
		// the read is the one keploy made before the switch.
		url += "?final=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create scope-windows request: %w", err)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get scope windows: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode == http.StatusNotFound {
		// An agent without the scope API. Under the proven switch what each
		// test did is keploy's verdict, so an agent that cannot say is an
		// error, never "no tests".
		if provenswitch.On() {
			return nil, errors.New("this keploy agent does not report what each test did (its scope windows answered 404); run the agent of this keploy version")
		}
		return nil, nil
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("scope windows returned status %d: %s", res.StatusCode, string(body))
	}
	var windows []models.ScopeWindow
	if err := json.NewDecoder(res.Body).Decode(&windows); err != nil {
		return nil, fmt.Errorf("failed to decode scope windows: %w", err)
	}
	return windows, nil
}

func (a *AgentClient) PushSetTable(ctx context.Context, root string, sets map[string]models.SetTable) (retErr error) {
	defer func() {
		if retErr == nil && a.session().recording() {
			recorded := maps.Clone(sets)
			a.session().record(stepSetTable, func(c context.Context) error { return a.PushSetTable(c, root, recorded) })
		}
	}()
	body, err := json.Marshal(models.ScopeTableReq{Root: root, Sets: sets})
	if err != nil {
		return fmt.Errorf("failed to marshal the set table: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/scope/table", a.conf.Agent.AgentURI), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to push the set table: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("set table returned status %d: %s", res.StatusCode, string(b))
	}
	return nil
}

// PushProvenTable hands the agent, under the proven switch, the per-test
// table and each key's pool slice (models.ScopeTableReq.Pool) in one
// request. Every answer but 200 is an error: a replay judged per test must
// not run on an agent that serves its tests and pool otherwise.
//
// windows are the recorded window of every test of the set
// (models.ScopeTableReq.Windows), and given the values each test gave when
// it was recorded (models.ScopeTableReq.Given), sent with them: the agent
// installs both before it serves the pool.
func (a *AgentClient) PushProvenTable(ctx context.Context, table, pool map[string][]string, windows []models.TestWindow, given map[string][]models.GivenValue) (retErr error) {
	if pool == nil {
		pool = map[string][]string{}
	}
	defer func() {
		if retErr == nil && a.session().recording() {
			t, p, w, g := maps.Clone(table), maps.Clone(pool), slices.Clone(windows), maps.Clone(given)
			a.session().record(stepScopeTable, func(c context.Context) error { return a.PushProvenTable(c, t, p, w, g) })
		}
	}()
	body, err := json.Marshal(models.ScopeTableReq{Mappings: table, Pool: pool, Windows: windows, Given: given})
	if err != nil {
		return fmt.Errorf("failed to marshal scope table: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/scope/table", a.conf.Agent.AgentURI), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create scope-table request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to push scope table: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("the agent refused the per-test table and pool slices (status %d): %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// PushTestBegins hands the agent when each test of the replayed recording
// began, so it moves the run's clock per test (models.TestBeginsReq). They
// are given again to a restarted agent, like the scope table. An agent that
// cannot take them is an error: the tests would run on another clock than
// their recording's.
func (a *AgentClient) PushTestBegins(ctx context.Context, begins map[string]time.Time, real map[string][]string) (retErr error) {
	defer func() {
		if retErr == nil && a.session().recording() {
			recorded, recordedReal := maps.Clone(begins), maps.Clone(real)
			a.session().record(stepTestBegins, func(c context.Context) error { return a.PushTestBegins(c, recorded, recordedReal) })
		}
	}()
	body, err := json.Marshal(models.TestBeginsReq{Begins: begins, Real: real})
	if err != nil {
		return fmt.Errorf("failed to marshal the tests' recorded begins: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/replay/clock", a.conf.Agent.AgentURI), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send the agent the tests' recorded begins: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("the agent did not take the tests' recorded begins: status %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// PushScopeTable hands the agent the replay-time per-test name→mock-names table
// (from mappings.yaml) so the runner's /agent/scope/begin calls can restrict the
// served pool per test. A missing endpoint (older agent) is a no-op.
func (a *AgentClient) PushScopeTable(ctx context.Context, table map[string][]string) (retErr error) {
	defer func() {
		if retErr == nil && a.session().recording() {
			recorded := maps.Clone(table)
			a.session().record(stepScopeTable, func(c context.Context) error { return a.PushScopeTable(c, recorded) })
		}
	}()
	url := fmt.Sprintf("%s/scope/table", a.conf.Agent.AgentURI)
	body, err := json.Marshal(models.ScopeTableReq{Mappings: table})
	if err != nil {
		return fmt.Errorf("failed to marshal scope table: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create scope-table request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to push scope table: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode == http.StatusNotFound {
		return nil
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("scope table returned status %d: %s", res.StatusCode, string(b))
	}
	return nil
}

// PushScopeGate tells the agent which tests should run this replay; run == nil
// lets every test run. The gate is made again on a restarted agent, like the
// scope table. An agent that cannot gate (older: 404; or 501) returns
// models.ErrScopeGateUnsupported.
func (a *AgentClient) PushScopeGate(ctx context.Context, run []string, reason string) (retErr error) {
	defer func() {
		if retErr == nil && a.session().recording() {
			recorded := slices.Clone(run)
			a.session().record(stepScopeGate, func(c context.Context) error { return a.PushScopeGate(c, recorded, reason) })
		}
	}()
	body, err := json.Marshal(models.ScopeGateReq{Run: run, Reason: reason})
	if err != nil {
		return fmt.Errorf("failed to marshal the scope gate: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/replay/gate", a.conf.Agent.AgentURI), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create the scope-gate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to push the scope gate: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusNotImplemented {
		return models.ErrScopeGateUnsupported
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("scope gate returned status %d: %s", res.StatusCode, string(b))
	}
	return nil
}

// PushScopeSkips hands the agent the proven model's gate: the tests to skip,
// by ID, each with its reason, and the tests each test needs to have begun
// before it (models.ScopeGateReq.Needs); an empty map skips nothing. Made
// again on a restarted agent, like the scope gate. Any answer but 200 is a
// failure: a replay that cannot skip what CI skips refuses.
func (a *AgentClient) PushScopeSkips(ctx context.Context, skip map[string]string, needs map[string][]models.ScopeNeed) (retErr error) {
	defer func() {
		if retErr == nil && a.session().recording() {
			recorded, recordedNeeds := maps.Clone(skip), maps.Clone(needs)
			a.session().record(stepScopeGate, func(c context.Context) error { return a.PushScopeSkips(c, recorded, recordedNeeds) })
		}
	}()
	body, err := json.Marshal(models.ScopeGateReq{Skip: skip, Needs: needs})
	if err != nil {
		return fmt.Errorf("failed to marshal the tests to skip: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/replay/gate", a.conf.Agent.AgentURI), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create the scope-gate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to push the tests to skip: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	answer, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the agent refused the tests to skip: status %d: %s", res.StatusCode, string(answer))
	}
	var ack models.ScopeGateResp
	if err := json.Unmarshal(answer, &ack); err != nil || ack.V != models.ScopeProtocolVersion {
		return fmt.Errorf("the agent did not acknowledge the tests to skip (answer %q): it predates keploy's scope protocol v1, or runs without KEPLOY_PROVEN=1", string(answer))
	}
	if ack.Skips != len(skip) || ack.Needs != len(needs) {
		return fmt.Errorf("the agent applies %d of the %d tests to skip, and the needs of %d of the %d tests that need another", ack.Skips, len(skip), ack.Needs, len(needs))
	}
	return nil
}

// DrainCapturedMocks fetches (and clears) the mocks the proxy captured on miss
// during a `--on-miss record` replay session. The CLI appends them to the set.
func (a *AgentClient) DrainCapturedMocks(ctx context.Context) ([]*models.Mock, error) {
	url := fmt.Sprintf("%s/mock/captured", a.conf.Agent.AgentURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create captured-mocks request: %w", err)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get captured mocks: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("captured mocks returned status %d: %s", res.StatusCode, string(b))
	}
	var mocks []*models.Mock
	if err := gob.NewDecoder(res.Body).Decode(&mocks); err != nil {
		return nil, fmt.Errorf("failed to decode captured mocks: %w", err)
	}
	return mocks, nil
}

// fromContainerStopBudget bounds stopping and starting the user's own container
// around a --from-container session. Generous: the container gets its
// configured stop signal and grace period first, and a slow shutdown is the
// app behaving correctly rather than a fault.
const fromContainerStopBudget = 45 * time.Second

// fromContainerRestoreBudget bounds starting the user's container again. Longer
// than the stop budget because it retries: the agent container holds the app's
// published ports until it exits, and the bind only frees once it has.
const fromContainerRestoreBudget = 90 * time.Second

// appNetworkingFromContainer reads the published ports, user networks and host
// entries off an already-running container, in the shapes GenerateDockerCommand
// expects: AppPorts as whole `-p …` flags, AppNetworks as bare names,
// AppExtraHosts as NAME:ADDRESS, AppDNS as the container's own resolver
// settings.
//
// The agent publishes on the app's behalf, because the app shares the agent's
// network namespace and therefore cannot publish anything itself. That is true
// of the command-string path too; the difference is where the values come from.
// A regex over a command string sees one spelling of each flag and nothing the
// user did not type; an inspect sees what the container actually has.
//
// Network ALIASES are not carried over yet - they are attached per-network with
// a separate flag - so a dependency that resolves the app by alias rather than
// by container name still needs that alias set by hand.
func (a *AgentClient) appNetworkingFromContainer(ctx context.Context, name string) (ports, networks, hosts []string, dns models.AppDNS, err error) {
	inspect, err := a.dockerClient.ContainerInspect(ctx, name)
	if err != nil {
		return nil, nil, nil, models.AppDNS{}, err
	}
	// Its host entries and resolver settings, which its replacement cannot
	// carry under the agent's network mode (replicaConfig): the agent
	// container carries them (AppExtraHosts, AppDNS).
	if inspect.HostConfig != nil {
		hosts = append(hosts, inspect.HostConfig.ExtraHosts...)
		dns = models.AppDNS{
			Servers: append([]string(nil), inspect.HostConfig.DNS...),
			Search:  append([]string(nil), inspect.HostConfig.DNSSearch...),
			Options: append([]string(nil), inspect.HostConfig.DNSOptions...),
		}
	}

	// PortBindings is the REQUEST ("-p 8080" -> HostPort ""), which is what to
	// re-publish. NetworkSettings.Ports is the RESULT, and re-publishing that
	// would pin whatever ephemeral host port the daemon happened to pick.
	if inspect.HostConfig != nil {
		// `docker run -P` publishes every exposed port to a random host port
		// and leaves PortBindings empty, so reading bindings alone republishes
		// nothing and the app is unreachable for the whole session.
		if inspect.HostConfig.PublishAllPorts {
			ports = append(ports, "-P")
		}
		for containerPort, bindings := range inspect.HostConfig.PortBindings {
			for _, b := range bindings {
				spec := b.HostPort + ":" + containerPort.Port()
				if b.HostIP != "" {
					hostIP := b.HostIP
					// An IPv6 address has to be bracketed or the colons in it
					// are indistinguishable from the spec's own separators:
					// "::1:5353:53" is rejected as having too many colons.
					if strings.Contains(hostIP, ":") {
						hostIP = "[" + hostIP + "]"
					}
					spec = hostIP + ":" + spec
				}
				if proto := containerPort.Proto(); proto != "" && proto != "tcp" {
					spec += "/" + proto
				}
				ports = append(ports, "-p "+spec)
			}
		}
	}
	// Sorted so a re-run produces the same agent command; map iteration order
	// would otherwise churn it.
	sort.Strings(ports)

	if inspect.NetworkSettings != nil {
		for netName := range inspect.NetworkSettings.Networks {
			// The predefined networks are not joinable by name in the way a
			// user network is: bridge is the default anyway, and host/none are
			// incompatible with the namespace sharing keploy needs.
			switch netName {
			case "bridge", "host", "none":
				continue
			}
			networks = append(networks, netName)
		}
	}
	sort.Strings(networks)
	return ports, networks, hosts, dns, nil
}

// RestoreFromContainer removes keploy's copy of the user's container and starts
// the user's own again. Safe to call from every teardown path and from as many
// of them as reach it: the work happens exactly once.
//
// Best-effort by design. The recording is already on disk by the time this
// runs, so failing the session over a container that would not restart would
// turn a cleanup problem into a lost recording - but every failure names the
// container, so a user left with a stopped one knows which.
func (a *AgentClient) RestoreFromContainer() {
	if a.fromContainer == "" {
		return
	}
	a.restoreFromContainerOnce.Do(func() {
		// Order matters, and it is the mirror of the bring-up. The AGENT holds
		// the app's published ports for the whole session - it publishes on the
		// app's behalf, because the app shares its network namespace and cannot
		// publish anything itself - so the source cannot have those ports back
		// until the agent container is gone.
		//
		// Nothing else has stopped it by this point on the ordinary path: the
		// agent container dies when the agent context is cancelled, and every
		// service cancels that AFTER its shutdown notification. So this stops it
		// here rather than waiting for a teardown stage it cannot see.
		if usrApp, err := a.getApp(); err == nil {
			usrApp.RemoveReplacement()
		}
		run := a.currentRun()
		a.stopAgent()
		if !a.fromContainerWasRunning {
			return
		}
		// The agent holds the app's ports until its container has exited:
		// restore after that -- or, as a drain gives up, once the wait has run
		// its bound with no stop holding the drains, or once its stop killed
		// it at once at the user's Ctrl+C (run.letGo; utils.AwaitAgentExit).
		// restoreSourceContainer then retries the start while the ports are
		// still held.
		if run != nil && !utils.AwaitAgentExit(run.gone, agentExitBound, run.letGo) {
			a.logger.Warn("Restoring your container while the keploy agent container is still shutting down",
				zap.String("container", a.fromContainer))
		}
		a.restoreSourceContainer(a.fromContainer)
	})
}

// releaseSourceContainer stops the container a --from-container run is going to
// re-create, and reports whether it was running. Stopping rather than removing:
// it is the user's container and they asked to record against it, not to lose
// it. restoreSourceContainer puts it back.
func (a *AgentClient) releaseSourceContainer(ctx context.Context, name string) (bool, error) {
	inspect, err := a.dockerClient.ContainerInspect(ctx, name)
	if err != nil {
		return false, err
	}
	if inspect.ContainerJSONBase == nil || inspect.State == nil || !inspect.State.Running {
		return false, nil
	}
	a.fromContainerSpec = specFromInspect(inspect)
	a.logger.Info("stopping your container so keploy can run a copy of it under its own namespaces",
		zap.String("container", name))
	stopCtx, cancel := context.WithTimeout(ctx, fromContainerStopBudget)
	defer cancel()
	if err := a.dockerClient.ContainerStop(stopCtx, inspect.ID, container.StopOptions{}); err != nil {
		return true, err
	}
	return true, nil
}

// sourceContainerSpec is the user's container as it was before keploy touched
// it: enough to build it again, including the compose labels that keep
// `docker compose` recognising it as its own.
type sourceContainerSpec struct {
	name       string
	config     *container.Config
	hostConfig *container.HostConfig
	networks   map[string]*network.EndpointSettings
	// imageID pins the image by digest. Config.Image is a TAG, and a tag can
	// stop resolving between the capture and the re-create - rebuilt, retagged,
	// pruned - while the id it resolved to is still on the machine.
	imageID string
	// mounts is the only place anonymous volume NAMES appear. Without it a
	// re-create hands the app brand new empty volumes and orphans its data.
	mounts []mount.Mount
	// platform matters on a machine whose default differs from the container's,
	// which is every Apple Silicon host running an amd64 image.
	platform *ocispec.Platform
	// id is what says the container answering to this name later is the same
	// one keploy stopped. Names get reused; a rebuild is destructive.
	id string
}

// backupSuffix marks the container while its replacement is built. The user's
// container is renamed aside rather than removed, so a failed re-create can put
// it back instead of leaving them with nothing.
//
// The name carries a timestamp because it has to be free: a backup stranded by
// an earlier failed run would otherwise take the one name this can use and
// block the repair for good.
const backupSuffix = "-keploy-restore-backup"

func backupNameFor(name string) string {
	return fmt.Sprintf("%s%s-%d", name, backupSuffix, time.Now().UnixNano())
}

// recreateBudget is the destructive sequence's OWN budget, never inherited.
// The restore loop can spend most of its budget waiting for the agent to
// release the app's ports, and a stop/create/start sequence that runs out of
// time halfway is how a container gets lost.
//
// A variable so a test can exhaust it: what a rollback does once the budget has
// run out is the whole reason it has a context of its own.
var recreateBudget = 45 * time.Second

// rollbackBudget is separate again, because a rollback exists to run after
// something has already overrun: reusing the budget that just expired means the
// daemon refuses every call and the container stays under a name the user never
// chose.
const rollbackBudget = 30 * time.Second

func specFromInspect(inspect container.InspectResponse) *sourceContainerSpec {
	if inspect.ContainerJSONBase == nil || inspect.Config == nil || inspect.HostConfig == nil {
		return nil
	}
	spec := &sourceContainerSpec{
		name:       strings.TrimPrefix(inspect.Name, "/"),
		id:         inspect.ID,
		config:     inspect.Config,
		hostConfig: inspect.HostConfig,
		imageID:    inspect.Image,
		mounts:     app.MountsFrom(inspect, inspect.HostConfig.Mounts),
	}
	if inspect.NetworkSettings != nil {
		spec.networks = inspect.NetworkSettings.Networks
	}
	if inspect.ImageManifestDescriptor != nil {
		spec.platform = inspect.ImageManifestDescriptor.Platform
	}
	return spec
}

// primaryNetwork is the network the container is created on, so it is never
// briefly attached to nothing. It returns the key into the endpoint map, which
// NetworkMode is not always spelled as.
//
// NetworkMode reads "default" for a container on the default bridge, and can be
// a network ID for one started with `--network <id>`. Either way it matches no
// key in the map, leaving the primary endpoint's aliases behind - and the
// aliases are how the rest of the project reaches this container by name.
func primaryNetwork(hostConfig *container.HostConfig, networks map[string]*network.EndpointSettings) string {
	mode := string(hostConfig.NetworkMode)
	if _, ok := networks[mode]; ok {
		return mode
	}
	for name, endpoint := range networks {
		if endpoint != nil && endpoint.NetworkID == mode {
			return name
		}
	}
	if mode == "default" {
		return "bridge"
	}
	return mode
}

// startWithinBudget starts a container, retrying until ctx is done. Every
// failure worth retrying here is something letting go of a resource: the agent
// releasing the app's published ports as it exits, or a network settling.
func (a *AgentClient) startWithinBudget(ctx context.Context, id string) error {
	for {
		err := a.dockerClient.ContainerStart(ctx, id, container.StartOptions{})
		if err == nil {
			return nil
		}
		// A container that is not there will not appear, and a configuration
		// the daemon refuses will be refused again.
		if errdefs.IsNotFound(err) || errdefs.IsInvalidParameter(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
}

// endpointFor keeps what the user declared and drops the identity of the
// endpoint Docker discarded.
//
// The aliases are how the rest of the project reaches this container by name.
// IPAMConfig, DriverOpts and GwPriority are declared too: GwPriority decides
// which network carries the default route on a multi-network container, and the
// daemon migrates per-endpoint sysctls into DriverOpts and then refuses the
// top-level spelling, so that key has to be replayed where it was found.
//
// The endpoint id and assigned address belong to the dead endpoint and Docker
// issues fresh ones. MacAddress is left behind with them: it is only an input
// before the container runs, and reads back as whatever the daemon generated -
// on a bridge that is derived from the assigned IP, so replaying it as a
// request can pin a second interface to a MAC another container now holds.
func endpointFor(settings *network.EndpointSettings) *network.EndpointSettings {
	if settings == nil {
		return &network.EndpointSettings{}
	}
	return &network.EndpointSettings{
		Aliases:    settings.Aliases,
		IPAMConfig: settings.IPAMConfig,
		DriverOpts: settings.DriverOpts,
		GwPriority: settings.GwPriority,
	}
}

// restoreSourceContainer puts the user's container back after a
// --from-container session.
//
// A plain start is tried first and then VERIFIED, because it is enough whenever
// the container came through with its network intact. When it has not, the
// container is rebuilt from the spec captured before it was stopped.
//
// Rebuilding is not the first choice - it is the only one that works. A
// container that lost its network endpoint cannot be repaired in place: a
// reconnect on the stopped container reports success and is gone again after
// the start, and ports are published onto an endpoint, so no start or restart
// will ever bind them. `docker compose up -d` fixes it precisely because it
// re-creates.
func (a *AgentClient) restoreSourceContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), fromContainerRestoreBudget)
	defer cancel()

	// --from-container takes an id just as happily as a name, and a rebuilt
	// container has a new one. Addressing it by name from here means the
	// verification after a rebuild is not looking up the id that was removed.
	if a.fromContainerSpec != nil && a.fromContainerSpec.name != "" {
		name = a.fromContainerSpec.name
	}

	var lastErr error
	recreated := false
	explained := false
	inspectFailed := false
	for {
		lastErr = a.dockerClient.ContainerStart(ctx, name, container.StartOptions{})

		// A container that is GONE cannot be started, so waiting for it to
		// start is waiting for nothing. --rm is the ordinary way to get here:
		// AutoRemove fires on any exit, including the stop that began the
		// session, so the rebuild is the only thing that returns it at all.
		if lastErr != nil && errdefs.IsNotFound(lastErr) {
			switch {
			case recreated || a.fromContainerSpec == nil:
				utils.LogError(a.logger, lastErr, "your container no longer exists and keploy cannot rebuild it",
					zap.String("container", name),
					zap.String("next_step", "start it again: docker compose up -d, or docker run it as before"))
				return
			default:
				recreated = true
				a.logger.Info("your container was removed when it stopped; rebuilding it as it was",
					zap.String("container", name))
				if err := a.recreateSourceContainer(); err != nil {
					utils.LogError(a.logger, err, "could not rebuild your container",
						zap.String("container", name))
					return
				}
				continue
			}
		}

		if lastErr == nil {
			problems, running, err := a.restoreShortfall(ctx, name)
			switch {
			case err != nil && !inspectFailed:
				// One failed call is not an answer about the container, and
				// every other transient in this loop is retried.
				inspectFailed = true
				a.logger.Debug("could not check on your container; trying once more",
					zap.String("container", name), zap.Error(err))
			case !running && err == nil:
				// restoreShortfall has already reported the exit. Retrying
				// would only start it again to no effect.
				return
			case err != nil:
				a.logger.Warn("started your container again but could not verify its network and ports",
					zap.String("container", name), zap.Error(err))
				return
			case len(problems) == 0:
				a.logger.Info("started your container again", zap.String("container", name))
				return
			case recreated || a.fromContainerSpec == nil:
				// Rebuilding was the repair, or there is no spec to rebuild
				// from. Either way another pass cannot help, and looping would
				// only stall teardown on a context Ctrl+C cannot reach.
				a.logger.Warn("your container is running but not fully back",
					zap.String("container", name), zap.Strings("problems", problems),
					zap.String("next_step", "recreate it: docker compose up -d --force-recreate, or docker run it again"))
				return
			default:
				recreated = true
				a.logger.Info("your container came back without its network; rebuilding it as it was",
					zap.String("container", name), zap.Strings("problems", problems))
				if err := a.recreateSourceContainer(); err != nil {
					utils.LogError(a.logger, err, "could not rebuild your container",
						zap.String("container", name))
					return
				}
				// Round again to verify. The rebuild has already started it,
				// so its start is a no-op, while falling through to the wait
				// below would sleep for nothing and report a start error that
				// does not exist.
				continue
			}
		}

		if lastErr != nil && !explained {
			// Otherwise this is a silent minute and a half at the end of a run.
			// The usual cause is the agent still holding the published ports,
			// which resolves itself as it exits.
			explained = true
			a.logger.Info("waiting to start your container again",
				zap.String("container", name), zap.Error(lastErr))
		}

		select {
		case <-ctx.Done():
			utils.LogError(a.logger, lastErr, "could not restore your container's network and ports",
				zap.String("container", name),
				zap.String("next_step", "recreate it: docker compose up -d --force-recreate, or docker run it again"))
			return
		case <-time.After(time.Second):
		}
	}
}

// recreateSourceContainer rebuilds the user's container from the captured spec,
// under its own name.
//
// The original is RENAMED ASIDE rather than removed, and only deleted once its
// replacement is running. Removing first means every failure in between -
// an image tag that no longer resolves, a config the daemon refuses, a deadline
// running out - leaves the user with no container at all and the only copy of
// its spec inside a process that is exiting. On any failure the original is put
// back under its own name and restarted, which leaves the user with a container
// that is running but unreachable - the same state the failed session left, and
// one they can still recover by hand.
func (a *AgentClient) recreateSourceContainer() error {
	spec := a.fromContainerSpec
	if spec == nil {
		return errors.New("no captured spec for the source container")
	}

	// Its own budget, never the caller's remainder.
	ctx, cancel := context.WithTimeout(context.Background(), recreateBudget)
	defer cancel()

	backup := ""
	restoreBackup := func(cause error) error {
		if backup == "" {
			return cause
		}
		// Its own context: every path that reaches here may have exhausted the
		// recreate budget, and a rollback on a dead context fails instantly and
		// leaves the container under a name the user never chose.
		ctx, cancel := context.WithTimeout(context.Background(), rollbackBudget)
		defer cancel()
		if err := a.dockerClient.ContainerRename(ctx, backup, spec.name); err != nil {
			// The container is not lost, but it is neither where the user left
			// it nor running, so the message has to carry both steps back.
			if startErr := a.dockerClient.ContainerStart(ctx, backup, container.StartOptions{}); startErr != nil {
				a.logger.Debug("could not start the container under its temporary name",
					zap.String("container", backup), zap.Error(startErr))
			}
			utils.LogError(a.logger, err, "your container still exists, under a temporary name",
				zap.String("container", backup), zap.String("expected_name", spec.name),
				zap.String("next_step", fmt.Sprintf("docker rename %s %s && docker start %s", backup, spec.name, spec.name)))
			return cause
		}
		if err := a.dockerClient.ContainerStart(ctx, spec.name, container.StartOptions{}); err != nil {
			a.logger.Warn("put your container back but could not start it",
				zap.String("container", spec.name), zap.Error(err),
				zap.String("next_step", "docker start "+spec.name))
		}
		return cause
	}

	// A container started with --rm is already gone: AutoRemove fires on any
	// exit, including the stop that began the session. There is nothing to set
	// aside, and rebuilding is the only way the user gets it back at all.
	//
	// Only a NOT-FOUND means that. Any other inspect failure is a daemon
	// problem, and reading it as "gone" would go on to create over a container
	// that is still there.
	inspect, inspectErr := a.dockerClient.ContainerInspect(ctx, spec.name)
	switch {
	case inspectErr != nil && !errdefs.IsNotFound(inspectErr):
		return fmt.Errorf("could not check on %s before rebuilding it: %w", spec.name, inspectErr)
	case inspectErr == nil:
		// The name is all this has to go on otherwise, and a name can be taken
		// by something else: a compose run in another terminal after keploy
		// stopped the container, or a fresh container reusing a name --rm
		// freed. Rebuilding then means force-removing a container keploy never
		// captured and replacing it with a stale spec.
		if spec.id != "" && inspect.ContainerJSONBase != nil && inspect.ID != spec.id {
			return fmt.Errorf("%s is a different container than the one keploy stopped (%s); leaving it alone",
				spec.name, inspect.ID)
		}
		backup = backupNameFor(spec.name)
		if err := a.dockerClient.ContainerRename(ctx, spec.name, backup); err != nil {
			return fmt.Errorf("could not set %s aside to rebuild it: %w", spec.name, err)
		}
		// The original is RUNNING at this point - the restore loop started it
		// and the shortfall check confirmed it - and renaming does not stop it.
		// Creating the replacement now would start a second container on the
		// same volumes and the same host ports. It gets its own stop signal and
		// grace period, because this is the user's app shutting down rather
		// than a fault.
		//
		// On its own deadline, not a slice of the rebuild's: a container with a
		// long stop grace period would otherwise spend the whole budget here
		// and leave the create to fail on an expired context.
		stopCtx, cancelStop := context.WithTimeout(context.Background(), fromContainerStopBudget)
		err := a.dockerClient.ContainerStop(stopCtx, backup, container.StopOptions{})
		cancelStop()
		if err != nil {
			// Aborting rather than pressing on. The broken container publishes
			// nothing, so nothing would refuse the replacement's ports either -
			// two containers would end up on the same volumes, and the
			// force-remove afterwards would SIGKILL one of them.
			return restoreBackup(fmt.Errorf("could not stop %s before rebuilding it: %w", spec.name, err))
		}
	}

	config := *spec.config
	hostConfig := *spec.hostConfig
	// Deprecated in favour of the per-endpoint field, and read back as whatever
	// the daemon generated, so replaying it requests a MAC that was never asked
	// for. Dropped for the same reason endpointFor drops its copy.
	config.MacAddress = ""
	// Mounts are authoritative; the create is refused if the same target
	// arrives twice through Binds or the image's VOLUME set.
	hostConfig.Mounts = spec.mounts
	hostConfig.Binds = nil
	hostConfig.VolumesFrom = nil
	config.Volumes = nil
	// Inspect rebuilds Links as `/child:/parent/alias` rather than what was
	// asked for, so feeding it back produces an alias containing a slash and
	// the daemon refuses the create. Dropped rather than guessed at: a legacy
	// --link is recoverable by hand, a deleted container is not.
	if len(hostConfig.Links) > 0 {
		a.logger.Warn("your container used legacy --link, which cannot be reproduced exactly; rebuilding without it",
			zap.String("container", spec.name), zap.Strings("links", hostConfig.Links))
		hostConfig.Links = nil
	}

	endpoints := map[string]*network.EndpointSettings{}
	primary := primaryNetwork(spec.hostConfig, spec.networks)
	if settings, ok := spec.networks[primary]; ok {
		endpoints[primary] = endpointFor(settings)
	}

	created, err := a.dockerClient.ContainerCreate(ctx, &config, &hostConfig,
		&network.NetworkingConfig{EndpointsConfig: endpoints}, spec.platform, spec.name)
	if err != nil && errdefs.IsNotFound(err) && spec.imageID != "" && config.Image != spec.imageID {
		// The tag stopped resolving between the capture and now. The id it
		// resolved to is still on the machine.
		a.logger.Warn("could not rebuild from the image tag; using the image it was running",
			zap.String("container", spec.name), zap.String("tag", config.Image),
			zap.String("image", spec.imageID), zap.Error(err))
		config.Image = spec.imageID
		created, err = a.dockerClient.ContainerCreate(ctx, &config, &hostConfig,
			&network.NetworkingConfig{EndpointsConfig: endpoints}, spec.platform, spec.name)
	}
	if err != nil {
		return restoreBackup(fmt.Errorf("could not rebuild %s: %w", spec.name, err))
	}

	secondary := make([]string, 0, len(spec.networks))
	for netName := range spec.networks {
		if netName != primary {
			secondary = append(secondary, netName)
		}
	}
	sort.Strings(secondary)
	for _, netName := range secondary {
		if err := a.dockerClient.NetworkConnect(ctx, netName, created.ID, endpointFor(spec.networks[netName])); err != nil {
			a.logger.Warn("could not put your container back on one of its networks",
				zap.String("container", spec.name), zap.String("network", netName), zap.Error(err))
		}
	}

	// Retried for as long as the budget allows, not attempted once. The agent
	// container publishes the app's ports on its behalf and is stopped
	// asynchronously, so a rebuild that reaches this point promptly can find
	// 9410 still bound - and rolling back over a transient bind would abandon
	// a repair that has already stopped the user's container.
	err = a.startWithinBudget(ctx, created.ID)
	if err != nil {
		// The rebuilt container exists; only the start failed. It has to go
		// before the original can have its name back. Its own context, for the
		// same reason the rollback has one.
		rmCtx, cancelRm := context.WithTimeout(context.Background(), rollbackBudget)
		defer cancelRm()
		if rmErr := a.dockerClient.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			// Not a detail: keploy's container is holding the name the user's
			// container is about to be renamed back to, so that rename fails
			// too and nothing else says where the name went.
			utils.LogError(a.logger, rmErr, "could not remove the container keploy built; it is holding your container's name",
				zap.String("container", spec.name))
		}
		return restoreBackup(fmt.Errorf("could not start the rebuilt %s: %w", spec.name, err))
	}

	if backup != "" {
		// A fresh deadline for the same reason the rollback has one: by here the
		// rebuild's budget has paid for an inspect, a rename, a stop, a create,
		// the network connects and a start.
		rmCtx, cancelRm := context.WithTimeout(context.Background(), rollbackBudget)
		defer cancelRm()
		// RemoveVolumes stays off: the replacement mounts those same volumes by
		// name, and this copy is only the shell the data used to hang off.
		if err := a.dockerClient.ContainerRemove(rmCtx, backup, container.RemoveOptions{Force: true}); err != nil {
			// Harmless but visible: a stopped duplicate under a temporary name.
			a.logger.Warn("rebuilt your container but could not remove the old copy; remove it by hand",
				zap.String("container", backup), zap.Error(err))
		}
	}
	return nil
}

// restoreShortfall reports what is still wrong with a restored container: the
// networks it should be on but is not, and the ports it declares but has not
// published. Empty means it really is back.
func (a *AgentClient) restoreShortfall(ctx context.Context, name string) (problems []string, running bool, err error) {
	inspect, err := a.dockerClient.ContainerInspect(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if inspect.ContainerJSONBase == nil {
		return nil, false, errors.New("docker returned an inspect with no container in it")
	}

	// A container that is not RUNNING looks exactly like the bug: no endpoint
	// id, no published ports. Judging it here would rebuild a container that
	// was never broken - including one that crashes on boot precisely because
	// its dependency moved during the session, which is the case this is
	// supposed to help with.
	if inspect.State == nil || !inspect.State.Running {
		exit := 0
		if inspect.State != nil {
			exit = inspect.State.ExitCode
		}
		a.logger.Warn("your container started and then exited; keploy has left it alone",
			zap.String("container", name), zap.Int("exit_code", exit))
		return nil, false, nil
	}

	attached := map[string]bool{}
	if inspect.NetworkSettings != nil {
		for netName, endpoint := range inspect.NetworkSettings.Networks {
			// A dropped endpoint is still LISTED with nothing in it, so the
			// endpoint id is what says whether it is really attached.
			if endpoint != nil && endpoint.EndpointID != "" {
				attached[netName] = true
			}
		}
	}
	var missingNets []string
	if a.fromContainerSpec != nil {
		for netName := range a.fromContainerSpec.networks {
			if !attached[netName] {
				missingNets = append(missingNets, netName)
			}
		}
	}
	sort.Strings(missingNets)
	if len(missingNets) > 0 {
		problems = append(problems, "not on "+strings.Join(missingNets, ", "))
	}

	// host and none ignore port bindings by design, so a container declaring
	// both would report a shortfall it can never clear - and be rebuilt for
	// nothing, repeatedly.
	if inspect.HostConfig != nil && !inspect.HostConfig.NetworkMode.IsHost() && !inspect.HostConfig.NetworkMode.IsNone() {
		bound := 0
		if inspect.NetworkSettings != nil {
			for _, bindings := range inspect.NetworkSettings.Ports {
				bound += len(bindings)
			}
		}

		var unpublished []string
		for port, bindings := range inspect.HostConfig.PortBindings {
			if len(bindings) == 0 {
				continue
			}
			if inspect.NetworkSettings == nil || len(inspect.NetworkSettings.Ports[port]) == 0 {
				unpublished = append(unpublished, string(port))
			}
		}
		sort.Strings(unpublished)
		if len(unpublished) > 0 {
			problems = append(problems, "not publishing "+strings.Join(unpublished, ", "))
		}

		// `docker run -P` declares nothing in PortBindings - the daemon assigns
		// a host port per exposed port - so there is no binding to compare and
		// the loss would go unnoticed.
		if inspect.HostConfig.PublishAllPorts && len(inspect.Config.ExposedPorts) > 0 && bound == 0 {
			problems = append(problems, "not publishing any of its exposed ports")
		}
	}

	return problems, true, nil
}

func (a *AgentClient) GetIDPairs(ctx context.Context) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.conf.Agent.AgentURI+"/ids", nil)
	if err != nil {
		return nil, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		rawBody, _ := readAgentBody(res)
		return nil, agentRespErr("get id pairs", res, rawBody)
	}
	var pairs map[string]string
	if err := json.NewDecoder(res.Body).Decode(&pairs); err != nil {
		return nil, err
	}
	return pairs, nil
}

// TestVariables returns the test variables the agent bound in the current test
// (GET /agent/variables; proven switch).
func (a *AgentClient) TestVariables(ctx context.Context) ([]models.VariableBinding, error) {
	if a.conf.Agent.AgentURI == "" {
		return nil, errors.New("keploy has no agent to read the test's variables from")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.conf.Agent.AgentURI+"/variables", nil)
	if err != nil {
		return nil, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		rawBody, _ := readAgentBody(res)
		return nil, agentRespErr("get test variables", res, rawBody)
	}
	var out models.TestVariablesResp
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Variables, nil
}

// startWithConsole starts an agent's cmd through start, its output going to
// keploy's console through a utils.ChildConsole: so the agent's lines are cut
// of the values the log redactor holds, when it holds any. Call the console's
// Exited once cmd.Wait has returned.
func startWithConsole(cmd *exec.Cmd, start func(*exec.Cmd) error) (*utils.ChildConsole, error) {
	console, err := utils.SetChildConsole(cmd)
	if err != nil {
		return nil, err
	}
	err = start(cmd)
	console.Started()
	if err != nil {
		return nil, err
	}
	return console, nil
}
