package replay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"go.uber.org/zap"

	// "encoding/json"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// healthPollInterval is how often the --health-url probe is retried while waiting for 2xx.
const healthPollInterval = 500 * time.Millisecond

// healthProbeTimeout bounds a single GET attempt to --health-url.
const healthProbeTimeout = 1 * time.Second

// healthPoller is swapped out in tests; production wires to net/http via httpHealthPoll.
var healthPoller = httpHealthPoll

// httpHealthPoll issues a single GET to url with a short per-request timeout and reports
// whether it returned a 2xx status. Non-2xx, transport errors, and timeouts all report false;
// the caller decides whether to retry.
//
// The response body is fully drained (io.Copy to io.Discard) before Close so the underlying
// TCP connection can be returned to the DefaultClient's keep-alive pool and reused by the
// next poll iteration. Without the drain, net/http treats the connection as dirty and opens
// a new socket every 500ms, which is pure overhead against an endpoint we are going to hit
// many times in a row.
func httpHealthPoll(ctx context.Context, url string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		// Drain first, then close — enables HTTP keep-alive reuse across poll iterations.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// sanitizeHealthURL returns a log-safe rendering of a --health-url value.
// The raw string is operator-supplied and can legitimately carry secrets:
// basic-auth userinfo (e.g. https://user:pw@host/healthz), API tokens in
// the query string (...?token=abc), or session fragments. Emitting any of
// those to zap fields would leak them into structured log sinks, tailing
// aggregators, and error-reporting backends. We strip userinfo, query,
// and fragment; scheme + host + path is enough signal to diagnose a
// misconfigured poll target.
//
// On unparseable input we return a fixed placeholder — the operator
// already gets the raw reason via the separate "reason" field in the
// invalid-URL Error log, so we don't need to echo the raw string back.
func sanitizeHealthURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "<unparseable-health-url-redacted>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// validateHealthURL checks that s is a syntactically usable HTTP(S) URL for
// http.NewRequestWithContext — i.e. it has a scheme of http or https and a
// non-empty host. We intentionally keep validation purely syntactic: no DNS
// resolution, no TCP dial, no HEAD probe. The poll loop is what actually
// confirms reachability; this function only exists to fail fast on operator
// typos (missing scheme, "not-a-url", stray whitespace) so we don't burn the
// whole HealthPollTimeout window on errors that will never succeed.
//
// Returns ("", true) when the URL is usable, and (reason, false) otherwise
// with a short human-readable reason the caller can surface to the operator.
func validateHealthURL(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil {
		return err.Error(), false
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return "missing scheme (expected http:// or https://)", false
	default:
		return "unsupported scheme " + fmt.Sprintf("%q", u.Scheme) + " (expected http or https)", false
	}
	if u.Host == "" {
		return "missing host", false
	}
	return "", true
}

// waitForAppReady gates the first test on the user-application being up.
//
// If Test.HealthURL is empty (or syntactically invalid) we keep the historical
// behavior exactly: sleep for Test.Delay seconds or until ctx is canceled. An
// invalid HealthURL also logs at ERROR so operators see the misconfig, but it
// is NOT a fatal failure — the fixed-delay path runs so replay never regresses
// vs the pre-health-url behavior.
//
// Otherwise we poll HealthURL every healthPollInterval (with a per-request cap
// of healthProbeTimeout). The first 2xx unblocks immediately. If HealthPollTimeout
// elapses with no 2xx, we log an INFO telling the operator what to tune and fall
// back to the fixed Delay sleep so operators never get stuck worse than today.
//
// The poll cadence uses a single time.Ticker, and the overall poll ceiling is
// enforced by a derived context with timeout observed through Done(). This
// avoids per-iteration timer allocation from time.After and gives deterministic
// teardown via defer Stop / defer cancel.
//
// Returns true when the caller should proceed to run tests, false only when ctx
// was canceled (caller should treat as user abort). Specifically, an invalid
// HealthURL does NOT cause a false return — callers rely on this contract to
// disambiguate user abort from misconfiguration (see replay.go classification).
func waitForAppReady(ctx context.Context, logger *zap.Logger, cfg *config.Config) bool {
	delay := time.Duration(cfg.Test.Delay) * time.Second

	healthURL := cfg.Test.HealthURL

	// Fail gracefully on a malformed --health-url instead of burning the entire
	// HealthPollTimeout window on http.NewRequestWithContext errors that
	// will never succeed. Common mistakes: missing scheme ("localhost:8080"),
	// stray whitespace, "not-a-url". net/url.Parse is deliberately lenient,
	// so we also require a non-empty scheme (http/https) and host — matching
	// what net/http's transport actually needs to dial. No DNS, no dial here:
	// just syntactic validation so the operator gets actionable feedback now
	// rather than after 60s of silent retries.
	//
	// On invalid URL we fall back to the fixed-delay path (same as the
	// empty-URL branch) rather than returning false. Returning false here
	// would be classified by callers as TestSetStatusUserAbort + context
	// cancellation, which is a behavior regression vs the pre-health-url
	// fixed-delay sleep. The ERROR log still fires so operators see the
	// misconfig, and the contract of "false only on ctx cancel" stays
	// truthful for callers (see replay.go classification of the return).
	if healthURL != "" {
		if reason, ok := validateHealthURL(healthURL); !ok {
			logger.Error("invalid --health-url; falling back to fixed delay",
				zap.String("healthUrl", sanitizeHealthURL(healthURL)),
				zap.String("reason", reason),
				zap.String("next_step", "--health-url must be a full URL with scheme (http:// or https://) and host — fix it or omit to use --delay only"),
			)
			healthURL = "" // fall through to the empty-URL / fixed-delay branch below
		}
	}

	if healthURL == "" {
		// NewTimer + defer Stop so a ctx cancel releases the timer
		// immediately instead of leaving it scheduled until `delay`
		// fires — matters under large --delay and repeated aborts.
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		}
	}

	pollCeiling := cfg.Test.HealthPollTimeout
	if pollCeiling <= 0 {
		pollCeiling = 60 * time.Second
	}

	logger.Info("polling application health endpoint before firing tests",
		zap.String("healthUrl", sanitizeHealthURL(cfg.Test.HealthURL)),
		zap.Duration("pollTimeout", pollCeiling),
	)

	// Derive a single deadline ctx for the ENTIRE poll window. This is what
	// makes pollCeiling a true upper bound: every probe — including the
	// immediate one below and each ticker-driven retry — inherits the
	// remaining ceiling via context. httpHealthPoll still applies its own
	// healthProbeTimeout per request, but context.WithTimeout takes whichever
	// expires first, so a small pollCeiling (e.g. 100ms) cannot be exceeded
	// by a probe's 1s per-request cap. We keep the caller ctx as the parent
	// so user-initiated cancel still unblocks instantly.
	pollCtx, pollCancel := context.WithTimeout(ctx, pollCeiling)
	defer pollCancel()

	// Probe once immediately so a fast-ready app doesn't pay the first tick's
	// healthPollInterval of latency. The probe inherits pollCtx, so even this
	// first attempt is bounded by the remaining poll ceiling.
	if healthPoller(pollCtx, cfg.Test.HealthURL) {
		logger.Debug("health endpoint reported 2xx; proceeding",
			zap.String("healthUrl", sanitizeHealthURL(cfg.Test.HealthURL)),
		)
		return true
	}

	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Parent ctx canceled by the user — treat as abort, distinct
			// from the pollCtx deadline branch below which is a normal
			// fallback-to-fixed-delay path.
			return false
		case <-pollCtx.Done():
			// pollCtx is derived from ctx, so a user cancel also fires
			// this branch. Since select picks a ready case
			// non-deterministically, we cannot rely on the ctx.Done()
			// branch above to win the race — explicitly disambiguate
			// here. If the parent ctx is canceled we treat the wakeup
			// as a user abort and return false instead of falling
			// through to the fixed-delay fallback (which would return
			// true and incorrectly proceed to run tests).
			if ctx.Err() != nil {
				return false
			}
			// pollCeiling elapsed with no 2xx. Downgraded from Warn per
			// repo logging guidelines; the message still tells operators
			// exactly which knobs to turn.
			logger.Info("health probe timed out, falling back to fixed delay — raise --health-poll-timeout (or test.healthPollTimeout in keploy.yml) or point --health-url at an endpoint that returns 2xx sooner",
				zap.String("healthUrl", sanitizeHealthURL(cfg.Test.HealthURL)),
				zap.Duration("pollTimeout", pollCeiling),
				zap.Duration("fallbackDelay", delay),
			)
			fallbackTimer := time.NewTimer(delay)
			defer fallbackTimer.Stop()
			select {
			case <-fallbackTimer.C:
				return true
			case <-ctx.Done():
				return false
			}
		case <-ticker.C:
			if healthPoller(pollCtx, cfg.Test.HealthURL) {
				logger.Debug("health endpoint reported 2xx; proceeding",
					zap.String("healthUrl", sanitizeHealthURL(cfg.Test.HealthURL)),
				)
				return true
			}
		}
	}
}

type TestReportVerdict struct {
	total     int
	passed    int
	failed    int
	obsolete  int
	ignored   int
	status    bool
	duration  time.Duration
	timeTaken string
}

func LeftJoinNoise(globalNoise config.GlobalNoise, tsNoise config.GlobalNoise) config.GlobalNoise {
	noise := CloneGlobalNoise(globalNoise)

	// Preserve the historical guarantee that the body and header buckets always
	// exist, since some callers index them directly.
	if _, ok := noise["body"]; !ok {
		noise["body"] = make(map[string][]string)
	}
	if _, ok := noise["header"]; !ok {
		noise["header"] = make(map[string][]string)
	}

	// Merge EVERY section present in the test-set noise — not just body/header.
	// The previous code hard-coded those two buckets, so test-set-scoped
	// `requestbody` noise (and any future bucket) was silently dropped while the
	// global bucket worked, which under --schema-noise-strict could turn a
	// noised request-body field back into a match-affecting one and falsely
	// reject the mock.
	for section, fields := range tsNoise {
		if _, ok := noise[section]; !ok {
			noise[section] = make(map[string][]string)
		}
		for field, regexArr := range fields {
			noise[section][field] = regexArr
		}
	}

	return noise
}

func CloneGlobalNoise(src config.GlobalNoise) config.GlobalNoise {
	cloned := make(config.GlobalNoise, len(src))
	for section, fields := range src {
		fieldCopy := make(map[string][]string, len(fields))
		for field, patterns := range fields {
			patternCopy := make([]string, len(patterns))
			copy(patternCopy, patterns)
			fieldCopy[field] = patternCopy
		}
		cloned[section] = fieldCopy
	}
	return cloned
}

// PrepareMockNoiseConfig builds the noise configuration handed to the proxy
// integrations (OutgoingOptions.NoiseConfig) for mock matching. It carries
// the "header" noise (HTTP outgoing header matching) AND the "body" noise:
// parsers that compare outgoing payloads — e.g. the Pulsar SEND payload
// matcher — strip these body-noise field names so non-deterministic fields
// (emit timestamps, generated event ids) don't false-reject a replay.
func PrepareMockNoiseConfig(globalNoise config.GlobalNoise, testSetNoise config.TestsetNoise, testSetID string) map[string]map[string][]string {
	noiseConfig := CloneGlobalNoise(globalNoise)
	if tsNoise, ok := testSetNoise[testSetID]; ok {
		noiseConfig = LeftJoinNoise(noiseConfig, tsNoise)
	}

	// Extract the noise buckets the proxy consumes for mock matching:
	//   - "header": HTTP outgoing header-key matching.
	//   - "body":   outgoing-payload matchers (e.g. the Pulsar SEND matcher in
	//               the integrations repo) strip these field names.
	//   - "requestbody": DEDICATED HTTP request-body matching noise — kept
	//               separate from "body" so response-assertion noise can't
	//               silently soften request matching (see http/decode.go).
	mockNoiseConfig := map[string]map[string][]string{}
	if headerNoise, ok := noiseConfig["header"]; ok {
		mockNoiseConfig["header"] = headerNoise
	}
	if bodyNoise, ok := noiseConfig["body"]; ok {
		mockNoiseConfig["body"] = bodyNoise
	}
	if reqBodyNoise, ok := noiseConfig["requestbody"]; ok {
		mockNoiseConfig["requestbody"] = reqBodyNoise
	}

	return mockNoiseConfig
}

// PrepareHeaderNoiseConfig is retained for backward compatibility with
// downstream importers of pkg/service/replay. It preserves the original
// header-only behavior.
//
// Deprecated: use PrepareMockNoiseConfig, which also carries body noise for
// outgoing-payload matchers (e.g. the Pulsar SEND matcher).
func PrepareHeaderNoiseConfig(globalNoise config.GlobalNoise, testSetNoise config.TestsetNoise, testSetID string) map[string]map[string][]string {
	mockNoiseConfig := PrepareMockNoiseConfig(globalNoise, testSetNoise, testSetID)
	headerOnly := map[string]map[string][]string{}
	if headerNoise, ok := mockNoiseConfig["header"]; ok {
		headerOnly["header"] = headerNoise
	}
	return headerOnly
}

// ReplaceBaseURL replaces the baseUrl of the old URL with the new URL's.
func ReplaceBaseURL(newURL, oldURL string) (string, error) {
	parsedOldURL, err := url.Parse(oldURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse the old URL: %v", err)
	}

	parsedNewURL, err := url.Parse(newURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse the new URL: %v", err)
	}
	// if scheme is empty, then add the scheme from the old URL in order to parse it correctly
	if parsedNewURL.Scheme == "" {
		parsedNewURL.Scheme = parsedOldURL.Scheme
		parsedNewURL, err = url.Parse(parsedNewURL.String())
		if err != nil {
			return "", fmt.Errorf("failed to parse the scheme added new URL: %v", err)
		}
	}

	parsedOldURL.Scheme = parsedNewURL.Scheme
	parsedOldURL.Host = parsedNewURL.Host
	apiPath := path.Join(parsedNewURL.Path, parsedOldURL.Path)

	parsedOldURL.Path = apiPath
	parsedOldURL.RawPath = apiPath
	replacedURL := parsedOldURL.String()
	decodedURL, err := url.PathUnescape(replacedURL)
	if err != nil {
		return "", fmt.Errorf("failed to decode the URL: %v", err)
	}
	return decodedURL, nil
}

func mergeMaps(map1, map2 map[string][]string) map[string][]string {
	for key, values := range map2 {
		if _, exists := map1[key]; exists {
			map1[key] = append(map1[key], values...)
		} else {
			map1[key] = values
		}
	}
	return map1
}

func removeFromMap(map1, map2 map[string][]string) map[string][]string {
	for key := range map2 {
		delete(map1, key)
	}
	return map1
}

// effectiveStreamMockWindow calculates the effective time window for streaming mocks.
// It returns the start time (request timestamp) and end time (anchor + timeout),
// where anchor is the later of request/response timestamps (falling back to time.Now).
// The timeout is calculated using pkg.ComputeStreamingTimeoutSeconds which considers the test case's timeout configuration.
func effectiveStreamMockWindow(tc *models.TestCase, defaultAPITimeout uint64) (time.Time, time.Time) {
	if tc == nil {
		return time.Time{}, time.Time{}
	}

	reqTs := tc.HTTPReq.Timestamp
	respTs := tc.HTTPResp.Timestamp
	timeoutSeconds := pkg.ComputeStreamingTimeoutSeconds(tc, defaultAPITimeout)

	anchor := reqTs
	if anchor.IsZero() || (!respTs.IsZero() && respTs.After(anchor)) {
		anchor = respTs
	}
	if anchor.IsZero() {
		anchor = time.Now().UTC()
	}

	return reqTs, anchor.Add(time.Duration(timeoutSeconds) * time.Second)
}

func timeWithUnits(duration time.Duration) string {
	if duration.Seconds() < 1 {
		return fmt.Sprintf("%v ms", duration.Milliseconds())
	} else if duration.Minutes() < 1 {
		return fmt.Sprintf("%.2f s", duration.Seconds())
	} else if duration.Hours() < 1 {
		return fmt.Sprintf("%.2f min", duration.Minutes())
	}
	return fmt.Sprintf("%.2f hr", duration.Hours())
}

func getFailedTCs(results []models.TestResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if r.Status == models.TestStatusFailed {
			ids = append(ids, r.TestCaseID)
		}
	}
	return ids
}

// retainNoisyTestCaseMocks injects mocks used by noisy test cases into the
// consumed-mocks set used for pruning so those mocks are not deleted.
func retainNoisyTestCaseMocks(noisyTestCaseNames []string, mapping *models.Mapping, consumed map[string]models.MockState) int {
	if len(noisyTestCaseNames) == 0 || mapping == nil || len(mapping.TestCases) == 0 || consumed == nil {
		return 0
	}

	noisySet := make(map[string]struct{}, len(noisyTestCaseNames))
	for _, testCaseName := range noisyTestCaseNames {
		if testCaseName == "" {
			continue
		}
		noisySet[testCaseName] = struct{}{}
	}
	if len(noisySet) == 0 {
		return 0
	}

	added := 0
	for _, mappedTestCase := range mapping.TestCases {
		if _, ok := noisySet[mappedTestCase.ID]; !ok {
			continue
		}

		for _, mock := range mappedTestCase.Mocks {
			if mock.Name == "" {
				continue
			}
			if _, exists := consumed[mock.Name]; exists {
				continue
			}

			consumed[mock.Name] = models.MockState{
				Name:      mock.Name,
				Kind:      models.Kind(mock.Kind),
				Timestamp: mock.Timestamp,
			}
			added++
		}
	}

	return added
}

// isMockSubsetWithConfig reports whether the streaming-path replay consumed a
// mock set consistent with the test's mapping: a consumed mock that is NOT in
// the expected set flags a mismatch — UNLESS it is a mock that doesn't
// participate in the per-test comparison. Only PER-TEST mocks are compared;
// reusable/startup-tier mocks (session / connection / config, recorded once at
// app boot and reused) and DNS (non-deterministic resolution order) are
// ignored, mirroring the non-streaming path's filtering. They belong in the
// mapping but must not, by their reuse, falsely demote a test to OBSOLETE.
func isMockSubsetWithConfig(consumedMocks []models.MockState, expectedMocks []string) bool {
	expectedMap := make(map[string]bool)
	for _, name := range expectedMocks {
		expectedMap[name] = true
	}

	for _, m := range consumedMocks {
		if !expectedMap[m.Name] {
			// An extra consumed mock that wasn't in the mapping is a mismatch
			// ONLY if it's a per-test mock. Reusable/startup-tier mocks
			// (session/connection/config) and DNS are excluded from the
			// comparison — they are reused / non-deterministically attributed.
			if m.Kind != models.DNS && !isReusableTierState(m) {
				return false
			}
		}
	}
	return true
}

// recordReqResTimestamps returns the RECORD-TIME request and response
// timestamps for a test case, regardless of kind (HTTP vs gRPC). The fallback
// to tc.Created covers very old fixtures that only carry the coarse creation
// timestamp. Either value may be zero when the recording did not populate it.
func recordReqResTimestamps(tc *models.TestCase) (time.Time, time.Time) {
	if tc == nil {
		return time.Time{}, time.Time{}
	}
	var req, resp time.Time
	if !tc.HTTPReq.Timestamp.IsZero() {
		req = tc.HTTPReq.Timestamp
	} else if !tc.GrpcReq.Timestamp.IsZero() {
		req = tc.GrpcReq.Timestamp
	}
	if !tc.HTTPResp.Timestamp.IsZero() {
		resp = tc.HTTPResp.Timestamp
	} else if !tc.GrpcResp.Timestamp.IsZero() {
		resp = tc.GrpcResp.Timestamp
	}
	if req.IsZero() && tc.Created > 0 {
		req = time.Unix(tc.Created, 0)
	}
	if resp.IsZero() && !req.IsZero() {
		// When the recording has no response timestamp, fall back to
		// the request timestamp plus a 1-second grace. That covers the
		// common case where the app responds quickly and the missing
		// resp timestamp comes from a fixture gap rather than a long
		// request. Longer-running requests will simply lose some
		// trailing mocks from the window — still deterministic.
		resp = req.Add(time.Second)
	}
	return req, resp
}

// upsertActualTestMockMapping updates the actual test-to-mock mappings with the mocks
// consumed during the replay of a specific test case.
//
// tcReq / tcResp are the RECORD-TIME request/response timestamps of the test case.
// When both are non-zero, consumed mocks are filtered by their recorded
// ReqTimestampMock / ResTimestampMock falling inside [tcReq, tcResp]. This keeps
// mapping assignment deterministic on the record data instead of tracking
// replay-time consumption order, which can drift when a dependency reconnects
// across tests (e.g. a Redis client re-handshaking during a later test's
// window would otherwise attribute the startup-time HELLO mock to the wrong
// test case). When both timestamps are zero, filtering is skipped and the
// legacy append-all behavior is used.
func upsertActualTestMockMapping(actualTestMockMappings *models.Mapping, testCaseID string, consumedMocks []models.MockState, tcReq, tcResp time.Time) {
	if actualTestMockMappings == nil || testCaseID == "" || len(consumedMocks) == 0 {
		return
	}

	filter := !tcReq.IsZero() && !tcResp.IsZero() && !tcResp.Before(tcReq)

	newMocks := make([]models.MockEntry, 0, len(consumedMocks))
	for _, m := range consumedMocks {
		timestamp := m.Timestamp
		var parsedReqTime, parsedResTime time.Time
		if m.ReqTimestampMock != "" {
			if t, err := time.Parse(time.RFC3339Nano, m.ReqTimestampMock); err == nil {
				parsedReqTime = t
				timestamp = t.Unix()
			}
		}
		if m.ResTimestampMock != "" {
			if t, err := time.Parse(time.RFC3339Nano, m.ResTimestampMock); err == nil {
				parsedResTime = t
			}
		}

		if filter {
			// Keep the mock only if its recorded request or response
			// timestamp overlaps the test case's recorded window.
			// Mocks recorded strictly before the test case's request
			// or strictly after its response belong to a different
			// test (or to session-level traffic that should not be
			// per-test). DNS mocks have no stable timestamps and are
			// intentionally never filtered out here.
			//
			// Lifetime carve-out: session- and connection-tier mocks
			// are recorded during app boot or pool warm-up — strictly
			// before the first HTTP test fires. Their ReqTimestampMock
			// is therefore *always* outside every test window, even
			// though the test legitimately consumes them at replay
			// time (handshake, SET TIME ZONE, prepared-Parse, JDBC
			// keepalive). The window filter must NOT discard them or
			// mappings.yaml comes back empty for any test whose mock
			// budget is dominated by session/connection traffic
			// (postgres v3 wire-features, listmonk's HikariCP-style
			// warm-up, mongo's handshake/heartbeat). DNS uses the same
			// always-keep semantics for an analogous reason: its
			// timestamps aren't anchored to a test window.
			switch {
			case strings.EqualFold(string(m.Kind), string(models.DNS)):
				// DNS: always keep.
			case m.Lifetime == models.LifetimeSession || m.Lifetime == models.LifetimeConnection:
				// Session/connection-tier: always keep — the recorder
				// stamped them outside every test window by design.
			default:
				reqInWindow := !parsedReqTime.IsZero() && !parsedReqTime.Before(tcReq) && !parsedReqTime.After(tcResp)
				resInWindow := !parsedResTime.IsZero() && !parsedResTime.Before(tcReq) && !parsedResTime.After(tcResp)
				hasAnyTimestamp := !parsedReqTime.IsZero() || !parsedResTime.IsZero()
				// If we have ANY record timestamp and none of them
				// fall in the test's window, drop it. If the mock has
				// no record timestamp at all, we can't tell — keep it
				// to preserve the legacy behavior for that edge case.
				if hasAnyTimestamp && !reqInWindow && !resInWindow {
					continue
				}
			}
		}

		newMocks = append(newMocks, models.MockEntry{
			Name:             m.Name,
			Kind:             string(m.Kind),
			Timestamp:        timestamp,
			ReqTimestampMock: m.ReqTimestampMock,
			ResTimestampMock: m.ResTimestampMock,
		})
	}

	if len(newMocks) == 0 {
		return
	}

	// If the test case already has an entry, append the new mocks to it.
	for i := range actualTestMockMappings.TestCases {
		if actualTestMockMappings.TestCases[i].ID == testCaseID {
			actualTestMockMappings.TestCases[i].Mocks = append(actualTestMockMappings.TestCases[i].Mocks, newMocks...)
			return
		}
	}

	// No existing entry — create a new one.
	actualTestMockMappings.TestCases = append(actualTestMockMappings.TestCases, models.MappedTestCase{
		ID:    testCaseID,
		Mocks: newMocks,
	})
}

type TestFailure struct {
	TestSetID      string
	TestID         string
	ExpectedMocks  []string
	ActualMocks    []string
	FailureReason  models.ParserErrorType
	MismatchReport *models.MockMismatchReport
}

type TestFailureStore struct {
	mu       sync.Mutex
	failures []TestFailure
}

func NewTestFailureStore() *TestFailureStore {
	return &TestFailureStore{
		failures: make([]TestFailure, 0),
	}
}

func (tfs *TestFailureStore) AddFailure(testSetID, testID string, expectedMocks, actualMocks []string) {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	failure := TestFailure{
		TestSetID:     testSetID,
		TestID:        testID,
		ExpectedMocks: expectedMocks,
		ActualMocks:   actualMocks,
	}
	tfs.failures = append(tfs.failures, failure)
}

// AddUnmatchedCallForTest records a mock-not-found error fetched via the
// agent's GetMockErrors API (the path used on all transports, notably the
// HTTP agent transport whose error channel is nil) so it appears in the
// end-of-run mock mismatch report alongside channel-sourced failures.
func (tfs *TestFailureStore) AddUnmatchedCallForTest(testSetID string, testCaseID string, call models.UnmatchedCall) {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	failure := TestFailure{
		TestSetID:     testSetID,
		TestID:        testCaseID,
		ExpectedMocks: []string{},
		ActualMocks:   []string{},
		FailureReason: models.ErrMockNotFound,
		MismatchReport: &models.MockMismatchReport{
			Protocol:       call.Protocol,
			ActualSummary:  call.ActualSummary,
			Destination:    call.Destination,
			ClosestMock:    call.ClosestMock,
			Diff:           call.Diff,
			NextSteps:      call.NextSteps,
			MatchPhase:     call.MatchPhase,
			CandidateCount: call.CandidateCount,
			FieldDiffs:     call.FieldDiffs,
			ClosestMockReq: call.ClosestMockReq,
			ReceivedReq:    call.ReceivedReq,
		},
	}
	tfs.failures = append(tfs.failures, failure)
}

func (tfs *TestFailureStore) GetFailures() []TestFailure {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	// Return a copy to prevent external modifications
	failures := make([]TestFailure, len(tfs.failures))
	copy(failures, tfs.failures)
	return failures
}

type MockDifference struct {
	Key            string
	ExpectedValues []string
	ActualValues   []string
	DiffType       string // "missing", "extra", "different"
}

// CompareMockSlices compares two mock slices and returns the differences
func CompareMockSlices(expected, actual []string) []MockDifference {
	var differences []MockDifference

	// Convert slices to maps for easier comparison
	expectedMap := make(map[string]bool)
	actualMap := make(map[string]bool)

	for _, mock := range expected {
		expectedMap[mock] = true
	}
	for _, mock := range actual {
		actualMap[mock] = true
	}

	// Get all unique keys
	allKeys := make(map[string]bool)
	for mock := range expectedMap {
		allKeys[mock] = true
	}
	for mock := range actualMap {
		allKeys[mock] = true
	}

	// Sort keys for consistent output
	var keys []string
	for key := range allKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		_, expectedExists := expectedMap[key]
		_, actualExists := actualMap[key]

		if !expectedExists && actualExists {
			differences = append(differences, MockDifference{
				Key:            key,
				ExpectedValues: []string{},
				ActualValues:   []string{key},
				DiffType:       "extra",
			})
		} else if expectedExists && !actualExists {
			differences = append(differences, MockDifference{
				Key:            key,
				ExpectedValues: []string{key},
				ActualValues:   []string{},
				DiffType:       "missing",
			})
		}
	}

	return differences
}

// PrintFailuresTable prints all failures, one block per failed test case: a
// "Mock mismatch" heading for each unmatched outgoing call followed by the
// same Expect/Actual diff used for HTTP test-case mismatches, and the hint.
func (tfs *TestFailureStore) PrintFailuresTable() {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	if len(tfs.failures) == 0 {
		fmt.Println("No test failures recorded.")
		return
	}

	fmt.Println("\n=========================== MOCK MISMATCH ===========================")

	testSetGroups := make(map[string][]TestFailure)
	for _, failure := range tfs.failures {
		testSetGroups[failure.TestSetID] = append(testSetGroups[failure.TestSetID], failure)
	}
	var testSetIDs []string
	for testSetID := range testSetGroups {
		testSetIDs = append(testSetIDs, testSetID)
	}
	sort.Strings(testSetIDs)

	for _, testSetID := range testSetIDs {
		testIDGroups := make(map[string][]TestFailure)
		for _, failure := range testSetGroups[testSetID] {
			testIDGroups[failure.TestID] = append(testIDGroups[failure.TestID], failure)
		}
		var testIDs []string
		for testID := range testIDGroups {
			testIDs = append(testIDs, testID)
		}
		sort.Strings(testIDs)

		for _, testID := range testIDs {
			fmt.Printf("\nTest: %s / %s\n", testSetID, testID)

			var mappingNotes []string
			for _, failure := range testIDGroups[testID] {
				if failure.FailureReason == models.ErrMockNotFound {
					printMismatchReport(failure.MismatchReport)
				}
				if len(failure.ExpectedMocks) > 0 || len(failure.ActualMocks) > 0 {
					differences := CompareMockSlices(failure.ExpectedMocks, failure.ActualMocks)
					var missingMocks, extraMocks []string
					for _, diff := range differences {
						switch diff.DiffType {
						case "missing":
							missingMocks = append(missingMocks, diff.Key)
						case "extra":
							extraMocks = append(extraMocks, diff.Key)
						}
					}
					if len(missingMocks) > 0 {
						mappingNotes = append(mappingNotes, fmt.Sprintf("Expected mocks not consumed: %s", strings.Join(missingMocks, ", ")))
					}
					if len(extraMocks) > 0 {
						mappingNotes = append(mappingNotes, fmt.Sprintf("Unexpected mocks consumed: %s", strings.Join(extraMocks, ", ")))
					}
				}
			}
			for _, note := range mappingNotes {
				fmt.Printf("  %s\n", note)
			}
		}
	}
	fmt.Println()
}

// printMismatchReport renders one unmatched outgoing call: a heading, then the
// best available view — a side-by-side whole-request diff when the parser
// rendered both requests, else the structured FIELD | EXPECTED | RECEIVED
// table, else the one-line Diff — followed by the hint.
func printMismatchReport(r *models.MockMismatchReport) {
	if r == nil {
		fmt.Println("  Outgoing call mock was not matched")
		return
	}
	heading := fmt.Sprintf("  Mock mismatch: [%s] %s", r.Protocol, r.ActualSummary)
	if r.Destination != "" {
		heading += fmt.Sprintf("  →  %s", r.Destination)
	}
	if r.ClosestMock != "" {
		heading += fmt.Sprintf("   (closest mock: %s)", r.ClosestMock)
	}
	if r.MatchPhase != "" {
		heading += fmt.Sprintf("   [match stopped at: %s", r.MatchPhase)
		if r.CandidateCount > 0 {
			heading += fmt.Sprintf(", %d candidate mock(s)", r.CandidateCount)
		}
		heading += "]"
	}
	fmt.Println(heading)

	switch {
	case r.ClosestMockReq != "" && r.ReceivedReq != "":
		printRequestDiff(r.ClosestMock, r.ClosestMockReq, r.ReceivedReq)
	case len(r.FieldDiffs) > 0:
		printFieldDiffTable(r.FieldDiffs)
	case r.Diff != "":
		fmt.Printf("  Diff: %s\n", strings.ReplaceAll(r.Diff, "\n", " "))
	}

	if r.NextSteps != "" {
		fmt.Printf("  Hint: %s\n", strings.ReplaceAll(r.NextSteps, "\n", " "))
	}
}

// printRequestDiff renders the closest mock's recorded request against the live
// request in the SAME visual style as an HTTP test-case (response) mismatch: a
// boxed diff whose rows are Expect/Actual sub-tables for the request line,
// headers and body, produced by the shared matcher.DiffsPrinter. expected and
// received are the whole rendered requests (request line + headers + body) the
// parser emitted; the body therefore diffs field-by-field (changed fields
// only), exactly like a response-body mismatch.
func printRequestDiff(mockName, expected, received string) {
	title := mockName
	if title == "" {
		title = "closest mock"
	}
	dp := matcherUtils.NewDiffsPrinter(title)

	mockLine, mockHeaders, mockBody := parseRenderedRequest(expected)
	liveLine, liveHeaders, liveBody := parseRenderedRequest(received)

	// Request line -> method + target, surfaced as Expect/Actual rows only when
	// they differ (for a body mismatch the closest mock shares method/path).
	mockMethod, mockTarget := splitRequestLine(mockLine)
	liveMethod, liveTarget := splitRequestLine(liveLine)
	if mockMethod != liveMethod {
		dp.PushHeaderDiff(mockMethod, liveMethod, "method", nil)
	}
	if mockTarget != liveTarget {
		dp.PushHeaderDiff(mockTarget, liveTarget, "url", nil)
	}

	// Headers: push a row whenever a key is present on only one side or its
	// value differs. Two-value lookups distinguish an absent header from one
	// present with an empty value, so a key-presence mismatch (the exact reason
	// a mock can be rejected) is never silently dropped.
	for k, mv := range mockHeaders {
		if lv, ok := liveHeaders[k]; !ok || mv != lv {
			dp.PushHeaderDiff(mv, lv, k, nil)
		}
	}
	for k, lv := range liveHeaders {
		if _, ok := mockHeaders[k]; !ok {
			dp.PushHeaderDiff("", lv, k, nil)
		}
	}

	// Body: JSON bodies diff field-by-field (changed fields only), just like a
	// response body mismatch; non-JSON falls back to a plain Expect/Actual cell.
	dp.PushBodyDiff(mockBody, liveBody, nil)

	_ = dp.Render()
}

// parseRenderedRequest splits a rendered request
// ("METHOD target\nKey: val\n...\n\n<body>") back into its request line, header
// map and body so the pieces can feed the shared Expect/Actual diff renderer.
func parseRenderedRequest(s string) (line string, headers map[string]string, body string) {
	headers = map[string]string{}
	headBlock := s
	if i := strings.Index(s, "\n\n"); i >= 0 {
		headBlock, body = s[:i], s[i+2:]
	}
	lines := strings.Split(headBlock, "\n")
	if len(lines) == 0 {
		return line, headers, body
	}
	line = lines[0]
	for _, l := range lines[1:] {
		if j := strings.Index(l, ": "); j >= 0 {
			headers[l[:j]] = l[j+2:]
		}
	}
	return line, headers, body
}

// splitRequestLine splits "METHOD target" into its method and target.
func splitRequestLine(line string) (method, target string) {
	if i := strings.Index(line, " "); i >= 0 {
		return line[:i], line[i+1:]
	}
	return line, ""
}

// printFieldDiffTable renders the compact FIELD | EXPECTED | RECEIVED table —
// the fallback when whole-request renders aren't available. Reads the
// noise-vocabulary MockFieldDiff (Path/Kind/Expected/Actual).
func printFieldDiffTable(fieldDiffs []models.MockFieldDiff) {
	colWidths := tw.NewMapper[int, int]().Set(0, 24).Set(1, 40).Set(2, 40)
	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithRendition(tw.Rendition{
			Settings: tw.Settings{Separators: tw.Separators{BetweenRows: tw.On}},
		}),
		tablewriter.WithRowAutoWrap(1),
		tablewriter.WithHeaderAlignment(tw.AlignCenter),
		tablewriter.WithRowAlignment(tw.AlignLeft),
		tablewriter.WithMaxWidth(120),
		tablewriter.WithColumnWidths(colWidths),
	)
	table.Header([]string{"FIELD", "EXPECTED (MOCK)", "RECEIVED (REQUEST)"})
	for _, d := range fieldDiffs {
		exp, rec := d.Expected, d.Actual
		switch d.Kind {
		case models.DiffKindMissingInLive:
			rec = "(missing)"
		case models.DiffKindMissingInMock:
			exp = "(not recorded)"
		case models.DiffKindTypeChanged:
			exp, rec = "type "+d.Expected, "type "+d.Actual
		}
		table.Append([]string{d.Path, exp, rec})
	}
	table.Render()
}
