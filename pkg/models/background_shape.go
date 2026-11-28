package models

import (
	"net/url"
	"slices"
	"sort"
	"strings"
)

// BackgroundShapeKey computes a canonical request-shape key so repeated
// invocations of the same background operation (including ones whose query or
// body values vary with bare epoch timestamps or SigV4 headers) collapse into
// one per-shape bucket while distinct endpoints/operations stay separate.
func BackgroundShapeKey(m *Mock) string {
	if m == nil {
		return ""
	}
	if m.onDisk != nil {
		return m.onDisk.bgShape
	}
	dest := m.Spec.Metadata["destAddr"]
	switch m.Kind {
	case HTTP:
		if r := m.Spec.HTTPReq; r != nil {
			uHost, pathShape := bgURLShape(r.URL)
			host := bgHeaderCaseInsensitive(r.Header, "Host")
			if host == "" {
				host = uHost
			}
			if host == "" {
				host = dest
			}
			amz := bgHeaderCaseInsensitive(r.Header, "X-Amz-Target")
			return string(m.Kind) + "\x00" + string(r.Method) + "\x00" + host + "\x00" + pathShape + "\x00" + amz
		}
	case HTTP2:
		if r := m.Spec.HTTP2Req; r != nil {
			uHost, pathShape := bgURLShape(r.URL)
			auth := r.Authority
			if auth == "" {
				auth = uHost
			}
			if auth == "" {
				auth = dest
			}
			amz := bgHeaderCaseInsensitive(r.Headers, "X-Amz-Target")
			return string(m.Kind) + "\x00" + string(r.Method) + "\x00" + auth + "\x00" + pathShape + "\x00" + amz
		}
	case GRPC_EXPORT:
		if r := m.Spec.GRPCReq; r != nil {
			auth := r.Headers.PseudoHeaders[":authority"]
			if auth == "" {
				auth = dest
			}
			return string(m.Kind) + "\x00" + auth + "\x00" + r.Headers.PseudoHeaders[":path"]
		}
	case Mongo:
		return string(m.Kind) + "\x00" + dest + "\x00" + bgMongoRequestShape(m.Spec.MongoRequests)
	}
	if stmt := StatementKey(m); stmt != "" {
		return string(m.Kind) + "\x00" + dest + "\x00" + stmt
	}
	if keys := RequestKeys(m); len(keys) > 0 {
		return string(m.Kind) + "\x00" + dest + "\x00" + keys[0]
	}
	return string(m.Kind) + "\x00" + dest + "\x00" + m.Spec.Metadata["operation"]
}

// SpecificBackgroundShapeKey returns BackgroundShapeKey(m) only when m carries
// a concrete request identity (non-empty HTTP/HTTP2 method + path/target,
// non-empty gRPC path, non-empty Mongo command/collection, or non-empty
// statement/request key/operation). Stub or unparsed mocks with no request
// payload return "" so two unrelated mocks with empty request details never
// match each other by shape.
func SpecificBackgroundShapeKey(m *Mock) string {
	if m == nil {
		return ""
	}
	if m.onDisk != nil {
		return m.onDisk.specificBgShape
	}
	switch m.Kind {
	case HTTP:
		r := m.Spec.HTTPReq
		if r == nil || r.Method == "" {
			return ""
		}
		_, pathShape := bgURLShape(r.URL)
		amz := bgHeaderCaseInsensitive(r.Header, "X-Amz-Target")
		if pathShape == "" && amz == "" {
			return ""
		}
		return BackgroundShapeKey(m)
	case HTTP2:
		r := m.Spec.HTTP2Req
		if r == nil || r.Method == "" {
			return ""
		}
		_, pathShape := bgURLShape(r.URL)
		amz := bgHeaderCaseInsensitive(r.Headers, "X-Amz-Target")
		if pathShape == "" && amz == "" {
			return ""
		}
		return BackgroundShapeKey(m)
	case GRPC_EXPORT:
		r := m.Spec.GRPCReq
		if r == nil || r.Headers.PseudoHeaders[":path"] == "" {
			return ""
		}
		return BackgroundShapeKey(m)
	case Mongo:
		if bgMongoRequestShape(m.Spec.MongoRequests) == "" {
			return ""
		}
		return BackgroundShapeKey(m)
	}
	if StatementKey(m) == "" && len(RequestKeys(m)) == 0 && m.Spec.Metadata["operation"] == "" {
		return ""
	}
	return BackgroundShapeKey(m)
}

func bgURLShape(raw string) (host, shape string) {
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		path, _, _ := strings.Cut(raw, "?")
		return "", path
	}
	host = u.Host
	if u.RawQuery == "" {
		return host, u.Path
	}
	parts := strings.Split(u.RawQuery, "&")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		k, _, _ := strings.Cut(p, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	keys = slices.Compact(keys)
	return host, u.Path + "?" + strings.Join(keys, "&")
}

func bgHeaderCaseInsensitive(h map[string]string, name string) string {
	if v, ok := h[name]; ok {
		return v
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func bgMongoRequestShape(reqs []MongoRequest) string {
	if len(reqs) == 0 {
		return ""
	}
	switch msg := reqs[0].Message.(type) {
	case *MongoOpMessage:
		if len(msg.Sections) == 0 {
			return ""
		}
		return bgMongoSectionCommand(msg.Sections[0])
	case *MongoOpQuery:
		return "query:" + msg.FullCollectionName
	case *MongoOpUpdate:
		return "update:" + msg.FullCollectionName
	case *MongoOpInsert:
		return "insert:" + msg.FullCollectionName
	case *MongoOpDelete:
		return "delete:" + msg.FullCollectionName
	case *MongoOpGetMore:
		return "getmore:" + msg.FullCollectionName
	}
	return ""
}

func bgMongoSectionCommand(section string) string {
	s := section
	if idx := strings.Index(s, "msg:"); idx >= 0 {
		s = s[idx+4:]
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		if len(s) > 64 {
			return s[:64]
		}
		return s
	}
	body := strings.TrimSpace(s[start+1:])
	end := strings.IndexAny(body, ",}")
	cmd := body
	if end >= 0 {
		cmd = body[:end]
	}
	cmd = strings.TrimSpace(cmd)
	if len(cmd) > 96 {
		cmd = cmd[:96]
	}
	if qKeys := bgMongoQueryKeys(s[start:]); qKeys != "" {
		return cmd + "|q=" + qKeys
	}
	return cmd
}

// bgMongoQueryKeys extracts sorted, value-stripped structural key paths from a
// Mongo command document's query/filter/q subdocument (supporting both strict
// JSON and Mongo shell unquoted-key syntax). Two polls of the same collection
// with different bare timestamps produce the same key path (e.g. dirtyAt.$lte),
// whereas a test's own claim with a different filter (e.g. _id,owner) stays
// distinct so shape matching never confuses distinct queries on one collection.
func bgMongoQueryKeys(doc string) string {
	for _, field := range []string{"query", "filter", "q"} {
		sub, ok := extractNamedBraceBlock(doc, field)
		if !ok {
			continue
		}
		var paths []string
		collectDocKeyPaths(sub, "", &paths, 0)
		if len(paths) == 0 {
			continue
		}
		sort.Strings(paths)
		paths = slices.Compact(paths)
		joined := strings.Join(paths, ",")
		if len(joined) > 128 {
			joined = joined[:128]
		}
		return joined
	}
	return ""
}

func extractNamedBraceBlock(doc, field string) (string, bool) {
	n := len(doc)
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < n; i++ {
		c := doc[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			if depth == 1 {
				// Potential top-level quoted key.
				j := i + 1
				for j < n && doc[j] != '"' {
					if doc[j] == '\\' {
						j++
					}
					j++
				}
				if j < n && doc[i+1:j] == field {
					if blk, ok := braceBlockAfterColon(doc, j+1); ok {
						return blk, true
					}
				}
				i = j
				continue
			}
			inStr = true
			continue
		}
		if c == '{' {
			depth++
			continue
		}
		if c == '}' {
			depth--
			continue
		}
		if depth == 1 && isIdentStart(c) {
			j := i + 1
			for j < n && isIdentChar(doc[j]) {
				j++
			}
			if doc[i:j] == field {
				if blk, ok := braceBlockAfterColon(doc, j); ok {
					return blk, true
				}
			}
			i = j - 1
		}
	}
	return "", false
}

func braceBlockAfterColon(doc string, pos int) (string, bool) {
	n := len(doc)
	for pos < n && (doc[pos] == ' ' || doc[pos] == '\t' || doc[pos] == '\n' || doc[pos] == '\r') {
		pos++
	}
	if pos >= n || doc[pos] != ':' {
		return "", false
	}
	pos++
	for pos < n && (doc[pos] == ' ' || doc[pos] == '\t' || doc[pos] == '\n' || doc[pos] == '\r') {
		pos++
	}
	if pos >= n || doc[pos] != '{' {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := pos; i < n; i++ {
		c := doc[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return doc[pos : i+1], true
			}
		}
	}
	return "", false
}

func collectDocKeyPaths(blk, prefix string, out *[]string, level int) {
	if level > 4 || len(blk) < 2 || blk[0] != '{' || blk[len(blk)-1] != '}' {
		return
	}
	inner := blk[1 : len(blk)-1]
	n := len(inner)
	depth := 0
	inStr := false
	esc := false
	expectKey := true
	for i := 0; i < n; i++ {
		c := inner[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if depth == 0 {
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				continue
			}
			if c == ',' {
				expectKey = true
				continue
			}
			if expectKey {
				var key string
				var next int
				if c == '"' {
					j := i + 1
					for j < n && inner[j] != '"' {
						if inner[j] == '\\' {
							j++
						}
						j++
					}
					if j >= n {
						return
					}
					key = inner[i+1 : j]
					next = j + 1
				} else if isIdentStart(c) {
					j := i + 1
					for j < n && isIdentChar(inner[j]) {
						j++
					}
					key = inner[i:j]
					next = j
				}
				if key != "" {
					expectKey = false
					full := key
					if prefix != "" {
						full = prefix + "." + key
					}
					if sub, ok := braceBlockAfterColon(inner, next); ok {
						beforeLen := len(*out)
						collectDocKeyPaths(sub, full, out, level+1)
						if len(*out) == beforeLen {
							*out = append(*out, full)
						}
						// Advance past the sub-block.
						if idx := strings.Index(inner[next:], sub); idx >= 0 {
							i = next + idx + len(sub) - 1
							continue
						}
					} else {
						*out = append(*out, full)
					}
					i = next - 1
					continue
				}
			}
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$'
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '.' || c == '-'
}
