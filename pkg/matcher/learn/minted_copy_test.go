package learn

import (
	"strings"
	"testing"
)

// Rows of a minted field's copies (mintedUses): every other place of the
// call that holds the minted value, whole or a word of it, is a use of the
// minted field, so a replay holds it to the value the rule binds there.
// Without the rule the copy is a use of the field's words, as learning
// splits them; with it, never a value of its own that no replay compares.

// mCopyInput is a call whose minted field mPath holds rec (round F: live)
// and whose field ref holds refRec (round F: refLive), ref listed first in
// the call's order where refFirst.
func mCopyInput(rec, live, refRec, refLive string, refFirst bool, earlier ...Earlier) (Input, string) {
	const ref = "body.docs.msgs.0.ref"
	in := oInput(mPath, rec, live, earlier, nil)
	in.Drift = []string{mPath, ref}
	if refFirst {
		in.Drift = []string{ref, mPath}
	}
	in.Rec.Leaves[ref], in.Live.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: refRec}, Leaf{Path: ref, Noise: ref, Type: String, Value: refLive}
	return in, ref
}

// mUsesOf checks every decision of ds at ref is a use of the minted field
// mPath: whole (word < 0) or of its word word[i] at ref's i-th decision.
func mUsesOf(t *testing.T, what string, ds []Decision, ref string, words ...int) {
	t.Helper()
	var minted, uses int
	for _, d := range ds {
		switch {
		case d.Minted != nil && d.Outcome == OutcomeFresh && mAtPath(d) == mPath:
			minted++
		case d.Outcome == OutcomeValue && d.From.Path == mPath && d.From.Mock == "mock-300":
			want := -1
			if uses < len(words) {
				want = words[uses]
			}
			if d.FromAt.Word != want || d.From.Whole != (want < 0) || !strings.Contains(d.Line, mRule) {
				t.Errorf("FALSE GREEN? %s: use %d is %s (from word %d, whole %v); want a use of word %d of the minted field, naming the rule", what, uses, oDescribe([]Decision{d}), d.FromAt.Word, d.From.Whole, want)
			}
			uses++
		default:
			t.Errorf("FALSE GREEN? %s: %s is neither the minted field nor a use of it", what, oDescribe([]Decision{d}))
		}
	}
	if minted != 1 || uses != max(1, len(words)) {
		t.Errorf("FALSE GREEN? %s: %s; want the minted field and %d use(s) of it at %s", what, oDescribe(ds), max(1, len(words)), ref)
	}
}

func TestAMintedFieldsCopyIsAUseOfItWholeOrByWord(t *testing.T) {
	const hexRec, hexLiv = "64b7f0c2a1d3e5f6a7b8c9d0", "65c8e1d3b2e4f6a7b8c9d0e1"
	hexForm := mForm
	hexForm.MinLen, hexForm.MaxLen = 24, 24
	for _, refFirst := range []bool{false, true} {
		order := map[bool]string{false: "the copy last", true: "the copy first"}[refFirst]
		// A UUID copied whole: one use of the minted value whole, not five
		// values of the copy's own that no replay compares.
		in, ref := mCopyInput(mRec, mLiv, mRec, mLiv, refFirst)
		in.Source = mMinted(mPath)
		mUsesOf(t, "a UUID copied whole, "+order, Decide(in), ref)
		// Inside a longer text: each of its words a use of that word of
		// the minted field.
		in, ref = mCopyInput(mRec, mLiv, "apps/"+mRec, "apps/"+mLiv, refFirst)
		in.Source = mMinted(mPath)
		mUsesOf(t, "a UUID copied into apps/<id>, "+order, Decide(in), ref, 0, 1, 2, 3, 4)
		// One word: a use of it whole, whole or inside a longer text.
		for _, c := range []struct{ what, r, l string }{{"whole", hexRec, hexLiv}, {"into apps/<id>", "apps/" + hexRec, "apps/" + hexLiv}} {
			in, ref = mCopyInput(hexRec, hexLiv, c.r, c.l, refFirst)
			in.Source = mMintedAs(mPath, hexForm)
			mUsesOf(t, "a one-word id copied "+c.what+", "+order, Decide(in), ref)
		}
		// Without the rule, the copy uses the field's words as learning
		// splits them: kept as it was.
		in, _ = mCopyInput(mRec, mLiv, mRec, mLiv, refFirst)
		ds := Decide(in)
		var fresh, used int
		for _, d := range ds {
			switch {
			case d.Outcome == OutcomeFresh && d.Minted == nil:
				fresh++
			case d.Outcome == OutcomeValue && d.From.Mock == "mock-300":
				used++
			}
		}
		if fresh != 5 || used != 5 {
			t.Errorf("a UUID copied whole with no rule, %s: %s; want its five words bound and used at the copy", order, oDescribe(ds))
		}
	}
}

func TestAMintedFieldsCopyThatRoundFSentApartIsRefused(t *testing.T) {
	const other = "0d9c8b7a-6f5e-4d3c-8b2a-1f0e9d8c7b6a"
	for _, c := range []struct {
		what, refRec, refLive string
		kind                  RefusalKind
	}{
		{"a UUID copied whole that round F sent apart", mRec, other, RefusalTwoSources},
		{"a UUID copied into apps/<id> that round F sent apart", "apps/" + mRec, "apps/" + other, RefusalTwoSources},
	} {
		for _, refFirst := range []bool{false, true} {
			in, ref := mCopyInput(mRec, mLiv, c.refRec, c.refLive, refFirst)
			in.Source = mMinted(mPath)
			ds := Decide(in)
			refused := false
			for _, d := range ds {
				if mAtPath(d) == ref && d.Refusal != nil && d.Refusal.Kind == c.kind {
					refused = true
				}
				if mAtPath(d) == ref && (d.Outcome == OutcomeFresh || d.Outcome == OutcomeValue && d.From.Path != mPath) {
					t.Errorf("FALSE GREEN? %s (copy first %v): %s holds the copy apart from the minted value", c.what, refFirst, oDescribe([]Decision{d}))
				}
			}
			if !refused {
				t.Errorf("FALSE GREEN? %s (copy first %v): %s; want the copy refused as %s", c.what, refFirst, oDescribe(ds), c.kind)
			}
			mNamesRule(t, c.what, ds)
		}
	}
}

// mAtPath is the field d decides, as its line names it.
func mAtPath(d Decision) string {
	for _, p := range []string{mPath, "body.docs.msgs.0.ref"} {
		if strings.HasPrefix(d.Line, "test-2 mock-300 "+p+": ") || strings.HasPrefix(d.Line, "test-2 mock-300 "+p+" word ") {
			return p
		}
	}
	return ""
}

// A minted value whose recorded words repeat: the copy's word is a use of
// the word of it round F's value pairs with there.
func TestAMintedFieldsCopyOfARepeatedWordUsesTheWordRoundFPairs(t *testing.T) {
	const rec, live = "abcd1234-2222-48d1-2222-444455556666", "9999cccc-7777-4a2e-8888-000011112222"
	in, ref := mCopyInput(rec, live, "apps/"+rec, "apps/"+live, false)
	in.Source = mMinted(mPath)
	mUsesOf(t, "a UUID of a repeated word copied into apps/<id>", Decide(in), ref, 0, 1, 2, 3, 4)
}

// A copy of which learning refused a word (here a group of the id that an
// earlier call's text holds by chance) keeps learning's refusal: the rule
// decides its own field whole, and holds no copy past a refusal.
func TestAMintedFieldsCopyKeepsAWordLearningRefused(t *testing.T) {
	for _, c := range []struct{ what, r, l string }{{"whole", mRec, mLiv}, {"into apps/<id>", "apps/" + mRec, "apps/" + mLiv}} {
		in, ref := mCopyInput(mRec, mLiv, c.r, c.l, false, mChance())
		in.Source = mMinted(mPath)
		ds := Decide(in)
		refused := false
		for _, d := range ds {
			if mAtPath(d) == ref && d.Refusal != nil {
				refused = true
			}
			if mAtPath(d) == mPath && d.Refusal != nil {
				t.Errorf("%s: %s; want the minted field decided whole by the rule", c.what, oDescribe([]Decision{d}))
			}
		}
		if !refused {
			t.Errorf("FALSE GREEN? a UUID copied %s beside a chance holder: %s; want learning's refusal of its word kept", c.what, oDescribe(ds))
		}
	}
}

// A one-word id at a word of a longer text that round F sent apart from
// the minted field: two values, refused.
func TestAMintedFieldsOneWordCopyThatRoundFSentApartIsRefused(t *testing.T) {
	const hexRec, hexLiv, hexNext = "64b7f0c2a1d3e5f6a7b8c9d0", "65c8e1d3b2e4f6a7b8c9d0e1", "66d9f2e4c3f5a7b8c9d0e1f2"
	f := mForm
	f.MinLen, f.MaxLen = 24, 24
	in, ref := mCopyInput(hexRec, hexLiv, "apps/"+hexRec, "apps/"+hexNext, false)
	in.Source = mMintedAs(mPath, f)
	ds := Decide(in)
	for _, d := range ds {
		if mAtPath(d) == ref && (d.Refusal == nil || d.Refusal.Kind != RefusalTwoSources) {
			t.Errorf("FALSE GREEN? a one-word id copied into apps/<id> that round F sent apart: %s; want it refused as two values", oDescribe(ds))
		}
	}
}
