package secrets

import (
	"strings"
	"sync"

	ossModels "go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// SecretEngine is the interface for both encryption and obfuscation engines.
type SecretEngine interface {
	Process(plaintext string) (string, error)
}

// FieldTracker records which fields were processed (for noise injection).
type FieldTracker struct {
	Headers   map[string]struct{}
	BodyPaths map[string]struct{}
	URLParams map[string]struct{}
}

func NewFieldTracker() *FieldTracker {
	return &FieldTracker{
		Headers:   make(map[string]struct{}),
		BodyPaths: make(map[string]struct{}),
		URLParams: make(map[string]struct{}),
	}
}

func (t *FieldTracker) AddHeader(key string)   { t.Headers[key] = struct{}{} }
func (t *FieldTracker) AddBody(path string)    { t.BodyPaths[path] = struct{}{} }
func (t *FieldTracker) AddURLParam(key string) { t.URLParams[key] = struct{}{} }

// DetectionEntry records a single detected secret for the audit trail.
type DetectionEntry struct {
	Field  string `json:"field" yaml:"field"`   // e.g. "header.Authorization"
	Reason string `json:"reason" yaml:"reason"` // e.g. "name_match", "value_pattern:JWT"
	Action string `json:"action" yaml:"action"` // "encrypted", "obfuscated", "skipped-allowlist"
}

// DetectionReport is a session-wide audit trail of all detected secrets.
type DetectionReport struct {
	mu      sync.Mutex
	Entries []DetectionEntry `json:"entries" yaml:"entries"`
}

func (r *DetectionReport) Add(field, reason, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Entries = append(r.Entries, DetectionEntry{Field: field, Reason: reason, Action: action})
}

// Processor orchestrates secret detection and protection for test cases and mocks.
type Processor struct {
	detector *Detector
	engine   SecretEngine
	mode     string // "encrypt" or "obfuscate"
	logger   *zap.Logger

	// Only set in encrypt mode — holds the metadata for the current session.
	encMeta *EncryptionMetadata

	// Audit trail: all detections across the session (Fix #7).
	Report *DetectionReport
}

// NewProcessor creates a Processor based on the given mode.
// For encrypt mode, pass an initialized EncryptionEngine.
// For obfuscate mode, pass nil — an ObfuscationEngine is used.
func NewProcessor(mode string, detector *Detector, encEngine *EncryptionEngine, logger *zap.Logger) *Processor {
	p := &Processor{
		detector: detector,
		mode:     mode,
		logger:   logger,
		Report:   &DetectionReport{},
	}

	if mode == "encrypt" && encEngine != nil {
		p.engine = encEngine
		p.encMeta = encEngine.Metadata()
	} else {
		obfEng, err := NewObfuscationEngine()
		if err != nil {
			if logger != nil {
				logger.Error("failed to create obfuscation engine — secret protection disabled for this session. This typically indicates an OS entropy issue.", zap.Error(err))
			}
			return nil
		}
		p.engine = obfEng
		p.mode = "obfuscate"
	}

	return p
}

// Mode returns the active protection mode.
func (p *Processor) Mode() string { return p.mode }

// EncryptionMeta returns the encryption metadata for the session (nil in obfuscate mode).
func (p *Processor) EncryptionMeta() *EncryptionMetadata { return p.encMeta }

// ProcessTestCase detects and protects secrets in a test case in-place.
// In both modes, response-side secret fields are added to noise (Fix #4)
// because response secrets are often dynamic (e.g. new session tokens per call).
// In obfuscate mode, ALL processed fields are added to noise.
func (p *Processor) ProcessTestCase(tc *ossModels.TestCase) {
	if p == nil || tc == nil {
		return
	}

	reqTracker := NewFieldTracker()
	respTracker := NewFieldTracker()

	// --- HTTP Request ---
	p.processHeaders(tc.HTTPReq.Header, reqTracker)
	tc.HTTPReq.URL = ProcessURL(tc.HTTPReq.URL, p.detector, p.engine, reqTracker)
	tc.HTTPReq.URLParams = ProcessURLParams(tc.HTTPReq.URLParams, p.detector, p.engine, reqTracker)
	tc.HTTPReq.Body = p.processBody(tc.HTTPReq.Body, reqTracker)
	tc.HTTPReq.Form = ProcessFormData(tc.HTTPReq.Form, p.detector, p.engine, reqTracker)

	// --- HTTP Response ---
	p.processHeaders(tc.HTTPResp.Header, respTracker)
	tc.HTTPResp.Body = p.processBody(tc.HTTPResp.Body, respTracker)

	// --- gRPC Request (Fix #10: process body, not just headers) ---
	p.processGRPCHeaders(tc.GrpcReq.Headers, reqTracker)
	if tc.GrpcReq.Body.DecodedData != "" {
		tc.GrpcReq.Body.DecodedData = p.processBody(tc.GrpcReq.Body.DecodedData, reqTracker)
	}

	// --- gRPC Response (Fix #10) ---
	p.processGRPCHeaders(tc.GrpcResp.Headers, respTracker)
	if tc.GrpcResp.Body.DecodedData != "" {
		tc.GrpcResp.Body.DecodedData = p.processBody(tc.GrpcResp.Body.DecodedData, respTracker)
	}

	// Record all detections in the audit trail (headers are recorded in processHeaders;
	// body/URL/form detections are recorded here from tracker data).
	action := "encrypted"
	if p.mode == "obfuscate" {
		action = "obfuscated"
	}
	for path := range reqTracker.BodyPaths {
		p.Report.Add("body."+path, "auto", action)
	}
	for path := range respTracker.BodyPaths {
		p.Report.Add("resp_body."+path, "auto", action)
	}
	for param := range reqTracker.URLParams {
		p.Report.Add("url_param."+param, "auto", action)
	}

	// Noise injection:
	// - Obfuscate mode: ALL fields (request + response) → noise (so replay skips them).
	// - Encrypt mode: RESPONSE fields → noise (Fix #4: response secrets are dynamic,
	//   e.g. server returns new tokens each call, so even with decrypt they won't match).
	//   Request fields are NOT noised in encrypt mode because decrypt restores exact values.
	if p.mode == "obfuscate" {
		injectNoise(tc, reqTracker)
		injectNoise(tc, respTracker)
	} else {
		// Encrypt mode: only response-side noise.
		injectNoise(tc, respTracker)
	}
}

// ProcessMock detects and protects secrets in a mock in-place.
func (p *Processor) ProcessMock(mock *ossModels.Mock) {
	if p == nil || mock == nil {
		return
	}

	tracker := NewFieldTracker()

	if mock.Kind == ossModels.HTTP {
		if mock.Spec.HTTPReq != nil {
			p.processHeaders(mock.Spec.HTTPReq.Header, tracker)
			mock.Spec.HTTPReq.URL = ProcessURL(mock.Spec.HTTPReq.URL, p.detector, p.engine, tracker)
			mock.Spec.HTTPReq.URLParams = ProcessURLParams(mock.Spec.HTTPReq.URLParams, p.detector, p.engine, tracker)
			mock.Spec.HTTPReq.Body = p.processBody(mock.Spec.HTTPReq.Body, tracker)
			mock.Spec.HTTPReq.Form = ProcessFormData(mock.Spec.HTTPReq.Form, p.detector, p.engine, tracker)
		}
		if mock.Spec.HTTPResp != nil {
			p.processHeaders(mock.Spec.HTTPResp.Header, tracker)
			mock.Spec.HTTPResp.Body = p.processBody(mock.Spec.HTTPResp.Body, tracker)
		}
	} else {
		ProcessNonHTTPMock(&mock.Spec, mock.Kind, p.detector, p.engine, tracker, p.logger)
	}

	// Record mock detections in session-wide audit trail.
	action := "encrypted"
	if p.mode == "obfuscate" {
		action = "obfuscated"
	}
	for path := range tracker.BodyPaths {
		p.Report.Add("mock.body."+path, "auto", action)
	}
	for param := range tracker.URLParams {
		p.Report.Add("mock.url_param."+param, "auto", action)
	}
}

// processHeaders detects and replaces secrets in an HTTP header map.
func (p *Processor) processHeaders(headers map[string]string, tracker *FieldTracker) {
	results := p.detector.DetectInHeaders(headers)
	for _, r := range results {
		if r.Value == "" {
			continue // Skip empty values — encrypting "" produces a sentinel that changes semantics
		}
		processed, err := p.engine.Process(r.Value)
		if err != nil {
			// Fail-closed: redact the value rather than leaving the secret in place.
			processed = "[KEPLOY_REDACTED:process_error]"
			p.Report.Add(r.Field, r.Reason, "redacted")
		} else {
			action := "encrypted"
			if p.mode == "obfuscate" {
				action = "obfuscated"
			}
			p.Report.Add(r.Field, r.Reason, action)
		}
		key := strings.TrimPrefix(r.Field, "header.")
		headers[key] = processed
		tracker.AddHeader(key)
	}
}

// processGRPCHeaders processes gRPC metadata using the same header detection
// and reporting logic as HTTP headers, ensuring consistent allowlist support.
func (p *Processor) processGRPCHeaders(headers ossModels.GrpcHeaders, tracker *FieldTracker) {
	p.processHeaders(headers.OrdinaryHeaders, tracker)
	p.processHeaders(headers.PseudoHeaders, tracker)
}

// processBody routes to the appropriate body processor based on content.
func (p *Processor) processBody(body string, tracker *FieldTracker) string {
	if body == "" {
		return body
	}
	trimmed := strings.TrimSpace(body)
	if len(trimmed) == 0 {
		return body
	}

	switch trimmed[0] {
	case '{', '[':
		return ProcessJSONBody(body, p.detector, p.engine, tracker)
	case '<':
		return ProcessXMLBody(body, p.detector, p.engine, tracker)
	default:
		return body
	}
}

// injectNoise adds processed field paths into the test case's Noise map
// so the replay comparator skips them.
func injectNoise(tc *ossModels.TestCase, tracker *FieldTracker) {
	if tc.Noise == nil {
		tc.Noise = make(map[string][]string)
	}
	for hdr := range tracker.Headers {
		tc.Noise["header"] = appendUnique(tc.Noise["header"], hdr)
	}
	for path := range tracker.BodyPaths {
		tc.Noise["body"] = appendUnique(tc.Noise["body"], path)
	}
	for param := range tracker.URLParams {
		tc.Noise["body"] = appendUnique(tc.Noise["body"], "url_param."+param)
	}
}

func appendUnique(slice []string, val string) []string {
	for _, existing := range slice {
		if existing == val {
			return slice
		}
	}
	return append(slice, val)
}
