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
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.keploy.io/server/v3/pkg/agent/token"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	ptls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	agentUtils "go.keploy.io/server/v3/pkg/platform/http/utils"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	agentReadyRetryInterval = 2 * time.Second
)

// TODO: Need to refactor this file
type AgentClient struct {
	logger       *zap.Logger
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	apps         sync.Map
	client       http.Client
	// hookClient serves the /hooks/* calls, which want a request deadline.
	// Built here, not at the call sites, so it carries the bearer token like
	// every other request to the agent.
	hookClient  http.Client
	conf        *config.Config
	agentCmd    *exec.Cmd             // Track the agent process
	agentPTY    *agentUtils.PTYHandle // Track the PTY handle for interactive commands
	mu          sync.Mutex
	agentCancel context.CancelFunc // Function to cancel the agent context
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
}

// var initStopScript []byte

func New(logger *zap.Logger, client kdocker.Client, c *config.Config) *AgentClient {

	return &AgentClient{
		logger:       logger,
		dockerClient: client,
		client:       agentHTTPClient(token.Session()),
		hookClient:   agentHTTPClientWithTimeout(token.Session(), agentHookTimeout),
		conf:         c,
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

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get incoming: %s", err.Error())
	}
	if err := agentStreamStatus("get incoming", res); err != nil {
		return nil, err
	}

	// Create a channel to stream TestCase data
	tcChan := make(chan *models.TestCase)

	// Determine stream type
	contentType := res.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		utils.LogError(a.logger, err, "failed to parse content type", zap.String("content-type", contentType))
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			if res.Body != nil {
				res.Body.Close()
			}
			return nil, fmt.Errorf("missing multipart boundary in content-type: %s", contentType)
		}
		go func() {
			defer func() {
				close(tcChan)
				if res.Body != nil {
					res.Body.Close()
				}
			}()

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
							return
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
						return
					case tcChan <- pendingTestCase:
						pendingTestCase = nil
					}
				}
			}
		}()
	} else {
		// Legacy JSON stream
		go func() {
			defer func() {
				close(tcChan)

				err := res.Body.Close()
				if err != nil {
					utils.LogError(a.logger, err, "failed to close response body for incoming request")
				}
			}()

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
					return
				case tcChan <- &testCase:
					// Send the decoded test case to the channel
				}
			}
		}()
	}

	a.logger.Debug("Successfully connected to incoming test cases stream.")
	return tcChan, nil
}

// mockHandoffBuffer sizes the decoder -> consumer hand-off. Deep enough that a
// slow InsertMock cannot park the decoder behind a mock it has already read off
// the wire.
const mockHandoffBuffer = 1024

// mockHandoffGrace bounds how long the decoder waits for the consumer to take a
// mock before giving up on it. Only reachable if the consumer has stopped
// draining entirely; a busy one drains in milliseconds.
const mockHandoffGrace = 30 * time.Second

func (a *AgentClient) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {

	a.logger.Debug("Connecting to outgoing mocks stream...")

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

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get outgoing response: %s", err.Error())
	}
	if err := agentStreamStatus("get outgoing", res); err != nil {
		return nil, err
	}

	// Buffered. An unbuffered channel parks the decoder for as long as the
	// consumer spends inside InsertMock - and the first insert does mkdir +
	// create + YAML encode + flush, which is exactly when the last mock of a
	// recording arrives. A decoded mock waiting there was the widest of the
	// three places a trailing mock could be lost.
	mockChan := make(chan *models.Mock, mockHandoffBuffer)

	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return nil, fmt.Errorf("failed to get errorgroup from the context")
	}

	grp.Go(func() error {
		defer func() {
			close(mockChan)

			err := res.Body.Close()
			if err != nil {
				utils.LogError(a.logger, err, "failed to close response body for getoutgoing")
			}
		}()

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
			select {
			case mockChan <- &mock:
				// Send the decoded mock to the channel
			case <-time.After(mockHandoffGrace):
				utils.LogError(a.logger, nil, "dropping a decoded mock: the consumer did not accept it in time",
					zap.String("mock", mock.Name), zap.String("kind", string(mock.Kind)),
					zap.Duration("waited", mockHandoffGrace))
				return nil
			}
		}
		return nil
	})

	a.logger.Debug("Successfully connected to outgoing mocks stream.")

	return mockChan, nil
}

func (a *AgentClient) GetMappings(ctx context.Context, opts models.IncomingOptions) (<-chan models.TestMockMapping, error) {

	a.logger.Debug("Connecting to mappings stream...")

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/mappings", a.conf.Agent.AgentURI), nil)
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for mappings")
		return nil, fmt.Errorf("error creating request for mappings: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	// Make the HTTP request
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get mappings response: %s", err.Error())
	}
	// An agent predating test-mock mapping (keploy #3715, Feb 2026) has no
	// /mappings route and answers 404. That is version skew, not a failure, and
	// it must NOT be reported as one: this call sits inside the record errgroup
	// (pkg/service/record/record.go), so an error here aborts the entire
	// RECORDING. Before this status check existed the decoder goroutine simply
	// logged "failed to decode mapping from stream" and returned nil, and
	// recording completed without mappings — that degraded-but-working
	// behaviour is what a missing endpoint should still produce.
	//
	// Same tolerance BeginTestErrorCapture, GetScopeWindows, GetScopeTable and
	// DrainCapturedMocks already apply, and for the same reason StoreMocks
	// carries its legacy-framing fallback: the CLI ships ahead of the agent
	// image, so a lagging agent is the normal state during a rollout rather than
	// an edge case. /incoming and /outgoing need no such guard — both routes
	// predate #3016 and every agent that can serve a session has them.
	if res.StatusCode == http.StatusNotFound {
		if closeErr := res.Body.Close(); closeErr != nil {
			utils.LogError(a.logger, closeErr, "failed to close response body for getmappings")
		}
		a.logger.Warn("agent has no /mappings endpoint — recording without test-mock mappings",
			zap.String("consequence", "replay falls back to timestamp-based mock filtering for this recording"),
			zap.String("remedy", "upgrade the agent image to one built from keploy #3715 or later"))
		noMappings := make(chan models.TestMockMapping)
		close(noMappings)
		return noMappings, nil
	}
	if err := agentStreamStatus("get mappings", res); err != nil {
		return nil, err
	}

	mappingChan := make(chan models.TestMockMapping)

	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return nil, fmt.Errorf("failed to get errorgroup from the context")
	}

	grp.Go(func() error {
		defer func() {
			close(mappingChan)
			if err := res.Body.Close(); err != nil {
				utils.LogError(a.logger, err, "failed to close response body for getmappings")
			}
		}()

		decoder := json.NewDecoder(res.Body)

		for {
			var mapping models.TestMockMapping
			if err := decoder.Decode(&mapping); err != nil {
				if utils.IsShutdownError(err) {
					break
				}
				utils.LogError(a.logger, err, "failed to decode mapping from stream")
				break
			}

			select {
			case <-ctx.Done():
				return nil
			case mappingChan <- mapping:
			}
		}
		return nil
	})

	a.logger.Debug("Successfully connected to mappings stream.")
	return mappingChan, nil
}

func (a *AgentClient) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {

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

	return agentRespErr("mock outgoing", res, rawBody)
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

	requestBody := models.BeforeSimulateRequest{
		TimeStamp:    *timestamp,
		TestSetID:    testSetID,
		TestCaseName: tcName,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/before-simulate")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hookClient.Do(req)
	if err != nil {
		a.logger.Debug("failed to call agent hook", zap.String("endpoint", "/hooks/before-simulate"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil
}

func (a *AgentClient) AfterSimulate(ctx context.Context, tcName string, testSetID string) error {

	requestBody := models.AfterSimulateRequest{
		TestSetID:    testSetID,
		TestCaseName: tcName,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/after-simulate")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hookClient.Do(req)
	if err != nil {
		a.logger.Debug("failed to call agent hook", zap.String("endpoint", "/hooks/after-simulate"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil
}

func (a *AgentClient) BeforeTestRun(ctx context.Context, testRunID string) error {

	requestBody := models.BeforeTestRunReq{
		TestRunID: testRunID,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/before-test-run")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hookClient.Do(req)
	if err != nil {
		a.logger.Debug("failed to call agent hook", zap.String("endpoint", "/hooks/before-test-run"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil

}

func (a *AgentClient) BeforeTestSetCompose(ctx context.Context, testRunID string, testSetID string, firstRun bool) error {

	requestBody := models.BeforeTestSetCompose{
		TestRunID: testRunID,
		TestSetID: testSetID,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/before-test-set-compose")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hookClient.Do(req)
	if err != nil {
		a.logger.Debug("failed to call agent hook", zap.String("endpoint", "/hooks/before-test-set-compose"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil

}

func (a *AgentClient) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {

	requestBody := models.AfterTestRunReq{
		TestRunID:  testRunID,
		TestSetIDs: testSetIDs,
		Coverage:   coverage,
	}
	if a.conf.Agent.AgentURI == "" {
		return nil
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, "/hooks/after-test-run")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hookClient.Do(req)
	if err != nil {
		a.logger.Debug("failed to call agent hook", zap.String("endpoint", "/hooks/after-test-run"), zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		a.logger.Error("agent hook returned error", zap.Int("status", resp.StatusCode), zap.String("body", string(respBody)))
		return fmt.Errorf("agent hook failed: %d", resp.StatusCode)
	}
	return nil

}

// StoreMocks sends the mock corpus to the agent. It streams by default (a gob
// MockStreamHeader over an io.Pipe, then filtered then unfiltered mocks, one gob
// frame each) — streaming was added (#4327) so the agent doesn't buffer the
// whole corpus and OOM on large recordings.
//
// Backward compatibility: a pre-streaming agent (keploy <= v3.5.84) gob-decodes
// the request body as a single StoreMocksReq and fails the instant it meets the
// MockStreamHeader ("type mismatch: no fields matched"), returning HTTP 400.
// Rather than lockstep-break every rolling upgrade where the deployed agent
// image lags the controller (a fresh streaming controller talking to an
// already-running old agent), StoreMocks transparently retries such a 400 with
// the legacy single-shot framing the old agent understands. An agent old enough
// to reject the stream is old enough to have handled single-shot before — and
// predates the large-corpus recordings streaming exists for — so the fallback
// is safe; a genuine bad-request 400 simply fails again on the retry.
func (a *AgentClient) StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	// A dedicated cancelable context for the stream request: cancelling it tears
	// down the transport's read of the io.Pipe body, which unblocks the encoder
	// goroutine's pw.Write if the agent responded (e.g. 400) before consuming the
	// whole stream — so the fallback path can't leak that goroutine.
	streamCtx, cancelStream := context.WithCancel(ctx)

	res, err := a.storeMocksStream(streamCtx, filtered, unFiltered)
	if err != nil {
		cancelStream()
		return err
	}

	if res.StatusCode == http.StatusBadRequest {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		cancelStream()
		a.logger.Warn("agent rejected streaming /storemocks with HTTP 400; retrying with legacy single-shot framing. The usual cause is a deployed agent that predates streaming (keploy <= v3.5.84) during a rolling upgrade — align the agent image with the controller's keploy version to use streaming and avoid buffering the whole mock corpus. (A new agent that returns 400 for another reason will simply fail the legacy retry too, surfacing the real error.)")
		return a.storeMocksLegacy(ctx, filtered, unFiltered)
	}

	defer cancelStream()
	defer res.Body.Close()
	return decodeStoreMocksResp(res)
}

// storeMocksStream POSTs the corpus as a gob stream (MockStreamHeader + one
// frame per mock) and returns the raw response so the caller can decide whether
// to fall back on a 400.
func (a *AgentClient) storeMocksStream(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) (*http.Response, error) {
	pr, pw := io.Pipe()
	go func() {
		enc := gob.NewEncoder(pw)
		if err := enc.Encode(models.MockStreamHeader{
			FilteredCount:   len(filtered),
			UnfilteredCount: len(unFiltered),
		}); err != nil {
			_ = pw.CloseWithError(err)
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
				if err := enc.Encode(m); err != nil {
					return err
				}
			}
			return nil
		}
		if err := encodeAll(filtered); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := encodeAll(unFiltered); err != nil {
			_ = pw.CloseWithError(err)
			return
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
		return nil, fmt.Errorf("create request for storemocks: %s", err.Error())
	}
	req.Header.Set("Content-Type", models.StoreMocksStreamContentType)
	req.Header.Set("Accept", "application/x-gob")

	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request for storemocks: %s", err.Error())
	}
	return res, nil
}

// storeMocksLegacy POSTs the corpus in the pre-streaming single-shot framing:
// one gob-encoded StoreMocksReq with Content-Type application/x-gob. Used only
// as the compatibility fallback when a deployed agent rejects the stream (see
// StoreMocks). It buffers the whole corpus in memory the way the pre-#4327 path
// did — acceptable here because it only runs against an old agent that never
// supported streaming anyway.
func (a *AgentClient) storeMocksLegacy(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(models.StoreMocksReq{
		Filtered:   filtered,
		UnFiltered: unFiltered,
	}); err != nil {
		utils.LogError(a.logger, err, "failed to gob-encode request body for storemocks (legacy)")
		return fmt.Errorf("gob encode request for storemocks (legacy): %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/storemocks", a.conf.Agent.AgentURI), &buf)
	if err != nil {
		utils.LogError(a.logger, err, "failed to create legacy request for storemocks")
		return fmt.Errorf("create legacy request for storemocks: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/x-gob")
	req.Header.Set("Accept", "application/x-gob")

	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("send legacy request for storemocks: %s", err.Error())
	}
	defer res.Body.Close()
	return decodeStoreMocksResp(res)
}

// decodeStoreMocksResp reads the agent's single AgentResp gob reply, shared by
// the streaming and legacy paths so their response handling is identical.
func decodeStoreMocksResp(res *http.Response) error {
	// Non-2xx? Try to decode anyway; if that fails, return status text.
	// An OLD agent reaches the decode failure here on every error: its gob
	// encode aborted on the `error` interface field, having already written a
	// PARTIAL type descriptor, so the decode fails with "unexpected EOF" on a
	// truncated value rather than on an empty body — hence the bare
	// "storemocks http 500" this used to give. Unchanged for an old agent;
	// a new one now carries its reason in ErrorMsg.
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var fail models.AgentResp
		if err := gob.NewDecoder(res.Body).Decode(&fail); err != nil {
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
	requestBody := models.UpdateMockParamsReq{
		FilterParams: params,
	}

	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for updatemockparams")
		return fmt.Errorf("error marshaling request body for updatemockparams: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/updatemockparams", a.conf.Agent.AgentURI), bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to create request for updatemockparams")
		return fmt.Errorf("error creating request for update mock params: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request for updatemockparams: %s", err.Error())
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		if err := res.Body.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to close response body for updatemockparams")
		}
	}()

	rawBody, readErr := readAgentBody(res)
	if readErr != nil {
		return fmt.Errorf("failed to read response body for updatemockparams: %s", readErr.Error())
	}

	return agentRespErr("update mock params", res, rawBody)
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
// stored before any test fires: a replacement agent (the bring-up retry
// recreates the stack, agent included) reports a loaded count of zero.
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

func (a *AgentClient) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	app, err := a.getApp()
	if err != nil {
		utils.LogError(a.logger, err, "failed to get app while running")
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	}

	runAppErrGrp, runAppCtx := errgroup.WithContext(ctx)
	appErrCh := make(chan models.AppError, 1)
	defer func() {
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(a.logger, err, "failed to stop the app")
		}
	}()

	runAppErrGrp.Go(func() error {
		defer utils.Recover(a.logger)
		appErr := app.Run(runAppCtx)
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

	// Create a context for the agent that can be cancelled independently
	agentCtx, cancel := context.WithCancel(ctx)
	a.agentCancel = cancel
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
			err := a.startInDocker(agentCtx, a.logger, opts)
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
		err := a.startNativeAgent(agentCtx, opts, exited)
		if err != nil {
			cancel()
			return nil, err
		}
	}

	// Monitor agent process and cancel client context if agent stops using errgroup
	grp.Go(func() error {
		a.monitorAgent(ctx, agentCtx)
		return nil
	})

	return exited, nil
}

// startNativeAgent starts the keploy agent as a native process. The process's
// exit is sent on exited.
func (a *AgentClient) startNativeAgent(ctx context.Context, opts models.SetupOptions, exited chan<- error) error {

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

	// Build args (binary is passed separately to utils)
	args := []string{
		"agent",
		"--port", strconv.Itoa(int(opts.AgentPort)),
		"--proxy-port", strconv.Itoa(int(opts.ProxyPort)),
		"--dns-port", strconv.Itoa(int(opts.DnsPort)),
		"--client-pid", strconv.Itoa(int(os.Getpid())),
		"--mode", string(opts.Mode),
	}

	extraArgs := opts.ExtraArgs
	if len(extraArgs) > 0 {
		args = append(args, extraArgs...)
	}
	if a.conf.Debug {
		args = append(args, "--debug")
	}
	if a.conf.Record.Synchronous {
		args = append(args, "--sync")
	}
	// Forward the operator's --disable-mapping (root-level config) so
	// the native agent process — even though it inherits the same
	// keploy.yml via --config-path — sees an explicit override on
	// invocations where the host CLI mutated the in-memory config
	// after viper load (e.g. --disable-mapping=false on the test
	// subcommand path that drove this agent start).
	args = append(args, fmt.Sprintf("--disable-mapping=%t", a.conf.DisableMapping))
	if a.conf.Record.EnableSampling > 0 {
		args = append(args, fmt.Sprintf("--enable-sampling=%d", a.conf.Record.EnableSampling))
	}
	if opts.EnableTesting {
		args = append(args, "--enable-testing")
	}
	if opts.GlobalPassthrough {
		args = append(args, "--global-passthrough")
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
	// Upstream TLS verification. Forwarded UNCONDITIONALLY as =%t, the same
	// pattern (and for the same reason) as --disable-mapping above: the
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
	if models.IsAnsiDisabled == true {
		args = append(args, "--disable-ansi")
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

	args = appendTokenFileArg(a.logger, args)

	// The PTY and the cached-credentials probe both exist for one reason: sudo
	// may prompt for a password. On a platform where the agent is never
	// elevated (darwin) there is no prompt, and taking the PTY path anyway is
	// pure cost — startNativeAgentWithPTY puts the user's real terminal into
	// raw mode for the whole run, swallows their keystrokes into a pty nothing
	// reads, and closes the master on stop, which delivers SIGHUP to an agent
	// that would otherwise have shut down gracefully.
	elevates := agentUtils.AgentNeedsElevation(runtime.GOOS)

	sudoCached := false
	if elevates {
		// Cached credentials mean we can use sudo -n and skip the prompt.
		sudoCached = utils.AreSudoCredentialsCached()
	}

	if elevates && agentUtils.NeedsPTY() && !sudoCached {
		return a.startNativeAgentWithPTY(ctx, keployBin, args, grp, exited)
	}

	// Create OS-appropriate command (handles sudo/process-group on Unix; plain on Windows)
	// If credentials are cached, this will use sudo -n (non-interactive)
	cmd := agentUtils.NewAgentCommand(keployBin, args, sudoCached)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	a.mu.Lock()
	a.agentCmd = cmd // this has been set for proper stopping of the native agent
	a.mu.Unlock()
	// Start (OS-specific tweaks happen inside utils.StartCommand)
	if err := agentUtils.StartCommand(cmd); err != nil {
		utils.LogError(a.logger, err, "failed to start keploy agent")
		return err
	}

	pid := cmd.Process.Pid
	a.logger.Debug("keploy agent started", zap.Int("pid", pid))

	grp.Go(func() error {
		defer utils.Recover(a.logger)

		err := cmd.Wait()
		// Reported BEFORE the error is returned below: returning it cancels
		// the group, and the readiness wait must find the exit already here
		// when it sees that cancellation.
		exited <- err
		// If ctx wasn't cancelled, bubble up unexpected exits
		if err != nil && ctx.Err() == nil {
			a.logger.Error("agent process exited with error", zap.Error(err))
			return err
		}
		a.mu.Lock()
		a.agentCmd = nil
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
		if err := a.requestAgentStop(); err != nil {
			a.logger.Debug("failed to request keploy agent shutdown, sending stop signal", zap.Error(err))
			// Fallback: forcefully stop the agent process
			a.mu.Lock()
			cmd := a.agentCmd
			a.mu.Unlock()
			if cmd != nil {
				if stopErr := agentUtils.StopCommand(cmd, a.logger); stopErr != nil {
					a.logger.Error("failed to forcefully stop agent", zap.Error(stopErr))
				}
			}
			return nil
		}
		a.logger.Info("Keploy agent stopped.")
		return nil
	})

	return nil
}

// startNativeAgentWithPTY starts the agent with PTY support for interactive input (e.g., sudo password)
func (a *AgentClient) startNativeAgentWithPTY(ctx context.Context, keployBin string, args []string, grp *errgroup.Group, exited chan<- error) error {
	// Create command configured for PTY
	cmd := agentUtils.NewAgentCommandForPTY(keployBin, args)

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
	a.logger.Debug("keploy agent started with PTY", zap.Int("pid", pid))

	grp.Go(func() error {
		defer utils.Recover(a.logger)

		err := ptyHandle.Wait()
		// Before the return below, for the reason given in startNativeAgent.
		exited <- err
		// If ctx wasn't cancelled, bubble up unexpected exits
		if err != nil && ctx.Err() == nil {
			a.logger.Error("agent process exited with error", zap.Error(err))
			return err
		}
		a.mu.Lock()
		a.agentCmd = nil
		a.agentPTY = nil
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
		if err := a.requestAgentStop(); err != nil {
			a.logger.Debug("failed to request keploy agent shutdown, sending stop signal", zap.Error(err))
			// Fallback: forcefully stop the agent process
			a.mu.Lock()
			ptyHandle := a.agentPTY
			a.mu.Unlock()
			if ptyHandle != nil {
				if stopErr := agentUtils.StopPTYCommand(ptyHandle, a.logger); stopErr != nil {
					a.logger.Error("failed to forcefully stop agent", zap.Error(stopErr))
				}
			}
			return nil
		}
		a.logger.Info("Keploy agent stopped.")
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

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(stopCtx, http.MethodPost, fmt.Sprintf("%s/stop", a.conf.Agent.AgentURI), nil)
	if err != nil {
		return err
	}

	res, err := a.client.Do(req)
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

	// If PTY is active, close it to unblock stdin/stdout copies
	if a.agentPTY != nil {
		// Use the utils function to gracefully stop PTY
		if err := agentUtils.StopPTYCommand(a.agentPTY, a.logger); err != nil {
			a.logger.Debug("failed to stop PTY command", zap.Error(err))
		}
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

	logs, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "200", container).CombinedOutput()
	state, _ := exec.CommandContext(ctx, "docker", "inspect", "-f",
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
		switch ee.ExitCode() {
		case utils.ExitPrivilegeRequired:
			cause = utils.ErrPrivilegeRequired
		case utils.ExitEnvironmentUnsupported:
			cause = utils.ErrEnvironmentUnsupported
		}
	}
	if cause == nil {
		if exitErr == nil {
			return errors.New("the keploy agent exited before it became ready")
		}
		return fmt.Errorf("the keploy agent exited before it became ready: %w", exitErr)
	}
	utils.SetExitCodeOnce(utils.ExitCodeFor(cause))
	a.logger.Error("the keploy agent could not start", zap.Error(cause), zap.String("next_step", agentStartRemedy(cause, isDockerCmd, runtime.GOOS)))
	return fmt.Errorf("%w: the keploy agent could not start (%v)", cause, exitErr)
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
// check (nil), its process ends (exited), its ready budget runs out, or ctx is
// cancelled.
func (a *AgentClient) waitForAgent(ctx context.Context, exited <-chan error, isDockerCmd bool, opts models.SetupOptions) error {
	// Wait up to the agent's own healthcheck budget for it to become ready.
	// Under heavy CI docker-daemon contention the agent container can take
	// well over a minute just to start (observed: a local-image `docker run`
	// taking 126s), so a 60s wait gave up prematurely and tore down a
	// bring-up that would have succeeded. Overridable via KEPLOY_AGENT_READY_TIMEOUT.
	readyTimeout := pkg.AgentReadyTimeout()
	if opts.AgentReadyTimeout > 0 {
		readyTimeout = opts.AgentReadyTimeout
	}
	agentCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	agentReadyCh := make(chan bool, 1)
	go pkg.AgentHealthTicker(agentCtx, a.logger, a.conf.Agent.AgentURI, agentReadyCh, 1*time.Second)

	// agentCtx is ctx's child, so its Done covers the caller's cancellation
	// as well as the budget running out, and the ticker closes agentReadyCh
	// (ready == false) on either. Which of those it was is decided below, in
	// order, rather than by whichever case the select happens to pick when
	// several are ready at once.
	var ready bool
	select {
	case exitErr := <-exited:
		return a.agentStoppedBeforeReady(exitErr, isDockerCmd)
	case <-agentCtx.Done():
	case ready = <-agentReadyCh:
	}
	if ready {
		return nil
	}
	if ctx.Err() != nil {
		// The caller's Ctrl+C -- or a goroutine in the caller's errgroup
		// failing, which cancels ctx too. The one waiting on the agent
		// process is such a goroutine, and it reports the exit before it
		// fails the group, so when that is what happened the exit is already
		// here. Otherwise the cause says which it was: ctx.Err() is "context
		// canceled" for both, and a caller could not tell keploy failing
		// from the user stopping it.
		select {
		case exitErr := <-exited:
			return a.agentStoppedBeforeReady(exitErr, isDockerCmd)
		default:
		}
		return context.Cause(ctx)
	}
	// The agent never reported healthy. Setup's cleanup defer is not in
	// scope yet, so without this the wedged agent container and its
	// goroutine leak — and any retry inherits the mess. Dump the container's
	// own logs first: the CLI otherwise shows the full readiness window of
	// silence followed by a bare error, with nothing to root-cause from.
	if isDockerCmd {
		a.logAgentContainerDiagnostics(opts.KeployContainer)
	}
	a.stopAgent()
	// Sentinel so the caller can retry a fresh bring-up on this specific,
	// nondeterministic stall without retrying deterministic failures.
	return fmt.Errorf("%w", pkg.ErrAgentNotReady)
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

func (a *AgentClient) Setup(ctx context.Context, cmd string, opts models.SetupOptions) error {
	isDockerCmd := utils.IsDockerCmd(utils.CmdType(opts.CommandType))
	opts.IsDocker = isDockerCmd

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

		var appPorts, appNetworks []string
		if utils.CmdType(opts.CommandType) == utils.FromContainer {
			// No command to scrape. The container itself is the source of
			// truth, and it answers with published ports and networks the
			// regex below cannot see: ExtractDockerFlags matches only the
			// space-separated spelling, so `--network=foo` and `-p=8080:80`
			// slip past it today and then collide with the `--network=container:`
			// keploy splices in.
			appPorts, appNetworks, err = a.appNetworkingFromContainer(ctx, opts.FromContainer)
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
			cmd, appPorts, appNetworks = agentUtils.ExtractDockerFlags(cmd)
		}

		opts.AppPorts = appPorts
		if len(appNetworks) > 0 {
			opts.AppNetworks = appNetworks
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

	// Set up cleanup on failure
	defer func() {
		if err != nil {
			a.logger.Info("Setup failed, cleaning up agent")
			a.stopAgent()
		}
	}()

	// TODO : Proxy or TLS should not be importes in the agent
	// This is done because to set env variable for TLS
	err = ptls.SetupCaCertEnv(a.logger)
	if err != nil {
		utils.LogError(a.logger, err, "failed to set TLS environment")
		return err
	}

	// Mock mode: export the agent's scope-API address into the wrapped
	// command's environment so a test-runner plugin / glue-code can mark
	// per-test boundaries (POST {KEPLOY_MOCK_AGENT}/agent/scope/begin|end).
	// The child inherits this process's environment. Native + docker-run put
	// the child in the agent's netns, so localhost:<agentPort> reaches it;
	// docker-compose shares the host loopback. Harmless when unused.
	if opts.MockMode {
		if err := os.Setenv("KEPLOY_MOCK_AGENT", fmt.Sprintf("http://localhost:%d", agentPort)); err != nil {
			a.logger.Debug("failed to export KEPLOY_MOCK_AGENT", zap.Error(err))
		}
		// The scope API is guarded like every other control-plane route, and
		// this caller is not keploy — it is the user's test runner. Without a
		// credential the advertised integration would simply 401.
		//
		// os.Setenv is process-wide, so strictly every child started after
		// this inherits it, not only the wrapped runner. That is the same
		// trust domain: in mock mode the wrapped process is the intended API
		// client, and the incidental children are keploy's own docker and
		// shell teardown commands. What matters is the guard: only under
		// MockMode, so the application under test in a record or test run is
		// still handed nothing.
		//
		// A separate variable from token.Env, so a process that inherits it
		// presents the token rather than adopting it as one to enforce.
		if tok := token.Session(); tok != "" {
			if err := os.Setenv(token.MockAgentTokenEnv, tok); err != nil {
				a.logger.Debug("failed to export "+token.MockAgentTokenEnv, zap.Error(err))
			}
		}
		session := "record"
		if opts.Mode == models.MODE_TEST {
			session = "replay"
		}
		if err := os.Setenv("KEPLOY_MOCK_SESSION", session); err != nil {
			a.logger.Debug("failed to export KEPLOY_MOCK_SESSION", zap.Error(err))
		}
	}

	err = usrApp.Setup(ctx)
	if err != nil {
		utils.LogError(a.logger, err, "failed to setup app")
		return err
	}

	a.logger.Debug("Keploy client setup completed successfully")
	return nil
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
// compose reporting the dependency it lost, or the test command's code after
// compose stopped it, and neither is the test command's verdict. The agent's
// container, read before keploy's teardown removes it, says which it was.
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
	return fmt.Errorf("the keploy agent stopped while the test command was running: %w", exit)
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

func (a *AgentClient) startInDocker(ctx context.Context, logger *zap.Logger, opts models.SetupOptions) error {
	keployAlias, err := kdocker.GetKeployDockerAlias(ctx, logger, &config.Config{
		InstallationID: a.conf.InstallationID,
	}, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to prepare docker command and environment")
		return err
	}

	cmd, err := kdocker.PrepareDockerCommand(ctx, keployAlias)
	if err != nil {
		utils.LogError(logger, err, "failed to prepare docker command")
		return err
	}

	cmd.Cancel = func() error {
		logger.Debug("Context cancelled. Explicitly stopping the 'keploy-v3' Docker container.")

		containerName := opts.KeployContainer

		// Try stopping the container without sudo first (works if user is in docker group)
		stopCmd := exec.Command("docker", "stop", containerName)
		if output, err := stopCmd.CombinedOutput(); err != nil {
			// If that fails on Linux, try with sudo -n (non-interactive, won't prompt for password)
			if runtime.GOOS == "linux" {
				logger.Debug("docker stop without sudo failed, trying with sudo -n", zap.Error(err))
				stopCmd = exec.Command("sudo", "-n", "docker", "stop", containerName)
				if output, err := stopCmd.CombinedOutput(); err != nil {
					logger.Debug("Could not stop the docker container. It may have already stopped.",
						zap.String("container", containerName),
						zap.Error(err),
						zap.String("output", string(output)))
				} else {
					logger.Debug("Successfully sent stop command to the container.", zap.String("container", containerName))
				}
			} else {
				logger.Debug("Could not stop the docker container. It may have already stopped.",
					zap.String("container", containerName),
					zap.Error(err),
					zap.String("output", string(output)))
			}
		} else {
			logger.Debug("Successfully sent stop command to the container.", zap.String("container", containerName))
		}

		if cmd.Process != nil {
			return utils.SendSignal(logger, cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	logger.Debug("running the following command to start agent in docker", zap.String("command", redactToken(cmd.String())))

	// Check if we need PTY for interactive input (e.g., sudo password on Linux)
	if agentUtils.NeedsPTY() {
		return a.startInDockerWithPTY(ctx, logger, cmd)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.Canceled {
			cmd.Process.Kill()
			logger.Info("Keploy agent in docker stopped gracefully.")
			return nil
		}
		utils.LogError(logger, err, "failed to run keploy agent in docker")
		return err
	}

	return nil
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
	a.agentCmd = nil
	a.agentPTY = nil
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
	return mockErrors, nil
}

// BeginTestErrorCapture asks the agent to open a per-test mock-error capture
// window so the next GetMockErrors returns only this test's misses. A missing
// endpoint (older agent) returns 404 and is treated as a no-op, preserving the
// legacy global-queue behaviour.
func (a *AgentClient) BeginTestErrorCapture(ctx context.Context) error {
	url := fmt.Sprintf("%s/test-capture/begin", a.conf.Agent.AgentURI)
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

func (a *AgentClient) MakeAgentReadyForDockerCompose(ctx context.Context) error {
	// Aligned with the agent's own healthcheck budget; see pkg.AgentReadyTimeout.
	ctx, cancel := context.WithTimeout(ctx, pkg.AgentReadyTimeout())
	defer cancel()

	ticker := time.NewTicker(agentReadyRetryInterval)
	defer ticker.Stop()

	url := fmt.Sprintf("%s/agent/ready", a.conf.Agent.AgentURI)

	for {
		req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
		if err != nil {
			return err
		}

		resp, err := a.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				a.logger.Debug("Successfully marked agent as ready")
				return nil
			}
			a.logger.Debug("Agent returned non-200 status for ready check", zap.Int("status", resp.StatusCode))
		} else {
			a.logger.Debug("Failed to call agent ready endpoint, retrying...", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timeout waiting for agent to become ready")
			}
			return ctx.Err()
		case <-ticker.C:
			// retry
		}
	}
}

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
		return a.streamPcapArtifact(gctx, "/pcap/traffic", filepath.Join(destDir, "traffic.pcap"), 0o644)
	})
	g.Go(func() error {
		return a.streamPcapArtifact(gctx, "/pcap/keylog", filepath.Join(destDir, "sslkeys.log"), 0o644)
	})
	return g.Wait()
}

// streamPcapArtifact holds a single GET open and copies its body
// into dstFile until the server closes the stream or ctx is done.
// 503 means "capture not active" — silently skipped so the recorder
// stays a no-op for non-capture sessions. Other non-200s surface as
// errors. Context cancellation returns nil so callers don't see
// spurious errors at recording stop.
func (a *AgentClient) streamPcapArtifact(ctx context.Context, urlPath, dstFile string, mode os.FileMode) error {
	url := fmt.Sprintf("%s%s", a.conf.Agent.AgentURI, urlPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build pcap stream request: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return fmt.Errorf("dial %s: %w", url, err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && !errors.Is(cerr, context.Canceled) {
			a.logger.Debug("failed to close pcap stream body", zap.String("url", url), zap.Error(cerr))
		}
	}()

	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusServiceUnavailable:
		a.logger.Debug("agent reports no pcap stream available; capture likely off",
			zap.String("url", url))
		return nil
	case http.StatusOK:
		// fall through to the long copy below
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("stream %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	out, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dstFile, err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil {
			a.logger.Warn("failed to close pcap dst file",
				zap.String("path", dstFile), zap.Error(cerr))
		}
	}()

	a.logger.Debug("pcap stream connected",
		zap.String("url", url), zap.String("path", dstFile))
	written, copyErr := io.Copy(out, resp.Body)
	a.logger.Debug("pcap stream ended",
		zap.String("url", url),
		zap.String("path", dstFile),
		zap.Int64("bytes", written),
		zap.Error(copyErr))
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		return copyErr
	}
	return nil
}

// GetScopeWindows fetches the per-test scope windows the wrapped runner reported
// during a `keploy mock record` session (via /agent/scope/*), so the CLI can
// correlate captured mocks into mappings.yaml. A missing endpoint (older agent)
// returns an empty slice, degrading to suite-level recording.
func (a *AgentClient) GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	url := fmt.Sprintf("%s/scope/windows", a.conf.Agent.AgentURI)
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

// PushScopeTable hands the agent the replay-time per-test name→mock-names table
// (from mappings.yaml) so the runner's /agent/scope/begin calls can restrict the
// served pool per test. A missing endpoint (older agent) is a no-op.
func (a *AgentClient) PushScopeTable(ctx context.Context, table map[string][]string) error {
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

// appNetworkingFromContainer reads the published ports and user networks off an
// already-running container, in the shapes GenerateDockerCommand expects:
// AppPorts as whole `-p …` flags, AppNetworks as bare names.
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
func (a *AgentClient) appNetworkingFromContainer(ctx context.Context, name string) (ports []string, networks []string, err error) {
	inspect, err := a.dockerClient.ContainerInspect(ctx, name)
	if err != nil {
		return nil, nil, err
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
	return ports, networks, nil
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
		a.stopAgent()
		if !a.fromContainerWasRunning {
			return
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
