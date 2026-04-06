package secrets

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// DetectionResult describes a detected secret.
type DetectionResult struct {
	Field  string // e.g. "header.Authorization", "body.nested.token", "url_param.api_key"
	Value  string // the original value
	Reason string // "name_match", "value_pattern:JWT", "config_rule", "ui_rule"
}

// compiledPattern is a lazy-compiled regex pattern.
type compiledPattern struct {
	Name    string
	Raw     string
	Regex   *regexp.Regexp
	OnlyCtx bool // only match when field name also looks secret-ish
}

// --- Built-in value-pattern regexes ---

var builtinValuePatterns = []struct {
	Name    string
	Pattern string
	OnlyCtx bool // require contextual field name hint
}{
	{"JWT", `eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+`, false},
	{"AWS Access Key", `AKIA[0-9A-Z]{16}`, false},
	{"GitHub PAT", `gh[ps]_[A-Za-z0-9_]{36,}`, false},
	{"GitHub Fine-Grained PAT", `github_pat_[A-Za-z0-9_]{22,}`, false},
	{"Stripe Key", `[sr]k_(test|live)_[0-9a-zA-Z]{24,}`, false},
	{"Slack Token", `xox[bporas]-[0-9A-Za-z\-]{10,}`, false},
	{"Google API Key", `AIza[0-9A-Za-z\-_]{35}`, false},
	{"Private Key Block", `-----BEGIN\s+(RSA\s+|EC\s+|DSA\s+|OPENSSH\s+)?PRIVATE\s+KEY-----`, false},
	{"Bearer Token", `[Bb]earer\s+[A-Za-z0-9\-._~+/]+=*`, false},
	{"Basic Auth", `[Bb]asic\s+[A-Za-z0-9+/=]{10,}`, false},
	// Context-only: only flag when field name also suggests a secret.
	{"Hex Secret 32+", `[0-9a-fA-F]{32,}`, true},
}

// --- Built-in sensitive field names (case-insensitive) ---

var defaultSensitiveHeaders = map[string]struct{}{
	"authorization":            {},
	"proxy-authorization":      {},
	"cookie":                   {},
	"set-cookie":               {},
	"x-api-key":                {},
	"x-auth-token":             {},
	"x-csrf-token":             {},
	"x-xsrf-token":             {},
	"x-access-token":           {},
	"x-secret":                 {},
	"x-session-id":             {},
	"x-forwarded-access-token": {},
	"www-authenticate":         {},
}

var defaultSensitiveBodyKeys = map[string]struct{}{
	"password": {}, "passwd": {}, "secret": {}, "token": {},
	"access_token": {}, "refresh_token": {}, "api_key": {}, "apikey": {},
	"api_secret": {}, "apisecret": {}, "private_key": {}, "client_secret": {},
	"auth": {}, "credentials": {}, "session_id": {}, "sessionid": {},
	"ssn": {}, "credit_card": {}, "card_number": {}, "cvv": {}, "pin": {},
	"secret_key": {}, "signing_key": {}, "encryption_key": {},
	"auth_token": {}, "bearer": {},
}

var defaultSensitiveURLParams = map[string]struct{}{
	"api_key": {}, "apikey": {}, "token": {}, "access_token": {},
	"secret": {}, "key": {}, "auth": {}, "password": {}, "passwd": {},
	"refresh_token": {}, "client_secret": {},
}

// contextHintKeys are field name substrings that suggest the value might be secret
// (used by OnlyCtx patterns to reduce false positives on long hex strings etc.)
var contextHintKeys = []string{
	"secret", "token", "key", "password", "passwd", "auth", "credential",
	"api_key", "apikey", "private",
}

// Detector combines name-based and value-based secret detection.
type Detector struct {
	nameHeaders   map[string]struct{}
	nameBody      map[string]struct{}
	nameURLParams map[string]struct{}
	allowlist     map[string]struct{} // "header.X-Foo", "body.field", "url_param.key"

	customPatterns  []CustomRegex // from config file
	valuePatterns   []compiledPattern
	compileOnce     sync.Once
	InvalidPatterns []string // patterns that failed to compile (for diagnostics)
}

// NewDetector builds a Detector from the protection config and optional config file rules.
func NewDetector(headers, bodyKeys, urlParams, allowlist []string, fileRules *ConfigFileRules) *Detector {
	d := &Detector{
		nameHeaders:   mergeLookup(headers, defaultSensitiveHeaders),
		nameBody:      mergeLookup(bodyKeys, defaultSensitiveBodyKeys),
		nameURLParams: mergeLookup(urlParams, defaultSensitiveURLParams),
		allowlist:     make(map[string]struct{}),
	}

	// Merge allowlist from all sources.
	for _, a := range allowlist {
		d.allowlist[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}

	// Merge config file rules.
	if fileRules != nil {
		for _, h := range fileRules.CustomHeaders {
			d.nameHeaders[strings.ToLower(strings.TrimSpace(h))] = struct{}{}
		}
		for _, b := range fileRules.CustomBodyKeys {
			d.nameBody[strings.ToLower(strings.TrimSpace(b))] = struct{}{}
		}
		for _, u := range fileRules.CustomURLParams {
			d.nameURLParams[strings.ToLower(strings.TrimSpace(u))] = struct{}{}
		}
		for _, a := range fileRules.Allowlist {
			d.allowlist[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
		}
		d.customPatterns = fileRules.CustomPatterns
	}

	return d
}

func (d *Detector) ensureCompiled() {
	d.compileOnce.Do(func() {
		d.valuePatterns = make([]compiledPattern, 0, len(builtinValuePatterns)+len(d.customPatterns))
		for _, p := range builtinValuePatterns {
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				continue
			}
			d.valuePatterns = append(d.valuePatterns, compiledPattern{
				Name:    p.Name,
				Raw:     p.Pattern,
				Regex:   re,
				OnlyCtx: p.OnlyCtx,
			})
		}
		// Compile custom patterns from config file.
		for _, cp := range d.customPatterns {
			re, err := regexp.Compile(cp.Pattern)
			if err != nil {
				d.InvalidPatterns = append(d.InvalidPatterns, fmt.Sprintf("%s: %s (%v)", cp.Name, cp.Pattern, err))
				continue
			}
			d.valuePatterns = append(d.valuePatterns, compiledPattern{
				Name:  cp.Name,
				Raw:   cp.Pattern,
				Regex: re,
			})
		}
	})
}

// IsAllowed checks whether a given field path is in the allowlist.
func (d *Detector) IsAllowed(fieldPath string) bool {
	_, ok := d.allowlist[strings.ToLower(fieldPath)]
	return ok
}

// DetectInHeaders scans HTTP headers for secrets.
func (d *Detector) DetectInHeaders(headers map[string]string) []DetectionResult {
	d.ensureCompiled()
	var results []DetectionResult
	for key, val := range headers {
		fieldPath := "header." + key
		if d.IsAllowed(fieldPath) {
			continue
		}
		if _, ok := d.nameHeaders[strings.ToLower(key)]; ok {
			results = append(results, DetectionResult{Field: fieldPath, Value: val, Reason: "name_match"})
			continue
		}
		if reason := d.scanValue(key, val); reason != "" {
			results = append(results, DetectionResult{Field: fieldPath, Value: val, Reason: reason})
		}
	}
	return results
}

// DetectInURLParams scans URL query parameters for secrets.
func (d *Detector) DetectInURLParams(params map[string]string) []DetectionResult {
	d.ensureCompiled()
	var results []DetectionResult
	for key, val := range params {
		fieldPath := "url_param." + key
		if d.IsAllowed(fieldPath) {
			continue
		}
		if _, ok := d.nameURLParams[strings.ToLower(key)]; ok {
			results = append(results, DetectionResult{Field: fieldPath, Value: val, Reason: "name_match"})
			continue
		}
		if reason := d.scanValue(key, val); reason != "" {
			results = append(results, DetectionResult{Field: fieldPath, Value: val, Reason: reason})
		}
	}
	return results
}

// IsBodyKeySensitive checks whether a JSON field name matches the sensitive body key set.
func (d *Detector) IsBodyKeySensitive(fieldName string) bool {
	_, ok := d.nameBody[strings.ToLower(fieldName)]
	return ok
}

// ScanValue checks a value against regex patterns. Returns the pattern name or "".
func (d *Detector) ScanValue(fieldName, value string) string {
	d.ensureCompiled()
	return d.scanValue(fieldName, value)
}

func (d *Detector) scanValue(fieldName, value string) string {
	if len(value) < 8 {
		return "" // too short to be a meaningful secret
	}
	hasCtx := fieldNameLooksSecret(fieldName)
	for _, p := range d.valuePatterns {
		if p.OnlyCtx && !hasCtx {
			continue
		}
		if p.Regex.MatchString(value) {
			return "value_pattern:" + p.Name
		}
	}
	return ""
}

func fieldNameLooksSecret(name string) bool {
	lower := strings.ToLower(name)
	for _, hint := range contextHintKeys {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func mergeLookup(custom []string, defaults map[string]struct{}) map[string]struct{} {
	if len(custom) > 0 {
		m := make(map[string]struct{}, len(defaults)+len(custom))
		// Start with defaults, then add custom.
		for k := range defaults {
			m[k] = struct{}{}
		}
		for _, k := range custom {
			m[strings.ToLower(strings.TrimSpace(k))] = struct{}{}
		}
		return m
	}
	// Clone defaults.
	m := make(map[string]struct{}, len(defaults))
	for k := range defaults {
		m[k] = struct{}{}
	}
	return m
}
