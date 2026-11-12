package learn

import (
	"encoding/base64"
	"encoding/hex"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows of a minted value's chance floor (mintedCopy, shownIn): a word of the
// minted value is evidence that another field of the call copies it only
// where that field holds, in the recording, a word of the value that carries
// chanceBits or more. A shorter word of it (a base64url id's one-letter
// word, a UUID's 4-hex group) stands elsewhere by chance, so the field keeps
// learning's decisions: the rule never sets aside a test the run with no
// rule keeps over a field it does not declare. A copy inside a longer word
// is no place a use can name, and keeps learning's decision too.

// kRec and kLiv are api-server's kepreq_ ids (newSecret: base64url of 32
// bytes), whose words pair in both runs; the recorded one holds the
// one-letter word "x".
const (
	kRec = "kepreq_Qa7Lm2Zp9Rt4Vw8Yb3Nc6Hd1Fg5Jk0S-x_Ue7Io2PqWs"
	kLiv = "kepreq_Zp9Rt4Vw8Yb3Nc6Hd1Fg5Jk0SQa7Lm2-q_Te7Io2PqWs"
)

// kForm is the form of a kepreq_ id.
var kForm = models.MintedForm{Prefix: "kepreq_", MinLen: 50, MaxLen: 50}

// mKeptApart checks ds is the field mPath decided made anew whole by mRule
// beside a field ref that the rule left to learning: every decision at ref
// made anew, no use of the minted field, and no refusal anywhere.
func mKeptApart(t *testing.T, what string, ds []Decision, ref string) {
	t.Helper()
	var minted, at int
	for _, d := range ds {
		switch {
		case d.Refusal != nil:
			t.Errorf("FALSE RED: %s: %s is refused; a word of the minted value that stands there by chance is no copy of it", what, oDescribe([]Decision{d}))
		case d.Minted != nil && mAtPath(d) == mPath:
			minted++
		case mAtPath(d) == ref:
			at++
			if d.Outcome != OutcomeFresh || d.From.Mock != "" {
				t.Errorf("%s: %s; want learning's decision at the field the rule does not declare (made anew)", what, oDescribe([]Decision{d}))
			}
		default:
			t.Errorf("%s: %s is neither the minted field nor the other field", what, oDescribe([]Decision{d}))
		}
	}
	if minted != 1 || at == 0 {
		t.Errorf("%s: %s; want the minted field decided whole and the other field as learning decided it", what, oDescribe(ds))
	}
}

func TestAMintedValuesShortWordAtAnotherFieldIsChance(t *testing.T) {
	const hash = "c0ffee12d34b56789e0a1b2c3d4e5f6a7b8c9d0e9444f1a2b3c4d5e6f7a8b9c0"
	for _, c := range []struct {
		what, id, idLive, r, l string
		f                      models.MintedForm
	}{
		{"a kepreq_ id's one-letter word inside a secret's word", kRec, kLiv, "kepc_9f8e7d6c5b4a3x2w1v0u", "kepc_0a1b2c3d4e5f6g7h8i9j", kForm},
		{"a kepreq_ id's one-letter word inside a secret's word, the id's words unpaired in round F", kRec, "kepreq_Lm2Zp9Rt4Vw8Yb3Nc6Hd1Fg5Jk0SQa7-Te7Io2PqWsAb", "kepc_9f8e7d6c5b4a3x2w1v0u", "kepc_0a1b2c3d4e5f6g7h8i9j", kForm},
		{"a kepreq_ id's one-letter word as a word of a secret", kRec, kLiv, "kepc_Hd1Fg5Jk0SbN3c-x_Wq8Er4Ty6Ui", "kepc_Pz2Lk7Mj9NhB4v-y_Gf3Ds5Aq1We", kForm},
		{"a UUID's 4-hex group inside a word of another UUID", mRec, mLiv, "9fe48d1f-0c2b-4e3a-8f1d-2b3c4d5e6f70", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", mForm},
		{"a UUID's 4-hex group as a word of another UUID", mRec, mLiv, "0d9c8b7a-48d1-4c3d-8b2a-1f0e9d8c7b6a", "5e4d3c2b-6a7f-4e8d-9c0b-1a2b3c4d5e6f", mForm},
		{"a UUID's 4-hex group inside a sha256 hex", mRec, mLiv, hash, "77aa" + hash[4:], mForm},
	} {
		for _, refFirst := range []bool{false, true} {
			in, ref := mCopyInput(c.id, c.idLive, c.r, c.l, refFirst)
			in.Source = mMintedAs(mPath, c.f)
			mKeptApart(t, c.what+" (the other field first "+strconv.FormatBool(refFirst)+")", Decide(in), ref)
		}
	}
}

// A rate row, as the minted form's: of 10,000 random calls of each shape
// (a minted id beside an unrelated value the call makes anew), none that
// the run with no rule keeps is lost only because the rule declares the id.
func TestAMintedRuleLosesNoCallItsIdsChanceWordsStandIn(t *testing.T) {
	const n = 10000
	b64 := func(rng *rand.Rand, k int) string {
		b := make([]byte, k)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	hx := func(rng *rand.Rand, k int) string {
		b := make([]byte, k)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		return hex.EncodeToString(b)
	}
	uuid := func(rng *rand.Rand) string {
		s := hx(rng, 16)
		return s[:8] + "-" + s[8:12] + "-4" + s[13:16] + "-" + string("89ab"[rng.IntN(4)]) + s[17:20] + "-" + s[20:]
	}
	refused := func(ds []Decision) bool {
		return slices.ContainsFunc(ds, func(d Decision) bool { return d.Refusal != nil })
	}
	for k, c := range []struct {
		what    string
		id, tok func(*rand.Rand) string
		f       models.MintedForm
	}{
		{"a kepreq_ id beside a kepc_ secret", func(r *rand.Rand) string { return "kepreq_" + b64(r, 32) }, func(r *rand.Rand) string { return "kepc_" + b64(r, 32) }, kForm},
		{"a UUID beside a sha256 hex", uuid, func(r *rand.Rand) string { return hx(r, 32) }, mForm},
		{"a UUID beside another UUID", uuid, uuid, mForm},
	} {
		t.Run(c.what, func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(11, uint64(k)))
			lost, kept := 0, 0
			var first string
			for range n {
				in, _ := mCopyInput(c.id(rng), c.id(rng), c.tok(rng), c.tok(rng), false)
				if refused(Decide(in)) {
					continue
				}
				kept++
				in.Source = mMintedAs(mPath, c.f)
				if ds := Decide(in); refused(ds) {
					lost++
					if first == "" {
						first = oDescribe(ds)
					}
				}
			}
			if lost > 0 || kept < n*99/100 {
				t.Errorf("FALSE RED: %s: of %d calls the run with no rule keeps, %d (%.2f%%) are lost only because the rule declares the id, first %s", c.what, kept, lost, 100*float64(lost)/float64(max(kept, 1)), first)
			}
		})
	}
}

// A copy of the minted value inside a longer word ("x<uuid>", or "x" and a
// dashless UUID): keploy places no use inside a word, so the word keeps
// learning's decision, as with no rule; the UUID's other words are uses.
func TestAMintedValueInsideALongerWordKeepsLearningsDecision(t *testing.T) {
	for _, refFirst := range []bool{false, true} {
		what := "a UUID inside a longer word (copy first " + strconv.FormatBool(refFirst) + ")"
		in, ref := mCopyInput(mRec, mLiv, "x"+mRec, "x"+mLiv, refFirst)
		in.Source = mMinted(mPath)
		var uses []int
		for _, d := range Decide(in) {
			switch {
			case d.Refusal != nil:
				t.Errorf("FALSE RED: %s: %s; the word that holds the id keeps learning's decision", what, oDescribe([]Decision{d}))
			case mAtPath(d) != ref:
			case d.Outcome == OutcomeValue && d.From.Path == mPath:
				uses = append(uses, d.FromAt.Word)
			case d.Outcome != OutcomeFresh || d.Recorded != "x"+mRec[:8]:
				t.Errorf("%s: %s; want x and the id's first group made anew, as learning decided", what, oDescribe([]Decision{d}))
			}
		}
		if !slices.Equal(uses, []int{1, 2, 3, 4}) {
			t.Errorf("FALSE GREEN? %s: the copy's other words use the minted field's words %v; want 1 2 3 4", what, uses)
		}
		const dRec, dLiv = "1111aaaa222248d19444444455556666", "9999cccc77774a2e8888000011112222"
		what = "a dashless UUID inside a longer word (copy first " + strconv.FormatBool(refFirst) + ")"
		in, ref = mCopyInput(dRec, dLiv, "x"+dRec, "x"+dLiv, refFirst)
		in.Source = mMintedAs(mPath, models.MintedForm{MinLen: 32, MaxLen: 32})
		mKeptApart(t, what, Decide(in), ref)
	}
}

// A copy of a word of an id whose words round F did not pair (round F's id
// splits into one word more): keploy cannot tell which word of round F's id
// that word is, so it places no use, and the copy keeps learning's decision.
// (A copy of such an id whole, inside a longer text, adds or drops a word
// itself, and learning refuses it as of another shape.)
func TestAMintedValuesWordCopyWhoseWordsRoundFDidNotPairKeepsLearningsDecision(t *testing.T) {
	const live = "kepreq_Zp9Rt4Vw8Yb3Nc6-d1Fg5Jk0SQa7Lm2-q_Te7Io2PqWs"
	for _, refFirst := range []bool{false, true} {
		in, ref := mCopyInput(kRec, live, "Qa7Lm2Zp9Rt4Vw8Yb3Nc6Hd1Fg5Jk0S", "Zp9Rt4Vw8Yb3Nc6", refFirst)
		in.Source = mMintedAs(mPath, kForm)
		mKeptApart(t, "a kepreq_ id's word copied, the id's words unpaired in round F (copy first "+strconv.FormatBool(refFirst)+")", Decide(in), ref)
	}
}

// A copy whose one word of chanceBits is the id's 32-bit group: that word
// shows the copy, so the id's 16-bit groups beside it are uses too.
func TestAMintedValuesCopyShownByA32BitWordUsesItsShorterWords(t *testing.T) {
	const rec, live = "1111aaaa-2222-48d1", "9999cccc-7777-4a2e"
	for _, refFirst := range []bool{false, true} {
		in, ref := mCopyInput(rec, live, "apps/"+rec, "apps/"+live, refFirst)
		in.Source = mMintedAs(mPath, models.MintedForm{MinLen: 18, MaxLen: 18})
		mUsesOf(t, "an id of a 32-bit and two 16-bit groups copied into apps/<id> (copy first "+strconv.FormatBool(refFirst)+")", Decide(in), ref, 0, 1, 2)
	}
}

// A minted value of its own prefixed ("apps/<uuid>"): a copy of it whole is
// one use of it whole, and a copy of its bare UUID uses each of the UUID's
// words, the prefix being word 0.
func TestAPrefixedMintedValuesCopiesAreUsesOfIt(t *testing.T) {
	const pRec, pLiv = "apps/" + mRec, "apps/" + mLiv
	f := models.MintedForm{Prefix: "apps/", MinLen: 41, MaxLen: 41}
	for _, refFirst := range []bool{false, true} {
		order := " (copy first " + strconv.FormatBool(refFirst) + ")"
		in, ref := mCopyInput(pRec, pLiv, pRec, pLiv, refFirst)
		in.Source = mMintedAs(mPath, f)
		mUsesOf(t, "a prefixed id copied whole"+order, Decide(in), ref)
		in, ref = mCopyInput(pRec, pLiv, mRec, mLiv, refFirst)
		in.Source = mMintedAs(mPath, f)
		mUsesOf(t, "a prefixed id's bare UUID copied"+order, Decide(in), ref, 1, 2, 3, 4, 5)
		in, ref = mCopyInput(pRec, pLiv, mRec, "0d9c8b7a-6f5e-4d3c-8b2a-1f0e9d8c7b6a", refFirst)
		in.Source = mMintedAs(mPath, f)
		ds := Decide(in)
		for _, d := range ds {
			if mAtPath(d) == ref && (d.Refusal == nil || d.Refusal.Kind != RefusalTwoSources) {
				t.Errorf("FALSE GREEN? a prefixed id's bare UUID that round F sent apart%s: %s; want each word refused as two values", order, oDescribe([]Decision{d}))
			}
		}
	}
}

// A minted "<copied>-<random>" whose copied word learning bound to an
// earlier call: a copy of the value whole is one use of the minted field
// whole; inside a longer text, the copied word keeps its binding to the
// earlier call and the random word is a use of the minted field's word.
func TestAMintedValuesCopyKeepsAWordLearningBoundToAnEarlierCall(t *testing.T) {
	const tRec, tLiv = "t4821-7f3e9b2c41d6", "t9057-c0d18e5a77b2"
	f := models.MintedForm{MinLen: 18, MaxLen: 18}
	tenant := func() Earlier {
		return oCarried("mock-48", "body.tenant", "t4821", "t9057", 2*time.Second, time.Second, 1)
	}
	for _, refFirst := range []bool{false, true} {
		order := " (copy first " + strconv.FormatBool(refFirst) + ")"
		for _, c := range []struct {
			what, r, l string
			whole      bool
		}{{"whole", tRec, tLiv, true}, {"into apps/<id>", "apps/" + tRec, "apps/" + tLiv, false}} {
			in, ref := mCopyInput(tRec, tLiv, c.r, c.l, refFirst, tenant())
			in.Source = mMintedAs(mPath, f)
			ds := Decide(in)
			var got []string
			for _, d := range ds {
				switch {
				case mAtPath(d) != ref:
				case d.Outcome == OutcomeValue && d.From.Path == mPath && d.From.Mock == "mock-300":
					got = append(got, "use "+strconv.Itoa(d.FromAt.Word)+" "+strconv.FormatBool(d.From.Whole))
				case d.Outcome == OutcomeValue && d.From.Mock == "mock-48" && d.Recorded == "t4821":
					got = append(got, "tenant")
				default:
					got = append(got, oDescribe([]Decision{d}))
				}
			}
			want := "use -1 true"
			if !c.whole {
				want = "tenant|use 1 false"
			}
			if strings.Join(got, "|") != want {
				t.Errorf("FALSE GREEN? a minted <copied>-<random> copied %s%s: %q (%s); want %q", c.what, order, got, oDescribe(ds), want)
			}
		}
	}
}

// A word copy round F sent apart: its line names round F's word of the
// minted value there, not round F's whole value.
func TestAMintedValuesWordCopySentApartNamesRoundFsWord(t *testing.T) {
	const other = "0d9c8b7a-6f5e-4d3c-8b2a-1f0e9d8c7b6a"
	lw := strings.Split(mLiv, "-")
	in, ref := mCopyInput(mRec, mLiv, "apps/"+mRec, "apps/"+other, false)
	in.Source = mMinted(mPath)
	n := 0
	for _, d := range Decide(in) {
		if mAtPath(d) != ref {
			continue
		}
		n++
		w := d.At.Word - 1
		if d.Refusal == nil || w < 0 || w >= len(lw) || !strings.Contains(d.Line, strconv.Quote(d.Full)+" here and "+strconv.Quote(lw[w])+" there") {
			t.Errorf("%s; want word %d refused naming round F's word %q of the minted value there", oDescribe([]Decision{d}), d.At.Word, lw[max(w, 0)])
		}
	}
	if n != 5 {
		t.Errorf("%d decisions at the copy; want its 5 words refused", n)
	}
}
