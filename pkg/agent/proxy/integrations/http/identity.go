package http

import (
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/identity"
)

// aliasIdentity rewrites the replay pod's own identity to the recorded pod's
// in a live request before it is matched: the URL (path, raw path, query, and
// the host, for a call the app addresses to its own IP), every header value
// (the Host among them: the destination compare reads it) and the body. The
// body as the client sent it is kept (req.sent), because a SigV4 payload hash
// is a hash of those bytes. A nil rewriter leaves the request as it is.
func aliasIdentity(rw *identity.Rewriter, input *req) {
	if rw == nil || input == nil {
		return
	}
	if input.url != nil {
		u := *input.url
		u.Path = rw.String(u.Path)
		u.RawPath = rw.String(u.RawPath)
		u.RawQuery = rw.String(u.RawQuery)
		u.Host = rw.String(u.Host)
		input.url = &u
	}
	if len(input.header) > 0 {
		input.header = aliasHeader(rw, input.header)
	}
	if body := rw.Bytes(input.body); len(body) > 0 && &body[0] != &input.body[0] {
		input.sent = input.sentBody()
		input.body = body
	}
}

// aliasHeader is h with every value rewritten; h itself when none changed.
func aliasHeader(rw *identity.Rewriter, h http.Header) http.Header {
	var out http.Header
	for k, vs := range h {
		for i, v := range vs {
			nv := rw.String(v)
			if nv == v {
				continue
			}
			if out == nil {
				out = h.Clone()
			}
			out[k][i] = nv
		}
	}
	if out == nil {
		return h
	}
	return out
}

// aliasResponse puts the replay pod's own identity back into a recorded
// response that is about to be served: its header values, and its body when
// the body is text (JSON, XML, YAML, form, plain, JavaScript) in valid UTF-8.
// A binary body (protobuf, an image, an archive, an Office document, a body
// that names no Content-Type) is served as recorded: a length-prefixed
// format would not survive a rewrite that changes lengths. Content-Length is
// recomputed by the caller over the bytes it sends.
func aliasResponse(reverse *identity.Rewriter, header map[string][]string, body string) (string, map[string][]string) {
	if reverse == nil {
		return body, header
	}
	if len(header) > 0 {
		var out map[string][]string
		for k, vs := range header {
			for i, v := range vs {
				nv := reverse.String(v)
				if nv == v {
					continue
				}
				if out == nil {
					out = make(map[string][]string, len(header))
					for kk, vv := range header {
						out[kk] = append([]string(nil), vv...)
					}
				}
				out[k][i] = nv
			}
		}
		if out != nil {
			header = out
		}
	}
	if body != "" && textualContentType(header) && utf8.ValidString(body) {
		body = reverse.String(body)
	}
	return body, header
}

// textualContentType reports a response whose body is text the reverse
// rewrite may edit: a text/* type, a textual application type, or a +json,
// +xml or +yaml one. The type is matched whole: an Office document's type
// (application/vnd.openxmlformats-officedocument...) names xml but is a ZIP.
// No Content-Type is not taken as text: the kube API and most JSON services
// name one, and a body that names none may be anything.
func textualContentType(header map[string][]string) bool {
	var ct string
	for k, vs := range header {
		if strings.EqualFold(k, "Content-Type") && len(vs) > 0 {
			ct = vs[0]
			break
		}
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	typ, sub, _ := strings.Cut(mt, "/")
	if typ == "text" {
		return true
	}
	if typ != "application" {
		return false
	}
	if strings.HasSuffix(sub, "+json") || strings.HasSuffix(sub, "+xml") || strings.HasSuffix(sub, "+yaml") {
		return true
	}
	switch sub {
	case "json", "xml", "yaml", "x-yaml", "javascript", "x-javascript", "x-www-form-urlencoded", "x-ndjson", "graphql":
		return true
	}
	return false
}
