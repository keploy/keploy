package learn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/variables"
)

// aUUID is the k-th id of a prompt's run (0 the recording, 1 round F), a
// random (version 4) UUID, as an application mints one, the same for the
// same k and run.
func aUUID(run, k int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("anchors/%d/%d", run, k)))
	h[6], h[8] = h[6]&0x0f|0x40, h[8]&0x3f|0x80
	s := hex.EncodeToString(h[:16])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// aPrompt is an LLM prompt of the shape an application builds from a test
// suite it ran (synthetic: a suite of steps, each with its ids, headers
// and request line, then the run's time, then the instructions), with the
// ids of run (aUUID: 22 ids, 4 of the suite and 2 of each of 9 steps) and
// the time at. named: each step's name and body hold a word no other place
// holds (an anchor, anchorsOf); otherwise every step reads alike but for
// its ids. extra, when set, is a header line step 5 alone sends.
func aPrompt(run int, at time.Time, named bool, extra string) string {
	names := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india"}
	var b strings.Builder
	fmt.Fprintf(&b, "Suite (JSON):\n{\n  \"id\": %q,\n  \"app_id\": %q,\n  \"job_id\": %q,\n  \"run_id\": %q,\n", aUUID(run, 0), aUUID(run, 1), aUUID(run, 2), aUUID(run, 3))
	b.WriteString("  \"name\": \"Create_Widget_With_Existing_Sku\",\n  \"description\": \"Attempt to create a widget whose sku is already taken.\",\n  \"steps\": [\n")
	for k, n := range names {
		name, sku := "Create the next widget", "w"
		if named {
			name, sku = "Create the "+n+" widget", fmt.Sprintf("w-%d", 101+k)
		}
		step, req, suite := aUUID(run, 4+2*k), aUUID(run, 5+2*k), aUUID(run, 0)
		fmt.Fprintf(&b, "    {\n      \"id\": %q,\n      \"name\": %q,\n      \"method\": \"POST\",\n      \"url\": \"/widgets\",\n", step, name)
		fmt.Fprintf(&b, "      \"body\": \"{\\\"sku\\\": \\\"%s\\\", \\\"count\\\": 3}\",\n      \"headers\": {\n", sku)
		fmt.Fprintf(&b, "        \"Authorization\": \"***redacted***\",\n        \"Content-Type\": \"application/json\",\n        \"Keploy-Test-Name\": %q,\n        \"X-Keploy-Test-Step-ID\": %q,\n        \"X-Keploy-Test-Suite-ID\": %q\n      },\n", step, step, suite)
		hdr := ""
		if k == 5 {
			hdr = extra
		}
		fmt.Fprintf(&b, "      \"req_id\": %q,\n      \"request_headers\": \"POST /widgets HTTP/1.1\\r\\nHost: localhost:4000\\r\\nUser-Agent: Go-http-client/1.1\\r\\nContent-Length: 42\\r\\nAuthorization: ***redacted***\\r\\nContent-Type: application/json\\r\\n%sKeploy-Test-Name: %s\\r\\nX-Keploy-Test-Step-Id: %s\\r\\nX-Keploy-Test-Suite-Id: %s\\r\\nAccept-Encoding: gzip\\r\\n\\r\\n\"\n    }", req, hdr, step, step, suite)
		if k < len(names)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "  ],\n  \"created_at\": %d\n}\n\n", at.Unix())
	for i := 0; i < 40; i++ {
		b.WriteString("Write a report of the run above: name each step that failed, say why it failed, and what to check next.\n")
	}
	return b.String()
}

// The prompt an application builds from the run's own data holds the
// run's ids and the run's time among many words that stay: 22 ids (4 of
// the suite, 2 of each of 9 steps), each standing several times, and one
// time. Between the words both runs begin and end with, the words of the
// two runs compare in a table far past maxMovedCells, which was read as
// every word there moved: no word stayed paired, and the reader refused
// the prompt as changed in shape. Cut at the words each run holds once and
// in the same order (anchorsOf, a patience diff's anchors), each stretch
// between two anchors compares within the bound: every unchanged word
// pairs and stays checked, and the changed words are the ids' groups and
// the time, nothing else. Decided, every id group is learned (fresh at its
// first place, bound there after), the time is decided as one word by the
// clock's rules, and no word else is decided: every other word is checked
// as recorded.
func TestReaderAPromptOfARunsIdsPairsEveryWordThatStayed(t *testing.T) {
	rAt, fAt := oAt, oAt.Add(73*time.Minute)
	rTime, fTime := fmt.Sprint(rAt.Add(-time.Second).Unix()), fmt.Sprint(fAt.Add(-time.Second).Unix())
	rec, live := aPrompt(0, rAt.Add(-time.Second), true, ""), aPrompt(1, fAt.Add(-time.Second), true, "")
	rw, lw := textsOf(rec, variables.Words(rec)), textsOf(live, variables.Words(live))
	if len(rw) != len(lw) {
		t.Fatalf("fixture: %d words recorded, %d in round F", len(rw), len(lw))
	}
	ids := map[string]bool{rTime: true, fTime: true}
	for k := 0; k < 22; k++ {
		for run := 0; run < 2; run++ {
			for _, g := range strings.Split(aUUID(run, k), "-") {
				ids[g] = true
			}
		}
	}
	first, last, differ := -1, 0, 0
	for i := range rw {
		if rw[i] != lw[i] {
			if !ids[rw[i]] || !ids[lw[i]] {
				t.Fatalf("fixture: word %d %q -> %q is no id group or time", i, rw[i], lw[i])
			}
			if first < 0 {
				first = i
			}
			last, differ = i, differ+1
		} else if ids[rw[i]] {
			t.Fatalf("fixture: id group %q stands in both runs at word %d", rw[i], i)
		}
	}
	if mid := last - first + 1; mid*mid <= maxMovedCells {
		t.Fatalf("fixture: the changed middle (%d words a side) is within the bound %d", mid, maxMovedCells)
	}
	rd := readText(rec, live)
	if rd.ChangedShape {
		t.Fatalf("the prompt reads as changed in shape; want every word that stayed paired")
	}
	if len(rd.Words) != differ {
		t.Errorf("the reader read %d changed words; want the %d id groups and the time alone", len(rd.Words), differ)
	}
	for _, w := range rd.Words {
		if w.Rec == w.Live || !ids[w.Rec] || !ids[w.Live] || w.RecIdx != w.LiveIdx {
			t.Errorf("changed word %q (word %d) -> %q (word %d): want an id group or the time, at its own place in each run", w.Rec, w.RecIdx, w.Live, w.LiveIdx)
		}
	}
	in := oInput("body.messages.1.content", rec, live, nil, nil)
	in.Window = iWin(rAt, fAt, 5*time.Second)
	ds := Decide(in)
	if len(ds) != differ {
		t.Errorf("%d decisions; want one per changed word (%d): %s", len(ds), differ, oDescribe(ds[:min(len(ds), 6)]))
	}
	times := 0
	for _, d := range ds {
		switch {
		case d.Recorded == rTime && d.Full == fTime:
			times++
		case !ids[d.Recorded] || !ids[d.Full] || d.Outcome != OutcomeFresh && d.Outcome != OutcomeValue:
			t.Errorf("FALSE GREEN or refusal? %s; want each id group learned, nothing else", oDescribe([]Decision{d}))
		}
	}
	if times != 1 {
		t.Errorf("the time decided %d times; want once, as one word", times)
	}
}

// A stretch between two anchors (or the text's bounds) still past
// maxMovedCells reads as before: every word in it between its own common
// start and end has moved, and no word there pairs. A prompt whose steps
// read alike but for their ids holds no anchor among them; round F adds a
// header line in one step, so that stretch is of unequal counts and the
// prompt stays refused as changed in shape, as it was with no anchors at
// all. The same added line among steps that each hold an anchor is read
// within the bound: every other word of the steps pairs, and the line
// alone stands in one run, a changed shape, refused too.
func TestReaderAStretchPastTheBoundBetweenAnchorsStaysRefused(t *testing.T) {
	rAt, fAt := oAt, oAt.Add(73*time.Minute)
	const extra = `X-Retry-Attempt: 2\r\n`
	for _, named := range []bool{false, true} {
		rec, live := aPrompt(0, rAt.Add(-time.Second), named, ""), aPrompt(1, fAt.Add(-time.Second), named, extra)
		rw, lw := textsOf(rec, variables.Words(rec)), textsOf(live, variables.Words(live))
		// From the first step's id to the suite's id in the last step's
		// request line.
		from, to := slices.Index(rw, strings.Split(aUUID(0, 4), "-")[0]), 0
		for i, w := range rw {
			if w == strings.Split(aUUID(0, 0), "-")[4] {
				to = i
			}
		}
		groups := map[string]bool{}
		for k := 0; k < 22; k++ {
			for _, g := range strings.Split(aUUID(0, k), "-") {
				groups[g] = true
			}
		}
		inSteps, stayed := 0, 0
		for _, k := range keptWords(rw, lw) {
			if k[0] >= from && k[0] <= to {
				inSteps++
			}
		}
		for _, w := range rw[from : to+1] {
			if !groups[w] {
				stayed++
			}
		}
		// Of the words that stayed, the added line's neighbour ("r" of its
		// "\r\n") pairs either way, so neither way keeps it.
		switch {
		case !named && inSteps != 0:
			t.Errorf("steps alike, no anchor among them: %d words of the steps kept; want none (the stretch is past the bound)", inSteps)
		case named && inSteps < stayed-1:
			t.Errorf("steps each with an anchor: %d words of the steps kept; want the %d that stayed, but the added line's neighbour", inSteps, stayed)
		}
		in := oInput("body.messages.1.content", rec, live, nil, nil)
		in.Window = iWin(rAt, fAt, 5*time.Second)
		ds := Decide(in)
		rd := readText(rec, live)
		if !rd.ChangedShape && !slices.ContainsFunc(rd.Stretches, func(s wordStretch) bool { return s.Shape == stretchUnequal }) || !slices.ContainsFunc(ds, func(d Decision) bool {
			return d.Refusal != nil && d.Refusal.Kind == RefusalChangedShape
		}) || slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome == OutcomeFresh && d.At.Word < 0 }) {
			t.Errorf("FALSE GREEN? named %v: a header line round F alone sends: %s; want it refused as changed in shape", named, oDescribe(ds[:min(len(ds), 4)]))
		}
	}
}

// anchorsOf keeps a pair only where each list holds the word once and
// every longest chain of such pairs in order holds it: two words that
// swapped are neither, nor is a pair one longest chain holds and another
// skips, nor a word one list holds twice.
func TestAnchorsAreWordsEachRunHoldsOnceInOneOrder(t *testing.T) {
	for _, c := range []struct {
		name    string
		r, l    string
		anchors [][2]int
	}{
		{"each word once, in order", "a b c", "a b c", [][2]int{{0, 0}, {1, 1}, {2, 2}}},
		{"a word added between", "a b c", "a x b c", [][2]int{{0, 0}, {1, 2}, {2, 3}}},
		{"two words swapped", "a b c d", "a c b d", [][2]int{{0, 0}, {3, 3}}},
		{"a word one run holds twice", "a b c", "a b b c", [][2]int{{0, 0}, {2, 3}}},
		{"a word the recording holds twice", "a b b c", "a b c", [][2]int{{0, 0}, {3, 2}}},
		// Only its second place can follow a and b, so a chain would take
		// it: it is still no anchor, since the other place could be its pair.
		{"a word the recording holds twice, one place in order", "x a b x", "a b x", [][2]int{{1, 0}, {2, 1}}},
		{"a word one run lacks", "a b c", "a c", [][2]int{{0, 0}, {2, 1}}},
		{"two chains as long", "a b c d e", "a d e b c", [][2]int{{0, 0}}},
		{"one chain longer", "a b c d e f", "a d b c e f", [][2]int{{0, 0}, {1, 2}, {2, 3}, {4, 4}, {5, 5}}},
		// The recording's last word stands first in the replay: its chain
		// is the shortest, and the longest is the one before it.
		{"the last word moved to the front", "b c d a", "a b c d", [][2]int{{0, 1}, {1, 2}, {2, 3}}},
		{"no word once in each", "a a b b", "b b a a", nil},
		{"the last pair alone", "x y z", "z", [][2]int{{2, 0}}},
	} {
		got := anchorsOf(strings.Fields(c.r), strings.Fields(c.l))
		if fmt.Sprint(got) != fmt.Sprint(c.anchors) {
			t.Errorf("%s: anchors %v; want %v", c.name, got, c.anchors)
		}
	}
}

// Past maxMovedCells, the work of lining two word lists up grows with
// their length, not its square, where anchors cut them into stretches of a
// bounded size: of blocks that each hold an anchor and an id that changed,
// each stretch compares its id's 5 groups with the other run's alone (its
// own common start and end set aside), 25 cells a block in the tables
// keptWordsIn and movedWordsIn compare in, where the whole would take the
// blocks' words squared. A count of cells, never a clock.
func TestAnchoredTablesGrowWithTheTextsLength(t *testing.T) {
	// Each block's id, in each run, shares no group with any other id: a
	// group both runs hold once is an anchor of its own (anchorsOf), and
	// one that crossed blocks would cost the blocks between it an anchor.
	seen, ids, seq := map[string]bool{}, [2][]string{}, 0
	for len(ids[1]) < 320 {
		var pair [2]string
		fresh, own := true, map[string]bool{}
		for run := range pair {
			pair[run] = aUUID(run, 1000+seq)
			for _, g := range strings.Split(pair[run], "-") {
				fresh = fresh && !seen[g] && !own[g]
				own[g] = true
			}
		}
		if seq++; !fresh {
			continue
		}
		for run, id := range pair {
			for _, g := range strings.Split(id, "-") {
				seen[g] = true
			}
			ids[run] = append(ids[run], id)
		}
	}
	block := func(run, k int) []string {
		words := []string{"step", fmt.Sprintf("s%d", k), "id"}
		for i := 0; i < 30; i++ {
			words = append(words, "header", "value")
		}
		return append(append(words, strings.Split(ids[run][k], "-")...), "end")
	}
	text := func(run, n int) []string {
		var out []string
		for k := 0; k < n; k++ {
			out = append(out, block(run, k)...)
		}
		return out
	}
	for _, n := range []int{10, 20, 40, 80, 160, 320} {
		r, l := text(0, n), text(1, n)
		if len(r)*len(l) <= maxMovedCells {
			t.Fatalf("fixture: %d blocks are within the bound", n)
		}
		kept, kc := keptWordsIn(r, l, true)
		mr, ml, mc := movedWordsIn(r, l, true)
		if want := len(r) - 5*n; len(kept) != want || len(mr) != 5*n || len(ml) != 5*n {
			t.Errorf("%d blocks: kept %d, moved %d and %d; want %d kept and each id's 5 groups moved", n, len(kept), len(mr), len(ml), want)
		}
		if kc != 25*n || mc != 25*n {
			t.Errorf("%d blocks (%d words a side): the tables compared %d and %d cells; want 25 a block, %d", n, len(r), kc, mc, 25*n)
		}
	}
}

// A stretch between two anchors still past maxMovedCells is not cut again
// at the words it alone holds once: those stand elsewhere between the
// text's common start and end, so they are no anchor there, and the
// stretch reads as the whole did before, every word between its own common
// start and end moved. Here 600 words stand once before the anchor "mid"
// in each run, after a word that changed, and again after it, the second
// time shifted by a word round F adds: before "mid" the 600 pair as the
// stretch's common end; after it, past the bound, nothing pairs.
func TestAStretchPastTheBoundIsNotCutAgain(t *testing.T) {
	const n = 600
	var words []string
	for i := 0; i < n; i++ {
		words = append(words, fmt.Sprintf("y%d", i))
	}
	rec := append(append(append([]string{"a", "c1"}, words...), "mid"), words...)
	live := append(append(append([]string{"a", "c2"}, words...), "mid", "new"), words[:n-1]...)
	rec, live = append(rec, "z"), append(live, "z")
	kept := keptWords(rec, live)
	want := [][2]int{{0, 0}}
	for i := 2; i < n+3; i++ {
		want = append(want, [2]int{i, i})
	}
	if want = append(want, [2]int{len(rec) - 1, len(live) - 1}); fmt.Sprint(kept) != fmt.Sprint(want) {
		t.Errorf("kept %d pairs (%v...); want the 600 words before \"mid\", \"mid\", and the common start and end (%d), none after \"mid\"", len(kept), kept[:min(len(kept), 4)], len(want))
	}
	if mr, ml := movedWords(rec, live); len(mr) != n+1 || len(ml) != n+1 {
		t.Errorf("moved %d and %d words; want the changed word and the %d after \"mid\" a side", len(mr), len(ml), n)
	}
	// Within the bound, the lists' longest common subsequence decides, not
	// their anchors: "q" stands once in each, but the three "x" outweigh it.
	if kept := keptWords(strings.Fields("q x x x"), strings.Fields("x x x q")); fmt.Sprint(kept) != "[[1 0] [2 1] [3 2]]" {
		t.Errorf("within the bound: kept %v; want the three x", kept)
	}
	if mr, ml := movedWords(strings.Fields("q x x x"), strings.Fields("x x x q")); fmt.Sprint(mr, ml) != "[q] [q]" {
		t.Errorf("within the bound: moved %v and %v; want q alone", mr, ml)
	}
}
