package integrations

import (
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// ValueBindings is an optional MockMemDb capability: the replay's table that
// maps an app-random value a recording carried (the recorded value) to the
// value the app made in its place on this replay (the live value).
//
// An app that generates its own ids (uuid.New()) sends a NEW one on every
// replay; only such a generated UUID is ever bound. A parser that matches the
// mock which first carried such a value (see ValueIndex.FirstCarried), and
// sees that the live request is the recorded one but for another value at
// that place, binds the pair. A later request that carries the live value — a read-back by the id
// the app was just given — can then be matched against, and answered from, the
// recording that carried the recorded one.
//
// A pair is bound once: for the rest of the test set the recorded value has
// that one live value, and the live value stands for that one recorded value
// (see BindingTable). The table lives in a numbered epoch: the agent starts a
// new one, empty, at every test-set boundary.
type ValueBindings interface {
	// Bindings returns the table as it is now, for one match; nil when the
	// store keeps none — the replay did not ask for rebinding, or the staged
	// set is not one that can be followed.
	Bindings() *Bindings
}

// Bindings is the binding table as one match sees it: the table, and the
// epoch the match began in.
type Bindings struct {
	table  *BindingTable
	epoch  uint64
	commit func(creator string, pairs map[string]string, epoch uint64) bool
}

// NewBindings is a view of table taken in epoch. commit stores what a match
// established and reports whether it did; it is given the epoch the view was
// taken in, and must store nothing when that epoch has ended, so a match that
// straddles a test-set boundary cannot carry a binding into the next set.
func NewBindings(table *BindingTable, epoch uint64, commit func(creator string, pairs map[string]string, epoch uint64) bool) *Bindings {
	return &Bindings{table: table, epoch: epoch, commit: commit}
}

// Active reports whether anything is bound; a match that finds nothing bound
// has no answer to rewrite.
func (b *Bindings) Active() bool { return b.table.Len() > 0 }

// Live returns the live value bound to recorded.
func (b *Bindings) Live(recorded string) (string, bool) { return b.table.Live(recorded) }

// Default returns the live value an answer names in place of recorded when
// the request it answers does not name the value itself (a list): the one
// bound to it — unless the binding was since disowned (Disown), in which case
// there is none and the answer keeps the recorded value.
func (b *Bindings) Default(recorded string) (string, bool) { return b.table.Default(recorded) }

// SentAsRecorded notes that the app sent recorded — a value some recorded
// request carries — exactly as it was recorded. See BindingTable.
func (b *Bindings) SentAsRecorded(recorded string) { b.table.SentAsRecorded(recorded) }

// NeverBound reports whether recorded may no longer be bound: the app sent it
// as recorded while it was unbound, so this run does not make it anew.
func (b *Bindings) NeverBound(recorded string) bool { return b.table.NeverBound(recorded) }

// Disown notes that the binding of recorded was a mistake: the app made the
// call that introduced the value again, exactly as recorded. See BindingTable.
func (b *Bindings) Disown(recorded string) bool { return b.table.Disown(recorded) }

// FirstNote reports whether this is the first time key is noted in this
// epoch, so that something worth saying once is said once.
func (b *Bindings) FirstNote(key string) bool { return b.table.FirstNote(key) }

// Recorded returns the recorded value live stands for.
func (b *Bindings) Recorded(live string) (string, bool) { return b.table.Recorded(live) }

// Claimed reports whether a match has already answered a live request from
// the creator named name (a mock that first carries a value; see Commit).
func (b *Bindings) Claimed(name string) bool { return b.table.Claimed(name) }

// Commit stores what a match established: that creator — the matched mock,
// when it is the first carrier of some value; "" otherwise — answered the
// request it was recorded for, and the recorded→live pairs that request made.
// It reports whether that now holds. It does not when the epoch has ended, or
// when the table refuses (see BindingTable.ClaimAndBind: another request took
// the creator or one of the values first): the caller then has nothing to
// answer with but the recording as it is.
func (b *Bindings) Commit(creator string, pairs map[string]string) bool {
	if creator == "" && len(pairs) == 0 {
		return true
	}
	return b.commit(creator, pairs, b.epoch)
}

// ValueIndex is an optional MockMemDb capability: the app-random values each
// mock of the staged set carries in its request, and the values each mock is
// the first recording to carry, indexed once when the whole set is staged (so
// it still knows mocks a replay has since consumed, and mocks outside the
// current test's window). Only a value some request carries can ever be bound,
// so only those are indexed.
type ValueIndex interface {
	// RequestValues returns the app-random values m's request carries,
	// anywhere in it; nil for a mock the set did not index.
	RequestValues(m *models.Mock) []string
	// FirstCarried returns the ids (UUIDs) m is the first mock of the set,
	// in recorded order, to carry (in its request or its response) and
	// carries at a bindable place in its request (see
	// ValueTokenizer.Request). A mock that has any is a creator: the
	// recording a live request with ids made anew in those places is lined
	// up with. Whether such an id is then bound is MayBind's to say.
	FirstCarried(m *models.Mock) []string
	// MayBind reports whether the replay follows the recorded value v (see
	// models.OutgoingOptions.RebindMinted / RebindValues). A creator whose
	// ids it does not follow is still claimed by the request it answers,
	// with nothing bound: so a request of the same shape that makes an id
	// the replay follows is not taken for that creator's.
	MayBind(v string) bool
	// Carries reports whether some request of the set carries v. Such a
	// value is a recorded one: a live request that carries it did not mint
	// it this run, so it is never bound as a live value.
	Carries(v string) bool
}

// RecordedBefore orders two mocks by when they were recorded: request time,
// then name in natural order (mock-9 before mock-10), so mocks recorded in the
// same instant still order as the recorder numbered them.
func RecordedBefore(a, b *models.Mock) bool {
	ta, tb := a.Spec.ReqTimestampMock, b.Spec.ReqTimestampMock
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return naturalLess(a.Name, b.Name)
}

// naturalLess compares names with runs of digits compared as numbers.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digitRun(a), digitRun(b)
		if da > 0 && db > 0 {
			na, nb := strings.TrimLeft(a[:da], "0"), strings.TrimLeft(b[:db], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[da:], b[db:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitRun(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// ValueTokenizer finds the app-random values in the mocks of one kind. Parsers
// register theirs for their kind from init(); a kind without one is not
// indexed, and a set that holds a mock of such a kind is not followed at all
// (see BuildMockValueIndex). Both functions run on every mock of a set when
// the set is staged, so they must be cheap.
type ValueTokenizer struct {
	// Request returns the app-random values m's request carries, each once
	// (carried), and the subset it carries at a place a match can line up
	// with the same place in a live request (bindable) — for HTTP a URL path
	// segment, a query value or a string of a JSON body. Only a bindable value
	// can ever be bound: a value the app first sent anywhere else (a
	// per-request header such as a trace id or a request signature) is
	// carried, so a copy can still swap it, but never captured.
	//
	// carried is what decides WHO carried a value first, so it must miss
	// nothing: a request that goes unseen as the first to send an id leaves a
	// later one looking like the first.
	//
	// readable is false for a request it cannot read — a body stored in an
	// encoding the recorder did not decode. Such a request may be the first to
	// carry an id without it showing, so the set is not followed (as for an
	// unreadable response).
	Request func(m *models.Mock) (carried, bindable []string, readable bool)
	// Response calls visit with every word of m's response that could be an
	// app-random value, and reports whether it could read the response. The
	// index only asks whether some request carried the word, so Response need
	// not classify it. A response it cannot read — a body stored in an
	// encoding the recorder did not decode — may be where a dependency first
	// handed the app an id, and nothing can be said about who carried what
	// first: readable is false, and the set is not followed.
	Response func(m *models.Mock, visit func(word string)) (readable bool)
	// Lookup reports whether m looked for something and found nothing (for
	// HTTP, a safe request answered 404). Its request is no first
	// carrier, and neither is its answer for what the request carries (an
	// echo of the key it asked for): those ids did not exist yet, and the
	// first request that writes one is its creator — a HEAD that finds no
	// object under a fresh key, then the PUT that makes it. Any other word of
	// its answer the dependency handed over, and that answer carries it
	// first. nil: no mock is one.
	Lookup func(m *models.Mock) bool
}

var (
	tokenizersMu sync.RWMutex
	tokenizers   = map[models.Kind]ValueTokenizer{}
)

// RegisterValueTokenizer installs the tokenizer for kind, replacing any.
func RegisterValueTokenizer(kind models.Kind, tk ValueTokenizer) {
	tokenizersMu.Lock()
	tokenizers[kind] = tk
	tokenizersMu.Unlock()
}

// valueTokenizerFor returns the tokenizer registered for kind.
func valueTokenizerFor(kind models.Kind) (ValueTokenizer, bool) {
	tokenizersMu.RLock()
	defer tokenizersMu.RUnlock()
	tk, ok := tokenizers[kind]
	return tk, ok && tk.Request != nil
}

// BindingTable is a concurrency-safe store of recorded→live value bindings
// and of the creators a replay has claimed. The zero value is ready to use.
//
// A binding is made once and kept until Reset: a recorded value has one live
// value, and a live value stands for one recorded value. Whatever would give
// either a second partner — the creator of an id answering a second request
// with another id (a retried create, more creates than were recorded) — is
// refused, and that request is answered as it would be with no table at all.
// One id for one entity is what lets an answer be rewritten without choosing
// between ids, and what the replay's own comparison of the app's answers
// relies on.
type BindingTable struct {
	mu       sync.RWMutex
	live     map[string]string   // recorded -> live
	recorded map[string]string   // live -> recorded
	claimed  map[string]struct{} // creators that answered the request they were recorded for
	// sent are the recorded values the app sent exactly as recorded while
	// they were unbound. Such a value is not made anew by this run — a
	// fixture's id, a tenant, an id the client supplied — and is never bound
	// (NeverBound): bound to the first other value that turns up in its
	// place, it would hand one entity's data out under another id.
	sent map[string]struct{}
	// disowned are bound recorded values whose binding turned out to be a
	// mistake: after it was made, the app sent the call that introduced the
	// value again exactly as recorded, so the value is a constant, and what
	// was bound to it was a question about another entity. The binding still
	// answers a request that names its live value, so that request sees what
	// it saw before, but it no longer speaks for answers to requests that
	// name neither (Default).
	disowned map[string]struct{}
	noted    map[string]struct{}
}

// Live returns the live value bound to recorded.
func (t *BindingTable) Live(recorded string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.live[recorded]
	return v, ok
}

// Recorded returns the recorded value live stands for.
func (t *BindingTable) Recorded(live string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.recorded[live]
	return v, ok
}

// ClaimAndBind stores, in one step, what answering a request from a creator
// establishes: that the creator is claimed, and every pair (recorded → live)
// the request made — or nothing at all, and reports which. creator is "" for
// a mock that is no creator.
//
// A creator answers the one request it was recorded for. Once claimed its ids
// are settled — bound to what that request made, or left as recorded because
// that request sent them so — and a later request can only repeat pairs the
// table already holds; one that brings another id for them is refused. So are
// two requests racing for one creator: the first is the one it was recorded
// for.
//
// The pairs are one request's, and a request is bound whole or not at all. It
// is refused when a pair is empty or binds a value to itself, when its
// recorded value already has another live value or may no longer be bound
// (NeverBound), when its live value already stands for another recorded one,
// and when a value would be recorded in one binding and live in another.
func (t *BindingTable) ClaimAndBind(creator string, pairs map[string]string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, spent := t.claimed[creator]; spent && creator != "" {
		for rec, live := range pairs {
			if t.live[rec] != live {
				return false
			}
		}
		return true
	}
	for rec, live := range pairs {
		if rec == "" || live == "" || rec == live {
			return false
		}
		if cur, ok := t.live[rec]; ok && cur != live {
			return false
		}
		if _, sent := t.sent[rec]; sent {
			return false
		}
		if cur, ok := t.recorded[live]; ok && cur != rec {
			return false
		}
		_, recIsLive := t.recorded[rec]
		_, liveIsRec := t.live[live]
		if recIsLive || liveIsRec {
			return false
		}
		// Within pairs itself: a map holds one live value per recorded one,
		// but two recorded values may name the same live one.
		for other, l := range pairs {
			if other != rec && (l == live || l == rec) {
				return false
			}
		}
	}
	if creator != "" {
		if t.claimed == nil {
			t.claimed = make(map[string]struct{})
		}
		t.claimed[creator] = struct{}{}
	}
	if len(pairs) > 0 && t.live == nil {
		t.live = make(map[string]string)
		t.recorded = make(map[string]string)
	}
	for rec, live := range pairs {
		t.live[rec], t.recorded[live] = live, rec
	}
	return true
}

// SentAsRecorded notes that the app sent recorded exactly as it was recorded.
// It matters only for a value nothing is bound to yet; see sent.
func (t *BindingTable) SentAsRecorded(recorded string) {
	t.mu.RLock()
	_, known := t.sent[recorded]
	_, bound := t.live[recorded]
	t.mu.RUnlock()
	if known || bound {
		return
	}
	t.mu.Lock()
	if _, bound := t.live[recorded]; !bound {
		if t.sent == nil {
			t.sent = make(map[string]struct{})
		}
		t.sent[recorded] = struct{}{}
	}
	t.mu.Unlock()
}

// NeverBound reports whether the app sent recorded as recorded while it was
// unbound: this run does not make it anew, and nothing may be bound to it.
func (t *BindingTable) NeverBound(recorded string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, sent := t.sent[recorded]
	return sent
}

// Disown marks the binding of recorded as a mistake (see disowned), and
// reports whether there was one that was not already marked.
func (t *BindingTable) Disown(recorded string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, bound := t.live[recorded]; !bound {
		return false
	}
	if _, done := t.disowned[recorded]; done {
		return false
	}
	if t.disowned == nil {
		t.disowned = make(map[string]struct{})
	}
	t.disowned[recorded] = struct{}{}
	return true
}

// Default returns the live value bound to recorded for an answer whose
// request does not name the value; none for a binding that was disowned.
func (t *BindingTable) Default(recorded string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, off := t.disowned[recorded]; off {
		return "", false
	}
	v, ok := t.live[recorded]
	return v, ok
}

// FirstNote reports whether key is noted for the first time since Reset.
func (t *BindingTable) FirstNote(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, seen := t.noted[key]; seen {
		return false
	}
	if t.noted == nil {
		t.noted = make(map[string]struct{})
	}
	t.noted[key] = struct{}{}
	return true
}

// Claimed reports whether the creator named name is claimed.
func (t *BindingTable) Claimed(name string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.claimed[name]
	return ok
}

// Len is the number of bindings.
func (t *BindingTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.live)
}

// Reset drops every binding, claim and note.
func (t *BindingTable) Reset() {
	t.mu.Lock()
	t.live, t.recorded, t.claimed, t.sent, t.disowned, t.noted = nil, nil, nil, nil, nil, nil
	t.mu.Unlock()
}
