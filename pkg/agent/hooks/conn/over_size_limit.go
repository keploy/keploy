package conn

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// MaxTestCaseBodySize is the most of one body, a request's or a response's,
// that the HTTP/1 ingress holds to record an exchange as a test case: half of
// MaxTestCaseSize, so that an exchange's two bodies stay within it together.
// A body larger than it is forwarded whole, and its exchange is not recorded
// (NoteOverSizeLimit, BodyLimit).
const MaxTestCaseBodySize = MaxTestCaseSize / 2

// SizeLimit is one of keploy's limits on what one test case holds: an exchange
// over it is forwarded as ever, and not recorded as a test case
// (NoteOverSizeLimit). Neither can be changed by a flag or a setting.
type SizeLimit int

const (
	// TestCaseLimit: an exchange's request and response together over
	// MaxTestCaseSize: their bodies decoded (Content-Encoding) as the
	// capture reads them, a gRPC call's messages as it decodes them (their
	// text, which can be several times their wire size), or, as the HTTP/1
	// ingress holds the exchange, its heads and bodies.
	TestCaseLimit SizeLimit = iota
	// BodyLimit: a request's or a response's body, as the HTTP/1 ingress
	// reads it, over MaxTestCaseBodySize.
	BodyLimit
)

// Bytes is the limit's size in bytes.
func (l SizeLimit) Bytes() int {
	if l == BodyLimit {
		return MaxTestCaseBodySize
	}
	return MaxTestCaseSize
}

// Why is why the recording's console says an exchange over l is not recorded
// (models.NotRecordedCall.Why): one sentence for every exchange over l, so the
// console says a call over it once, and counts its repeats.
func (l SizeLimit) Why() string {
	if l == BodyLimit {
		return "its request or response body is over " + mib(l.Bytes()) + ", the most of one body keploy reads to record a test case"
	}
	return "its request and response together, as keploy holds them, are over " + mib(l.Bytes()) + ", the most keploy records in one test case"
}

// overSizeWarnEvery is how often, past the first, a request over a size limit
// is warned of as it comes: the recording's summary counts them all.
const overSizeWarnEvery = 1024

// mib is n bytes in mebibytes, as a limit is said: "5 MiB", "2.5 MiB".
func mib(n int) string {
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', -1, 64) + " MiB"
}

// TellNotRecorded is what the line that tells that req is not recorded as a
// test case says of it (utils.GetReqMeta: its method, host, path's shape, path
// tag and query names), and it hands the recording's client the request, why
// it is not recorded, and the same path tag (syncMock NoteNotRecorded,
// models.NotRecordedPart): the client's console prints its path beside the
// tag, so the user can tell which request the line is about, which the line's
// shape alone does not say (GET /{} names /events and /poll alike). The one
// builder of what the console is told of a request the ingress served and did
// not record, whatever its reason.
func TellNotRecorded(ctx context.Context, req *http.Request, why string) map[string]string {
	meta := utils.GetReqMeta(ctx, req)
	if req != nil && req.URL != nil {
		syncMock.FromContextOrGlobal(ctx).NoteNotRecorded(models.NotRecordedCall{
			Method:    req.Method,
			Path:      req.URL.EscapedPath(),
			QueryKeys: meta["query keys"],
			PathTag:   meta["path_tag"],
			Why:       why,
		})
	}
	return meta
}

// NoteOverSizeLimit counts req, which the app was served and a capture leaves
// out because the exchange is over limit, unless the recording's filters
// (opts) leave req out anyway: on the manager ctx carries
// (syncMock NoteOverSizeLimit), named by its method, its path's shape and its
// path tag, for the recording's summary; and on the recording's console, by
// its path beside the same tag, with why (TellNotRecorded). The first such
// request of the agent's life and every overSizeWarnEvery-th are said at WARN
// as they come. A capture that leaves an exchange out for its size calls it
// once, where it decides: never for an exchange a pause of the memory guard
// cost, which is counted as that.
//
// The filters read req as Capture reads it: the ingress's request carries its
// host in req.Host alone, and the request Capture is handed (parsed from its
// dump) in its Host header too, which a filter on headers can match.
func NoteOverSizeLimit(ctx context.Context, logger *zap.Logger, req *http.Request, opts models.IncomingOptions, limit SizeLimit) {
	if req == nil || req.URL == nil {
		return
	}
	filtered := req
	if len(opts.Filters) > 0 && req.Host != "" && req.Header.Get("Host") == "" {
		withHost := *req
		withHost.Header = req.Header.Clone()
		if withHost.Header == nil {
			withHost.Header = http.Header{}
		}
		withHost.Header.Set("Host", req.Host)
		filtered = &withHost
	}
	if IsFiltered(logger, filtered, opts) {
		return
	}
	noteOverSizeLimit(ctx, logger, req, limit)
}

// NoteGRPCOverSizeLimit is NoteOverSizeLimit of a gRPC call to path (its
// :path, "/package.Service/Method") over limit. A gRPC call is a POST, and
// no filter of the recording reads it.
func NoteGRPCOverSizeLimit(ctx context.Context, logger *zap.Logger, path string, limit SizeLimit) {
	noteOverSizeLimit(ctx, logger, &http.Request{Method: http.MethodPost, URL: &url.URL{Path: path}}, limit)
}

// noteOverSizeLimit is NoteOverSizeLimit of a request whose filters its
// caller has read already.
func noteOverSizeLimit(ctx context.Context, logger *zap.Logger, req *http.Request, limit SizeLimit) {
	mgr := syncMock.FromContextOrGlobal(ctx)
	if !mgr.Recording() {
		// No recording takes it: its recording has ended and none has
		// started since. Counted as a request no recording took, as
		// HandOn counts one, not as one a recording lost to its size.
		mgr.NoteNotCaptured()
		return
	}
	call := models.WithPathTag(req.Method+" "+utils.URLPathShape(req.URL), models.URLPathTag(ctx, req.Method, req.URL))
	n := mgr.NoteOverSizeLimit(call)
	TellNotRecorded(ctx, req, limit.Why())
	if logger != nil && (n == 1 || n%overSizeWarnEvery == 0) {
		logger.Warn("agent: a request the app served was not recorded as a test case: "+limit.Why()+"; the recording's summary counts every such request",
			zap.String("call", call),
			zap.Int("limit_bytes", limit.Bytes()),
			zap.Uint64("over_size_limit_so_far", n),
			zap.String("next_step", models.OverSizeLimitNextStep))
	}
}
