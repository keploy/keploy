package testdb

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

const (
	maxSlugLen  = 40
	fallbackTC  = "test"
	slugSuffMin = 1
)

var (
	nonSlugChar = regexp.MustCompile(`[^a-z0-9]+`)
	dashRun     = regexp.MustCompile(`-+`)
	// uuidPattern matches canonical 8-4-4-4-12 hex UUIDs.
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// hexIDPattern matches long hex strings (e.g. Mongo ObjectID).
	hexIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
)

// BuildTestCaseSlug returns a descriptive slug derived from a recorded
// test case. It never includes a trailing counter — the caller is
// responsible for disambiguation.
func BuildTestCaseSlug(tc *models.TestCase) string {
	switch tc.Kind {
	case models.GRPC_EXPORT:
		return slugForGRPC(tc)
	default:
		return slugForHTTP(tc)
	}
}

func slugForHTTP(tc *models.TestCase) string {
	method := strings.ToLower(strings.TrimSpace(string(tc.HTTPReq.Method)))
	path := extractPath(tc.HTTPReq.URL)
	segs := splitAndLabelPathSegments(path)

	parts := make([]string, 0, len(segs)+1)
	if method != "" {
		parts = append(parts, method)
	}
	if len(segs) == 0 {
		// No meaningful path — label the slug so the method stays paired
		// with a stable token instead of getting a bare "get".
		parts = append(parts, "root")
	} else {
		parts = append(parts, segs...)
	}

	slug := sanitizeSlug(strings.Join(parts, "-"))
	if slug == "" {
		return fallbackTC
	}
	return truncateSlug(slug, maxSlugLen)
}

func slugForGRPC(tc *models.TestCase) string {
	rpcPath := tc.GrpcReq.Headers.PseudoHeaders[":path"]
	// gRPC :path is "/package.Service/Method"
	rpcPath = strings.TrimPrefix(rpcPath, "/")
	parts := strings.Split(rpcPath, "/")

	pieces := []string{"grpc"}
	if len(parts) >= 1 && parts[0] != "" {
		// Drop the leading package segment(s), keep only the final Service name.
		svcParts := strings.Split(parts[0], ".")
		service := svcParts[len(svcParts)-1]
		pieces = append(pieces, service)
	}
	if len(parts) >= 2 && parts[1] != "" {
		pieces = append(pieces, parts[1])
	}

	slug := sanitizeSlug(strings.Join(pieces, "-"))
	if slug == "" || slug == "grpc" {
		return "grpc"
	}
	return truncateSlug(slug, maxSlugLen)
}

// extractPath returns just the path component of a URL, tolerating
// inputs that are already bare paths.
func extractPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		return u.Path
	}
	// Fallback: strip query/fragment manually.
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// splitAndLabelPathSegments splits a URL path into segments and replaces
// segments that look like identifiers with a placeholder token so that
// requests like /users/42 and /users/43 collapse to the same slug.
func splitAndLabelPathSegments(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	raw := strings.Split(path, "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg == "" {
			continue
		}
		if isIDSegment(seg) {
			out = append(out, "by-id")
			continue
		}
		out = append(out, seg)
	}
	return out
}

// isIDSegment reports whether a path segment looks like an opaque
// identifier (numeric, UUID, or long hex). Short non-numeric slugs like
// "me" or "login" are preserved.
func isIDSegment(seg string) bool {
	if _, err := strconv.Atoi(seg); err == nil {
		return true
	}
	if uuidPattern.MatchString(seg) {
		return true
	}
	if hexIDPattern.MatchString(seg) {
		return true
	}
	return false
}

// sanitizeSlug lowercases input, replaces disallowed characters with
// dashes, and collapses/trims dash runs.
func sanitizeSlug(s string) string {
	s = strings.ToLower(s)
	s = nonSlugChar.ReplaceAllString(s, "-")
	s = dashRun.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// truncateSlug caps slug length and avoids leaving a trailing dash or
// cutting a segment mid-word when possible.
func truncateSlug(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndex(cut, "-"); i > 0 {
		cut = cut[:i]
	}
	return strings.Trim(cut, "-")
}
