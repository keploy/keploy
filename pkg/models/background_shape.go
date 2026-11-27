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
	s = strings.TrimSpace(s[start+1:])
	end := strings.IndexAny(s, ",}")
	if end >= 0 {
		s = s[:end]
	}
	s = strings.TrimSpace(s)
	if len(s) > 96 {
		s = s[:96]
	}
	return s
}
