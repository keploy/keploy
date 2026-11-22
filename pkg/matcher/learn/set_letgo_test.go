package learn

import (
	"fmt"
	"testing"
)

// The set's one budget lets go of words first. Once the words tier has
// filled it, a held or forms part a recording needs is still kept: the
// words tier lets go of its recordings read least recently, as many bytes as
// the part needs past the budget, and keeps the rest. Held and forms let go
// of none, and a held tier that reads full (room) stops the staging's reads
// (take) and every later call's warm, so a held part refused past words
// that fill the budget would leave the set to read each payload once per
// test. Counted in the tiers' bytes, never timed.
func TestASetsWordsLetGoForItsHeldAndFormsParts(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 1 << 20
	s := newSetDerived()
	s.mu.Lock()
	defer s.mu.Unlock()
	const size = 4 << 10
	id := func(kind string, i int) string { return fmt.Sprintf("%s-%04d", kind, i) }
	slot := int64(slotBytes + len(id("w", 0)))

	// Words fill the set's budget.
	words := 0
	for s.budget.used+size+slot <= s.budget.limit {
		s.words.keep(id("w", words), partTokens, size, func(*wordParts) {})
		words++
	}
	if words < 100 || s.budget.limit-s.budget.used >= size+slot {
		t.Fatalf("the words tier keeps %d recordings, %d of the set's %d bytes: the row needs it full", words, s.budget.used, s.budget.limit)
	}

	// Held and forms parts of 40 recordings each are kept, past them.
	const n = 40
	for i := 0; i < n; i++ {
		s.held.keep(id("h", i), partMoments, size, func(*heldParts) {})
		s.forms.keep(id("f", i), partDecoded, size, func(*formParts) {})
	}
	if !s.held.room() || !s.forms.room() {
		t.Fatalf("held full %v, forms full %v once words filled the set's budget: want both kept, the words let go", s.held.full, s.forms.full)
	}
	if len(s.held.by) != n || len(s.forms.by) != n {
		t.Errorf("held keeps %d recordings and forms %d of %d each: want all", len(s.held.by), len(s.forms.by), n)
	}
	if used := s.held.used + s.words.used + s.forms.used; used != s.budget.used || used > s.budget.limit {
		t.Errorf("the tiers keep %d bytes, the budget counts %d of %d: want them equal and within it", used, s.budget.used, s.budget.limit)
	}

	// The words let go of their recordings read least recently, and only
	// as many as held and forms needed: one recording's room past them.
	gone := words - len(s.words.by)
	if gone == 0 {
		t.Fatal("the words tier let go of nothing")
	}
	for i := 0; i < gone; i++ {
		if s.words.by[id("w", i)] != nil {
			t.Errorf("the words tier kept %s, among the %d it read least recently, and let go of later ones", id("w", i), gone)
			break
		}
	}
	if free := s.budget.limit - s.budget.used; free >= size+slot {
		t.Errorf("%d bytes of the set's budget are free once held and forms are kept: the words let go of more than they needed", free)
	}
}
