package learn

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// newWordsCall decides a call whose recorded value holds n words (82, 83,
// ...), each changed in round F (50, 51, ...), beside an earlier leaf whose
// recorded text holds every recorded word and 7, and where round F gave 7
// alone (nothing new): what it decided, and nothingNew's reads of a place's
// texts (newWordReads) while it did.
func newWordsCall(n int) ([]Decision, int64) {
	rec, live, held := make([]string, n), make([]string, n), make([]string, n)
	for i := range n {
		rec[i], live[i], held[i] = fmt.Sprintf("took %d ms", 82+i), fmt.Sprintf("took %d ms", 50+i), fmt.Sprint(82+i)
	}
	leaf := eLeaf(eRecAt.Add(-50*time.Millisecond), 6, strings.Join(held, " ")+" 7", "7")
	reads := newWordReads.Load()
	ds := Decide(costF(strings.Join(rec, ", "), strings.Join(live, ", "), []Earlier{leaf}, eStartup(), nil))
	return ds, newWordReads.Load() - reads
}

// Whether round F gave anything new at a place is read once for the call,
// however many changed words its check asks there: the reads do not grow
// with the words, and each word is refused at the leaf F gave nothing new
// at, as the one word of TestOutcomeCostTheEvidenceTablesPinnedRowsStayRefused
// is (follows_test.go's "nothing new" row).
func TestOutcomeCostALeafIsReadForNewWordsOnceACall(t *testing.T) {
	one, oneReads := newWordsCall(1)
	if oneReads != 1 {
		t.Errorf("one word at a leaf F gave nothing new at: %d reads of its texts; want the word's check to read them once", oneReads)
	}
	alone := oWord(t, one, "82", "50")
	costRefused(t, "82 alone, at a leaf F gave nothing new at", alone, costUnknown, "mock-8")
	for _, n := range []int{1, 3, 6} {
		ds, reads := newWordsCall(n)
		for i := range n {
			r, f := fmt.Sprint(82+i), fmt.Sprint(50+i)
			d := oWord(t, ds, r, f)
			costRefused(t, fmt.Sprintf("%s of %d words, at a leaf F gave nothing new at", r, n), d, costUnknown, "mock-8")
			if d.Refusal != nil && alone.Refusal != nil && (d.Refusal.Kind != alone.Refusal.Kind || d.Refusal.Why != alone.Refusal.Why) {
				t.Errorf("%s of %d words: refused %s %q; 82 alone was refused %s %q", r, n, d.Refusal.Kind, d.Refusal.Why, alone.Refusal.Kind, alone.Refusal.Why)
			}
		}
		if reads != oneReads {
			t.Errorf("%d words at one leaf: %d reads of its texts, %d for one word; want one read a place a call, not one a word", n, reads, oneReads)
		}
	}
}

// The answer kept for a place is that place's own: beside the leaf F gave
// nothing new at, a second leaf where F gave a new word keeps its own
// answer, so its recorded word (90) is decided as alone, not as the first
// leaf's are.
func TestOutcomeCostEachLeafKeepsItsOwnNewWordsAnswer(t *testing.T) {
	before := eRecAt.Add(-50 * time.Millisecond)
	payload := RawOf(&models.Mock{Name: "mock-9", Spec: models.MockSpec{ReqTimestampMock: before, ResTimestampMock: before.Add(time.Millisecond)}}, true)
	second := Earlier{Mock: "mock-9", At: before, Payload: payload.ServedAs(7), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.retries", Type: Number, Value: "90"}, Live: "91"},
	}}
	alone := oWord(t, Decide(costF("took 90 ms", "took 51 ms", []Earlier{second}, eStartup(), nil)), "90", "51")
	both := Decide(costF("took 82 ms, took 90 ms", "took 50 ms, took 51 ms", []Earlier{eLeaf(before, 6, "82 7", "7"), second}, eStartup(), nil))
	costRefused(t, "82 beside a second leaf, at the leaf F gave nothing new at", oWord(t, both, "82", "50"), costUnknown, "mock-8")
	if got, want := oDescribe([]Decision{oWord(t, both, "90", "51")}), oDescribe([]Decision{alone}); got != want {
		t.Errorf("90 at a leaf F gave a new word at, beside a leaf F gave nothing new at: %s; decided alone: %s", got, want)
	}
	if alone.Refusal != nil && strings.Contains(alone.Refusal.Why, "holds no word the recorded one lacked") {
		t.Errorf("FALSE GREEN? 90 at a leaf F gave 91 at: refused %q, as if F gave nothing new there", alone.Refusal.Why)
	}
}
