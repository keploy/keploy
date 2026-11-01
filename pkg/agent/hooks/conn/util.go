package conn

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Deprecated: GlobalTestCounter is the legacy process-global test-ID
// counter. The per-session path now uses SyncMockManager.NextTestID()
// (resolved via syncMock.FromContextOrGlobal). Retained for the
// enterprise async capture path until it migrates to the per-runtime
// manager (E6). Do not use in new code.
var GlobalTestCounter int64

// resolveTestWindow stamps a synthetic "test-N" name, fires ResolveRange to bin
// buffered mocks into this window, and returns the name as a local string.
// Callers assign the return value directly to testCase.Name rather than reading
// testCase.Name back — this severs any CodeQL taint flow: testCase may carry
// HTTP/gRPC-sourced data so reads from its fields are flagged, but the return
// value here is always a synthetic "test-N" identifier derived only from the
// atomic counter and is safe to forward to ResolveRange's diagnostic logs.
func resolveTestWindow(reqTime, resTime time.Time, mapping bool) string {
	currentID := atomic.AddInt64(&GlobalTestCounter, 1)
	testName := fmt.Sprintf("test-%d", currentID)
	if mgr := syncMock.Get(); mgr != nil {
		mgr.ResolveRange(reqTime, resTime, testName, true, mapping)
	}
	return testName
}

// mapping (last bool before appPort) controls whether the synchronous
// branch tells SyncMockManager.ResolveRange to emit a TestMockMapping
// entry onto the agent's mappingChan. Mirrors !cfg.DisableMapping —
// before this parameter existed the OSS call site hardcoded false, so
// mappings.yaml production during record was silently a no-op (#3905).
type CaptureFunc func(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, req *http.Request, resp *http.Response, reqTimeTest time.Time, resTimeTest time.Time, opts models.IncomingOptions, synchronous bool, mapping bool, appPort uint16)

var CaptureHook CaptureFunc = Capture

// GRPCCaptureHook is called in CaptureGRPC before a test case is built.
// isDuplicate reports whether the pair repeats an already-captured schema and
// should be dropped; schemaKey is the canonical static-dedup fingerprint of
// the pair, stamped onto the kept test case (TestCase.SchemaKey) so the
// control plane can additionally mark duplicates across pods of a recording.
// An empty schemaKey means "no fingerprint" and disables cross-pod judgement
// for this test case. Enterprise sets this when --static-dedup is enabled;
// nil means no dedup.
var GRPCCaptureHook func(req *models.GrpcReq, resp *models.GrpcResp) (isDuplicate bool, schemaKey string)

// MaxTestCaseSize is the maximum combined size of HTTP/gRPC request and response (5MB)
const MaxTestCaseSize = 5 * 1024 * 1024 // 5 MB

// LargeBodyThreshold is the size threshold (1MB) above which response bodies
// are skipped during recording and only body size is stored.
const LargeBodyThreshold = 1 * 1024 * 1024 // 1 MB

// SkipUnderPressure reports whether the memory guard has paused recording, as
// a capture hook decides whether to record an exchange. When it has, it closes
// the exchange's bodies, so the proxied connection's readers do not leak, and
// counts the request as not recorded under pressure on the ingress connection
// ctx names (syncMock NoteNotRecordedUnderPressure, WithIngressConn), so the
// recording says what the pause cost. A capture hook asks it first and returns
// when it says true.
func SkipUnderPressure(ctx context.Context, req *http.Request, resp *http.Response) bool {
	if !memoryguard.IsRecordingPaused() {
		return false
	}
	if req != nil && req.Body != nil {
		_ = req.Body.Close()
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	syncMock.FromContextOrGlobal(ctx).NoteNotRecordedUnderPressure(syncMock.IngressConn(ctx))
	return true
}

// SkipGRPCUnderPressure reports whether a pause of the memory guard costs a
// gRPC call, as CaptureGRPC decides whether to record it: the guard has
// paused recording now, or had as the forwarder read the call's request
// (pausedAtRequest, pkg.HTTP2Stream.RequestPaused). When it does, it counts
// the call as not recorded under pressure on the ingress connection ctx names
// (syncMock NoteNotRecordedUnderPressure, WithIngressConn), so the recording
// says what the pause cost.
//
// The gRPC ingress tees a connection's whole stream through a pause, so its
// calls come here whenever they complete: whether the pause was on as the
// request was read is what puts a call in it, as an HTTP ingress reads the
// pause as a request comes. The request's time cannot: a pause's span ends at
// the instant the pause does, and on a clock as coarse as Windows' the next
// call's request is stamped in that same tick, so the first call after every
// pause was judged to be in it. A call whose request came before a pause that
// has ended by its response is captured, and the recording suppresses it for
// the overlap (WasPressureActiveInWindow), as it does an HTTP exchange's.
func SkipGRPCUnderPressure(ctx context.Context, pausedAtRequest bool) bool {
	paused := pausedAtRequest || memoryguard.IsRecordingPaused()
	if paused {
		syncMock.FromContextOrGlobal(ctx).NoteNotRecordedUnderPressure(syncMock.IngressConn(ctx))
	}
	return paused
}

// HandOn hands testCase, which a capture built, to t, the recording's queue,
// and reports whether it did. A capture runs on its ingress's context (ctx),
// which ends as that ingress stops: its forwarder for the app's port, or the
// agent, is stopping. A test case that ctx's end beats to the queue never
// reaches the recording, though the app served its request: it is counted on
// the manager ctx carries (syncMock NoteNotHandedOn), so the recording says
// how many it lost so. A CaptureFunc that builds its own test cases hands them
// on with it.
func HandOn(ctx context.Context, t chan<- *models.TestCase, testCase *models.TestCase) bool {
	select {
	case t <- testCase:
		return true
	case <-ctx.Done():
		syncMock.FromContextOrGlobal(ctx).NoteNotHandedOn()
		return false
	}
}

func Capture(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, req *http.Request, resp *http.Response, reqTimeTest time.Time, resTimeTest time.Time, opts models.IncomingOptions, synchronous bool, mapping bool, appPort uint16) {
	if SkipUnderPressure(ctx, req, resp) {
		return
	}

	// Registered before any early return below (e.g. a decompression
	// failure on the request body) so resp.Body never leaks.
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close the http response body")
		}
	}()

	// What the decodes below hold reserved with the caller's
	// DecodeReserveFunc, their output buffers: given back once the test case
	// is built from them, before it is handed on, or as the capture returns
	// without one.
	decodes := &decodeRoom{fn: DecodeReserve(ctx)}
	defer decodes.giveBack()

	var reqBody []byte
	if req.Body != nil { // Read
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			logger.Debug("failed to read the http request body", zap.Any("metadata", utils.GetReqMeta(req)), zap.Int64("of size", int64(len(reqBody))), zap.String("body", base64.StdEncoding.EncodeToString(reqBody)), zap.Error(err))
		}

		if req.Header.Get("Content-Encoding") != "" {
			reqBody, err = pkg.DecompressReserving(logger, req.Header.Get("Content-Encoding"), reqBody, MaxTestCaseSize, decodes.reserve())
			if err != nil {
				if errors.Is(err, pkg.ErrDecodeRefused) {
					// The caller's DecodeReserveFunc refused room for the
					// decoded body: it counts what it leaves out.
					return
				}
				if errors.Is(err, pkg.ErrDecompressedTooLarge) {
					// Oversized, not corrupt: same condition as the post-
					// decompression MaxTestCaseSize check further down, hit
					// earlier — log it the same way, not as a decode failure.
					logger.Error("HTTP test case data exceeds 5MB limit, skipping capture",
						zap.String("url", req.URL.String()),
						zap.String("method", req.Method))
					return
				}
				utils.LogError(logger, err, "failed to decode the http request body", zap.Any("metadata", utils.GetReqMeta(req)))
				return
			}
		}
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.LogError(logger, err, "failed to read the http response body")
		return
	}

	if IsFiltered(logger, req, opts) {
		logger.Debug("The request is a filtered request")
		return
	}
	// The request body decoded, before it is taken apart below: what
	// BodiesDecoded is told the test case holds of it, at most.
	reqDecoded := len(reqBody)
	var formData []models.FormData
	sent := false // the test case was handed on t
	if contentType := req.Header.Get("Content-Type"); strings.HasPrefix(contentType, "multipart/form-data") {
		parts := strings.Split(contentType, ";")
		if len(parts) > 1 {
			req.Header.Set("Content-Type", strings.TrimSpace(parts[0]))
		}
		formData = ExtractFormData(logger, reqBody, contentType)
		reqBody = []byte{}
		// The files the parts were written to are the test case's: the
		// stream it is sent on deletes them (routes/record.go). One left out
		// below is sent nowhere, and nothing else knows of its files.
		defer func() {
			if !sent {
				RemoveFormFiles(logger, formData)
			}
		}()
	} else if contentType := req.Header.Get("Content-Type"); contentType == "application/x-www-form-urlencoded" {
		decodedBody, err := url.QueryUnescape(string(reqBody))
		if err != nil {
			utils.LogError(logger, err, "failed to decode the url-encoded request body")
			return
		}
		reqBody = []byte(decodedBody)
	}

	if resp.Header.Get("Content-Encoding") != "" {
		respBody, err = pkg.DecompressReserving(logger, resp.Header.Get("Content-Encoding"), respBody, MaxTestCaseSize, decodes.reserve())
		if err != nil {
			if errors.Is(err, pkg.ErrDecodeRefused) {
				return // the caller that refused counts it, as above
			}
			if errors.Is(err, pkg.ErrDecompressedTooLarge) {
				logger.Error("HTTP test case data exceeds 5MB limit, skipping capture",
					zap.String("url", req.URL.String()),
					zap.String("method", req.Method))
				return
			}
			utils.LogError(logger, err, "failed to decompress the response body")
			return
		}
	}

	// Check if combined request and response size exceeds 5MB limit (after decompression)
	totalSize := len(reqBody) + len(respBody)
	if totalSize > MaxTestCaseSize {
		logger.Error("HTTP test case data exceeds 5MB limit, skipping capture",
			zap.Int("totalSize", totalSize),
			zap.Int("reqBodySize", len(reqBody)),
			zap.Int("respBodySize", len(respBody)),
			zap.String("url", req.URL.String()),
			zap.String("method", req.Method))
		return
	}

	// If response body exceeds 1MB, mark it as skipped and store only the size
	var respBodySkipped bool
	var respBodySize int64
	if len(respBody) > LargeBodyThreshold {
		respBodySkipped = true
		respBodySize = int64(len(respBody))
		logger.Debug("response body exceeds 1MB during recording, storing only body size",
			zap.Int64("body_size_bytes", respBodySize),
			zap.String("url", req.URL.String()),
			zap.String("method", req.Method))
		respBody = []byte{} // clear the body — only size is stored
	}

	// The caller that carried a BodiesDecodedFunc weighs the test case by
	// its bodies decoded here, and may leave it out; it counts what it
	// leaves out.
	if !BodiesDecoded(ctx, reqDecoded, len(respBody)) {
		return
	}

	hasBinaryFile := false
	for _, fd := range formData {
		if len(fd.Paths) > 0 {
			hasBinaryFile = true
			logger.Debug("Detected binary file in request form data", zap.String("key", fd.Key), zap.Strings("paths", fd.Paths))
			break
		}
	}

	testCase := &models.TestCase{
		Version:       models.GetVersion(),
		Name:          pkg.ToYamlHTTPHeader(req.Header)["Keploy-Test-Name"],
		Kind:          models.HTTP,
		Created:       time.Now().Unix(),
		SourcePod:     SourcePodFromContext(ctx),
		HasBinaryFile: hasBinaryFile,
		HTTPReq: models.HTTPReq{
			Method:     models.Method(req.Method),
			ProtoMajor: req.ProtoMajor,
			ProtoMinor: req.ProtoMinor,
			URL:        fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
			Form:       formData,
			Header:     pkg.ToYamlHTTPHeader(req.Header),
			Body:       string(reqBody),
			URLParams:  pkg.URLParams(req),
			Timestamp:  reqTimeTest,

			// Replay sends this request again: keep what it takes to
			// send a repeated header on its own lines.
			HeaderLineLengths: pkg.ToYamlHTTPHeaderLineLengths(req.Header),
		},
		HTTPResp: models.HTTPResp{
			StatusCode:    resp.StatusCode,
			Header:        pkg.ToYamlHTTPHeader(resp.Header),
			Body:          string(respBody),
			BodySkipped:   respBodySkipped,
			BodySize:      respBodySize,
			Timestamp:     resTimeTest,
			StatusMessage: http.StatusText(resp.StatusCode),
		},
		Noise:   map[string][]string{},
		AppPort: appPort,
		// Mocks: mocks,
	}
	// The clock its window start is on (proven switch).
	StampOrigin(ctx, testCase)
	// The test case holds its own copies of the bodies: the decoded buffers
	// are the capture's no longer, and their room goes back before the test
	// case waits to be handed on.
	decodes.giveBack()

	if synchronous {
		// The request's open window (the ingress opened it at the request's
		// first byte: syncMock.OpenWindow) kept every mock it may own from the
		// reapers while it was in flight. Claim it for this resolve. The
		// manager gives a window up only when a request stays in flight while
		// more traffic is captured than it holds, or when memory pressure lets
		// go of mocks it may own; they are gone then, so its test case is left
		// out, not recorded without them.
		//
		// A left-out request's verdict prunes nothing. It is not a duplicate:
		// it ran for long, and a keep=false resolve over its whole span would
		// drop the mocks of every call that ran beside it and is not decided
		// yet (a gRPC call opens no window to hold them). What is left of its
		// window is claimed by whoever's it is, or goes at the stale cutoff.
		//
		// The window is on the manager its ingress opened it on, the one the
		// ctx carries (or the global one): the test-id counter's, and the one
		// resolved here.
		//
		// The window is claimed only once the test case is in the
		// recording's queue (HandOn), not before: a claimed window is never
		// given up, so it pins every mock held from its start on, and the
		// queue stays full for as long as the recording's stream is slow.
		// Claimed while the test case waited, the hold grew with all the
		// traffic captured meanwhile, past its bound. So the window stays
		// open and unclaimed until the test case is queued, and the mocks
		// are resolved to it (ResolveKept) after that:
		//   - given up already: left out, as before, and never handed on;
		//   - not handed on (the ingress stopped first, and HandOn counted
		//     it): nothing is resolved to a test case the recording never
		//     gets; the ingress ends the window, as for a left-out request;
		//   - given up while the test case waited for the queue: it went out
		//     without mocks it may own, and RevokeTestCase leaves it out.
		mgr := syncMock.FromContextOrGlobal(ctx)
		win := syncMock.WindowFromContext(ctx)
		if win.LeftOut() {
			// The manager warned of it as LeftOut found the window given up;
			// the ingress ends the window.
			logger.Debug("Test case left out: its request's window was given up while it was in flight",
				zap.String("method", req.Method),
				zap.Time("in_flight_since", reqTimeTest))
			return
		}
		// Per-session counter: resolves to the ctx-carried manager for
		// multi-app callers, or the package-global manager otherwise —
		// the latter reproduces the old GlobalTestCounter sequence.
		currentID := mgr.NextTestID()
		testName := fmt.Sprintf("test-%d", currentID)
		testCase.Name = testName
		// Pass testName (the locally-synthesised "test-N" identifier),
		// not testCase.Name. CodeQL flow analysis treats testCase as
		// HTTP-sourced and any read from its fields as potentially
		// sensitive — even though we just wrote a synthetic ID into
		// .Name a line earlier. Forwarding the local string severs
		// the taint flow and stops the go/clear-text-logging false
		// positives that fire downstream (syncMock.go diag/Info
		// logs include test_name for ResolveRange traceability).
		if !HandOn(ctx, t, testCase) {
			return
		}
		sent = true
		if !win.Keep() {
			// Given up while the test case waited for the queue: the manager
			// warned of it as Keep found it so, and the test case is left out
			// after all; the ingress ends the window.
			mgr.RevokeTestCase(testName)
			logger.Debug("Test case left out: its request's window was given up while its test case waited for the recording's queue",
				zap.String("method", req.Method),
				zap.Time("in_flight_since", reqTimeTest))
			return
		}
		// Dumping the test case from the mock manager in synchronous mode:
		// takes what was held for the request and ends its window before
		// handing the mocks on. ResolveKept noted the test case's span.
		mgr.ResolveKept(win, reqTimeTest, resTimeTest, testName, mapping)
		return
	}
	if syncMock.WindowFromContext(ctx).LeftAtStop() {
		// Without --sync nothing is resolved here, but a request that was in
		// flight across its recording's stop is left out as Keep leaves it
		// out with --sync: the recording it began in has ended without it,
		// and it is no request of the next one.
		logger.Debug("Test case left out: its request was in flight across its recording's stop",
			zap.String("method", req.Method),
			zap.Time("in_flight_since", reqTimeTest))
		return
	}
	if !HandOn(ctx, t, testCase) {
		return
	}
	sent = true
	// Without --sync no resolve ends the window: it is noted a test case's
	// here, before the ingress ends it (syncMock.TestSpanAt).
	syncMock.WindowFromContext(ctx).Tested(reqTimeTest, resTimeTest)
}
func IsFiltered(logger *zap.Logger, req *http.Request, opts models.IncomingOptions) bool {
	dstPort := 0
	var err error
	if p := req.URL.Port(); p != "" {
		dstPort, err = strconv.Atoi(p)
		if err != nil {
			utils.LogError(logger, err, "failed to obtain destination port from request")
			return false
		}
	}

	hasInclude := false
	matchedInclude := false

	type cond struct {
		eligible bool
		match    bool
	}

	for _, filter := range opts.Filters {

		//  1. bypass rule
		bypassEligible := !(filter.BypassRule.Host == "" &&
			filter.BypassRule.Path == "" &&
			filter.BypassRule.Port == 0)

		outgoingOpts := models.OutgoingOptions{Rules: []models.BypassRule{filter.BypassRule}}
		byPassMatch := utils.IsPassThrough(logger, req, uint(dstPort), outgoingOpts)

		//  2. URL-method rule
		urlMethodEligible := len(filter.URLMethods) > 0
		urlMethodMatch := false
		if urlMethodEligible {
			for _, m := range filter.URLMethods {
				if m == req.Method {
					urlMethodMatch = true
					break
				}
			}
		}

		//  3. header rule
		headerEligible := len(filter.Headers) > 0
		headerMatch := false
		if headerEligible {
			for key, vals := range filter.Headers {
				rx, err := regexp.Compile(vals)
				if err != nil {
					utils.LogError(logger, err, "bad header regex")
					continue
				}
				for _, v := range req.Header.Values(key) {
					if rx.MatchString(v) {
						headerMatch = true
						break
					}
				}
				if headerMatch {
					break
				}
			}
		}

		conds := []cond{
			{bypassEligible, byPassMatch},
			{urlMethodEligible, urlMethodMatch},
			{headerEligible, headerMatch},
		}

		isMatch := false
		switch filter.MatchType {
		case models.AND:
			pass := true
			seen := false
			for _, c := range conds {
				if !c.eligible {
					continue
				} // ignore ineligible ones
				seen = true
				if !c.match {
					pass = false
					break
				}
			}
			if seen && pass {
				isMatch = true
			}

		case models.OR:
			fallthrough
		default:
			for _, c := range conds {
				if c.eligible && c.match {
					isMatch = true
					break
				}
			}
		}

		if filter.FilterPolicy == models.Include {
			hasInclude = true
			if isMatch {
				matchedInclude = true
			}
		} else { // Exclude (default)
			if isMatch {
				return true // Exclude ALWAYS takes priority
			}
		}
	}

	if hasInclude && !matchedInclude {
		return true
	}

	return false
}

// RemoveFormFiles deletes the files ExtractFormData wrote the file parts of a
// form to. They belong to the test case whose Form carries them: whoever ends
// up with it deletes them, once it has streamed them or left it out.
func RemoveFormFiles(logger *zap.Logger, forms []models.FormData) {
	for _, form := range forms {
		for _, path := range form.Paths {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				utils.LogError(logger, err, "failed to remove the file a form's file part was written to", zap.String("path", path))
			}
		}
	}
}

func ExtractFormData(logger *zap.Logger, body []byte, contentType string) []models.FormData {
	boundary := ""
	if strings.HasPrefix(contentType, "multipart/form-data") {
		parts := strings.Split(contentType, "boundary=")
		if len(parts) > 1 {
			boundary = strings.TrimSpace(parts[1])
		} else {
			utils.LogError(logger, nil, "Invalid multipart/form-data content type")
			return nil
		}
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var formData []models.FormData

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			utils.LogError(logger, err, "Error reading part")
			continue
		}
		key := part.FormName()
		if key == "" {
			continue
		}

		fileName := part.FileName()
		var fileNames []string
		var values []string
		var paths []string

		if fileName != "" {
			file, err := os.CreateTemp("", "keploy-multipart-*.bin")
			if err != nil {
				utils.LogError(logger, err, "Error creating temp file")
				continue
			}
			tempPath := file.Name()
			_, err = io.Copy(file, part)
			if err != nil {
				utils.LogError(logger, err, "Error copying part to temp file")
				if cerr := file.Close(); cerr != nil {
					utils.LogError(logger, cerr, "Error closing temp file")
				}
				if rerr := os.Remove(tempPath); rerr != nil {
					utils.LogError(logger, rerr, "Error removing temp file", zap.String("path", tempPath))
				}
				continue
			}
			if err := file.Close(); err != nil {
				utils.LogError(logger, err, "Error closing temp file")
				if rerr := os.Remove(tempPath); rerr != nil {
					utils.LogError(logger, rerr, "Error removing temp file", zap.String("path", tempPath))
				}
				continue
			}

			paths = append(paths, tempPath)
			fileNames = append(fileNames, fileName)
		} else {

			value, err := io.ReadAll(part)
			if err != nil {
				utils.LogError(logger, err, "Error reading part value")
				continue
			}
			values = append(values, string(value))
		}

		formData = append(formData, models.FormData{
			Key:       key,
			Values:    values,
			FileNames: fileNames,
			Paths:     paths,
		})
	}

	return formData
}

// InlineFormFiles moves the file parts ExtractFormData left in local temp files
// into tc itself (FormData.FileContents), for a test case persisted by another
// process than this one: the DaemonSet agent streams its test cases to
// k8s-proxy's control plane, which cannot read this pod's temp files. Each temp
// file is removed once read and its path cleared, so the persister writes the
// asset from the bytes. A file that cannot be read keeps its path.
func InlineFormFiles(logger *zap.Logger, tc *models.TestCase) {
	for i := range tc.HTTPReq.Form {
		form := &tc.HTTPReq.Form[i]
		for j, path := range form.Paths {
			if path == "" {
				continue
			}
			content, err := os.ReadFile(path)
			if err != nil {
				utils.LogError(logger, err, "Error reading multipart temp file", zap.String("path", path))
				continue
			}
			if len(form.FileContents) < len(form.Paths) {
				form.FileContents = append(form.FileContents, make([][]byte, len(form.Paths)-len(form.FileContents))...)
			}
			form.FileContents[j] = content
			form.Paths[j] = ""
			if err := os.Remove(path); err != nil {
				utils.LogError(logger, err, "Error removing temp file", zap.String("path", path))
			}
		}
	}
}

// grpcMessagesSize is the size of a direction's messages' protoscope text.
func grpcMessagesSize(msgs []models.GrpcLengthPrefixedMessage) int {
	n := 0
	for _, m := range msgs {
		n += len(m.DecodedData)
	}
	return n
}

// CaptureGRPC captures a gRPC request/response pair and sends it to the test case channel.
// Mirrors NewCapture (HTTP) exactly:
//   - A call over MaxTestCaseSize, every message of both directions counted, is
//     never a test case: it is judged before GRPCCaptureHook, which so never
//     counts its shape as seen (a later call of that shape under the limit is
//     no duplicate of it), and its mocks are released (keep=false).
//   - GRPCCaptureHook is invoked before ResolveRange for every other call, so
//     ResolveRange always runs — even for duplicates (with keep=false to flush
//     their mocks cleanly).
//   - Counter is always incremented so concurrent duplicate streams each get a
//     unique window name; keep=false discards their mocks without saving the TC.
//   - Duplicate streams return after ResolveRange, before the test case is sent.
//
// A call a pause of the memory guard costs (SkipGRPCUnderPressure) is left out
// and counted before any of that.
func CaptureGRPC(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, http2Stream *pkg.HTTP2Stream, appPort uint16, synchronous bool, mapping bool) {
	if http2Stream == nil {
		logger.Error("Stream is nil")
		return
	}

	if http2Stream.GRPCReq == nil || http2Stream.GRPCResp == nil {
		logger.Error("gRPC request or response is nil")
		return
	}

	if SkipGRPCUnderPressure(ctx, http2Stream.RequestPaused) {
		return
	}

	testName := http2Stream.GRPCReq.Headers.OrdinaryHeaders["Keploy-Test-Name"]
	// Reuse the same 5 MB cap as HTTP. DecodedData is the protoscope text
	// representation of the binary payload — large protobuf messages can
	// easily exceed this threshold, and a stream's later messages count too.
	totalSize := grpcMessagesSize(http2Stream.GRPCReq.AllMessages()) + grpcMessagesSize(http2Stream.GRPCResp.AllMessages())
	oversized := totalSize > MaxTestCaseSize
	var isDuplicate bool
	var schemaKey string
	if GRPCCaptureHook != nil && !oversized {
		isDuplicate, schemaKey = GRPCCaptureHook(http2Stream.GRPCReq, http2Stream.GRPCResp)
	}

	// Bin buffered outgoing mocks into this stream's window before any early
	// return. For non-duplicates keep=true attributes the mocks; for duplicates
	// keep=false releases them from the buffer so they don't accumulate as
	// orphans. Must run before the isDuplicate early-return below.
	if synchronous {
		// Every stream, duplicate or not, takes a name of its own: concurrent
		// duplicates would otherwise share one and collide in the manager's
		// window map. It is the name its mocks are mapped to and its test
		// case's (below), as an HTTP request's is (Capture): the recording
		// joins a mapping to its test case by it. It comes off the manager
		// the call's context carries, as Capture's does, so an HTTP
		// request's and a gRPC call's names never share one, and the call's
		// mocks resolve on that manager.
		if mgr := syncMock.FromContextOrGlobal(ctx); mgr != nil {
			testName = fmt.Sprintf("test-%d", mgr.NextTestID())
			mgr.ResolveRange(
				http2Stream.GRPCReq.Timestamp,
				http2Stream.GRPCResp.Timestamp,
				testName,
				!isDuplicate && !oversized,
				mapping,
			)
		}
	}

	if oversized {
		logger.Error("gRPC test case data exceeds 5MB limit, skipping capture",
			zap.Int("totalSize", totalSize),
			zap.String("path", http2Stream.GRPCReq.Headers.PseudoHeaders[":path"]))
		return
	}
	if isDuplicate {
		return
	}

	testCase := &models.TestCase{
		Version:   models.GetVersion(),
		Name:      testName,
		Kind:      models.GRPC_EXPORT,
		Created:   time.Now().Unix(),
		SourcePod: SourcePodFromContext(ctx),
		SchemaKey: schemaKey,
		GrpcReq:   *http2Stream.GRPCReq,
		GrpcResp:  *http2Stream.GRPCResp,
		Noise:     map[string][]string{},
		AppPort:   appPort,
	}
	// The clock its window start is on (proven switch).
	StampOrigin(ctx, testCase)

	// With --sync the call's mocks were resolved to it above, before it is
	// handed on, unlike an HTTP request's (Capture): a gRPC call opens no
	// window to hold its mocks while its test case waits for the queue, so
	// resolved after the wait they could be gone at the stale cutoff. One
	// HandOn cannot hand on (counted) leaves those mocks, and their mapping,
	// to a test case the recording never gets: replay serves none of them.
	if HandOn(ctx, t, testCase) {
		logger.Debug("Captured gRPC test case",
			zap.String("path", http2Stream.GRPCReq.Headers.PseudoHeaders[":path"]))
	}
}
