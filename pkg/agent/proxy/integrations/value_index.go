package integrations

import (
	"fmt"
	"slices"
	"sort"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

// MockValueIndex is a staged set's ValueIndex: for every mock of a kind with a
// registered ValueTokenizer, the app-random values its request carries, and
// for every such value the mock that carried it first in recorded order.
//
// It is built once, from the whole set, when the set is staged, and replaced
// only by the next set's staging — so it still knows a mock a replay has since
// consumed, or that lies outside the current test's window. That is what makes
// "the matched mock is the first to carry this value" a fact about the
// recording, and not about whichever mocks happen to be left in the pool.
//
// Only a value some request carries can ever be bound, so a response's words
// are used only to find a first carrier (a value a dependency handed the app is
// first carried by that response) and are not kept. A value carried by a mock
// with no recorded time cannot be ordered, so it has no first carrier and is
// never bound.
//
// What it knows is what the recording shows as text. An id a response hands
// over inside base64 or a signed token, or in another letter case than the
// request that sends it back, is not seen there, and that request then passes
// for the first to carry it.
//
// Mocks are told apart by identity (Name, Kind, recorded request time), not by
// pointer: a match replaces a pooled mock with an updated copy, and the copy
// must still be found. All methods accept a nil index.
type MockValueIndex struct {
	req     map[mockIdentity][]string
	first   map[mockIdentity][]string // ids each mock is the first carrier of, at a bindable place
	carried map[string]bool           // every value some request carries
	mayBind func(recorded string) bool
}

// mockIdentity is a recording's identity across copies of it.
type mockIdentity struct {
	name string
	kind models.Kind
	at   int64
}

func identityOf(mk *models.Mock) (mockIdentity, bool) {
	if mk == nil || mk.Name == "" {
		return mockIdentity{}, false
	}
	return mockIdentity{name: mk.Name, kind: mk.Kind, at: mk.Spec.ReqTimestampMock.UnixNano()}, true
}

// valueFreeKinds are the mock kinds that carry no value an app could mint (a
// DNS answer names hosts), so a set may hold them and still be followed.
var valueFreeKinds = map[models.Kind]bool{models.DNS: true}

// BuildMockValueIndex indexes the given mocks (a mock listed twice counts
// once). mayBind says which recorded values the replay may bind (see
// models.Rebinding).
//
// Every id (mocknoise.IsUUID) gets a creator, whether the replay follows it
// or not. Two creates of one shape, one with an id the replay follows and an
// earlier one with an id it does not, must each line up with their own
// recording: were only the followed id's recording a creator, the earlier
// create's request would be taken for it, and the followed id bound to the
// wrong entity's id.
//
// The index is nil — nothing is bound, and the replay is what it is without
// rebinding — when no request carries, where it can be lined up, a value the
// replay may bind; and when the set cannot be followed, in which case why
// says, for a person reading a log, what stands in the way:
//
//   - The set holds a mock of a kind that has no tokenizer and is not
//     value-free. An id the app mints usually reaches more than one
//     dependency: it is INSERTed into a database and sent to an HTTP service.
//     Following it in the HTTP mocks while the database mocks still hold the
//     recorded one would answer the app with two ids for one entity. So a set
//     is followed whole or not at all.
//   - A response cannot be read: its tokenizer says so (a body stored in an
//     encoding the recorder did not decode), or it is kept apart from its mock
//     (Mock.HasSpilledResponse) and could not be loaded. A value a dependency
//     handed the app is first carried by that response and is not the app's to
//     bind; with a response unread, the request that sends such a value back
//     would pass for the first to carry it.
//
// Requests are tokenized first; a response is then only scanned for words
// some request carried, and nothing is kept of it — a response kept apart
// from its mock is loaded onto a copy, never onto the mock a replay serves —
// so the build holds one list of values per request and costs one pass over
// every payload.
func BuildMockValueIndex(mayBind func(recorded string) bool, lists ...[]*models.Mock) (ix *MockValueIndex, why string) {
	type entry struct {
		mk            *models.Mock
		id            mockIdentity
		resp          func(*models.Mock, func(string)) bool
		req, bindable []string
		lookup        bool // looked for something and found nothing: introduces no id
	}
	var all []entry
	seen := map[mockIdentity]bool{}
	requested := map[string]bool{}
	followable := false
	for _, list := range lists {
		for _, mk := range list {
			if mk == nil {
				continue
			}
			tk, ok := valueTokenizerFor(mk.Kind)
			if !ok {
				if valueFreeKinds[mk.Kind] {
					continue
				}
				return nil, fmt.Sprintf("mock %q is of kind %s, and keploy cannot follow ids in mocks of that kind yet: an id followed in some dependencies only would be two ids for one entity", mk.Name, mk.Kind)
			}
			id, ok := identityOf(mk)
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			req, bindable, readable := tk.Request(mk)
			if !readable {
				return nil, fmt.Sprintf("the request of mock %q is stored in an encoding keploy does not decode, so the ids it carries cannot be read and nothing can be said about which call first sent an id", mk.Name)
			}
			for _, v := range req {
				requested[v] = true
			}
			followable = followable || slices.ContainsFunc(bindable, mayBind)
			all = append(all, entry{mk: mk, id: id, resp: tk.Response, req: req, bindable: bindable, lookup: tk.Lookup != nil && tk.Lookup(mk)})
		}
	}
	if !followable {
		return nil, ""
	}
	// words calls visit with the words of e's response some request carried.
	words := func(e entry, visit func(string)) (why string) {
		if e.resp == nil {
			return ""
		}
		mk, err := e.mk.WithResponse()
		if err != nil {
			return fmt.Sprintf("the recorded response of mock %q could not be loaded (%v), and it may be where a dependency first handed the app an id", e.mk.Name, err)
		}
		readable := e.resp(mk, func(w string) {
			if requested[w] {
				visit(w)
			}
		})
		if !readable {
			return fmt.Sprintf("the recorded response of mock %q is stored in an encoding keploy does not decode, and it may be where a dependency first handed the app an id", e.mk.Name)
		}
		return ""
	}

	// An untimed carrier makes a value unorderable: no first carrier.
	unordered := map[string]bool{}
	var timed []entry
	for _, e := range all {
		if !e.mk.Spec.ReqTimestampMock.IsZero() {
			timed = append(timed, e)
			continue
		}
		for _, v := range e.req {
			unordered[v] = true
		}
		if why := words(e, func(w string) { unordered[w] = true }); why != "" {
			return nil, why
		}
	}
	sort.SliceStable(timed, func(i, j int) bool { return RecordedBefore(timed[i].mk, timed[j].mk) })

	ix = &MockValueIndex{req: map[mockIdentity][]string{}, first: map[mockIdentity][]string{}, carried: requested, mayBind: mayBind}
	for _, e := range all {
		if len(e.req) > 0 {
			ix.req[e.id] = e.req
		}
	}
	taken := map[string]bool{}
	for _, e := range timed {
		for _, v := range e.req {
			if e.lookup {
				break // a lookup introduces nothing (ValueTokenizer.Lookup)
			}
			if unordered[v] || taken[v] {
				continue
			}
			taken[v] = true
			if !mocknoise.IsUUID(v) {
				continue // not an id
			}
			if slices.Contains(e.bindable, v) {
				ix.first[e.id] = append(ix.first[e.id], v)
			}
		}
		// First carried by a response: never bound. A lookup that found
		// nothing echoing what it asked for (S3's <Key>, "no item <id>")
		// hands nothing over by that; every other word of its answer (a
		// request id, an error id) the dependency made, and takes.
		if why := words(e, func(w string) {
			if !e.lookup || !slices.Contains(e.req, w) {
				taken[w] = true
			}
		}); why != "" {
			return nil, why
		}
	}
	return ix, ""
}

// RequestValues implements ValueIndex.
func (ix *MockValueIndex) RequestValues(mk *models.Mock) []string {
	if ix == nil {
		return nil
	}
	id, ok := identityOf(mk)
	if !ok {
		return nil
	}
	return ix.req[id]
}

// FirstCarried implements ValueIndex.
func (ix *MockValueIndex) FirstCarried(mk *models.Mock) []string {
	if ix == nil {
		return nil
	}
	id, ok := identityOf(mk)
	if !ok {
		return nil
	}
	return ix.first[id]
}

// Carries implements ValueIndex.
func (ix *MockValueIndex) Carries(v string) bool {
	return ix != nil && ix.carried[v]
}

// MayBind implements ValueIndex.
func (ix *MockValueIndex) MayBind(v string) bool {
	return ix != nil && ix.mayBind != nil && ix.mayBind(v)
}
