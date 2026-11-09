package integrations

import (
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// takenLog is a take log that holds each case AnsweredInRound and
// ClaimedInRound tell apart, read at view generation 2.
func takenLog() []Take {
	m := func(name string) *models.Mock { return &models.Mock{Name: name} }
	return []Take{
		{Mock: m("consumed"), How: TakeConsumed, Of: -1},                           // 0
		{Mock: m("spent-old-view"), How: TakeSpent, ForView: true, Gen: 1, Of: -1}, // 1: another view's, given back by the view change
		{Mock: m("spent-this-view"), How: TakeSpent, ForView: true, Gen: 2, Of: -1},
		{Mock: m("spent"), How: TakeSpent, Gen: 1, Of: -1}, // 3
		{Mock: m("given-back"), How: TakeSpent, Of: -1},    // 4
		{Mock: m("given-back"), How: TakeRestored, Of: 4},
		{Mock: m("other"), How: TakeConsumed, Of: -1},           // 6
		{Mock: m("restores-another"), How: TakeRestored, Of: 6}, // names other's entry: gives back none of its own
		{Mock: m("restored-early"), How: TakeRestored, Of: 9},   // 8: names an entry not taken yet
		{Mock: m("restored-early"), How: TakeConsumed, Of: -1},  // 9
		{Mock: m("claimed"), How: TakeClaimed, Of: -1},
		{How: TakeConsumed, Of: -1},                   // no recording
		{Mock: m("twice"), How: TakeConsumed, Of: -1}, // 12
		{Mock: m("twice"), How: TakeConsumed, Of: -1}, // 13
		{Mock: m("twice"), How: TakeRestored, Of: 12}, // one of two given back
	}
}

// A store's snapshot (WithParts) reads its take log once for every ask of
// AnsweredInRound and ClaimedInRound, and answers each as a read of the log
// for that one recording does: a consume or a spend that holds for the view
// in force answers, a give-back takes back only its own recording's entry,
// and a claim is no answer. A copy whose log was cut reads its own log.
func TestRoundSnapshotTakenAnswersAsAScanOfTheLog(t *testing.T) {
	want := map[string][2]bool{ // name: answered, claimed
		"consumed": {true, false}, "spent-old-view": {false, false}, "spent-this-view": {true, false},
		"spent": {true, false}, "given-back": {false, false}, "other": {true, false},
		"restores-another": {false, false}, "restored-early": {true, false}, "claimed": {false, true},
		"twice": {true, false}, "never": {false, false},
	}
	log := takenLog()
	literal := RoundSnapshot{Taken: log, viewGen: 2}
	stored := RoundSnapshot{Taken: log}.WithParts(RoundParts{ViewGen: 2})
	for name, w := range want {
		for _, s := range []struct {
			how  string
			snap RoundSnapshot
		}{{"read through the log", literal}, {"read through the snapshot's index", stored}} {
			if got := s.snap.AnsweredInRound(name); got != w[0] {
				t.Errorf("%s: AnsweredInRound(%q) = %v, want %v", s.how, name, got, w[0])
			}
			if got := s.snap.ClaimedInRound(name); got != w[1] {
				t.Errorf("%s: ClaimedInRound(%q) = %v, want %v", s.how, name, got, w[1])
			}
		}
	}
	if stored.taken == nil || stored.taken.answered == nil {
		t.Fatalf("a store's snapshot answered without its index")
	}
	// A copy whose log ends before the give-back: the spend still answers,
	// and nothing after it is in the log.
	cut := stored
	cut.Taken = log[:5]
	if !cut.AnsweredInRound("given-back") || cut.AnsweredInRound("other") || cut.ClaimedInRound("claimed") {
		t.Errorf("a copy with its log cut at 5 entries answered from the whole log: given-back %v (want true), other %v (want false), claimed %v (want false)",
			cut.AnsweredInRound("given-back"), cut.AnsweredInRound("other"), cut.ClaimedInRound("claimed"))
	}
}

// A learn step asks AnsweredInRound and ClaimedInRound of each recording
// its call's take log names: a store's snapshot reads its log once for all
// of them, never once per ask (the square of the log's length, which on a
// long staging took more of a learn step than anything but its decisions).
func TestRoundSnapshotTakenReadsItsLogOnce(t *testing.T) {
	for _, n := range []int{100, 2000} {
		log := make([]Take, n)
		for i := range log {
			log[i] = Take{Mock: &models.Mock{Name: fmt.Sprintf("mock-%d", i)}, How: TakeConsumed, Of: -1}
		}
		s := RoundSnapshot{Taken: log}.WithParts(RoundParts{})
		before := takeReads.Load()
		for i := range log {
			c := s // each ask on a copy, as a learn step passes the snapshot by value
			if !c.AnsweredInRound(log[i].Mock.Name) || c.ClaimedInRound(log[i].Mock.Name) {
				t.Fatalf("mock-%d: answered %v claimed %v, want answered, not claimed", i, c.AnsweredInRound(log[i].Mock.Name), c.ClaimedInRound(log[i].Mock.Name))
			}
		}
		reads := takeReads.Load() - before
		t.Logf("%d entries, %d asks of each: %d entries read", n, n, reads)
		if reads > int64(2*n) {
			t.Errorf("%d entries asked %d times each read %d entries; want the log read once for all (at most %d)", n, n, reads, 2*n)
		}
	}
}
