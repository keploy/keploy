package models

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"
)

// PassThroughMode selects how keploy treats egress to a matched telemetry /
// noisy endpoint (port/host). See PassThroughRule.
type PassThroughMode string

const (
	// PassThroughSkip: never record egress to the matched endpoint. On replay,
	// keploy synthesizes a protocol-appropriate success (HTTP 200 / gRPC
	// grpc-status:0) so the telemetry client succeeds without a recorded mock.
	// In proxyless/DaemonSet mode this is enforced in-kernel (never captured).
	PassThroughSkip PassThroughMode = "skip"
	// PassThroughRecordOne: record exactly ONE exchange per
	// (host,port,path,method) and, on replay, serve that recorded response for
	// every matching call body-agnostically (no fuzzy match). Keeps a correctly
	// shaped response for clients that parse it (e.g. Datadog /v0.7/config).
	PassThroughRecordOne PassThroughMode = "recordOne"
	// PassThroughOff: an explicit opt-out. The endpoint is matched (so it can
	// override a built-in default for the same target) but is NOT treated as
	// passthrough — it is recorded and replayed as a normal dependency. This is
	// how a user turns off a built-in telemetry default when that endpoint is
	// actually one of their own internal routes they need matched on replay.
	PassThroughOff PassThroughMode = "off"
)

// DefaultPassThroughMode is applied to a rule whose Mode is empty and to the
// bare-scalar shorthand form.
const DefaultPassThroughMode = PassThroughRecordOne

// PassThroughRule marks telemetry / noisy egress that keploy should not capture
// normally. A rule matches an egress call when every field it sets matches: Host
// matches the destination host/authority, Port equals the destination port, Path
// matches the request path (segment-boundary by default, see PathPrefix), and
// Method (if set) equals the request method. Body and query are never part of the
// match — that is the point of passthrough.
//
// Config form (keploy.yml, decoded by viper/mapstructure) is the OBJECT form only:
//   - {port: 8126, mode: skip}   or   {host: "*.datadoghq.com", mode: recordOne}
//
// The bare-scalar shorthand (a lone `8126`) is accepted ONLY on the JSON agent
// wire (UnmarshalJSON); it is NOT supported in keploy.yml because mapstructure
// does not call UnmarshalJSON. Do not document the scalar form for keploy.yml.
type PassThroughRule struct {
	Port uint32          `json:"port,omitempty" yaml:"port,omitempty" mapstructure:"port"`
	Host string          `json:"host,omitempty" yaml:"host,omitempty" mapstructure:"host"`
	Path string          `json:"path,omitempty" yaml:"path,omitempty" mapstructure:"path"`
	Mode PassThroughMode `json:"mode,omitempty" yaml:"mode,omitempty" mapstructure:"mode"`
	// Method, when non-empty, restricts the rule to one HTTP method (case-
	// insensitive). Built-in telemetry defaults set "POST" so a GET to a
	// same-named app route isn't swallowed. Empty ⇒ any method.
	Method string `json:"method,omitempty" yaml:"method,omitempty" mapstructure:"method"`
	// PathPrefix, when true, matches Path as a raw prefix (needed for the OTLP/gRPC
	// service path). When false (default) Path matches on segment boundaries only
	// (reqPath == Path, or reqPath starts with Path+"/"), so a rule for "/ingest"
	// does NOT swallow an app route "/ingestData".
	PathPrefix bool `json:"pathPrefix,omitempty" yaml:"pathPrefix,omitempty" mapstructure:"pathPrefix"`
	// QueryKeys names the query parameters that are significant to endpoint
	// identity under recordOne. Params listed here are folded into the record-time
	// dedup key and required to match at replay; every other query param (e.g. a
	// per-session run_id/token) is ignored, so recordOne still collapses repeat
	// calls. Empty (the default) means the query is ignored entirely.
	//
	// This exists mainly for BuiltinTelemetryDefaults: query-multiplexed
	// collectors — New Relic POSTs every RPC to one path, keyed only by
	// ?method= — would otherwise collapse all operations to a single mock.
	// Users rarely need to set it by hand; the built-ins carry it for known APIs.
	QueryKeys []string `json:"queryKeys,omitempty" yaml:"queryKeys,omitempty" mapstructure:"queryKeys"`
}

// UnmarshalJSON accepts either the object form ({port,host,path,mode}) or the
// bare-scalar shorthand: a JSON number (a port) or a numeric string ("8126").
// The shorthand defaults Mode to DefaultPassThroughMode. This lets
// `passThroughPorts: [8126, {"port":4317,"mode":"skip"}]` parse element-wise.
func (r *PassThroughRule) UnmarshalJSON(b []byte) error {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return nil
	}
	switch b[0] {
	case '{': // object form — decode into an alias to avoid recursion
		type alias PassThroughRule
		var a alias
		if err := json.Unmarshal(b, &a); err != nil {
			return err
		}
		*r = PassThroughRule(a)
		return nil
	case '"': // string scalar: a numeric port
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		p, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
		if err != nil {
			return err
		}
		*r = PassThroughRule{Port: uint32(p)}
		return nil
	default: // number scalar: a bare port
		var n uint32
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*r = PassThroughRule{Port: n}
		return nil
	}
}

// NormalizeMode returns the rule's mode. An EMPTY mode defaults to
// DefaultPassThroughMode (recordOne) — that is the documented default for a rule
// that omits it. An UNKNOWN/typo'd mode (e.g. "OFF", "Skip", "of") fails CLOSED
// to PassThroughOff — i.e. NOT passthrough, record normally — rather than being
// coerced to a passthrough mode, so a mistyped config never silently drops or
// fakes traffic. Config load should validate and warn on unknown modes; this is
// the hot-path safety net.
func (r PassThroughRule) NormalizeMode() PassThroughMode {
	switch r.Mode {
	case PassThroughSkip, PassThroughRecordOne, PassThroughOff:
		return r.Mode
	case "":
		return DefaultPassThroughMode
	default:
		return PassThroughOff // fail closed: unknown mode ⇒ no passthrough
	}
}

// pathMatches reports whether reqPath satisfies the rule's Path. Empty Path ⇒
// true. PathPrefix ⇒ raw prefix (OTLP/gRPC service path). Otherwise segment
// boundary only, so "/ingest" matches "/ingest" and "/ingest/x" but NOT
// "/ingestData".
func pathMatches(r PassThroughRule, reqPath string) bool {
	if r.Path == "" {
		return true
	}
	if r.PathPrefix {
		return strings.HasPrefix(reqPath, r.Path)
	}
	return reqPath == r.Path || strings.HasPrefix(reqPath, r.Path+"/")
}

// passThroughScore returns a specificity score for a rule against an egress call,
// or (0,false) when it does not match. A rule matches only when EVERY field it
// sets matches (host, port, path, method). Score is additive so Path adds
// specificity: host=2, port=1, path=1 (host+port=3, host+path=3, host=2, …).
func passThroughScore(r PassThroughRule, host string, port uint32, method, reqPath string) (int, bool) {
	if r.Host == "" && r.Port == 0 && r.Path == "" {
		return 0, false // empty rule matches nothing
	}
	if r.Method != "" && method != "" && !strings.EqualFold(r.Method, method) {
		return 0, false
	}
	if !pathMatches(r, reqPath) {
		return 0, false
	}
	score := 0
	if r.Host != "" {
		if !matchHost(r.Host, host) {
			return 0, false
		}
		score += 2
	}
	if r.Port != 0 {
		if r.Port != port {
			return 0, false
		}
		score++
	}
	if r.Path != "" {
		score++
	}
	return score, true
}

// MatchPassThrough returns the best-matching rule within a SINGLE tier (the given
// slice) and true, or a zero rule and false when none match. Most specific wins;
// ties keep the first in slice order. The returned rule's Mode is normalized.
// This does not do user-vs-builtin tiering — use ResolvePassThrough for that.
func MatchPassThrough(rules []PassThroughRule, host string, port uint32, method, reqPath string) (PassThroughRule, bool) {
	host = normalizeHost(host)
	best := PassThroughRule{}
	bestScore := 0
	for _, r := range rules {
		score, ok := passThroughScore(r, host, port, method, reqPath)
		if ok && score > bestScore {
			bestScore = score
			best = r
		}
	}
	if bestScore == 0 {
		return PassThroughRule{}, false
	}
	best.Mode = best.NormalizeMode()
	return best, true
}

// ResolvePassThrough is THE single entry point for the egress passthrough
// decision. It is two-tier: any matching USER rule (userPorts+userHosts) wins
// over every built-in default, and specificity only orders WITHIN a tier — so a
// user rule for a target can never be silently outranked by a more-specific
// built-in. It returns (rule, passthrough): passthrough is false when nothing
// matched OR the winning rule is mode:"off" (record the endpoint normally), and
// true with rule.Mode ∈ {skip, recordOne} otherwise. Callers must branch on the
// bool, not re-derive "does matched mean passthrough" themselves.
func ResolvePassThrough(userPorts, userHosts []PassThroughRule, host string, port uint32, method, reqPath string) (PassThroughRule, bool) {
	// Tier 1: all user rules compete by specificity. Tier 2 (built-ins) is only
	// consulted when NO user rule matched, so a user rule always wins.
	user := make([]PassThroughRule, 0, len(userPorts)+len(userHosts))
	user = append(user, userPorts...)
	user = append(user, userHosts...)
	rule, ok := MatchPassThrough(user, host, port, method, reqPath)
	if !ok {
		rule, ok = MatchPassThrough(BuiltinTelemetryDefaults(), host, port, method, reqPath)
	}
	if !ok || rule.NormalizeMode() == PassThroughOff {
		return PassThroughRule{}, false
	}
	rule.Mode = rule.NormalizeMode()
	return rule, true
}

// normalizeHost lowercases the host and strips a trailing :port (IPv6 literals
// in brackets are left intact). Authorities like "api.datadoghq.com:443" match
// a "api.datadoghq.com" rule.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" || strings.HasSuffix(h, "]") {
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i+1:], "]") {
		// Only strip when the suffix looks like a port (all digits).
		if isAllDigits(h[i+1:]) {
			return h[:i]
		}
	}
	return h
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// matchHost reports whether host matches pattern. Patterns support glob "*"
// (e.g. "*.datadoghq.com"); comparison is case-insensitive. Hostnames contain
// no "/", so path.Match's "*" spans dots as intended.
func matchHost(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	host = normalizeHost(host)
	if pattern == "" || host == "" {
		return false
	}
	if pattern == host {
		return true
	}
	if strings.ContainsAny(pattern, "*?[") {
		if ok, err := path.Match(pattern, host); err == nil && ok {
			return true
		}
	}
	return false
}

// BuiltinTelemetryDefaults returns the built-in "skip" rules for well-known
// telemetry / observability endpoints, matched deterministically by request
// path-prefix (HTTP OTLP + Pyroscope + OTLP/gRPC method) or host (SaaS
// backends). They generalize the old hardcoded {/v1/traces,/ingest} bypass into
// the configurable rule set. Merged UNDER user rules so a user rule for the same
// target wins (see MergePassThroughDefaults). Users disable a default by
// declaring a recordOne rule for the same target.
func BuiltinTelemetryDefaults() []PassThroughRule {
	defs := TelemetryDefaults()
	rules := make([]PassThroughRule, len(defs))
	for i, d := range defs {
		rules[i] = d.Rule
	}
	return rules
}

// TelemetryDefaultsVersion identifies the built-in telemetry defaults SET. Bump
// it whenever TelemetryDefaults() changes (a rule added/removed/retargeted or a
// mode/queryKeys change). It is served to clients (the UI) alongside the rules so
// a UI can prove it is showing the set the running agent actually applies, rather
// than a drifting hardcoded copy. It is NOT the mock-format version (see
// models.GetVersion) and does not affect on-disk compatibility.
const TelemetryDefaultsVersion = 2

// TelemetryDefault is a built-in passthrough rule plus display metadata, so a UI
// can present each default with a human-readable provider/label and let the user
// turn it off (mode:"off") or override its mode. It is the single source of truth
// for the defaults; BuiltinTelemetryDefaults() is derived from it.
type TelemetryDefault struct {
	Provider string          `json:"provider"`
	Label    string          `json:"label"`
	Rule     PassThroughRule `json:"rule"`
}

// TelemetryDefaults returns the built-in "skip"/"recordOne" rules for well-known
// telemetry / observability endpoints, with display metadata. Matched
// deterministically by request path-prefix (HTTP OTLP + Pyroscope + OTLP/gRPC
// method) or host (SaaS backends). Merged UNDER user rules so a user rule for the
// same target wins (see MergePassThroughDefaults); a user rule with mode:"off"
// disables the default entirely (records the endpoint normally).
func TelemetryDefaults() []TelemetryDefault {
	// Every path-based built-in is POST-only and matches on segment boundaries
	// (Method:"POST", PathPrefix left false) so a GET or a same-prefixed app route
	// (e.g. /ingestData, /v1/tracesearch) is never swallowed.
	return []TelemetryDefault{
		// OTLP/HTTP exporters (protobuf/json).
		{"OpenTelemetry", "OTLP/HTTP traces", PassThroughRule{Path: "/v1/traces", Method: "POST", Mode: PassThroughSkip}},
		{"OpenTelemetry", "OTLP/HTTP metrics", PassThroughRule{Path: "/v1/metrics", Method: "POST", Mode: PassThroughSkip}},
		{"OpenTelemetry", "OTLP/HTTP logs", PassThroughRule{Path: "/v1/logs", Method: "POST", Mode: PassThroughSkip}},
		// Pyroscope continuous-profiler ingest.
		{"Pyroscope", "profiler ingest", PassThroughRule{Path: "/ingest", Method: "POST", Mode: PassThroughSkip}},
		// Datadog trace-agent intake.
		{"Datadog", "trace agent v0.4", PassThroughRule{Path: "/v0.4/traces", Method: "POST", Mode: PassThroughSkip}},
		{"Datadog", "trace agent v0.7", PassThroughRule{Path: "/v0.7/traces", Method: "POST", Mode: PassThroughSkip}},
		// Prometheus remote_write: stateless POST, empty 2xx/204 body → skip is
		// safe (the sender ignores the body). Pull-scrape /metrics is ingress and
		// intentionally not here.
		{"Prometheus", "remote_write", PassThroughRule{Path: "/api/v1/write", Method: "POST", Mode: PassThroughSkip}},
		// OTLP/gRPC exporters — the gRPC :path is the OTLP collector service, so
		// this one is a genuine PREFIX (PathPrefix:true) over the h2 POST :path.
		{"OpenTelemetry", "OTLP/gRPC", PassThroughRule{Path: "/opentelemetry.proto.collector.", Method: "POST", PathPrefix: true, Mode: PassThroughSkip}},
		// New Relic RPM: every RPC is POSTed to ONE path, distinguished only by
		// ?method= (preconnect, connect, metric_data, …). It needs recordOne (not
		// skip) because the agent parses the response — connect must return an
		// agent_run_id — and QueryKeys:[method] keeps each operation a distinct
		// mock while ignoring the per-session run_id, so recordOne still collapses
		// repeat calls of the same method. This is the one built-in that is not
		// skip, because a synthetic empty 200 would fail the agent's handshake.
		{"New Relic", "RPM collector", PassThroughRule{Host: "*.newrelic.com", Path: "/agent_listener/invoke_raw_method", Method: "POST",
			Mode: PassThroughRecordOne, QueryKeys: []string{"method"}}},
		// Well-known SaaS telemetry hosts (glob).
		{"Datadog", "SaaS intake", PassThroughRule{Host: "*.datadoghq.com", Mode: PassThroughSkip}},
		{"Sentry", "SaaS intake", PassThroughRule{Host: "*.sentry.io", Mode: PassThroughSkip}},
		{"Azure App Insights", "SaaS intake", PassThroughRule{Host: "dc.services.visualstudio.com", Mode: PassThroughSkip}},
	}
}

// MergePassThroughDefaults returns userPorts+userHosts followed by the built-in
// telemetry defaults as one slice (user rules first). It is retained for callers
// that want the full merged list (e.g. listing/inspection). The DECISION path
// must use ResolvePassThrough instead, which enforces user-beats-builtin tiering
// that a single flat MatchPassThrough over this slice does NOT (a more-specific
// built-in could otherwise outrank a user rule).
func MergePassThroughDefaults(userPorts, userHosts []PassThroughRule) []PassThroughRule {
	out := make([]PassThroughRule, 0, len(userPorts)+len(userHosts)+10)
	out = append(out, userPorts...)
	out = append(out, userHosts...)
	out = append(out, BuiltinTelemetryDefaults()...)
	return out
}
