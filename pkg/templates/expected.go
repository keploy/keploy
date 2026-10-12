package templates

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
)

// Capture is one value an expected response took from the app's answer.
type Capture struct {
	Key   string
	Value string
	// Where names the place: "body.items[0].id", "header Set-Cookie.sid".
	Where string
}

// Failure is a variable place that the app's answer does not satisfy: a
// capture with no value of its type there, or a use whose place does not
// hold the key's value. Either fails its test case, whatever the rest of the
// comparison says, noise included.
type Failure struct {
	Where       string
	Placeholder string
	Why         string
}

func (f Failure) String() string { return f.Where + ": " + f.Placeholder + " " + f.Why }

// ExpectOptions are the settings an expected response is resolved with.
type ExpectOptions struct {
	// IgnoreOrdering is test.ignoreOrdering: the comparison takes an array's
	// elements in any order, so a use inside an array is satisfied by any
	// element of the answer's array that holds the key's value at that place.
	IgnoreOrdering bool
	// Now is the clock a cookie's Expires date is read against when the
	// answer has no Date header; time.Now when nil.
	Now func() time.Time
}

func (o ExpectOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Resolved is an expected response made ready to compare with the answer it
// expects.
type Resolved struct {
	// Resp is the expected response to compare: each capture holds the app's
	// answer there, and each other placeholder this run's value.
	Resp models.HTTPResp
	// RecordedBody is the body as it was recorded: every placeholder,
	// captures included, at its config.yaml value. A header that is a digest
	// of the body (ETag) was computed over this body, not over Resp.Body.
	RecordedBody string
	// BodyTemplated reports that the expected body held a placeholder.
	BodyTemplated bool
	// Captures are the values this response gave the run, in order.
	Captures []Capture
	// Failures are the variable places the answer does not satisfy. Each one
	// fails the test case on its own: a variable place is asserted even
	// where noise, or a body the comparison does not read, leaves it
	// unchecked.
	Failures []Failure
}

// edit replaces s[start:end] with text, which is not rendered again.
type edit struct {
	start, end int
	text       string
}

// Expected resolves exp against the app's answer, the way rule 4 says: first
// each capture takes what the answer holds at its place (and that becomes the
// key's value for the rest of the run), then every other placeholder takes
// the key's value now. Then each variable place is checked against the
// answer on its own (Failures). exp is not changed.
//
// A placeholder in a JSON object key is a use like any other: the answer's
// places are found with each key at the value it has before this response's
// captures, and the body is rendered with each key at its value after them.
func (r *Run) Expected(exp, answer models.HTTPResp, opts ExpectOptions) Resolved {
	exp.Header = r.renamed(exp.Header)
	out := exp
	out.Header = copyStringMap(exp.Header)
	res := Resolved{Resp: out, RecordedBody: exp.Body}
	if r.Empty() {
		// No key is defined: every placeholder is text, and the expected
		// response is compared as written.
		return res
	}
	bodyHas := Contains(exp.Body)
	if !bodyHas && !anyHeaderPlaceholder(exp.Header) {
		return res
	}

	// 1. Captures: the body in document order, then headers by name. Each
	// document is read once: the expected body as written, then, only when
	// an object key holds a placeholder, the body with its keys rendered.
	body := exp.Body
	var expDoc, doc *jsonDoc
	var bodyJSON bool
	overrides := map[int]string{}
	if bodyHas {
		res.BodyTemplated = true
		if expDoc, bodyJSON = parseJSON(exp.Body); bodyJSON {
			body, doc = r.withKeys(exp.Body, expDoc, r.use)
		}
	}
	var ansDoc *jsonDoc
	answerJSON := false
	if bodyJSON {
		ansDoc, answerJSON = parseAnswer(answer.Body)
		var answerIdx map[string]Leaf
		if answerJSON {
			answerIdx = leafIndex(ansDoc.leaves)
		}
		for i, l := range doc.leaves {
			p, ok := soleCapture(body, l)
			// A capture on a Shadowed leaf is refused when the set loads.
			if !ok || !r.usable(p) || isSecret(p) || l.Shadowed {
				continue
			}
			where := l.Under("body")
			a, found := answerIdx[l.PathKey()]
			value, why := captureLeaf(body, l, p, answer.Body, a, found && answerJSON)
			if why != "" {
				res.Failures = append(res.Failures, Failure{Where: where, Placeholder: p.Text, Why: why})
				continue
			}
			r.set(p.Key, value)
			overrides[i] = a.Raw(answer.Body)
			res.Captures = append(res.Captures, Capture{Key: p.Key, Value: value, Where: where})
		}
	}
	headerEdits := map[string][]edit{}
	for _, name := range sortedKeys(out.Header) {
		v := out.Header[name]
		if !Contains(v) {
			continue
		}
		var es []edit
		var caps []Capture
		var fails []Failure
		if strings.EqualFold(name, "Set-Cookie") {
			es, caps, fails = r.setCookieCaptures(name, v, exp, answer, opts.now())
		} else {
			es, caps, fails = r.headerCapture(name, v, answer.Header)
		}
		headerEdits[name] = es
		res.Captures = append(res.Captures, caps...)
		res.Failures = append(res.Failures, fails...)
	}

	// 2. Every other placeholder takes the value each key has now.
	if bodyHas {
		if bodyJSON {
			if expDoc.keysTemplated(exp.Body) {
				body, doc = r.withKeys(exp.Body, expDoc, r.use)
			}
			out.Body = renderJSON(body, doc.leaves, r.use, r, overrides)
			res.RecordedBody = r.recordedBody(exp.Body, expDoc)
		} else {
			out.Body = renderRaw(exp.Body, r.use, r)
			res.RecordedBody = renderRaw(exp.Body, r.asRecorded, r)
		}
	}
	for name, v := range out.Header {
		if !Contains(v) {
			continue
		}
		// A URL-valued header whose variable places all hold what they must
		// reads as the answer's: each value in it is read decoded for its
		// place, so the answer may write it escaped another way
		// (th%C3%A9-42). A header that holds only secrets is literal text,
		// rendered and compared as written.
		if live, found := headerLookup(answer.Header, name); found && isURLHeader(name) && r.hasVariable(v) && r.holds(v, live, true) {
			out.Header[name] = live
			continue
		}
		out.Header[name] = renderAround(v, headerEdits[name], r)
	}
	res.Resp = out

	// 3. Each use is asserted on its own, against the answer.
	if bodyHas {
		if bodyJSON {
			res.Failures = append(res.Failures, r.keyUseFailures("body", exp.Body, expDoc, doc, ansDoc, opts)...)
			var ansLeaves []Leaf
			if answerJSON {
				ansLeaves = ansDoc.leaves
			}
			res.Failures = append(res.Failures, r.bodyUseFailures("body", body, doc.leaves, answer.Body, ansLeaves, answerJSON, opts)...)
		} else {
			res.Failures = append(res.Failures, r.textUseFailures("body", exp.Body, answer.Body, "body", false)...)
		}
	}
	for _, name := range sortedKeys(exp.Header) {
		if Contains(exp.Header[name]) {
			res.Failures = append(res.Failures, r.headerUseFailures(name, exp, answer, opts.now())...)
		}
	}
	return res
}

// withKeys is a JSON document read as d with each object key's placeholders
// at look's values, and the result read: the document itself when no key
// holds one.
func (r *Run) withKeys(doc string, d *jsonDoc, look lookupFunc) (string, *jsonDoc) {
	if !d.keysTemplated(doc) {
		return doc, d
	}
	out := renderKeys(doc, d.keys, look, r)
	if out == doc {
		return doc, d
	}
	nd, ok := parseJSON(out)
	if !ok {
		return doc, d
	}
	return out, nd
}

// recordedBody is body with every placeholder, captures included, at its
// config.yaml value: the body as it was recorded, which a digest header on
// disk (an ETag) is computed over. d is body read as JSON, or nil.
func (r *Run) recordedBody(body string, d *jsonDoc) string {
	if d == nil {
		var ok bool
		if d, ok = parseJSON(body); !ok {
			return renderRaw(body, r.asRecorded, r)
		}
	}
	doc, nd := r.withKeys(body, d, r.asRecorded)
	return renderJSON(doc, nd.leaves, r.asRecorded, r, nil)
}

// usable reports whether a placeholder's key has a value this run can use.
func (r *Run) usable(p Placeholder) bool {
	if _, bad := r.Unusable(p); bad {
		return false
	}
	return r.Defined(p)
}

// headerCapture takes a header's one capture from the answer's header of the
// same name, anchored on the rest of the expected value as rendered: its
// text as written, and each use beside it (of a key this response does not
// capture, as Check requires) at the key's value. In a URL-valued header
// (Location, Content-Location) the URL is read by its parts: the capture
// takes the one path segment or query value it stands in (never a '/' of the
// path, never a '&', '=' or '#' of the query), decoded for that part as
// keploy templatize read it, and each use beside it is read decoded too. A
// value that decodes to a delimiter of its part (a%2Fb in a path segment)
// cannot be told from two parts, and fails.
func (r *Run) headerCapture(name, v string, answer map[string]string) ([]edit, []Capture, []Failure) {
	var caps []Placeholder
	for _, p := range Find(v) {
		if p.Capture && r.usable(p) {
			caps = append(caps, p)
		}
	}
	if len(caps) != 1 {
		return nil, nil, nil
	}
	p := caps[0]
	where := "header " + name
	live, found := headerLookup(answer, name)
	if !found {
		return nil, nil, []Failure{{Where: where, Placeholder: p.Text, Why: "needs a value and the answer has no such header"}}
	}
	url := isURLHeader(name)
	spans, ok := r.read(v, live, url)
	if !ok {
		prefix, suffix := renderRaw(v[:p.Start], r.use, r), renderRaw(v[p.End:], r.use, r)
		return nil, nil, []Failure{{Where: where, Placeholder: p.Text, Why: fmt.Sprintf("needs the answer to read %q…%q around it; it reads %q", prefix, suffix, live)}}
	}
	span := spans[p.Start]
	value := span
	if url {
		part := urlPartAt(v, p.Start)
		value = decodeSpan(span, part)
		if d, held := heldDelimiter(value, part); held {
			return nil, nil, []Failure{{Where: where, Placeholder: p.Text, Why: fmt.Sprintf("needs a value one %s can hold; the answer's %q decodes to %q, which holds %q", partName(part), span, value, d)}}
		}
	}
	if !fitsType(p.Type, value) {
		return nil, nil, []Failure{{Where: where, Placeholder: p.Text, Why: fmt.Sprintf("needs a non-empty %s; the answer holds %q there", p.Type, span)}}
	}
	r.set(p.Key, value)
	return []edit{{start: p.Start, end: p.End, text: span}}, []Capture{{Key: p.Key, Value: value, Where: where}}, nil
}

// setCookieCaptures resolves a Set-Cookie value cookie by cookie. A cookie is
// found in the answer by its identity (name, Domain, Path), the last of an
// identity being the one a user agent keeps. A capture that is a cookie's
// whole value takes the answer's cookie value (up to ';'), when the answer
// sets that cookie and does not delete it; a capture in a cookie that a later
// cookie of the value replaces, at this run's values, fails (Check refuses it
// at the recorded values). Every cookie of the value, a literal sibling of a
// templated cookie included, has its Expires date compared as a lifetime
// (expiresHolds): the answer's date stands when it gives the cookie the
// recorded lifetime. Max-Age and every other attribute are compared as
// recorded.
func (r *Run) setCookieCaptures(name, v string, exp, answer models.HTTPResp, now time.Time) ([]edit, []Capture, []Failure) {
	live, _ := headerLookup(answer.Header, name)
	liveByID := cookiesOfID(live)
	recordedAt, answerAt := recordedTime(exp), responseTime(answer.Header)
	cookies := SplitSetCookie(v)
	shadowed := r.shadowed(cookies, v)
	ordinal := map[CookieID]int{}
	var es []edit
	var caps []Capture
	var fails []Failure
	for i, c := range cookies {
		id := c.ID(v, r.renderUses)
		shown := shownID(c, id)
		where := cookieWhere(name, c, cookies, shown)
		// The answer's cookie this one is read against: the last of its
		// identity for the cookie a user agent keeps, and for one a later
		// cookie replaces, the one at its position among that identity.
		k := ordinal[id]
		ordinal[id]++
		lcs := liveByID[id]
		var lc SetCookie
		has := false
		switch {
		case !shadowed[i] && len(lcs) > 0:
			lc, has = lcs[len(lcs)-1], true
		case shadowed[i] && k < len(lcs):
			lc, has = lcs[k], true
		}
		value := v[c.ValueStart:c.ValueEnd]
		ps := Find(value)
		switch {
		case shadowed[i]:
			for _, p := range r.placed(Find(v[c.Start:c.End])) {
				if p.Capture {
					fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: whyShadowedAtRun})
				}
			}
		case len(ps) == 1 && ps[0].Capture && ps[0].Start == 0 && ps[0].End == len(value) && r.usable(ps[0]):
			p := ps[0]
			lv := lc.Value(live)
			var deleted string
			if has {
				deleted = cookieDeleted(c, v, recordedAt, lc, live, answerAt, now)
			}
			switch {
			case !has:
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: "needs a value and the answer sets no cookie " + shown.String()})
			case deleted != "":
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: "needs the cookie set; the answer deletes it (" + deleted + ")"})
			case !fitsType(p.Type, lv):
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: fmt.Sprintf("needs a non-empty %s; the answer's cookie holds %q", p.Type, live[lc.ValueStart:lc.ValueEnd])})
			default:
				r.set(p.Key, lv)
				es = append(es, edit{start: c.ValueStart, end: c.ValueEnd, text: live[lc.ValueStart:lc.ValueEnd]})
				caps = append(caps, Capture{Key: p.Key, Value: lv, Where: where})
			}
		}
		if !has {
			continue
		}
		// Expires: the answer's date stands when it gives the cookie the
		// recorded lifetime; otherwise the recorded date is compared.
		if a, ok := c.Attr("Expires"); ok && a.HasValue && !Contains(v[a.ValueStart:a.ValueEnd]) {
			if la, ok := lc.Attr("Expires"); ok && la.HasValue && expiresHolds(v[a.ValueStart:a.ValueEnd], recordedAt, live[la.ValueStart:la.ValueEnd], answerAt, now) {
				es = append(es, edit{start: a.ValueStart, end: a.ValueEnd, text: live[la.ValueStart:la.ValueEnd]})
			}
		}
	}
	sort.Slice(es, func(i, j int) bool { return es[i].start < es[j].start })
	return es, caps, fails
}

// whyShadowedAtRun is why a placeholder in a cookie that a later cookie of the
// same Set-Cookie value replaces fails at run time.
const whyShadowedAtRun = "sits in a cookie that a later cookie of this Set-Cookie value, of the same name, Domain and Path at this run's values, replaces: what it sets never reaches a user agent; move the placeholder to the last cookie of that identity"

// shadowed reports, for each cookie of a Set-Cookie value, whether a later
// cookie of the value replaces it at this run's values: the one identity rule
// (Shadowed, each attribute with its uses rendered) that Check applies at the
// recorded values and a run applies at its own.
func (r *Run) shadowed(cookies []SetCookie, v string) []bool {
	return Shadowed(cookies, v, r.renderUses)
}

// shadowedAsRecorded is shadowed at the values every pass starts from.
func (r *Run) shadowedAsRecorded(cookies []SetCookie, v string) []bool {
	return Shadowed(cookies, v, func(s string) string { return renderRaw(s, r.recordedUse, r) })
}

// renderUses is s with each use at its value now: an attribute value read
// for a cookie's identity.
func (r *Run) renderUses(s string) string { return renderRaw(s, r.use, r) }

// recordedTime is when an expected response was recorded: its Date header,
// else its timestamp.
func recordedTime(exp models.HTTPResp) time.Time {
	if t := responseTime(exp.Header); !t.IsZero() {
		return t
	}
	return exp.Timestamp
}

// cookieDeleted says how the answer deletes a cookie the recording set and
// kept, or "": a cookie that holds a variable must be set, and a cookie a
// user agent deletes on arrival is not.
func cookieDeleted(c SetCookie, v string, recordedAt time.Time, lc SetCookie, live string, answerAt, now time.Time) string {
	if c.deletedAt(v, recordedAt) != "" {
		return "" // the recording deleted it too
	}
	at := answerAt
	if at.IsZero() {
		at = now
	}
	return lc.deletedAt(live, at)
}

// cookieWhere names a cookie's place: header Set-Cookie.sid, with its Path and
// Domain when the value sets another cookie of that name.
// shownID is id as a failure names it: with the cookie's name as written, so
// a secret keploy sanitize wrote there (read at its value for the identity)
// is never printed.
func shownID(c SetCookie, id CookieID) CookieID {
	id.Name = c.Name
	return id
}

func cookieWhere(name string, c SetCookie, all []SetCookie, id CookieID) string {
	for _, o := range all {
		if o.Start != c.Start && o.Name == c.Name {
			return "header " + name + "." + id.String()
		}
	}
	return "header " + name + "." + c.Name
}

// renderAround is v with each edit's text in its span and every placeholder
// outside the edits at its value now.
func renderAround(v string, es []edit, r *Run) string {
	var b strings.Builder
	last := 0
	for _, e := range es {
		b.WriteString(renderRaw(v[last:e.start], r.use, r))
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(renderRaw(v[last:], r.use, r))
	return b.String()
}

// bodyUseFailures checks each use placeholder of a JSON expected body against
// the answer's body: a JSON value that is one use must equal the key's value
// there; a string that holds a use among other text must read as written,
// with each use at its key's value (textUseFailures). prefix names the
// document in each place ("body", "body frame 0").
func (r *Run) bodyUseFailures(prefix, body string, leaves []Leaf, answer string, answerLeaves []Leaf, answerJSON bool, opts ExpectOptions) []Failure {
	var fails []Failure
	for _, l := range leaves {
		raw := l.Raw(body)
		ps := Find(raw)
		uses := r.uses(ps)
		if len(uses) == 0 {
			continue
		}
		where := l.Under(prefix)
		var candidates []Leaf
		if answerJSON {
			candidates = leavesAt(answerLeaves, l, opts.IgnoreOrdering && l.InArray)
		}
		whole := (l.Kind == LeafBare && len(ps) == 1) || (l.Kind == LeafString && wholeString(raw, ps))
		if whole {
			p := uses[0]
			want, ok := renderLeaf(body, l, r.use, r)
			if !ok {
				v, _ := r.current(p)
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: r.redact(p, fmt.Sprintf("cannot be asserted: its value %q is not a number of type %s", v, p.Type))})
				continue
			}
			if why := matchesAny(want, answer, candidates); why != "" {
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: r.redact(p, fmt.Sprintf("must equal %s's value %s; %s", p.Key, want, why))})
			}
			continue
		}
		// A use among other text in a string: some element there must read
		// as the string does with each use at its value.
		text := l.Value(body)
		var strs []Leaf
		for _, c := range candidates {
			if c.Kind == LeafString {
				strs = append(strs, c)
			}
		}
		if len(strs) == 0 {
			for _, p := range uses {
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: r.redact(p, "needs a string there; the answer has none")})
			}
			continue
		}
		var first []Failure
		held := false
		for i, c := range strs {
			fs := r.textUseFailures(where, text, c.Value(answer), "string", false)
			if len(fs) == 0 {
				held = true
				break
			}
			if i == 0 {
				first = fs
			}
		}
		if !held {
			fails = append(fails, first...)
		}
	}
	return fails
}

// keyUseFailures checks each use placeholder in a JSON body's object keys: the
// answer must have that key, at the value the key now has, at its path (its
// own array indexes, or any under ignoreOrdering). exp is the expected body
// as written, read as expDoc; rendered is it with its keys rendered; answer
// is the answer's body read, or nil when it is not JSON.
func (r *Run) keyUseFailures(prefix, exp string, expDoc, rendered, answer *jsonDoc, opts ExpectOptions) []Failure {
	if !expDoc.keysTemplated(exp) {
		return nil
	}
	var fails []Failure
	for i, k := range expDoc.keys {
		uses := r.uses(Find(k.Raw(exp)))
		if len(uses) == 0 {
			continue
		}
		if i >= len(rendered.keys) {
			break
		}
		rk := rendered.keys[i]
		found := false
		if answer != nil {
			for _, a := range answer.keys {
				if !a.Shadowed && samePath(a.Path, rk.Path, opts.IgnoreOrdering && rk.InArray) {
					found = true
					break
				}
			}
		}
		if !found {
			name := rk.Path[len(rk.Path)-1].Key
			for _, p := range uses {
				fails = append(fails, Failure{Where: prefix + " key " + rk.DisplayPath(), Placeholder: p.Text, Why: r.redact(p, fmt.Sprintf("must name the key %q there; the answer has no such key", name))})
			}
		}
	}
	return fails
}

// headerUseFailures checks each use placeholder of an expected header against
// the answer's header: a value that is one use must equal the key's value; a
// Set-Cookie cookie that holds a use must be set by the answer, its value
// equal to the key's value when the use is the whole value; any other use
// must sit in the answer's text as written around it (textUseFailures).
func (r *Run) headerUseFailures(name string, exp, answer models.HTTPResp, now time.Time) []Failure {
	v := exp.Header[name]
	live, found := headerLookup(answer.Header, name)
	if strings.EqualFold(name, "Set-Cookie") {
		return r.setCookieUseFailures(name, v, exp, live, found, answer, now)
	}
	ps := Find(v)
	uses := r.uses(ps)
	if len(uses) == 0 {
		return nil
	}
	where := "header " + name
	if !found {
		return []Failure{{Where: where, Placeholder: uses[0].Text, Why: "needs the answer to have this header; it does not"}}
	}
	if len(ps) == 1 && ps[0].Start == 0 && ps[0].End == len(v) {
		p := uses[0]
		if want, _ := r.current(p); live != want {
			return []Failure{{Where: where, Placeholder: p.Text, Why: r.redact(p, fmt.Sprintf("must equal %s's value %q; the answer holds %q", p.Key, want, live))}}
		}
		return nil
	}
	return r.textUseFailures(where, v, live, "header", isURLHeader(name))
}

// setCookieUseFailures checks the uses of an expected Set-Cookie value cookie
// by cookie: the answer must set the cookie of that identity and not delete
// it; a use that is the cookie's whole value must equal the key's value, and
// any other use must sit in its value or attribute as written around it.
func (r *Run) setCookieUseFailures(name, v string, exp models.HTTPResp, live string, found bool, answer models.HTTPResp, now time.Time) []Failure {
	var fails []Failure
	liveCookies := cookiesByID(live)
	recordedAt, answerAt := recordedTime(exp), responseTime(answer.Header)
	cookies := SplitSetCookie(v)
	shadowed := r.shadowed(cookies, v)
	for i, c := range cookies {
		cookie := v[c.Start:c.End]
		uses := r.uses(Find(cookie))
		if len(uses) == 0 {
			continue
		}
		id := c.ID(v, r.renderUses)
		shown := shownID(c, id)
		where := cookieWhere(name, c, cookies, shown)
		if shadowed[i] {
			for _, p := range uses {
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: whyShadowedAtRun})
			}
			continue
		}
		lc, has := liveCookies[id]
		if !has || !found {
			for _, p := range uses {
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: "needs the answer to set cookie " + shown.String() + "; it does not"})
			}
			continue
		}
		if how := cookieDeleted(c, v, recordedAt, lc, live, answerAt, now); how != "" {
			for _, p := range uses {
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: "needs the answer to set cookie " + shown.String() + "; it deletes it (" + how + ")"})
			}
			continue
		}
		// Each use is read in the part of the cookie it sits in: its value,
		// or the value of one of its attributes.
		value := v[c.ValueStart:c.ValueEnd]
		if ps := Find(value); len(ps) == 1 && ps[0].Start == 0 && ps[0].End == len(value) && r.isUse(ps[0]) {
			p := ps[0]
			if want, _ := r.current(p); lc.Value(live) != want {
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: r.redact(p, fmt.Sprintf("must equal %s's value %q; the answer's cookie holds %q", p.Key, want, live[lc.ValueStart:lc.ValueEnd]))})
			}
		} else if len(r.uses(Find(value))) > 0 {
			fails = append(fails, r.textUseFailures(where, value, live[lc.ValueStart:lc.ValueEnd], "cookie value", false)...)
		}
		for _, a := range c.Attrs {
			if !a.HasValue || len(r.uses(Find(v[a.ValueStart:a.ValueEnd]))) == 0 {
				continue
			}
			// The attribute's name as the answer writes it: a secret
			// there is the name the recording held.
			lv, ok := lc.attrValue(live, r.renderUses(a.Name))
			if !ok {
				for _, p := range r.uses(Find(v[a.ValueStart:a.ValueEnd])) {
					fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: "needs the answer's cookie to have " + a.Name + "; it does not"})
				}
				continue
			}
			fails = append(fails, r.textUseFailures(where, v[a.ValueStart:a.ValueEnd], lv, a.Name, false)...)
		}
	}
	return fails
}

// textUseFailures checks the uses in text, a value that holds them among
// other text, against live, the answer's value at the same place. live must
// read as text does, anchored at both ends: the text around the placeholders
// as written, each use's span its key's value, and each capture (a header's
// one, beside uses of keys the response does not capture) any text. In a
// URL-valued header (url) a span is read decoded for its place, a path
// segment or a query value, so the answer may escape a value either way;
// the value is never escaped to be compared. When live does not read so, its
// text is split along text's shape (each placeholder a span) to say which
// use fails and what the answer holds there.
func (r *Run) textUseFailures(where, text, live, in string, url bool) []Failure {
	ps := r.placed(Find(text))
	uses := r.uses(ps)
	if len(uses) == 0 {
		return nil
	}
	var fails []Failure
	for _, p := range uses {
		v, ok := valueFor(p, r.use, r)
		if !ok {
			v, _ = r.current(p)
			fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: r.redact(p, fmt.Sprintf("cannot be asserted: its value %q is not a number of type %s", v, p.Type))})
			continue
		}
		// In a URL-valued header a value is read by the part it stands in;
		// one that holds a delimiter of that part cannot be told from two
		// parts there.
		if url {
			part := urlPartAt(text, p.Start)
			if d, held := heldDelimiter(v, part); held {
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: r.redact(p, fmt.Sprintf("cannot be asserted in a URL: %s's value %q holds %q, which delimits a %s", p.Key, v, d, partName(part)))})
			}
		}
	}
	if len(fails) > 0 {
		return fails
	}
	if _, ok := r.read(text, live, url); ok {
		return nil
	}
	spans, read := spansOf(text, ps, live)
	for i, p := range ps {
		// A capture takes any text, and a secret is literal text: only a
		// use's span is said to fail.
		if !r.isUse(p) {
			continue
		}
		v, _ := r.current(p)
		var why string
		switch {
		case !read:
			why = fmt.Sprintf("must be %s's value %q in the answer's %s as written around it (%q); the answer's reads %q", p.Key, v, in, text, live)
		case spanValue(spans[i], text, p, url) != v:
			why = fmt.Sprintf("must be %s's value %q; the answer's %s holds %q there", p.Key, v, in, spans[i])
		default:
			continue
		}
		fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: r.redact(p, why)})
	}
	if len(fails) == 0 {
		// Every span the split found holds its value, yet the answer does
		// not read as text: say so for each use rather than pass it.
		for _, p := range uses {
			v, _ := r.current(p)
			fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: r.redact(p, fmt.Sprintf("must be %s's value %q in the answer's %s as written around it (%q); the answer's reads %q", p.Key, v, in, text, live))})
		}
	}
	return fails
}

// spanValue is the value a span of the answer holds for p: in a URL-valued
// header, the span decoded for p's place in text.
func spanValue(span, text string, p Placeholder, url bool) string {
	if !url {
		return span
	}
	return decodeSpan(span, urlPartAt(text, p.Start))
}

// spansOf reads live against the shape of text: the text between the
// placeholders as written, each placeholder any span. It returns each
// placeholder's span, or false when live does not have that shape.
func spansOf(text string, ps []Placeholder, live string) ([]string, bool) {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	last := 0
	for _, p := range ps {
		b.WriteString(regexp.QuoteMeta(text[last:p.Start]))
		b.WriteString(`(.*?)`)
		last = p.End
	}
	b.WriteString(regexp.QuoteMeta(text[last:]))
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, false
	}
	m := re.FindStringSubmatch(live)
	if m == nil {
		return nil, false
	}
	return m[1:], true
}

// read reads live as text, anchored at both ends: the text around text's
// placed placeholders as written, each use's span holding its key's value
// (decoded for its URL place when url), each secret's its value as written
// (the literal the recording held), each capture any span. It returns
// each placed placeholder's span by the placeholder's start in text, or
// false when live does not read so (or a use's value is not a number of its
// type, so no text holds it).
func (r *Run) read(text, live string, url bool) (map[int]string, bool) {
	ps := r.placed(Find(text))
	var b strings.Builder
	b.WriteString(`(?s)^`)
	last := 0
	for _, p := range ps {
		b.WriteString(regexp.QuoteMeta(text[last:p.Start]))
		last = p.End
		part := partText
		if url {
			part = urlPartAt(text, p.Start)
		}
		if p.Capture {
			b.WriteString(`(` + captureClass(part) + `)`)
			continue
		}
		v, ok := valueFor(p, r.use, r)
		if !ok {
			return nil, false
		}
		if isSecret(p) {
			// A secret is the literal the recording held there, in the
			// syntax of its place already: read as written.
			b.WriteString(`(` + regexp.QuoteMeta(v) + `)`)
			continue
		}
		b.WriteString(`(` + spanPattern(v, part) + `)`)
	}
	b.WriteString(regexp.QuoteMeta(text[last:]))
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, false
	}
	m := re.FindStringSubmatch(live)
	if m == nil {
		return nil, false
	}
	out := make(map[int]string, len(ps))
	for i, p := range ps {
		out[p.Start] = m[i+1]
	}
	return out, true
}

// holds reports whether live reads as text does (read).
func (r *Run) holds(text, live string, url bool) bool {
	_, ok := r.read(text, live, url)
	return ok
}

// uses are the placeholders that assert a defined key's value now (isUse).
func (r *Run) uses(ps []Placeholder) []Placeholder {
	var out []Placeholder
	for _, p := range ps {
		if r.isUse(p) {
			out = append(out, p)
		}
	}
	return out
}

// isUse reports whether p is a use: a placeholder of a config.yaml key with a
// usable value, not a capture. A secret ({{string .secret.K}}, written by
// keploy sanitize) is not a variable: it is a redaction of the literal the
// recording held, so in an expected response it stands for that literal and
// is compared, noise and all, as the literal would be. Only a variable's
// place is asserted on its own.
func (r *Run) isUse(p Placeholder) bool {
	return !p.Capture && !isSecret(p) && r.usable(p)
}

// hasVariable reports whether text holds a variable: a capture or a use of a
// config.yaml key (never a secret).
func (r *Run) hasVariable(text string) bool {
	for _, p := range Find(text) {
		if !isSecret(p) && r.usable(p) {
			return true
		}
	}
	return false
}

// placed are the placeholders a run reads at their place, captures and uses
// of keys with a usable value; any other placeholder is text.
func (r *Run) placed(ps []Placeholder) []Placeholder {
	var out []Placeholder
	for _, p := range ps {
		if r.usable(p) {
			out = append(out, p)
		}
	}
	return out
}

// The parts of a URL-valued header a placeholder can sit in.
const (
	partText = iota
	partPath
	partQuery
)

// isURLHeader reports whether a response header's value is a URL whose path
// segments and query values keploy templatize reads decoded: Location and
// Content-Location.
func isURLHeader(name string) bool {
	return strings.EqualFold(name, "Location") || strings.EqualFold(name, "Content-Location")
}

// urlPartAt is the part of the URL u that the byte at is in, as templatize's
// urlLocations splits it: the path (after the scheme and host), the query
// (after '?'), or text (the scheme and host, the fragment).
func urlPartAt(u string, at int) int {
	pathStart := 0
	if k := strings.Index(u, "://"); k >= 0 {
		slash := strings.IndexByte(u[k+3:], '/')
		if slash < 0 {
			return partText
		}
		pathStart = k + 3 + slash
	}
	end := len(u)
	if k := strings.IndexByte(u, '#'); k >= 0 {
		end = k
	}
	queryAt := end
	if k := strings.IndexByte(u[:end], '?'); k >= 0 {
		queryAt = k
	}
	switch {
	case at < pathStart || at >= end:
		return partText
	case at < queryAt:
		return partPath
	}
	return partQuery
}

// decodeSpan is the value a span of a URL holds at its part: a path segment
// PathUnescaped, a query value QueryUnescaped, any other text as written. A
// span that does not decode is read as written, as templatize reads it.
func decodeSpan(span string, part int) string {
	switch part {
	case partPath:
		if v, err := url.PathUnescape(span); err == nil {
			return v
		}
	case partQuery:
		if v, err := url.QueryUnescape(span); err == nil {
			return v
		}
	}
	return span
}

// partName names a part of a URL-valued header, for a failure's reason.
func partName(part int) string {
	switch part {
	case partPath:
		return "path segment"
	case partQuery:
		return "query value"
	}
	return "URL"
}

// heldDelimiter is the first character of value that delimits part: a '/'
// in a path segment; a '&', '=' or '#' in a query value. A value holding one
// cannot be read at its place, whichever way the answer escapes it.
func heldDelimiter(value string, part int) (string, bool) {
	var set string
	switch part {
	case partPath:
		set = "/"
	case partQuery:
		set = "&=#"
	default:
		return "", false
	}
	if i := strings.IndexAny(value, set); i >= 0 {
		return value[i : i+1], true
	}
	return "", false
}

// HoldsURLDelimiter reports whether value holds a delimiter of the part of a
// URL-valued header it would stand in (a path segment, or with query a query
// value): such a value cannot be read there (heldDelimiter).
func HoldsURLDelimiter(value string, query bool) bool {
	part := partPath
	if query {
		part = partQuery
	}
	_, held := heldDelimiter(value, part)
	return held
}

// captureClass is the span a capture may take at part: one path segment (no
// '/', '?' or '#'), one query value (no '&', '=' or '#'), or any text.
func captureClass(part int) string {
	switch part {
	case partPath:
		return `[^/?#]*`
	case partQuery:
		return `[^&=#]*`
	}
	return `.*`
}

// encodedOnly reports whether ch may stand in a span at part only
// percent-encoded: written as it is, it would be the URL's own syntax (a '/',
// '?' or '#' in a path; a '&', '=', '#' or '+' in a query) or an escape ('%').
func encodedOnly(ch string, part int) bool {
	switch {
	case ch == "%":
		return true
	case part == partPath:
		return ch == "/" || ch == "?" || ch == "#"
	case part == partQuery:
		return ch == "&" || ch == "=" || ch == "#" || ch == "+"
	}
	return false
}

// spanPattern is a regular expression for every span whose value at part is
// value (decodeSpan): in text, value as written; in a URL path or query, each
// character as written or percent-encoded (either letter case), with the
// part's own syntax and a '%' only encoded (encodedOnly), and in a query a
// space also as '+'.
func spanPattern(value string, part int) string {
	if part == partText {
		return regexp.QuoteMeta(value)
	}
	const hex = "0123456789ABCDEF"
	digit := func(n byte) string {
		d := hex[n]
		if d >= 'A' {
			return "[" + string(d) + string(d+'a'-'A') + "]"
		}
		return string(d)
	}
	var b strings.Builder
	for i := 0; i < len(value); {
		c, size := utf8.DecodeRuneInString(value[i:])
		ch := value[i : i+size]
		b.WriteString("(?:")
		for j := 0; j < size; j++ {
			b.WriteString("%" + digit(ch[j]>>4) + digit(ch[j]&15))
		}
		switch {
		case c == utf8.RuneError && size == 1, encodedOnly(ch, part):
		case part == partQuery && ch == " ":
			b.WriteString(`|\+| `)
		default:
			b.WriteString("|" + regexp.QuoteMeta(ch))
		}
		b.WriteString(")")
		i += size
	}
	return b.String()
}

// GRPCUseFailures checks each use placeholder of an expected gRPC response
// against the answer on its own, as Expected does for HTTP: a header that is
// one use must equal the key's value, and any other header or message text
// that holds a use must read as written with each use at its value
// (textUseFailures), message by message.
func (r *Run) GRPCUseFailures(exp, answer models.GrpcResp) []Failure {
	if r.Empty() {
		return nil
	}
	var fails []Failure
	for _, h := range []struct {
		where      string
		exp, given map[string]string
	}{
		{"header ", exp.Headers.PseudoHeaders, answer.Headers.PseudoHeaders},
		{"header ", exp.Headers.OrdinaryHeaders, answer.Headers.OrdinaryHeaders},
	} {
		for _, name := range sortedKeys(h.exp) {
			v := h.exp[name]
			ps := Find(v)
			uses := r.uses(ps)
			if len(uses) == 0 {
				continue
			}
			where := h.where + name
			live, found := headerLookup(h.given, name)
			switch {
			case !found:
				fails = append(fails, Failure{Where: where, Placeholder: uses[0].Text, Why: "needs the answer to have this header; it does not"})
			case len(ps) == 1 && ps[0].Start == 0 && ps[0].End == len(v):
				if want, _ := r.current(uses[0]); live != want {
					fails = append(fails, Failure{Where: where, Placeholder: uses[0].Text, Why: r.redact(uses[0], fmt.Sprintf("must equal %s's value %q; the answer holds %q", uses[0].Key, want, live))})
				}
			default:
				fails = append(fails, r.textUseFailures(where, v, live, "header", false)...)
			}
		}
	}
	expMsgs, ansMsgs := exp.AllMessages(), answer.AllMessages()
	for i, m := range expMsgs {
		uses := r.uses(Find(m.DecodedData))
		if len(uses) == 0 {
			continue
		}
		where := "body"
		if len(expMsgs) > 1 {
			where = "body message " + strconv.Itoa(i)
		}
		if i >= len(ansMsgs) {
			for _, p := range uses {
				fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: fmt.Sprintf("needs message %d of the answer; it has %d", i, len(ansMsgs))})
			}
			continue
		}
		fails = append(fails, r.textUseFailures(where, m.DecodedData, ansMsgs[i].DecodedData, "message", false)...)
	}
	return fails
}

// StreamUseFailures checks each use placeholder of a streamed expected
// response against the answer on its own, as Expected does for a response
// read whole, whatever the stream comparison and its noise say: its headers
// against the answer's headers, and each recorded frame (StreamFrames)
// against the frame received at its position, field by field. received is
// what the answer delivered, split as a recording splits a stream
// (models.StreamChunksOf, by the recorded Content-Type), so frame i of the
// recording and frame i of the answer are read alike. A field that is a JSON
// document is read by its keys' and values' paths, any other field anchored
// on its text.
//
// complete says received is all the answer delivered: a frame it lacks then
// fails each use the recorded frame holds. When the stream comparison stopped
// reading at a frame that did not match (which fails the test case on its
// own), the frames after it were never read, and are not asserted here.
func (r *Run) StreamUseFailures(exp models.HTTPResp, received string, complete bool, answer models.HTTPResp, opts ExpectOptions) []Failure {
	if r.Empty() {
		return nil
	}
	var fails []Failure
	for _, name := range sortedKeys(exp.Header) {
		if Contains(exp.Header[name]) {
			fails = append(fails, r.headerUseFailures(name, exp, answer, opts.now())...)
		}
	}
	frames := r.framesWithUses(exp)
	if frames == nil {
		return fails
	}
	got := models.StreamChunksOf(exp.Header, received)
	// The stream comparison compares a frame's JSON with its arrays in order
	// (ignoreOrdering is not read there), so a use in a frame's array is
	// asserted at its own index: tied to its element under noise too.
	opts.IgnoreOrdering = false
	for i, chunk := range frames {
		nth := map[string]int{}
		for _, f := range chunk.Data {
			key := strings.ToLower(strings.TrimSpace(f.Key))
			n := nth[key]
			nth[key]++
			uses := r.uses(Find(f.Value))
			if len(uses) == 0 {
				continue
			}
			where := frameWhere(i, f.Key)
			if i >= len(got) {
				if !complete {
					continue
				}
				for _, p := range uses {
					fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: fmt.Sprintf("needs frame %d of the answer; the answer delivered %d", i, len(got))})
				}
				continue
			}
			live, ok := fieldOf(got[i], key, n)
			if !ok {
				for _, p := range uses {
					fails = append(fails, Failure{Where: where, Placeholder: p.Text, Why: fmt.Sprintf("needs frame %d of the answer to have its %s field; it does not", i, f.Key)})
				}
				continue
			}
			fails = append(fails, r.documentUseFailures(where, f.Value, live, opts)...)
		}
	}
	return fails
}

// StreamHasUses reports whether a streamed expected response's frames hold a
// use placeholder of a defined key: only then does a run keep the frames it
// receives, to assert them.
func (r *Run) StreamHasUses(exp models.HTTPResp) bool {
	return r.framesWithUses(exp) != nil
}

// StreamFrames are a streamed expected response's frames as recorded: its
// StreamBody, or, when the recording kept the body flat, the body split the
// way a recording splits a stream (models.StreamChunksOf). nil when a flat
// body does not split (a multipart body, a stream keploy keeps whole): Check
// refuses a variable there.
func StreamFrames(exp models.HTTPResp) []models.HTTPStreamChunk {
	if len(exp.StreamBody) > 0 {
		return exp.StreamBody
	}
	if exp.Body == "" {
		return nil
	}
	return models.StreamChunksOf(exp.Header, exp.Body)
}

// framesWithUses is StreamFrames when a frame holds a use, else nil. A body
// that holds no placeholder is never split.
func (r *Run) framesWithUses(exp models.HTTPResp) []models.HTTPStreamChunk {
	if r.Empty() {
		return nil
	}
	if len(exp.StreamBody) == 0 && !Contains(exp.Body) {
		return nil
	}
	frames := StreamFrames(exp)
	for _, c := range frames {
		for _, f := range c.Data {
			if len(r.uses(Find(f.Value))) > 0 {
				return frames
			}
		}
	}
	return nil
}

// ExpectedStream is ExpectedNoAnswer for a streamed response, which the
// stream comparison reads frame by frame (StreamBody). When the recording
// kept the body flat and a frame of it holds a use, the comparison is given
// those frames, each rendered: it then reads every frame the run asserts, and
// compares each, rather than an empty list of frames that matches any stream.
func (r *Run) ExpectedStream(exp models.HTTPResp) models.HTTPResp {
	if len(exp.StreamBody) == 0 {
		if frames := r.framesWithUses(exp); frames != nil {
			exp.StreamBody = frames
		}
	}
	return r.ExpectedNoAnswer(exp)
}

// fieldOf is the value of a frame's nth field named key (in lower case).
func fieldOf(c models.HTTPStreamChunk, key string, nth int) (string, bool) {
	for _, f := range c.Data {
		if strings.ToLower(strings.TrimSpace(f.Key)) != key {
			continue
		}
		if nth == 0 {
			return f.Value, true
		}
		nth--
	}
	return "", false
}

// documentUseFailures checks the uses of an expected text that is a document
// of its own (a stream frame's field) against the answer's: a JSON document
// by its object keys' and values' paths, any other text anchored as written.
func (r *Run) documentUseFailures(where, exp, answer string, opts ExpectOptions) []Failure {
	d, ok := parseJSON(exp)
	if !ok {
		return r.textUseFailures(where, exp, answer, "frame", false)
	}
	doc, rd := r.withKeys(exp, d, r.use)
	var ansLeaves []Leaf
	ad, answerJSON := parseAnswer(answer)
	if answerJSON {
		ansLeaves = ad.leaves
	} else {
		ad = nil
	}
	fails := r.keyUseFailures(where, exp, d, rd, ad, opts)
	return append(fails, r.bodyUseFailures(where, doc, rd.leaves, answer, ansLeaves, answerJSON, opts)...)
}

// redact keeps a secret's value out of a failure: the report and the log
// say which secret, never what it is.
func (r *Run) redact(p Placeholder, why string) string {
	if name, ok := p.Secret(); ok {
		return "must hold the value of secret " + name + "; the answer does not"
	}
	return why
}

// leavesAt are the answer's leaves at l's place: its path, or with
// anyIndex, its path with any index in each array step. A Shadowed leaf is at
// no place: of a key an object repeats, only the last occurrence's value is
// at its path, as encoding/json, JSON.parse and json.loads read it.
func leavesAt(answer []Leaf, l Leaf, anyIndex bool) []Leaf {
	var out []Leaf
	for _, a := range answer {
		if !a.Shadowed && samePath(a.Path, l.Path, anyIndex) {
			out = append(out, a)
		}
	}
	return out
}

func samePath(a, b []PathSeg, anyIndex bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].IsIndex != b[i].IsIndex {
			return false
		}
		if a[i].IsIndex {
			if !anyIndex && a[i].Index != b[i].Index {
				return false
			}
			continue
		}
		if a[i].Key != b[i].Key {
			return false
		}
	}
	return true
}

// matchesAny says why none of the candidates holds the JSON value want, or
// "" when one does.
func matchesAny(want, answer string, candidates []Leaf) string {
	if len(candidates) == 0 {
		return "the answer has no value there"
	}
	for _, c := range candidates {
		if jsonValueEqual(want, c.Raw(answer)) {
			return ""
		}
	}
	return "the answer holds " + candidates[0].Raw(answer)
}

// ExpectedNoAnswer is exp with each use placeholder holding this run's value,
// for a test case that got no answer to capture from (it failed before one
// came) or that never captures (a streamed response). Captures stay as
// written. A streamed response's frames (StreamBody), which the stream
// comparison reads, are rendered field value by field value, each as a body
// of its own. exp is not changed.
func (r *Run) ExpectedNoAnswer(exp models.HTTPResp) models.HTTPResp {
	out := exp
	out.Header = copyStringMap(r.renamed(exp.Header))
	if r.Empty() {
		return out
	}
	for name, v := range out.Header {
		out.Header[name] = renderRaw(v, r.use, r)
	}
	out.Body = renderBody(exp.Body, "", r.use, r)
	if streamHasPlaceholder(exp.StreamBody) {
		out.StreamBody = make([]models.HTTPStreamChunk, len(exp.StreamBody))
		for i, c := range exp.StreamBody {
			out.StreamBody[i] = models.HTTPStreamChunk{TS: c.TS, Data: make([]models.HTTPStreamDataField, len(c.Data))}
			for j, f := range c.Data {
				out.StreamBody[i].Data[j] = models.HTTPStreamDataField{Key: f.Key, Value: renderBody(f.Value, "", r.use, r)}
			}
		}
	}
	return out
}

func streamHasPlaceholder(chunks []models.HTTPStreamChunk) bool {
	for _, c := range chunks {
		for _, f := range c.Data {
			if Contains(f.Value) {
				return true
			}
		}
	}
	return false
}

// soleCapture is the capture a JSON string leaf holds when it holds one
// placeholder and that is a capture.
func soleCapture(doc string, l Leaf) (Placeholder, bool) {
	if l.Kind != LeafString {
		return Placeholder{}, false
	}
	ps := Find(l.Raw(doc))
	if len(ps) != 1 || !ps[0].Capture {
		return Placeholder{}, false
	}
	return ps[0], true
}

// captureLeaf reads the value a JSON leaf capture takes from the answer's leaf
// at the same path; why says what is wrong when there is none.
func captureLeaf(doc string, l Leaf, p Placeholder, answer string, a Leaf, found bool) (string, string) {
	if !found {
		return "", "needs a value and the answer has none there"
	}
	raw := l.Raw(doc)
	if wholeString(raw, []Placeholder{p}) {
		switch p.Type {
		case TypeString:
			if a.Kind != LeafString {
				return "", fmt.Sprintf("needs a JSON string; the answer holds %s", a.Raw(answer))
			}
			v := a.Value(answer)
			if v == "" {
				return "", "needs a non-empty string; the answer holds \"\""
			}
			return v, ""
		case TypeInt, TypeFloat:
			text := a.Raw(answer)
			if a.Kind != LeafNumber || !fitsType(p.Type, text) {
				return "", fmt.Sprintf("needs a JSON number (%s); the answer holds %s", p.Type, text)
			}
			return text, ""
		}
		return "", "has no type"
	}
	// A capture inside a longer string: the text around it must be there,
	// and the capture is what it leaves.
	if a.Kind != LeafString {
		return "", fmt.Sprintf("needs a JSON string; the answer holds %s", a.Raw(answer))
	}
	prefix, err1 := decodeJSONFragment(raw[1:p.Start])
	suffix, err2 := decodeJSONFragment(raw[p.End : len(raw)-1])
	if err1 != nil || err2 != nil {
		return "", "sits in a string keploy cannot read"
	}
	return captureAnchored(prefix, suffix, p.Type, a.Value(answer))
}

// captureAnchored takes the text between prefix and suffix in live.
func captureAnchored(prefix, suffix, typ, live string) (string, string) {
	if len(live) < len(prefix)+len(suffix) || !strings.HasPrefix(live, prefix) || !strings.HasSuffix(live, suffix) {
		return "", fmt.Sprintf("needs the answer to read %q…%q around it; it reads %q", prefix, suffix, live)
	}
	v := live[len(prefix) : len(live)-len(suffix)]
	if !fitsType(typ, v) {
		return "", fmt.Sprintf("needs a non-empty %s; the answer holds %q there", typ, v)
	}
	return v, ""
}

func decodeJSONFragment(s string) (string, error) {
	var out string
	err := json.Unmarshal([]byte(`"`+s+`"`), &out)
	return out, err
}

func headerLookup(h map[string]string, name string) (string, bool) {
	if v, ok := h[name]; ok {
		return v, true
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

func anyHeaderPlaceholder(h map[string]string) bool {
	for _, v := range h {
		if Contains(v) {
			return true
		}
	}
	return false
}
