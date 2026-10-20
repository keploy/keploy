// Package pkg provides utility functions for Keploy.
package pkg

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"

	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

var SortCounter int64 = -1

const maxStreamTokenSize = 10 * 1024 * 1024

// HTTPStreamMode represents the type of HTTP streaming response.
type HTTPStreamMode string

const (
	HTTPStreamModeNone      HTTPStreamMode = "none"
	HTTPStreamModeSSE       HTTPStreamMode = "sse"
	HTTPStreamModeNDJSON    HTTPStreamMode = "ndjson"
	HTTPStreamModeMultipart HTTPStreamMode = "multipart"
	HTTPStreamModePlainText HTTPStreamMode = "plain-text"
	HTTPStreamModeBinary    HTTPStreamMode = "binary"
)

// HTTPStreamConfig holds configuration for HTTP streaming responses.
type HTTPStreamConfig struct {
	Mode     HTTPStreamMode
	Boundary string
	// Target names the stream's request ("GET /events"), for what a stream
	// that ended with nothing of its answer to compare says
	// (AnswerStallError). Set only under the proven switch.
	Target string
}

// StreamMismatchInfo holds details about a streaming frame mismatch for reporting.
type StreamMismatchInfo struct {
	FrameIndex    int    // Index of the mismatched frame (-1 if no specific frame)
	ExpectedFrame string // The expected frame content
	ActualFrame   string // The actual frame content received
	Reason        string // Description of why frames don't match
}

// StreamingHTTPResponse holds the response metadata and a reader for streaming responses.
// The caller is responsible for reading from the Reader and closing it when done.
type StreamingHTTPResponse struct {
	StatusCode    int
	StatusMessage string
	Header        map[string]string
	Reader        io.ReadCloser
	StreamConfig  HTTPStreamConfig
}

func InitSortCounter(counter int64) {
	atomic.StoreInt64(&SortCounter, counter)
}

func GetNextSortNum() int64 {
	return atomic.AddInt64(&SortCounter, 1)
}

func UpdateSortCounterIfHigher(val int64) {
	for {
		curr := atomic.LoadInt64(&SortCounter)
		if val <= curr {
			return
		}
		if atomic.CompareAndSwapInt64(&SortCounter, curr, val) {
			return
		}
	}
}

// URLParams returns the Url and Query parameters from the request url.
func URLParams(r *http.Request) map[string]string {
	qp := r.URL.Query()
	result := make(map[string]string)

	for key, values := range qp {
		result[key] = strings.Join(values, ", ")
	}

	return result
}

// ToYamlHTTPHeader converts the http header into yaml format
func ToYamlHTTPHeader(httpHeader http.Header) map[string]string {
	header := map[string]string{}
	for i, j := range httpHeader {
		header[i] = strings.Join(j, ",")
	}
	return header
}

// ToYamlHTTPHeaderLineLengths is the other half of ToYamlHTTPHeader for a
// header keploy will send back out: for each name that arrived on more than one
// line, the length of each line's value, in arrival order. ToYamlHTTPHeader's
// join cannot be undone without them (see models.HeaderLineLengths). nil when
// every name arrived on one line, so the recording is stored as it always was.
func ToYamlHTTPHeaderLineLengths(httpHeader http.Header) models.HeaderLineLengths {
	var lineLengths models.HeaderLineLengths
	for name, values := range httpHeader {
		if len(values) < 2 {
			continue
		}
		lengths := make([]int, len(values))
		for i, value := range values {
			lengths[i] = len(value)
		}
		if lineLengths == nil {
			lineLengths = models.HeaderLineLengths{}
		}
		lineLengths[name] = lengths
	}
	return lineLengths
}

// ToWireHTTPHeader is ToHTTPHeader for a recorded header keploy sends back out:
// a test case's request, a mock's response. A value recorded from several lines
// goes out on those lines again, in their order, as long as it is still exactly
// those lines joined by ","; anything else goes out as ToHTTPHeader sends it,
// one line per name.
func ToWireHTTPHeader(header map[string]string, lineLengths models.HeaderLineLengths) http.Header {
	wire := ToHTTPHeader(header)
	for name, lengths := range lineLengths {
		value, ok := header[name]
		if !ok {
			continue
		}
		if lines, ok := splitHeaderLines(value, lengths); ok {
			wire[name] = lines
		}
	}
	return wire
}

// splitHeaderLines cuts value back into the lines ToYamlHTTPHeader joined, or
// reports false when value is not len(lengths) lines of those lengths joined by
// ",": a value rewritten since it was recorded (a template rendered, a secret
// decrypted to something else, an edit) is not split by a guess.
func splitHeaderLines(value string, lengths []int) ([]string, bool) {
	if len(lengths) < 2 {
		return nil, false
	}
	lines := make([]string, 0, len(lengths))
	pos := 0
	for i, n := range lengths {
		if n < 0 || n > len(value)-pos {
			return nil, false
		}
		lines = append(lines, value[pos:pos+n])
		pos += n
		if i == len(lengths)-1 {
			break
		}
		if pos >= len(value) || value[pos] != ',' {
			return nil, false
		}
		pos++
	}
	if pos != len(value) {
		return nil, false
	}
	return lines, true
}

// CompareMultiValueHeaders compares a mock header value (as a comma-separated string)
// with an input header value (as a slice of strings). It normalizes whitespace,
// splits the mock header value by commas, trims spaces, sorts both sets of values,
// and returns true if they contain the same elements in any order.
func CompareMultiValueHeaders(mockHeaderValue string, inputHeaderValue []string) bool {
	// early returns
	if mockHeaderValue == "" && len(inputHeaderValue) == 0 {
		return true
	}

	if mockHeaderValue == "" || len(inputHeaderValue) == 0 {
		return false
	}

	mockValues := strings.Split(mockHeaderValue, ",")
	normalizedMockValues := make([]string, len(mockValues))
	for i, v := range mockValues {
		normalizedMockValues[i] = strings.TrimSpace(v)
	}

	// Normalize input header values
	normalizedInputValues := make([]string, len(inputHeaderValue))
	for i, v := range inputHeaderValue {
		normalizedInputValues[i] = strings.TrimSpace(v)
	}

	// Sort both slices for comparison
	sort.Strings(normalizedMockValues)
	sort.Strings(normalizedInputValues)

	// Compare lengths first
	if len(normalizedMockValues) != len(normalizedInputValues) {
		return false
	}

	// Compare each value
	for i, mockVal := range normalizedMockValues {
		if mockVal != normalizedInputValues[i] {
			return false
		}
	}

	return true
}

func ToHTTPHeader(mockHeader map[string]string) http.Header {
	header := http.Header{}
	for i, j := range mockHeader {
		// Emit one wire header per YAML key. The recorded value is kept
		// intact even when it contains commas — this is the canonical
		// comma-folded form for RFC 7230 list-valued headers (Accept,
		// Accept-Encoding, Baggage, Forwarded, Via, ...) and is what
		// most HTTP clients (axios, requests, fetch) actually emit on
		// the wire. Splitting on "," and sending one wire header per
		// element fans out into multiple same-name headers; that breaks
		// strict-mapping server middlewares such as
		// `Response(headers=dict(**request.headers), ...)` which raise
		// `TypeError: dict() got multiple values for keyword argument
		// 'accept'` on duplicate field names — a known FastAPI /
		// Starlette failure mode reproducible during replay even when
		// the original client sent exactly one Accept header.
		//
		// A value that really did arrive on several lines is folded
		// here too. That is right for comparing and matching, and wrong
		// for sending: an app reading the first of `X-A: a` twice gets
		// "a", and of `X-A: a,a` gets "a,a". Code that sends a
		// recorded header uses ToWireHTTPHeader, which puts the lines
		// back.
		header[i] = []string{j}
	}
	return header
}

// looksLikeTime is a fast-path byte-prefix check that rejects strings which
// cannot possibly match any of the date/time formats we try below. Avoiding
// the time.Parse loop on the 99%+ of strings that obviously aren't times
// saves an enormous amount of allocations (each failed time.Parse allocates
// a *time.parseError + a copied error message).
//
// We accept a string as a candidate iff its first non-whitespace byte is:
//   - a digit (most ISO/RFC formats start with year or day)
//   - one of Mon/Tue/Wed/Thu/Fri/Sat/Sun (RFC1123, RFC850 weekday prefixes)
//   - one of Jan/Feb/Mar/Apr/May/Jun/Jul/Aug/Sep/Oct/Nov/Dec (ANSIC,
//     UnixDate, RubyDate, time.Stamp* prefixes)
//   - one of '0'..'9' for the time.Kitchen "3:04PM" format (digit handled above)
//
// Anything else (random hex blobs, JSON keys, "true"/"false"/"null",
// payload values) returns false immediately, skipping 20 failed time.Parse
// calls — each of which would allocate ~120 bytes.
func looksLikeTime(s string) bool {
	// strip leading whitespace cheaply
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i >= len(s) {
		return false
	}
	c := s[i]
	if c >= '0' && c <= '9' {
		return true
	}
	// Check 3-letter prefix against weekday/month abbreviations.
	if len(s)-i < 3 {
		return false
	}
	switch s[i : i+3] {
	case "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun",
		"Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec":
		return true
	}
	return false
}

// IsTime verifies whether a given string represents a valid date or not.
//
// Hot-path optimisation: a fast-path looksLikeTime check rules out 99%+ of
// non-time strings before the 20-format time.Parse loop. Without this, every
// non-time string allocates ~20 *time.parseError objects (~2.4 KB) — when
// called millions of times during a load test, that's GBs of GC pressure.
func IsTime(stringDate string) bool {
	date := strings.TrimSpace(stringDate)
	if date == "" {
		return false
	}
	if secondsFloat, err := strconv.ParseFloat(date, 64); err == nil {
		seconds := int64(secondsFloat / 1e9)
		nanoseconds := int64(secondsFloat) % 1e9
		expectedTime := time.Unix(seconds, nanoseconds)
		currentTime := time.Now()
		if currentTime.Sub(expectedTime) < 24*time.Hour && currentTime.Sub(expectedTime) > -24*time.Hour {
			return true
		}
	}
	// Cheap byte-prefix gate to avoid the 20-format time.Parse loop on
	// obvious non-time inputs.
	if !looksLikeTime(date) {
		return false
	}
	for _, dateFormat := range dateFormats {
		_, err := time.Parse(dateFormat, date)
		if err == nil {
			return true
		}
	}
	return false
}

type SimulationConfig struct {
	APITimeout uint64
	// Unbounded: no clock bounds the app's answer, and APITimeout is not
	// applied. The replay sets it under the proven switch when the user set
	// no apiTimeout of their own (config.Test.APITimeoutSet, rule 15).
	Unbounded       bool
	RequestTimeout  time.Duration
	ConfigPort      uint32
	KeployPath      string
	ConfigHost      string
	URLReplacements map[string]string
	PortMappings    map[uint32]uint32
	// TargetFromURL says an HTTP test case's URL already names where the app
	// is, as it does once test.basePath has been put in it. What the recording
	// kept of the address the app had then is another deployment's, and is
	// left out of the request: the recorded app_port does not replace the
	// URL's port, and the recorded Host header is not sent, so the request
	// asks for the URL's host (see HostHeaderOf). An app behind an ingress or
	// a virtual host is not reached under the name it was recorded with. (A
	// CONNECT in authority form still asks for the target it was recorded
	// with: its Host names that, not the app; ConnectsToAuthority.)
	TargetFromURL bool
	// SentTo, when set, is told where each HTTP request goes once everything
	// that moves it (replaceWith, ports, TargetFromURL) has been applied: the
	// URL it is sent to, the Host it asks for, and the Host header its test
	// was recorded with ("" when it has none).
	SentTo func(target *url.URL, host, recordedHost string)
	// TLSConfig, when non-nil, is applied to the http.Transport used by
	// the replay client. Lets callers pin a specific cert (e.g. for
	// short-lived replay pods serving a self-signed cert) without
	// relaxing TLS verification globally. Default nil preserves the
	// stdlib system-pool behaviour.
	TLSConfig *tls.Config
	// AppPortReachability, when non-nil, is asked about a test request that
	// was refused, reset or dropped before an answer: an address the app can
	// never be reached at fails the test at once, saying why, instead of
	// being re-sent as an app still starting.
	AppPortReachability AppPortReachability
	// PublishedPort, when non-nil, is the host port a test for the app's port
	// appPort goes to from host in place of appPort, or 0 to send it to
	// appPort (PublishedTarget): in Docker mode, the one host port the app's
	// port is published on when host port appPort is not published to it
	// (AppPortPublishing).
	PublishedPort func(ctx context.Context, host string, appPort uint16) uint16
	// StallGrace and StallClock are the stall rule on the exchange when
	// Unbounded, under the proven switch (answerWatch): its grace
	// (AnswerStallGrace when 0) and the runnable clock it is measured on
	// (the process's own when nil). Tests set them.
	StallGrace time.Duration
	StallClock stall.Clock
	// answer is the stall rule's watch on this exchange, for the dial.
	answer *answerWatch
}

// AppPortReachability is implemented by an instrumentation that can tell a
// failed connection to an address the app can NEVER be reached at from one to
// an app that is still starting. In Docker mode the host reaches the app only
// through the ports its docker command publishes. Replay sends each test from
// the host to the port it was recorded on, unless test.port (--port), a
// protocol port or replaceWith sends it to another host port. That host port
// then has to be published to where the app serves the test: the app's port
// the test was recorded on, or, for an app that has moved since the recording,
// the port it listens on now. A test sent to a port the command does not
// publish is refused every time (the app calling its own in-container server
// is recorded as ingress on such a port). A test on a published port where the
// app listens only on 127.0.0.1 inside the container fails every time too, as
// a dropped connection: docker accepts it, finds nothing listening on the
// container's own address, and drops it.
type AppPortReachability interface {
	// UnreachableAppPort explains why a connection to host:port cannot reach
	// the app's server on appPort, and how to fix that, as a clause that reads
	// after the prefix UnreachableAppPortError gives it. appPort is the port the
	// test was recorded on, where the app listened in its container then; 0
	// when it is not known, and then port is all there is to go on. It returns
	// "" when the address can reach the app or it cannot tell.
	//
	// It bounds its own wait: it is asked from a recording's insert loop, a
	// readiness gate and a failing test, with whatever context they hold.
	UnreachableAppPort(ctx context.Context, host string, port, appPort uint16) string
}

// AppPortPublishing is implemented by an instrumentation that can tell, in
// Docker mode, which host port reaches the app's port a test was recorded on
// when host port appPort does not: a compose file publishing "18080:8080"
// makes the app's port 8080 the host's 18080, and the client that was
// recorded called 18080. Replay sends each test from the host to the port it
// was recorded on (app_port, over the URL's: an app serving on several ports
// gets each test on its own), and nothing answers there.
type AppPortPublishing interface {
	// PublishedHostPort is the one host port, on an address host reaches,
	// the app's port appPort is published on, when host port appPort is not
	// published to it. It is 0 when host port appPort reaches it, and when
	// appPort is published on several host ports or none: the test then goes
	// where it was recorded, as without it. known is false when it cannot
	// tell (not Docker mode, a host not this machine's loopback, a container
	// that cannot be inspected or is not running, host networking).
	PublishedHostPort(ctx context.Context, host string, appPort uint16) (port uint16, known bool)
}

// PublishedTarget is target, as ResolveTestTarget made it from original for a
// test recorded on the app's port appPort, sent instead to the host port
// cfg.PublishedPort names: in Docker mode, the one host port the app's port is
// published on when host port appPort is not published to it. It applies only
// when target is that app port and nothing the user set chose this test's
// port (ConfigPort, a replaceWith URL that matches original and names a
// port, a replaceWith port mapping of appPort, TargetFromURL), since a port
// the user chose is where they mean the test to go. A replacement or mapping
// that does not apply to this test, or one that changes only its host or
// path, does not stop it. Otherwise target is returned as it is.
func PublishedTarget(ctx context.Context, cfg SimulationConfig, original, target string, appPort uint16, isHTTP bool) string {
	if cfg.PublishedPort == nil || appPort == 0 || cfg.ConfigPort != 0 || cfg.TargetFromURL {
		return target
	}
	if _, mapped := cfg.PortMappings[uint32(appPort)]; mapped {
		return target
	}
	if _, replacement, ok := urlReplacement(original, cfg.URLReplacements); ok && replacementNamesPort(replacement, isHTTP) {
		return target
	}
	var u *url.URL
	var host, port string
	if isHTTP {
		var err error
		if u, err = url.Parse(target); err != nil {
			return target
		}
		host, port = u.Hostname(), u.Port()
	} else {
		var err error
		if host, port, err = net.SplitHostPort(target); err != nil {
			return target
		}
	}
	if port != strconv.Itoa(int(appPort)) {
		return target
	}
	p := cfg.PublishedPort(ctx, host, appPort)
	if p == 0 || p == appPort {
		return target
	}
	hostPort := net.JoinHostPort(host, strconv.Itoa(int(p)))
	if !isHTTP {
		return hostPort
	}
	u.Host = hostPort
	return u.String()
}

// replacementNamesPort says a replaceWith value names a port of its own (a
// "host:port", with or without the scheme), which then decides the port its
// tests go to (ResolveTestTarget's replacementHasPort).
//
// ResolveTestTarget asks it of the replacement it applies (urlReplacement).
func replacementNamesPort(replacement string, isHTTP bool) bool {
	checkStr := replacement
	if isHTTP {
		if strings.HasPrefix(replacement, "http://") {
			checkStr = strings.TrimPrefix(replacement, "http://")
		} else if strings.HasPrefix(replacement, "https://") {
			checkStr = strings.TrimPrefix(replacement, "https://")
		}
	}
	return hasExplicitPort(checkStr)
}

// UnreachableAppPortError is a test request that failed to connect at an
// address the app can never be reached at. It unwraps to the refusal or reset,
// so it still classifies as one everywhere that matters; the one thing that
// must not act on it is a re-send (see IsUnreachableAppPort).
type UnreachableAppPortError struct {
	// Port is the port the request was sent to on the host.
	Port uint16
	// AppPort is the app's port the test was recorded on; 0 when not known.
	AppPort uint16
	Reason  string
	Err     error
}

// Error names the app's port the test was recorded on and, when the request
// went to another host port, that one too: "the app's port 8080 cannot be
// reached from the host at port 18080: <reason>". A test that does not say its
// app port names the host port alone.
func (e *UnreachableAppPortError) Error() string {
	switch {
	case e.AppPort == 0:
		return fmt.Sprintf("the app cannot be reached from the host at port %d: %s: %v", e.Port, e.Reason, e.Err)
	case e.AppPort != e.Port:
		return fmt.Sprintf("the app's port %d cannot be reached from the host at port %d: %s: %v", e.AppPort, e.Port, e.Reason, e.Err)
	}
	return fmt.Sprintf("the app's port %d cannot be reached from the host: %s: %v", e.Port, e.Reason, e.Err)
}

func (e *UnreachableAppPortError) Unwrap() error { return e.Err }

// IsUnreachableAppPort reports whether err is a test request sent to an address
// the app can never be reached at: re-sending it cannot help.
func IsUnreachableAppPort(err error) bool {
	var u *UnreachableAppPortError
	return errors.As(err, &u)
}

// unreachableAppPort asks reach, if set, whether the address a connection
// failed at is one the app's server on appPort (0: not known) can never be
// reached at, and returns the error to fail with if it is.
func unreachableAppPort(ctx context.Context, reach AppPortReachability, host, port string, appPort uint16, refused error) error {
	if reach == nil {
		return nil
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return nil
	}
	reason := reach.UnreachableAppPort(ctx, host, uint16(n), appPort)
	if reason == "" {
		return nil
	}
	return &UnreachableAppPortError{Port: uint16(n), AppPort: appPort, Reason: reason, Err: refused}
}

// urlHostPort is the host and port a request to u dials.
func urlHostPort(u *url.URL) (string, string) {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return u.Hostname(), port
}

// preparedHTTPRequest holds the prepared HTTP request and client for execution.
type preparedHTTPRequest struct {
	Request *http.Request
	Client  *http.Client
	// AppPort is the app's port the request is for: the one its test was
	// recorded on, which test.port or replaceWith may have sent to another
	// host port. 0 when not known, or when the URL names where the app is
	// (SimulationConfig.TargetFromURL).
	AppPort uint16
}

// HostHeaderOf is the Host header a client sends for u: its host, and its port
// unless that is the one its scheme has anyway. A front that routes by name
// matches the header as written, and need not know "staging:443" for the
// "staging" every client of https://staging asks it for.
func HostHeaderOf(u *url.URL) string {
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		return strings.TrimSuffix(u.Host, ":"+port)
	}
	return u.Host
}

// prepareHTTPRequest handles all common request preparation logic shared between
// SimulateHTTP and SimulateHTTPStreaming: URL decoding, body loading,
// multipart construction, compression, and client creation.
func prepareHTTPRequest(ctx context.Context, tc *models.TestCase, testSet string, logger *zap.Logger, cfg SimulationConfig) (*preparedHTTPRequest, error) {
	// A URL that is not rendered and holds "%7B" is unescaped whole before
	// it is sent. A URL a replay rendered (pkg/templates) has each value
	// escaped for its place already, and unescaping it again would turn a
	// '+' in a value into a space and a "%26" into a new query parameter: it
	// is sent as it stands.
	if strings.Contains(tc.HTTPReq.URL, "%7B") && !tc.HTTPReq.URLRendered {
		decoded, err := url.QueryUnescape(tc.HTTPReq.URL)
		if err == nil {
			tc.HTTPReq.URL = decoded
		}
	}

	// The test case arrives as it is to be sent: a replay renders its
	// placeholders (pkg/templates) on a copy before handing it here, and
	// nothing here reads or writes template values.

	reqBody := []byte(tc.HTTPReq.Body)
	var err error

	// If the request body was offloaded to an asset file (>1MB), load it back
	if tc.HTTPReq.BodyRef.Path != "" {
		bodyRefPath := tc.HTTPReq.BodyRef.Path
		// Resolve relative paths against keployPath so assets work even if
		// the keploy directory has been moved since recording.
		if cfg.KeployPath != "" && !filepath.IsAbs(bodyRefPath) {
			bodyRefPath = filepath.Join(cfg.KeployPath, bodyRefPath)
		}
		bodyData, readErr := os.ReadFile(bodyRefPath)
		if readErr != nil {
			utils.LogError(logger, readErr, "failed to read request body from asset file", zap.String("path", bodyRefPath))
			return nil, readErr
		}
		reqBody = bodyData
		logger.Debug("loaded request body from asset file",
			zap.String("path", bodyRefPath),
			zap.Int("size", len(bodyData)))
	}

	// If form field values were offloaded to asset files (>1MB) and they were not actual files (json,html,xml,txt etc...), load them back
	for i, form := range tc.HTTPReq.Form {
		if len(form.FileNames) == 0 && len(form.Paths) > 0 && len(form.Values) > 0 {
			for j, value := range form.Values {
				if value == "" && j < len(form.Paths) && form.Paths[j] != "" {
					formPath := form.Paths[j]
					if cfg.KeployPath != "" && !filepath.IsAbs(formPath) {
						formPath = filepath.Join(cfg.KeployPath, formPath)
					}
					valData, readErr := os.ReadFile(formPath)
					if readErr != nil {
						utils.LogError(logger, readErr, "failed to read form value from asset file",
							zap.String("path", formPath),
							zap.String("key", form.Key))
						return nil, readErr
					}
					tc.HTTPReq.Form[i].Values[j] = string(valData)
					logger.Debug("loaded form value from asset file",
						zap.String("key", form.Key),
						zap.String("path", formPath),
						zap.Int("size", len(valData)))
				}
			}
			// Clear Paths after restoring values so the multipart builder
			// doesn't treat these asset paths as file uploads.
			tc.HTTPReq.Form[i].Paths = nil
		}
	}

	// Build multipart body if needed
	contentType := tc.HTTPReq.Header["Content-Type"]
	if strings.HasPrefix(contentType, "multipart/form-data") && len(tc.HTTPReq.Form) > 0 {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		logger.Debug("building multipart body",
			zap.Int("form_fields", len(tc.HTTPReq.Form)),
			zap.String("content_type", contentType),
		)
		for _, field := range tc.HTTPReq.Form {
			logger.Debug("multipart field",
				zap.String("key", field.Key),
				zap.Int("values", len(field.Values)),
				zap.Int("paths", len(field.Paths)),
			)
			for _, path := range field.Paths {
				// Resolve relative paths against keployPath
				resolvedPath := path
				if cfg.KeployPath != "" && !filepath.IsAbs(path) {
					resolvedPath = filepath.Join(cfg.KeployPath, path)
				}
				logger.Debug("multipart file path", zap.String("path", resolvedPath), zap.String("field", field.Key))
				file, ferr := os.Open(resolvedPath)
				if ferr != nil {
					utils.LogError(logger, ferr, "failed to open multipart file", zap.String("path", resolvedPath))
					return nil, ferr
				}
				part, perr := writer.CreateFormFile(field.Key, filepath.Base(resolvedPath))
				if perr != nil {
					file.Close()
					utils.LogError(logger, perr, "failed to create multipart file part", zap.String("field", field.Key))
					return nil, perr
				}
				if _, cerr := io.Copy(part, file); cerr != nil {
					file.Close()
					utils.LogError(logger, cerr, "failed to write multipart file part", zap.String("field", field.Key))
					return nil, cerr
				}
				if cerr := file.Close(); cerr != nil {
					utils.LogError(logger, cerr, "failed to close multipart file", zap.String("path", path))
					return nil, cerr
				}
			}
			for valueIndex, value := range field.Values {
				logger.Debug("multipart field value",
					zap.String("field", field.Key),
					zap.Int("value_len", len(value)),
					zap.Bool("looks_binary", looksBinary(value)),
				)
				isFileValue := false
				fileName := ""
				if len(field.Paths) == 0 && len(field.FileNames) > 0 {
					isFileValue = true
				} else if len(field.Paths) == 0 && len(field.FileNames) == 0 && looksBinary(value) {
					isFileValue = true
				}

				if isFileValue {
					if len(field.FileNames) > 0 && valueIndex < len(field.FileNames) {
						fileName = field.FileNames[valueIndex]
					}
					if fileName == "" {
						fileName = "upload.bin"
					}
					fileName = filepath.Base(fileName)
					if fileName == "." || fileName == string(filepath.Separator) || fileName == "" {
						fileName = "upload.bin"
					}
					part, perr := writer.CreateFormFile(field.Key, fileName)
					if perr != nil {
						utils.LogError(logger, perr, "failed to create multipart file part", zap.String("field", field.Key))
						return nil, perr
					}
					if _, werr := part.Write([]byte(value)); werr != nil {
						utils.LogError(logger, werr, "failed to write multipart file content", zap.String("field", field.Key))
						return nil, werr
					}
					continue
				}
				if werr := writer.WriteField(field.Key, value); werr != nil {
					utils.LogError(logger, werr, "failed to write multipart field", zap.String("field", field.Key))
					return nil, werr
				}
			}
		}
		if cerr := writer.Close(); cerr != nil {
			utils.LogError(logger, cerr, "failed to close multipart writer")
			return nil, cerr
		}
		logger.Debug("multipart body built", zap.Int("body_len", body.Len()), zap.String("content_type", writer.FormDataContentType()))
		reqBody = body.Bytes()
		tc.HTTPReq.Header["Content-Type"] = writer.FormDataContentType()
		delete(tc.HTTPReq.Header, "Content-Length")
	}

	if tc.HTTPReq.Header["Content-Encoding"] != "" {
		reqBody, err = Compress(logger, tc.HTTPReq.Header["Content-Encoding"], reqBody)
		if err != nil {
			utils.LogError(logger, err, "failed to compress the request body")
			return nil, err
		}
	}

	// Resolve the execution target using ResolveTestTarget's precedence:
	// app_port < config port < replaceWith URL replacements
	// (explicit replacement port short-circuits lower-priority port overrides)
	// < replaceWith port mappings.
	appPort := tc.AppPort
	if cfg.TargetFromURL {
		appPort = 0
	}
	testURL, err := ResolveTestTarget(tc.HTTPReq.URL, cfg.URLReplacements, cfg.PortMappings, cfg.ConfigHost, appPort, cfg.ConfigPort, true, logger)
	if err != nil {
		return nil, err
	}
	testURL = PublishedTarget(ctx, cfg, tc.HTTPReq.URL, testURL, appPort, true)

	req, err := http.NewRequestWithContext(ctx, string(tc.HTTPReq.Method), testURL, bytes.NewBuffer(reqBody))
	if err != nil {
		utils.LogError(logger, err, "failed to create a http request from the yaml document")
		return nil, err
	}
	req.Header = ToWireHTTPHeader(tc.HTTPReq.Header, tc.HTTPReq.HeaderLineLengths)
	req.ProtoMajor = tc.HTTPReq.ProtoMajor
	req.ProtoMinor = tc.HTTPReq.ProtoMinor
	req.Header.Set("KEPLOY-TEST-ID", tc.Name)
	req.Header.Set("KEPLOY-TEST-SET-ID", testSet)
	// send if its the last testcase
	if tc.IsLast {
		req.Header.Set("KEPLOY-LAST-TESTCASE", "true")
	}

	// override host header if present in the request
	hostHeader := tc.HTTPReq.Header["Host"]
	if cfg.TargetFromURL {
		// Not left to the URL as it stands: ResolveTestTarget has written the
		// scheme's own port into it, and "staging:443" is not the name a
		// client of https://staging asks for.
		req.Host = HostHeaderOf(req.URL)
	} else if hostHeader != "" {
		logger.Debug("overriding host header", zap.String("host", hostHeader))
		req.Host = hostHeader
	}
	// A CONNECT in authority form (ConnectsToAuthority) names the host and
	// port it asks to reach as its request target and as its Host (RFC 9110
	// §9.3.6, §7.2): the Host it was recorded with, wherever the request is
	// sent (a base path's host included: it is the proxy the CONNECT goes
	// through, not the target). net/http would send the recorded URL's path
	// "/" as the request target; with no path and the target as the opaque
	// part, it sends the target, and the answer's label (describe) names it.
	// A recording with no Host asks for the host it is sent to.
	if ConnectsToAuthority(req.Method, req.URL) {
		target := hostHeader
		if target == "" {
			target = req.URL.Host
		}
		req.URL.Path, req.URL.RawPath, req.URL.Opaque = "", "", target
		req.Host = target
	}
	if cfg.SentTo != nil {
		asked := req.Host
		if asked == "" {
			asked = req.URL.Host
		}
		cfg.SentTo(req.URL, asked, hostHeader)
	}

	// Creating the client and disabling redirects
	_, hasAcceptEncoding := req.Header["Accept-Encoding"]
	disableCompression := !hasAcceptEncoding

	var client *http.Client
	keepAlive, ok := req.Header["Connection"]
	if ok && strings.EqualFold(keepAlive[0], "keep-alive") {
		logger.Debug("simulating request with conn:keep-alive")
		client = &http.Client{
			Timeout: cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableCompression: disableCompression,
				TLSClientConfig:    cfg.TLSConfig,
			},
		}
	} else if ok && strings.EqualFold(keepAlive[0], "close") {
		logger.Debug("simulating request with conn:close")
		client = &http.Client{
			Timeout: cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableKeepAlives:  true,
				DisableCompression: disableCompression,
				TLSClientConfig:    cfg.TLSConfig,
			},
		}
	} else {
		logger.Debug("simulating request with conn:keep-alive (maxIdleConn=1)")
		client = &http.Client{
			Timeout: cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DisableKeepAlives:  false,
				MaxIdleConns:       1,
				DisableCompression: disableCompression,
				TLSClientConfig:    cfg.TLSConfig,
			},
		}
	}

	return &preparedHTTPRequest{
		Request: req,
		Client:  client,
		AppPort: appPort,
	}, nil
}

// endAtHeaders gives an answer that switches its connection to another
// protocol no body: a 101 (Switching Protocols), whose response ends at its
// header section, what follows on the connection belonging to the protocol it
// switched to (RFC 9110 §15.2.2); and a 2xx answer to CONNECT, after whose
// header section the connection is a tunnel, its bytes those of the
// connection it tunnels to (RFC 9110 §9.3.6). For a 101, net/http hands back
// the connection itself as the body; for a 2xx answer to CONNECT with no
// length, a body read until the connection closes. keploy test speaks no
// protocol a request switches to and opens no tunnel, so it closes the
// connection unread, as a client that will not go on does. Read as the body,
// the next protocol's bytes (an h2c tunnel's preface, a WebSocket's frames, a
// tunnelled TLS handshake) made a replayed answer differ from its recording,
// which has none, once a body of another kind than the recorded one was
// compared (pkg/matcher/http, cfea2520e); and the read lasted as long as the
// app kept the connection open. A 101's connection is the caller's once it is
// handed back, so for an app that waits on its peer (most WebSocket servers)
// that was for ever: neither --api-timeout nor the proven switch's stall rule
// closes it. A CONNECT's body is the transport's still, and they end it, but
// the test failed all the same.
func endAtHeaders(resp *http.Response) {
	if resp == nil || resp.Body == nil || !switchesProtocol(resp) {
		return
	}
	_ = resp.Body.Close()
	resp.Body = http.NoBody
}

// ConnectsToAuthority reports whether a test's request, by its method and
// URL, is a CONNECT that names its target in authority form, the host and
// port it asks to reach (RFC 9110 §9.3.6). The capture records one as a URL of
// that authority (its Host) with the path "/" and no query. "CONNECT /", an
// origin-form CONNECT to the path "/", records alike, and is taken as the
// authority form, CONNECT's own; a CONNECT with another path or a query (Go's
// net/rpc's "CONNECT /_goRPC_") is in origin form, its path its own.
func ConnectsToAuthority(method string, u *url.URL) bool {
	return method == http.MethodConnect && u != nil && u.Opaque == "" &&
		(u.Path == "/" || u.Path == "") && u.RawQuery == "" && !u.ForceQuery
}

// switchesProtocol reports whether resp's connection goes on in another
// protocol after its header section (endAtHeaders).
func switchesProtocol(resp *http.Response) bool {
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return true
	}
	return resp.Request != nil && resp.Request.Method == http.MethodConnect && resp.StatusCode/100 == 2
}

// connClosingBody is the body of the app's answer to a test request: closed,
// it also closes the connection the request's own client (prepareHTTPRequest
// makes one for each request) left idle. No later request uses that client,
// so a keep-alive connection it kept would stay open to the app for the rest
// of keploy's run, one for each test: an app that serves each connection on
// a thread of its own holds those threads, and one that waits for its open
// connections as it stops (Python's ThreadingTCPServer joins each one's
// thread on its way out) cannot stop while keploy runs.
type connClosingBody struct {
	io.ReadCloser
	client *http.Client
}

func (b *connClosingBody) Close() error {
	err := b.ReadCloser.Close()
	b.client.CloseIdleConnections()
	return err
}

// maxConnRefusedRetries bounds how many times a replay test request is re-sent
// when the connection is REFUSED before the app accepts it. A refused connection
// proves the app never received the request — zero bytes sent, zero single-use
// mocks consumed — so re-sending is safe and does NOT fabricate a result: it
// obtains the real response the suite must assert on instead of a false
// status_code=0. We deliberately do NOT retry a mid-response reset (ECONNRESET),
// EOF, or broken pipe: those can occur AFTER the app read the request and
// consumed mocks, where a retry would re-run non-idempotent logic against
// exhausted mocks and fabricate a verdict. Under CI contention an app container
// can briefly refuse while still coming up; this turns that transient transport
// failure into the real comparison rather than a flaky false regression.
const (
	maxConnRefusedRetries   = 3
	connRefusedRetryBackoff = 150 * time.Millisecond
)

// isPreResponseConnRefused reports whether err is a pure "connection refused".
// errors.Is unwraps the *url.Error -> *net.OpError -> *os.SyscallError ->
// syscall.Errno chain, so no manual unwrapping is needed.
func isPreResponseConnRefused(err error) bool {
	return neterr.IsConnRefused(err)
}

// netHTTPServerClosedIdle is the text of net/http's errServerClosedIdle
// (net/http/transport.go). net/http does not export the error, so its text
// is all there is to know it by. This test provokes the real one, and fails if
// the text changes:
// TestIsTransportConnResetClassifiesAServerThatClosedTheConnectionFirst.
const netHTTPServerClosedIdle = "http: server closed idle connection"

// inErrorTree reports whether match holds for err or any error it wraps. It
// walks the tree errors.Is walks: through Unwrap() error and Unwrap() []error
// alike.
func inErrorTree(err error, match func(error) bool) bool {
	if err == nil {
		return false
	}
	if match(err) {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return inErrorTree(u.Unwrap(), match)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if inErrorTree(e, match) {
				return true
			}
		}
	}
	return false
}

// isServerClosedIdle reports whether err, or any error it wraps, is net/http's
// errServerClosedIdle.
func isServerClosedIdle(err error) bool {
	return inErrorTree(err, func(e error) bool { return e.Error() == netHTTPServerClosedIdle })
}

// isTimedOut reports whether err, or any error it wraps, says it timed out
// (a Timeout method that returns true, as net.Error has): a context's deadline
// (context.DeadlineExceeded); net/http's client timeout, awaiting the answer's
// headers or reading its body; a deadline on the connection ("i/o timeout");
// a dial, a TLS handshake or a name lookup that timed out.
func isTimedOut(err error) bool {
	return inErrorTree(err, func(e error) bool {
		t, ok := e.(interface{ Timeout() bool })
		return ok && t.Timeout()
	})
}

// isAnswerCutShort reports whether err is the connection closing part-way
// through the app's answer, by the answer's own framing: net/http reports that
// as io.ErrUnexpectedEOF.
func isAnswerCutShort(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// isConnDropped reports whether err is the connection to the app dropping: a
// reset (ECONNRESET), a broken pipe (EPIPE), a bare io.EOF, or net/http's
// errServerClosedIdle. One drop is reported as any of these. Which one depends
// on timing: net/http reports a close its read loop reads before the request
// is on the connection as errServerClosedIdle, which a request on a fresh
// connection gets as is (on a reused one, net/http sends again a request it
// can replay), and a close it reads after as io.EOF. By the error alone a
// reset can also have come part-way through an answer (see
// IsTransportConnReset). Every rule about a drop asks this one.
func isConnDropped(err error) bool {
	return neterr.IsConnReset(err) || neterr.IsBrokenPipe(err) ||
		errors.Is(err, io.EOF) || isServerClosedIdle(err)
}

// IsAppConnectionError reports whether err says a test request failed because
// the connection to the app failed: it was refused (isPreResponseConnRefused,
// which the refusal re-send and the gRPC dial ask), the app's host was not
// found, it dropped (isConnDropped), or a gRPC call lost it before the server
// answered (isGRPCConnectionLost). This is what a test result's
// APP_CONNECTION_ERROR means, and replay asks only this to set it.
//
// A close part-way through an answer (isAnswerCutShort) is not one: the app
// answered, if not completely. IsTransportConnReset takes it too, for the
// re-send its caller gates on the mocks the request consumed, and so does
// IsAppNoAnswer.
func IsAppConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	return isPreResponseConnRefused(err) ||
		(errors.As(err, &dnsErr) && dnsErr.IsNotFound) ||
		isConnDropped(err) ||
		isGRPCConnectionLost(err)
}

// IsAppNoAnswer reports whether err says a test request got no complete
// answer: none came in time (isTimedOut), the request was cancelled (a stop:
// context.Canceled), or the connection closed part-way through the answer
// (isAnswerCutShort). grpc-go turns the end of a call's
// context into a status and keeps no cause, so a gRPC call's is its code
// (isGRPCNoAnswer). This is replay's no-answer mark, which holds off the mocks
// prune and a new mappings.yaml, and replay asks only this to set it.
//
// It is not APP_CONNECTION_ERROR: a stop's cancel is not the app being down.
// An answer that came back whole but does not decode (answerDoesNotDecode) is
// an answer, so it is neither.
func IsAppNoAnswer(err error) bool {
	if err == nil {
		return false
	}
	return isTimedOut(err) ||
		errors.Is(err, context.Canceled) ||
		isAnswerCutShort(err) ||
		isGRPCNoAnswer(err) ||
		errors.Is(err, ErrAnswerStalled)
}

// IsTransportConnReset reports whether err is a transport-level connection
// reset / unexpected close while exchanging the request with the app: the
// connection dropping before any of an answer came back (isConnDropped), or
// closing part-way through one (isAnswerCutShort).
//
// This class is dominated, under loaded CI replaying a DOCKER app, by docker's
// userland proxy (docker-proxy) resetting a freshly-accepted host-side
// connection during connection setup / before the backend app ever processes
// the request. The CLI dials the published host port ([::1]:<port>) and sees
//
//	read tcp [::1]:<ephem>-><[::1]:<port>: read: connection reset by peer
//
// even for a request whose handler makes no downstream calls — proving the
// reset is in the host<->proxy hop, not the app's response logic.
//
// IMPORTANT: a reset is, by the error alone, AMBIGUOUS about whether the app
// already consumed single-use mocks (a mid-stream reset after the handler ran
// looks identical). So this predicate only CLASSIFIES the error; the decision
// to re-send is made by the caller (replay orchestration) and is gated on the
// app having consumed ZERO new mocks for this request — the same "provably
// nothing irreversible happened" invariant that makes the ECONNREFUSED re-send
// safe. Without that gate this must NEVER drive a retry.
func IsTransportConnReset(err error) bool {
	if err == nil {
		return false
	}
	// net/http surfaces a peer-side drop during the response read as a bare
	// io.EOF / io.ErrUnexpectedEOF (no syscall in the chain) — the docker-proxy
	// reset frequently lands here too (see the "EOF" hits in the reproduction).
	return isConnDropped(err) || isAnswerCutShort(err)
}

// doRequestWithConnRefusedRetry executes client.Do, re-sending ONLY on a
// pre-response connection-refused (bounded, ctx-aware backoff, request body
// rewound via GetBody). Any other error, or a real response, returns
// immediately — so a genuinely crashed/unreachable app still fails fast after
// the bounded retries, and a mid-response reset is never retried. A refusal,
// reset or drop at an address reach says the app's server on appPort (the port
// the test was recorded on, 0 when not known) can never be reached at fails at
// once, saying why, as an *UnreachableAppPortError.
func doRequestWithConnRefusedRetry(ctx context.Context, logger *zap.Logger, client *http.Client, req *http.Request, reach AppPortReachability, appPort uint16) (*http.Response, error) {
	asked := false
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		refused := isPreResponseConnRefused(err)
		// A reset or drop is asked about too: at a published port where the
		// app listens only on 127.0.0.1 in its container, docker accepts the
		// connection and drops it, every time.
		if !asked && (refused || IsTransportConnReset(err)) {
			asked = true
			host, port := urlHostPort(req.URL)
			if unreachable := unreachableAppPort(ctx, reach, host, port, appPort, err); unreachable != nil {
				return nil, unreachable
			}
		}
		if !refused {
			return nil, err
		}
		if attempt >= maxConnRefusedRetries {
			return nil, err
		}
		// Only retry if we can faithfully re-send the body; otherwise stop so we
		// never send a truncated/empty body (which would fabricate a result).
		if req.Body != nil && req.GetBody == nil {
			return nil, err
		}
		if req.GetBody != nil {
			body, gbErr := req.GetBody()
			if gbErr != nil {
				return nil, err
			}
			req.Body = body
		}
		logger.Warn("test request refused by app (app not yet accepting connections); re-sending — the request was never processed, so this is not a retry of an assertion",
			zap.Int("attempt", attempt+1), zap.Int("maxRetries", maxConnRefusedRetries), zap.Error(err))
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(connRefusedRetryBackoff * time.Duration(attempt+1)):
		}
	}
}

// answerDoesNotDecode is the error of a test request whose answer came back,
// whole by its own framing, but whose body does not decode as its
// Content-Encoding says (an encoded answer with no body, such as a 204 or the
// answer to a HEAD, or an encoded body cut short). It keeps the decoder's
// error as text only, never wrapped: gzip reports a body with no bytes as
// io.EOF and a short one as io.ErrUnexpectedEOF, and br reports both as
// io.ErrUnexpectedEOF, the errors of a dropped connection and of an answer the
// connection cut short, and the app did answer. Wrapped, they would read as a
// drop to the reset re-send (IsTransportConnReset) and to APP_CONNECTION_ERROR
// (IsAppConnectionError), and as no answer to replay's no-answer mark
// (IsAppNoAnswer). A body that decodes but inflates past the cap is not one:
// it keeps Decompress's error, which wraps ErrDecompressedTooLarge and none of
// those.
func answerDoesNotDecode(status int, encoding string, err error) error {
	return fmt.Errorf("the app's %d answer does not decode as its Content-Encoding %q says: %v", status, encoding, err)
}

// decodedPart is what of a body cut part-way decodes as its Content-Encoding
// says: the bytes gzip or br gave before the cut, which may be none. A body
// in no encoding, or in one keploy does not decode (Decompress keeps such a
// body as it came), is kept as it came.
func decodedPart(encoding string, data []byte) string {
	var r io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return ""
		}
		r = zr
	case "br":
		r = brotli.NewReader(bytes.NewReader(data))
	default:
		return string(data)
	}
	out, _ := io.ReadAll(io.LimitReader(r, MaxDecompressedSize))
	return string(out)
}

// SimulateHTTP sends tc's request to the app and returns its answer. It sends
// tc as given: a test case's placeholders are rendered by the replay, on a
// copy, before it gets here (pkg/templates), and what the app answers is
// read for template values there too, never here.
func SimulateHTTP(ctx context.Context, tc *models.TestCase, testSet string, logger *zap.Logger, cfg SimulationConfig) (*models.HTTPResp, error) {
	logger.Info("starting test for", zap.Any("test case", models.HighlightString(tc.Name)), zap.Any("test set", models.HighlightString(testSet)))

	// Prepare the HTTP request using the shared helper
	cfg.RequestTimeout = time.Second * time.Duration(cfg.APITimeout)
	if cfg.Unbounded {
		cfg.RequestTimeout = 0
	}
	// With no clock on the app's answer, the stall rule (answerWatch) ends
	// an exchange the app makes no progress on: the whole call waits on the
	// app, and the cut cancels the request's context.
	cfg.answer = newAnswerWatch(cfg)
	defer cfg.answer.stop()
	defer cfg.answer.waiting()()
	ctx = cfg.answer.bind(ctx)
	prepared, err := prepareHTTPRequest(ctx, tc, testSet, logger, cfg)
	if err != nil {
		return nil, err
	}
	cfg.answer.describe(prepared.Request.Method+" "+prepared.Request.URL.RequestURI(), prepared.Request.ContentLength)
	cfg.answer.countRequestBody(prepared.Request)

	logger.Debug(fmt.Sprintf("Sending request to user app:%v", prepared.Request))

	// Execute the request (re-sending only on a pre-response connection-refused)
	httpResp, errHTTPReq := doRequestWithConnRefusedRetry(ctx, logger, prepared.Client, prepared.Request, cfg.AppPortReachability, prepared.AppPort)
	if errHTTPReq != nil {
		prepared.Client.CloseIdleConnections()
		if cfg.answer.didStall() {
			return nil, cfg.answer.err()
		}
		utils.LogError(logger, errHTTPReq, "failed to send testcase request to app")
		return nil, errHTTPReq
	}
	endAtHeaders(httpResp)
	httpResp.Body = &connClosingBody{ReadCloser: httpResp.Body, client: prepared.Client}
	if cfg.answer != nil {
		cfg.answer.answering()
		httpResp.Body = &answerBody{ReadCloser: httpResp.Body, w: cfg.answer}
	}

	defer func() {
		if httpResp != nil && httpResp.Body != nil {
			if err := httpResp.Body.Close(); err != nil {
				utils.LogError(logger, err, "failed to close response body")
			}
		}
	}()

	// Read full response body
	respBody, errReadRespBody := io.ReadAll(httpResp.Body)
	if errReadRespBody != nil {
		if cfg.answer.didStall() {
			// The answer stopped part-way: nothing whole to compare. Its
			// status, headers and the part of its body that came, decoded
			// as far as it decodes, go with the error, for the replay to
			// judge the start first (AnswerBegan).
			return &models.HTTPResp{
				StatusCode:    httpResp.StatusCode,
				StatusMessage: http.StatusText(httpResp.StatusCode),
				Header:        ToYamlHTTPHeader(httpResp.Header),
				Body:          decodedPart(httpResp.Header.Get("Content-Encoding"), respBody),
			}, cfg.answer.err()
		}
		utils.LogError(logger, errReadRespBody, "failed reading response body")
		return nil, errReadRespBody
	}

	// Decompress if needed
	if enc := httpResp.Header.Get("Content-Encoding"); enc != "" {
		respBody, err = Decompress(logger, enc, respBody, MaxDecompressedSize)
		if err != nil {
			utils.LogError(logger, err, "failed to decode response body")
			// A body that inflates past the cap decodes; it is only too large.
			// It keeps Decompress's error, by which a caller tells it from a
			// body that does not decode (ErrDecompressedTooLarge).
			if errors.Is(err, ErrDecompressedTooLarge) {
				return nil, err
			}
			return nil, answerDoesNotDecode(httpResp.StatusCode, enc, err)
		}
	}

	statusMessage := http.StatusText(httpResp.StatusCode)

	resp := &models.HTTPResp{
		StatusCode:    httpResp.StatusCode,
		StatusMessage: statusMessage,
		Body:          string(respBody),
		Header:        ToYamlHTTPHeader(httpResp.Header),
	}

	return resp, errHTTPReq
}

// SimulateHTTPStreaming sends an HTTP request and returns the streaming response with a reader.
// The caller is responsible for reading from the response Reader and closing it when done.
// Use CompareHTTPStream to compare the streaming response with expected values.
func SimulateHTTPStreaming(ctx context.Context, tc *models.TestCase, testSet string, logger *zap.Logger, cfg SimulationConfig) (*StreamingHTTPResponse, error) {
	logger.Info("starting streaming test for", zap.Any("test case", models.HighlightString(tc.Name)), zap.Any("test set", models.HighlightString(testSet)))

	// Calculate streaming timeout
	APITimeout := ComputeStreamingTimeoutSeconds(tc, cfg.APITimeout)
	cfg.RequestTimeout = time.Second * time.Duration(APITimeout)
	if cfg.Unbounded {
		// The stream's mocks keep their window (effectiveStreamMockWindow
		// computes it on its own); only the clock on the app's answer goes.
		cfg.RequestTimeout = 0
	}

	// With no clock on the app's answer, the stall rule (answerWatch) ends
	// an exchange the app makes no progress on, the stream's read included:
	// this call waits on the app, then each read of the returned Reader
	// does, and its Close ends the watch. The cut cancels the request's
	// context.
	cfg.answer = newAnswerWatch(cfg)
	handedOff := false
	defer func() {
		if !handedOff {
			cfg.answer.stop()
		}
	}()
	defer cfg.answer.waiting()()
	ctx = cfg.answer.bind(ctx)

	// Prepare the HTTP request using the shared helper
	prepared, err := prepareHTTPRequest(ctx, tc, testSet, logger, cfg)
	if err != nil {
		return nil, err
	}
	cfg.answer.describe(prepared.Request.Method+" "+prepared.Request.URL.RequestURI(), prepared.Request.ContentLength)
	cfg.answer.countRequestBody(prepared.Request)

	logger.Debug(fmt.Sprintf("Sending streaming request to user app:%v", prepared.Request))

	// Execute the request (re-sending only on a pre-response connection-refused)
	httpResp, errHTTPReq := doRequestWithConnRefusedRetry(ctx, logger, prepared.Client, prepared.Request, cfg.AppPortReachability, prepared.AppPort)
	if errHTTPReq != nil {
		prepared.Client.CloseIdleConnections()
		if cfg.answer.didStall() {
			return nil, cfg.answer.err()
		}
		utils.LogError(logger, errHTTPReq, "failed to send testcase request to app")
		return nil, errHTTPReq
	}
	endAtHeaders(httpResp)
	httpResp.Body = &connClosingBody{ReadCloser: httpResp.Body, client: prepared.Client}
	if cfg.answer != nil {
		cfg.answer.answering()
		httpResp.Body = &answerBody{ReadCloser: httpResp.Body, w: cfg.answer}
	}

	statusMessage := http.StatusText(httpResp.StatusCode)
	streamCfg := DetectHTTPStreamConfig(tc, httpResp)
	if provenswitch.On() {
		streamCfg.Target = prepared.Request.Method + " " + prepared.Request.URL.RequestURI()
	}

	logger.Debug("detected HTTP streaming response",
		zap.String("testcase", tc.Name),
		zap.String("content_type", httpResp.Header.Get("Content-Type")),
		zap.String("stream_mode", string(streamCfg.Mode)))

	// Wrap the response body with decompression if needed
	streamReader := io.ReadCloser(httpResp.Body)
	contentEncoding := strings.ToLower(strings.TrimSpace(httpResp.Header.Get("Content-Encoding")))
	switch contentEncoding {
	case "gzip":
		gzipReader, gzErr := gzip.NewReader(httpResp.Body)
		if gzErr != nil {
			httpResp.Body.Close()
			if cfg.answer.didStall() {
				// Not one byte of the stream came: nothing to compare.
				return nil, cfg.answer.err()
			}
			utils.LogError(logger, gzErr, "failed to create gzip reader for streaming response")
			// A body that ended, by its own framing, before its first byte
			// (io.EOF), or whose first bytes are not a gzip header, is the
			// answer not decoding. Any other error may have come from reading
			// the body off the connection, and stays as it is.
			if errors.Is(gzErr, io.EOF) || errors.Is(gzErr, gzip.ErrHeader) {
				return nil, answerDoesNotDecode(httpResp.StatusCode, contentEncoding, gzErr)
			}
			return nil, gzErr
		}
		// Cap decompressed output so a bomb fails the test instead of
		// OOMing while CompareHTTPStream accumulates the body (#3867).
		streamReader = NewCappedReadCloser(&gzipReadCloser{gzipReader: gzipReader, underlying: httpResp.Body}, MaxDecompressedSize)
	case "br":
		streamReader = NewCappedReadCloser(&brotliReadCloser{reader: brotli.NewReader(httpResp.Body), underlying: httpResp.Body}, MaxDecompressedSize)
	case "":
		// no-op, use httpResp.Body directly
	default:
		logger.Debug("unsupported content-encoding for stream; returning raw response body",
			zap.String("content_encoding", contentEncoding))
	}

	if cfg.answer != nil {
		streamReader = &answerReader{ReadCloser: streamReader, w: cfg.answer}
		handedOff = true
	}
	return &StreamingHTTPResponse{
		StatusCode:    httpResp.StatusCode,
		StatusMessage: statusMessage,
		Header:        ToYamlHTTPHeader(httpResp.Header),
		Reader:        streamReader,
		StreamConfig:  streamCfg,
	}, nil
}

// gzipReadCloser wraps a gzip reader and its underlying body to close both.
type gzipReadCloser struct {
	gzipReader *gzip.Reader
	underlying io.ReadCloser
}

func (g *gzipReadCloser) Read(p []byte) (int, error) {
	return g.gzipReader.Read(p)
}

func (g *gzipReadCloser) Close() error {
	gzErr := g.gzipReader.Close()
	underErr := g.underlying.Close()
	if gzErr != nil {
		return gzErr
	}
	return underErr
}

// brotliReadCloser wraps a brotli reader and its underlying body.
type brotliReadCloser struct {
	reader     io.Reader
	underlying io.ReadCloser
}

func (b *brotliReadCloser) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *brotliReadCloser) Close() error {
	return b.underlying.Close()
}

// DetectHTTPStreamConfig is how a test case's response is read: as a stream
// of which format, or whole (HTTPStreamModeNone). It is keploy's one stream
// rule (models.HTTPStreamOf) on the answer's Content-Type (the recorded one
// when there is no answer yet, or the answer has none) and on whether the
// recording kept its body as frames. How the answer framed its length
// (Transfer-Encoding: chunked, no Content-Length) is no evidence of a stream.
//
// A response that HTTP says has no body (the answer to a HEAD, a 1xx, 204 or
// 304: models.HTTPResponseHasNoBody) is no stream, whatever its Content-Type
// says: it has no frame to compare, and its status and headers are compared
// as any response's are. Which response has none the app's answer says
// (streamResponseHasNoBody), never the recording.
func DetectHTTPStreamConfig(tc *models.TestCase, resp *http.Response) HTTPStreamConfig {
	if streamResponseHasNoBody(tc, resp) {
		return HTTPStreamConfig{Mode: HTTPStreamModeNone}
	}
	contentType := ""
	if resp != nil {
		contentType = resp.Header.Get("Content-Type")
	}
	if contentType == "" && tc != nil {
		contentType = getHeaderValueCaseInsensitive(tc.HTTPResp.Header, "Content-Type")
	}
	framed := tc != nil && len(tc.HTTPResp.StreamBody) > 0

	switch models.HTTPStreamOf(contentType, framed) {
	case models.HTTPStreamSSE:
		return HTTPStreamConfig{Mode: HTTPStreamModeSSE}
	case models.HTTPStreamNDJSON:
		return HTTPStreamConfig{Mode: HTTPStreamModeNDJSON}
	case models.HTTPStreamMultipart:
		boundary := boundaryFromContentTypeHeader(contentType)
		if boundary == "" && tc != nil {
			boundary = boundaryFromContentTypeHeader(getHeaderValueCaseInsensitive(tc.HTTPResp.Header, "Content-Type"))
		}
		if boundary != "" {
			return HTTPStreamConfig{Mode: HTTPStreamModeMultipart, Boundary: boundary}
		}
	case models.HTTPStreamText:
		return HTTPStreamConfig{Mode: HTTPStreamModePlainText}
	case models.HTTPStreamBinary:
		return HTTPStreamConfig{Mode: HTTPStreamModeBinary}
	}

	return HTTPStreamConfig{Mode: HTTPStreamModeNone}
}

// streamResponseHasNoBody reports whether resp, the app's answer, has no body
// by HTTP: by the method of the request it answers (the test's recorded one
// when resp names none) and its status. Before the answer (resp nil:
// IsHTTPStreamingTestCase, choosing how a test's answer is read) it is false:
// a test case of a stream's Content-Type is sent as a stream test whatever
// its recorded method and status, and its answer says how it is read. The
// recorded status cannot say it: the app may answer a recorded 204 or 304
// with a stream, which read whole, as a response with no body by its
// recording was, is a read that never ends when the app never ends the
// stream (under the proven switch no clock bounds it, and every byte of it
// is progress to the stall rule). Read as a stream, a recording of no frame
// ends that read on the stream's first keepalive or frame
// (streamRecordedWithNoFrame), and an answer with no body (any answer to a
// HEAD among them) is compared as a response (SimulateHTTPStreaming, by its
// answer).
func streamResponseHasNoBody(tc *models.TestCase, resp *http.Response) bool {
	if resp == nil {
		return false
	}
	method := ""
	if resp.Request != nil {
		method = resp.Request.Method
	} else if tc != nil {
		method = string(tc.HTTPReq.Method)
	}
	return models.HTTPResponseHasNoBody(method, resp.StatusCode)
}

// IsHTTPStreamingTestCase reports whether a test case's recorded response is a
// stream by keploy's one stream rule (DetectHTTPStreamConfig, on its recorded
// Content-Type and frames): such a test case is replayed after its set's other
// test cases, with a stream's mock window (effectiveStreamMockWindow), and its
// answer read frame by frame. Every other one runs in its recorded order and
// is read whole. A recorded HEAD, 204 or 304 is one by its Content-Type, as
// any test case is, and its answer says whether it has a body
// (streamResponseHasNoBody): one with none is compared as a response.
func IsHTTPStreamingTestCase(tc *models.TestCase) bool {
	if tc == nil {
		return false
	}
	return DetectHTTPStreamConfig(tc, nil).Mode != HTTPStreamModeNone
}

// CompareHTTPStream compares an expected HTTP response with a streaming response.
// It returns whether they match, the captured body content, mismatch details (if any), and any error.
//
// Without the proven switch the comparison stops at the last recorded frame
// (part, or byte), so a stream that goes on is not waited for; a stream
// recorded with no frame has none to stop at, and is read to its end under
// either switch (streamRecordedWithNoFrame). Under the
// switch every response is compared whole (O-K): the stream is read to its
// end, and a frame after the recorded ones is a difference (trailingFrame),
// as are bytes beyond a recorded binary stream. A keepalive beyond the
// recorded ones (an SSE frame of comments alone, an NDJSON blank line) once
// every recorded frame has come ends the read: the app kept the stream open
// past its recording, and the test has no verdict (ErrStreamHeldOpen, row
// W1). The read is bounded as the request is: the --api-timeout or the
// recorded duration plus 10 s, whichever is longer
// (ComputeStreamingTimeoutSeconds); under the
// switch only when the user set an apiTimeout of their own, and otherwise by
// no clock (SimulationConfig.Unbounded).
func CompareHTTPStream(expectedResp models.HTTPResp, stream io.Reader, cfg HTTPStreamConfig, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	ok, body, info, err := compareHTTPStream(expectedResp, stream, cfg, jsonNoiseKeys, logger)
	var st *AnswerStallError
	if errors.As(err, &st) && st.Stream && info == nil {
		// The stall rule cut the stream with every frame that came
		// matching: judged on what came, not judged as a whole.
		ok = true
	}
	if errors.As(err, &st) && !st.Stream && st.Target == "" && cfg.Target != "" {
		// A stream with nothing of its answer to compare names its request.
		named := *st
		named.Target = cfg.Target
		err = &named
	}
	return ok, body, info, err
}

func compareHTTPStream(expectedResp models.HTTPResp, stream io.Reader, cfg HTTPStreamConfig, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	switch cfg.Mode {
	case HTTPStreamModeSSE:
		return compareSSEStream(expectedResp, stream, jsonNoiseKeys, logger)
	case HTTPStreamModeNDJSON:
		return compareNDJSONStream(expectedResp, stream, jsonNoiseKeys, logger)
	case HTTPStreamModeMultipart:
		return compareMultipartStream(expectedResp, stream, cfg.Boundary, jsonNoiseKeys, logger)
	case HTTPStreamModePlainText:
		return comparePlainTextStream(expectedResp, stream, logger)
	case HTTPStreamModeBinary:
		return compareBinaryStream(expectedResp, stream, logger)
	default:
		return false, "", nil, fmt.Errorf("unsupported HTTP stream mode: %s", cfg.Mode)
	}
}

// ErrStreamHeldOpen ends the read of a stream, under the proven switch, that
// the app kept open past its recording (row W1): every recorded frame (gRPC:
// message) came, and then a keepalive beyond the recorded ones. A stream is
// read to its end and no clock ends it, so the app's keepalives do: the test
// has no verdict (models.FailureInfo.StreamHeldOpen), never a pass, and the
// wait does not outlast the app's next keepalive. A frame beyond the
// recording that is no keepalive fails the test, as before.
var ErrStreamHeldOpen = errors.New("the app kept the stream open past its recording")

// ErrStreamStalled ends the read of a stream, under the proven switch, that
// stalled short of its recording (row W1, round 2): the app neither ended it
// nor sent the rest of its recorded answer, and sent keepalives, two in a row
// with no frame (gRPC: message) between them. It is an ErrStreamHeldOpen
// (errors.Is): the test has no verdict, never a pass or a failure on the rest
// that did not come, and the wait never hangs. What came is still compared,
// and a difference in it fails the test. An app idle for two of its
// keepalive intervals before the rest of its answer is not judged either.
var ErrStreamStalled error = streamStalledError{}

type streamStalledError struct{}

func (streamStalledError) Error() string {
	return "the app kept the stream open and sent no more of its recording, keepalive after keepalive"
}

func (streamStalledError) Is(target error) bool { return target == ErrStreamHeldOpen }

// ErrStreamNotRecorded refuses a stream test whose recording saved nothing of
// its stream: its body was over 1 MB, and a recording does not save one
// (BodySkipped). Compared with nothing, a stream passed on its first frame,
// whatever it held, with the proven switch off. The test fails with the
// reason instead, under either switch, and its stream is not read. A
// recording that saved its body is compared with it, however little it holds
// (streamRecordedWithNoFrame): re-recording the app gives the same one.
var ErrStreamNotRecorded = errors.New("keploy refuses this stream test: its recording keeps nothing of the stream to compare the app's stream with")

// streamRecordedWithNoFrame is the rule for a stream recorded with no frame
// to compare (no SSE frame, NDJSON line, multipart part, text line or byte).
// A recording that saved nothing of its stream (its body was over 1 MB, and a
// recording does not save one: BodySkipped) is refused: the error it returns
// (ErrStreamNotRecorded). Any other saved its body, and holds what the stream
// held: nothing, the app having ended it with nothing in it, or blank lines
// alone (an SSE body of blank lines, NDJSON's keepalives, which a recording
// keeps no frame of; a multipart body of no part). The app's stream is
// compared with that, under either switch, and read to its end (nil): with no
// recorded frame to stop at, the comparison stopped before the answer's first
// frame, and passed whatever came. A frame (an SSE event, a line, a part) is
// one past the recording (trailingFrame); a keepalive (an SSE comment, an
// NDJSON blank line) ends the read with nothing of the answer to compare, not
// judged (keptOpenWithKeepalivesOnly); and its end passes. A recording of SSE
// comments alone is none of these: its comments are its frames, compared in
// order as any recording's are, the comparison stopping at the last of them
// with the switch off. Re-recording the app gives the same recording, so it
// is never refused.
func streamRecordedWithNoFrame(expected models.HTTPResp) error {
	if !expected.BodySkipped {
		return nil
	}
	why := "its body was over 1 MB, and a recording does not save one"
	if expected.BodySize > 0 {
		why = fmt.Sprintf("its body, %d bytes, was over 1 MB, and a recording does not save one", expected.BodySize)
	}
	return fmt.Errorf("%w (%s)", ErrStreamNotRecorded, why)
}

// trailingFrame is the mismatch of a frame the answer streamed after the
// recorded ones (index of them): under the proven switch a stream is read to
// its end, and one that goes on past its recording differs from it.
func trailingFrame(index int, frame string) *StreamMismatchInfo {
	return &StreamMismatchInfo{
		FrameIndex:    index,
		ExpectedFrame: "(the recorded stream ended)",
		ActualFrame:   frame,
		Reason:        fmt.Sprintf("the stream went on after its %d recorded frames", index),
	}
}

// sseCommentsOnly reports whether an SSE frame holds comment lines alone.
func sseCommentsOnly(fields []sseField) bool {
	for _, f := range fields {
		if !f.comment {
			return false
		}
	}
	return len(fields) > 0
}

func ComputeStreamingTimeoutSeconds(tc *models.TestCase, defaultSeconds uint64) uint64 {
	baseTimeout := defaultSeconds
	if baseTimeout == 0 {
		baseTimeout = 10
	}

	if tc == nil {
		return baseTimeout
	}

	reqTs := tc.HTTPReq.Timestamp
	respTs := tc.HTTPResp.Timestamp
	if reqTs.IsZero() || respTs.IsZero() {
		return baseTimeout
	}

	diff := respTs.Sub(reqTs)
	if diff < 0 {
		diff = -diff
	}

	timeout := diff + 10*time.Second
	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	}
	streamTimeoutSeconds := uint64(math.Ceil(timeout.Seconds()))
	if streamTimeoutSeconds < 10 {
		streamTimeoutSeconds = 10
	}
	if baseTimeout > streamTimeoutSeconds {
		return baseTimeout
	}
	return streamTimeoutSeconds
}

// CollectStreamingGlobalNoiseKeys extracts noise keys from global body noise and test case noise configurations
// that should be ignored during streaming response comparison.
func CollectStreamingGlobalNoiseKeys(globalBodyNoise map[string][]string, tcNoise map[string][]string) map[string]struct{} {
	keys := make(map[string]struct{})
	add := func(candidate string) {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			return
		}
		if strings.HasPrefix(candidate, "body.") {
			candidate = strings.TrimPrefix(candidate, "body.")
		}
		if strings.Contains(candidate, ".") {
			return
		}
		keys[candidate] = struct{}{}
	}

	for k := range globalBodyNoise {
		add(k)
	}
	for k := range tcNoise {
		add(k)
	}
	return keys
}

func getHeaderValueCaseInsensitive(headers map[string]string, key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	for k, v := range headers {
		if strings.ToLower(strings.TrimSpace(k)) == key {
			return v
		}
	}
	return ""
}

func hasSSEContentType(value string) bool {
	return strings.Contains(strings.ToLower(value), "text/event-stream")
}

// IsSSERequest checks whether a test case targets an SSE endpoint based on
// the recorded response Content-Type or request Accept header. It picks the
// port the request is sent to (an app may serve SSE on its own port, and a
// CORS preflight to it asks for text/event-stream in its Accept), not
// whether the response is a stream (models.HTTPStreamOf).
func IsSSERequest(tc *models.TestCase) bool {
	if tc == nil || tc.Kind != models.HTTP {
		return false
	}
	return hasSSEContentType(getHeaderValueCaseInsensitive(tc.HTTPResp.Header, "Content-Type")) ||
		hasSSEContentType(getHeaderValueCaseInsensitive(tc.HTTPReq.Header, "Accept"))
}

func boundaryFromContentTypeHeader(contentType string) string {
	if contentType == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(params["boundary"])
}

type expectedSSEFrame struct {
	fields []sseField
	raw    string
}

func compareSSEStream(expectedResp models.HTTPResp, stream io.Reader, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	readToEnd := provenswitch.On()
	expectedQueue, err := extractExpectedSSEQueue(expectedResp)
	if err != nil {
		return false, "", nil, err
	}
	if len(expectedQueue) == 0 {
		// No frame: an empty stream's, or blank lines alone. A recording
		// of comments alone holds frames, and is compared by them, as any
		// recording is.
		if err := streamRecordedWithNoFrame(expectedResp); err != nil {
			return false, "", nil, err
		}
		readToEnd = true
	}
	actualQueue := make([]string, 0, len(expectedQueue))
	nextExpected := 0

	scanner := sseFrameScanner(stream)
	for scanner.Scan() {
		frame, ok := sseFrameOf(scanner.Text())
		if !ok {
			continue
		}

		if nextExpected >= len(expectedQueue) {
			if readToEnd {
				// A frame of comments alone (": ping") is a keepalive: it
				// carries no event, so one after the recorded frames is no
				// difference. It is one beyond the recorded keepalives,
				// which are among the recorded frames: the app kept the
				// stream open past its recording, and the read ends there
				// (ErrStreamHeldOpen).
				if sseCommentsOnly(parseSSEFrame(frame)) {
					if nextExpected == 0 {
						// A recording of no frame, and the app sent
						// none: nothing of the answer to compare.
						return false, strings.Join(actualQueue, "\n\n"), nil, keptOpenWithKeepalivesOnly()
					}
					return true, strings.Join(actualQueue, "\n\n"), nil, ErrStreamHeldOpen
				}
				actualQueue = append(actualQueue, frame)
				return false, strings.Join(actualQueue, "\n\n"), trailingFrame(len(expectedQueue), frame), nil
			}
			logger.Debug("received additional SSE data after expected stream was fully matched; closing stream capture",
				zap.Int("expected_frames", len(expectedQueue)))
			break
		}

		actualQueue = append(actualQueue, frame)
		expectedFrame := expectedQueue[nextExpected]
		match, reason := compareSSEFields(expectedFrame.fields, parseSSEFrame(frame), jsonNoiseKeys, logger)
		if !match {
			logger.Debug("SSE frame mismatch",
				zap.Int("frame_index", nextExpected),
				zap.String("reason", reason),
				zap.String("expected_frame", expectedFrame.raw),
				zap.String("actual_frame", frame))
			mismatchInfo := &StreamMismatchInfo{
				FrameIndex:    nextExpected,
				ExpectedFrame: expectedFrame.raw,
				ActualFrame:   frame,
				Reason:        reason,
			}
			return false, strings.Join(actualQueue, "\n\n"), mismatchInfo, nil
		}

		nextExpected++
		if nextExpected == len(expectedQueue) && !readToEnd {
			logger.Debug("all expected SSE frames matched; closing stream capture early to avoid waiting for extra stream events",
				zap.Int("matched_frames", nextExpected))
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return false, strings.Join(actualQueue, "\n\n"), nil, streamCut(err, nextExpected, len(expectedQueue), "frame")
	}

	if nextExpected < len(expectedQueue) {
		logger.Debug("SSE stream ended before all expected frames were received",
			zap.Int("expected_frames", len(expectedQueue)),
			zap.Int("matched_frames", nextExpected))
		// Build expected frames that were not received
		var missingFrames []string
		for i := nextExpected; i < len(expectedQueue); i++ {
			missingFrames = append(missingFrames, expectedQueue[i].raw)
		}
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    nextExpected,
			ExpectedFrame: strings.Join(missingFrames, "\n\n"),
			ActualFrame:   "(stream ended - no more frames)",
			Reason:        fmt.Sprintf("expected %d frames but only received %d", len(expectedQueue), nextExpected),
		}
		return false, strings.Join(actualQueue, "\n\n"), mismatchInfo, nil
	}

	return true, strings.Join(actualQueue, "\n\n"), nil, nil
}

func compareNDJSONStream(expectedResp models.HTTPResp, stream io.Reader, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	readToEnd := provenswitch.On()
	expectedQueue, err := extractExpectedRawQueue(expectedResp, canonicalizeNDJSONLine, true)
	if err != nil {
		return false, "", nil, err
	}
	if len(expectedQueue) == 0 {
		// No line: an empty stream's, or blank lines alone (keepalives).
		if err := streamRecordedWithNoFrame(expectedResp); err != nil {
			return false, "", nil, err
		}
		readToEnd = true
	}
	actualQueue := make([]string, 0, len(expectedQueue))
	nextExpected := 0

	scanner := lineScanner(stream)

	// keepalives counts the blank lines since the last line.
	keepalives := 0
	for scanner.Scan() {
		line := canonicalizeNDJSONLine(scanner.Text())
		if line == "" {
			// A blank line is NDJSON's keepalive. One after the recorded
			// lines is beyond the recorded keepalives (a recording keeps
			// none: it drops blank lines): the app kept the stream open
			// past its recording (ErrStreamHeldOpen). Two in a row before
			// them, with no line between, and the app stalled short of
			// its recording (ErrStreamStalled): the lines that came
			// matched theirs.
			// Either with no line come is nothing of the answer to
			// compare: it fails closed, not judged (ErrAnswerStalled).
			if readToEnd && nextExpected >= len(expectedQueue) {
				if nextExpected == 0 {
					return false, strings.Join(actualQueue, "\n"), nil, keptOpenWithKeepalivesOnly()
				}
				return true, strings.Join(actualQueue, "\n"), nil, ErrStreamHeldOpen
			}
			keepalives++
			if readToEnd && keepalives >= 2 {
				if nextExpected == 0 {
					return false, strings.Join(actualQueue, "\n"), nil, keptOpenWithKeepalivesOnly()
				}
				return true, strings.Join(actualQueue, "\n"), nil, ErrStreamStalled
			}
			continue
		}
		keepalives = 0

		if nextExpected >= len(expectedQueue) {
			if readToEnd {
				actualQueue = append(actualQueue, line)
				return false, strings.Join(actualQueue, "\n"), trailingFrame(len(expectedQueue), line), nil
			}
			logger.Debug("received additional NDJSON data after expected stream was fully matched; closing stream capture",
				zap.Int("expected_frames", len(expectedQueue)))
			break
		}

		actualQueue = append(actualQueue, line)
		expected := expectedQueue[nextExpected]
		ok, cmpErr := compareJSONTextWithNoise(expected, line, jsonNoiseKeys)
		if cmpErr != nil || !ok {
			reason := "json mismatch"
			if cmpErr != nil {
				reason = cmpErr.Error()
			}
			logger.Debug("NDJSON stream mismatch",
				zap.Int("frame_index", nextExpected),
				zap.String("reason", reason),
				zap.String("expected_frame", expected),
				zap.String("actual_frame", line))
			mismatchInfo := &StreamMismatchInfo{
				FrameIndex:    nextExpected,
				ExpectedFrame: expected,
				ActualFrame:   line,
				Reason:        reason,
			}
			return false, strings.Join(actualQueue, "\n"), mismatchInfo, nil
		}

		nextExpected++
		if nextExpected == len(expectedQueue) && !readToEnd {
			logger.Debug("all expected NDJSON frames matched; closing stream capture early to avoid waiting for extra stream events",
				zap.Int("matched_frames", nextExpected))
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return false, strings.Join(actualQueue, "\n"), nil, streamCut(err, nextExpected, len(expectedQueue), "line")
	}

	if nextExpected < len(expectedQueue) {
		logger.Debug("NDJSON stream ended before all expected frames were received",
			zap.Int("expected_frames", len(expectedQueue)),
			zap.Int("matched_frames", nextExpected))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    nextExpected,
			ExpectedFrame: expectedQueue[nextExpected],
			ActualFrame:   "(stream ended - no more frames)",
			Reason:        fmt.Sprintf("expected %d frames but only received %d", len(expectedQueue), nextExpected),
		}
		return false, strings.Join(actualQueue, "\n"), mismatchInfo, nil
	}

	return true, strings.Join(actualQueue, "\n"), nil, nil
}

func comparePlainTextStream(expectedResp models.HTTPResp, stream io.Reader, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	readToEnd := provenswitch.On()
	expectedQueue, err := extractExpectedRawQueue(expectedResp, canonicalizePlainTextLine, false)
	if err != nil {
		return false, "", nil, err
	}
	if len(expectedQueue) == 0 {
		// No line (a text stream's blank lines are its lines): an empty
		// stream's.
		if err := streamRecordedWithNoFrame(expectedResp); err != nil {
			return false, "", nil, err
		}
		readToEnd = true
	}
	actualQueue := make([]string, 0, len(expectedQueue))
	nextExpected := 0

	scanner := lineScanner(stream)

	for scanner.Scan() {
		line := canonicalizePlainTextLine(scanner.Text())

		if nextExpected >= len(expectedQueue) {
			if readToEnd {
				actualQueue = append(actualQueue, line)
				return false, strings.Join(actualQueue, "\n"), trailingFrame(len(expectedQueue), line), nil
			}
			logger.Debug("received additional plain-text stream data after expected stream was fully matched; closing stream capture",
				zap.Int("expected_frames", len(expectedQueue)))
			break
		}

		actualQueue = append(actualQueue, line)
		expected := expectedQueue[nextExpected]
		if line != expected {
			logger.Debug("plain-text stream mismatch",
				zap.Int("frame_index", nextExpected),
				zap.String("expected", expected),
				zap.String("actual", line))
			mismatchInfo := &StreamMismatchInfo{
				FrameIndex:    nextExpected,
				ExpectedFrame: expected,
				ActualFrame:   line,
				Reason:        fmt.Sprintf("content mismatch at frame %d", nextExpected),
			}
			return false, strings.Join(actualQueue, "\n"), mismatchInfo, nil
		}

		nextExpected++
		if nextExpected == len(expectedQueue) && !readToEnd {
			logger.Debug("all expected plain-text frames matched; closing stream capture early to avoid waiting for extra stream events",
				zap.Int("matched_frames", nextExpected))
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return false, strings.Join(actualQueue, "\n"), nil, streamCut(err, nextExpected, len(expectedQueue), "line")
	}

	if nextExpected < len(expectedQueue) {
		logger.Debug("plain-text stream ended before all expected frames were received",
			zap.Int("expected_frames", len(expectedQueue)),
			zap.Int("matched_frames", nextExpected))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    nextExpected,
			ExpectedFrame: expectedQueue[nextExpected],
			ActualFrame:   "(stream ended - no more frames)",
			Reason:        fmt.Sprintf("expected %d frames but only received %d", len(expectedQueue), nextExpected),
		}
		return false, strings.Join(actualQueue, "\n"), mismatchInfo, nil
	}

	return true, strings.Join(actualQueue, "\n"), nil, nil
}

func compareBinaryStream(expectedResp models.HTTPResp, stream io.Reader, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	readToEnd := provenswitch.On()
	expectedBytes := expectedBinaryBytes(expectedResp)
	if len(expectedBytes) == 0 {
		// No byte: an empty stream's. The answer's first byte ends the
		// read and fails it under either switch (the size differs), and
		// its end with none passes: readToEnd changes nothing here.
		if err := streamRecordedWithNoFrame(expectedResp); err != nil {
			return false, "", nil, err
		}
	}
	expectedSize := len(expectedBytes)
	actualSize := 0
	contentMatch := true
	mismatchOffset := -1
	buffer := make([]byte, 32*1024)
	// cut: the stall rule cut the stream after bytes that differ came.
	cut := false

	for {
		n, err := stream.Read(buffer)
		if n > 0 {
			if contentMatch && actualSize < expectedSize {
				end := actualSize + n
				if end > expectedSize {
					end = expectedSize
				}
				if !bytes.Equal(buffer[:end-actualSize], expectedBytes[actualSize:end]) {
					contentMatch = false
					mismatchOffset = actualSize
				}
			}
			actualSize += n
			if readToEnd && actualSize > expectedSize {
				// Bytes beyond the recording: the stream differs, whether
				// or not it goes on, so it is not read further.
				break
			}
			if actualSize >= expectedSize && !readToEnd {
				if actualSize > expectedSize {
					logger.Debug("received additional binary stream data after expected size was matched; closing stream capture",
						zap.Int("expected_size", expectedSize),
						zap.Int("actual_size", actualSize))
				}
				break
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			var st *AnswerStallError
			if errors.As(err, &st) && actualSize == 0 {
				// Cut by the stall rule with nothing of the stream
				// come: nothing to compare.
				return false, "0", nil, st
			}
			if errors.As(err, &st) && contentMatch {
				// Cut by the stall rule, every byte that came matching.
				return true, strconv.Itoa(actualSize), nil, st.asStream(fmt.Sprintf("%d of the %d recorded bytes came", actualSize, expectedSize))
			}
			if errors.As(err, &st) {
				// What came differs: the comparison says so below, by
				// its content (the cut, not the app, set its size).
				cut = true
				break
			}
			return false, strconv.Itoa(actualSize), nil, err
		}
	}

	if actualSize != expectedSize && !cut {
		logger.Debug("binary stream size mismatch",
			zap.Int("expected_size", expectedSize),
			zap.Int("actual_size", actualSize))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    0,
			ExpectedFrame: fmt.Sprintf("%d bytes", expectedSize),
			ActualFrame:   fmt.Sprintf("%d bytes", actualSize),
			Reason:        fmt.Sprintf("size mismatch: expected %d bytes, got %d bytes", expectedSize, actualSize),
		}
		return false, strconv.Itoa(actualSize), mismatchInfo, nil
	}

	if !contentMatch {
		logger.Debug("binary stream content mismatch",
			zap.Int("size", actualSize),
			zap.Int("first_mismatch_offset", mismatchOffset))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    0,
			ExpectedFrame: fmt.Sprintf("%d bytes", expectedSize),
			ActualFrame:   fmt.Sprintf("%d bytes (content differs starting near byte %d)", actualSize, mismatchOffset),
			Reason:        fmt.Sprintf("content mismatch starting near byte %d of %d bytes", mismatchOffset, actualSize),
		}
		return false, strconv.Itoa(actualSize), mismatchInfo, nil
	}

	return true, strconv.Itoa(actualSize), nil, nil
}

func expectedBinaryBytes(expectedResp models.HTTPResp) []byte {
	if len(expectedResp.StreamBody) > 0 {
		var buf bytes.Buffer
		for _, chunk := range expectedResp.StreamBody {
			raw, ok := streamChunkFieldValue(chunk, "raw")
			if ok {
				buf.WriteString(raw)
			}
		}
		if buf.Len() > 0 {
			return buf.Bytes()
		}
	}
	return []byte(expectedResp.Body)
}

// extractExpectedSSEQueue is the recorded SSE events an answer is compared
// with: those its recording kept (StreamBody), or, when it kept none (a test
// case carried as JSON carries its body alone), its recorded body read as the
// answer is read (sseQueueOfBody). One that holds no frame is compared with
// none, and the answer read to its end (streamRecordedWithNoFrame): stopping
// before the answer's first frame, any stream would pass.
func extractExpectedSSEQueue(expectedResp models.HTTPResp) ([]expectedSSEFrame, error) {
	if queue := sseQueueOf(expectedResp.StreamBody); len(queue) > 0 {
		return queue, nil
	}
	return sseQueueOfBody(expectedResp.Body)
}

// sseQueueOfBody is a recorded SSE body's events, read as compareSSEStream
// reads the answer's (sseFrameScanner, sseFrameOf, parseSSEFrame), so an
// event of the recording and the same event of the answer are read alike. A
// body that cannot be read so (a frame over maxStreamTokenSize) is an error,
// never the events before it alone.
func sseQueueOfBody(body string) ([]expectedSSEFrame, error) {
	var queue []expectedSSEFrame
	scanner := sseFrameScanner(strings.NewReader(body))
	for scanner.Scan() {
		if frame, ok := sseFrameOf(scanner.Text()); ok {
			queue = append(queue, expectedSSEFrame{fields: parseSSEFrame(frame), raw: frame})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the recorded SSE body: %w", err)
	}
	return queue, nil
}

// sseFrameScanner reads an SSE stream frame by frame (splitSSEFrames).
func sseFrameScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamTokenSize)
	scanner.Split(splitSSEFrames)
	return scanner
}

// sseFrameOf is a frame sseFrameScanner read, its line endings normalized and
// its edges trimmed; false for a blank one, which carries nothing.
func sseFrameOf(token string) (string, bool) {
	frame := strings.Trim(normalizeLineEndings(token), "\n")
	return frame, strings.TrimSpace(frame) != ""
}

// lineScanner reads a stream of lines (NDJSON, text) line by line.
func lineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamTokenSize)
	return scanner
}

func sseQueueOf(chunks []models.HTTPStreamChunk) []expectedSSEFrame {
	if len(chunks) == 0 {
		return nil
	}
	queue := make([]expectedSSEFrame, 0, len(chunks))
	for _, chunk := range chunks {
		if len(chunk.Data) == 0 {
			continue
		}
		fields := make([]sseField, 0, len(chunk.Data))
		lines := make([]string, 0, len(chunk.Data))
		for _, dataField := range chunk.Data {
			key := strings.TrimSpace(dataField.Key)
			if key == "" {
				continue
			}
			if strings.EqualFold(key, "comment") {
				fields = append(fields, sseField{
					key:      ":",
					value:    dataField.Value,
					hasValue: true,
					comment:  true,
				})
				lines = append(lines, ":"+dataField.Value)
				continue
			}

			lowerKey := strings.ToLower(key)
			fields = append(fields, sseField{
				key:      lowerKey,
				value:    dataField.Value,
				hasValue: true,
			})
			lines = append(lines, lowerKey+":"+dataField.Value)
		}
		if len(fields) == 0 {
			continue
		}
		queue = append(queue, expectedSSEFrame{
			fields: fields,
			raw:    strings.Join(lines, "\n"),
		})
	}
	return queue
}

// extractExpectedRawQueue is the recorded lines (NDJSON, a text stream) an
// answer is compared with: those its recording kept (StreamBody), or, when it
// kept none, its recorded body read as the answer is read (rawQueueOfBody),
// as extractExpectedSSEQueue does for SSE.
func extractExpectedRawQueue(expectedResp models.HTTPResp, canonicalizer func(string) string, ignoreEmpty bool) ([]string, error) {
	if queue := rawQueueOf(expectedResp.StreamBody, canonicalizer, ignoreEmpty); len(queue) > 0 {
		return queue, nil
	}
	return rawQueueOfBody(expectedResp.Body, canonicalizer, ignoreEmpty)
}

// rawQueueOfBody is a recorded body's lines, read as compareNDJSONStream and
// comparePlainTextStream read the answer's (lineScanner): each canonicalized,
// a blank one skipped when ignoreEmpty (NDJSON's keepalives). A body that
// cannot be read so (a line over maxStreamTokenSize) is an error, never the
// lines before it alone.
func rawQueueOfBody(body string, canonicalizer func(string) string, ignoreEmpty bool) ([]string, error) {
	var queue []string
	scanner := lineScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := canonicalizer(scanner.Text())
		if ignoreEmpty && line == "" {
			continue
		}
		queue = append(queue, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the recorded body by its lines: %w", err)
	}
	return queue, nil
}

func rawQueueOf(chunks []models.HTTPStreamChunk, canonicalizer func(string) string, ignoreEmpty bool) []string {
	if len(chunks) == 0 {
		return nil
	}
	queue := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		raw, ok := streamChunkFieldValue(chunk, "raw")
		if !ok {
			continue
		}
		raw = canonicalizer(raw)
		if ignoreEmpty && raw == "" {
			continue
		}
		queue = append(queue, raw)
	}
	return queue
}

func streamChunkFieldValue(chunk models.HTTPStreamChunk, key string) (string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, field := range chunk.Data {
		if strings.ToLower(strings.TrimSpace(field.Key)) == key {
			return field.Value, true
		}
	}
	return "", false
}

type sseField struct {
	key      string
	value    string
	hasValue bool
	comment  bool
}

func parseSSEFrame(frame string) []sseField {
	frame = normalizeLineEndings(frame)
	frame = strings.Trim(frame, "\n")
	if strings.TrimSpace(frame) == "" {
		return nil
	}

	lines := strings.Split(frame, "\n")
	fields := make([]sseField, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		if strings.HasPrefix(line, ":") {
			fields = append(fields, sseField{
				key:      ":",
				value:    strings.TrimPrefix(line, ":"),
				hasValue: true,
				comment:  true,
			})
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			fields = append(fields, sseField{
				key:      strings.ToLower(strings.TrimSpace(line)),
				value:    "",
				hasValue: false,
			})
			continue
		}

		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		fields = append(fields, sseField{
			key:      strings.ToLower(strings.TrimSpace(key)),
			value:    value,
			hasValue: true,
		})
	}
	return fields
}

func compareSSEFrame(expectedFrame, actualFrame string, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string) {
	return compareSSEFields(parseSSEFrame(expectedFrame), parseSSEFrame(actualFrame), jsonNoiseKeys, logger)
}

func compareSSEFields(expectedFields, actualFields []sseField, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string) {
	expectedFields = mergeConsecutiveSSEDataFields(expectedFields)
	actualFields = mergeConsecutiveSSEDataFields(actualFields)

	if len(expectedFields) != len(actualFields) {
		return false, "field-count mismatch"
	}

	for i := range expectedFields {
		e := expectedFields[i]
		a := actualFields[i]

		if e.comment {
			if !a.comment {
				return false, "comment-position mismatch"
			}
			if len(strings.TrimSpace(e.value)) != len(strings.TrimSpace(a.value)) {
				logger.Debug("SSE comment size differs (ignored)",
					zap.Int("frame_field_index", i),
					zap.Int("expected_size", len(strings.TrimSpace(e.value))),
					zap.Int("actual_size", len(strings.TrimSpace(a.value))))
			}
			continue
		}

		if e.key != a.key {
			return false, "field-key-order mismatch"
		}

		if e.hasValue != a.hasValue {
			return false, "field-value-presence mismatch"
		}

		if !e.hasValue {
			continue
		}

		if e.key == "data" {
			eVal := strings.TrimSpace(e.value)
			aVal := strings.TrimSpace(a.value)
			expJSON := json.Valid([]byte(eVal))
			actJSON := json.Valid([]byte(aVal))
			if expJSON || actJSON {
				if !(expJSON && actJSON) {
					return false, "data-json-type mismatch"
				}
				ok, err := compareJSONTextWithNoise(eVal, aVal, jsonNoiseKeys)
				if err != nil {
					return false, "data-json-parse-error"
				}
				if !ok {
					return false, "data-json-mismatch"
				}
				continue
			}

			if eVal != aVal {
				return false, "data-value mismatch"
			}
			continue
		}

		if strings.TrimSpace(e.value) != strings.TrimSpace(a.value) {
			return false, "field-value mismatch"
		}
	}

	return true, ""
}

func mergeConsecutiveSSEDataFields(fields []sseField) []sseField {
	if len(fields) == 0 {
		return fields
	}

	merged := make([]sseField, 0, len(fields))
	for _, field := range fields {
		if !field.comment && strings.EqualFold(field.key, "data") {
			current := field
			if !current.hasValue {
				current.hasValue = true
				current.value = ""
			}

			if len(merged) > 0 {
				last := &merged[len(merged)-1]
				if !last.comment && strings.EqualFold(last.key, "data") && last.hasValue {
					last.value = last.value + "\n" + current.value
					continue
				}
			}
			merged = append(merged, current)
			continue
		}

		merged = append(merged, field)
	}

	return merged
}

func compareJSONTextWithNoise(expected, actual string, jsonNoiseKeys map[string]struct{}) (bool, error) {
	var exp any
	var act any

	if err := json.Unmarshal([]byte(expected), &exp); err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(actual), &act); err != nil {
		return false, err
	}

	exp = removeGlobalNoiseKeys(exp, jsonNoiseKeys)
	act = removeGlobalNoiseKeys(act, jsonNoiseKeys)

	return reflect.DeepEqual(exp, act), nil
}

func removeGlobalNoiseKeys(node any, jsonNoiseKeys map[string]struct{}) any {
	switch v := node.(type) {
	case map[string]any:
		filtered := make(map[string]any, len(v))
		for k, child := range v {
			lk := strings.ToLower(strings.TrimSpace(k))
			if _, ignored := jsonNoiseKeys[lk]; ignored {
				continue
			}
			filtered[k] = removeGlobalNoiseKeys(child, jsonNoiseKeys)
		}
		return filtered
	case []any:
		arr := make([]any, 0, len(v))
		for _, child := range v {
			arr = append(arr, removeGlobalNoiseKeys(child, jsonNoiseKeys))
		}
		return arr
	default:
		return node
	}
}

func compareMultipartStream(expectedResp models.HTTPResp, stream io.Reader, boundary string, jsonNoiseKeys map[string]struct{}, logger *zap.Logger) (bool, string, *StreamMismatchInfo, error) {
	readToEnd := provenswitch.On()
	if strings.TrimSpace(boundary) == "" {
		return false, "", nil, fmt.Errorf("missing multipart boundary for stream comparison")
	}

	expectedBody := expectedResp.Body
	// The recording is read by its own boundary, the answer by its own: a
	// multipart writer picks a new boundary for each response (Go's does),
	// and read by the answer's, the recorded body held no part to compare.
	expectedBoundary := boundaryFromContentTypeHeader(getHeaderValueCaseInsensitive(expectedResp.Header, "Content-Type"))
	if expectedBoundary == "" {
		expectedBoundary = boundary
	}
	expectedQueue, err := parseMultipartQueue(strings.NewReader(expectedBody), expectedBoundary)
	if err != nil {
		return false, "", nil, fmt.Errorf("failed to read the recorded multipart body by its boundary %q: %w", expectedBoundary, err)
	}
	if len(expectedQueue) == 0 {
		// No part: an empty stream's, or a body of none.
		if err := streamRecordedWithNoFrame(expectedResp); err != nil {
			return false, "", nil, err
		}
		readToEnd = true
	}

	actualQueue := make([]string, 0, len(expectedQueue))
	nextExpected := 0
	answer := &byteCount{r: stream}
	reader := multipart.NewReader(answer, boundary)

	for {
		part, partErr := reader.NextPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil && answer.n == 0 && errors.Is(partErr, io.EOF) {
			// An answer that ended before its first byte holds no
			// part: multipart.Reader reads it as an error ("NextPart:
			// EOF"), not as the end of a stream of none.
			break
		}
		if partErr != nil {
			return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), nil, streamCut(partErr, nextExpected, len(expectedQueue), "part")
		}

		actualPart, partReadErr := readMultipartPart(part)
		_ = part.Close()
		if partReadErr != nil {
			return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), nil, streamCut(partReadErr, nextExpected, len(expectedQueue), "part")
		}

		if nextExpected >= len(expectedQueue) {
			if readToEnd {
				actualQueue = append(actualQueue, actualPart.describe())
				return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), trailingFrame(len(expectedQueue), actualPart.describe()), nil
			}
			logger.Debug("received additional multipart stream data after expected stream was fully matched; closing stream capture",
				zap.Int("expected_parts", len(expectedQueue)))
			break
		}

		expected := expectedQueue[nextExpected]
		actualQueue = append(actualQueue, actualPart.describe())
		ok, reason := compareMultipartPart(expected, actualPart, jsonNoiseKeys)
		if !ok {
			logger.Debug("multipart stream mismatch",
				zap.Int("part_index", nextExpected),
				zap.String("reason", reason),
				zap.String("expected_part", expected.describe()),
				zap.String("actual_part", actualPart.describe()))
			mismatchInfo := &StreamMismatchInfo{
				FrameIndex:    nextExpected,
				ExpectedFrame: expected.describe(),
				ActualFrame:   actualPart.describe(),
				Reason:        reason,
			}
			return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), mismatchInfo, nil
		}

		nextExpected++
		if nextExpected == len(expectedQueue) && !readToEnd {
			logger.Debug("all expected multipart parts matched; closing stream capture early to avoid waiting for extra stream parts",
				zap.Int("matched_parts", nextExpected))
			break
		}
	}

	if nextExpected < len(expectedQueue) {
		logger.Debug("multipart stream ended before all expected parts were received",
			zap.Int("expected_parts", len(expectedQueue)),
			zap.Int("matched_parts", nextExpected))
		mismatchInfo := &StreamMismatchInfo{
			FrameIndex:    nextExpected,
			ExpectedFrame: expectedQueue[nextExpected].describe(),
			ActualFrame:   "(stream ended - no more parts)",
			Reason:        fmt.Sprintf("expected %d parts but only received %d", len(expectedQueue), nextExpected),
		}
		return false, strings.Join(actualQueue, "\n\n--PART--\n\n"), mismatchInfo, nil
	}

	return true, strings.Join(actualQueue, "\n\n--PART--\n\n"), nil, nil
}

// byteCount counts the bytes read through it.
type byteCount struct {
	r io.Reader
	n int64
}

func (c *byteCount) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type multipartPartPayload struct {
	contentType string
	body        []byte
}

func (m multipartPartPayload) describe() string {
	return fmt.Sprintf("content-type:%s size:%d", m.contentType, len(m.body))
}

func parseMultipartQueue(reader io.Reader, boundary string) ([]multipartPartPayload, error) {
	if strings.TrimSpace(boundary) == "" {
		return nil, fmt.Errorf("multipart boundary is empty")
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	queue, parseErr := parseMultipartQueueBytes(data, boundary)
	if parseErr == nil {
		return queue, nil
	}

	closingBoundary := []byte("--" + boundary + "--")
	if !bytes.Contains(data, closingBoundary) {
		patchedData := append([]byte{}, data...)
		patchedData = append(patchedData, []byte("\r\n--"+boundary+"--\r\n")...)
		if patchedQueue, patchedErr := parseMultipartQueueBytes(patchedData, boundary); patchedErr == nil {
			return patchedQueue, nil
		}
	}

	return nil, parseErr
}

func parseMultipartQueueBytes(data []byte, boundary string) ([]multipartPartPayload, error) {
	mr := multipart.NewReader(bytes.NewReader(data), boundary)
	queue := make([]multipartPartPayload, 0)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		payload, readErr := readMultipartPart(part)
		_ = part.Close()
		if readErr != nil {
			return nil, readErr
		}
		if payload.contentType == "" && len(payload.body) == 0 {
			continue
		}
		queue = append(queue, payload)
	}

	return queue, nil
}

func readMultipartPart(part *multipart.Part) (multipartPartPayload, error) {
	bodyBytes, err := io.ReadAll(part)
	if err != nil {
		return multipartPartPayload{}, err
	}

	contentType := strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Type")))
	if parsedType, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = strings.ToLower(strings.TrimSpace(parsedType))
	}

	if isJSONContentType(contentType) {
		bodyBytes = []byte(strings.TrimSpace(string(bodyBytes)))
	} else if strings.HasPrefix(contentType, "text/") || contentType == "application/xml" || contentType == "application/x-ndjson" || contentType == "application/ndjson" {
		normalized := normalizeLineEndings(string(bodyBytes))
		normalized = strings.TrimSuffix(normalized, "\n")
		bodyBytes = []byte(normalized)
	}

	return multipartPartPayload{
		contentType: contentType,
		body:        append([]byte(nil), bodyBytes...),
	}, nil
}

func compareMultipartPart(expected multipartPartPayload, actual multipartPartPayload, jsonNoiseKeys map[string]struct{}) (bool, string) {
	if expected.contentType != actual.contentType {
		return false, "content-type mismatch"
	}

	if isJSONContentType(expected.contentType) {
		ok, err := compareJSONTextWithNoise(strings.TrimSpace(string(expected.body)), strings.TrimSpace(string(actual.body)), jsonNoiseKeys)
		if err != nil {
			return false, err.Error()
		}
		if !ok {
			return false, "json body mismatch"
		}
		return true, ""
	}

	if !bytes.Equal(expected.body, actual.body) {
		return false, "body mismatch"
	}
	return true, ""
}

func isJSONContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return contentType == "application/json" || strings.HasSuffix(contentType, "+json") || contentType == "application/x-ndjson" || contentType == "application/ndjson"
}

func canonicalizeNDJSONLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}

	var parsed interface{}
	if err := json.NewDecoder(strings.NewReader(line)).Decode(&parsed); err == nil {
		if marshaled, err := json.Marshal(parsed); err == nil {
			return string(marshaled)
		}
	}

	return line
}

func canonicalizePlainTextLine(line string) string {
	return strings.TrimRight(normalizeLineEndings(line), "\n")
}

func splitSSEFrames(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	doubleLFIdx := bytes.Index(data, []byte("\n\n"))
	doubleCRLFIdx := bytes.Index(data, []byte("\r\n\r\n"))

	switch {
	case doubleLFIdx == -1 && doubleCRLFIdx == -1:
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	case doubleLFIdx == -1:
		return doubleCRLFIdx + len("\r\n\r\n"), data[:doubleCRLFIdx], nil
	case doubleCRLFIdx == -1:
		return doubleLFIdx + len("\n\n"), data[:doubleLFIdx], nil
	case doubleLFIdx < doubleCRLFIdx:
		return doubleLFIdx + len("\n\n"), data[:doubleLFIdx], nil
	default:
		return doubleCRLFIdx + len("\r\n\r\n"), data[:doubleCRLFIdx], nil
	}
}

func canonicalizeSSEFrame(frame string) string {
	frame = normalizeLineEndings(frame)
	frame = strings.Trim(frame, "\n")
	if strings.TrimSpace(frame) == "" {
		return ""
	}

	lines := strings.Split(frame, "\n")
	canonicalLines := make([]string, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		if strings.HasPrefix(line, ":") {
			comment := strings.TrimSpace(strings.TrimPrefix(line, ":"))
			canonicalLines = append(canonicalLines, ":"+comment)
			continue
		}

		key, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			canonicalLines = append(canonicalLines, strings.ToLower(strings.TrimSpace(line)))
			continue
		}

		key = strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}

		switch key {
		case "data":
			value = canonicalizeSSEDataValue(value)
		case "event", "id", "retry":
			value = strings.TrimSpace(value)
		default:
			value = strings.TrimSpace(value)
		}

		canonicalLines = append(canonicalLines, key+":"+value)
	}

	return strings.Join(canonicalLines, "\n")
}

func canonicalizeSSEDataValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	var parsed interface{}
	if json.Unmarshal([]byte(trimmed), &parsed) == nil {
		marshaled, err := json.Marshal(parsed)
		if err == nil {
			return string(marshaled)
		}
	}

	if strings.Contains(trimmed, ";base64,") {
		prefix, encoded, ok := strings.Cut(trimmed, ",")
		if ok {
			if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
				return prefix + "," + base64.StdEncoding.EncodeToString(decoded)
			}
			if decoded, err := base64.RawStdEncoding.DecodeString(encoded); err == nil {
				return prefix + "," + base64.StdEncoding.EncodeToString(decoded)
			}
		}
	}

	return trimmed
}

func normalizeLineEndings(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return value
}

func looksBinary(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

func ParseHTTPRequest(requestBytes []byte) (*http.Request, error) {
	// Parse the request using the http.ReadRequest function
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBytes)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Host", request.Host)

	return request, nil
}

func ParseHTTPResponse(data []byte, request *http.Request) (*http.Response, error) {
	buffer := bytes.NewBuffer(data)
	reader := bufio.NewReader(buffer)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func MakeCurlCommand(tc models.HTTPReq) string {
	curl := fmt.Sprintf("curl --request %s \\\n", string(tc.Method))
	curl = curl + fmt.Sprintf("  --url %s \\\n", tc.URL)
	for k, lines := range ToWireHTTPHeader(tc.Header, tc.HeaderLineLengths) {
		if k == "Content-Length" {
			continue
		}
		for _, v := range lines {
			curl = curl + fmt.Sprintf("  --header '%s: %s' \\\n", k, v)
		}
	}
	if len(tc.Form) > 0 {
		for _, form := range tc.Form {
			key := form.Key
			if len(form.Values) == 0 {
				continue
			}
			value := form.Values[0]
			curl = curl + fmt.Sprintf("  --form '%s=%s' \\\n", key, value)
		}
	} else if tc.Body != "" {
		curl = curl + fmt.Sprintf("  --data %s", strconv.Quote(tc.Body))
	}
	return curl
}

func ReadSessionIndices(path string, Logger *zap.Logger) ([]string, error) {
	indices := []string{}

	dir, err := os.OpenFile(path, os.O_RDONLY, fs.FileMode(os.O_RDONLY))
	if err != nil {
		Logger.Debug("creating a folder for the keploy  generated testcases", zap.Error(err))
		return indices, err
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			Logger.Debug("failed to close directory", zap.Error(closeErr))
		}
	}()

	files, err := dir.ReadDir(0)
	if err != nil {
		Logger.Debug("failed to read directory contents", zap.Error(err))
		return indices, err
	}

	for _, v := range files {
		if v.Name() != "reports" && v.IsDir() {
			indices = append(indices, v.Name())
		}
	}

	return indices, nil
}

func NextID(IDs []string, identifier string) string {
	latestIndx := 0
	for _, ID := range IDs {
		Indx, ok := parseIDIndex(ID, identifier)
		if !ok {
			continue
		}
		if latestIndx < Indx+1 {
			latestIndx = Indx + 1
		}
	}
	return fmt.Sprintf("%s%v", identifier, latestIndx)
}

func LastID(IDs []string, identifier string) string {
	latestIndx := 0
	for _, ID := range IDs {
		Indx, ok := parseIDIndex(ID, identifier)
		if !ok {
			continue
		}
		if latestIndx < Indx {
			latestIndx = Indx
		}
	}
	return fmt.Sprintf("%s%v", identifier, latestIndx)
}

func parseIDIndex(ID, identifier string) (int, bool) {
	if !strings.HasPrefix(ID, identifier) {
		return 0, false
	}
	suffix := strings.TrimPrefix(ID, identifier)
	// Digits only. strconv.Atoi accepts a leading sign, so without this a
	// directory literally named "test-set--1" would parse as index -1 and a
	// "test-set-+5" as 5. Neither is an ID keploy generates, and the old
	// len(Split(ID, "-")) == 3 check rejected both.
	if suffix == "" || strings.ContainsFunc(suffix, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, false
	}
	Indx, err := strconv.Atoi(suffix)
	if err != nil {
		return 0, false
	}
	return Indx, true
}

var (
	dateFormats = []string{
		time.Layout,
		time.ANSIC,
		time.UnixDate,
		time.RubyDate,
		time.RFC822,
		time.RFC822Z,
		time.RFC850,
		time.RFC1123,
		time.RFC1123Z,
		time.RFC3339,
		time.RFC3339Nano,
		time.Kitchen,
		time.Stamp,
		time.StampMilli,
		time.StampMicro,
		time.StampNano,
		time.DateTime,
		time.DateOnly,
		time.TimeOnly,
	}
)

// ExtractPort extracts the port from a given URL string, defaulting to 80 if no port is specified.
func ExtractPort(rawURL string) (uint32, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}

	host := parsedURL.Host
	if strings.Contains(host, ":") {
		// Split the host by ":" and return the port part
		parts := strings.Split(host, ":")
		port, err := strconv.ParseUint(parts[len(parts)-1], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("invalid port in URL: %s", rawURL)
		}
		return uint32(port), nil
	}

	// Default ports based on scheme
	switch parsedURL.Scheme {
	case "https":
		return 443, nil
	default:
		return 80, nil
	}
}

func ExtractHostAndPort(curlCmd string) (string, string, error) {
	// Split the command string to find the URL
	parts := strings.Split(curlCmd, " ")
	for _, part := range parts {
		if strings.HasPrefix(part, "http") {
			u, err := url.Parse(part)
			if err != nil {
				return "", "", err
			}
			host := u.Hostname()
			port := u.Port()
			if port == "" {
				if u.Scheme == "https" {
					port = "443"
				} else {
					port = "80"
				}
			}
			return host, port, nil
		}
	}
	return "", "", fmt.Errorf("no URL found in CURL command")
}

func WaitForPort(ctx context.Context, host string, port string, timeout time.Duration) error {
	addr := net.JoinHostPort(host, port)
	// Probe once up front so an already-listening port returns without paying
	// the ticker's first-tick (1s) latency below — this is what makes the
	// readiness gates truly instant for an app that is already accepting.
	if conn, err := net.DialTimeout("tcp", addr, 800*time.Millisecond); err == nil {
		_ = conn.Close()
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
			if err == nil {
				err := conn.Close()
				if err != nil {
					return err
				}
				return nil
			}
		case <-timer.C:
			msg := "Please add delay if your application takes more time to start"
			return fmt.Errorf("timeout after %v waiting for port %s:%s, %s", timeout, host, port, msg)
		}
	}
}

// ErrAgentNotReady is the error of an agent start that stopped making progress
// before the agent reported healthy (pkg/platform/http, agent_boot_watch.go):
// one keploy started itself (Setup), or one docker compose started
// (WatchAgentStart). No budget of wall time ends a start that makes progress:
// under heavy CI docker-daemon contention the agent container can take
// minutes just to start, and a fixed budget (KEPLOY_AGENT_READY_TIMEOUT, 330 s,
// since removed) tore down bring-ups that would have succeeded. It is a
// distinct sentinel so callers can retry a fresh bring-up on this — a
// nondeterministic container-runtime stall, observed intermittently on macOS
// Docker Desktop — WITHOUT retrying deterministic failures like missing kernel
// privileges or a bad config.
var ErrAgentNotReady = errors.New("keploy-agent did not become ready in time")

// AgentStartWatcher is implemented by an instrumentation that can watch the
// start of the agent it set up (pkg/platform/http's AgentClient): a wait for an
// agent it did not start itself (docker compose starts it as a service of the
// project) follows the start's progress, not a clock. WatchAgentStart sends one
// error once the start has failed (the agent says so) or has stopped making
// progress (ErrAgentNotReady), and nothing while it makes progress, or while
// its process does not run yet; ctx ends it.
type AgentStartWatcher interface {
	WatchAgentStart(ctx context.Context) <-chan error
}

// WatchAgentStart is inst's watch of its agent's start (AgentStartWatcher), or
// nil, which never sends, for an instrumentation that cannot watch one.
func WatchAgentStart(ctx context.Context, inst any) <-chan error {
	if w, ok := inst.(AgentStartWatcher); ok {
		return w.WatchAgentStart(ctx)
	}
	return nil
}

// agentSetupAttempts bounds how many times a stalled agent bring-up is retried
// with a fresh agent before giving up. Three attempts takes a ~37%-per-attempt
// intermittent stall (observed on macOS Docker Desktop) to ~5% while adding no
// delay to the overwhelmingly common first-try success.
const agentSetupAttempts = 3

// RetryAgentSetup runs an agent Setup and retries it, up to agentSetupAttempts,
// ONLY when it fails with ErrAgentNotReady. Each retry is a full fresh bring-up
// (Setup re-draws ports and regenerates the agent container), which is what
// clears a nondeterministic container-runtime stall. Deterministic failures
// (missing privileges, bad config) do not match the sentinel and return at
// once, and a cancelled context stops the loop immediately. The test assertions
// downstream still run exactly once, against a healthy agent — this retries the
// agent infrastructure, never a test.
func RetryAgentSetup(ctx context.Context, logger *zap.Logger, setup func(ctx context.Context, attempt int) error) error {
	var err error
	for attempt := 1; attempt <= agentSetupAttempts; attempt++ {
		err = setup(ctx, attempt)
		if err == nil || ctx.Err() != nil || !errors.Is(err, ErrAgentNotReady) {
			return err
		}
		if attempt < agentSetupAttempts {
			logger.Warn("keploy-agent bring-up stalled; retrying with a fresh agent",
				zap.Int("attempt", attempt), zap.Int("of", agentSetupAttempts), zap.Error(err))
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return err
}

// agentHealthFirstWait is how long the first agent health check waits for its
// answer. An agent on the same host answers in well under a millisecond, and one
// that is not up yet refuses the connection at once, so this keeps the local case
// as quick as it always was.
//
// agentHealthMaxWait bounds the wait a run of slow answers can grow to (see
// nextAgentHealthWait); the caller's context still bounds the whole wait.
const (
	agentHealthFirstWait = 500 * time.Millisecond
	agentHealthMaxWait   = 30 * time.Second
)

// nextAgentHealthWait is how long the next agent health check waits: twice the
// last wait when that check ran out of time, up to agentHealthMaxWait, and the
// same wait otherwise. Only a check that timed out says the answer needs longer
// to arrive than it was given; one refused at once (nothing listening yet) or
// answered says nothing about the path, so it leaves the wait alone.
//
// A fixed wait cannot fit every path to the agent. Reached through a Kubernetes
// port-forward, each new connection costs two stream set-ups before the request
// is sent, so a check takes about three round trips: 735ms over a 245ms link.
// A fixed 500ms cut every check off and the agent was never found ready.
func nextAgentHealthWait(wait time.Duration, timedOut bool) time.Duration {
	if !timedOut {
		return wait
	}
	if wait >= agentHealthMaxWait/2 {
		return agentHealthMaxWait
	}
	return 2 * wait
}

// AgentHealthTicker checks the agent's health endpoint every checkInterval
// until an answer finds it healthy, sends true on agentReadyCh, and returns. It
// returns when ctx ends too, and closes agentReadyCh either way, so a receive
// that gets false means ctx ended first.
//
// Each check waits agentHealthFirstWait for its answer to begin with, and twice
// as long after a check that ran out of time (nextAgentHealthWait), so an agent
// behind a slow path is found ready instead of every check being cut off.
//
// When an agent first becomes healthy it also kicks off verifyControlPlaneGuarded
// for it, off this goroutine and after the readiness signal, so the check never
// spends the caller's readiness budget. That check can log at error level, which
// keploy's CI treats as fatal.
func AgentHealthTicker(ctx context.Context, logger *zap.Logger, agentURI string, agentReadyCh chan<- bool, checkInterval time.Duration) {
	defer close(agentReadyCh)
	agentHealthTicker(ctx, logger, agentURI, checkInterval, func(http.Header) bool {
		select {
		case agentReadyCh <- true:
			return true
		case <-ctx.Done():
			return false
		}
	})
}

// AgentHealthTickerWithAnswer is AgentHealthTicker that sends, in place of
// true, the headers of the answer that found the agent healthy (never nil),
// so the caller reads what that same answer said, with no second request that
// an agent which has since stopped answering, or a budget that has since run
// out, could fail. A receive that gets nil means ctx ended first.
func AgentHealthTickerWithAnswer(ctx context.Context, logger *zap.Logger, agentURI string, answers chan<- http.Header, checkInterval time.Duration) {
	defer close(answers)
	agentHealthTicker(ctx, logger, agentURI, checkInterval, func(header http.Header) bool {
		select {
		case answers <- header:
			return true
		case <-ctx.Done():
			return false
		}
	})
}

// agentHealthTicker is the loop of the two above: it checks the agent's health
// every checkInterval until an answer finds it healthy, hands that answer's
// headers to ready, which reports whether it delivered them (false when ctx
// ended first), and returns; or it returns when ctx ends.
func agentHealthTicker(ctx context.Context, logger *zap.Logger, agentURI string, checkInterval time.Duration, ready func(http.Header) bool) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	// No client-wide timeout: each check waits as long as `wait` (below).
	client := &http.Client{}
	wait := agentHealthFirstWait

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, wait)
			header := agentHealthAnswer(checkCtx, logger, client, agentURI)
			timedOut := header == nil && errors.Is(checkCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
			cancel()
			if next := nextAgentHealthWait(wait, timedOut); next != wait {
				logger.Debug("agent health check ran out of time; the next one waits longer",
					zap.String("agentURI", agentURI), zap.Duration("waited", wait), zap.Duration("next_wait", next))
				wait = next
			}
			if header == nil {
				continue
			}
			if ready(header) {
				// Only now, and off this goroutine. The check is a
				// diagnostic: every caller races this signal against a
				// readiness deadline, and spending any of that budget on it
				// could fail the user's run with "did not become ready"
				// against an agent that had just answered its health check.
				// Detached from ctx for the same reason it runs at all --
				// the caller cancels this context as soon as it has its
				// answer, which would cancel the check before it could
				// report anything.
				go verifyControlPlaneGuarded(context.WithoutCancel(ctx), logger, agentURI)
			}
			return
		}
	}
}

// agentHealthAnswer checks whether the agent is running and healthy by calling
// its /agent/health endpoint, and returns the headers of a healthy (200)
// answer, never nil, or nil when there was none: no answer, or an answer
// other than 200, whose headers say nothing about the agent.
func agentHealthAnswer(ctx context.Context, logger *zap.Logger, client *http.Client, agentURI string) http.Header {
	healthURL := fmt.Sprintf("%s/health", agentURI)
	logger.Debug("Checking agent health", zap.String("url", healthURL))

	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		logger.Debug("Failed to create health check request", zap.Error(err))
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Debug("Failed to read agent health response body", zap.Error(err))
		return nil
	}
	logger.Debug("Agent health check response", zap.Int("status_code", resp.StatusCode), zap.String("body", string(body)))

	if resp.StatusCode != http.StatusOK {
		return nil
	}
	if resp.Header == nil {
		// A RoundTripper other than net/http's may leave it nil; healthy is
		// never nil.
		return http.Header{}
	}
	return resp.Header
}

// TestSetMocksReader is an optional extension of the mock store a replay, the
// runner or the mock service loads a test set's mocks from: both of its pools,
// the session pool before the mapping prune, and the per-test candidates
// before the prune and the window filter, from ONE read of the set's mock file
// (models.TestSetMocks). A store without it is asked for each pool
// separately, and *mockdb.MockYaml reads and decodes the whole file for each.
type TestSetMocksReader interface {
	GetTestSetMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) (models.TestSetMocks, error)
}

// ConsumedScopeReader is an optional extension of the instrumentation a replay
// or a runner talks to the agent through: whether the agent has said it reads
// the consumed-mock history only for the per-test mocks it stages
// (models.ConsumedScopeHeader), so filter params may carry just their entries
// (ConsumedForAgent). Without it the whole history is sent.
type ConsumedScopeReader interface {
	AgentReadsConsumedPerTestOnly() bool
}

// PerTestRegion is the set of names of the per-test mocks a test set hands the
// agent: the filtered half of its StoreMocks.
func PerTestRegion(filtered []*models.Mock) map[string]struct{} {
	region := make(map[string]struct{}, len(filtered))
	for _, m := range filtered {
		if m != nil {
			region[m.Name] = struct{}{}
		}
	}
	return region
}

// ConsumedForAgent narrows a test set's consumed-mock history to the entries
// the agent can read: those of the per-test mocks in region.
//
// The agent consults MockFilterParams.TotalConsumedMocks in one place,
// filterOutDeleted, and only for the per-test mocks it is about to stage, every
// one of which comes from the per-test half of the set's StoreMocks (resident,
// parked on disk, or carried over). An entry for any other mock, such as each
// session, connection or config mock the replay served, is never looked up.
// Sending those entries anyway re-encoded the whole history for every test, so
// the payload grew with each test; in a replay whose data mocks are reusable
// (lax-mode MySQL) all of it was dead weight. The agent's verdict is the same
// with the narrowed map. Only send it to an agent that says it reads the
// history that way (models.ConsumedScopeHeader): agents from v3.0.0-beta1
// through v3.3.22 also applied it to the session pool.
//
// A nil region means the per-test half is not known: total is returned as is.
func ConsumedForAgent(total map[string]models.MockState, region map[string]struct{}) map[string]models.MockState {
	if region == nil || total == nil {
		return total
	}
	out := make(map[string]models.MockState, min(len(total), len(region)))
	if len(region) < len(total) {
		for name := range region {
			if st, ok := total[name]; ok {
				out[name] = st
			}
		}
		return out
	}
	for name, st := range total {
		if _, ok := region[name]; ok {
			out[name] = st
		}
	}
	return out
}

// FilterTcsMocks applies the per-test time-window filter to candidate
// mocks. Pass strict=true for Option-1 containment (out-of-window
// non-config mocks are dropped instead of promoted to the cross-test
// config pool); pass strict=false for legacy Option-2 behaviour. Note
// that Go has no default parameter values — callers resolve the
// effective strict value from config.Test.StrictMockWindow (ships
// true by default) combined with the KEPLOY_STRICT_MOCK_WINDOW env
// override, via strictWindowEnabled. This function only sees the
// resolved strict flag.
func FilterTcsMocks(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool) []*models.Mock {
	filteredMocks, _ := filterByTimeStamp(ctx, logger, m, afterTime, beforeTime, strict)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	return filteredMocks
}

// FilterPerTestAndLaxPromoted splits per-test candidate mocks into two
// slices using filterByTimeStamp:
//
//   - perTestInWindow  — mocks whose request-timestamp is inside
//     [afterTime, beforeTime]. These go into the per-test pool for
//     the current test.
//   - promotedToSession — mocks that filterByTimeStamp's lax branch
//     moved to its "unfiltered" return slice because they are out-of-
//     window (but neither strict-dropped nor invalid-order dropped).
//     They represent the pre-Phase-2 kind-fallback's "reuse across
//     tests" semantic — fixture queries recorded in test 1 that
//     every subsequent test legitimately re-issues.
//
// Under strict mode the second slice is always empty (strict drops
// rather than promotes). Under lax mode callers should append the
// promoted slice to the session pool passed to the proxy so cross-
// test fixture reuse keeps working for suites that relied on the
// old kind-fallback's implicit promotion.
//
// Both slices are sorted by request-timestamp for deterministic
// matcher ordering.
func FilterPerTestAndLaxPromoted(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool) ([]*models.Mock, []*models.Mock) {
	return FilterPerTestAndLaxPromotedTierAware(ctx, logger, m, afterTime, beforeTime, strict, time.Time{})
}

// FilterPerTestAndLaxPromotedTierAware is the tier-aware variant of
// FilterPerTestAndLaxPromoted. It accepts firstWindowStart (the earliest
// test window start observed by the agent's MockManager, or zero before
// any real test has fired) so the strict gate can preserve per-test
// startup-init mocks (req < firstWindowStart) in the returned perTestIn
// slice instead of dropping them. The preservation runs in BOTH strict and
// lax mode: agentStrict is false on a WindowedProxy, so a strict-only
// preservation never runs on a windowed replay. MockManager.SetMocksWithWindow's
// startup-tier partition then routes those preserved mocks into the
// dedicated startup tree, and the v3 tier-aware dispatcher reaches them
// via GetStartupMocks.
//
// Passing firstWindowStart == time.Time{} reproduces the legacy blanket-
// drop contract (every per-test mock outside the current window is dropped
// in strict mode / promoted in lax, regardless of whether the mock predates
// the first test), because there is no cutoff to preserve against. Legacy callers that don't know firstWindowStart can keep
// calling FilterPerTestAndLaxPromoted unchanged.
func FilterPerTestAndLaxPromotedTierAware(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool, firstWindowStart time.Time) ([]*models.Mock, []*models.Mock) {
	return FilterPerTestAndLaxPromotedInFlightAware(ctx, logger, m, afterTime, beforeTime, strict, firstWindowStart, time.Time{}, false)
}

// FilterPerTestAndLaxPromotedInFlightAware is FilterPerTestAndLaxPromotedTierAware
// that also keeps in the per-test slice the per-test mocks in flight when the
// window began (sent after inFlightFrom, the end of the gap before it, and
// answered at or after afterTime); see filterByTimeStampInFlightAware.
// strictWindow is the user's window mode (MockFilterParams.StrictMockWindow),
// which decides that, whatever strict says. A zero inFlightFrom keeps none.
func FilterPerTestAndLaxPromotedInFlightAware(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool, firstWindowStart, inFlightFrom time.Time, strictWindow bool) ([]*models.Mock, []*models.Mock) {
	perTestInWindow, promotedToSession := filterByTimeStampInFlightAware(ctx, logger, m, afterTime, beforeTime, strict, firstWindowStart, inFlightFrom, strictWindow)

	sort.SliceStable(perTestInWindow, func(i, j int) bool {
		return perTestInWindow[i].Spec.ReqTimestampMock.Before(perTestInWindow[j].Spec.ReqTimestampMock)
	})
	sort.SliceStable(promotedToSession, func(i, j int) bool {
		return promotedToSession[i].Spec.ReqTimestampMock.Before(promotedToSession[j].Spec.ReqTimestampMock)
	})

	return perTestInWindow, promotedToSession
}

// FilterConfigMocks applies the per-test time-window filter to the config
// mock pool. Pass strict=true for Option-1 containment (out-of-window
// non-config mocks are dropped); pass strict=false for legacy Option-2.
// See FilterTcsMocks for the env / config precedence that determines
// the runtime-effective value.
func FilterConfigMocks(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool) []*models.Mock {
	return FilterConfigMocksTierAware(ctx, logger, m, afterTime, beforeTime, strict, time.Time{})
}

// FilterConfigMocksTierAware is the tier-aware variant of
// FilterConfigMocks. Identical to FilterConfigMocks except it threads
// firstWindowStart through to filterByTimeStampTierAware. Used by the
// agent's UpdateMockParams so startup-init entries in the unfiltered
// (config) pool are kept in the filtered return slice (via the
// tier-aware strict branch) and reach MockManager.SetMocksWithWindow
// where the startup-tier partition sorts them correctly.
func FilterConfigMocksTierAware(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strict bool, firstWindowStart time.Time) []*models.Mock {
	filteredMocks, unfilteredMocks := filterByTimeStampTierAware(ctx, logger, m, afterTime, beforeTime, strict, firstWindowStart)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	sort.SliceStable(unfilteredMocks, func(i, j int) bool {
		return unfilteredMocks[i].Spec.ReqTimestampMock.Before(unfilteredMocks[j].Spec.ReqTimestampMock)
	})

	return append(filteredMocks, unfilteredMocks...)
}

func FilterTcsMocksMapping(ctx context.Context, logger *zap.Logger, m []*models.Mock, mocksPresentInMapping []string) []*models.Mock {
	filteredMocks, _ := filterByMapping(ctx, logger, m, mocksPresentInMapping)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	return filteredMocks
}

// IsTestWindow reports whether [afterTime, beforeTime] is a test's window: a
// staging call's starts at models.BaseTime (the set's, a scope's, an exact
// staging's), and a call with no window has a zero bound. The mock manager
// reads its start so (SetMocksWithWindow).
func IsTestWindow(afterTime, beforeTime time.Time) bool {
	return !afterTime.IsZero() && !afterTime.Equal(models.BaseTime) && !beforeTime.IsZero()
}

// FilterTcsMocksMappingInWindow is FilterTcsMocksMapping for the test window
// [afterTime, beforeTime]: beside the per-test mocks the mapping names, the
// ones placed at their answer (models.IsAnswerPlaced) whose place the window
// holds (models.PlaceOf), whatever the mapping lists. A mapping lists what a
// test consumed; one an older keploy wrote, or a --sync recording's, lists a
// consumer's poll under the test it was sent in, by request time, and the
// mock manager's staging keeps a mock placed at its answer only in the
// window that holds its answer: named for the test before, it would be
// staged by no test. With no test's window (IsTestWindow: a staging call's
// names the test's whole recording, ExactMapping), or no kind placed at its
// answer, it is FilterTcsMocksMapping.
func FilterTcsMocksMappingInWindow(ctx context.Context, logger *zap.Logger, m []*models.Mock, mocksPresentInMapping []string, afterTime, beforeTime time.Time) []*models.Mock {
	if !models.AnswerPlacedRegistered() || !IsTestWindow(afterTime, beforeTime) {
		return FilterTcsMocksMapping(ctx, logger, m, mocksPresentInMapping)
	}
	names := append([]string(nil), mocksPresentInMapping...)
	for _, mk := range m {
		if mk == nil || !models.IsAnswerPlaced(mk) {
			continue
		}
		if place := models.PlaceOf(mk); !place.Before(afterTime) && !place.After(beforeTime) {
			names = append(names, mk.Name)
		}
	}
	return FilterTcsMocksMapping(ctx, logger, m, names)
}

// FilterConfigMocksMapping tags the reusable (config/session/connection) pool
// with its mapping membership and returns it in RECORDED order.
//
// Mapping membership is not a chronological signal and must not reorder this
// pool. Its members are reusable tier — none is owned by a single test — so
// membership only records which tests happened to consume a mock, and says
// nothing about when it was recorded. Partitioning on it and concatenating
// (mapped first, then unmapped) puts every consumed entry ahead of every
// unconsumed one, inverting chronology whenever the two interleave: a pool
// recorded rev1,rev2,rev3,rev4 with rev3,rev4 consumed came back as
// rev3,rev4,rev1,rev2.
//
// That matters because downstream reads this pool as a SEQUENCE, not a set. The
// slice order becomes TestModeInfo.SortOrder in MockManager.buildTier,
// which keys the RB-tree that GetUnFilteredMocksByKind walks in order — so a
// replayer that walks a recorded revision sequence (a cluster-config poll, a
// bootstrap handshake) is handed it backwards.
//
// The tagging is done IN PLACE over m rather than by partitioning, so entries
// with equal ReqTimestampMock keep their recorded order under the stable sort.
// Partition-then-sort left ties in mapped-first order — the very inversion this
// removes — and ties are not rare: a protocol encoder that drains several frames
// from one TCP read stamps them all with that chunk's timestamp (see
// mysql/recorder/record_v2.go, where ReqTimestampMock is the ReadAt of the chunk
// that delivered the command bytes), which is exactly what a coalesced bootstrap
// burst looks like. In a legacy pool carrying no timestamps at all the whole
// slice is one tie, so the sort does nothing and the partition alone decided the
// order — that is where the inversion was total, not absent.
//
// This deliberately does NOT try to reproduce FilterConfigMocksTierAware's
// output. That path additionally applies window semantics the mapping path has
// no equivalent of — it hoists a mock with a missing timestamp into its filtered
// pool and drops one whose response predates its request — so the two agree only
// on a pool where neither rule fires. The property claimed here is narrower and
// is the one that matters: mapping membership is not a chronological signal, so
// it must not reorder this pool.
func FilterConfigMocksMapping(_ context.Context, logger *zap.Logger, m []*models.Mock, mocksPresentInMapping []string) []*models.Mock {
	mapping := make(map[string]bool, len(mocksPresentInMapping))
	for _, name := range mocksPresentInMapping {
		mapping[name] = true
	}

	isNonKeploy := false
	pool := make([]*models.Mock, 0, len(m))
	for _, mock := range m {
		if mock == nil {
			continue
		}
		p := mock.DeepCopy()
		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}
		p.TestModeInfo.IsFiltered = mapping[p.Name]
		pool = append(pool, p)
	}
	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
	}

	sort.SliceStable(pool, func(i, j int) bool {
		return pool[i].Spec.ReqTimestampMock.Before(pool[j].Spec.ReqTimestampMock)
	})

	return pool
}

// strictWindowEnvOverride holds the result of one-time env-var parsing
// at process start. Env var KEPLOY_STRICT_MOCK_WINDOW=1|true|yes|on
// enables strict containment process-wide, overriding the per-call
// flag plumbed via MockFilterParams (both routes OR together).
// KEPLOY_STRICT_MOCK_WINDOW=0|false|no|off explicitly disables it,
// overriding the per-call flag too — this is the escape hatch for
// users who need to turn off strict mode after the Phase 2.5 default
// flip (see config.Test.StrictMockWindow in config.go) without
// editing keploy.yaml.
//
// Evaluated ONCE at package init — changing the env var later in the
// same process has no effect. Tests that need to exercise the env
// path must set the variable before the binary starts; in-process
// tests should drive the per-call bool instead, which is both
// simpler and deterministic.
var strictWindowEnvOverride = func() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_STRICT_MOCK_WINDOW")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}()

// strictWindowEnvExplicitOff reports whether the env var was set to a
// disabling value. Used so KEPLOY_STRICT_MOCK_WINDOW=0 can force
// strict off even when the per-call flag says true or the config
// default has flipped.
var strictWindowEnvExplicitOff = func() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_STRICT_MOCK_WINDOW")))
	switch v {
	case "0", "false", "no", "off":
		return true
	}
	return false
}()

// IsStrictMockWindow reports whether strict mock-window containment is
// effectively enabled — either via the per-call flag (from
// config.Test.StrictMockWindow / MockFilterParams.StrictMockWindow) or the
// process-wide KEPLOY_STRICT_MOCK_WINDOW env override. Exposed so the
// agent's UpdateMockParams can log the effective state (covering both
// opt-in routes) without duplicating the env-parsing logic.
func IsStrictMockWindow(perCall bool) bool {
	return strictWindowEnabled(perCall)
}

// strictWindowEnabled returns true when strict containment should apply.
//
// Strict mode ships ON by default (config/default.go
// StrictMockWindow: true) now that every stateful-protocol recorder
// classifies mocks finely enough for legitimate cross-test sharing
// to be encoded as session/connection lifetime. Opting out is
// available via config.Test.StrictMockWindow: false or the
// KEPLOY_STRICT_MOCK_WINDOW=0 env override for older recordings
// that still rely on the legacy lax behaviour.
//
// Precedence implemented here (perCall is the already-resolved
// effective call-site decision — config.Test.StrictMockWindow has
// already been OR-ed in by the caller):
//   - If the env var is explicitly set to a disabling value
//     (KEPLOY_STRICT_MOCK_WINDOW=0), strict is DISABLED unconditionally.
//     This wins over the per-call/config decision so users who hit
//     problems with new recordings can opt out instantly without
//     editing config or redeploying callers.
//   - Otherwise, strict follows perCall; an enabling env var can also
//     force it on even if perCall was false.
//
// In practice, opting out of strict mode is done either by:
//   - setting KEPLOY_STRICT_MOCK_WINDOW=0 in env, or
//   - setting test.strictMockWindow: false in keploy.yaml (which
//     flows through perCall).
//
// When strict:
//   - per-test (LifetimePerTest) mocks must have their request
//     timestamp inside the outer test's [req,res] window
//   - out-of-window per-test mocks are DROPPED instead of promoted to
//     the cross-test pool (stops the cross-test bleed bug)
//   - session (LifetimeSession) and connection (LifetimeConnection)
//     mocks are NEVER dropped — their visibility is bounded by
//     test-set / connID lifetime rather than the per-test window.
//
// Lax (opt-out) preserves pre-unification behaviour: out-of-window
// per-test mocks remain visible via the unfiltered pool so
// scoring-based matchers can still pick them up.
func strictWindowEnabled(perCall bool) bool {
	// Explicit env-var disable wins over everything else — escape
	// hatch for users whose recordings predate full Phase 2
	// classification coverage.
	if strictWindowEnvExplicitOff {
		return false
	}
	return perCall || strictWindowEnvOverride
}

func filterByTimeStamp(_ context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strictPerCall bool) ([]*models.Mock, []*models.Mock) {
	return filterByTimeStampTierAware(nil, logger, m, afterTime, beforeTime, strictPerCall, time.Time{})
}

// filterByTimeStampTierAware is the tier-aware form of filterByTimeStamp.
// It accepts firstWindowStart — the earliest test window start the agent's
// MockManager has observed — so the strict gate can distinguish the two
// out-of-window classes a per-test mock can fall into:
//
//   - Startup-init (req < firstWindowStart): genuine app-bootstrap traffic
//     recorded before any test fired (Flyway migrations, Hibernate init,
//     HikariCP pool warm-up, listmonk's cacheUsers SELECT). Under the
//     legacy blanket-drop semantics these were incorrectly treated as
//     bleed; under tier-aware semantics they are PRESERVED in the
//     filtered slice so MockManager.SetMocksWithWindow can route them
//     into the startup tree (its own scan for req < firstStart picks
//     them up and rebuilds the startup tier — see pkg/agent/proxy/
//     mockmanager.go's SetMocksWithWindow).
//
//   - Stale previous-test (firstWindowStart <= req < afterTime, OR
//     req > beforeTime): real cross-test bleed. These are DROPPED under
//     strict mode, same as before — this is the containment guarantee
//     strictMockWindow exists to provide.
//
// Session- and connection-scoped mocks are kept unconditionally (they
// span tests by definition; their visibility is bounded by test-set /
// connID lifetime, not the per-test window). Mocks with zero or
// inverted-order timestamps get the same defensive handling as the
// legacy path.
//
// Authoritative-routing contract (mockdb/filter-layer owns attribution):
// the filter routes each mock using ONLY two signals — the derived
// Lifetime enum and the request-timestamp. Lifetime-first: Session and
// Connection mocks unconditionally land in the unfiltered (session)
// pool; PerTest mocks consult the window. The recorder-stamped
// Metadata["scope"] field is informational on the wire and is NOT
// consulted by this function (nor by any pool-routing site in the
// filter/mockmanager layer). A perTest-lifetime mock whose recorder
// stamped scope=session but whose request-timestamp falls inside the
// current [afterTime, beforeTime] window lands in the per-test pool —
// timestamp attribution is the source of truth. This resolves the
// "27 perTest mocks with scope=session dropped into session pool"
// symptom the sap-demo-java debug bundle captured.
//
// Passing firstWindowStart == time.Time{} restores the legacy blanket-
// drop behaviour (no startup-tier preservation). Callers that can
// supply a real cutoff — principally the agent, via the
// FirstWindowStartReader optional extension on *Proxy — should thread
// it through. Third-party Proxy implementations that don't implement
// the reader fall through to the zero-time behaviour and remain
// functionally identical to the pre-fix blanket-drop contract.
func filterByTimeStampTierAware(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strictPerCall bool, firstWindowStart time.Time) ([]*models.Mock, []*models.Mock) {
	return filterByTimeStampInFlightAware(ctx, logger, m, afterTime, beforeTime, strictPerCall, firstWindowStart, time.Time{}, false)
}

// filterByTimeStampInFlightAware is filterByTimeStampTierAware that also keeps
// in the per-test slice the per-test mocks in flight when the window began
// (models.InFlightEligible): sent after inFlightFrom, the end of the gap
// before the window (models.WindowSchedule.GapBefore), before afterTime, and
// answered at or after it (models.SentInGapAnsweredBy). They are the window's
// test's, as the mock manager's own window filter holds them.
//
// The user's window mode decides (strictWindow, MockFilterParams.
// StrictMockWindow, through strictWindowEnabled), not strictPerCall: on a
// windowed proxy the agent's own filter is never strict (agentStrict), and
// the mock manager does the strict filtering, from what this slice hands it.
// With the strict window (the default) a mock in flight is kept per-test,
// stated lifetime or not, as the window's own mocks are. In lax mode
// (strictMockWindow false, KEPLOY_STRICT_MOCK_WINDOW=0) one whose recorder
// stated its lifetime is kept, since lax mode drops a stated mock out of its
// window, and an unstated one is promoted to the session pool as every
// unstated per-test mock out of the window is, as before. A zero inFlightFrom
// keeps none.
func filterByTimeStampInFlightAware(_ context.Context, logger *zap.Logger, m []*models.Mock, afterTime time.Time, beforeTime time.Time, strictPerCall bool, firstWindowStart, inFlightFrom time.Time, strictWindow bool) ([]*models.Mock, []*models.Mock) {

	filteredMocks := make([]*models.Mock, 0)
	unfilteredMocks := make([]*models.Mock, 0)

	if afterTime.Equal(time.Time{}) {
		return m, unfilteredMocks
	}

	if beforeTime.Equal(time.Time{}) {
		return m, unfilteredMocks
	}

	strict := strictWindowEnabled(strictPerCall)
	isNonKeploy := false
	var droppedOutOfWindow, droppedInvalidOrder, preservedStartup, droppedWindowBound int

	for _, mock := range m {
		if mock == nil {
			continue
		}
		// doing deep copy to prevent data race, which was happening due to the write to isFiltered
		// field in this for loop, and write in mockmanager functions.
		p := mock.DeepCopy()
		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}
		// A window-bound mock (a connection failure) is served from its own
		// window or the startup band and nowhere else, in both modes — see
		// placeWindowBound. Routed before every rule below, each of which can
		// otherwise hand it to every test: the untimed branch, the session
		// short-circuit, the config/connection tag fallback, lax promotion.
		if models.WindowBound(p.Kind) {
			if placeWindowBound(p, afterTime, beforeTime, firstWindowStart) == windowBoundDropped {
				droppedWindowBound++
				continue
			}
			p.TestModeInfo.Lifetime = models.LifetimePerTest
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			continue
		}
		if p.Spec.ReqTimestampMock.Equal(time.Time{}) || p.Spec.ResTimestampMock.Equal(time.Time{}) {
			logger.Debug("request or response timestamp of mock is missing",
				zap.String("mock", p.Name), zap.Bool("strict", strict))
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			continue
		}

		// Lifetime-first routing (mockdb/filter-layer authoritative
		// attribution design): Session- and Connection-lifetime mocks
		// belong in the session/unfiltered pool REGARDLESS of whether
		// their request-timestamp happens to fall inside the current
		// test window. Semantics: these are cross-test reusable by
		// contract — a BEGIN / SET / SHOW / handshake probe that was
		// captured during test-N's window is still a session-lifetime
		// probe; routing it into test-N's per-test pool would make it
		// invisible to every subsequent test (per-test pools are
		// single-test-scoped). This is load-bearing for the user's
		// "route by lifetime + reqTimestampMock, ignore metadata.scope"
		// design — a perTest-lifetime mock stamped scope=session by the
		// recorder (scope is informational-only on the wire) but whose
		// req-timestamp sits inside test-N's window still falls through
		// to the timestamp-based in-window check below and lands in
		// test-N's per-test pool. The metadata.scope field is NOT read
		// for pool routing anywhere in the filter/mockmanager layer;
		// scope and lifetime may legitimately disagree on the wire when
		// the recorder's context-probe captured a session-scope fire
		// inside a live test window.
		if p.TestModeInfo.Lifetime == models.LifetimeSession ||
			p.TestModeInfo.Lifetime == models.LifetimeConnection {
			p.TestModeInfo.IsFiltered = false
			unfilteredMocks = append(unfilteredMocks, p)
			continue
		}
		// NOTE: this runs BELOW the session/connection short-circuit above,
		// deliberately. It is a window-ordering sanity check, and only per-test
		// mocks are window-routed — a session or config mock is never compared
		// against a test window, so a skewed pair cannot mis-route it. Running
		// it above the short-circuit dropped clock-skewed session mocks (an
		// auth or handshake recording) out of EVERY pool, which is a hard miss
		// on traffic that had a perfectly good mock. mockmanager's own
		// equivalent check is per-test-scoped for the same reason.
		// Defensive sanity check: if the response-timestamp is BEFORE the
		// request-timestamp the recording is inconsistent (clock skew,
		// serialisation bug, or file corruption). Skip it — keeping such
		// a mock in either pool risks confusing downstream scoring.
		if LeaveOutAnsweredBeforeAsked(logger, p) {
			droppedInvalidOrder++
			continue
		}
		// Defensive fallback: DeriveLifetime may not have run on mocks
		// constructed inline by tests or produced by legacy ingest
		// paths. Fall back to the raw Metadata["type"] tag so a
		// config/connection-tagged mock still routes to the session
		// pool even when the cached Lifetime field is the zero value
		// (LifetimePerTest). metadata.scope is deliberately NOT
		// consulted here.
		if p.Spec.Metadata != nil && !p.StatesLifetime() &&
			(p.Spec.Metadata["type"] == "config" || p.Spec.Metadata["type"] == "connection") {
			if p.Spec.Metadata["type"] == "config" {
				p.TestModeInfo.Lifetime = models.LifetimeSession
			} else {
				p.TestModeInfo.Lifetime = models.LifetimeConnection
			}
			p.TestModeInfo.IsFiltered = false
			unfilteredMocks = append(unfilteredMocks, p)
			continue
		}

		// In-window = placed inside [afterTime, beforeTime] (models.PlaceOf).
		// The response may straggle past beforeTime — DB/HTTP downstream
		// responses routinely complete a few micro-to-milliseconds after
		// the outer test response is captured. The cross-test bleed bug
		// strict mode protects against is caused by out-of-window REQUESTS
		// (a different test's data mock getting reused here), not by
		// in-flight responses landing late. Checking req is sufficient
		// in both modes — the strict-vs-lax difference is solely in the
		// out-of-window handling below (strict drops, lax promotes to
		// the cross-test config pool).
		//
		// Only per-test-lifetime mocks reach this predicate — session/
		// connection-lifetime mocks short-circuited above to the session
		// pool. A perTest-lifetime mock placed inside
		// [afterTime, beforeTime] lands in the per-test pool; outside
		// the window it falls through to the strict/lax out-of-window
		// handling below.
		// A mock is in the window its place is in (models.PlaceOf): its
		// request time, or its answer's for a mock placed at its answer.
		place := models.PlaceOf(p)
		inWindow := !place.Before(afterTime) && !place.After(beforeTime)
		if inWindow {
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			continue
		}

		// Out-of-window handling for per-test-lifetime mocks only.
		// Session/connection-lifetime mocks were already routed into
		// the unfiltered pool by the lifetime-first short-circuit
		// above, so the strict/lax divergence here applies exclusively
		// to the perTest class that actually needs window containment.
		// Startup-init (req < firstWindowStart) is preserved in the filtered
		// slice so MockManager.SetMocksWithWindow's startup-tier partition
		// routes it into the startup tree, where the tier-aware dispatcher
		// reaches it via GetStartupMocks.
		//
		// This runs in BOTH modes, deliberately. It used to sit inside the
		// strict branch below, and `agentStrict` is permanently false on a
		// WindowedProxy (agent.go: `params.StrictMockWindow && !isWindowedProxy`),
		// so on every windowed replay the lax branch diverted these mocks into
		// the session/unfiltered pool instead — a pool the mongo tier-aware
		// path never consults for bootstrap traffic (it reads GetFilteredMocks
		// and GetStartupMocks only). The startup tier was therefore EMPTY for
		// the whole run, and a bootstrap query issued while a window was open
		// missed with candidates:0 even though its mock was on disk.
		//
		// It is not a widening of what may be served: these mocks predate the
		// first test window, so they can only be bootstrap traffic, and routing
		// them to the tier built for exactly that is what both modes intend.
		// A zero firstWindowStart still means "no cutoff yet" and falls through
		// to the legacy behaviour below, unchanged.
		if !firstWindowStart.IsZero() && place.Before(firstWindowStart) {
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			preservedStartup++
			continue
		}

		// A per-test mock whose recorder stated its lifetime is the data of
		// the test it was recorded in, and is served to no other test whatever
		// the window mode: lax promotion exists for recordings whose recorders
		// could not tell data from shared traffic, which this one did. Nor does
		// KEPLOY_STRICT_MOCK_WINDOW=0, that promotion's escape hatch, serve it
		// to another test: an HTTP call every test may make is declared in
		// keploy.yaml (models.SessionCallRule).
		// In flight when the window began: sent in the gap before it and
		// answered at or after its start (a consumer's long poll). The
		// window's test's, as the mock manager holds it, in the user's
		// strict window mode, and for a stated mock in lax mode too, where
		// it would otherwise be dropped. Lax mode promotes an unstated one
		// to the session pool below, as every unstated per-test mock out of
		// the window.
		if !inFlightFrom.IsZero() && (strictWindowEnabled(strictWindow) || p.StatesLifetime()) && models.InFlightEligible(p) &&
			models.SentInGapAnsweredBy(p.Spec.ReqTimestampMock, p.Spec.ResTimestampMock, inFlightFrom, afterTime) {
			p.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, p)
			continue
		}

		if strict || p.StatesLifetime() {
			// Per-test, out-of-window. Genuine stale cross-test bleed
			// (firstWindowStart <= req < afterTime, or req > beforeTime) is
			// dropped — the strictMockWindow guarantee. The startup-init band
			// was already preserved above, in both modes.
			// Per-mock diagnostic: emit the hash + window + actual ts
			// at Debug for the strict-drop path so a CI log can
			// pinpoint which postgres / http / mongo mock the per-test
			// cohort lost. Without this, "candidates: 0" downstream is
			// the only visible signal and operators have no way to
			// confirm whether the mock was on disk at all vs filtered
			// out. Aggregate count is preserved below for parity.
			if logger != nil {
				// Info-level (not Debug) so this surfaces in default CI
				// runs without --debug. Tagged with "diag/" so it is
				// obviously diagnostic. Will be demoted/removed once the
				// strict-window regression on listmonk + pgtype-tour is
				// root-caused. A stated mock dropped outside strict mode is
				// at Debug: every test's staging leaves out every other
				// test's mocks, which is the design, not a regression.
				log := logger.Info
				if !strict {
					log = logger.Debug
				}
				log("diag/strict-mode drop: per-test mock outside window",
					zap.String("mock", p.Name),
					zap.String("kind", string(p.Kind)),
					zap.String("connID", p.ConnectionID),
					zap.Time("mock_req_ts", p.Spec.ReqTimestampMock),
					zap.Time("mock_res_ts", p.Spec.ResTimestampMock),
					zap.Time("window_after", afterTime),
					zap.Time("window_before", beforeTime),
					zap.Duration("delta_before_after", p.Spec.ReqTimestampMock.Sub(afterTime)),
					zap.Duration("delta_before_before", p.Spec.ReqTimestampMock.Sub(beforeTime)),
				)
			}
			droppedOutOfWindow++
		} else {
			// Legacy: anything out-of-window goes to the unfiltered pool
			// (where parsers may still pick it up as a fallback).
			p.TestModeInfo.IsFiltered = false
			unfilteredMocks = append(unfilteredMocks, p)
		}
	}
	if (strict && droppedOutOfWindow > 0) || droppedInvalidOrder > 0 || preservedStartup > 0 || droppedWindowBound > 0 {
		logger.Debug("filterByTimeStamp tier-aware outcome (see separate counts for reasons)",
			zap.Int("dropped_out_of_window", droppedOutOfWindow),
			zap.Int("dropped_invalid_timestamp_order", droppedInvalidOrder),
			zap.Int("preserved_startup_pre_first_window", preservedStartup),
			zap.Int("dropped_window_bound_outside_own_window", droppedWindowBound),
			zap.Bool("strict", strict),
			zap.Time("after", afterTime),
			zap.Time("before", beforeTime),
			zap.Time("firstWindowStart", firstWindowStart))
	}
	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
	}
	return filteredMocks, unfilteredMocks
}

// FilterByTimeStampThreeTier is the three-pool, Lifetime-authoritative
// variant of filterByTimeStampTierAware. It elevates the startup tier
// (LifetimeConnection) to a first-class split at the core filter level
// so integrations that consume the pools (Postgres v3's BuildIndex,
// MySQL's per-connID trees) can read the three tiers as disjoint slices
// without having to re-partition from metadata["type"] themselves.
//
// Return slice contract:
//   - filteredMocks:   LifetimePerTest with request-timestamp inside
//     [afterTime, beforeTime]. These go into the per-test pool for
//     the current test. Under strict mode, out-of-window per-test
//     mocks are DROPPED, except startup-init (req < firstWindowStart)
//     which lands in startupMocks instead (preserving genuine app-
//     bootstrap traffic that fired before any test started).
//   - unfilteredMocks: LifetimeSession. Reusable across every test in
//     the session; never window-filtered. This is the "session" pool
//     in three-tier parlance.
//   - startupMocks:    LifetimeConnection (per-connection reusable,
//     bounded by connID lifetime) + any LifetimePerTest mock whose
//     request-timestamp predates firstWindowStart (legitimate
//     bootstrap traffic under strict mode).
//
// Backward compatibility with unstamped / legacy-tagged mocks:
//  1. metadata["type"]=="config" with no Lifetime → LifetimeSession,
//     routed to unfilteredMocks.
//  2. metadata["type"]=="connection" with no Lifetime → LifetimeConnection,
//     routed to startupMocks.
//  3. No Lifetime, no "type" tag → falls through to per-test window logic.
//
// Integrations that don't emit LifetimeConnection mocks (HTTP, HTTP/2,
// gRPC, generic, Kafka, Redis data-plane, etc.) get an empty startupMocks
// slice — safe to ignore. Integrations that DO care (Postgres v3,
// MySQL's prepared-statement setup) consume it as an authoritative
// "connection-scoped reusable" pool without having to re-derive the
// classification from metadata.
func FilterByTimeStampThreeTier(ctx context.Context, logger *zap.Logger, m []*models.Mock, afterTime, beforeTime time.Time, strictPerCall bool, firstWindowStart time.Time) (filtered, unfiltered, startup []*models.Mock) {
	filtered = make([]*models.Mock, 0)
	unfiltered = make([]*models.Mock, 0)
	startup = make([]*models.Mock, 0)

	if afterTime.Equal(time.Time{}) || beforeTime.Equal(time.Time{}) {
		// No window supplied — fall back to lifetime-only partitioning
		// so callers still get a three-way split. PerTest (and legacy-
		// untagged) mocks return in `filtered` to match the two-slice
		// legacy contract where the first return slice was "every mock
		// with no effective window to apply".
		for _, mk := range m {
			if mk == nil {
				continue
			}
			p := mk.DeepCopy()
			lt := effectiveLifetimeForRouting(p)
			if models.WindowBound(p.Kind) {
				// Never a reusable pool, whatever its tag says (see
				// placeWindowBound). With no window there is nothing to place
				// it in, so it goes where every per-test mock goes.
				lt = models.LifetimePerTest
				p.TestModeInfo.Lifetime = models.LifetimePerTest
			}
			switch lt {
			case models.LifetimeSession:
				p.TestModeInfo.IsFiltered = false
				unfiltered = append(unfiltered, p)
			case models.LifetimeConnection:
				p.TestModeInfo.IsFiltered = false
				startup = append(startup, p)
			default:
				p.TestModeInfo.IsFiltered = true
				filtered = append(filtered, p)
			}
		}
		return filtered, unfiltered, startup
	}

	strict := strictWindowEnabled(strictPerCall)
	var droppedOutOfWindow, droppedInvalidOrder, preservedStartup, droppedWindowBound int
	isNonKeploy := false

	for _, mk := range m {
		if mk == nil {
			continue
		}
		p := mk.DeepCopy()
		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}
		// Window-bound: own window or startup band only, in both modes. Same
		// rule, same reason, as in filterByTimeStampTierAware.
		if models.WindowBound(p.Kind) {
			switch placeWindowBound(p, afterTime, beforeTime, firstWindowStart) {
			case windowBoundInWindow:
				p.TestModeInfo.Lifetime = models.LifetimePerTest
				p.TestModeInfo.IsFiltered = true
				filtered = append(filtered, p)
			case windowBoundStartup:
				p.TestModeInfo.Lifetime = models.LifetimePerTest
				p.TestModeInfo.IsFiltered = true
				startup = append(startup, p)
			default:
				droppedWindowBound++
			}
			continue
		}
		if p.Spec.ReqTimestampMock.Equal(time.Time{}) || p.Spec.ResTimestampMock.Equal(time.Time{}) {
			logger.Debug("request or response timestamp of mock is missing",
				zap.String("mock", p.Name), zap.Bool("strict", strict))
			p.TestModeInfo.IsFiltered = true
			filtered = append(filtered, p)
			continue
		}
		if LeaveOutAnsweredBeforeAsked(logger, p) {
			droppedInvalidOrder++
			continue
		}

		lt := effectiveLifetimeForRouting(p)
		switch lt {
		case models.LifetimeConnection:
			p.TestModeInfo.IsFiltered = false
			p.TestModeInfo.Lifetime = models.LifetimeConnection
			startup = append(startup, p)
			continue
		case models.LifetimeSession:
			p.TestModeInfo.IsFiltered = false
			p.TestModeInfo.Lifetime = models.LifetimeSession
			unfiltered = append(unfiltered, p)
			continue
		}

		// PerTest: window containment by place (models.PlaceOf).
		// A mock is in the window its place is in (models.PlaceOf): its
		// request time, or its answer's for a mock placed at its answer.
		place := models.PlaceOf(p)
		inWindow := !place.Before(afterTime) && !place.After(beforeTime)
		if inWindow {
			p.TestModeInfo.IsFiltered = true
			filtered = append(filtered, p)
			continue
		}

		// Startup-init band, preserved in BOTH modes. Identical reasoning to
		// filterByTimeStampTierAware: agentStrict is false on a WindowedProxy, so
		// a strict-only preservation never runs on a windowed replay and the
		// startup tier stays empty for the whole run. This twin is only
		// test-called today, but fixing one of two identical functions would
		// guarantee the bug returns the moment the three-tier path is wired for
		// postgres v3 / mysql.
		if !firstWindowStart.IsZero() && place.Before(firstWindowStart) {
			p.TestModeInfo.IsFiltered = true
			startup = append(startup, p)
			preservedStartup++
			continue
		}

		// Out-of-window per-test: strict drops, lax promotes to unfiltered.
		// The startup-init band was already claimed above, in both modes. A
		// per-test mock whose recorder stated its lifetime is dropped in both,
		// as filterByTimeStampTierAware drops it.
		if strict || p.StatesLifetime() {
			// Per-mock diagnostic: emit hash + window deltas at Debug
			// for the three-tier strict-drop path so a CI log can
			// identify which postgres / mongo / etc. mock the per-test
			// cohort lost. Mirrors the same diagnostic added to the
			// two-tier filterByTimeStampTierAware so callers using
			// either path get the visibility uniformly.
			if logger != nil {
				logger.Info("diag/strict-mode drop (3-tier): per-test mock outside window",
					zap.String("mock", p.Name),
					zap.String("kind", string(p.Kind)),
					zap.String("connID", p.ConnectionID),
					zap.Time("mock_req_ts", p.Spec.ReqTimestampMock),
					zap.Time("mock_res_ts", p.Spec.ResTimestampMock),
					zap.Time("window_after", afterTime),
					zap.Time("window_before", beforeTime),
					zap.Duration("delta_before_after", p.Spec.ReqTimestampMock.Sub(afterTime)),
					zap.Duration("delta_before_before", p.Spec.ReqTimestampMock.Sub(beforeTime)),
				)
			}
			droppedOutOfWindow++
		} else {
			p.TestModeInfo.IsFiltered = false
			unfiltered = append(unfiltered, p)
		}
	}

	if (strict && droppedOutOfWindow > 0) || droppedInvalidOrder > 0 || preservedStartup > 0 || droppedWindowBound > 0 {
		logger.Debug("FilterByTimeStampThreeTier outcome",
			zap.Int("dropped_out_of_window", droppedOutOfWindow),
			zap.Int("dropped_invalid_timestamp_order", droppedInvalidOrder),
			zap.Int("preserved_startup_pre_first_window", preservedStartup),
			zap.Int("dropped_window_bound_outside_own_window", droppedWindowBound),
			zap.Int("perTest", len(filtered)),
			zap.Int("session", len(unfiltered)),
			zap.Int("startup", len(startup)),
			zap.Bool("strict", strict),
			zap.Time("after", afterTime),
			zap.Time("before", beforeTime),
			zap.Time("firstWindowStart", firstWindowStart))
	}
	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
	}
	return filtered, unfiltered, startup
}

// windowBoundPlace is where the filters put a window-bound mock.
type windowBoundPlace int

const (
	windowBoundDropped windowBoundPlace = iota
	windowBoundInWindow
	windowBoundStartup
)

// placeWindowBound places a models.WindowBound mock (a connection failure) for
// the test window [afterTime, beforeTime]: in the window when its request time
// is inside it; in the startup band when it predates firstWindowStart (app
// boot, before any test); dropped in every other case, strict mode or lax.
//
// Lax mode promotes an out-of-window per-test mock to the session pool, which
// every later test reads; a window-bound mock without both timestamps, or with
// its response before its request, cannot be placed in any window at all and
// would otherwise go to the per-test pool of every test. Either way a failure
// recorded in one test would fail connects in others, so neither happens.
func placeWindowBound(p *models.Mock, afterTime, beforeTime, firstWindowStart time.Time) windowBoundPlace {
	req, res := p.Spec.ReqTimestampMock, p.Spec.ResTimestampMock
	if req.IsZero() || res.IsZero() || res.Before(req) {
		return windowBoundDropped
	}
	if !req.Before(afterTime) && !req.After(beforeTime) {
		return windowBoundInWindow
	}
	if !firstWindowStart.IsZero() && req.Before(firstWindowStart) {
		return windowBoundStartup
	}
	return windowBoundDropped
}

// effectiveLifetimeForRouting resolves a mock's Lifetime for
// routing purposes, applying the legacy-tag fallback when the cached
// enum is not explicitly derived. Mirrors the "defensive fallback"
// branch of filterByTimeStampTierAware but returns a value instead of
// mutating the mock — the caller decides whether to stamp the result
// onto the DeepCopy.
//
// Precedence:
//  1. Explicit LifetimeDerived + non-zero Lifetime wins (the recorder
//     stamped it at emit time; honour the authoritative classification).
//  2. LifetimeSession / LifetimeConnection cached wins even without
//     LifetimeDerived (legacy DeriveLifetime paths may have derived
//     the non-zero enum without setting the bool).
//  3. metadata["type"]=="config" → LifetimeSession.
//  4. metadata["type"]=="connection" → LifetimeConnection.
//  5. Anything else → LifetimePerTest (zero value).
//
// Item 3/4 is the backward-compat path for mocks produced before every
// integration stamped Lifetime at emit time, AND for mocks constructed
// inline by tests that set metadata but never called DeriveLifetime.
func effectiveLifetimeForRouting(m *models.Mock) models.Lifetime {
	if m == nil {
		return models.LifetimePerTest
	}
	// Explicit non-zero Lifetime wins regardless of LifetimeDerived.
	// This honours recorder-stamped values AND any DeriveLifetime path
	// that cached a non-zero enum without setting the derived bool.
	if m.TestModeInfo.Lifetime == models.LifetimeSession ||
		m.TestModeInfo.Lifetime == models.LifetimeConnection {
		return m.TestModeInfo.Lifetime
	}
	if m.TestModeInfo.LifetimeDerived {
		return m.TestModeInfo.Lifetime
	}
	// A recorder's statement decides over the type tag, as DeriveLifetime
	// decides it.
	if m.StatesLifetime() {
		c := *m
		c.DeriveLifetime()
		return c.TestModeInfo.Lifetime
	}
	if m.Spec.Metadata != nil {
		switch m.Spec.Metadata["type"] {
		case "config":
			return models.LifetimeSession
		case "connection":
			return models.LifetimeConnection
		}
	}
	return models.LifetimePerTest
}

func filterByMapping(_ context.Context, logger *zap.Logger, m []*models.Mock, mocksPresentInMapping []string) ([]*models.Mock, []*models.Mock) {
	mapping := make(map[string]bool, len(mocksPresentInMapping))
	for _, name := range mocksPresentInMapping {
		mapping[name] = true
	}
	var filtered, unfiltered []*models.Mock
	isNonKeploy := false
	for _, mock := range m {
		if mock == nil {
			continue
		}
		p := mock.DeepCopy()
		if p.Version != "api.keploy.io/v1beta1" && p.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
		}
		if mapping[p.Name] {
			p.TestModeInfo.IsFiltered = true
			filtered = append(filtered, p)
		} else {
			p.TestModeInfo.IsFiltered = false
			unfiltered = append(unfiltered, p)
		}
	}
	if isNonKeploy {
		logger.Debug("Few mocks in the mock File are not recorded by keploy ignoring them")
	}
	return filtered, unfiltered
}

func GuessContentType(data []byte) models.BodyType {
	// Use net/http library's DetectContentType for basic MIME type detection
	mimeType := http.DetectContentType(data)

	// Additional checks to further specify the content type
	switch {
	case IsJSON(data):
		return models.JSON
	case IsXML(data):
		return models.XML
	case strings.Contains(mimeType, "text/html"):
		return models.HTML
	case strings.Contains(mimeType, "text/plain"):
		if IsCSV(data) {
			return models.CSV
		}
		return models.Plain
	}

	return models.UnknownType
}

func IsJSON(body []byte) bool {
	var js interface{}
	return json.Unmarshal(body, &js) == nil
}

func IsXML(data []byte) bool {
	var xm xml.Name
	return xml.Unmarshal(data, &xm) == nil
}

// IsCSV checks if data can be parsed as CSV by looking for common characteristics
func IsCSV(data []byte) bool {
	// Very simple CSV check: look for commas in the first line
	content := string(data)
	if lines := strings.Split(content, "\n"); len(lines) > 0 {
		return strings.Contains(lines[0], ",")
	}
	return false
}

// MaxDecompressedSize caps Decompress on paths with no downstream size limit
// (replay, mock decode, SimulateHTTP) so a decompression bomb errors instead
// of OOMing. The capture path passes the tighter conn.MaxTestCaseSize, since
// larger testcases are dropped anyway. See #3867.
const MaxDecompressedSize = 100 * 1024 * 1024 // 100 MiB

// ErrDecompressedTooLarge reports that a body inflated past the caller's
// limit during Decompress. Callers use errors.Is against it to distinguish
// an oversized (but valid) body from a corrupt stream, which deserve
// different handling (e.g. capture drops oversized bodies with the regular
// size-limit message instead of a scary bomb error).
var ErrDecompressedTooLarge = errors.New("decompressed size exceeds limit")

// NewCappedReader wraps r so cumulative reads past limit fail with an error
// wrapping ErrDecompressedTooLarge instead of streaming unbounded data. It
// bounds streaming decompression (replay streams, mock downloads, archive
// extraction) where readAllCapped's materialize-everything shape doesn't
// fit. A stream of exactly limit bytes still reaches its natural EOF; the
// error surfaces only when the stream holds at least one byte more.
func NewCappedReader(r io.Reader, limit int64) io.Reader {
	return &cappedReader{r: r, limit: limit}
}

type cappedReader struct {
	r     io.Reader
	read  int64
	limit int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.read > c.limit {
		return 0, fmt.Errorf("%w of %d bytes (possible decompression bomb)", ErrDecompressedTooLarge, c.limit)
	}
	// Budget reads to limit+1: an exactly-limit stream hits its natural
	// EOF, while a single extra byte reveals an oversized stream.
	if max := c.limit + 1 - c.read; int64(len(p)) > max {
		p = p[:max]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		return 0, fmt.Errorf("%w of %d bytes (possible decompression bomb)", ErrDecompressedTooLarge, c.limit)
	}
	return n, err
}

// NewCappedReadCloser applies NewCappedReader to a ReadCloser, preserving
// Close so consumers can still release the underlying resource (e.g. an
// HTTP response body) after a capped read.
func NewCappedReadCloser(rc io.ReadCloser, limit int64) io.ReadCloser {
	return &cappedReadCloser{Reader: NewCappedReader(rc, limit), closer: rc}
}

type cappedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (c *cappedReadCloser) Close() error { return c.closer.Close() }

// readAllCapped reads r to EOF, failing with ErrDecompressedTooLarge once
// the stream exceeds limit bytes. Unlike io.ReadAll, buffer capacity is
// never grown past limit+1, so at most limit+1 bytes are read from r and
// the returned slice retains no doubling slack (io.ReadAll can retain ~2x
// the limit near the cap).
func readAllCapped(r io.Reader, limit int64) ([]byte, error) {
	errTooLarge := fmt.Errorf("%w of %d bytes (possible decompression bomb)", ErrDecompressedTooLarge, limit)
	startCap := int64(64 * 1024)
	if startCap > limit+1 {
		startCap = limit + 1
	}
	data := make([]byte, 0, startCap)
	for {
		if len(data) == cap(data) {
			if int64(len(data)) > limit {
				return nil, errTooLarge
			}
			newCap := int64(cap(data)) * 2
			if newCap > limit+1 {
				newCap = limit + 1
			}
			grown := make([]byte, len(data), newCap)
			copy(grown, data)
			data = grown
		}
		n, err := r.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if err == io.EOF {
			if int64(len(data)) > limit {
				return nil, errTooLarge
			}
			return data, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Decompress inflates data per the given Content-Encoding, reading at most
// limit bytes (see MaxDecompressedSize) so a bomb errors instead of OOMing;
// errors.Is(err, ErrDecompressedTooLarge) distinguishes that from a corrupt
// stream. The encoding is matched case-insensitively after trimming —
// Compress uses the same matching, so a recorded body round-trips at replay.
// An unsupported encoding (e.g. zstd, deflate) is returned undecompressed.
func Decompress(logger *zap.Logger, encoding string, data []byte, limit int64) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "br":
		logger.Debug("decompressing brotli compressed data")
		reader := brotli.NewReader(bytes.NewReader(data))
		decodedData, err := readAllCapped(reader, limit)
		if err != nil {
			// Over-limit is reported by the caller (e.g. Capture logs it as
			// the regular size-limit drop) — logging it here too would
			// double-report every oversized body as a decode failure.
			if !errors.Is(err, ErrDecompressedTooLarge) {
				utils.LogError(logger, err, "failed to read the brotli compressed data")
			}
			return nil, err
		}
		return decodedData, nil
	case "gzip":
		logger.Debug("decoding gzip compressed data")
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			utils.LogError(logger, err, "failed to create gzip reader")
			return nil, err
		}
		defer reader.Close()
		decodedData, err := readAllCapped(reader, limit)
		if err != nil {
			// See the brotli branch: over-limit is the caller's to report.
			if !errors.Is(err, ErrDecompressedTooLarge) {
				utils.LogError(logger, err, "failed to read the gzip compressed data")
			}
			return nil, err
		}
		return decodedData, nil
	case "", "identity":
		// No Content-Encoding, or "identity" (RFC 9110 §8.4.1): the body
		// is already uncompressed.
		return data, nil
	default:
		// Common on proxied traffic (zstd, deflate, ...) — debug, not warn,
		// matching the streaming path in SimulateHTTPStreaming.
		logger.Debug("unsupported Content-Encoding; storing body without decompression",
			zap.String("encoding", encoding))
		return data, nil
	}
}

// Compress deflates data per the given Content-Encoding. The encoding is
// matched case-insensitively after trimming — symmetric with Decompress, so
// a body stored decompressed at record time is re-compressed at replay even
// when the recorded header used a non-canonical spelling (e.g. "GZIP").
// An unsupported encoding is returned uncompressed, mirroring Decompress.
func Compress(logger *zap.Logger, encoding string, data []byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		logger.Debug("compressing data using gzip")
		var compressedBuffer bytes.Buffer
		gw := gzip.NewWriter(&compressedBuffer)
		_, err := gw.Write(data)
		if err != nil {
			utils.LogError(logger, err, "failed to write compressed data to gzip writer")
			return nil, err
		}
		err = gw.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close gzip writer")
			return nil, err
		}
		return compressedBuffer.Bytes(), nil
	case "br":
		logger.Debug("compressing data using brotli")
		var compressedBuffer bytes.Buffer
		bw := brotli.NewWriter(&compressedBuffer)
		_, err := bw.Write(data)
		if err != nil {
			utils.LogError(logger, err, "failed to write compressed data to brotli writer")
			return nil, err
		}
		err = bw.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close brotli writer")
			return nil, err
		}
		return compressedBuffer.Bytes(), nil
	}
	return data, nil
}

// LooksLikeJSON checks if a string appears to be JSON by checking for opening and closing brackets/braces.
// It trims whitespace and returns false for empty strings.
func LooksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return (strings.HasPrefix(s, "{") && strings.Contains(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.Contains(s, "]"))
}

// hasExplicitPort checks if the given host string (or host:port)
// has an explicit port defined. It handles IPv6 addresses (e.g. [::1]:8080) correctly.
func hasExplicitPort(hostStr string) bool {
	// Basic check: if no colon, definitely no port (unless it's a bare IPv6, but that doesn't have a port either)
	if !strings.Contains(hostStr, ":") {
		return false
	}

	// Attempt to split host/port
	// net.SplitHostPort handles IPv6 addresses enclosed in brackets [::1]:8080
	_, port, err := net.SplitHostPort(hostStr)
	if err != nil {
		return false
	}

	// If a port is found, verify it is numeric
	if port != "" {
		if _, err := strconv.Atoi(port); err == nil {
			return true
		}
	}

	return false
}

// urlReplacement is the replaceWith URL entry that applies to target: of those
// whose substring target contains, the one with the longest substring (the
// most specific), then the lowest. One entry, chosen the same way wherever it
// is asked: ResolveTestTarget applies it, and PublishedTarget leaves a test
// it sends to a port of its own where it is. (A map's order is random: taking
// the first entry that matched applied a different one from run to run when
// several did.)
func urlReplacement(target string, urlReplacements map[string]string) (substr, replacement string, ok bool) {
	for s, r := range urlReplacements {
		if !strings.Contains(target, s) {
			continue
		}
		if !ok || len(s) > len(substr) || (len(s) == len(substr) && s < substr) {
			substr, replacement, ok = s, r, true
		}
	}
	return substr, replacement, ok
}

// ResolveTestTarget determines the final target (URL for HTTP, Authority for gRPC)
// by applying replacement logic and precedence rules.
//
// Priority logic (lowest → highest):
//  1. AppPort (recorded port).
//  2. ConfigPort (top-level port/grpcPort/ssePort combined with protocol overrides).
//  3. replaceWith URL replacements – if replacement has explicit port, skip steps 1-2.
//  4. replaceWith port mappings – always applied last, overrides everything.
func ResolveTestTarget(originalTarget string, urlReplacements map[string]string, portMappings map[uint32]uint32, configHost string, appPort uint16, configPort uint32, isHTTP bool, logger *zap.Logger) (string, error) {
	finalTarget := originalTarget
	replacementHasPort := false
	replacementMatched := false

	// Step 1: Check replaceWith URL
	if substr, replacement, ok := urlReplacement(finalTarget, urlReplacements); ok {
		replacementMatched = true
		finalTarget = strings.Replace(finalTarget, substr, replacement, 1)
		// Whether the replacement value explicitly defines a port.
		replacementHasPort = replacementNamesPort(replacement, isHTTP)
		logger.Debug("Applied replaceWith substitution",
			zap.String("find", substr),
			zap.String("replace", replacement),
			zap.String("result_target", finalTarget),
			zap.Bool("replacement_has_port", replacementHasPort))
	}

	// If URL replacement set a port and there are no port mappings, we are done
	if replacementHasPort && len(portMappings) == 0 {
		return finalTarget, nil
	}

	// Step 2: ConfigHost override (only if no URL replacement match)
	if !replacementMatched && !replacementHasPort && configHost != "" {
		var err error
		if isHTTP {
			finalTarget, err = utils.ReplaceHost(finalTarget, configHost)
		} else {
			finalTarget, err = utils.ReplaceGrpcHost(finalTarget, configHost)
		}
		if err != nil {
			utils.LogError(logger, err, "failed to replace host with config host")
			return "", err
		}
		logger.Debug("Replaced host with config host", zap.String("host", configHost), zap.String("target", finalTarget))
	}

	// Parse host and port from finalTarget
	var host string
	var port string
	var scheme string // only for HTTP

	if isHTTP {
		parsedURL, parseErr := url.Parse(finalTarget)
		if parseErr != nil {
			utils.LogError(logger, parseErr, "failed to parse test case URL")
			return "", parseErr
		}
		host = parsedURL.Hostname()
		port = parsedURL.Port()
		scheme = parsedURL.Scheme
		if port == "" {
			if scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
			logger.Debug("URL has no explicit port, using scheme default",
				zap.String("target", finalTarget), zap.String("default_port", port))
		}
	} else {
		host = finalTarget
		if colonIdx := strings.LastIndex(finalTarget, ":"); colonIdx != -1 {
			h, p, err := net.SplitHostPort(finalTarget)
			if err == nil {
				host = h
				port = p
			}
		}
		if port == "" {
			logger.Debug("Authority has no explicit port, using gRPC default",
				zap.String("target", finalTarget), zap.String("default_port", "443"))
			port = "443"
		}
	}

	// Steps 3-4: AppPort/ConfigPort (only if URL replacement didn't set port)
	if !replacementHasPort {
		if appPort > 0 {
			port = fmt.Sprintf("%d", appPort)
			logger.Debug("Overriding port with app_port",
				zap.Uint16("app_port", appPort))
		}

		if configPort > 0 {
			if appPort > 0 && uint32(appPort) != configPort {
				logger.Debug("Config port overrides recorded app_port",
					zap.Uint32("config_port", configPort),
					zap.Uint16("recorded_app_port", appPort))
			}
			port = fmt.Sprintf("%d", configPort)
		}
	}

	// Step 5: Port mappings (highest priority, always checked)
	if len(portMappings) > 0 {
		currentPort, convErr := strconv.ParseUint(port, 10, 32)
		if convErr == nil {
			if mappedPort, ok := portMappings[uint32(currentPort)]; ok {
				logger.Debug("Applied replaceWith port mapping",
					zap.Uint32("from", uint32(currentPort)),
					zap.Uint32("to", mappedPort))
				port = fmt.Sprintf("%d", mappedPort)
			}
		}
	}

	// Reassemble
	if isHTTP {
		u, _ := url.Parse(finalTarget)
		u.Host = net.JoinHostPort(host, port)
		finalTarget = u.String()
	} else {
		finalTarget = net.JoinHostPort(host, port)
	}

	logger.Debug("Final resolved target", zap.String("target", finalTarget))
	return finalTarget, nil
}

// controlPlaneProbed records the agents that have already given a conclusive
// answer to the check below, keyed by the launch that started each one.
//
// Per agent, not per process: a docker-compose `keploy test` restarts the agent
// for each test-set. A process-wide latch would check the first agent and none
// of the others, and a handoff that breaks on a later agent and not the first
// would go unseen. Per launch rather than per address, because those agents
// share one: the port is fixed in the compose file generated once for the
// session, so keyed by address alone each later agent would inherit its
// predecessor's answer and never be checked. The app records a launch every
// time it runs compose (App.withAgentToken in pkg/client/app).
var controlPlaneProbed sync.Map

// verifyControlPlaneGuarded checks that an agent this process launched is
// really enforcing the control-plane token it was handed.
//
// A token that never reaches the agent fails OPEN rather than closed: an agent
// started with no --token-file natively, or whose docker or compose client did
// not pass the variable through to the container, has no token, and serves its
// whole control plane — live TLS session keys included — to anything that can
// reach the port, while this process keeps sending a header nothing checks.
// Every request still succeeds, so nothing downstream notices and no test of a
// working flow can tell.
//
// One request settles it: a token that is wrong by construction must be
// refused. It runs here, at the readiness gate, because this is the one point
// every mode passes through — native, `docker run` and docker compose, where
// the agent is a service in the compose project keploy rewrites and is started
// by the user's own `docker compose up`, not by this process directly.
//
// Only for an agent this process launched (token.Launched). A process that
// drives an agent something else started — a sidecar k8s-proxy injected, whose
// token (if it has one) k8s-proxy put in its pod spec, or a pinned agent image
// that predates the token — made no handoff of its own, and blaming one that
// never happened is a false alarm at error level. A sidecar without a token
// says for itself that it is running unauthenticated; an image that predates
// the token says nothing, as it checks nothing.
//
// Reported rather than fatal: an agent that could not be given a token still
// records and replays, and refusing to run would be the worse outcome. It is
// logged at error level so that it reaches the operator and fails keploy's own
// CI, which treats any ERROR in a run as fatal.
func verifyControlPlaneGuarded(ctx context.Context, logger *zap.Logger, agentURI string) {
	launch, launched := token.Launched(agentURI)
	if !launched {
		logger.Debug("not checking the control-plane authentication of an agent this process did not launch",
			zap.String("agent", agentURI))
		return
	}
	key := fmt.Sprintf("%s#%d", agentURI, launch)
	if _, done := controlPlaneProbed.Load(key); done {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentURI+token.ProbePath, nil)
	if err != nil {
		logger.Debug("could not build the agent authentication probe", zap.Error(err))
		return
	}
	// Wrong by construction, and a bare client: a request carrying the real
	// token would be accepted by a guarded agent and prove nothing.
	req.Header.Set("Authorization", "Bearer not-the-session-token")

	resp, err := (&http.Client{Timeout: controlPlaneProbeTimeout}).Do(req)
	if err != nil {
		// The agent answered its health check a moment ago, so a failure here
		// is not evidence either way. Saying nothing beats crying wolf with an
		// error that fails CI — and this attempt is deliberately NOT recorded,
		// so a later readiness gate for the same agent tries again.
		logger.Debug("agent authentication probe did not complete", zap.Error(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Only an answer counts. Marking earlier would let an attempt that proved
	// nothing retire the check for this agent.
	controlPlaneProbed.Store(key, struct{}{})

	if resp.StatusCode == http.StatusUnauthorized {
		logger.Debug("agent control-plane authentication verified", zap.String("agent", agentURI))
		return
	}

	utils.LogError(logger, nil, "the agent is NOT enforcing control-plane authentication: it accepted a request carrying an invalid token",
		zap.Int("probe_status", resp.StatusCode),
		zap.String("impact", "/agent/pcap/keylog streams live TLS session keys and /agent/stop and /agent/storemocks alter this session; any local user or neighbouring container that can reach the agent port can use them"),
		zap.String("next_step", "the token this keploy process handed to the agent did not reach it — check that the agent was started with --token-file (native), or that "+token.Env+" reached the docker or docker compose client's environment (docker; a doas in front of docker compose resets that environment unless its doas.conf rule has keepenv or setenv { "+token.Env+" }), and report this if you did not change how keploy starts its agent"))
}

// controlPlaneProbeTimeout bounds the check above. Generous: the agent answered
// a health check with a 500ms budget immediately before it runs.
const controlPlaneProbeTimeout = 5 * time.Second
