package http

import (
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// An HTTP API can be a store (models.HTTPStoreReader): a call of the test's
// writes a record, and a later one reads it back. keploy answers the read
// with its recorded answer, which holds what the recorded run wrote, while
// the application checks it against what it wrote in this run (a lead's
// name, stamped with the time it was made). The read-back pass answers such
// a read with what this run wrote, as Mongo's does (integrations'
// planReadBack): each leaf of the read's recorded answer that the reader
// links to a record, and that learn.ReadBack finds written earlier in the
// test by one of the test's own calls with another value in this run, is a
// variable bound at that write and answered here. Every other leaf is
// answered as recorded.
//
// It lives beside the learn step's pairing, tracker and claim (learnPlan,
// claimLearned), which it extends; what a call of a store's API writes and
// reads is the registered reader's (enterprise's DynamoDB reader).

// readBackPlan adds to p what the read answered (m's recorded call, with its
// recorded answer) reads back: for each answer leaf the store reader links to
// a record (models.HTTPStoreAccessOf, over the calls of served, the test's
// calls served before it in the order served, that the reader follows for
// the read), and p does not answer already, the place learn.ReadBack finds
// in earlier with the reader's marks of what each call stored, when the
// test's own call made it. A leaf whose write holds a variable of the test
// already (bound or used there) answers that variable; any other is a new
// variable bound at the write (p.binds). p is returned as it was where no
// reader reads m, or where the answer of a call the reader follows cannot be
// read back. Only the answers of the calls the reader follows are loaded
// (models.HTTPStoreFollows decides from the requests alone), so a read loads
// no answer of another API's call.
func (h *HTTP) readBackPlan(tracker *learn.Tracker, test uint64, answered *models.Mock, live learn.Shape, served []*models.Mock, earlier []learn.Earlier, given []variables.Binding, p pairing) pairing {
	if answered == nil {
		return p
	}
	follows := func(read, call *models.Mock) bool {
		return models.HTTPStoreFollows(read.Spec.HTTPReq, call.Spec.HTTPReq)
	}
	accessOf := models.HTTPStoreAccessOf
	if !models.HTTPStoreReads(answered.Spec.HTTPReq) {
		// No registered reader reads it: HTTP's own store does where it is
		// a GET (putstore.go).
		if !putReads(answered) {
			return p
		}
		follows = putFollows
		accessOf = func(read *models.Mock, calls []*models.Mock) (models.HTTPStoreAccess, bool) {
			return putAccess(read, calls, earlier, live)
		}
	}
	calls := make([]*models.Mock, 0, len(served))
	learned := map[string]models.MockVariables{}
	for _, s := range served {
		if !follows(answered, s) {
			continue
		}
		learned[s.Name] = s.Spec.Learned()
		c, err := withResponse(s)
		if err != nil {
			h.Logger.Warn("keploy cannot read back the recorded answer of a call served earlier in the test, so it answers a later read of what the test stored as recorded",
				zap.String("mock", s.Name), zap.Error(err))
			return p
		}
		calls = append(calls, c)
	}
	access, ok := accessOf(answered, calls)
	if !ok {
		return p
	}
	if len(access.Unread) > 0 {
		// One line per read: how many fields, and the first of them by
		// path with its reason.
		paths := make([]string, 0, len(access.Unread))
		for path := range access.Unread {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		h.Logger.Info("keploy answers fields of a read with their recorded values, not with what this run wrote, because of a write it cannot follow",
			zap.String("mock", answered.Name), zap.Int("fields", len(paths)), zap.String("first", paths[0]), zap.String("why", access.Unread[paths[0]]))
	}
	if len(access.Read) == 0 {
		return p
	}
	// The tracker holds each mock it served once (Tracker.Placed), so a
	// call's name names one serve.
	written := map[string]map[string]string{}
	for i, c := range calls {
		written[c.Name] = access.Written[i]
	}
	marked := markWritten(earlier, written)
	taken := map[string]bool{}
	for _, v := range p.vars {
		for _, a := range v.Answer {
			taken[a] = true
		}
	}
	reserved := map[string]bool{}
	for _, g := range given {
		reserved[g.Name] = true
	}
	for _, a := range respLeaves(answered.Spec.HTTPResp) {
		record, ok := access.Read[a.Path]
		if !ok {
			continue
		}
		place, ok := learn.VarPlace(a, true)
		if !ok || taken[place] {
			continue
		}
		from, ok := learn.ReadBack(a, record, marked)
		if !ok || !from.Own {
			continue
		}
		if name, k, ok := wordUsedAt(learned[from.Mock], from.Var, a.Value); ok {
			// The write's place holds a variable of the test at one word
			// (learn.Place.Word: a word a client test sent), the rest of it
			// held to the recording: the read answers that variable at the
			// same word of its place, never a second one bound at the
			// whole write.
			w := variables.Words(from.Live)
			if k >= len(w) {
				// wordUsedAt read the recorded write; F's holds no such
				// word: the field is answered as recorded.
				continue
			}
			v := p.vars[name]
			v.Recorded, v.Answer = learned[from.Mock][name].Recorded, appendNew(v.Answer, place+"#"+strconv.Itoa(k))
			p.vars[name] = v
			taken[place] = true
			if _, ok := p.values[name]; !ok {
				p.values[name] = variables.Binding{Name: name, Value: from.Live[w[k][0]:w[k][1]], Recorded: v.Recorded, Mock: from.Mock, Place: from.Var + "#" + strconv.Itoa(k)}
			}
			continue
		}
		if from.Name != "" {
			// The write's place holds a variable of the test already: the
			// read answers that variable, bound where its value was first
			// sent, never a second one bound at the write.
			v := p.vars[from.Name]
			v.Recorded, v.Answer = a.Value, appendNew(v.Answer, place)
			p.vars[from.Name] = v
			taken[place] = true
			if _, ok := p.values[from.Name]; !ok {
				p.values[from.Name] = variables.Binding{Name: from.Name, Value: from.Live, Recorded: a.Value, Mock: from.Mock, Place: from.Var}
			}
			continue
		}
		name, ok := tracker.Name(test, from.Mock, from.Var, reserved)
		if !ok {
			continue
		}
		v := p.vars[name]
		v.Recorded, v.Answer = a.Value, appendNew(v.Answer, place)
		p.vars[name] = v
		taken[place] = true
		b := p.binds[from.Mock]
		if b == nil {
			b = models.MockVariables{}
			p.binds[from.Mock] = b
		}
		bv := b[name]
		bv.Recorded, bv.Bind = a.Value, from.Var
		bv.Clock = tracker.Drifts().ClockReads(a.Value, from.Live)
		b[name] = bv
		p.values[name] = variables.Binding{Name: name, Value: from.Live, Recorded: a.Value, Mock: from.Mock, Place: from.Var}
	}
	return p
}

// markWritten is earlier with each request leaf of an exchange of the
// running test (neither the test case's own request nor an earlier test's
// or the start-up's) that written names for its mock marked Written into its
// record (learn.Seen.Written, Record). earlier itself is left as it was: an
// exchange marked is a copy.
func markWritten(earlier []learn.Earlier, written map[string]map[string]string) []learn.Earlier {
	out := make([]learn.Earlier, len(earlier))
	copy(out, earlier)
	for i, e := range out {
		marks := written[e.Mock]
		if e.Given || e.Run || len(marks) == 0 {
			continue
		}
		seen := make([]learn.Seen, len(e.Seen))
		copy(seen, e.Seen)
		for j, s := range seen {
			if s.Side != learn.Request {
				continue
			}
			if record, ok := marks[s.Leaf.Path]; ok {
				seen[j].Written, seen[j].Record = true, record
			}
		}
		out[i].Seen = seen
	}
	return out
}

// wordUsedAt is the variable vars (a write's learned variables) uses at one
// word of place, the write's leaf, whose recorded value is that word of
// value, the read's recorded field, and the word's index. place holds value
// whole (learn.ReadBack), so the read is answered at that word. None, or
// two: false.
func wordUsedAt(vars models.MockVariables, place, value string) (string, int, bool) {
	name, word, n := "", -1, 0
	words := variables.Words(value)
	for k, v := range vars {
		for _, u := range v.Use {
			path, at, ok := strings.Cut(u, "#")
			if !ok || path != place {
				continue
			}
			i, err := strconv.Atoi(at)
			if err != nil || i < 0 || i >= len(words) || value[words[i][0]:words[i][1]] != v.Recorded {
				continue
			}
			name, word, n = k, i, n+1
		}
	}
	return name, word, n == 1
}
