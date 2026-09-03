package replay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"sync"
	"time"

	"gopkg.in/yaml.v3"

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
// dockerPublishedHostPort returns the first host (ip, port) that a docker
// `-p/--publish` mapping in cmd exposes on the host, so a docker-run replay can
// gate on the app actually listening before firing requests. It handles the
// common `hostPort:containerPort` and `hostIP:hostPort:containerPort` forms
// (with an optional `/tcp`|`/udp` suffix). It returns ok=false — keeping the
// pure --delay behavior — for native apps (no -p), container-only publishes
// (random host port), port ranges, or anything it cannot parse with
// confidence, so it never guesses a wrong port to wait on.
func dockerPublishedHostPort(cmd string) (host, port string, ok bool) {
	fields := strings.Fields(cmd)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "-p" && fields[i] != "--publish" {
			continue
		}
		spec := strings.SplitN(fields[i+1], "/", 2)[0] // drop /tcp,/udp
		segs := strings.Split(spec, ":")
		// host port is the second-to-last segment:
		//   "hp:cp"     -> [hp, cp]
		//   "ip:hp:cp"  -> [ip, hp, cp]
		//   "cp"        -> [cp]  (random host port; not waitable -> skip)
		if len(segs) < 2 {
			continue
		}
		hp := segs[len(segs)-2]
		h := "127.0.0.1"
		if len(segs) >= 3 && segs[0] != "" {
			h = segs[0]
		}
		if hp == "" || strings.ContainsAny(hp, "-") { // skip empty / ranges
			continue
		}
		return h, hp, true
	}
	return "", "", false
}

// composePublishedHostPort finds the host port the application's service
// publishes in a docker-compose file, so a compose replay gets the same
// readiness gate a `docker run -p` replay already gets.
//
// Without it, compose apps have no gate at all: dockerPublishedHostPort only
// recognises -p/--publish on the command line, and `docker compose up` carries
// none, so the fixed --delay is the entire protection. That is not a
// theoretical gap — keploy's own generated compose holds the application behind
// the agent's healthcheck (start_period 10s, interval 5s), so on a contended
// machine the app starts as the delay expires, the first test window opens
// while the app is still booting, and its bootstrap queries are scored against
// a per-test pool that has nothing in it. The app then dies on a refused
// dependency and every test in the set fails, while the diagnostic points at
// the mocks and tells the user to re-record.
//
// Conservative like its sibling: it returns ok=false rather than guess. The
// service is identified by container_name so a multi-service stack cannot be
// mistaken for the app, falling back to a lone port-publishing service only
// when the stack is unambiguous. Short and long port syntax are both read;
// ranges and container-only publishes are skipped because neither names a host
// port to wait on.
func composePublishedHostPort(cmd, containerName string) (host, port string, ok bool) {
	path, ok := composeFileFromCommand(cmd)
	if !ok {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	var doc struct {
		Services map[string]struct {
			ContainerName string      `yaml:"container_name"`
			Ports         []yaml.Node `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", "", false
	}

	type candidate struct{ host, port string }
	var byName, only []candidate
	published := 0
	for _, svc := range doc.Services {
		h, p, found := firstComposeHostPort(svc.Ports)
		if !found {
			continue
		}
		published++
		only = append(only, candidate{h, p})
		if containerName != "" && svc.ContainerName == containerName {
			byName = append(byName, candidate{h, p})
		}
	}
	if len(byName) == 1 {
		return byName[0].host, byName[0].port, true
	}
	// No container_name match: only trust the stack when exactly one service
	// publishes anything, so a db or cache port is never mistaken for the app.
	if published == 1 {
		return only[0].host, only[0].port, true
	}
	return "", "", false
}

// composeFileFromCommand returns the compose file the command operates on,
// honouring an explicit -f/--file and otherwise falling back to the names
// docker compose itself looks for in the working directory.
func composeFileFromCommand(cmd string) (string, bool) {
	fields := strings.Fields(cmd)
	isCompose := false
	for i, f := range fields {
		if f == "compose" || strings.HasPrefix(f, "docker-compose") {
			isCompose = true
		}
		if (f == "-f" || f == "--file") && i+1 < len(fields) {
			return fields[i+1], true
		}
	}
	if !isCompose {
		return "", false
	}
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		if _, err := os.Stat(name); err == nil {
			return name, true
		}
	}
	return "", false
}

// firstComposeHostPort reads the first entry that names a host port, in either
// the short form ("8080:80", "127.0.0.1:8080:80") or the long form
// (published: 8080). Ranges and container-only publishes are skipped: neither
// gives a single port that can be waited on.
func firstComposeHostPort(ports []yaml.Node) (host, port string, ok bool) {
	for i := range ports {
		n := &ports[i]
		switch n.Kind {
		case yaml.ScalarNode:
			if h, p, found := parseShortPortSpec(n.Value); found {
				return h, p, true
			}
		case yaml.MappingNode:
			var long struct {
				Published interface{} `yaml:"published"`
				HostIP    string      `yaml:"host_ip"`
			}
			if err := n.Decode(&long); err != nil || long.Published == nil {
				continue
			}
			p := strings.TrimSpace(fmt.Sprintf("%v", long.Published))
			if p == "" || strings.ContainsAny(p, "-") {
				continue
			}
			h := long.HostIP
			if h == "" {
				h = "127.0.0.1"
			}
			return h, p, true
		}
	}
	return "", "", false
}

// parseShortPortSpec reads docker's short port syntax, sharing
// dockerPublishedHostPort's rules: the host port is the second-to-last
// colon-separated segment, and anything without one (a bare container port, so
// a random host port) or with a range is refused.
func parseShortPortSpec(spec string) (host, port string, ok bool) {
	spec = strings.SplitN(strings.TrimSpace(spec), "/", 2)[0]
	segs := strings.Split(spec, ":")
	if len(segs) < 2 {
		return "", "", false
	}
	hp := segs[len(segs)-2]
	if hp == "" || strings.ContainsAny(hp, "-") {
		return "", "", false
	}
	h := "127.0.0.1"
	if len(segs) >= 3 && segs[0] != "" {
		h = segs[0]
	}
	return h, hp, true
}

// keployReadinessProbePath is the path the reset-resend readiness probe requests.
// It is deliberately one no real app routes, so a well-behaved server answers it
// with a 404 WITHOUT invoking an application handler — the probe therefore never
// consumes a mock or triggers an app side effect. Any HTTP response to it (the
// 404 included) still proves the proxy→app path is serving.
const keployReadinessProbePath = "/keploy/__readiness_probe__"

// resetProbeRequestTimeout bounds a single readiness probe round-trip.
const resetProbeRequestTimeout = 800 * time.Millisecond

// resetProbeInterval is the pause between readiness probe attempts.
const resetProbeInterval = 200 * time.Millisecond

// resolveProbeTarget returns the scheme, host and port the test case's HTTP
// request is ACTUALLY dialed to, so the readiness probe hits the SAME endpoint
// (and address family) the real request will. It must mirror the simulation's
// resolution — the recorded tc.HTTPReq.URL is NOT the dial target: SimulateHTTP
// rewrites it via ResolveTestTarget (ConfigHost/--host override, replaceWith,
// port precedence). In the couchbase suite the recorded host is 127.0.0.1 but
// the default ConfigHost "localhost" rewrites it to localhost → IPv6 ::1, which
// is exactly the path that resets; probing the recorded 127.0.0.1 would gate on
// IPv4 (which never resets) and defeat the fix. Returns ok=false for a nil /
// non-HTTP test case or an unresolvable target (the caller then falls back to
// the docker published-port TCP gate).
//
// It resolves from the pre-template-render URL, so a {{ }} placeholder in the
// host/port (as opposed to the path/query/body, where they actually live) would
// make the probe target diverge from the dial. That is best-effort only and never
// affects correctness, but MIND THE BUDGET, because it differs by caller:
//
//   - waitForResetResendReady bounds a wrong target by resetResendReadyTimeout
//     (5s) and the re-send still runs.
//   - gateOnAppAddress's HTTP stage, via resolveTestSetProbeTarget, is bounded by
//     test.healthPollTimeout instead — 3 minutes by default and operator-
//     configurable. So a target that is resolvable but DEAD (an un-rendered
//     template in the host, or a --host override pointing nowhere) makes the
//     pre-test gate retry until that budget expires before warning and firing
//     anyway, on every test-set.
//
// That residual is deliberate rather than overlooked. Shortening it would take
// the budget away from the case the gate exists for — a cold application start
// measured at ~55s to first serve — and a target this resolver gets wrong is
// almost always one the real dispatch will get wrong too, so the run was going
// to fail regardless. It is bounded, warn-and-proceed, and never turns a passing
// run into a failing one.
func resolveProbeTarget(testCfg config.Test, tc *models.TestCase, testSetID string, logger *zap.Logger) (scheme, host, port string, ok bool) {
	if tc == nil || tc.Kind != models.HTTP {
		return "", "", "", false
	}
	urlReplacements, portMappings := mergeReplaceWith(testCfg, testSetID)
	hostToUse := testCfg.Host
	if hostToUse == "" {
		hostToUse = "localhost"
	}
	configPort := effectiveHTTPConfigPort(tc, testCfg)

	target, err := pkg.ResolveTestTarget(tc.HTTPReq.URL, urlReplacements, portMappings, hostToUse, tc.AppPort, configPort, true, logger)
	if err != nil {
		return "", "", "", false
	}
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil || u == nil {
		return "", "", "", false
	}
	scheme = u.Scheme
	if scheme != "http" && scheme != "https" {
		return "", "", "", false
	}
	host = u.Hostname()
	if host == "" {
		return "", "", "", false
	}
	port = u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return scheme, host, port, true
}

// isTLSVerificationError reports whether err is the TLS handshake failing on
// certificate VERIFICATION, as opposed to the connection never getting that far.
//
// The distinction is the whole point: an untrusted or mismatched certificate
// means the peer is up, listening, and speaking TLS. A refusal, reset or timeout
// means it is not. A readiness probe cares only about the latter.
func isTLSVerificationError(err error) bool {
	if err == nil {
		return false
	}
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}
	var invalid x509.CertificateInvalidError
	return errors.As(err, &invalid)
}

// waitForHTTPServing polls until an HTTP round-trip to host:port completes with
// ANY status — proving the proxy→app path actually serves — or ctx expires. A
// transport reset / refusal / timeout counts as not-ready and keeps waiting.
//
// This is the HTTP-level counterpart to a bare TCP-accept gate: docker's
// userland-proxy ACCEPTS a freshly-published host-port connection and only then
// resets it mid-response under load, so a completed TCP handshake does NOT imply
// the next request will land. Only a finished HTTP exchange proves the app is
// truly reachable. Fresh (keep-alive-disabled) connections mirror the real
// per-request connect the proxy resets. The probe hits a keploy-reserved 404
// path (see keployReadinessProbePath) so a well-behaved app answers it without
// invoking a handler; and in the reset (failure) state the userland-proxy resets
// the probe before the app ever sees it, so it never reaches application code.
func waitForHTTPServing(ctx context.Context, scheme, host, port string) error {
	return waitForHTTPServingPath(ctx, scheme, host, port, keployReadinessProbePath)
}

// waitForHTTPServingPath is waitForHTTPServing with an explicit request path, so
// an operator-supplied health path (test.healthPath) can be probed instead of the
// keploy-reserved one. An empty path falls back to the reserved probe path.
//
// The contract is identical either way: ANY completed HTTP response — including
// 404 or 503 — proves the proxy→app path serves, which is the only thing a
// readiness gate needs to know. We deliberately do NOT require 2xx here: a health
// endpoint that reports "unhealthy" has still proved the app is listening and
// answering, and holding replay back for application-level health would turn a
// readiness gate into a liveness policy.
func waitForHTTPServingPath(ctx context.Context, scheme, host, port, path string) error {
	if path == "" {
		path = keployReadinessProbePath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, port), path)
	client := &http.Client{
		Timeout: resetProbeRequestTimeout,
		// Don't follow redirects — any 3xx already proves the app serves.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			// Fresh connection per probe so we exercise the same connect the
			// userland-proxy resets, rather than reusing a warm one.
			DisableKeepAlives: true,
			// Never route a loopback readiness probe through an env proxy.
			Proxy: nil,
		},
	}
	defer client.CloseIdleConnections()

	probe := func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			// A certificate we do not trust still answers the only question this
			// gate asks. Verification failing means the server completed a TLS
			// handshake and presented a certificate — it is listening and
			// serving; we simply do not vouch for who it is. A self-signed cert
			// is the norm for a local or CI HTTPS fixture, so treating that as
			// "not ready" would burn the whole ceiling on every run.
			//
			// Deliberately NOT solved with InsecureSkipVerify: that disables the
			// check everywhere in this client and is a real finding for scanners
			// to flag. Reading the specific error keeps verification on and only
			// reinterprets its meaning for a readiness decision.
			if isTLSVerificationError(err) {
				return true
			}
			return false // transport reset / refused / timeout → not ready yet
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return true // any HTTP response means the app is serving
	}

	if probe() {
		return nil
	}
	ticker := time.NewTicker(resetProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if probe() {
				return nil
			}
		}
	}
}

// gateOnAppAddress blocks until host:port looks ready, then returns. It is the
// single implementation behind every pre-test readiness gate.
//
// Two stages, because one is not enough:
//
//  1. TCP accept. Proves something is bound. Works for any protocol, so it is
//     all we can do for a non-HTTP app.
//  2. A real HTTP round-trip, but ONLY when the app is known to serve HTTP.
//
// Stage 2 exists because stage 1 is not sufficient for the case this gate was
// built for. Docker's userland proxy binds and ACCEPTS on the published host
// port as soon as the container is created — before anything inside the
// container is listening — and then RESETS the connection when it cannot reach
// the app. A TCP-accept gate therefore passes instantly and hands replay a port
// that will reset its first request, which is exactly the status_code=0 flake on
// the first test of a set. Only a completed HTTP exchange proves the whole
// proxy→app path carries a request.
//
// "Known to serve HTTP" is not a guess: it is derived from the recorded test set
// (see testSetServesHTTP). If we recorded HTTP requests against this app, the app
// serves HTTP. For a non-HTTP app (MySQL, Redis, a gRPC-only service) stage 2 is
// skipped entirely rather than burning the ceiling on a probe that can never
// parse a response.
//
// Best-effort throughout, preserving the pre-existing contract: it can only ever
// wait LONGER than --delay, never shorter; a timeout WARNS and proceeds rather
// than failing the run; and it never weakens an assertion. Returns false only on
// context cancellation (user abort).
// gateMode tweaks how gateOnAppAddress probes. The zero value runs both
// stages; httpOnly skips the stage-1 TCP dial, for the one caller whose
// stage-1 and stage-2 addresses are identical and whose bare
// connect-then-close is therefore both redundant and harmful.
type gateMode int

const (
	bothStages gateMode = iota
	httpOnly
)

func gateOnAppAddress(ctx context.Context, logger *zap.Logger, cfg *config.Config, host, port, label string, probe httpProbeTarget, modes ...gateMode) bool {
	mode := bothStages
	if len(modes) > 0 {
		mode = modes[0]
	}
	ceiling := cfg.Test.HealthPollTimeout
	if ceiling <= 0 {
		ceiling = config.DefaultHealthPollTimeout
	}
	deadline := time.Now().Add(ceiling)

	// Stage 1: the address accepts a TCP connection. Skipped in
	// httpOnly mode, where stage 2 probes the very same address and the
	// bare connect-then-close would be redundant — see the rationale at
	// the resolved-test-target call site.
	if mode != httpOnly {
		if err := pkg.WaitForPort(ctx, host, port, ceiling); err != nil {
			if ctx.Err() != nil {
				return false
			}
			logger.Warn("app address did not accept a connection within the ceiling; firing tests anyway (replay may see status_code got=0)",
				zap.String("gate", label),
				zap.String("host", host),
				zap.String("port", port),
				zap.Duration("ceiling", ceiling),
				zap.Error(err))
			return true
		}
	}

	if !probe.ok {
		return true
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return true
	}
	hctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()

	// Scheme comes from the resolved target; healthScheme is an explicit override
	// for the rare case an operator needs to force it.
	scheme := probe.scheme
	if forced := strings.TrimSpace(cfg.Test.HealthScheme); forced != "" && !strings.EqualFold(forced, scheme) {
		if strings.EqualFold(forced, "http") || strings.EqualFold(forced, "https") {
			scheme = strings.ToLower(forced)
		}
	}
	if err := waitForHTTPServingPath(hctx, scheme, probe.host, probe.port, cfg.Test.HealthPath); err != nil {
		if ctx.Err() != nil {
			return false
		}
		logger.Warn("app accepted TCP but never completed an HTTP round-trip within the ceiling; firing tests anyway (replay may see status_code got=0)",
			zap.String("gate", label),
			zap.String("probeHost", probe.host),
			zap.String("probePort", probe.port),
			zap.String("probePath", healthProbePathForLog(cfg.Test.HealthPath)),
			zap.Duration("ceiling", ceiling),
			zap.Error(err))
		return true
	}
	logger.Debug("app readiness confirmed by a completed HTTP round-trip",
		zap.String("gate", label),
		zap.String("probeHost", probe.host),
		zap.String("probePort", probe.port),
		zap.String("probePath", healthProbePathForLog(cfg.Test.HealthPath)))
	return true
}

// healthProbePathForLog reports the path gateOnAppAddress actually requests.
func healthProbePathForLog(configured string) string {
	if configured == "" {
		return keployReadinessProbePath
	}
	if !strings.HasPrefix(configured, "/") {
		return "/" + configured
	}
	return configured
}

// httpProbeTarget is the address a recorded HTTP test case actually dials, used
// as the target of the readiness gate's HTTP stage.
type httpProbeTarget struct {
	scheme, host, port string
	ok                 bool
}

// resolveTestSetProbeTarget returns the address the readiness gate should complete
// an HTTP round-trip against, derived from the recorded test set.
//
// This is EVIDENCE, not a guess, and the distinction is the whole point. An
// earlier revision decided only WHETHER to escalate from the test set and then
// escalated against whatever address the docker/compose heuristic produced —
// dockerPublishedHostPort returns the FIRST -p flag it finds, with no check that
// it is the HTTP port. For `-p 5432:5432 -p 8080:8080` that is the database, and
// an HTTP probe against it can never succeed: stage 1 passes instantly because
// the port is bound, then stage 2 burns the entire ceiling on EVERY run before
// warning and proceeding. Same for an operator who points appReadyProbeAddr at a
// non-HTTP service, which its own documentation invites.
//
// resolveProbeTarget mirrors the simulation's own ResolveTestTarget (ConfigHost /
// --host override, replaceWith, port precedence) so the probe hits exactly what
// the test will dial, and is address-family-correct — an IPv6 "localhost" dial is
// gated on the IPv6 path, not a mismatched 127.0.0.1. It is the same resolver the
// reset re-send gate already uses.
//
// Returns ok=false when no test case resolves (empty set, non-HTTP set, or an
// unresolvable target); the caller then keeps the TCP-accept stage only, which is
// exactly the pre-existing behaviour.
func resolveTestSetProbeTarget(testCfg config.Test, testCases []*models.TestCase, testSetID string, logger *zap.Logger) httpProbeTarget {
	for _, tc := range testCases {
		if tc == nil || tc.Kind != models.HTTP {
			continue
		}
		if scheme, host, port, ok := resolveProbeTarget(testCfg, tc, testSetID, logger); ok {
			return httpProbeTarget{scheme: scheme, host: host, port: port, ok: true}
		}
	}
	return httpProbeTarget{}
}

func waitForAppReady(ctx context.Context, logger *zap.Logger, cfg *config.Config, probe httpProbeTarget) bool {
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
			// --delay floor elapsed; fall through to the optional docker
			// port-readiness gate below.
		case <-ctx.Done():
			return false
		}

		// Docker apps with a published host port: after the --delay floor,
		// additionally gate on the host port accepting a TCP connection so
		// replay never fires at a not-yet-listening app (the "status_code
		// got=0" flake) when the fixed --delay was too short under CI
		// contention. Best-effort and bounded by a ceiling; native apps and
		// container-only / unmapped publishes (ok==false) keep the pure
		// --delay behavior. This can only ever wait LONGER than --delay, never
		// shorter, and never weakens an assertion — a fast-ready port returns
		// instantly.

		// An embedder can state that probing is unsafe here. Honoured
		// for every address-based gate below, not just the fallback,
		// and for the reset-resend readiness re-gate in replay.go:
		// the hazard is the connect-then-close itself, and all of these
		// perform one. --health-url is untouched — that is an explicit
		// operator request rather than an inferred probe.
		//
		// This is a stated intent, not an inferred one, because the
		// inference is unreliable: a caller in this position still sets
		// test.host (it rewrites recorded request URLs), so "no gate
		// input was supplied" is simply false for it, and the
		// resolved-test-target fallback would reach for that address
		// and probe it anyway. See config.Test.DisableAppReadyProbe.
		if cfg.Test.DisableAppReadyProbe {
			// Info, not Debug: a safety gate being switched off should
			// be visible at default verbosity, the way the health poll
			// announces itself below.
			logger.Info("app-readiness probing disabled by configuration; using the delay floor only",
				zap.Duration("delay", delay),
			)
			// The delay select above can take its timer branch even when
			// ctx is already cancelled. Every other exit from here goes
			// through gateOnAppAddress, which checks; this one must too,
			// or a cancelled run reports ready and fires tests instead
			// of reporting the abort.
			if ctx.Err() != nil {
				return false
			}
			return true
		}

		gated := false
		if host, port, ok := dockerPublishedHostPort(cfg.Command); ok {
			gated = true
			if !gateOnAppAddress(ctx, logger, cfg, host, port, "docker-published-port", probe) {
				return false
			}
		}

		// docker-compose apps: the same gate, with the host port read from the
		// compose file instead of the command line. Identical contract to the
		// -p gate above — best-effort, bounded, can only ever wait longer.
		if host, port, ok := composePublishedHostPort(cfg.Command, cfg.ContainerName); ok {
			gated = true
			if !gateOnAppAddress(ctx, logger, cfg, host, port, "compose-published-port", probe) {
				return false
			}
		}

		// Non-docker apps whose ready address is known — a k8s replay pod's app
		// Service, a remote host, a native app on a fixed host:port — gate on that
		// address accepting a TCP connection. This is the TCP-accept analog of the
		// docker published-port gate above (and of --health-url) for apps that
		// expose no HTTP health endpoint. dockerPublishedHostPort only fires for a
		// docker `-p` publish, so this covers everyone else who can name the app's
		// address. Bounded by its own HealthPollTimeout ceiling (default 60s) and
		// the same best-effort contract: it can only ever wait LONGER than --delay,
		// never shorter, and a warn-not-fail on timeout so replay never regresses
		// vs the pure --delay behavior. It is purely a readiness probe and never
		// rewrites a request target (unlike test.port).
		if addr := strings.TrimSpace(cfg.Test.AppReadyProbeAddr); addr != "" {
			host, port, err := net.SplitHostPort(addr)
			if err != nil || port == "" {
				logger.Error("invalid test.appReadyProbeAddr; expected host:port — skipping the readiness probe",
					zap.String("addr", addr),
					zap.Error(err))
			} else {
				if host == "" {
					host = "127.0.0.1" // ":<port>" shorthand → localhost, parity with the docker gate
				}
				gated = true
				if !gateOnAppAddress(ctx, logger, cfg, host, port, "app-ready-probe-addr", probe) {
					return false
				}
			}
		}

		// Nothing above could name the app's address — a NATIVE app, the common
		// case: no docker -p publish, no compose file, no operator-supplied
		// appReadyProbeAddr. Until now that meant no readiness gate at all beyond
		// the fixed --delay, so a native app slower than that delay had its first
		// test fired into a socket nothing was listening on yet, producing exactly
		// the status_code=0 this whole gate exists to prevent. Observed on the
		// python-cred-expiry lane (`python3 recorded_app.py`, --delay 5): 2 of 4
		// tests failed with expected=200 got=0.
		//
		// The recorded test set already told us where it dials, so use it. There
		// is nothing to guess: probe.host/probe.port IS the app's address for the
		// purposes of these tests, and the same best-effort contract holds —
		// bounded by the ceiling, warn-and-proceed on timeout, only ever waits
		// longer than --delay. Guarded on `gated` purely to avoid probing twice
		// when a published address already covered it.
		if !gated && probe.ok {
			// httpOnly: skip gateOnAppAddress's stage-1 bare TCP dial.
			//
			// Here, and only here, stage 1 and stage 2 probe the SAME
			// address, so the dial proves nothing the HTTP round-trip
			// does not — waitForHTTPServingPath already treats refusal
			// and reset as not-ready and retries to the ceiling. What
			// the dial does add is a naked connect-then-close, the
			// shape a kubectl port-forward's SPDY relay is most hostile
			// to. Dropping it removes that shape from the exact path
			// that broke keploy/k8s-proxy, for every embedder that
			// never learns about test.disableAppReadyProbe, at no cost
			// in coverage.
			//
			// The docker and compose gates above deliberately pass a
			// DIFFERENT stage-1 address than the probe target, so their
			// dial is not redundant and stays.
			if !gateOnAppAddress(ctx, logger, cfg, probe.host, probe.port, "resolved-test-target", probe, httpOnly) {
				return false
			}
		}
		return true
	}

	pollCeiling := cfg.Test.HealthPollTimeout
	if pollCeiling <= 0 {
		pollCeiling = config.DefaultHealthPollTimeout
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
			Protocol:      call.Protocol,
			ActualSummary: call.ActualSummary,
			Destination:   call.Destination,
			ClosestMock:   call.ClosestMock,
			Diff:          call.Diff,
			NextSteps:     call.NextSteps,
			MatchPhase:    call.MatchPhase,
			// Carried across so the CLI rendering can tell an out-of-scope
			// destination from a drifted request; the two need different
			// artifacts, not just different hints.
			DestinationScope: call.DestinationScope,
			CandidateCount:   call.CandidateCount,
			FieldDiffs:       call.FieldDiffs,
			ClosestMockReq:   call.ClosestMockReq,
			ReceivedReq:      call.ReceivedReq,
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
			var anyOutOfScope bool
			for _, failure := range testIDGroups[testID] {
				if failure.FailureReason == models.ErrMockNotFound {
					printMismatchReport(failure.MismatchReport)
					if failure.MismatchReport != nil &&
						failure.MismatchReport.DestinationScope == models.DestinationScopeNotInComparedSet {
						anyOutOfScope = true
					}
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
			// ONCE PER TEST, not once per missed call. The likely causes are
			// static text that is identical for every out-of-scope miss, and
			// one out-of-scope container produces one miss per outgoing call
			// it makes — inlining them in each call's hint put the same ~1 KB
			// paragraph on screen dozens of times per test.
			if anyOutOfScope {
				fmt.Println(models.RenderOutOfScopeDestinationCauses("  "))
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
	// When nothing in the compared set targets this destination there is no
	// meaningful "closest mock": the matcher picked the least-distant mock in
	// the pool, which belongs to a DIFFERENT upstream. Naming it in the
	// heading and rendering a side-by-side diff against it invites the reader
	// to reconcile two requests that were never related — the first cut of
	// this diagnostic fixed only the hint and left that diff leading the
	// output. The structured evidence (ClosestMock, FieldDiffs, both rendered
	// requests) stays on the report object for the yaml/platform consumers;
	// only this human rendering suppresses it.
	outOfScope := r.DestinationScope == models.DestinationScopeNotInComparedSet

	heading := fmt.Sprintf("  Mock mismatch: [%s] %s", r.Protocol, r.ActualSummary)
	if r.Destination != "" {
		heading += fmt.Sprintf("  →  %s", r.Destination)
	}
	if r.ClosestMock != "" && !outOfScope {
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
	case outOfScope:
		// Say why the diff is missing, so its absence reads as a deliberate
		// answer rather than a renderer that gave up — but say it in six
		// words: the hint printed immediately below already opens with "No
		// recorded <protocol> mock in the compared set targets <host>." and a
		// full second copy of that sentence, once per missed call, was pure
		// duplication.
		fmt.Println("  (closest-mock diff omitted — see hint)")
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
