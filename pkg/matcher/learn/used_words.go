package learn

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// ServedNamed is Served of Named(e, vars, clocks), and keeps, while test
// number n runs, each variable vars uses at one word of a request place of
// e: a place a variable holds whole names it (Seen.Name), and one it holds at
// a word is kept here (UsedWords), which a later answer that holds the
// place's recorded text whole is answered with at that word. A word a
// variable is bound at is the tracker's already (Name, BoundWords).
func (t *Tracker) ServedNamed(n uint64, e Earlier, vars models.MockVariables, clocks map[string]models.ClockOffset) {
	t.Served(n, Named(e, vars, clocks))
	if t == nil {
		return
	}
	used := usedWordNames(e, vars)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch != n {
		return
	}
	for _, u := range used {
		t.usedWords[e.Mock+"\x00"+u.place] = u.name
	}
}

// usedWord is a variable used at a word place, "<place>#<word>".
type usedWord struct {
	place, name string
}

// usedWordNames are the variables vars uses at a word place,
// "<place>#<word>", a place before the word ("#0" is no place's), the first
// by name where two are used at one word, as Named names a place. UsedWords
// reads a leaf's place and a word of it written as strconv writes a number,
// so a use written any other way ("<place>#01", or a word of the text a step
// opens, "<place>|jwt.sub#0") is kept and never read as a word of the leaf.
// An echo is its call's alone (Named); and of an exchange that is not the
// test's own (Earlier.Own) only the run's variable (models.RunVariable) is
// kept: another test's variable is unbound in this test's replay, as a whole
// place's name is read (boundEarlier).
func usedWordNames(e Earlier, vars models.MockVariables) []usedWord {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []usedWord
	for _, name := range names {
		v := vars[name]
		if v.Echo || !e.Own && !models.RunVariable(name) {
			continue
		}
		for _, u := range v.Use {
			if strings.LastIndexByte(u, '#') < 1 || slices.ContainsFunc(out, func(w usedWord) bool { return w.place == u }) {
				continue
			}
			out = append(out, usedWord{place: u, name: name})
		}
	}
	return out
}

// UsedWords is each variable of test number n its learn step used at a word
// of s, a request leaf of mock (ServedNamed), in word order, as BoundWords
// gives a bound one: the word's index, its text in each run, and s's place.
// The call's request was checked to hold the variable's value at that word,
// so the application sent it there, and a later read of it gives it back.
// Two runs' texts of different word counts name none, as BoundWords; and
// neither does a test that is no longer running.
func (t *Tracker) UsedWords(n uint64, mock string, s Seen) []BoundWord {
	if t == nil || s.Side != Request {
		return nil
	}
	rec, live := variables.Words(s.Leaf.Value), variables.Words(s.Live)
	if len(rec) != len(live) {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch != n {
		return nil
	}
	var out []BoundWord
	for k := range rec {
		if name, ok := t.usedWords[mock+"\x00"+s.Place+"#"+strconv.Itoa(k)]; ok {
			out = append(out, BoundWord{Name: name, Word: k, Recorded: s.Leaf.Value[rec[k][0]:rec[k][1]], Live: s.Live[live[k][0]:live[k][1]], Place: s.Place})
		}
	}
	return out
}
