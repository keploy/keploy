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
)

// DefaultPassThroughMode is applied to a rule whose Mode is empty and to the
// bare-scalar shorthand form.
const DefaultPassThroughMode = PassThroughRecordOne

// PassThroughRule marks telemetry / noisy egress that keploy should not capture
// normally. A rule matches an egress call when its Port equals the destination
// port AND/OR its Host matches the destination host/authority (and, optionally,
// its Path is a prefix of the request path). Body and query are never part of
// the match — that is the point of passthrough.
//
// Wire forms accepted per element (scalar shorthand is wired in T10):
//   - object:  {port: 8126, mode: skip}  or  {host: "*.datadoghq.com", mode: recordOne}
//   - scalar:  8126  (a bare port; mode defaults to DefaultPassThroughMode)
type PassThroughRule struct {
	Port uint32          `json:"port,omitempty" yaml:"port,omitempty" mapstructure:"port"`
	Host string          `json:"host,omitempty" yaml:"host,omitempty" mapstructure:"host"`
	Path string          `json:"path,omitempty" yaml:"path,omitempty" mapstructure:"path"`
	Mode PassThroughMode `json:"mode,omitempty" yaml:"mode,omitempty" mapstructure:"mode"`
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

// NormalizeMode returns the rule's mode, defaulting an empty/unknown value to
// DefaultPassThroughMode. Callers should treat the returned value as canonical.
func (r PassThroughRule) NormalizeMode() PassThroughMode {
	switch r.Mode {
	case PassThroughSkip, PassThroughRecordOne:
		return r.Mode
	default:
		return DefaultPassThroughMode
	}
}

// passThroughScore returns a specificity score for a rule against an egress
// call, or (0,false) when it does not match. Higher = more specific:
//   3 host+port, 2 host, 1 port. An optional Path must prefix-match when set.
func passThroughScore(r PassThroughRule, host string, port uint32, reqPath string) (int, bool) {
	if r.Path != "" && !strings.HasPrefix(reqPath, r.Path) {
		return 0, false
	}
	hostOK := r.Host != "" && matchHost(r.Host, host)
	portOK := r.Port != 0 && r.Port == port
	switch {
	case r.Host != "" && r.Port != 0:
		if hostOK && portOK {
			return 3, true
		}
		return 0, false
	case r.Host != "":
		if hostOK {
			return 2, true
		}
		return 0, false
	case r.Port != 0:
		if portOK {
			return 1, true
		}
		return 0, false
	default:
		// A rule with only a Path (no host/port) matches on path alone.
		if r.Path != "" {
			return 1, true
		}
		return 0, false
	}
}

// MatchPassThrough returns the winning rule for an egress call and true, or a
// zero rule and false when none match. When several rules match, the most
// specific wins (host+port > host > port); ties keep the first in slice order.
// The returned rule's Mode is normalized.
func MatchPassThrough(rules []PassThroughRule, host string, port uint32, reqPath string) (PassThroughRule, bool) {
	host = normalizeHost(host)
	best := PassThroughRule{}
	bestScore := 0
	for _, r := range rules {
		score, ok := passThroughScore(r, host, port, reqPath)
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
	return []PassThroughRule{
		// OTLP/HTTP exporters (protobuf/json), path-prefixed.
		{Path: "/v1/traces", Mode: PassThroughSkip},
		{Path: "/v1/metrics", Mode: PassThroughSkip},
		{Path: "/v1/logs", Mode: PassThroughSkip},
		// Pyroscope continuous-profiler ingest.
		{Path: "/ingest", Mode: PassThroughSkip},
		// Datadog trace-agent intake.
		{Path: "/v0.4/traces", Mode: PassThroughSkip},
		{Path: "/v0.7/traces", Mode: PassThroughSkip},
		// Prometheus remote_write: stateless POST, empty 2xx/204 body → skip is
		// safe (the sender ignores the body). Pull-scrape /metrics is ingress and
		// intentionally not here.
		{Path: "/api/v1/write", Mode: PassThroughSkip},
		// OTLP/gRPC exporters — the gRPC :path is the OTLP collector service.
		{Path: "/opentelemetry.proto.collector.", Mode: PassThroughSkip},
		// New Relic RPM: every RPC is POSTed to ONE path, distinguished only by
		// ?method= (preconnect, connect, metric_data, …). It needs recordOne (not
		// skip) because the agent parses the response — connect must return an
		// agent_run_id — and QueryKeys:[method] keeps each operation a distinct
		// mock while ignoring the per-session run_id, so recordOne still collapses
		// repeat calls of the same method. This is the one built-in that is not
		// skip, because a synthetic empty 200 would fail the agent's handshake.
		{Host: "*.newrelic.com", Path: "/agent_listener/invoke_raw_method",
			Mode: PassThroughRecordOne, QueryKeys: []string{"method"}},
		// Well-known SaaS telemetry hosts (glob).
		{Host: "*.datadoghq.com", Mode: PassThroughSkip},
		{Host: "*.sentry.io", Mode: PassThroughSkip},
		{Host: "dc.services.visualstudio.com", Mode: PassThroughSkip},
	}
}

// MergePassThroughDefaults returns userPorts+userHosts followed by the built-in
// telemetry defaults, so callers can pass a single rule slice to MatchPassThrough.
// Precedence is by specificity in MatchPassThrough (host+port > host > port),
// with slice order breaking ties — user rules come first, so an equally-specific
// user rule wins over a default.
func MergePassThroughDefaults(userPorts, userHosts []PassThroughRule) []PassThroughRule {
	out := make([]PassThroughRule, 0, len(userPorts)+len(userHosts)+10)
	out = append(out, userPorts...)
	out = append(out, userHosts...)
	out = append(out, BuiltinTelemetryDefaults()...)
	return out
}
