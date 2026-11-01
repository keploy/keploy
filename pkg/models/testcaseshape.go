package models

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The shape of a test case: what its request and its response look like,
// without the values they hold. Two test cases have one shape when replaying
// either exercises the same surface of the API:
//
//   - the same protocol, method and route: the path by PathShape, a gRPC call
//     by its :path;
//   - the same query parameter names;
//   - the same status: an HTTP status code, a gRPC call's grpc-status;
//   - the same request and response media types;
//   - the same GraphQL or JSON-RPC operation (graphQLOperation,
//     jsonRPCOperation), which a single route multiplexes;
//   - the same field paths, each with its type, in the request body and in
//     the response body (bodyShape, grpcMessagesShape).
//
// Header names are not part of it: they vary with the client, the proxy and
// the transfer (content-length or transfer-encoding, cookies, tracing), not
// with the API.
//
// The recorder's static dedup keeps one test case per shape (Key), and the
// store stamps each test case with the hash of its shape (CoverHash), from
// which cloud replay tells which shapes a release's test cases cover.

// CoverHashVersion starts every cover hash. A change to what a shape holds
// changes it, so a stored hash of an older definition reads as stale and is
// stamped again.
const CoverHashVersion = "c1"

// coverHashHexLen is the length a cover hash's sha256 is cut to (128 bits).
const coverHashHexLen = 32

// keyHashHexLen is the length a dedup key's sha256 of the shape is cut to.
const keyHashHexLen = 16

// TestCaseShape is a test case's shape (see above).
type TestCaseShape struct {
	// Kind is "http" or "grpc".
	Kind string
	// Method is the HTTP method, upper case; "" for gRPC.
	Method string
	// Route is the request path by PathShape, or a gRPC call's :path.
	Route string
	// Query is the sorted, distinct query parameter names (pathSegmentShape;
	// "{}" for a component with no '=').
	Query []string
	// Status is the HTTP status code, or the gRPC call's grpc-status.
	Status string
	// ReqMedia and RespMedia are the bodies' media types, lower case, without
	// parameters.
	ReqMedia  string
	RespMedia string
	// Operation is the GraphQL or JSON-RPC operation the request runs, "" for
	// none.
	Operation string
	// Req and Resp are the sorted, distinct "path:type" entries of the
	// request and the response body (bodyShape, grpcMessagesShape).
	Req  []string
	Resp []string
}

// HTTPExchange is what an HTTP request and its response's shape is read from.
type HTTPExchange struct {
	Method string
	// Path is the request's path, decoded (url.URL.Path).
	Path string
	// RawQuery is the request's query, without the '?'.
	RawQuery        string
	Status          int
	ReqContentType  string
	RespContentType string
	ReqBody         []byte
	RespBody        []byte
	// Form is a multipart request's fields, whose body the recorder does not
	// keep.
	Form []FormData
}

// HTTPShape is the shape of an HTTP request and its response.
func HTTPShape(x HTTPExchange) TestCaseShape {
	s := TestCaseShape{
		Kind:      "http",
		Method:    strings.ToUpper(x.Method),
		Route:     PathShape(x.Path),
		Query:     queryNameShapes(x.RawQuery),
		Status:    strconv.Itoa(x.Status),
		ReqMedia:  mediaType(x.ReqContentType),
		RespMedia: mediaType(x.RespContentType),
	}
	req, reqDoc := bodyShape(s.ReqMedia, x.ReqBody, x.Form)
	s.Req = req
	s.Resp, _ = bodyShape(s.RespMedia, x.RespBody, nil)
	s.Operation = requestOperation(s.ReqMedia, x.ReqBody, reqDoc, x.RawQuery)
	return s
}

// GRPCShape is the shape of a gRPC call: its :path, its grpc-status (0 when
// the answer carries none) and the field paths of every message each way.
func GRPCShape(req *GrpcReq, resp *GrpcResp) TestCaseShape {
	status := resp.TrailersAsRead().OrdinaryHeaders["grpc-status"]
	if status == "" {
		status = "0"
	}
	return TestCaseShape{
		Kind:   "grpc",
		Route:  req.Headers.PseudoHeaders[":path"],
		Status: status,
		Req:    grpcMessagesShape(req.AllMessages()),
		Resp:   grpcMessagesShape(resp.AllMessages()),
	}
}

// ShapeOf is a test case's shape, read from the test case as it is stored. It
// is false for a test case of a kind that has none (neither HTTP nor gRPC).
func ShapeOf(tc *TestCase) (TestCaseShape, bool) {
	if tc == nil {
		return TestCaseShape{}, false
	}
	switch tc.Kind {
	case GRPC_EXPORT:
		return GRPCShape(&tc.GrpcReq, &tc.GrpcResp), true
	case HTTP, HTTP2:
		path, rawQuery := splitTarget(tc.HTTPReq.URL)
		return HTTPShape(HTTPExchange{
			Method:          string(tc.HTTPReq.Method),
			Path:            path,
			RawQuery:        rawQuery,
			Status:          tc.HTTPResp.StatusCode,
			ReqContentType:  headerValue(tc.HTTPReq.Header, "Content-Type"),
			RespContentType: headerValue(tc.HTTPResp.Header, "Content-Type"),
			ReqBody:         []byte(tc.HTTPReq.Body),
			RespBody:        []byte(tc.HTTPResp.Body),
			Form:            tc.HTTPReq.Form,
		}), true
	}
	return TestCaseShape{}, false
}

// CoverHash is the hash of a test case's shape as a store keeps it:
// CoverHashVersion, ':' and the first 32 hex digits of the sha256 of the
// shape (Canonical). It is "" for a test case of a kind that has no shape.
func CoverHash(tc *TestCase) string {
	s, ok := ShapeOf(tc)
	if !ok {
		return ""
	}
	return s.CoverHash()
}

// CoverHash is the shape's cover hash (see CoverHash).
func (s TestCaseShape) CoverHash() string {
	sum := sha256.Sum256([]byte(s.Canonical()))
	return CoverHashVersion + ":" + hex.EncodeToString(sum[:])[:coverHashHexLen]
}

// Key is the shape as the recorder's static dedup keys it:
// "v2|METHOD|route|status|hash" ("v2|GRPC|:path|grpc-status|hash" for gRPC),
// the hash being the first 16 hex digits of the sha256 of Canonical. The
// method, the route and the status stay readable for the dedup's stats.
func (s TestCaseShape) Key() string {
	method := s.Method
	if s.Kind == "grpc" {
		method = "GRPC"
	}
	sum := sha256.Sum256([]byte(s.Canonical()))
	return "v2|" + method + "|" + s.Route + "|" + s.Status + "|" + hex.EncodeToString(sum[:])[:keyHashHexLen]
}

// Canonical is the shape written out, one labelled line per part and one per
// body entry, so two shapes write the same text exactly when they are equal.
// No part holds a newline: a body entry's path quotes a key that could
// (shapeKey), and the other parts are a token, a shaped path or a media type.
func (s TestCaseShape) Canonical() string {
	var b strings.Builder
	line := func(label, v string) {
		b.WriteString(label)
		b.WriteByte(' ')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	line("kind", s.Kind)
	line("method", s.Method)
	line("route", s.Route)
	for _, q := range s.Query {
		line("query", q)
	}
	line("status", s.Status)
	line("req.media", s.ReqMedia)
	line("resp.media", s.RespMedia)
	line("operation", s.Operation)
	for _, e := range s.Req {
		line("req", e)
	}
	for _, e := range s.Resp {
		line("resp", e)
	}
	return b.String()
}

// splitTarget is a stored request URL's decoded path and raw query. The URL
// is absolute ("http://host/path?q") or a request target ("/path?q").
func splitTarget(raw string) (path, rawQuery string) {
	if u, err := url.Parse(raw); err == nil {
		return u.Path, u.RawQuery
	}
	path, rawQuery, _ = strings.Cut(raw, "?")
	if i := strings.Index(path, "://"); i >= 0 {
		rest := path[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[j:], rawQuery
		}
		return "/", rawQuery
	}
	return path, rawQuery
}

// headerValue is a header's value whatever the case of its name.
func headerValue(h map[string]string, name string) string {
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

// queryNameShapes is a query's parameter names as PathShape keeps them
// (pathSegmentShape; "{}" for a component with no '='), sorted and distinct.
func queryNameShapes(rawQuery string) []string {
	set := map[string]struct{}{}
	for _, kv := range strings.Split(rawQuery, "&") {
		if kv == "" {
			continue
		}
		k, _, hasValue := strings.Cut(kv, "=")
		if !hasValue {
			set[pathHole] = struct{}{}
			continue
		}
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		set[pathSegmentShape(k)] = struct{}{}
	}
	return sortedKeys(set)
}

// mediaType is a Content-Type's media type, lower case, without parameters.
func mediaType(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	mt, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

func isJSONMedia(mt string) bool {
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func isXMLMedia(mt string) bool {
	return mt == "application/xml" || mt == "text/xml" || strings.HasSuffix(mt, "+xml")
}

// Body entry types: an entry is "path:type". The root's path is "$", an
// object's field is its path, '.' and the field's name (shapeKey), and an
// array's items are its path and "[]", every item adding its own entries, so
// an array's entries are the union of its items'.
const (
	shapeObject  = "o"
	shapeArray   = "a"
	shapeString  = "s"
	shapeNumber  = "n"
	shapeBool    = "b"
	shapeNull    = "z"
	shapeElement = "e" // an XML element
	shapeFile    = "f" // a form field holding a file
	shapeInvalid = "!" // a body its media type says is JSON or XML that does not parse
)

// bodyShape is a body's entries (see the types above), and the body parsed as
// JSON when it is JSON (nil otherwise), which requestOperation reads.
//
//   - JSON (a JSON media type, or none and a body that parses as a JSON object
//     or array): every value's path and type (walkJSON).
//   - A form: a url-encoded body's field names, or a multipart request's
//     fields (form), each a string or a file.
//   - XML: every element's path, its attributes and whether it holds text.
//   - Any other body: no entries; its media type tells it apart.
//
// An empty body has no entries, unlike an empty JSON object ("$:o").
func bodyShape(media string, body []byte, form []FormData) ([]string, any) {
	set := map[string]struct{}{}
	for _, f := range form {
		t := shapeString
		if len(f.Paths) > 0 || len(f.FileNames) > 0 {
			t = shapeFile
		}
		addShape(set, "$."+shapeKey(f.Key), t)
	}
	trimmed := bytes.TrimSpace(body)
	var doc any
	switch {
	case len(trimmed) == 0:
	case isJSONMedia(media) || (media == "" && (trimmed[0] == '{' || trimmed[0] == '[')):
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			if isJSONMedia(media) {
				addShape(set, "$", shapeInvalid)
			}
			doc = nil
			break
		}
		walkJSON(doc, "$", set)
	case media == "application/x-www-form-urlencoded":
		vals, err := url.ParseQuery(string(trimmed))
		if err != nil {
			addShape(set, "$", shapeInvalid)
			break
		}
		for k := range vals {
			addShape(set, "$."+shapeKey(k), shapeString)
		}
	case isXMLMedia(media):
		walkXML(trimmed, set)
	}
	return sortedKeys(set), doc
}

// walkJSON adds the entries of a parsed JSON value at path.
func walkJSON(v any, path string, set map[string]struct{}) {
	switch x := v.(type) {
	case map[string]any:
		addShape(set, path, shapeObject)
		for k, c := range x {
			walkJSON(c, path+"."+shapeKey(k), set)
		}
	case []any:
		addShape(set, path, shapeArray)
		items := path + "[]"
		for _, c := range x {
			walkJSON(c, items, set)
		}
	case string:
		addShape(set, path, shapeString)
	case float64, json.Number:
		addShape(set, path, shapeNumber)
	case bool:
		addShape(set, path, shapeBool)
	case nil:
		addShape(set, path, shapeNull)
	}
}

// walkXML adds an XML document's entries: each element's path (its local
// names from the root, after "$") as an element, each attribute but a
// namespace declaration as the element's path, '@' and its name, and an
// element that holds text as a string as well.
func walkXML(body []byte, set map[string]struct{}) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	stack := []string{"$"}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			addShape(set, "$", shapeInvalid)
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			p := stack[len(stack)-1] + "." + shapeKey(t.Name.Local)
			stack = append(stack, p)
			addShape(set, p, shapeElement)
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					continue
				}
				addShape(set, p+"@"+shapeKey(a.Name.Local), shapeString)
			}
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 1 && len(bytes.TrimSpace(t)) > 0 {
				addShape(set, stack[len(stack)-1], shapeString)
			}
		}
	}
}

// shapeKey is a field name as a body entry's path holds it: as it is when it
// holds only letters, digits and "_-$@", else quoted (strconv.Quote), so no
// two paths read alike and no entry holds a newline.
func shapeKey(k string) string {
	if k == "" {
		return `""`
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !isASCIILetter(c) && !isDigitByte(c) && c != '_' && c != '-' && c != '$' && c != '@' {
			return strconv.Quote(k)
		}
	}
	return k
}

func addShape(set map[string]struct{}, path, typ string) {
	set[path+":"+typ] = struct{}{}
}

func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// requestOperation is the operation a request runs when its route multiplexes
// several: a GraphQL operation (graphQLRequestOperation) or a JSON-RPC method
// (jsonRPCOperation); "" for neither.
func requestOperation(media string, body []byte, doc any, rawQuery string) string {
	if op := graphQLRequestOperation(media, body, doc, rawQuery); op != "" {
		return op
	}
	return jsonRPCOperation(doc)
}

// jsonRPCOperation is a JSON-RPC request's method ("jsonrpc:<method>"), or a
// batch's distinct methods, sorted and joined by ','; "" when the body is no
// JSON-RPC request.
func jsonRPCOperation(doc any) string {
	method := func(v any) (string, bool) {
		obj, ok := v.(map[string]any)
		if !ok {
			return "", false
		}
		if _, ok := obj["jsonrpc"].(string); !ok {
			return "", false
		}
		m, ok := obj["method"].(string)
		return m, ok
	}
	if m, ok := method(doc); ok {
		return "jsonrpc:" + m
	}
	arr, ok := doc.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	set := map[string]struct{}{}
	for _, v := range arr {
		m, ok := method(v)
		if !ok {
			return ""
		}
		set[m] = struct{}{}
	}
	return "jsonrpc:" + strings.Join(sortedKeys(set), ",")
}
