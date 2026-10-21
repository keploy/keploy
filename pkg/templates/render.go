package templates

import (
	"net/url"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// lookupFunc gives the text a placeholder is replaced with, or false to leave
// the placeholder as written.
type lookupFunc func(p Placeholder) (string, bool)

// use is a use placeholder's value now. A capture is never a use: it stays as
// written outside an expected response's capture step. An undefined key stays
// as written (Check names it once, when the test set loads).
func (r *Run) use(p Placeholder) (string, bool) {
	if p.Capture {
		return "", false
	}
	return r.current(p)
}

// asRecorded is a placeholder's config.yaml value, captures included: the
// text the recording held where the placeholder now is.
func (r *Run) asRecorded(p Placeholder) (string, bool) {
	return r.recordedValue(p)
}

// recordedUse is a use placeholder's config.yaml value; a capture stays as
// written.
func (r *Run) recordedUse(p Placeholder) (string, bool) {
	if p.Capture {
		return "", false
	}
	return r.recordedValue(p)
}

// Request is req with each placeholder holding this run's value, escaped for
// where it sits: URL path and query, a form body, inside or as a whole JSON
// value, or a header or form field as it is. A secret is the text the
// recording held, sent as recorded in a URL and a form body (variablesOnly)
// and JSON-escaped inside a JSON string, where keploy sanitize stored it
// decoded. req itself is not changed, and
// the copy shares no map or slice with it, so whatever sends it may change it
// freely and a re-send starts from the test case as written.
func (r *Run) Request(req models.HTTPReq) models.HTTPReq {
	out := req
	out.URLParams = copyStringMap(req.URLParams)
	out.Header = copyStringMap(req.Header)
	out.Form = copyForm(req.Form)
	if r.Empty() || !anyPlaceholder(req) {
		return out
	}
	look := r.use
	out.URL = renderURL(req.URL, look, r)
	for k, v := range out.URLParams {
		out.URLParams[k] = renderRaw(v, look, r)
	}
	out.Header = r.renamed(out.Header)
	for k, v := range out.Header {
		out.Header[k] = renderRaw(v, look, r)
	}
	for i := range out.Form {
		out.Form[i].Key = renderRaw(out.Form[i].Key, r.secretOnly, r)
		for j, v := range out.Form[i].Values {
			out.Form[i].Values[j] = renderRaw(v, look, r)
		}
	}
	out.Body = renderBody(req.Body, headerValue(req.Header, "Content-Type"), look, r)
	return out
}

// secretOnly is a secret's value; any other placeholder stays as written. It
// renders a name: a name is matched as written, a variable in one is refused
// when the set loads (Check), and a secret there is the text the recording
// held, rendered as main rendered every name.
func (r *Run) secretOnly(p Placeholder) (string, bool) {
	if p.Capture || !isSecret(p) {
		return "", false
	}
	return r.current(p)
}

// renamed is m with each name that holds a secret at the secret's value
// (secretOnly): m itself when no name changes, else a new map.
func (r *Run) renamed(m map[string]string) map[string]string {
	var out map[string]string
	for k, v := range m {
		if !Contains(k) {
			continue
		}
		nk := renderRaw(k, r.secretOnly, r)
		if nk == k {
			continue
		}
		if out == nil {
			out = copyStringMap(m)
		}
		delete(out, k)
		out[nk] = v
	}
	if out == nil {
		return m
	}
	return out
}

// GRPCRequest is req with each placeholder holding this run's value, on a copy.
func (r *Run) GRPCRequest(req models.GrpcReq) models.GrpcReq {
	out := req
	out.Headers = copyGRPCHeaders(req.Headers)
	out.AdditionalMessages = append([]models.GrpcLengthPrefixedMessage(nil), req.AdditionalMessages...)
	if r.Empty() {
		return out
	}
	renderGRPCHeaders(&out.Headers, r.use, r)
	out.Body.DecodedData = renderRaw(out.Body.DecodedData, r.use, r)
	for i := range out.AdditionalMessages {
		out.AdditionalMessages[i].DecodedData = renderRaw(out.AdditionalMessages[i].DecodedData, r.use, r)
	}
	return out
}

// GRPCExpected is an expected gRPC response with each use placeholder holding
// this run's value, on a copy. gRPC responses never capture. Its trailers are
// rendered too: only a secret can stand there (Check refuses a variable),
// the text the recording held, as main rendered it.
func (r *Run) GRPCExpected(exp models.GrpcResp) models.GrpcResp {
	out := exp
	out.Headers = copyGRPCHeaders(exp.Headers)
	out.Trailers = copyGRPCHeaders(exp.Trailers)
	out.AdditionalMessages = append([]models.GrpcLengthPrefixedMessage(nil), exp.AdditionalMessages...)
	if r.Empty() {
		return out
	}
	renderGRPCHeaders(&out.Headers, r.use, r)
	renderGRPCHeaders(&out.Trailers, r.secretOnly, r)
	out.Body.DecodedData = renderRaw(out.Body.DecodedData, r.use, r)
	for i := range out.AdditionalMessages {
		out.AdditionalMessages[i].DecodedData = renderRaw(out.AdditionalMessages[i].DecodedData, r.use, r)
	}
	return out
}

func renderGRPCHeaders(h *models.GrpcHeaders, look lookupFunc, r *Run) {
	h.PseudoHeaders, h.OrdinaryHeaders = r.renamed(h.PseudoHeaders), r.renamed(h.OrdinaryHeaders)
	for k, v := range h.PseudoHeaders {
		h.PseudoHeaders[k] = renderRaw(v, look, r)
	}
	for k, v := range h.OrdinaryHeaders {
		h.OrdinaryHeaders[k] = renderRaw(v, look, r)
	}
}

// valueFor is the text a placeholder renders to in a context with no syntax
// of its own: the value as it is, which for int and float must be a number.
func valueFor(p Placeholder, look lookupFunc, r *Run) (string, bool) {
	v, ok := look(p)
	if !ok {
		return "", false
	}
	if p.Type == TypeInt || p.Type == TypeFloat {
		if _, fits := typed(p.Type, v); !fits {
			r.warnOnce("a placeholder's value is not a number of its type; it is left as written", p.Key, v,
				zap.String("placeholder", Shape(p.Text)))
			return "", false
		}
	}
	return v, true
}

// splice replaces each placeholder in s that look gives a value, after escape.
func splice(s string, ps []Placeholder, look lookupFunc, r *Run, escape func(string) string) string {
	return spliceEach(s, ps, look, r, func(_ Placeholder, v string) string { return escape(v) })
}

// spliceEach is splice with an escape chosen per placeholder.
func spliceEach(s string, ps []Placeholder, look lookupFunc, r *Run, escape func(Placeholder, string) string) string {
	if len(ps) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, p := range ps {
		v, ok := valueFor(p, look, r)
		if !ok {
			continue
		}
		b.WriteString(s[last:p.Start])
		b.WriteString(escape(p, v))
		last = p.End
	}
	b.WriteString(s[last:])
	return b.String()
}

func identity(s string) string { return s }

// renderRaw is for a header, a form field, a URL param: no escaping.
func renderRaw(s string, look lookupFunc, r *Run) string {
	return splice(s, Find(s), look, r, identity)
}

// variablesOnly applies escape to a variable's value and leaves a secret's as
// it is. A secret is the text the recording held where keploy sanitize wrote
// it ({{string .secret.K}}): sanitize stores the whole scalar that held the
// secret as recorded, already in the syntax of its place (a URL with its
// query escaped, a form body), so it is sent exactly as recorded, as main
// sent it, and escaping it again would change what is sent.
func variablesOnly(escape func(string) string) func(Placeholder, string) string {
	return func(p Placeholder, v string) string {
		if isSecret(p) {
			return v
		}
		return escape(v)
	}
}

// renderURL escapes a variable's value for the URL part it lands in: the
// path, the query, or (as it is) the fragment. A secret is sent as recorded
// (variablesOnly).
func renderURL(u string, look lookupFunc, r *Run) string {
	ps := Find(u)
	if len(ps) == 0 {
		return u
	}
	queryAt, fragmentAt := len(u), len(u)
	if i := strings.IndexByte(u, '#'); i >= 0 {
		fragmentAt = i
		queryAt = i
	}
	if i := strings.IndexByte(u[:fragmentAt], '?'); i >= 0 {
		queryAt = i
	}
	var b strings.Builder
	last := 0
	for _, p := range ps {
		v, ok := valueFor(p, look, r)
		if !ok {
			continue
		}
		b.WriteString(u[last:p.Start])
		switch {
		case isSecret(p):
			b.WriteString(v)
		case p.Start < queryAt:
			b.WriteString(url.PathEscape(v))
		case p.Start < fragmentAt:
			b.WriteString(url.QueryEscape(v))
		default:
			b.WriteString(v)
		}
		last = p.End
	}
	b.WriteString(u[last:])
	return b.String()
}

// renderBody renders a body by what it is: a JSON document, a form, or text.
func renderBody(body, contentType string, look lookupFunc, r *Run) string {
	if !Contains(body) {
		return body
	}
	if d, ok := parseJSON(body); ok {
		doc, nd := r.withKeys(body, d, look)
		return renderJSON(doc, nd.leaves, look, r, nil)
	}
	if isForm(contentType) {
		return spliceEach(body, Find(body), look, r, variablesOnly(url.QueryEscape))
	}
	return renderRaw(body, look, r)
}

// renderJSON renders the placeholders of a JSON document. A string that is
// one int or float placeholder and nothing else becomes that number; a string
// placeholder fills its string, escaped for JSON; a placeholder outside any
// string becomes a whole JSON value of its type. overrides replaces whole
// leaves by index (an expected response's captured answers).
func renderJSON(doc string, leaves []Leaf, look lookupFunc, r *Run, overrides map[int]string) string {
	var b strings.Builder
	last := 0
	for i, l := range leaves {
		repl, ok := overrides[i]
		if !ok {
			repl, ok = renderLeaf(doc, l, look, r)
		}
		if !ok {
			continue
		}
		b.WriteString(doc[last:l.Start])
		b.WriteString(repl)
		last = l.End
	}
	b.WriteString(doc[last:])
	return b.String()
}

// renderKeys renders the placeholders in a JSON document's object keys, keys
// (as parseJSON read them). A key is a JSON string, so each value is spliced
// into it escaped for JSON, a number as its digits.
func renderKeys(doc string, keys []ObjectKey, look lookupFunc, r *Run) string {
	var b strings.Builder
	last := 0
	for _, k := range keys {
		raw := k.Raw(doc)
		ps := Find(raw)
		if len(ps) == 0 {
			continue
		}
		if out := splice(raw, ps, look, r, jsonEscape); out != raw {
			b.WriteString(doc[last:k.Start])
			b.WriteString(out)
			last = k.End
		}
	}
	if last == 0 {
		return doc
	}
	b.WriteString(doc[last:])
	return b.String()
}

// renderLeaf is the text a leaf renders to; false when it does not change.
func renderLeaf(doc string, l Leaf, look lookupFunc, r *Run) (string, bool) {
	raw := l.Raw(doc)
	switch l.Kind {
	case LeafBare:
		ps := Find(raw)
		if len(ps) != 1 {
			return "", false
		}
		v, ok := look(ps[0])
		if !ok {
			return "", false
		}
		out, fits := typed(ps[0].Type, v)
		if !fits {
			r.warnOnce("a placeholder's value is not a number of its type; it is left as written", ps[0].Key, v,
				zap.String("placeholder", Shape(ps[0].Text)))
			return "", false
		}
		return out, true
	case LeafString:
		ps := Find(raw)
		if len(ps) == 0 {
			return "", false
		}
		if whole := wholeString(raw, ps); whole && (ps[0].Type == TypeInt || ps[0].Type == TypeFloat) {
			v, ok := valueFor(ps[0], look, r)
			if !ok {
				return "", false
			}
			return v, true
		}
		out := splice(raw, ps, look, r, jsonEscape)
		return out, out != raw
	}
	return "", false
}

// wholeString reports whether a JSON string literal holds exactly one
// placeholder and nothing else.
func wholeString(raw string, ps []Placeholder) bool {
	return len(ps) == 1 && ps[0].Start == 1 && ps[0].End == len(raw)-1
}

func isForm(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "application/x-www-form-urlencoded")
}

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

func anyPlaceholder(req models.HTTPReq) bool {
	if Contains(req.URL) || Contains(req.Body) {
		return true
	}
	for k, v := range req.Header {
		if Contains(v) || Contains(k) {
			return true
		}
	}
	for _, v := range req.URLParams {
		if Contains(v) {
			return true
		}
	}
	for _, f := range req.Form {
		if Contains(f.Key) {
			return true
		}
		for _, v := range f.Values {
			if Contains(v) {
				return true
			}
		}
	}
	return false
}

func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyForm(f []models.FormData) []models.FormData {
	if f == nil {
		return nil
	}
	out := make([]models.FormData, len(f))
	for i, fd := range f {
		out[i] = models.FormData{
			Key:       fd.Key,
			Values:    append([]string(nil), fd.Values...),
			Paths:     append([]string(nil), fd.Paths...),
			FileNames: append([]string(nil), fd.FileNames...),
		}
	}
	return out
}

func copyGRPCHeaders(h models.GrpcHeaders) models.GrpcHeaders {
	return models.GrpcHeaders{
		PseudoHeaders:   copyStringMap(h.PseudoHeaders),
		OrdinaryHeaders: copyStringMap(h.OrdinaryHeaders),
	}
}

// sortedKeys is a header map's names in a fixed order (HeaderNames).
func sortedKeys(m map[string]string) []string { return HeaderNames(m) }

// HeaderNames is a header map's names in the order templates reads them: by
// name whatever its letter case (an HTTP header name is case-insensitive, so
// "location" and "Location" sort alike before "Set-Cookie"), and names that
// differ only in case by their bytes.
func HeaderNames(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}
