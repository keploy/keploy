package learn

import "testing"

// tierParts is every part a tier of setDerived is asked to keep of a
// recording, as derived.go asks for it: each part by keep (byKeep) for both
// keeps, and the decoded forms by whether the stamps are blanked.
func tierParts() map[string]part {
	ps := map[string]part{
		"moments": partMoments,
		"places":  partPlaces,
		"fields":  partFields,
		"forms":   partForms,
		"stamps":  partStamps,
		"at":      partAt,
		"extra":   partExtra,
		"keys":    partKeys,
	}
	for name, p := range map[string]part{
		"filter":   partFilter,
		"plain":    partPlain,
		"tokens":   partTokens,
		"answer":   partAnswer,
		"searched": partSearched,
	} {
		ps[name+" (keep false)"] = byKeep(p, false)
		ps[name+" (keep true)"] = byKeep(p, true)
	}
	ps["decoded (stamps as recorded)"] = partDecoded
	ps["decoded (stamps blanked)"] = partDecoded << 1
	return ps
}

// Each part a tier keeps is a bit of slot.has of its own, so a slot holds
// it once kept. A part whose shift runs past part's width is 0: a tier then
// never finds it (find reads has&0), so every call derives it again, and
// keeps it again, counting its size into the tier's budget once more each
// time, so an LRU tier lets go of other recordings' parts to make room for
// copies of it. With part a uint16, partSearched by keep (bit 16) was 0
// that way (fixed by making part a uint32).
func TestEachPartATierKeepsIsABitOfItsOwnThatTheTierHolds(t *testing.T) {
	var seen part
	for name, p := range tierParts() {
		if p == 0 {
			t.Errorf("%s: no bit of slot.has (its shift runs past part's width): a tier never holds it", name)
			continue
		}
		if p&(p-1) != 0 {
			t.Errorf("%s = %#x: more than one bit", name, p)
		}
		if seen&p != 0 {
			t.Errorf("%s = %#x: shares its bit with another part", name, p)
		}
		seen |= p
	}
	tr := newTier[int](1<<20, &budget{limit: 1 << 20}, true)
	for name, p := range tierParts() {
		if p == 0 {
			continue
		}
		tr.keep("rec", p, 8, func(v *int) { *v++ })
		used := tr.used
		if tr.find("rec", p) == nil {
			t.Errorf("%s: kept, then not found", name)
		}
		tr.keep("rec", p, 8, func(v *int) { *v++ })
		if tr.used != used {
			t.Errorf("%s: kept again, the tier counts %d bytes, was %d: one part counted twice", name, tr.used, used)
		}
	}
	if sl := tr.find("rec", seen); sl == nil || sl.has != seen || sl.p != len(tierParts()) {
		t.Errorf("the slot holds %+v; want every part (%#x), each put once", sl, seen)
	}
}
