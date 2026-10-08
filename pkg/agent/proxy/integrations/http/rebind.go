package http

import (
	"maps"
	"net/http"
	"net/url"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

func init() {
	integrations.RegisterValueTokenizer(models.Kind(models.HTTP), integrations.ValueTokenizer{
		Request:  httpRequestValues,
		Response: httpResponseWords,
		Lookup:   httpFoundNothing,
	})
}

// rebind is one match's view of the replay's value bindings
// (integrations.ValueBindings): ids the recording carried, mapped to the ids
// the app made in their place on this replay.
//
// An app that mints its own ids sends a new one on every replay. Only a
// generated UUID is ever taken for such an id (mocknoise.IsMintedUUID), and
// two rules decide what is done about one; nothing else does:
//
//   - Exact fit. An id is bound, and an answer rewritten, only when the live
//     request IS a recorded request with this run's ids in place of the
//     recorded ones (fits). A request that merely resembles a recording — one
//     only a lenient pass could match — is answered exactly as it is without
//     rebinding: nothing bound, nothing rewritten, no creator claimed. A
//     lenient pass takes the closest recording, which may be another entity's;
//     binding on its choice is how an app is handed one entity's data under
//     another's id.
//   - Bind once. A recorded id has one live id for the rest of the test set,
//     and a live id stands for one recorded id (integrations.BindingTable).
//     A creator answers the one request it was recorded for: the first that
//     fits it binds its id, and a later one brings nothing new.
//
// An id the app sends as recorded while nothing is bound to it is a constant,
// and is never bound (Bindings.NeverBound). And a binding whose creator's own
// recorded call comes back exactly as recorded was a constant after all: it
// is disowned (disowns), and stops speaking for any request but the one that
// names its live id.
//
// A pair is bound where the recording FIRST carried the id — at its creator
// (integrations.ValueIndex.FirstCarried) — when the live request holds
// another id of the same kind in its place (align). From then on a request
// that carries the live id — a read-back by the id the app was given — is
// matched against copies of the recordings that carried the recorded id, with
// the live id in its place (candidates), and the answer of the recording it
// fits names the live id (render).
//
// What this does not follow, on purpose; each replays as it does without
// rebinding:
//
//   - a creator asked a second time with another id: a retry that mints again,
//     more creates than were recorded, one recording replayed by several
//     parallel workers that each mint their own;
//   - a create or a read-back whose request carries a field that changes on
//     every run and is not request-body noise (a timestamp the app stamps into
//     the payload): it is no recording's request, ids aside;
//   - an id the app first sends where two requests cannot be lined up: in a
//     header, in a form or NDJSON body, inside a longer string;
//   - a replay under strict request-body noise (match keeps rebinding out).
//
// Ids are replaced as whole words (mocknoise.ReplaceWords), here and on the
// test-case side of a replay: an id that is part of a longer token
// ("order-<id>") is another word, and stays as recorded on both.
type rebind struct {
	binder *integrations.Bindings
	index  integrations.ValueIndex
	in     *req
	// same reports whether a request is the one a pooled mock recorded, as
	// the matcher's exact passes read one (HTTP.sameRequest).
	same   func(in *req, m *models.Mock) bool
	active bool // anything was bound when the match began
	// hits are the bindings whose live value this request carries, anywhere
	// in it: recorded -> live. sentAsRecorded are the bound recorded values it
	// carries as they were recorded; such an id is in no hit — a request that
	// names an id both ways speaks of the recorded entity.
	hits           map[string]string
	sentAsRecorded map[string]bool
	origin         map[*models.Mock]*models.Mock // copy -> the pooled mock it stands for
	alignments     map[*models.Mock]alignment    // per creator, for this request
	fitted         map[*models.Mock]bool         // per pooled mock, for this request
}

// newRebind returns nil — rebinding off — when the store keeps no bindings
// for the staged set (the replay did not ask for rebinding, or the set is not
// one that can be followed) or no value index.
//
// The live request is read once, for two things. The ids of this run it
// carries (hits). And the recorded values it carries exactly as recorded,
// which the table is told (Bindings.SentAsRecorded): an id the app still sends
// as recorded is not one this run makes anew.
func newRebind(mockDb integrations.MockMemDb, in *req, same func(in *req, m *models.Mock) bool) *rebind {
	vb, ok := mockDb.(integrations.ValueBindings)
	if !ok {
		return nil
	}
	ix, ok := mockDb.(integrations.ValueIndex)
	if !ok {
		return nil
	}
	b := vb.Bindings()
	if b == nil {
		return nil
	}
	r := &rebind{binder: b, index: ix, in: in, same: same, active: b.Active()}
	liveRequestWords(in, func(w string) {
		if rec, ok := b.Recorded(w); ok {
			if r.hits == nil {
				r.hits = map[string]string{}
			}
			r.hits[rec] = w
		}
		if !ix.Carries(w) {
			return
		}
		if _, bound := b.Live(w); bound {
			if r.sentAsRecorded == nil {
				r.sentAsRecorded = map[string]bool{}
			}
			r.sentAsRecorded[w] = true
		}
		b.SentAsRecorded(w)
	})
	for rec := range r.sentAsRecorded {
		delete(r.hits, rec)
	}
	return r
}

// creator returns the pooled mock m stands for when it is a creator: the
// first carrier of a value some match may bind. A creator is claimed by the
// one live request it was recorded for (commit), as a recording of a create
// is one entity's — whether or not its values were bound: a value that is the
// same on record and replay (a tenant id from a fixture) never is, and its
// creator is still spent.
func (r *rebind) creator(m *models.Mock) (*models.Mock, bool) {
	orig, _ := r.original(m)
	return orig, len(r.index.FirstCarried(orig)) > 0
}

// inOrder puts first, among ms, the recordings the live request is — as far
// as rebinding can tell — and leaves every other mock where it was:
//
//   - the copies it fits: recordings that carry this run's ids where the
//     request does. A recording that matches only through url-noise, or by
//     an equal body, would otherwise tie with the copy and win on pool order;
//   - then the one creator it can claim: the earliest recorded that is
//     unclaimed, introduced an id the request made anew, and that the request
//     fits. (A mock with no recorded time is no creator: see
//     integrations.BuildMockValueIndex.) Two creates recorded with
//     identical payloads but their own minted ids fit a live create equally
//     well; left in pool order, which a match reshuffles, the same one could
//     win every time and the other's id would never be bound.
//
// A request that brings no id of this run moves nothing: what answers it is
// chosen from the pool's own order — its rotation — exactly as it is without
// rebinding.
func (r *rebind) inOrder(ms []*models.Mock) []*models.Mock {
	if r == nil || len(ms) < 2 {
		return ms
	}
	var first []*models.Mock
	var claimable *models.Mock
	for _, m := range ms {
		orig, viaCopy := r.original(m)
		if viaCopy && r.fits(orig) {
			first = append(first, m)
			continue
		}
		c, ok := r.creator(m)
		if !ok || r.binder.Claimed(c.Name) || !r.fresh(c) || !r.fits(c) {
			continue
		}
		if claimable == nil || integrations.RecordedBefore(m, claimable) {
			claimable = m
		}
	}
	if claimable != nil {
		first = append(first, claimable)
	}
	if len(first) == 0 {
		return ms
	}
	moved := make(map[*models.Mock]bool, len(first))
	for _, m := range first {
		moved[m] = true
	}
	out := append(make([]*models.Mock, 0, len(ms)), first...)
	for _, m := range ms {
		if !moved[m] {
			out = append(out, m)
		}
	}
	return out
}

// alignment is how the live request lines up with a creator's recorded one,
// for the values that creator first carries.
type alignment struct {
	// ok: every such value lined up — the live request holds it as recorded,
	// or one value made this run in its place, everywhere it stands. When it
	// is not, nothing is said about any of them: a request is bound whole or
	// not at all, or an answer would name one entity by two ids.
	ok bool
	// pairs are the values the live request holds another in place of:
	// recorded -> live. byLive is the same, read the other way. They are
	// what the request is compared with the recording under (fits).
	pairs, byLive map[string]string
	// follow are those of pairs the replay follows (ValueIndex.MayBind):
	// what a match binds and renders. The rest line the request up with its
	// own recording and are answered as recorded.
	follow map[string]string
}

// align lines the live request up with creator m's recorded one
// (mocknoise.AlignStrings: URL path segments, query values, JSON strings by
// key and array position), once per match.
//
// An id made this run in place of a recorded one is a UUID, as every id that
// is followed is (mocknoise.IsMintedUUID), that the recording carries nowhere,
// and its only partner: it stands for no other recorded id, and the recorded
// id has no other live one yet (bind once) and was never sent as recorded
// (Bindings.NeverBound).
func (r *rebind) align(m *models.Mock) alignment {
	if al, done := r.alignments[m]; done {
		return al
	}
	al := r.alignTo(m)
	if r.alignments == nil {
		r.alignments = map[*models.Mock]alignment{}
	}
	r.alignments[m] = al
	return al
}

func (r *rebind) alignTo(m *models.Mock) alignment {
	first := r.index.FirstCarried(m)
	if len(first) == 0 || m.Spec.HTTPReq == nil || r.in == nil {
		return alignment{}
	}
	mine := make(map[string]bool, len(first))
	for _, v := range first {
		mine[v] = true
	}
	al := alignment{ok: true, pairs: map[string]string{}, byLive: map[string]string{}}
	unchanged := map[string]bool{}
	liveURL := ""
	if r.in.url != nil {
		liveURL = r.in.url.String()
	}
	mocknoise.AlignStrings(m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body, liveURL, string(r.in.body),
		func(rec, live string) {
			if !mine[rec] {
				return // any other difference is the matcher's to judge
			}
			if rec == live {
				unchanged[rec] = true
				return
			}
			if !mocknoise.IsUUID(live) || r.index.Carries(live) || r.binder.NeverBound(rec) {
				al.ok = false // not a UUID made this run, or not an id this run makes anew
				return
			}
			if was, ok := al.byLive[live]; ok && was != rec {
				al.ok = false
				return
			}
			if was, ok := r.binder.Recorded(live); ok && was != rec {
				al.ok = false
				return
			}
			if was, ok := r.binder.Live(rec); ok && was != live {
				al.ok = false // bound once: this is a second id for the entity
				return
			}
			if was, ok := al.pairs[rec]; ok && was != live {
				al.ok = false
				return
			}
			al.pairs[rec], al.byLive[live] = live, rec
		},
		func(rec string) {
			if mine[rec] {
				al.ok = false
			}
		})
	for _, v := range first {
		_, made := al.pairs[v]
		if made == unchanged[v] { // seen nowhere, or as recorded in one place and made anew in another
			al.ok = false
		}
	}
	if !al.ok {
		return alignment{}
	}
	al.follow = make(map[string]string, len(al.pairs))
	for rec, live := range al.pairs {
		if r.index.MayBind(rec) {
			al.follow[rec] = live
		}
	}
	return al
}

// fits reports whether the live request is the pooled mock m's recorded
// request with this run's ids in place of the recorded ones. It is the one
// gate: only a request that fits binds an id, claims a creator, or has its
// answer rewritten (HTTP.claim asks it for every pass).
//
// This run's ids are put back to the recorded ones in the URL path, the raw
// query and the body: the ids bound earlier, and, when m is a creator — which
// must line up whole (align) — those the request made in place of the ids m
// introduced. What results must be m's request as the exact passes read one
// (same): the exact URL pass, and the recorded body but for request-body
// noise.
//
// Every bound id m's request carries must also be named by the request, by
// the id this run made for it and nowhere as recorded. The comparison cannot
// see that: put back, an id sent as recorded reads like one made this run; a
// header's value is not compared at all; and a field that is noise may hold
// anything. A request that names such an id as recorded speaks of the recorded
// entity, and one that names something else in its place asks about another.
func (r *rebind) fits(m *models.Mock) bool {
	if r == nil || r.in == nil || r.in.url == nil || m == nil || m.Spec.HTTPReq == nil {
		return false
	}
	m, _ = r.original(m)
	if fit, done := r.fitted[m]; done {
		return fit
	}
	fit := r.fitsTo(m)
	if r.fitted == nil {
		r.fitted = map[*models.Mock]bool{}
	}
	r.fitted[m] = fit
	return fit
}

func (r *rebind) fitsTo(m *models.Mock) bool {
	var al alignment
	if _, ok := r.creator(m); ok {
		if al = r.align(m); !al.ok {
			return false
		}
	}
	for _, v := range r.index.RequestValues(m) {
		if r.sentAsRecorded[v] {
			return false
		}
		if _, ok := al.pairs[v]; ok {
			continue
		}
		if _, ok := r.hits[v]; ok {
			continue
		}
		if _, bound := r.binder.Live(v); bound {
			return false
		}
	}
	if len(al.byLive) == 0 && len(r.hits) == 0 {
		return r.same(r.in, m)
	}
	back := func(w string) (string, bool) {
		if rec, ok := al.byLive[w]; ok {
			return rec, true
		}
		return r.binder.Recorded(w)
	}
	in, u := *r.in, *r.in.url
	u.Path = mocknoise.ReplaceWords(u.Path, back)
	u.RawQuery = mocknoise.ReplaceWords(u.RawQuery, back)
	in.url = &u
	in.body = []byte(mocknoise.ReplaceWords(string(r.in.body), back))
	return r.same(&in, m)
}

// fresh reports whether creator c introduced an id the live request made anew
// and nothing has bound yet.
func (r *rebind) fresh(c *models.Mock) bool {
	for rec := range r.align(c).pairs {
		if _, bound := r.binder.Live(rec); !bound {
			return true
		}
	}
	return false
}

// templateMatch finds a create made with the id the app minted this run: the
// first of ms, in the order given (inOrder), whose pooled recording
// is a creator that introduced an id nothing has bound yet and that the live
// request fits. nil when there is none.
//
// It is what recognises such a create by its shape, without a lenient pass:
// scored as text, one random id is a few edits closer to another by chance,
// and that noise would decide between two recordings of the same call. A
// field that drifts on every run and is not noise means no creator fits, and
// the lenient passes decide as they always have — binding nothing.
//
// The creator is returned as a copy carrying this request's ids — those bound
// earlier, and those it just made — beside every other candidate copied the
// same way (sent). They are for the stateful cursor: a later, byte-identical
// request matches exactly such copies on the exact pass, so a cursor advanced
// over these is the one that request reads, and it gets the next recording
// (a create answered 201, then 409) and not this one again.
func (r *rebind) templateMatch(ms []*models.Mock) (creator *models.Mock, sent []*models.Mock) {
	if r == nil {
		return nil, nil
	}
	for i := range ms {
		if creator, sent = r.asSent(ms, i); creator != nil {
			return creator, sent
		}
	}
	return nil, nil
}

// asSent returns ms[i] and every other mock of ms as copies carrying this
// request's ids — those bound earlier, and those it makes at ms[i] — when
// ms[i] is a creator that introduced an id nothing has bound yet and that the
// live request fits; nil otherwise. The copies are what the stateful cursor
// groups: see templateMatch.
func (r *rebind) asSent(ms []*models.Mock, i int) (creator *models.Mock, sent []*models.Mock) {
	if r == nil {
		return nil, nil
	}
	c, ok := r.creator(ms[i])
	if !ok || !r.fresh(c) || !r.fits(c) {
		return nil, nil
	}
	live := maps.Clone(r.align(c).follow)
	maps.Copy(live, r.hits)
	sent = r.copies(r.originals(ms), live)
	return sent[i], sent
}

// fitting returns the pooled recording of the first of ms, in the order given
// (inOrder), that the live request fits only through this run's ids; nil when
// none does. A request that fits a recording with no id of this run in play is
// the matcher's to judge as it is without rebinding.
func (r *rebind) fitting(ms []*models.Mock) *models.Mock {
	if r == nil {
		return nil
	}
	for _, m := range ms {
		if orig, _ := r.original(m); r.rebinds(orig) && r.fits(orig) {
			return orig
		}
	}
	return nil
}

// rebinds reports whether this run's ids are in play between the live request
// and the pooled mock m: m's request carries a bound id the live request names
// by this run's id. (A creator the request makes ids anew at is the template
// pass's: templateMatch.)
func (r *rebind) rebinds(m *models.Mock) bool {
	for _, v := range r.index.RequestValues(m) {
		if _, ok := r.hits[v]; ok {
			return true
		}
	}
	return false
}

// candidates returns pool with every mock whose request carries a value this
// request's live values are bound to replaced, in place, by a copy carrying
// the live values. The copy stands in for its original: matching both would
// let the original, which can only match leniently, tie with the copy.
func (r *rebind) candidates(pool []*models.Mock) []*models.Mock {
	if r == nil {
		return pool
	}
	return r.copies(pool, r.hits)
}

// copies returns pool with every mock whose request carries a value live
// (recorded -> live) names replaced, in place, by a copy carrying the live
// values; pool itself when none does.
func (r *rebind) copies(pool []*models.Mock, live map[string]string) []*models.Mock {
	if len(live) == 0 {
		return pool
	}
	var out []*models.Mock
	for i, m := range pool {
		v := r.variant(m, live)
		if v == nil {
			if out != nil {
				out = append(out, m)
			}
			continue
		}
		if out == nil {
			out = make([]*models.Mock, i, len(pool))
			copy(out, pool[:i])
		}
		out = append(out, v)
	}
	if out == nil {
		return pool
	}
	return out
}

// variant returns a copy of the pooled mock m with the values its request
// carries that live (recorded -> live) names replaced by the live ones, or nil
// when its request carries none of them. The pooled mock is never touched.
func (r *rebind) variant(m *models.Mock, live map[string]string) *models.Mock {
	if m == nil || m.Spec.HTTPReq == nil {
		return nil
	}
	carries := false
	for _, v := range r.index.RequestValues(m) {
		if _, ok := live[v]; ok {
			carries = true
			break
		}
	}
	if !carries {
		return nil
	}
	hit := func(w string) (string, bool) { to, ok := live[w]; return to, ok }
	rq := m.Spec.HTTPReq
	nr := *rq
	nr.URL = mocknoise.ReplaceWords(rq.URL, hit)
	nr.Body = mocknoise.ReplaceWords(rq.Body, hit)
	nr.Header = replaceValues(rq.Header, hit)
	nr.URLParams = replaceValues(rq.URLParams, hit)
	v := m.ShallowCopy()
	v.Spec.HTTPReq = &nr
	if r.origin == nil {
		r.origin = make(map[*models.Mock]*models.Mock)
	}
	r.origin[v] = m
	return v
}

func replaceValues(in map[string]string, lookup func(string) (string, bool)) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = mocknoise.ReplaceWords(v, lookup)
	}
	return out
}

// original resolves a matched mock to the pooled one it stands for: the mock
// itself, or the original of a copy.
func (r *rebind) original(m *models.Mock) (orig *models.Mock, viaCopy bool) {
	if r == nil {
		return m, false
	}
	if o, ok := r.origin[m]; ok {
		return o, true
	}
	return m, false
}

// originals maps a candidate list back to the pooled mocks.
func (r *rebind) originals(ms []*models.Mock) []*models.Mock {
	if r == nil || len(r.origin) == 0 {
		return ms
	}
	out := make([]*models.Mock, len(ms))
	for i, m := range ms {
		out[i], _ = r.original(m)
	}
	return out
}

// commit stores what answering the live request from the pooled mock m, which
// it fits, established: that m, when it is a creator, is claimed, and that the
// ids m introduced that the replay follows stand for those the request made in
// their place. It reports whether that holds now. It does not when the test
// set ended while the match ran, or when another request claimed m or bound
// one of those ids first: the match is then made again (HTTP.claim), and
// finds the next recording.
func (r *rebind) commit(m *models.Mock) bool {
	c, ok := r.creator(m)
	if !ok {
		return true
	}
	return r.binder.Commit(c.Name, r.align(c).follow)
}

// disowns handles a live request that is, exactly as recorded, the request of
// a creator whose id is bound to something else: the app made the call that
// introduced the id again with the RECORDED id. So the id is a constant — a
// fixture's, a tenant's — and what was bound to it earlier was a call about
// another entity that reached this recording first. The binding is disowned
// (Bindings.Disown); the ids newly disowned are returned.
func (r *rebind) disowns(m *models.Mock) (recorded []string) {
	if r == nil || len(r.sentAsRecorded) == 0 {
		return nil
	}
	c, ok := r.creator(m)
	if !ok || !r.same(r.in, c) {
		return nil
	}
	for _, v := range r.index.FirstCarried(c) {
		if r.sentAsRecorded[v] && r.binder.Disown(v) {
			recorded = append(recorded, v)
		}
	}
	return recorded
}

// other returns, for a live request that does not fit the pooled mock m, a
// bound id m's recorded request carries that the live request names neither
// by the id this run made for it nor as recorded, with that id: the request
// names something else in its place, so m is the recording of another entity.
func (r *rebind) other(m *models.Mock) (recorded, live string, ok bool) {
	if r == nil || !r.active {
		return "", "", false
	}
	for _, v := range r.index.RequestValues(m) {
		if _, hit := r.hits[v]; hit || r.sentAsRecorded[v] {
			continue
		}
		if live, bound := r.binder.Live(v); bound {
			return v, live, true
		}
	}
	return "", "", false
}

// unfollowed reports whether answering the live request from the pooled mock
// m, which it does not fit, leaves an id unfollowed: m introduces ids, and
// they stay unbound; or the live request carries this run's id for one m's
// request names, and the answer will still name the recorded one.
func (r *rebind) unfollowed(m *models.Mock) bool {
	if r == nil {
		return false
	}
	if _, ok := r.creator(m); ok {
		return true
	}
	for _, v := range r.index.RequestValues(m) {
		if _, ok := r.hits[v]; ok {
			return true
		}
	}
	return false
}

// render returns served — the answer of the pooled mock m, which the live
// request fits — with every bound id its response carries replaced by the one
// this run made for it, on a copy. It decides nothing: whether an answer may
// name this run's ids is fits' to say.
//
// An id the request itself names — one it carries, or one it just made at
// its creator — is answered with that id. Any other is answered with what the
// table says by default (Bindings.Default): the id bound to it, unless the app
// has since sent the recorded one as recorded.
//
// served itself is returned when the response carries no bound id, or carries
// an integrity header that cannot be recomputed (see reSign). served may be m's
// answer as renderCorrelations left it, already signed for its body.
func (r *rebind) render(served, m *models.Mock) *models.Mock {
	if served == nil || served.Spec.HTTPResp == nil || !r.binder.Active() {
		return served
	}
	var made map[string]string
	if c, ok := r.creator(m); ok {
		made = r.align(c).follow
	}
	live := func(w string) (string, bool) {
		if v, ok := r.hits[w]; ok {
			return v, true
		}
		if v, ok := made[w]; ok {
			return v, true
		}
		return r.binder.Default(w)
	}
	rs := served.Spec.HTTPResp
	body := mocknoise.ReplaceWords(rs.Body, live)
	header := replaceValues(rs.Header, live)
	changed := body != rs.Body
	for k, v := range header {
		changed = changed || v != rs.Header[k]
	}
	if !changed {
		return served
	}
	if body != rs.Body && !reSign(header, rs.Body, body) {
		return served
	}
	out := served.ShallowCopy()
	rc := *rs
	rc.Body = body
	rc.Header = header
	out.Spec.HTTPResp = &rc
	return out
}

// httpRequestValues is the HTTP request tokenizer for the value index
// (integrations.ValueTokenizer.Request).
//
// carried: the app-random words of the URL path and query, the headers and
// the body, read as text — everything, wherever it stands and whatever the
// body is. bindable: those of them at a place align can line up with a live
// request (mocknoise.WalkStrings: a URL path segment, the value of a query key
// that has one, a string of a JSON body). The second list comes from the
// aligner's own walk, so a value is never called bindable where no match
// could bind it; a body that is not one JSON document (a form, NDJSON) has no
// such place.
//
// A request of a safe method (GET, HEAD, OPTIONS, TRACE: RFC 9110 §9.2.1)
// has no bindable place at all: it looks something up and makes nothing. One
// answered with success found the id, which existed before it — a fixture's,
// a configured one — so it is the id's first carrier and the id is never
// bound: a lookup of a constant is never taken for the create of a fresh id
// that happens to come first, as a test runner that reorders tests would have
// it. One that found nothing (httpFoundNothing: a 404) introduces the id it
// asked about nowhere, and an id looked up before it is made — a HEAD
// answered 404, then the PUT that makes it — is bound at the write.
//
// A body recorded under a Content-Encoding the recorder does not decode
// (pkg.DecodesContentEncoding) is stored as it was sent, and no word of it can
// be read: the request is reported unreadable.
func httpRequestValues(m *models.Mock) (carried, bindable []string, readable bool) {
	rq := m.Spec.HTTPReq
	if rq == nil {
		return nil, nil, true
	}
	for k, v := range rq.Header {
		if http.CanonicalHeaderKey(k) == "Content-Encoding" && !pkg.DecodesContentEncoding(v) {
			return nil, nil, false
		}
	}
	var c, b valueSet
	if u, err := url.Parse(rq.URL); err == nil {
		c.words(u.Path)
		c.words(u.RawQuery)
	}
	for _, v := range rq.URLParams {
		c.words(v)
	}
	for _, v := range rq.Header {
		c.words(v)
	}
	body := ""
	if c.words(rq.Body) {
		body = rq.Body // only a body that carries a value is worth decoding
	}
	if safeMethod(string(rq.Method)) {
		return c.list, nil, true
	}
	mocknoise.WalkStrings(rq.URL, body, func(s string) {
		if _, ok := c.seen[s]; ok {
			b.add(s)
		}
	})
	return c.list, b.list, true
}

// httpFoundNothing is the HTTP ValueTokenizer.Lookup: a safe request answered
// 404 Not Found looked for something that was not there. Any other answer
// found something: a 304 (it exists, unchanged), a redirect, a 401 or 403 (it
// exists, and is refused), a 410 (it existed, and was removed).
func httpFoundNothing(m *models.Mock) bool {
	rq, rs := m.Spec.HTTPReq, m.Spec.HTTPResp
	return rq != nil && safeMethod(string(rq.Method)) && rs != nil && rs.StatusCode == http.StatusNotFound
}

// safeMethod reports whether an HTTP method is safe (RFC 9110 §9.2.1): it
// asks for something and changes nothing.
func safeMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// httpResponseWords is the HTTP response tokenizer for the value index
// (integrations.ValueTokenizer.Response): every word of the headers and the
// whole body. A body recorded under a Content-Encoding the recorder does not
// decode (pkg.DecodesContentEncoding) is stored as it was sent, and no word of
// it can be read: the response is reported unreadable.
func httpResponseWords(m *models.Mock, visit func(string)) (readable bool) {
	rs := m.Spec.HTTPResp
	if rs == nil {
		return true
	}
	readable = true
	for k, v := range rs.Header {
		if http.CanonicalHeaderKey(k) == "Content-Encoding" && !pkg.DecodesContentEncoding(v) {
			readable = false
		}
		mocknoise.ScanWords(v, func(start, end int) { visit(v[start:end]) })
	}
	mocknoise.ScanWords(rs.Body, func(start, end int) { visit(rs.Body[start:end]) })
	return readable
}

// liveRequestWords visits every word of a live request's URL path and query,
// headers and body.
func liveRequestWords(in *req, visit func(string)) {
	if in == nil {
		return
	}
	each := func(s string) { mocknoise.ScanWords(s, func(start, end int) { visit(s[start:end]) }) }
	if in.url != nil {
		each(in.url.Path)
		each(in.url.RawQuery)
	}
	for _, vs := range in.header {
		for _, v := range vs {
			each(v)
		}
	}
	each(string(in.body))
}

// valueSet collects distinct app-random values in first-seen order.
type valueSet struct {
	seen map[string]struct{}
	list []string
}

// add adds v when it is an app-random value, and reports whether it is one.
func (t *valueSet) add(v string) bool {
	if _, ok := mocknoise.AppRandomClass(v); !ok {
		return false
	}
	if _, dup := t.seen[v]; dup {
		return true
	}
	if t.seen == nil {
		t.seen = map[string]struct{}{}
	}
	t.seen[v] = struct{}{}
	t.list = append(t.list, v)
	return true
}

// words adds the app-random words of s, and reports whether it holds any.
func (t *valueSet) words(s string) (found bool) {
	mocknoise.ScanWords(s, func(start, end int) { found = t.add(s[start:end]) || found })
	return found
}
