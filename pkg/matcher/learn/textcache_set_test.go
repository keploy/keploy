package learn

import (
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// setCorpus is texts holding instants in every layout the readers know,
// beside words that read as none: alone, in a JSON answer, in a header, in a
// query, and run together at random (a fixed seed).
func setCorpus() []string {
	at := time.Date(2026, 10, 22, 11, 14, 16, 473210300, time.UTC)
	pieces := []string{
		"2026-10-22T20:00:00-10:00", "2026-10-22T20:00:00+05:30", "2026-10-22T20:00:00-0500", "2026-10-22T20:00:00Z",
		at.Format(time.RFC3339Nano), "1792667656", "1792667656.4732103", strconv.FormatInt(at.UnixMilli(), 10),
		"Thu, 22 Oct 2026 11:14:16 GMT", "thu, 22 oct 2026 11:14:16 gmt", "20261022T111413Z", "20261022t111413z",
		"2026-10-22 20:00:00", "2026-10-22", "20261022T111413", "2026-10-22T11Z", "2026-10-22T11+05:30", "11:14:16",
		uuid7(at, "abc123456789abcdef"), "order 2025-13-45 x", "seq 14 of 41", "id 2026-10-22T11a",
		"tr-empty-" + strconv.FormatInt(at.UnixMilli(), 10), "e2e-page-surfaces-1792953247047-tguk95",
	}
	out := append([]string(nil), pieces...)
	for _, p := range pieces {
		out = append(out, `{"data":{"createdAt":"`+p+`","name":"x"}}`, "Date: "+p+"; seq 14", "x-amz-date="+p+"&n=7")
	}
	r := rand.New(rand.NewSource(7))
	seps := []string{" ", ",", `"`, "=", "&", "\n", ""}
	for i := 0; i < 400; i++ {
		var b strings.Builder
		for j := 0; j < 1+r.Intn(6); j++ {
			b.WriteString(seps[r.Intn(len(seps))])
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		out = append(out, b.String())
	}
	return out
}

// rawsOf are payloads of text: as it is, with its answer's clock read at
// each instant it holds, and with each instant it holds as an answer stamp.
func rawsOf(text string) []Raw {
	out := []Raw{{text: text}}
	for _, sp := range instantSpans(text) {
		if sp.keyed {
			out = append(out, Raw{text: text, answerClock: []time.Time{time.Unix(0, sp.key.at)}})
		}
		out = append(out, Raw{text: text, stamps: []rawField{{start: sp.start, end: sp.end}}})
	}
	return out
}

// The set's text cache reads each text's instants, and each payload's
// blanked texts and words, once for the set: every test's learn step reads
// them as a test reading them afresh would, the first test that reads one
// and every test after it that finds it read (TestTheSetsCacheReadsAsAFreshRead).
func TestTheSetsCacheReadsAsAFreshRead(t *testing.T) {
	fresh := &DriftCache{} // no cache: every text read now
	set := newTextCache()
	first, later := newDriftCacheOn(set), newDriftCacheOn(set)
	blank := func(sp instant) bool { return sp.keyed && sp.key.at%2 == 0 }
	for _, text := range setCorpus() {
		want := instantSpans(text)
		for _, dc := range []*DriftCache{fresh, first, later} {
			if got := dc.spansOf(text); !reflect.DeepEqual(got, want) {
				t.Fatalf("spansOf(%q) = %+v; want %+v", text, got, want)
			}
			if got, want := dc.blankSpans(text, blank), blankSpans(text, blank); got != want {
				t.Fatalf("blankSpans(%q) = %q; want %q", text, got, want)
			}
		}
		for _, x := range rawsOf(text) {
			for _, keep := range []bool{false, true} {
				wantText, wantTok := fresh.searched(x, keep), fresh.searchedTokens(x, keep)
				for _, dc := range []*DriftCache{first, later} {
					if got := dc.searched(x, keep); got != wantText {
						t.Fatalf("searched(%q, %v) = %q; want %q", text, keep, got, wantText)
					}
					if got := dc.searchedTokens(x, keep); !reflect.DeepEqual(got, wantTok) {
						t.Fatalf("searchedTokens(%q, %v) differs from a fresh read", text, keep)
					}
				}
			}
		}
	}
	if len(set.spans) == 0 || len(set.blanked) == 0 || len(set.blankTok) == 0 {
		t.Fatalf("the set's cache kept nothing: spans %d, blanked %d, blankTok %d", len(set.spans), len(set.blanked), len(set.blankTok))
	}
}

// A later test of the set finds what an earlier test read: its learn step
// reads no text the set read already (the reason the cache is the set's).
func TestALaterTestFindsWhatAnEarlierTestRead(t *testing.T) {
	set := newTextCache()
	text := `{"data":{"createdAt":"2026-10-22T20:00:00Z","at":"Thu, 22 Oct 2026 11:14:16 GMT"}}`
	x := Raw{text: text, answerClock: []time.Time{time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)}}
	newDriftCacheOn(set).searched(x, false)
	newDriftCacheOn(set).searchedTokens(x, false)
	spans, blanked, blankTok := len(set.spans), len(set.blanked), len(set.blankTok)
	later := newDriftCacheOn(set)
	later.spansOf(text)
	later.searched(x, false)
	later.searchedTokens(x, false)
	if len(set.spans) != spans || len(set.blanked) != blanked || len(set.blankTok) != blankTok {
		t.Fatalf("a later test read again: spans %d -> %d, blanked %d -> %d, blankTok %d -> %d", spans, len(set.spans), blanked, len(set.blanked), blankTok, len(set.blankTok))
	}
	if _, ok := set.blanked[rawKey(x)]; !ok {
		t.Fatal("the payload's blanked texts are not the set's")
	}
}
