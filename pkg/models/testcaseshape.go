package models

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The shape of a test case: what its request and its response look like,
// without the values they hold. Two test cases have one shape when replaying
// either exercises the same surface of the API:
//
//   - the same protocol, method and route: the path by shapeRoute, a gRPC
//     call by its :path;
//   - the same query parameter names;
//   - the same status: an HTTP status code, a gRPC call's grpc-status;
//   - the same request and response media types;
//   - the same GraphQL or JSON-RPC operation (graphQLOperation,
//     jsonRPCOperation), which a single route multiplexes;
//   - the same field paths, each with its types, in the request body and in
//     the response body (bodyShape, grpcMessagesShape).
//
// Header names are not part of it: they vary with the client, the proxy and
// the transfer (content-length or transfer-encoding, cookies, tracing), not
// with the API.
//
// Nothing in a body is cut off: a shape reads a body in full, however deep it
// nests, in time and memory linear in its size (shapeNode), so two bodies
// that differ anywhere have two shapes. The only bound is the capture's: a
// call over MaxTestCaseSize is never recorded.
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
	// Route is the request path by shapeRoute, or a gRPC call's :path.
	Route string
	// Query is the sorted, distinct query parameter names (queryNameShapes).
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
	// Req and Resp are the request's and the response's body shapes, written
	// out (shapeNode.String): "" for a body with none.
	Req  string
	Resp string
}

// HTTPExchange is what an HTTP request and its response's shape is read from.
type HTTPExchange struct {
	Method string
	// URL is the request's URL as sent, escaped: a request target
	// ("/path?query", url.URL.RequestURI) or an absolute URL (a stored test
	// case's).
	URL             string
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
	path, rawQuery := splitTarget(x.URL)
	s := TestCaseShape{
		Kind:      "http",
		Method:    strings.ToUpper(x.Method),
		Route:     shapeRoute(path),
		Query:     queryNameShapes(rawQuery),
		Status:    strconv.Itoa(x.Status),
		ReqMedia:  mediaType(x.ReqContentType),
		RespMedia: mediaType(x.RespContentType),
	}
	req, reqJSON := bodyShape(s.ReqMedia, x.ReqBody, x.Form, roleEnvelope)
	resp, _ := bodyShape(s.RespMedia, x.RespBody, nil, roleNone)
	s.Req, s.Resp = req.String(), resp.String()
	s.Operation = requestOperation(s.ReqMedia, x.ReqBody, reqJSON, rawQuery)
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
		return HTTPShape(HTTPExchange{
			Method:          string(tc.HTTPReq.Method),
			URL:             tc.HTTPReq.URL,
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

// Canonical is the shape written out, one labelled line per part, so two
// shapes write the same text exactly when they are equal. Every part but the
// bodies is quoted (strconv.Quote), as a request can put any text in it (a
// :path, an operation name, a JSON-RPC method); a body's shape holds no
// newline (shapeNode.String).
func (s TestCaseShape) Canonical() string {
	var b strings.Builder
	line := func(label, v string) {
		b.WriteString(label)
		b.WriteByte(' ')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	quoted := func(label, v string) { line(label, strconv.Quote(v)) }
	quoted("kind", s.Kind)
	quoted("method", s.Method)
	quoted("route", s.Route)
	for _, q := range s.Query {
		quoted("query", q)
	}
	quoted("status", s.Status)
	quoted("req.media", s.ReqMedia)
	quoted("resp.media", s.RespMedia)
	quoted("operation", s.Operation)
	line("req", s.Req)
	line("resp", s.Resp)
	return b.String()
}

// splitTarget is a stored request URL's escaped path and raw query. The URL
// is absolute ("http://host/path?q") or a request target ("/path?q").
func splitTarget(raw string) (escapedPath, rawQuery string) {
	if u, err := url.Parse(raw); err == nil {
		return u.EscapedPath(), u.RawQuery
	}
	path, rawQuery, _ := strings.Cut(raw, "?")
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

// shapeRoute is a request path, as sent (escaped), as a shape keeps it: each
// piece between two '/' decoded, then written {} when it reads as a value
// (isValueSegment), or else escaped again (url.PathEscape), so a piece that
// holds an escaped '/' stays one piece and a literal "{}" is no value. An
// empty path is "/"; a trailing '/' counts.
//
// It is not PathShape: the proven mocks' rule writes {} for any piece with a
// digit, which would merge /integrations/s3 and /integrations/ec2 into one
// shape, so the recorder would drop the second as a duplicate and coverage
// would count it covered.
func shapeRoute(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	segs := strings.Split(escapedPath, "/")
	for i, s := range segs {
		d, err := url.PathUnescape(s)
		if err != nil {
			d = s
		}
		if isValueSegment(d) {
			segs[i] = pathHole
		} else {
			segs[i] = url.PathEscape(d)
		}
	}
	return strings.Join(segs, "/")
}

// Bounds of the pieces isValueSegment reads as values.
const (
	// shapeMaxName is the longest piece with a digit kept as a name: a longer
	// one is an ID or a token (a UUID is 36, a Mongo ObjectID 24).
	shapeMaxName = 20
	// shapeMinHex is the shortest run of hex digits that is a hash or an ID.
	shapeMinHex = 8
)

// isValueSegment reports whether a decoded path piece or query name reads as
// a value rather than a name. A value holds a digit, and either no letter (a
// number, a date, a version, an IP address: 42, 2024-01-02, 4.17.21), or only
// hex digits, shapeMinHex or more of them (5f3a9c2e), or more than
// shapeMaxName bytes (a UUID, an ObjectID, a token). Any other piece is a
// name: one with a digit that is short and not hex (s3, ec2, k8s, md5,
// sha256, oauth2, e2e, v1, acct_42), and one without a digit however long
// (notification-preferences, a slug), so no two names ever merge.
func isValueSegment(s string) bool {
	digit, letter, hexOnly := false, false, true
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case unicode.IsDigit(r):
			digit, hexOnly = true, false
		case unicode.IsLetter(r):
			letter = true
			if !strings.ContainsRune("abcdefABCDEF", r) {
				hexOnly = false
			}
		default:
			hexOnly = false
		}
	}
	return digit && (!letter || (hexOnly && len(s) >= shapeMinHex) || len(s) > shapeMaxName)
}

// queryNameShapes is a query's parameter names, sorted and distinct: each
// name decoded, then {} when it reads as a value (isValueSegment) or else
// escaped again (url.QueryEscape); a component with no '=' (a flag or a bare
// value, as in ?1697040000) is {}.
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
		if isValueSegment(k) {
			set[pathHole] = struct{}{}
		} else {
			set[url.QueryEscape(k)] = struct{}{}
		}
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

// shapeTypes is the set of types a body shows at one path.
type shapeTypes uint16

// The types of a body's values, each written as one letter of
// shapeTypeLetters, in that order.
const (
	shapeObject          shapeTypes = 1 << iota // o
	shapeArray                                  // a
	shapeString                                 // s
	shapeNumber                                 // n
	shapeBool                                   // b
	shapeNull                                   // z
	shapeElement                                // e: an XML element
	shapeFile                                   // f: a form field holding a file
	shapeMessage                                // m: a gRPC direction that carries a message
	shapeLengthDelimited                        // l: a length-delimited protobuf field
	shapeInvalid                                // !: a body its media type says is JSON or XML that does not parse
)

const shapeTypeLetters = "oasnbzefml!"

func (t shapeTypes) writeTo(b *strings.Builder) {
	for i := 0; i < len(shapeTypeLetters); i++ {
		if t&(1<<i) != 0 {
			b.WriteByte(shapeTypeLetters[i])
		}
	}
}

// shapeNode is one path of a body's shape: the types the body shows there, a
// child per field name (an object's member, an XML child element or
// attribute, a protobuf field number) and one child for an array's items,
// which merges every item, so an array's shape is the union of its items'
// and its length does not count.
//
// A node is built once per distinct path, from a single pass over the body,
// and no path is ever written out, so a body costs time and memory linear in
// its size however deep it nests. A node is small (a body nested a million
// deep holds a million of them): its fields are a list, indexed by a map
// only once there are more than shapeFieldScan of them.
type shapeNode struct {
	fields *shapeFields
	items  *shapeNode
	types  shapeTypes
}

type shapeFields struct {
	list  []shapeField
	index map[string]*shapeNode
}

type shapeField struct {
	name string
	node *shapeNode
}

// shapeFieldScan is how many fields a node looks a name up among one by one.
const shapeFieldScan = 8

func (n *shapeNode) field(name string) *shapeNode {
	f := n.fields
	if f == nil {
		f = &shapeFields{}
		n.fields = f
	}
	if f.index != nil {
		if c := f.index[name]; c != nil {
			return c
		}
	} else {
		for i := range f.list {
			if f.list[i].name == name {
				return f.list[i].node
			}
		}
	}
	c := &shapeNode{}
	f.list = append(f.list, shapeField{name: name, node: c})
	switch {
	case f.index != nil:
		f.index[name] = c
	case len(f.list) > shapeFieldScan:
		f.index = make(map[string]*shapeNode, 2*len(f.list))
		for _, e := range f.list {
			f.index[e.name] = e.node
		}
	}
	return c
}

func (n *shapeNode) item() *shapeNode {
	if n.items == nil {
		n.items = &shapeNode{}
	}
	return n.items
}

// String is the shape written out: a node is its types' letters, then its
// fields in braces, sorted by name, each its name (shapeKey), ':' and its
// node, joined by ',', then its items' node in brackets.
// {"items":[{"sku":"a","qty":1}],"note":null} is o{items:a[o{qty:n,sku:s}],note:z}.
// It is written without recursion, one small frame per open node, however
// deep the shape.
func (n *shapeNode) String() string {
	if n == nil {
		return ""
	}
	const (
		atTypes = iota
		inFields
		atItems
		inItems
	)
	type frame struct {
		node  *shapeNode
		next  int32 // the field written next
		stage uint8
	}
	var b strings.Builder
	stack := []frame{{node: n}}
	for len(stack) > 0 {
		f := &stack[len(stack)-1]
		switch f.stage {
		case atTypes:
			f.node.types.writeTo(&b)
			f.stage = atItems
			if f.node.fields != nil && len(f.node.fields.list) > 0 {
				list := f.node.fields.list
				sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
				b.WriteByte('{')
				f.stage = inFields
			}
		case inFields:
			list := f.node.fields.list
			if int(f.next) == len(list) {
				b.WriteByte('}')
				f.stage = atItems
				continue
			}
			if f.next > 0 {
				b.WriteByte(',')
			}
			e := list[f.next]
			f.next++
			b.WriteString(shapeKey(e.name))
			b.WriteByte(':')
			stack = append(stack, frame{node: e.node})
		case atItems:
			if f.node.items == nil {
				stack = stack[:len(stack)-1]
				continue
			}
			b.WriteByte('[')
			f.stage = inItems
			stack = append(stack, frame{node: f.node.items})
		case inItems:
			b.WriteByte(']')
			stack = stack[:len(stack)-1]
		}
	}
	return b.String()
}

// bodyShape is a body's shape, and what it says of the operation it runs when
// it is JSON (nil otherwise), which requestOperation reads; role is what the
// JSON root is (roleEnvelope for a request, roleNone for a response).
//
//   - JSON (a JSON media type, or none or text/plain and a body that parses
//     as a JSON object or array): every value's path and type (scanJSON).
//   - A form: a url-encoded body's field names (formFieldNames), or a
//     multipart request's fields, each a string or a file.
//   - XML: every element's path, its attributes and whether it holds text
//     (xmlShape).
//   - Any other body: none; its media type tells it apart.
//
// An empty body has none, unlike an empty JSON object ("o").
func bodyShape(media string, body []byte, form []FormData, role jsonRole) (*shapeNode, *jsonRequest) {
	root := &shapeNode{}
	var req *jsonRequest
	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) == 0:
	case isJSONMedia(media):
		tree, r, ok := scanJSON(string(trimmed), role)
		if !ok {
			root.types |= shapeInvalid
			break
		}
		root, req = tree, r
	case (media == "" || media == "text/plain") && (trimmed[0] == '{' || trimmed[0] == '['):
		// No media type promised JSON, so a body that does not parse is opaque.
		if tree, r, ok := scanJSON(string(trimmed), role); ok {
			root, req = tree, r
		}
	case media == "application/x-www-form-urlencoded":
		for _, k := range formFieldNames(string(trimmed)) {
			root.field(k).types |= shapeString
		}
	case isXMLMedia(media):
		xmlShape(trimmed, root)
	}
	for _, f := range form {
		t := shapeString
		if len(f.Paths) > 0 || len(f.FileNames) > 0 {
			t = shapeFile
		}
		root.field(f.Key).types |= t
	}
	return root, req
}

// formFieldNames is a url-encoded body's field names: each '&'-separated
// component up to its '=', decoded when it decodes. A component that does
// not parse still names a field, unlike in url.ParseQuery, which drops it.
func formFieldNames(body string) []string {
	var names []string
	for _, kv := range strings.Split(body, "&") {
		if kv == "" {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		names = append(names, k)
	}
	return names
}

// xmlShape adds an XML document's shape to root: each element under its
// parent by its local name, each attribute but a namespace declaration under
// its element by '@' and its name (an element's name holds no '@'), and an
// element that holds text as a string as well. It reads raw tokens
// (xml.Decoder.RawToken), so the decoder keeps no stack of open elements;
// its own stack is one pointer per open element. A document that does not
// parse, or ends with an element open, keeps what was read and is marked
// invalid.
func xmlShape(body []byte, root *shapeNode) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	stack := []*shapeNode{root}
	names := map[string]string{} // each name once, however often it is read
	intern := func(n string) string {
		if s, ok := names[n]; ok {
			return s
		}
		names[n] = n
		return n
	}
	for {
		tok, err := dec.RawToken()
		if errors.Is(err, io.EOF) {
			if len(stack) > 1 {
				root.types |= shapeInvalid
			}
			return
		}
		if err != nil {
			root.types |= shapeInvalid
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := stack[len(stack)-1].field(intern(t.Name.Local))
			n.types |= shapeElement
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					continue
				}
				n.field(intern("@" + a.Name.Local)).types |= shapeString
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 1 && len(bytes.TrimSpace(t)) > 0 {
				stack[len(stack)-1].types |= shapeString
			}
		}
	}
}

// shapeKey is a field name as a body's shape writes it: as it is when it
// holds only letters, digits and "_-$@", else quoted (strconv.Quote), so no
// two names read alike and none holds a newline or a character of the
// shape's own syntax.
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
func requestOperation(media string, body []byte, req *jsonRequest, rawQuery string) string {
	if op := graphQLRequestOperation(media, body, req, rawQuery); op != "" {
		return op
	}
	return jsonRPCOperation(req)
}

// jsonRPCOperation is a JSON-RPC request's method ("jsonrpc:" and the method
// quoted), or a batch's distinct methods, each quoted, sorted and joined by
// ','; "" when the body is no JSON-RPC request.
func jsonRPCOperation(req *jsonRequest) string {
	if req == nil || req.envelopes == 0 || req.notJSONRPC {
		return ""
	}
	return "jsonrpc:" + strings.Join(sortedKeys(req.jsonRPC), ",")
}
