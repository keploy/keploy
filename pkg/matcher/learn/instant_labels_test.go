package learn

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

// instantLabelBefore is the word written before the i-th of spans as the
// learn step read it before instantLabels, one instant at a time, kept as it
// was: what instantLabels is held to.
func instantLabelBefore(t string, spans []instant, i int) string {
	j := spans[i].start - 1
	for j >= 0 {
		in := false
		for _, sp := range spans {
			if sp.start <= j && j < sp.end {
				j, in = sp.start-1, true
				break
			}
		}
		if in {
			continue
		}
		if !isWord(t[j]) {
			j--
			continue
		}
		end := j + 1
		for j >= 0 && isWord(t[j]) {
			j--
		}
		return t[j+1 : end]
	}
	return ""
}

// sameLabels fails t where instantLabels of s's instants (instantSpans) are
// not each instant's label as instantLabelBefore reads it, or where those
// instants are not in order, none overlapping, as instantLabels reads them.
func sameLabels(t *testing.T, s string) {
	t.Helper()
	spans := instantSpans(s)
	for k := 1; k < len(spans); k++ {
		if spans[k].start < spans[k-1].end {
			t.Fatalf("the instants of %q are %+v: want them in order, none overlapping", s, spans)
		}
	}
	got := instantLabels(s, spans)
	want := make([]string, len(spans))
	for i := range spans {
		want[i] = instantLabelBefore(s, spans, i)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the labels of the instants of %q are %q: one at a time they are %q", s, got, want)
	}
}

// The labels of a text's instants read in one pass (instantLabels) are those
// read one instant at a time (instantLabelBefore): a word before an
// instant, a list's word for each of its instants, none before the first,
// words between instants, keys, UUIDs whose time groups are an instant, and
// instants of every layout side by side.
func TestInstantLabelsAreEachInstantsLabel(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{`{"created_at":"2026-10-24T09:55:53Z"}`, []string{"at"}},
		{`{"jobs":["2026-10-24","2026-10-25","2026-10-26"]}`, []string{"jobs", "jobs", "jobs"}},
		{`["15:04:05","15:04:06"]`, []string{"", ""}},
		{`2026-10-24T09:55:53Z`, []string{""}},
		{`at 2026-10-24 then 2026-10-25, 2026-10-26`, []string{"at", "then", "then"}},
		{`{"a":{"since":"2026-10-24","until":"2026-10-25"},"ids":[1792656120,1792656121]}`, []string{"since", "until", "ids", "ids"}},
		{`x2026-10-24 2026-10-25`, nil},
		{"no instant here", nil},
		{"", nil},
	} {
		spans := instantSpans(c.text)
		got := instantLabels(c.text, spans)
		if c.want != nil && !reflect.DeepEqual(got, c.want) {
			t.Errorf("the labels of the instants of %q are %q: want %q", c.text, got, c.want)
		}
		sameLabels(t, c.text)
	}
	at := time.Date(2026, 10, 24, 9, 55, 53, 0, time.UTC)
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, time.RFC1123, time.RFC1123Z, "2006-01-02", "15:04:05", "2006-01-02 15:04:05", time.ANSIC} {
		var b strings.Builder
		b.WriteString(`{"list":[`)
		for i := range 40 {
			if i > 0 {
				b.WriteString(", ")
			}
			if i%7 == 3 {
				fmt.Fprintf(&b, `{"k%d":`, i)
			}
			b.WriteString(`"` + at.Add(time.Duration(i)*time.Hour).Format(layout) + `"`)
			if i%7 == 3 {
				b.WriteString("}")
			}
		}
		b.WriteString(`],"uuid":"1ef8c1a2-5b3c-11ef-8def-0242ac120002","v7":"01928f3e-5b3c-7abc-8def-0242ac120002","end":1792656120}`)
		sameLabels(t, b.String())
	}
}

// The same over random texts of instants of every layout, words, numbers,
// UUIDs and the punctuation between them, and over random bytes: 4 000 of
// each from a fixed seed.
func TestInstantLabelsAreEachInstantsLabelOverRandomTexts(t *testing.T) {
	at := time.Date(2026, 10, 24, 9, 55, 53, 0, time.UTC)
	r := rand.New(rand.NewSource(20261103))
	layouts := []string{time.RFC3339, time.RFC3339Nano, time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC, "2006-01-02", "15:04:05", "2006-01-02 15:04:05", "02/01/2006", "20060102"}
	pieces := []func() string{
		func() string {
			return at.Add(time.Duration(r.Int63n(1 << 50))).Format(layouts[r.Intn(len(layouts))])
		},
		func() string { return fmt.Sprint(1792656120 + r.Intn(1<<20)) },
		func() string { return fmt.Sprint(int64(1792656120000) + r.Int63n(1<<30)) },
		func() string {
			return []string{"at", "jobs", "created_at", "x", "id", "w7x1", "Z", "T", "a1b2"}[r.Intn(9)]
		},
		func() string {
			return []string{"1ef8c1a2-5b3c-11ef-8def-0242ac120002", "01928f3e-5b3c-7abc-8def-0242ac120002", "c0ffee00-1234-4abc-8def-000000000001"}[r.Intn(3)]
		},
		func() string { return string(`",:[]{} -_./`[r.Intn(12)]) },
		func() string { return strings.Repeat(" ", r.Intn(3)) },
	}
	for range 4000 {
		var b strings.Builder
		for range r.Intn(24) {
			b.WriteString(pieces[r.Intn(len(pieces))]())
		}
		sameLabels(t, b.String())
		raw := make([]byte, r.Intn(64))
		for i := range raw {
			raw[i] = "0123456789-:TZ ,\"abc+."[r.Intn(22)]
		}
		sameLabels(t, string(raw))
	}
}

// FuzzInstantLabels holds instantLabels to instantLabelBefore for any text.
func FuzzInstantLabels(f *testing.F) {
	for _, s := range []string{
		`{"jobs":["2026-10-24","2026-10-25"]}`,
		`at 2026-10-24T09:55:53Z, then 15:04:05 and 1792656120`,
		`["15:04:05","15:04:06","15:04:07"]`,
		`1ef8c1a2-5b3c-11ef-8def-0242ac120002 2026-10-24`,
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		sameLabels(t, s)
	})
}

// A field holding a list of thousands of instants has its keyed places read
// (readPlaces) with each instant's label the list's word, every one of them:
// 2 884 times of day, the list the staging took 26 s of CPU to read the
// places of before (instantLabels).
func TestTheKeyedPlacesOfALongListOfInstantsTakeTheListsWord(t *testing.T) {
	at := time.Date(2026, 10, 24, 9, 55, 53, 0, time.UTC)
	var b strings.Builder
	b.WriteString(`{"seen":[`)
	const n = 2884
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + at.Add(time.Duration(i)*time.Second).Format("15:04:05") + `"`)
	}
	b.WriteString(`]}`)
	m := getAnswer("seen-0", "http://api/seen", b.String(), eRecAt)
	dc := NewDriftCache()
	places := dc.readPlaces(RawOf(m, true))
	seen := 0
	for _, p := range places {
		if p.k.path == "Res.body" && p.k.form == 0 {
			if p.k.label != "seen" {
				t.Fatalf("an instant of the list is keyed %+v: want the list's word, seen", p.k)
			}
			seen++
		}
	}
	if seen != n {
		t.Errorf("%d of the list's %d instants were keyed in its field: want all", seen, n)
	}
}
