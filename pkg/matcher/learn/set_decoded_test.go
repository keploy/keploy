package learn

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countDecodes counts, by payload text, the decodes (decodeForms) of each of
// raw's payloads until the row ends; hold, where not nil, is called before
// each, with its text.
func countDecodes(t *testing.T, raw []Raw, hold func(text string)) *sync.Map {
	texts := map[string]bool{}
	for _, x := range raw {
		texts[x.payload().text] = true
	}
	dec := decodeForms
	t.Cleanup(func() { decodeForms = dec })
	var counts sync.Map
	decodeForms = func(text string) decoded {
		if texts[text] {
			if hold != nil {
				hold(text)
			}
			n, _ := counts.LoadOrStore(text, new(atomic.Int64))
			n.(*atomic.Int64).Add(1)
		}
		return dec(text)
	}
	return &counts
}

func decodesOf(counts *sync.Map) (payloads, decodes int64) {
	counts.Range(func(_, n any) bool {
		payloads++
		decodes += n.(*atomic.Int64).Load()
		return true
	})
	return payloads, decodes
}

// Each payload of the set is decoded (decodeForms: its escapes, its
// quoted-printable form and its encoded runs, the learn step's dearest reads
// of a payload) and read for its instants (instantSpans) once for the set,
// not once per call: each later test's calls read the set's of them, even
// where the set let go of its words and texts (derivedBudget) and builds
// them again. On api-server's round, decoding a payload again each time its
// words were let go was 30% of the agent's CPU, and reading its instants
// again in each test 75% before the test that timed out.
func TestEachPayloadOfTheSetIsDecodedAndReadForItsInstantsOnceNotOncePerCall(t *testing.T) {
	const n, tests = 48, 6
	words := derivedBudget
	derivedBudget = 1 << 10
	t.Cleanup(func() { derivedBudget = words })
	row := newHeldRow(n, 4)
	raw := row.raw()
	counts := countDecodes(t, raw, nil)
	spans := countSpans(t)
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	first := row.classify(tr.Drifts())
	tr.End()
	payloads, decodes := decodesOf(counts)
	read := spans.Load()
	if payloads != n || decodes != n {
		t.Fatalf("the set's first call decoded %d of its %d payloads, %d times in all: want each once", payloads, n, decodes)
	}
	for i := 0; i < tests; i++ {
		tr.Begin()
		got := row.classify(tr.Drifts())
		tr.End()
		if fmt.Sprint(got) != fmt.Sprint(first) {
			t.Fatalf("test %d of the set learned\n%v\nwhere its first learned\n%v", i+2, got, first)
		}
	}
	if _, d := decodesOf(counts); d != decodes {
		t.Errorf("%d more tests of the set decoded its %d payloads %d more times: want none (once for the set)", tests, n, d-decodes)
	}
	if r := spans.Load(); r != read {
		t.Errorf("%d more tests of the set read its payloads' instants %d more times: want none (once for the set)", tests, r-read)
	}
	// The word rule's words of each payload (searchedTokens), and the search
	// for the payload a drifted instant was recorded in (instantRawFrom) of
	// a moment none holds, which reads each: in each test, from the set's.
	none := moment{key: instantKey{at: 1}}
	for i := 0; i <= tests; i++ {
		tr.Begin()
		dc := tr.Drifts()
		for _, x := range raw {
			dc.searchedTokens(x, false)
			dc.searchedTokens(x, true)
		}
		if p, ok := dc.instantRawFrom(Leaf{}, none, eRecAt.Add(time.Hour), raw); ok {
			t.Fatalf("a moment no payload holds was found in %v", p)
		}
		tr.End()
		if i == 0 {
			_, decodes = decodesOf(counts)
			read = spans.Load()
		}
	}
	if _, d := decodesOf(counts); d != decodes {
		t.Errorf("%d tests of the set reading its payloads' words decoded them %d more times than the first: want none (once for the set)", tests, d-decodes)
	}
	if r := spans.Load(); r != read {
		t.Errorf("%d tests of the set searching its payloads for a moment read their instants %d more times than the first: want none (once for the set)", tests, r-read)
	}
}

// Handlers that need the set's payloads at once do not wait, each in turn,
// behind the one reading the first: while one reads a payload, another
// reads the others (warm), and none waits for a read while a payload is
// left that no handler has taken. On api-server's round, 20 of 24 Mongo
// handlers waited behind one handler decoding one payload, the agent used
// 1.4 of its 8 cores, and five tests timed out. Counted, not timed: the
// first payload's read is held until every other payload is read, or until
// a handler waits while a payload is left that no handler has taken (the
// wait the row refuses).
func TestTheSetsHandlersDoNotWaitInTurnBehindOnePayloadsRead(t *testing.T) {
	const n, handlers = 24, 4
	row := newHeldRow(n, 4)
	raw := row.raw()
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	dc.appClock = true
	held := raw[0].payload().text
	var others atomic.Int64
	var waitedFirst atomic.Bool
	counts := countDecodes(t, raw, func(text string) {
		if text != held {
			others.Add(1)
			return
		}
		// Held until each other payload is read, or a handler waits while
		// one is left that no handler has taken.
		s := dc.set
		for others.Load() < n-1 {
			s.mu.Lock()
			waits, taken := s.waits, len(s.warming)
			s.mu.Unlock()
			if waits > 0 && taken < n {
				waitedFirst.Store(true)
				return
			}
			runtime.Gosched()
		}
	})
	var wg sync.WaitGroup
	got := make([][]Learned, handlers)
	for h := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[h] = row.run(dc)
		}()
	}
	wg.Wait()
	if waitedFirst.Load() {
		t.Fatalf("a handler waited for a payload another was reading while some of the set's %d payloads were left that no handler had taken: the handlers read in turn behind one reader", n)
	}
	if payloads, decodes := decodesOf(counts); payloads != n || decodes != n {
		t.Errorf("%d handlers at once decoded %d of the set's %d payloads, %d times in all: want each once", handlers, payloads, n, decodes)
	}
	for h := 1; h < handlers; h++ {
		if fmt.Sprint(got[h]) != fmt.Sprint(got[0]) {
			t.Errorf("handler %d learned\n%v\nwhere handler 0 learned\n%v", h, got[h], got[0])
		}
	}
}

// A payload's forms read from what the set keeps of its decoding (formsOf)
// are its forms decoded now (textForms), in every reading: escapes,
// quoted-printable, base64 inside base64, and the case-foldings of each.
func TestAPayloadsFormsReadFromItsDecodingAreItsForms(t *testing.T) {
	for _, text := range []string{
		"",
		"plain words only",
		`{"Name":"Alice","path":"uploads%2Fa7k29q"}`,
		`{"msg":"line\nbreak été","q":"caf=C3=A9 =3D x"}`,
		"token=" + "ZXlKaGJHY2lPaUpJVXpJMU5pSjk=" + " Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyLTQyIn0.sig",
		"MixedCase 0xDEADBEEF and MFRGGZDFMZTWQ2LK",
	} {
		want := formsDecodedNow(text)
		d := decodeForms(text).clone()
		if got := d.formsOf(text); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("forms of %q read from its decoding:\n%q\nwant\n%q", text, got, want)
		}
		if got, want := d.extraOf(text), strings.Join(want[1:], "\n"); got != want {
			t.Errorf("other forms of %q read from its decoding: %q, want %q", text, got, want)
		}
	}
}

// formsDecodedNow are text's forms as textForms read them before a set kept
// their decoding: each decoded now, beside its case-folding.
func formsDecodedNow(text string) []string {
	out := []string{text}
	dec := unescaped(text)
	if dec != text {
		out = append(out, dec)
	}
	if low := strings.ToLower(dec); low != dec {
		out = append(out, low)
	}
	if q := qpForm(text); q != dec {
		out = append(out, q)
		if low := strings.ToLower(q); low != q {
			out = append(out, low)
		}
	}
	if b := b64Text(dec, nil); b != "" {
		out = append(out, b)
		if low := strings.ToLower(b); low != b {
			out = append(out, low)
		}
	}
	return out
}
