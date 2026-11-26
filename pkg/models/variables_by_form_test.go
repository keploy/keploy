package models

import "testing"

// A use's check by form (MockVariable.ByForm) survives a merge only where no
// learning checks that use exactly: a use one learning alone names keeps its
// check, one both name keeps it where both learned it in one form, and is
// checked exactly otherwise. Cloning shares no map.
func TestAUsesCheckByFormHoldsOnlyWhereNoLearningChecksItExactly(t *testing.T) {
	const at, other = "body.createdAt", "body.updatedAt"
	text := ClockOffset{Form: "text 9999-99-99a99:99:99a"}
	number := ClockOffset{Form: "number 9999999999"}
	for _, tc := range []struct {
		what       string
		aUse, bUse []string
		aBy, bBy   map[string]ClockOffset
		want       map[string]ClockOffset
	}{
		{what: "both check it in one form", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{at}, bBy: map[string]ClockOffset{at: text}, want: map[string]ClockOffset{at: text}},
		{what: "a checks it, b exactly", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{at}},
		{what: "a exactly, b checks it", aUse: []string{at}, bUse: []string{at}, bBy: map[string]ClockOffset{at: text}},
		{what: "two forms", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{at}, bBy: map[string]ClockOffset{at: number}},
		{what: "a alone names it", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{other}, want: map[string]ClockOffset{at: text}},
		{what: "b alone names it", aUse: []string{other}, bUse: []string{at}, bBy: map[string]ClockOffset{at: number}, want: map[string]ClockOffset{at: number}},
		{what: "neither checks one", aUse: []string{at}, bUse: []string{at}},
		{what: "each alone names one", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{other}, bBy: map[string]ClockOffset{other: number}, want: map[string]ClockOffset{at: text, other: number}},
		{what: "both check two in one form", aUse: []string{at, other}, aBy: map[string]ClockOffset{at: text, other: number}, bUse: []string{at, other}, bBy: map[string]ClockOffset{at: text, other: number}, want: map[string]ClockOffset{at: text, other: number}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got := MergeByForm(tc.aUse, tc.aBy, tc.bUse, tc.bBy)
			if len(got) != len(tc.want) {
				t.Fatalf("MergeByForm = %v; want %v", got, tc.want)
			}
			for p, o := range tc.want {
				if got[p] != o {
					t.Fatalf("MergeByForm = %v; want %v", got, tc.want)
				}
			}
			a := MockVariables{"v": {Recorded: "r", Use: tc.aUse, ByForm: tc.aBy}}
			b := MockVariables{"v": {Recorded: "r", Use: tc.bUse, ByForm: tc.bBy}}
			merged := MergeVariables(a, b)["v"].ByForm
			if len(merged) != len(tc.want) {
				t.Fatalf("MergeVariables keeps %v; want %v", merged, tc.want)
			}
		})
	}
	a := MockVariables{"v": {Recorded: "r", Use: []string{at}, ByForm: map[string]ClockOffset{at: text}}}
	c := a.Clone()
	c["v"].ByForm[at] = number
	if a["v"].ByForm[at] != text {
		t.Fatal("a clone's check by form is the original's map")
	}
}

func TestCanonicalTimeFormAndClockOffsetOffsetEquivalence(t *testing.T) {
	const canon = "text 9999-99-99a99:99:99a"
	for _, tc := range []struct {
		in, want string
	}{
		{"text 9999-99-99a99:99:99a", canon},
		{"text 9999-99-99a99:99:99+99:99", canon},
		{"text 9999-99-99a99:99:99-99:99", canon},
		{"text 9999-99-99a99:99:99+9999", canon},
		{"text 9999-99-99a99:99:99-9999", canon},
		{"text 9999-99-99a99:99:99+99", canon},
		{"text 9999-99-99a99:99:99-99", canon},
		{"typed 9999-99-99a99:99:99+99:99", "typed 9999-99-99a99:99:99a"},
		{"text 9999-99-99 99:99:99+99:99", "text 9999-99-99 99:99:99a"},
		{"text 9999-99-99 99:99:99 +99:99", "text 9999-99-99 99:99:99 a"},
		{"text 9999-99-99 99:99:99 -9999", "text 9999-99-99 99:99:99 a"},
		{"text 9999-99-99a99:99+99:99", "text 9999-99-99a99:99a"},
		{"text 9999-99-99a99+99:99", "text 9999-99-99a99a"},
		{"text 9999-99-99+99:99", "text 9999-99-99a"},
		{"text 9999-99-99-99:99", "text 9999-99-99a"},
		{"text 9999-99-99+9999", "text 9999-99-99a"},
		// Unchanged non-offset / non-ISO forms.
		{"text 9999-99-99a99:99:99", "text 9999-99-99a99:99:99"},
		{"text 9999-99-99", "text 9999-99-99"},
		{"text 9999-99-99 +99:99", "text 9999-99-99 +99:99"},
		{"text 9999-99-99a99:99:99 aaa", "text 9999-99-99a99:99:99 aaa"},
		{"text aaa, 99 aaa 9999 99:99:99 +9999", "text aaa, 99 aaa 9999 99:99:99 +9999"},
		{"number 9999999999", "number 9999999999"},
		{FormApart, FormApart},
		{"", ""},
	} {
		if got := CanonicalTimeForm(tc.in); got != tc.want {
			t.Errorf("CanonicalTimeForm(%q) = %q; want %q", tc.in, got, tc.want)
		}
		if got := CanonicalTimeForm(tc.want); got != tc.want {
			t.Errorf("CanonicalTimeForm not idempotent on %q: got %q", tc.want, got)
		}
	}

	// FormsOf preserves raw forms on disk (so prior releases pass either zone).
	const rawIST = "text 9999-99-99a99:99:99+99:99"
	const rawEST = "text 9999-99-99a99:99:99-99:99"
	if f, ff := FormsOf(rawIST, canon); f != rawIST || ff != canon {
		t.Fatalf("FormsOf(+99:99, a) = (%q, %q); want raw (%q, %q)", f, ff, rawIST, canon)
	}
	if f, ff := FormsOf(rawIST, rawIST); f != rawIST || ff != "" {
		t.Fatalf("FormsOf(+99:99, +99:99) = (%q, %q); want (%q, \"\")", f, ff, rawIST)
	}

	// WrittenAs matches persisted IST form against UTC Z and vice versa.
	legacyIST := ClockOffset{Form: rawIST}
	utc := ClockOffset{Form: canon}
	est := ClockOffset{Form: rawEST}
	if !legacyIST.WrittenAs(canon) || !utc.WrittenAs(rawIST) || !legacyIST.WrittenAs(rawEST) {
		t.Fatal("WrittenAs did not treat +99:99, -99:99, and a as equivalent")
	}
	if legacyIST.WrittenAs("text 9999-99-99 99:99:99a") || legacyIST.WrittenAs("text 9999-99-99a99:99:99") || legacyIST.WrittenAs("") {
		t.Fatal("FALSE GREEN: WrittenAs matched distinct layout or empty form")
	}

	// Meet preserves raw form when identical, keeps both raw forms when 2
	// share a canonical class (order-independent, v3.7.26 compatible), and
	// falls back to canonical when >2 distinct raw forms meet.
	if m := legacyIST.Meet(legacyIST); m.Form != rawIST || m.FormF != "" || m.Empty() {
		t.Fatalf("Meet(rawIST, rawIST) = %+v; want raw %q preserved", m, rawIST)
	}
	if m1, m2 := legacyIST.Meet(utc), utc.Meet(legacyIST); m1 != m2 || m1.Form != rawIST || m1.FormF != canon || m1.Empty() {
		t.Fatalf("Meet(rawIST, utc) = %+v / %+v; want commutative (%q, %q)", m1, m2, rawIST, canon)
	}
	if m1, m2 := legacyIST.Meet(est).Meet(utc), legacyIST.Meet(est.Meet(utc)); m1 != m2 || m1.Form != canon || m1.FormF != "" {
		t.Fatalf("3-way Meet(+99:99, -99:99, a) = %+v / %+v; want canonical %q", m1, m2, canon)
	}
}
