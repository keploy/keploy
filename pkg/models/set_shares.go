package models

import (
	"sort"
	"strings"
	"time"
)

// SetShares is what keploy record's full round settled, at a set's end, of the
// recordings it shared between tests whose requests were open together while
// they were recorded (Mapping.Groups, ExactServing.Concurrent): which test
// made each call. Under the proven switch only, and only in that round; the
// pin step of keploy record's judging writes it onto mappings.yaml
// (Mapping.ApplyShares) before the confirming round replays it.
type SetShares struct {
	// State says whether the round shared recordings in the set, and whether
	// what it settled can be written.
	State SharesState `json:"state" yaml:"state"`
	// Why is, for SharesIncomplete, why the set's shares cannot be settled.
	Why string `json:"why,omitempty" yaml:"why,omitempty"`
	// Pins gives each shared recording a test made the call of to that test,
	// by mock name: it is that test's from here, and in no group.
	Pins map[string]string `json:"pins,omitempty" yaml:"pins,omitempty"`
	// Moves are the shared recordings keploy could not give to the test that
	// made their call, by mock name: each goes to a test the round charged
	// for it (ShareMove.To), marked when its recordings cannot be told apart
	// (ShareMove.Marker), so that no test the round did not charge is left
	// with a call it did not make.
	Moves map[string]ShareMove `json:"moves,omitempty" yaml:"moves,omitempty"`
	// ToShared are the shared recordings no test made the call of while a
	// test the recording did not keep was open beside them: one of those may
	// have made it. They move to Mapping.Shared, owned by no test.
	ToShared []string `json:"toShared,omitempty" yaml:"toShared,omitempty"`
}

// SharesState is SetShares.State.
type SharesState string

const (
	// SharesNone: no test of the set was served a recording it shares
	// with another: its mappings.yaml stays as recorded.
	SharesNone SharesState = ""
	// SharesIncomplete: the set's run did not complete, its agent changed,
	// or what a test that was served shared recordings consumed could not
	// be read. Its round's verdicts were computed with sharing, so the set
	// is not judged, and its mappings.yaml stays as recorded.
	SharesIncomplete SharesState = "incomplete"
	// SharesComplete: every shared recording is pinned, moved, or shared.
	SharesComplete SharesState = "complete"
)

// ShareMove is where a shared recording goes that keploy could not give to the
// test that made its call (SetShares.Moves).
type ShareMove struct {
	To string `json:"to" yaml:"to"`
	// Marker is MockEntry.Unresolved for it, "" for none.
	Marker string `json:"marker,omitempty" yaml:"marker,omitempty"`
}

// Any reports whether s says anything to write.
func (s SetShares) Any() bool {
	return len(s.Pins) > 0 || len(s.Moves) > 0 || len(s.ToShared) > 0
}

// ApplyShares writes what keploy record's full round settled (s) onto m: each
// pinned recording leaves whatever lists it (a test's entry, or Shared) and
// joins its test's entry without its group; each moved one joins its test's
// entry with its group and marker; each recording s puts in Shared leaves the
// test that lists it. Every list keeps recorded order, and the groups no entry
// names any more are dropped. A recording m does not list is left alone. It
// is idempotent: applied twice, it changes nothing the second time. It
// reports whether it changed m.
func (m *Mapping) ApplyShares(s SetShares) bool {
	if m == nil || !s.Any() {
		return false
	}
	before := m.sharesShape()
	named := map[string]bool{}
	for n := range s.Pins {
		named[n] = true
	}
	for n := range s.Moves {
		named[n] = true
	}
	for _, n := range s.ToShared {
		named[n] = true
	}
	// Take every named entry out of where it is listed, the first listing
	// kept as its entry.
	taken := map[string]MockEntry{}
	take := func(list []MockEntry) []MockEntry {
		out := list[:0:0]
		for _, e := range list {
			if !named[e.Name] {
				out = append(out, e)
				continue
			}
			if _, ok := taken[e.Name]; !ok {
				taken[e.Name] = e
			}
		}
		return out
	}
	for i := range m.TestCases {
		if kept := take(m.TestCases[i].Mocks); len(kept) != len(m.TestCases[i].Mocks) {
			m.TestCases[i].Mocks = kept
		}
	}
	if kept := take(m.Shared); len(kept) != len(m.Shared) {
		m.Shared = kept
	}
	add := func(test string, e MockEntry) {
		for i := range m.TestCases {
			if m.TestCases[i].ID == test {
				m.TestCases[i].Mocks = insertRecorded(m.TestCases[i].Mocks, e)
				return
			}
		}
		m.TestCases = append(m.TestCases, MappedTestCase{ID: test, Mocks: []MockEntry{e}})
	}
	for _, name := range sortedNames(taken) {
		e := taken[name]
		switch {
		case s.Pins[name] != "":
			e.Group, e.Unresolved = "", ""
			add(s.Pins[name], e)
		case s.Moves[name].To != "":
			e.Unresolved = s.Moves[name].Marker
			add(s.Moves[name].To, e)
		default:
			e.Unresolved = ""
			m.Shared = insertRecorded(m.Shared, e)
		}
	}
	m.dropUnusedGroups()
	return m.sharesShape() != before
}

// insertRecorded is list with e inserted in recorded order (by request time),
// after every entry recorded at or before it.
func insertRecorded(list []MockEntry, e MockEntry) []MockEntry {
	at := entryRecordedAt(e)
	i := len(list)
	for i > 0 && entryRecordedAt(list[i-1]).After(at) {
		i--
	}
	out := make([]MockEntry, 0, len(list)+1)
	out = append(out, list[:i]...)
	out = append(out, e)
	return append(out, list[i:]...)
}

// entryRecordedAt is when an entry's request was recorded, else its
// timestamp in seconds.
func entryRecordedAt(e MockEntry) time.Time {
	if at, err := ParseMockTimestamp(e.ReqTimestampMock); err == nil && !at.IsZero() {
		return at
	}
	return time.Unix(e.Timestamp, 0)
}

// dropUnusedGroups drops the groups no entry of m names.
func (m *Mapping) dropUnusedGroups() {
	if len(m.Groups) == 0 {
		return
	}
	used := map[string]bool{}
	for _, tc := range m.TestCases {
		for _, e := range tc.Mocks {
			if e.Group != "" {
				used[e.Group] = true
			}
		}
	}
	for _, e := range m.Shared {
		if e.Group != "" {
			used[e.Group] = true
		}
	}
	out := m.Groups[:0:0]
	for _, g := range m.Groups {
		if used[g.ID] {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		out = nil
	}
	m.Groups = out
}

// sharesShape is what ApplyShares changes of m, as one string, to tell
// whether it changed anything.
func (m *Mapping) sharesShape() string {
	var b strings.Builder
	for _, tc := range m.TestCases {
		b.WriteString(tc.ID)
		b.WriteByte('\x00')
		for _, e := range tc.Mocks {
			b.WriteString(e.Name + "\x01" + e.Group + "\x01" + e.Unresolved + "\x02")
		}
		b.WriteByte('\x03')
	}
	for _, e := range m.Shared {
		b.WriteString(e.Name + "\x01" + e.Group + "\x02")
	}
	b.WriteByte('\x03')
	for _, g := range m.Groups {
		b.WriteString(g.ID + "\x02")
	}
	return b.String()
}

// sortedNames is m's keys, sorted.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
