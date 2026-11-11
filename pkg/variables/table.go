package variables

import (
	"maps"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Binding is the value a variable has in a scope, and where it was taken:
// Name, the live Value, the Recorded value at the binding place, and the Mock
// and Place where it was taken.
type Binding = models.VariableBinding

// Table holds the variables one scope has bound: one test case in keploy
// test, one test in keploy mock replay. A variable is bound once; a later binding of
// the same name does not take (Bind). Safe for concurrent use.
//
// A scope ends only when the next begins (Start, Reset): between two tests
// the ended test's bindings stay in force, and a call of the gap reads and
// binds in them (the proxy's gap rule).
//
// With KeepRun, a value bound before the set's first scope began (the run's
// start-up, before its first test) is the run's: every scope of the set sees
// it, until NewSet. So is a run's variable (models.RunVariable) bound in a
// scope: an earlier test's call binds it for the tests after it. Every other
// value bound once a scope began, in it or in the gap after it, is that
// scope's, forgotten when the next begins. A scope begins in the step that
// makes its mocks live (StartOpening), so no call its mocks answer binds in
// the start-up's. A table whose
// scope is the next test's, begun early, reads the run's (ReadRunOf); the
// run's are bound beside its scope's (Beside), as its are beside the run's,
// so the first test, which carries both in, holds no two variables recorded
// with two values at one live value, whichever bound first.
type Table struct {
	mu   sync.Mutex
	vals map[string]Binding
	// run are the run's bindings (KeepRun).
	run map[string]Binding
	// runOf is the table whose run's bindings t reads as its run's too
	// (ReadRunOf), and readers, under t.mu, the tables that read t's. A
	// table is locked before every table that reads its run's, never after
	// (lock, readersScopes): no two tables wait on each other.
	runOf   atomic.Pointer[Table]
	readers []*Table
	// began is whether a scope has begun since the table was made or NewSet.
	began bool
	// keepRun: what is bound before the first scope began is the run's
	// (KeepRun).
	keepRun bool
	// noRun: KeepRun keeps none until AllowRun (KeepNoRun).
	noRun bool
	// gen counts the scopes the table has begun (Reset): a call checked in
	// one scope never binds in the next (BindAll).
	gen uint64
	// notes say, by name, why a variable of the scope is not bound (Note):
	// the scope's alone, forgotten when the next begins.
	notes map[string]string
	// ended is what the set's ended scopes of a named test bound themselves
	// (EndedOf): each such scope's own bindings with its test, appended as
	// the next scope begins, until NewSet. Only a table that keeps them
	// (KeepEnded) does.
	ended []endedScope
	// keepEnded: the table keeps its ended scopes' bindings (KeepEnded).
	keepEnded bool
	// test is the test whose scope is running (StartOpeningTest), "" for a
	// scope begun with no test named, or before the first.
	test string
	// scope is the number of the scope in force (InScope): NewSet and each
	// scope begun for a named test take the next, so a test run twice in a
	// set has two. A scope begun with no test named (Start, Reset) keeps the
	// number in force: none of its calls is read as a named test's, and a
	// call made in it reads the running scope (OwnIn), as when keploy
	// decides it at once.
	scope uint64
	// ofEnded says t is an ended scope's own table (In), whose run's are
	// those of the table it ended in (runOf): a run's variable a late call
	// of the scope binds is bound there, as at once (put).
	ofEnded bool
}

// endedScope is an ended scope's own table (In: what it bound itself, Own
// as it ended, and what a call made in it that keploy decided later bound
// in it since), the test it was the scope of, and its number (InScope).
type endedScope struct {
	test  string
	scope uint64
	t     *Table
}

// own is the bindings e's scope made itself, by name.
func (e endedScope) own() []Binding {
	e.t.mu.Lock()
	defer e.t.mu.Unlock()
	return sorted(e.t.vals)
}

// lock takes t.mu, and before it the lock of the table whose run's t reads
// (ReadRunOf), which it returns (nil for none): t's state and that table's
// run's are then read directly, until unlock.
func (t *Table) lock() *Table {
	for {
		from := t.runOf.Load()
		if from != nil {
			from.mu.Lock()
		}
		t.mu.Lock()
		// ReadRunOf changes runOf under t.mu: once t.mu is held it holds.
		if t.runOf.Load() == from {
			return from
		}
		t.mu.Unlock()
		if from != nil {
			from.mu.Unlock()
		}
	}
}

// unlock releases what lock took.
func (t *Table) unlock(from *Table) {
	t.mu.Unlock()
	if from != nil {
		from.mu.Unlock()
	}
}

// lookup is the binding of name in force: the scope's, else the run's. t is
// locked (lock), with from, the table whose run's it reads.
func (t *Table) lookup(from *Table, name string) (Binding, bool) {
	if b, ok := t.vals[name]; ok {
		return b, true
	}
	if b, ok := t.run[name]; ok {
		return b, true
	}
	if from != nil {
		b, ok := from.run[name]
		return b, ok
	}
	return Binding{}, false
}

// runInForce is a copy of every run's binding t reads, its own and those of
// from, the table it reads the run's of (ReadRunOf); nil for none. t is
// locked (lock).
func (t *Table) runInForce(from *Table) map[string]Binding {
	var theirs map[string]Binding
	if from != nil {
		theirs = from.run
	}
	if len(t.run)+len(theirs) == 0 {
		return nil
	}
	out := make(map[string]Binding, len(t.run)+len(theirs))
	for n, b := range theirs {
		out[n] = b
	}
	for n, b := range t.run {
		out[n] = b
	}
	return out
}

// all is every binding in force, the scope's over the run's. t is locked
// (lock).
func (t *Table) all(from *Table) map[string]Binding {
	run := t.runInForce(from)
	if len(run) == 0 {
		return t.vals
	}
	out := make(map[string]Binding, len(t.vals)+len(run))
	for n, b := range run {
		out[n] = b
	}
	for n, b := range t.vals {
		out[n] = b
	}
	return out
}

// readersScopes is, while t binds the run's (put), every binding of the
// scopes of the tables that read t's run's (ReadRunOf): the next test's calls
// in flight bound them before the set's first test began, and that test
// carries them in beside the run's (StartCarrying). nil once a scope began:
// what t binds then is its scope's, and two tests' variables may share a
// value. t is locked (lock); each reader is locked after it.
func (t *Table) readersScopes() []Binding {
	if t.began || !t.keepRun {
		return nil
	}
	var out []Binding
	for _, r := range t.readers {
		r.mu.Lock()
		for _, b := range r.vals {
			out = append(out, b)
		}
		r.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Gen is the scope the table is in now: what a call that is about to bind
// was checked against.
func (t *Table) Gen() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// Lookup returns the binding of name in force, if any.
func (t *Table) Lookup(name string) (Binding, bool) {
	from := t.lock()
	defer t.unlock(from)
	return t.lookup(from, name)
}

// Beside returns every binding a bind of t is checked beside now (BindAll's
// TwoValues rule), by name: those in force (Snapshot), then, while t binds
// the run's (KeepRun, before the first scope began), those of the scopes
// that read the run's (ReadRunOf), which the first test carries in beside
// them. An adapter that checks a call before it binds reads these (http's
// holdVariables), so the check and the bind refuse alike.
func (t *Table) Beside() []Binding {
	from := t.lock()
	defer t.unlock(from)
	return append(sorted(t.all(from)), t.readersScopes()...)
}

// Bind records b unless its name is already bound, or b's value is another
// variable's that was recorded with another value (TwoValues, BindAll's rule,
// the run's and Beside's included). It returns the binding in force afterwards
// of b's name, or the one b's value is, and whether it is b. A call binds with
// BindAll, a start-up's call too (its Gen is the table's before any scope
// begins), which also refuses a call of a scope that ended since: Bind checks
// no scope.
func (t *Table) Bind(b Binding) (Binding, bool) {
	from := t.lock()
	defer t.unlock(from)
	if have, ok := t.lookup(from, b.Name); ok {
		return have, false
	}
	if with, ok := collides(t.all(from), t.readersScopes(), b, nil); ok {
		return with, false
	}
	t.put(from, b)
	return b, true
}

// KeepRun makes what is bound before the first scope begins the run's, which
// every scope of the set sees, instead of the first scope's to forget, unless
// KeepNoRun refused it since the last AllowRun. It is called before the
// start-up binds anything (MockManager.NewVariableSet, as a test set's mocks
// are set up), and holds until KeepNoRun.
func (t *Table) KeepRun() {
	t.mu.Lock()
	t.keepRun = !t.noRun
	t.mu.Unlock()
}

// KeepNoRun undoes KeepRun, and refuses it until AllowRun: what is bound
// before the first scope begins is the first scope's to forget, as without
// KeepRun, and what the run's held so far goes with it
// (MockManager.SetMocksWithWindow: a start-up that is served every test's
// recordings binds nothing the run's can be told from a test's).
func (t *Table) KeepNoRun() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keepRun, t.noRun = false, true
	if len(t.run) == 0 {
		return
	}
	if t.vals == nil {
		t.vals = make(map[string]Binding, len(t.run))
	}
	for n, b := range t.run {
		if _, ok := t.vals[n]; !ok {
			t.vals[n] = b
		}
	}
	t.run = nil
}

// AllowRun lets a later KeepRun keep the run's again (a staging that serves
// the start-up exactly). It keeps none itself: a scope a call was checked in
// before it may have bound what is no run's.
func (t *Table) AllowRun() {
	t.mu.Lock()
	t.noRun = false
	t.mu.Unlock()
}

// ReadRunOf makes t read the run's bindings of from as its run's too, in
// every scope: a table whose scope is a test's begun early (the next test's
// calls in flight, answered in the gap before it) sees what the run's
// start-up bound, as the test does once it begins. t keeps no run's of its
// own from it: what t binds is its scope's. from never reads t's, but checks
// a bind of its run's beside t's scope's (Beside): whichever of the two binds
// a value first, the other is refused it for another variable.
func (t *Table) ReadRunOf(from *Table) {
	if from == t || from == nil {
		return
	}
	from.mu.Lock()
	t.mu.Lock()
	old := t.runOf.Load()
	if old == from {
		t.mu.Unlock()
		from.mu.Unlock()
		return
	}
	t.runOf.Store(from)
	t.mu.Unlock()
	from.readers = append(from.readers, t)
	from.mu.Unlock()
	if old != nil {
		old.mu.Lock()
		for i, r := range old.readers {
			if r == t {
				old.readers = append(old.readers[:i:i], old.readers[i+1:]...)
				break
			}
		}
		old.mu.Unlock()
	}
}

// put binds b in the scope, or the run's where the table keeps the run's
// (KeepRun) and b is one: bound before any scope began (the start-up's), or
// a run's variable (models.RunVariable) an earlier test's call binds for the
// tests after it. An ended scope's table (In) binds a run's variable in the
// run's of from, the table it ended in, where that table keeps the run's: a
// call keploy decides after its scope ended binds it for the tests after,
// as at once (an ended scope's table always reads that table's run's). t is
// locked (lock), with from, the table whose run's it reads.
func (t *Table) put(from *Table, b Binding) {
	if t.keepRun && (!t.began || models.RunVariable(b.Name)) {
		if t.run == nil {
			t.run = make(map[string]Binding)
		}
		t.run[b.Name] = b
		return
	}
	if t.ofEnded && from.keepRun && models.RunVariable(b.Name) {
		if from.run == nil {
			from.run = make(map[string]Binding)
		}
		from.run[b.Name] = b
		return
	}
	if t.vals == nil {
		t.vals = make(map[string]Binding)
	}
	t.vals[b.Name] = b
}

// BindAll binds bs together, all or none, under one lock, for a call checked
// in scope gen (Gen): each name holds one value (a name already bound, the
// run's included, must be bound to the same value), and two variables
// recorded with two values do not share a live value, unless both are clock
// reads the learn step marked, made in one second (TwoValues). It returns the
// first binding that does not hold and the binding it collides with; stale is
// true when a new scope began since gen (nothing is bound); ok is true when
// every binding of bs is in force afterwards.
func (t *Table) BindAll(gen uint64, bs []Binding) (bad, with Binding, stale, ok bool) {
	from := t.lock()
	defer t.unlock(from)
	if gen != t.gen {
		return Binding{}, Binding{}, true, false
	}
	inForce, beside := t.all(from), t.readersScopes()
	for i, b := range bs {
		if have, bound := inForce[b.Name]; bound && have.Value != b.Value {
			return b, have, false, false
		}
		if with, bad := collides(inForce, beside, b, bs[:i]); bad {
			return b, with, false, false
		}
	}
	for _, b := range bs {
		if _, bound := t.lookup(from, b.Name); !bound {
			t.put(from, b)
		}
	}
	return Binding{}, Binding{}, false, true
}

// collides is the binding b cannot be bound beside, of those in force, beside
// (the scopes that read the run's while the run's binds, readersScopes) and
// earlier (bound with b, before it): one of b's name with another value
// (earlier: in force, a name already bound keeps its own), or one whose
// value b's is (TwoValues).
func collides(inForce map[string]Binding, beside []Binding, b Binding, earlier []Binding) (Binding, bool) {
	for _, other := range inForce {
		if TwoValues(other, b) {
			return other, true
		}
	}
	for _, other := range beside {
		if TwoValues(other, b) {
			return other, true
		}
	}
	for _, other := range earlier {
		if other.Name == b.Name && other.Value != b.Value || TwoValues(other, b) {
			return other, true
		}
	}
	return Binding{}, false
}

// TwoValues reports whether b may not be bound beside held, a binding in
// force or bound with it, because they are two variables that were two values
// when the test was recorded and now share one live value: held is not a
// given value, and is not b's name. The one pair of such variables that may
// share a live value is two clock reads the learn step marked, made in one
// second as their recordings say (models.ReadInOneSecond, FD-3): nothing
// about the live values decides it. BindAll and Bind refuse such a b, and an
// adapter that checks a call before it binds asks the same (http's
// holdVariables).
//
// Under the proven switch (KEPLOY_PROVEN), two bindings at words of values
// (each with its WordRun, the value span its word is in: WordRuns) are set
// side by side at the word they share, each span at its own word
// (WordRunAt), and each pair of words they both hold there is read live and
// as recorded (WordRunRecorded). A span holds only words the recording names
// as one learned value: a neighbour that is, or may be, another value says
// nothing of the shared word, so "<P>/7" and "<Q>/7", where "<B>/3" and
// "<D>/5" were recorded, leave "7" its own value, refused as main refuses
// it, whether the mocks hold "<B>" and "<D>" whole, the whole texts, or
// nothing. A pair equal live that was two words when recorded is a copy:
// the application sent more than the one word where the recording had two
// values, whole ("<X>" where "<A>" and "<C>" were recorded) or beside
// another ("<X>" inside "<X>/<Y>", or "<P>/<X>" and "<Q>/<X>" where
// "<B>/<A>" and "<D>/<C>" were), and it is refused. A pair recorded as one
// word is one value at both places: no evidence either way. The shared word
// is equal by chance (a group of two fresh UUIDs, or two groups of one), and
// neither binding refused, only where no pair is a copy and at least one
// pair differs both live and as recorded; with no such pair it is refused.
// So a word binding is never bound where main's word by word check refuses
// it, but inside a span. A word binding beside a binding of a value whole is
// checked as two values whole, and with the switch off every pair is (main's
// word by word check).
func TwoValues(held, b Binding) bool {
	if held.Given || held.Name == b.Name || held.Value != b.Value || held.Recorded == b.Recorded || models.ReadInOneSecond(held.Clock, b.Clock) {
		return false
	}
	if held.WordRun != "" && b.WordRun != "" && provenswitch.On() {
		return !chanceInRuns(held, b)
	}
	return true
}

// chanceInRuns reports whether, set side by side at the word they share
// (held's WordRunAt beside b's), the two runs show the shared word equal by
// chance: no pair of words they both hold there is equal live where its
// recorded words differ, and at least one pair differs both live and as
// recorded. Runs whose recorded words are not as many as their live words
// show nothing.
func chanceInRuns(held, b Binding) bool {
	hw, bw := strings.Split(held.WordRun, wordRunSep), strings.Split(b.WordRun, wordRunSep)
	hr, br := strings.Split(held.WordRunRecorded, wordRunSep), strings.Split(b.WordRunRecorded, wordRunSep)
	if len(hr) != len(hw) || len(br) != len(bw) {
		return false
	}
	shift := b.WordRunAt - held.WordRunAt
	differ := false
	for i := range hw {
		j := i + shift
		if i == held.WordRunAt || j < 0 || j >= len(bw) || hr[i] == br[j] {
			continue
		}
		if hw[i] == bw[j] {
			return false
		}
		differ = true
	}
	return differ
}

// wordRunSep joins the words of a run: no word holds it (Words).
const wordRunSep = "-"

// WordBind is a binding one call makes at a word of a value: Text names the
// text that holds the word (its place and decode steps, as the adapter writes
// a place: one name for one text), Word is the word's index in it (Words; -1
// for a value whole), Live is the word, live, and Recorded the word its
// recording holds there. RecordedText is that text as recorded (decoded by
// its steps), and Elsewhere reports whether the binding's mock holds a
// string, whole, at a place other than Text (a field, a segment, a header, a
// value of its answer): the evidence that cuts a run into held stretches
// (WordRuns). Of names the learned value Live is a word of: a held stretch
// is one value only when each of its words names the same one, and a
// stretch that is one UUID by its form (WordRuns) is one value only when
// each of its words names that UUID. A WordBind with none of them has no
// span but its own word. The call's text there is RecordedText with its
// live words in place of the words it binds: the adapter matched every
// other byte as recorded.
type WordBind struct {
	Text         string
	Word         int
	Live         string
	Recorded     string
	RecordedText string
	Elsewhere    func(string) bool
	Of           string
}

// Run is one binding's value span (Binding.WordRun): its words, live and as
// recorded, each joined, and At, the index of the binding's own word among
// them. A binding of a value whole has the zero Run.
type Run struct {
	Live     string
	Recorded string
	At       int
}

// On is b with r as its run.
func (r Run) On(b Binding) Binding {
	b.WordRun, b.WordRunRecorded, b.WordRunAt = r.Live, r.Recorded, r.At
	return b
}

// maxSpanRun bounds the run spansOf reads (it tries each stretch of it): a
// longer run gives each of its words no span but its own (main's word by
// word check).
const maxSpanRun = 64

// WordRuns is, for each binding one call makes (ws[i]), its value span: the
// words of its run (the words the call binds at consecutive words of the same
// text, its own among them, in word order; words the call binds apart, with
// a word between them it does not bind, are two runs) that hold the one
// value its word is a word of. That is the shortest stretch of the run
// around its word whose recorded text its mock holds whole elsewhere (a cid
// or an appId sent whole in another field) and that holds no shorter such
// stretch (a stretch that holds one, "<B>/3" sent whole beside "<B>", is two
// values), when each of its words names the same learned value (Of). A held
// stretch is not proof of one value by itself: "<B>/3" sent whole elsewhere
// may be one id or a cid beside a count, and the recording does not say
// which. A stretch is one value by its form, held elsewhere or not, when its
// recorded text and its live text are each one canonical UUID (8-4-4-4-12
// hex digits: a minted id), the call binds every word of it, and each of
// those words names that recorded UUID (Of): its words are the groups of one
// id, never a count or a second value beside it. Every other word is its own
// value, alone, checked word by word as main checks it: a word no held or
// UUID stretch covers (never the words around it: "<cid>/<appId>/7" with
// only the cid held leaves "7" alone, not with the appId), a word of a
// stretch whose words name no one value, a word whose recorded text does not
// hold the run's recorded words where the run says, and one with neither
// Elsewhere nor a UUID's form. A binding of a value whole has none.
//
// Binds of one call at one word of one text, with one live and one recorded
// word (a variable and its run twin, bound at one place), take one position
// in the run and share its span; binds at one word that differ there cut the
// run, each its own word.
func WordRuns(ws []WordBind) []Run {
	runs := make([]Run, len(ws))
	byText := map[string][]int{}
	var texts []string
	for i, w := range ws {
		if w.Word < 0 {
			continue
		}
		if _, seen := byText[w.Text]; !seen {
			texts = append(texts, w.Text)
		}
		byText[w.Text] = append(byText[w.Text], i)
	}
	for _, k := range texts {
		idx := byText[k]
		sort.SliceStable(idx, func(x, y int) bool { return ws[idx[x]].Word < ws[idx[y]].Word })
		pos := positionsOf(ws, idx)
		for lo := 0; lo < len(pos); {
			hi := lo + 1
			for hi < len(pos) && ws[pos[hi][0]].Word == ws[pos[hi-1][0]].Word+1 {
				hi++
			}
			run := pos[lo:hi]
			for at, span := range spansOf(ws, run) {
				from, to := span[0], span[1]
				live, recorded := make([]string, 0, to-from+1), make([]string, 0, to-from+1)
				for _, p := range run[from : to+1] {
					live, recorded = append(live, ws[p[0]].Live), append(recorded, ws[p[0]].Recorded)
				}
				r := Run{Live: strings.Join(live, wordRunSep), Recorded: strings.Join(recorded, wordRunSep), At: at - from}
				for _, j := range run[at] {
					runs[j] = r
				}
			}
			lo = hi
		}
	}
	return runs
}

// positionsOf is idx (indexes of ws at one text, in word order) as the run's
// positions: the binds at one word with one live and one recorded word (a
// variable and its run twin) are one position; a bind at the same word that
// differs from the one before it there is a position of its own, which cuts
// the run (WordRuns: no word follows itself).
func positionsOf(ws []WordBind, idx []int) [][]int {
	var pos [][]int
	for _, j := range idx {
		if n := len(pos); n > 0 {
			if prev := ws[pos[n-1][0]]; prev.Word == ws[j].Word && prev.Live == ws[j].Live && prev.Recorded == ws[j].Recorded {
				pos[n-1] = append(pos[n-1], j)
				continue
			}
		}
		pos = append(pos, []int{j})
	}
	return pos
}

// spansOf is the value span (WordRuns) of each position of run (the binds of
// ws at each of consecutive words of one text, positionsOf), as the first
// and last index into run: read once for the run from its recorded text, its
// mock (Elsewhere) and its words' form, the same for every word of one text
// in one call.
func spansOf(ws []WordBind, run [][]int) [][2]int {
	n := len(run)
	spans := make([][2]int, n)
	for at := range spans {
		spans[at] = [2]int{at, at}
	}
	first := ws[run[0][0]]
	words := Words(first.RecordedText)
	if n > maxSpanRun || ws[run[n-1][0]].Word >= len(words) {
		return spans
	}
	for _, p := range run {
		for _, j := range p {
			if x := words[ws[j].Word]; ws[j].RecordedText != first.RecordedText || first.RecordedText[x[0]:x[1]] != ws[j].Recorded {
				return spans
			}
		}
	}
	// text is the stretch a..b of run as recorded, and live its live text:
	// the recorded text with the live words in place of its words.
	text := func(a, b int) string {
		return first.RecordedText[words[ws[run[a][0]].Word][0]:words[ws[run[b][0]].Word][1]]
	}
	live := func(a, b int) string {
		var s strings.Builder
		s.WriteString(ws[run[a][0]].Live)
		for p := a + 1; p <= b; p++ {
			s.WriteString(first.RecordedText[words[ws[run[p-1][0]].Word][1]:words[ws[run[p][0]].Word][0]])
			s.WriteString(ws[run[p][0]].Live)
		}
		return s.String()
	}
	// value[a*n+b]: the stretch a..b of run is held whole elsewhere, or is one
	// UUID by its form (uuidStretch), and holds no shorter such stretch
	// (inner). A held stretch that holds another ("<B>/3" beside "<B>") is two
	// values, never one.
	value, inner := make([]bool, n*n), make([]bool, n*n)
	for a := n - 1; a >= 0; a-- {
		for b := a; b < n; b++ {
			if b > a {
				inner[a*n+b] = value[(a+1)*n+b] || inner[(a+1)*n+b] || value[a*n+b-1] || inner[a*n+b-1]
			}
			value[a*n+b] = !inner[a*n+b] && (first.Elsewhere != nil && first.Elsewhere(text(a, b)) || uuidStretch(ws, run[a:b+1], text(a, b), func() string { return live(a, b) }))
		}
	}
	for at := range spans {
		from, to := -1, -1
		for a := 0; a <= at; a++ {
			for b := at; b < n; b++ {
				if value[a*n+b] && (from < 0 || b-a < to-from) {
					from, to = a, b
				}
			}
		}
		if from >= 0 && oneValue(ws, run[from:to+1]) {
			spans[at] = [2]int{from, to}
		}
	}
	return spans
}

// NameUUIDs names each word bind of ws (the binds of one call) a word of its
// recorded text (Of) where ws bind every word of that text and the text is
// one canonical UUID: a minted id bound word by word, whose words WordRuns
// then reads as one value by its form. A text bound at only some of its
// words, and a text that is no UUID, is named nothing: each of its words
// stays its own value, checked word by word. A bind already named keeps its
// name.
func NameUUIDs(ws []WordBind) {
	bound := map[string]map[int]bool{}
	for _, w := range ws {
		if bound[w.Text] == nil {
			bound[w.Text] = map[int]bool{}
		}
		bound[w.Text][w.Word] = true
	}
	for i, w := range ws {
		if w.Word < 0 || w.Of != "" || !canonicalUUID(w.RecordedText) {
			continue
		}
		every := true
		for k := range Words(w.RecordedText) {
			every = every && bound[w.Text][k]
		}
		if every {
			ws[i].Of = w.RecordedText
		}
	}
}

// uuidStretch reports whether a stretch of a run (its positions), recorded
// as text and live as live gives it, is one UUID by its form: both texts are
// each one canonical UUID and its first bind names the recorded one (Of; a
// span is taken only where every bind of it names one value, oneValue). The
// stretch covers every word of it: a UUID is five words.
func uuidStretch(ws []WordBind, stretch [][]int, text string, live func() string) bool {
	return canonicalUUID(text) && ws[stretch[0][0]].Of == text && canonicalUUID(live())
}

// canonicalUUID reports whether s is one UUID in its canonical text: 32 hex
// digits in groups of 8, 4, 4, 4 and 12, joined by "-".
func canonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		if !isHex(s[i]) {
			return false
		}
	}
	return true
}

// oneValue reports whether the word binds of stretch (the binds of ws at
// each of its positions) each name the same learned value (WordBind.Of).
func oneValue(ws []WordBind, stretch [][]int) bool {
	of := ws[stretch[0][0]].Of
	for _, p := range stretch {
		for _, j := range p {
			if ws[j].Of == "" || ws[j].Of != of {
				return false
			}
		}
	}
	return true
}

// Start begins a new scope (as Reset) with the values its test case gave
// bound, under one lock: no call of the scope binds before them. A name given
// twice keeps its first value.
func (t *Table) Start(given []Binding) {
	t.StartCarrying(given, nil)
}

// StartCarrying is Start, with the bindings carried also bound as the scope
// begins, as they were made (not given): what the answers of the test's calls
// in flight when it began bound before it, in a scope of their own. A given
// value wins over a carried one of its name, and the first of two carried
// ones wins.
//
// One lock, as Start: no call of the scope binds before them.
func (t *Table) StartCarrying(given, carried []Binding) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.start("", given, carried)
}

// StartOpening is StartCarrying in the step that opens the scope: open, what
// makes the scope's mocks live (a test's view, the pool of its window), runs
// under the table's lock, and the scope begins before the lock is let go. A
// call reads the scope it is checked in (Gen) before the mocks it is checked
// against: one that read the scope before, and the scope's mocks after, binds
// stale (BindAll), and is checked again in the scope. So no call answered by a
// mock of the scope binds before the scope began, in the scope before it or
// in the run's (KeepRun), which every later scope of the set would read.
// Opened in two steps, the mocks live and then the scope begun, a call of the
// first scope answered between the two bound in the run's.
//
// open must call none of t's methods: it holds t's lock.
func (t *Table) StartOpening(open func(), given, carried []Binding) {
	t.StartOpeningTest("", open, given, carried, nil)
}

// StartOpeningTest is StartOpening for the scope of the test named test
// ("" for none named): once the next scope begins, what this one bound
// itself is that test's (EndedOf), on a table that keeps it (KeepEnded). The
// name is the scope's from the step that begins it, so no binding of another
// scope is ever kept as test's.
//
// notes, by variable name, are noted in the scope as it begins (Note), in
// the same step: a call checked in the scope reads them from its first.
func (t *Table) StartOpeningTest(test string, open func(), given, carried []Binding, notes map[string]string) {
	t.StartOpeningScope(test, func(uint64) { open() }, given, carried, notes)
}

// StartOpeningScope is StartOpeningTest, open told the number the scope it
// begins has (InScope): what makes the scope's mocks live (a test's view)
// carries it, so a call made there reads that scope's bindings as its own
// test's (OwnIn), however late keploy decides it.
//
// open must call none of t's methods: it holds t's lock.
func (t *Table) StartOpeningScope(test string, open func(scope uint64), given, carried []Binding, notes map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	open(t.scopeOf(test))
	t.start(test, given, carried)
	t.notes = maps.Clone(notes)
}

// InScope runs f holding the table's lock, told the number of the scope in
// force (StartOpeningScope): what makes mocks live while that scope runs (a
// pool view between tests, whose calls are the ended test's at zero
// latency: its scope runs until the next test's begins) carries it, and no
// scope begins between the read and f's end.
//
// f must call none of t's methods: it holds t's lock.
func (t *Table) InScope(f func(scope uint64)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(t.scope)
}

// scopeOf is the number a scope begun now for test has: the next for a
// named test, the one in force for none named. t.mu is held.
func (t *Table) scopeOf(test string) uint64 {
	if test == "" {
		return t.scope
	}
	return t.scope + 1
}

// start begins a scope (StartCarrying), of test ("" for none named). t.mu
// is held.
func (t *Table) start(test string, given, carried []Binding) {
	t.end()
	t.scope = t.scopeOf(test)
	t.test = test
	t.gen++
	t.vals, t.notes = nil, nil
	t.began = true
	if len(given)+len(carried) == 0 {
		return
	}
	t.vals = make(map[string]Binding, len(given)+len(carried))
	for _, b := range given {
		b.Given = true
		if _, dup := t.vals[b.Name]; !dup {
			t.vals[b.Name] = b
		}
	}
	for _, b := range carried {
		if _, dup := t.vals[b.Name]; !dup {
			t.put(nil, b)
		}
	}
}

// Give binds b, a value the open scope's test gave (models.GivenReq), as a
// given value (Binding.Given) of the scope, under the table's lock, without
// beginning a new scope: what the scope bound before stays, and a call
// checked in it binds on (Gen does not move). It refuses, binding nothing,
// when no scope has begun (the value would be the run's, which every later
// scope reads) and when b's name is already bound, and returns the binding
// in force of b's name then. A given's name is the test's own
// (models.GivenPrefix): no learned variable takes it.
func (t *Table) Give(b Binding) (Binding, bool) {
	from := t.lock()
	defer t.unlock(from)
	if !t.began {
		return Binding{}, false
	}
	if have, ok := t.lookup(from, b.Name); ok {
		return have, false
	}
	b.Given = true
	if t.vals == nil {
		t.vals = make(map[string]Binding)
	}
	t.vals[b.Name] = b
	return b, true
}

// Note says why, in the open scope, the variable name is not bound: where
// the test binds it in a place no call is (a client test case's request,
// models.ClientCaseBind), why that place bound nothing, so a call that uses
// it misses saying so (Unbound) rather than naming a call that binds it.
// It refuses, noting nothing, when no scope has begun. A later Note of the
// name replaces it; the next scope forgets it.
func (t *Table) Note(name, why string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.began {
		return false
	}
	if t.notes == nil {
		t.notes = make(map[string]string)
	}
	t.notes[name] = why
	return true
}

// Unbound is the scope's note on name (Note) while name is not bound: ""
// where it is bound, or the scope noted nothing.
func (t *Table) Unbound(name string) string {
	from := t.lock()
	defer t.unlock(from)
	if _, ok := t.lookup(from, name); ok {
		return ""
	}
	return t.notes[name]
}

// Reset forgets every binding of the scope: a new scope begins. The run's
// stay.
func (t *Table) Reset() {
	t.mu.Lock()
	t.end()
	t.test = ""
	t.vals, t.notes = nil, nil
	t.began = true
	t.gen++
	t.mu.Unlock()
}

// NewSet forgets every binding, the run's too, and no scope has begun: the
// next test set's replay starts (as learn.Tracker.NewSet).
func (t *Table) NewSet() {
	t.mu.Lock()
	t.newSet()
	t.mu.Unlock()
}

// newSet is NewSet's step. t.mu is held.
func (t *Table) newSet() {
	t.vals, t.run, t.ended, t.notes = nil, nil, nil, nil
	t.began, t.test = false, ""
	t.gen++
	t.scope++
}

// SetBound is what a table had bound as its set ended (NewSetKeeping), as
// a background miss reads it: the running scope's own bindings, as Own gave
// them then, and what the set's ended scopes of each named test bound, as
// EndedOf reads them (their tables, In, which a call of the set that keploy
// decides later binds in still). The running scope's change no more. A nil
// SetBound holds none.
type SetBound struct {
	own   []Binding
	ended []endedScope
	// test is the test the running scope was begun for then ("" for none
	// named, or before the first scope began), scope its number (InScope).
	test  string
	scope uint64
}

// NewSetKeeping is NewSet, returning what the ending set had bound
// (SetBound), read in the same step: no binding is made after it is read
// and before NewSet forgets it. A call of the ending set that keploy
// decides once the next set began reads its set's bindings from it.
// publish, unless nil, is given it in that step too, before NewSet forgets
// it, under the table's lock: a reader that reads the table in force
// (Bound) and then finds nothing new published read the set before NewSet
// forgot it, never the next set's with the ending set's not yet published.
func (t *Table) NewSetKeeping(publish func(*SetBound)) *SetBound {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.bound()
	if publish != nil {
		publish(kept)
	}
	t.newSet()
	return kept
}

// Bound is what the table has bound now, as NewSetKeeping would keep it
// (SetBound), without forgetting it: a call keploy decides reads the
// bindings of its set in force from it, all in one step.
func (t *Table) Bound() *SetBound {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bound()
}

// bound is Bound's step. t.mu is held.
func (t *Table) bound() *SetBound {
	kept := &SetBound{ended: t.ended[:len(t.ended):len(t.ended)], scope: t.scope}
	if t.began {
		kept.own, kept.test = sorted(t.vals), t.test
	}
	return kept
}

// Own is the bindings the set's running scope had made itself when it
// ended (Table.Own then), by name.
func (s *SetBound) Own() []Binding {
	if s == nil {
		return nil
	}
	return append([]Binding(nil), s.own...)
}

// OwnOf is what a call marked in test's view read as its own test's
// bindings when the set ended (Table.OwnOf then).
func (s *SetBound) OwnOf(test string) []Binding {
	if s == nil {
		return nil
	}
	if !ownScope(s.test, test) {
		return endedOf(s.ended, test)
	}
	return append([]Binding(nil), s.own...)
}

// OwnIn is what a call made while scope was in force read as its own
// test's bindings when the set ended (Table.OwnIn then).
func (s *SetBound) OwnIn(scope uint64) []Binding {
	if s == nil {
		return nil
	}
	if scope != s.scope {
		return endedIn(s.ended, scope)
	}
	return append([]Binding(nil), s.own...)
}

// EndedOf is what the set's ended scopes of test had bound themselves when
// it ended (Table.EndedOf then): none for test "".
func (s *SetBound) EndedOf(test string) []Binding {
	if s == nil {
		return nil
	}
	return endedOf(s.ended, test)
}

// endedOf is the bindings of the scopes of ended begun for test, scope by
// scope in the order they ended: none for test "" (a scope begun with no
// test named is kept as no test's).
func endedOf(ended []endedScope, test string) []Binding {
	if test == "" {
		return nil
	}
	var out []Binding
	for _, e := range ended {
		if e.test == test {
			out = append(out, e.own()...)
		}
	}
	return out
}

// endedIn is the bindings of the scope of ended numbered scope (InScope),
// none where ended keeps none of that number.
func endedIn(ended []endedScope, scope uint64) []Binding {
	for _, e := range ended {
		if e.scope == scope {
			return e.own()
		}
	}
	return nil
}

// Clone is a table holding t's bindings in t's scope, the run's it reads
// included, sharing nothing with it: a match checked against the clone binds
// nothing in t.
func (t *Table) Clone() *Table {
	from := t.lock()
	defer t.unlock(from)
	c := &Table{gen: t.gen, began: t.began, keepRun: t.keepRun, notes: maps.Clone(t.notes)}
	if t.vals != nil {
		c.vals = make(map[string]Binding, len(t.vals))
		for k, v := range t.vals {
			c.vals[k] = v
		}
	}
	c.run = t.runInForce(from)
	return c
}

// Own is the bindings the running scope made itself, sorted by name: each
// value a call of the running test (or of the gap after it) bound, never one
// of the run's (KeepRun), nor one of the table whose run's it reads
// (ReadRunOf). Before the first scope began it is none: no test is running.
// A background miss reads it: only a value the call's own test bound before
// it may stand where its background recording holds the recorded value.
func (t *Table) Own() []Binding {
	from := t.lock()
	defer t.unlock(from)
	if !t.began {
		return nil
	}
	return sorted(t.vals)
}

// KeepEnded makes the table keep what each ended scope of a named test
// bound itself (EndedOf), from its next scope on, until NewSet; NewSet
// does not undo it. Only the table a background miss reads keeps them (the
// mock manager's test variables): any other table, such as the one the
// next test's calls in flight bind in, keeps none.
func (t *Table) KeepEnded() {
	t.mu.Lock()
	t.keepEnded = true
	t.mu.Unlock()
}

// EndedOf is what the set's ended scopes of test bound themselves, scope
// by scope in the order they ended, each scope's by name: every binding
// Own gave in a scope begun for test (StartOpeningTest) until the next
// began, never the run's (KeepRun), nor another test's scope's, nor one of
// a scope begun with no test named, which is kept as no test's: none for
// test "", and none on a table that does not keep them (KeepEnded). A
// test's scope ends only when the next begins, so the running scope's are
// Own's, not these; NewSet forgets them. A background miss reads them: a
// call the application makes for a test that has ended (a clean-up it runs
// once it answered the test) carries a value that test bound, which only
// these still hold, and only test's scope can have bound it.
func (t *Table) EndedOf(test string) []Binding {
	t.mu.Lock()
	defer t.mu.Unlock()
	return endedOf(t.ended, test)
}

// OwnOf is what a call marked in test's view reads as its own test's
// bindings (Own), read in one step. keploy can decide a test's call after
// the replay began the set's next test, whose scope's are not the call's:
// once a scope begun for another named test is running, they are what
// test's ended scopes bound (EndedOf), none where it bound none. The
// running scope's own (Own) while it is test's, as when keploy decides the
// call at once, and while it names no test (one begun with no test named,
// or none begun), or for a mark that names none (test ""), which no scope
// tells apart.
func (t *Table) OwnOf(test string) []Binding {
	from := t.lock()
	defer t.unlock(from)
	if !ownScope(t.test, test) {
		return endedOf(t.ended, test)
	}
	if !t.began {
		return nil
	}
	return sorted(t.vals)
}

// OwnIn is what a call made while scope was in force (InScope,
// StartOpeningScope) reads as its own test's bindings (Own), read in one
// step: the running scope's while it is scope, as when keploy decides the
// call at once; once a later scope began (keploy decided the call after the
// replay began the set's next test, or the same test's next run), what
// scope bound itself as it ended (kept for a named test's scope, on a table
// that keeps them: KeepEnded), never another scope's, nor the union of a
// test's runs. None for a scope ended unkept, and for NewSet's before any
// scope began (no test is running).
func (t *Table) OwnIn(scope uint64) []Binding {
	from := t.lock()
	defer t.unlock(from)
	if scope != t.scope {
		return endedIn(t.ended, scope)
	}
	if !t.began {
		return nil
	}
	return sorted(t.vals)
}

// ownScope reports whether a call marked in test's view reads the running
// scope, begun for running ("" for none named), as its own test's (OwnOf).
func ownScope(running, test string) bool {
	return test == "" || running == "" || running == test
}

// end keeps the running scope's own bindings as its test's ended scope's
// (EndedOf), as the next scope begins, on a table that keeps them
// (KeepEnded): nothing of a scope begun with no test named, nor before the
// first scope began (no test is named then: NewSet forgets the name), which
// no test bound. t.mu is held.
func (t *Table) end() {
	if t.keepEnded && t.test != "" {
		t.ended = append(t.ended, endedScope{test: t.test, scope: t.scope, t: t.endedTable(t.test, t.scope, maps.Clone(t.vals), maps.Clone(t.notes))})
	}
}

// In is the table a call made while scope was in force (InScope,
// StartOpeningScope) reads and binds in, however late keploy decides it: t
// while scope runs, as when keploy decides the call at once; once a later
// scope began (keploy decided the call after the replay began the set's
// next test, or the same test's next run), scope's own table as it ended
// (kept for a named test's scope, on a table that keeps them: KeepEnded),
// never the scope in force. That table holds what scope bound itself
// (OwnIn), reads t's run's in force, and binds what the call binds in
// scope, where EndedOf and OwnIn read it after, and a run's variable in t's
// run's (put), as at once. For a scope ended unkept, or one t's set never
// began, it is a table holding no scope's bindings, whose binds no scope
// keeps: nothing of another scope is read as the call's.
func (t *Table) In(scope uint64) *Table {
	t.mu.Lock()
	defer t.mu.Unlock()
	if scope == t.scope {
		return t
	}
	return t.scopeTable(scope)
}

// scopeTable is the ended table of scope (In): the one t kept as scope ended,
// else one holding nothing. t.mu is held.
func (t *Table) scopeTable(scope uint64) *Table {
	for _, e := range t.ended {
		if e.scope == scope {
			return e.t
		}
	}
	return t.endedTable("", scope, nil, nil)
}

// endedTable is an ended scope's own table (In), of test's scope numbered
// scope, holding vals and notes: begun, in a scope that never moves (Gen),
// reading t's run's (ReadRunOf, without t checking a bind of its run's
// beside it: t binds its run's beside no scope once one began). t.mu is
// held.
func (t *Table) endedTable(test string, scope uint64, vals map[string]Binding, notes map[string]string) *Table {
	e := &Table{vals: vals, notes: notes, test: test, scope: scope, gen: t.gen, began: true, ofEnded: true}
	e.runOf.Store(t)
	return e
}

// Snapshot returns the bindings in force, the run's included, by name.
func (t *Table) Snapshot() []Binding {
	from := t.lock()
	defer t.unlock(from)
	return sorted(t.all(from))
}

// sorted is the bindings of in, by name.
func sorted(in map[string]Binding) []Binding {
	out := make([]Binding, 0, len(in))
	for _, b := range in {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
