package learn

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"
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

// A leaf F served is keyed in each of its forms as a payload holding it is
// in that form (movePlaces, readPlaces): its other declared forms (rawExtra)
// by the words that form writes before its instants, and as many as that
// form holds, so the place F gave the application stands where another
// recording's payload holds the recording's in every form. A key written in
// capitals is case-folded in the other forms ("sentat"), and a value of
// escaped JSON holds its instant once as served and twice more there (its
// escapes read, and those case-folded).
func TestAServedLeafIsKeyedInEachFormByThatFormsOwnWords(t *testing.T) {
	keys := func(places []keyedPlace) []offsetKey {
		out := make([]offsetKey, len(places))
		for i, p := range places {
			out[i] = p.k
			out[i].path = ""
		}
		return out
	}
	for _, mv := range []servedMove{
		{path: "Res.body.SentAt", in: "Res.body", name: "SentAt", live: "2025-03-14", quoted: true},
		{path: "Res.body.Msg", in: "Res.body", name: "Msg", live: `{\"SentAt\":\"2025-03-14\"}`, quoted: true},
	} {
		dc := NewDriftCache()
		served := keys(dc.movePlaces("m-0", []servedMove{mv}))
		m := getAnswer("m-1", "http://api/x", `{"`+mv.name+`":"`+mv.live+`"}`, eRecAt)
		held := keys(NewDriftCache().readPlaces(RawOf(m, true)))
		if !reflect.DeepEqual(served, held) {
			t.Errorf("%s served as %q is keyed %+v: want as a payload holding it is, %+v", mv.name, mv.live, served, held)
		}
		var own, other []string
		for _, k := range served {
			if k.form == 0 {
				own = append(own, k.label)
			} else {
				other = append(other, k.label)
			}
		}
		if !slices.Equal(own, []string{"SentAt"}) || !slices.Contains(other, "sentat") || len(other) <= len(own) && mv.name == "Msg" {
			t.Errorf("%s served as %q is keyed %q in its own form and %q in its others: want SentAt, and sentat among more", mv.name, mv.live, own, other)
		}
	}
}

// The keyed places of a list of instants after one long word, copied for
// the set to keep (copyPlaces), hold the word once: each place's names are
// equal to those read and none is the text they were read from, every
// place of the list shares one copy of its label, and the held part counts
// the bytes the copies hold (placesBytes), each copy once. A base64 text
// of a JSON list of dates is that list: the instants of its decoded form
// (rawExtra) take its encoded run as their label.
func TestTheCopiedPlacesOfAListAfterALongWordHoldTheWordOnce(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := range 300 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02") + `"`)
	}
	b.WriteString("]")
	m := getAnswer("blob-0", "http://api/blob", `{"blob":"`+base64.StdEncoding.EncodeToString([]byte(b.String()))+`"}`, eRecAt)
	read := NewDriftCache().readPlaces(RawOf(m, true))
	places := slices.Clone(read)
	copyPlaces(places)
	runs, longest := 0, 0
	held := map[*byte]int{}
	for i, p := range places {
		r := read[i]
		if p != r {
			t.Fatalf("place %d was copied as %+v: want %+v", i, p, r)
		}
		for j, name := range [][2]string{{p.k.path, r.k.path}, {p.k.label, r.k.label}, {p.where, r.where}} {
			if name[0] == "" {
				continue
			}
			if unsafe.StringData(name[0]) == unsafe.StringData(name[1]) {
				t.Fatalf("place %d's name %d is the text it was read from: want a copy", i, j)
			}
			held[unsafe.StringData(name[0])] = len(name[0])
		}
		if i == 0 || r.k.label != read[i-1].k.label {
			runs++
		}
		longest = max(longest, len(r.k.label))
	}
	labels := map[*byte]bool{}
	for _, p := range places {
		labels[unsafe.StringData(p.k.label)] = true
	}
	if len(places) < 300 || longest < 1000 || len(labels) != runs {
		t.Errorf("%d places with labels of up to %d bytes hold %d copies of their labels: want 300 or more after a word of 1000 or more, one copy for each of the %d runs of a label", len(places), longest, len(labels), runs)
	}
	want := int64(headerBytes + placeBytes*len(places))
	for _, n := range held {
		want += int64(n)
	}
	if got := placesBytes(places); got != want {
		t.Errorf("the held part counts %d bytes of the copied places: want %d, each copy once", got, want)
	}
}

// The places a set keeps of a recording (setPlaces) and those a recording
// F gave the application earlier keeps (PlacesOf) are copies: none of
// their names is part of the recording's payload, so they keep none of it
// once the set's store lets it go.
func TestTheKeptPlacesOfARecordingHoldNoPartOfItsPayload(t *testing.T) {
	m := getAnswer("sent-0", "http://api/sent", `{"SentAt":"2025-03-14","jobs":["2025-03-15","2025-03-16"]}`, eRecAt)
	inside := func(name, text string) bool {
		p, base := uintptr(unsafe.Pointer(unsafe.StringData(name))), uintptr(unsafe.Pointer(unsafe.StringData(text)))
		return name != "" && p >= base && p < base+uintptr(len(text))
	}
	check := func(who string, places []keyedPlace, text string) {
		t.Helper()
		if len(places) < 3 {
			t.Fatalf("%s keeps %d places of the recording's three instants: want each", who, len(places))
		}
		for _, p := range places {
			for _, name := range []string{p.k.path, p.k.label, p.where} {
				if inside(name, text) {
					t.Errorf("%s keeps %q of a place %+v as part of the recording's payload: want a copy", who, name, p.k)
				}
			}
		}
	}
	x := RawOf(m, true)
	check("PlacesOf", PlacesOf(x).places, x.payload().text)

	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	y := dc.LazyRawOf(m, true, nil)
	places, ok := dc.setPlaces(y)
	if !ok {
		t.Fatal("the set keeps no places of its recording: want them kept (setPlaces)")
	}
	check("the set", places, y.payload().text)
}
