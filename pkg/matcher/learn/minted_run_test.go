package learn

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows of a minted value shown at another field by more than one word of it
// (mintedCopy, shownIn): a field shows the minted value where a text of it
// holds the value whole, or a run of the value's consecutive words, as the
// value writes them, whose words made anew carry chanceBits or more between
// them. A key of 4-character groups ("QX7K-M2RD-T9VB-W4HZ") has no word of
// 32 bits, so its copy inside a longer text is shown by the whole or by a run,
// never by one word; with the rule the copy's words are uses of the minted
// field's words, as they are with no rule (never a value of the copy's own).

// rKeys are 4x4 keys, letters and digits (four words of 20.7 bits) and hex
// (three of 16 bits and one of 13.3), recorded and round F's.
var rKeys = []struct{ what, rec, live string }{
	{"a 4x4 alnum key", "QX7K-M2RD-T9VB-W4HZ", "B8NP-L3WS-H6JC-D5QF"},
	{"a 4x4 hex key", "ab12-cd34-ef56-0789", "9e8d-7c6b-5a41-3f2e"},
}

// rTexts are the longer texts a copy stands in.
var rTexts = []string{"redeem/%s", "keys/%s", "your code is %s"}

// rForm is the form of a 4x4 key.
var rForm = models.MintedForm{MinLen: 19, MaxLen: 19}

// rIn is the call whose minted field holds the key and whose other field
// holds it inside text (a "%s" pattern), refFirst as mCopyInput takes it,
// with the rule where rule; the copy's round F text holds refLive.
func rIn(key, live, refLive, text string, refFirst, rule bool) (Input, string) {
	in, ref := mCopyInput(key, live, strings.Replace(text, "%s", key, 1), strings.Replace(text, "%s", refLive, 1), refFirst)
	if rule {
		in.Source = mMintedAs(mPath, rForm)
	}
	return in, ref
}

// rCase names a row's call.
func rCase(what, text string, refFirst bool) string {
	return what + " copied as " + strconv.Quote(text) + " (copy first " + strconv.FormatBool(refFirst) + ")"
}

func TestAMintedKeyOfShortWordsCopiedInsideALongerTextIsAUseOfEachWord(t *testing.T) {
	for _, k := range rKeys {
		for _, text := range rTexts {
			for _, refFirst := range []bool{false, true} {
				what := rCase(k.what, text, refFirst)
				// The rule: each of the copy's four words is a use of the
				// minted field's word.
				in, ref := rIn(k.rec, k.live, k.live, text, refFirst, true)
				mUsesOf(t, what, Decide(in), ref, 0, 1, 2, 3)
				// No rule: the copy's words are uses of the field's words,
				// as learning splits them.
				in, _ = rIn(k.rec, k.live, k.live, text, refFirst, false)
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
				if fresh != 4 || used != 4 {
					t.Errorf("%s with no rule: %s; want its four words bound and used at the copy", what, oDescribe(ds))
				}
				// Round F's copy is another key: two values at each word,
				// with the rule.
				other := strings.Map(func(r rune) rune {
					if r == '-' {
						return r
					}
					return 'z'
				}, k.live)
				in, ref = rIn(k.rec, k.live, other, text, refFirst, true)
				n := 0
				for _, d := range Decide(in) {
					if mAtPath(d) != ref {
						continue
					}
					n++
					if d.Refusal == nil || d.Refusal.Kind != RefusalTwoSources || !strings.Contains(d.Line, mRule) {
						t.Errorf("FALSE GREEN? %s, round F's copy another key: %s; want each word refused as two values, naming the rule", what, oDescribe([]Decision{d}))
					}
				}
				if n != 4 {
					t.Errorf("%s, round F's copy another key: %d decisions at the copy; want its four words refused", what, n)
				}
			}
		}
	}
}

// A copy of a run of the key's words, not of the key whole: two 4-character
// groups carry 41 bits (alnum) or exactly 32 (hex), so the run shows the key
// and its words are uses of the minted field's words 0 and 1.
func TestAMintedKeysRunOfWordsCopiedIsAUseOfEachWord(t *testing.T) {
	for _, k := range rKeys {
		for _, refFirst := range []bool{false, true} {
			what := rCase(k.what+"'s first two groups", "code %s here", refFirst)
			in, ref := rIn(k.rec, k.live, k.live, "", refFirst, true)
			in.Rec.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: "code " + k.rec[:9] + " here"}
			in.Live.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: "code " + k.live[:9] + " here"}
			mUsesOf(t, what, Decide(in), ref, 0, 1)
		}
	}
}

// A minted value of under 32 bits whole ("Q7K-M2R", two words of 15.5 bits)
// copied inside a longer text: the whole value shows it, whatever its words'
// bits.
func TestAMintedValueOfFewBitsCopiedWholeInsideALongerTextIsAUseOfEachWord(t *testing.T) {
	for _, refFirst := range []bool{false, true} {
		in, ref := mCopyInput("Q7K-M2R", "B8N-L3W", "redeem/Q7K-M2R", "redeem/B8N-L3W", refFirst)
		in.Source = mMintedAs(mPath, models.MintedForm{MinLen: 7, MaxLen: 7})
		mUsesOf(t, rCase("a 31-bit id", "redeem/%s", refFirst), Decide(in), ref, 0, 1)
	}
}

// A copy inside a text a step opens (base64 of "redeem/<key>"): the decoded
// text shows the key, so its words are uses of the minted field's words.
func TestAMintedKeyCopiedInsideATextAStepOpensIsAUseOfEachWord(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, k := range rKeys {
		for _, refFirst := range []bool{false, true} {
			in, ref := rIn(k.rec, k.live, k.live, "", refFirst, true)
			in.Rec.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: b64("redeem/" + k.rec)}
			in.Live.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: b64("redeem/" + k.live)}
			in.PlaceOf = func(l Leaf, answer bool) (string, bool) {
				if l.Path == ref {
					return l.Path + "|b64", true
				}
				return l.Path, true
			}
			mUsesOf(t, rCase(k.what, "b64(redeem/%s)", refFirst), Decide(in), ref, 0, 1, 2, 3)
		}
	}
}

// A word of a minted value of 24 to 31 bits (6 hex, 6 letters and digits)
// at another field by chance, its other words not beside it: under
// chanceBits, so no copy of the value, and the field keeps learning's
// decision.
func TestAMintedValuesWordOfUnder32BitsAtAnotherFieldIsChance(t *testing.T) {
	for _, c := range []struct{ what, rec, live, r, l string }{
		{"a 24-bit hex word", "c0ffee-a1b2c3d4e5f60718", "7d3a91-9e8d7c6b5a413f2e", "color #c0ffee", "color #1b2c3d"},
		{"a 31-bit alnum word", "Q7mK2p-a1b2c3d4e5f60718", "Z9xW4n-9e8d7c6b5a413f2e", "ref Q7mK2p", "ref L3vB8s"},
	} {
		for _, refFirst := range []bool{false, true} {
			in, ref := mCopyInput(c.rec, c.live, c.r, c.l, refFirst)
			in.Source = mMintedAs(mPath, models.MintedForm{MinLen: 23, MaxLen: 23})
			mKeptApart(t, c.what+" of the minted value at another field (copy first "+strconv.FormatBool(refFirst)+")", Decide(in), ref)
		}
	}
}

// A word of the minted value that round F kept (a tenant's) adds no bits:
// another field that holds it beside a one-letter word of the value holds
// nothing made anew of the value, so it keeps learning's decision.
func TestAMintedValuesWordRoundFKeptAddsNoBits(t *testing.T) {
	for _, refFirst := range []bool{false, true} {
		in, ref := mCopyInput("acmecorp42-QX7K-x", "acmecorp42-B8NP-w", "acmecorp42-x", "acmecorp42-q", refFirst)
		in.Source = mMintedAs(mPath, models.MintedForm{Prefix: "acmecorp42-", MinLen: 17, MaxLen: 17})
		mKeptApart(t, "a tenant word and a one-letter word of the minted value at another field (copy first "+strconv.FormatBool(refFirst)+")", Decide(in), ref)
	}
}

// A field of two texts: a JWT whose claim holds the key inside a longer
// text, and the key's first group before or after it, in the field's own
// text. The claim shows the key, so the field shows it: the claim's words
// and the group beside the JWT are each a use of the minted field's word.
func TestAMintedKeyShownInOneTextOfAFieldHoldsItsWordsInEach(t *testing.T) {
	const sig = "c2lnbmF0dXJl"
	for _, k := range rKeys {
		for _, before := range []bool{false, true} {
			for _, refFirst := range []bool{false, true} {
				in, ref := rIn(k.rec, k.live, k.live, "", refFirst, true)
				at := func(key string) string {
					jwt := jwtOf(true, `{"code":"redeem/`+key+`"}`, sig)
					if before {
						return key[:4] + " " + jwt
					}
					return jwt + " " + key[:4]
				}
				in.Rec.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: at(k.rec)}
				in.Live.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: at(k.live)}
				what, words := "a JWT claim of redeem/%s, then its first group", []int{0, 1, 2, 3, 0}
				if before {
					what, words = "its first group, then a JWT claim of redeem/%s", []int{0, 0, 1, 2, 3}
				}
				mUsesOf(t, rCase(k.what, what, refFirst), Decide(in), ref, words...)
			}
		}
	}
}

// One group of a 4x4 key at another field, beside no other word of it: a
// piece of 13 to 21 bits, no copy of the key, so the field keeps learning's
// decision, whichever group it is.
func TestAMintedKeysOneGroupAtAnotherFieldIsChance(t *testing.T) {
	for _, k := range rKeys {
		for w, g := range strings.Split(k.rec, "-") {
			for _, refFirst := range []bool{false, true} {
				in, ref := rIn(k.rec, k.live, "", "", refFirst, true)
				in.Rec.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: "color " + g}
				in.Live.Leaves[ref] = Leaf{Path: ref, Noise: ref, Type: String, Value: "color zzzz"}
				mKeptApart(t, k.what+"'s group "+strconv.Itoa(w)+" at another field (copy first "+strconv.FormatBool(refFirst)+")", Decide(in), ref)
			}
		}
	}
}
