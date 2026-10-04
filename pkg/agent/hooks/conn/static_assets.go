// Package conn captures incoming requests as test cases.
package conn

import (
	"mime"
	"net/http"
	"strings"
)

// isStaticAssetResponse recognizes successful asset downloads by their declared
// response type. Do not guess from a URL suffix: an API ending in .png can still
// return JSON. Mutating requests and failed downloads remain useful test cases.
func isStaticAssetResponse(req *http.Request, resp *http.Response) bool {
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
		"application/x-font-ttf", "application/x-font-opentype", "application/x-font-woff",
		"application/octet-stream", "application/pdf", "application/wasm",
		"application/zip", "application/gzip", "application/x-gzip", "application/x-tar",
		"application/x-7z-compressed", "application/vnd.rar", "application/x-rar-compressed":
		return true
	}
	return false
}
