package templates

import (
	"fmt"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/bodydigest"
	"go.keploy.io/server/v3/pkg/models"
)

// Keep is what keploy normalize writes for a test case whose answer it
// accepts. It starts from the app's answer and puts back each placeholder of
// the expected response on disk where the run's resolved expected value at
// that place equals what the app answered there: the placeholder still says
// what the test case means, and the next run passes with it. A placeholder
// whose place the app now answers differently is dropped for the answer, and
// dropped says which and why.
//
// A digest header on disk is a digest of the body as recorded: the body with
// each placeholder at its config.yaml value, the body every run's digest
// rule starts from (Resolved.RecordedBody). So when Keep puts a placeholder
// back into the body, each header of the answer that is a digest of the
// answer's body (bodydigest.Form: an Express, Rails or Spring ETag, a hex
// ETag, Content-MD5, Digest) is written as the same digest of the kept body
// rendered with r's recorded values. Any other header is the answer's.
//
// An expected response with no placeholder comes back as the answer itself.
func (r *Run) Keep(onDisk, resolved, answer models.HTTPResp) (models.HTTPResp, []string) {
	out := answer
	out.Header = copyStringMap(answer.Header)
	var dropped []string
	if Contains(onDisk.Body) {
		out.Body, dropped = keepBody(onDisk.Body, resolved.Body, answer.Body)
	}
	for _, name := range sortedKeys(onDisk.Header) {
		v := onDisk.Header[name]
		if !Contains(v) {
			continue
		}
		live, inAnswer := headerLookup(answer.Header, name)
		want, inResolved := headerLookup(resolved.Header, name)
		if strings.EqualFold(name, "Set-Cookie") && inAnswer {
			kept, drops := r.keepSetCookie(name, v, want, live)
			dropped = append(dropped, drops...)
			for k := range out.Header {
				if strings.EqualFold(k, name) {
					delete(out.Header, k)
				}
			}
			out.Header[name] = kept
			continue
		}
		if !inAnswer || !inResolved || want != live {
			dropped = append(dropped, fmt.Sprintf("header %s: %s (the app now answers %q)", name, v, live))
			continue
		}
		for k := range out.Header {
			if strings.EqualFold(k, name) {
				delete(out.Header, k)
			}
		}
		out.Header[name] = v
	}
	if out.Body != answer.Body {
		recorded := r.recordedBody(out.Body, nil)
		for _, name := range sortedKeys(out.Header) {
			v := out.Header[name]
			lower := strings.ToLower(name)
			if !bodydigest.IsHeader(lower) || Contains(v) {
				continue
			}
			if form, ok := bodydigest.Form(lower, v, answer.Body); ok {
				out.Header[name] = form(recorded)
			}
		}
	}
	return out, dropped
}

// keepBody puts the on-disk body's placeholders back into the answer's body
// where the resolved expected body and the answer agree.
func keepBody(onDisk, resolved, answer string) (string, []string) {
	diskDoc, okDisk := parseJSON(onDisk)
	if !okDisk {
		if resolved == answer {
			return onDisk, nil
		}
		return answer, []string{"body: every placeholder (the app's body changed and is not JSON)"}
	}
	var ans []Leaf
	ansDoc, okAnswer := parseAnswer(answer)
	if okAnswer {
		ans = ansDoc.leaves
	}
	res, okResolved := JSONLeaves(resolved)
	var dropped []string
	disk := diskDoc.leaves
	if !okAnswer || !okResolved {
		for _, l := range disk {
			if Contains(l.Raw(onDisk)) {
				dropped = append(dropped, fmt.Sprintf("%s: %s (the app's body is no longer JSON)", l.Under("body"), l.Raw(onDisk)))
			}
		}
		return answer, dropped
	}
	// A placeholder in an object key is not put back: the answer's keys are
	// written.
	for _, k := range diskDoc.keys {
		if Contains(k.Raw(onDisk)) {
			dropped = append(dropped, fmt.Sprintf("body key %s: %s (the answer's keys are written)", k.DisplayPath(), k.Raw(onDisk)))
		}
	}
	ansIdx, resIdx := leafIndex(ans), leafIndex(res)
	keep := map[int]string{} // index into ans → on-disk leaf text
	for _, l := range disk {
		raw := l.Raw(onDisk)
		if !Contains(raw) {
			continue
		}
		a, inAnswer := ansIdx[l.PathKey()]
		w, inResolved := resIdx[l.PathKey()]
		if !inAnswer || !inResolved || !jsonValueEqual(w.Raw(resolved), a.Raw(answer)) {
			now := "nothing"
			if inAnswer {
				now = a.Raw(answer)
			}
			dropped = append(dropped, fmt.Sprintf("%s: %s (the app now answers %s)", l.Under("body"), raw, now))
			continue
		}
		for i, al := range ans {
			if al.Start == a.Start {
				keep[i] = raw
				break
			}
		}
	}
	if len(keep) == 0 {
		return answer, dropped
	}
	return renderJSON(answer, ans, func(Placeholder) (string, bool) { return "", false }, nil, keep), dropped
}

// keepSetCookie is the answer's Set-Cookie with each on-disk cookie whose
// whole value is a placeholder put back, cookie by cookie (by identity: name,
// Domain, Path, read at the recorded values as Check reads it), where the run
// resolved that cookie to the value the app set. Every attribute is the
// answer's.
func (r *Run) keepSetCookie(name, onDisk, resolved, live string) (string, []string) {
	liveCookies, resolvedCookies := cookiesByID(live), cookiesByID(resolved)
	var es []edit
	var dropped []string
	cookies := SplitSetCookie(onDisk)
	shadowed := r.shadowed(cookies, onDisk)
	for i, c := range cookies {
		if !Contains(onDisk[c.Start:c.End]) || shadowed[i] {
			continue
		}
		value := onDisk[c.ValueStart:c.ValueEnd]
		ps := Find(value)
		id := c.ID(onDisk, r.renderUses)
		lc, inAnswer := liveCookies[id]
		rc, inResolved := resolvedCookies[id]
		whole := len(ps) == 1 && ps[0].Start == 0 && ps[0].End == len(value)
		if !whole || !inAnswer || !inResolved || resolved[rc.ValueStart:rc.ValueEnd] != live[lc.ValueStart:lc.ValueEnd] {
			now := "no such cookie"
			if inAnswer {
				now = fmt.Sprintf("%q", live[lc.ValueStart:lc.ValueEnd])
			}
			dropped = append(dropped, fmt.Sprintf("header %s.%s: %s (the app now answers %s)", name, c.Name, onDisk[c.Start:c.End], now))
			continue
		}
		es = append(es, edit{start: lc.ValueStart, end: lc.ValueEnd, text: value})
		for _, a := range c.Attrs {
			if a.HasValue && Contains(onDisk[a.ValueStart:a.ValueEnd]) {
				dropped = append(dropped, fmt.Sprintf("header %s.%s: %s (the app's %s is written)", name, c.Name, onDisk[a.ValueStart:a.ValueEnd], a.Name))
			}
		}
	}
	sort.Slice(es, func(i, j int) bool { return es[i].start < es[j].start })
	var b strings.Builder
	last := 0
	for _, e := range es {
		b.WriteString(live[last:e.start])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(live[last:])
	return b.String(), dropped
}
