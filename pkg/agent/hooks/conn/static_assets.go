// Package conn captures incoming requests as test cases.
package conn

import (
	"mime"
	"net/http"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// IsStaticAssetResponse recognizes successful web static asset responses by
// their declared Content-Type. Do not guess from a URL suffix: an API ending
// in .png can still return JSON. API download types (application/octet-stream,
// application/pdf, archives, application/wasm), mutating requests, and failed
// responses are never classified as static assets.
func IsStaticAssetResponse(req *http.Request, resp *http.Response) bool {
	if req == nil || resp == nil {
		return false
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && resp.StatusCode != http.StatusNotModified {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	for _, prefix := range []string{"image/", "audio/", "video/", "font/"} {
		if strings.HasPrefix(mediaType, prefix) {
			return true
		}
	}
	switch mediaType {
	case "text/css", "text/javascript", "application/javascript", "application/x-javascript",
		"application/font-woff", "application/font-woff2", "application/vnd.ms-fontobject",
		"application/x-font-ttf", "application/x-font-opentype", "application/x-font-woff":
		return true
	}
	return false
}

// ShouldSkipStaticAsset reports whether req/resp should be skipped under
// opts.IgnoreStaticAssets. Explicit Include rules in opts.Filters always take
// precedence over IgnoreStaticAssets (unless also excluded by an Exclude rule).
func ShouldSkipStaticAsset(logger *zap.Logger, req *http.Request, resp *http.Response, opts models.IncomingOptions) bool {
	if !opts.IgnoreStaticAssets || !IsStaticAssetResponse(req, resp) {
		return false
	}
	filtered, matchedInclude := evalFilters(logger, req, opts)
	if !filtered && matchedInclude {
		return false
	}
	return true
}
