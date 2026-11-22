package variables

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

const (
	mintRec  = "0f8a6c2e-6b1d-4c3a-9e5f-2a7b8c9d0e1f"
	mintLive = "5c1e7a9b-3d2f-4e8a-b6c4-9f0a1b2c3d4e"
	mintNext = "9d3b2a1c-7e6f-4a5b-8c9d-0e1f2a3b4c5d"
)

// mintedB is a binding of the minted variable name at mock-1's body._id,
// recorded mintRec, bound to value, of a UUID's form: no prefix, 36 bytes.
func mintedB(name, value string) Binding {
	return Binding{Name: name, Value: value, Recorded: mintRec, Mock: "mock-1", Place: "body._id", Minted: &models.MintedShape{Rule: "audit-id", MintedForm: models.MintedForm{MinLen: 36, MaxLen: 36}}}
}

// A minted value is new and keeps its rule's form: its recorded value, the
// value of a minted variable bound earlier, and a value of another length
// or prefix, or that holds whitespace (or of a form keploy cannot check),
// are refused, each saying which. Any printable character after the prefix
// keeps the form. A binding of no minted variable, and an earlier binding
// that is no minted variable's or holds another value, refuse nothing.
func TestMintedWhy(t *testing.T) {
	plain := mintedB("auditId", mintLive)
	plain.Minted = nil
	odd := mintedB("auditId", mintLive)
	odd.Minted.MaxLen = 30
	prefixed := mintedB("auditId", "kepreq_"+mintLive[7:])
	prefixed.Minted.Prefix, prefixed.Recorded = "kepreq_", "kepreq_"+mintRec[7:]
	ranged := mintedB("auditId", "n-123456")
	ranged.Minted.Prefix, ranged.Minted.MinLen, ranged.Minted.MaxLen, ranged.Recorded = "n-", 7, 9, "n-12345"
	unprefixed := prefixed
	unprefixed.Value = "kepreg_" + mintLive[7:]
	for _, c := range []struct {
		name    string
		b       Binding
		earlier []Binding
		want    string
	}{
		{"new and of its shape", mintedB("auditId", mintLive), nil, ""},
		{"no minted variable", Binding{Name: "auditId", Value: mintRec, Recorded: mintRec}, nil, ""},
		{"its recorded value", mintedB("auditId", mintRec), nil, `minted value reused: keploy.yml's rule audit-id declares variable auditId made anew at body._id of mock-1, and this call sends the value it was recorded with there, "` + mintRec + `"`},
		{"a minted value bound earlier", mintedB("auditId", mintLive), []Binding{mintedB("clusterId", mintNext), mintedB("otherId", mintLive)}, "minted value reused: keploy.yml's rule audit-id declares variable auditId made anew at body._id of mock-1, and this call sends \"" + mintLive + "\" there, the value variable otherId was bound to earlier in this run, at body._id of mock-1"},
		{"an earlier value of no minted variable", mintedB("auditId", mintLive), []Binding{plain}, ""},
		{"another length", mintedB("auditId", mintLive[:35]), nil, "minted value of another shape: keploy.yml's rule audit-id declares variable auditId made anew at body._id of mock-1, and this call sends"},
		{"a dash moved: the form names no byte's place", mintedB("auditId", "5c1e7a9b3-d2f-4e8a-b6c4-9f0a1b2c3d4e"), nil, ""},
		{"any lower-case letter", mintedB("auditId", "5c1e7a9b-3d2f-4e8a-b6c4-9f0a1b2c3d4z"), nil, ""},
		{"a capital none of its values held", mintedB("auditId", "5c1e7a9b-3d2f-4e8a-b6c4-9f0a1b2c3d4E"), nil, ""},
		{"a ':' none of its values held", mintedB("auditId", "5c1e7a9b:3d2f-4e8a-b6c4-9f0a1b2c3d4e"), nil, ""},
		{"a space", mintedB("auditId", "5c1e7a9b 3d2f-4e8a-b6c4-9f0a1b2c3d4e"), nil, "which does not keep the rule's form: a length of 36, and only printable characters and no whitespace"},
		{"its prefix", prefixed, nil, ""},
		{"no prefix", unprefixed, nil, `which does not keep the rule's form: the prefix "kepreq_", a length of 36`},
		{"a length in its range", ranged, nil, ""},
		{"a length past its range", mintedB("auditId", "n-12345678"), nil, "a length of 36"},
		{"a form keploy cannot check", odd, nil, `minted value of another shape: keploy.yml's rule audit-id declares variable auditId made anew at body._id of mock-1, in a minted form's longest length is shorter than its shortest`},
	} {
		got := MintedWhy(c.b, c.earlier...)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: MintedWhy = %q, want %q", c.name, got, c.want)
		}
	}
}

// bindMinted binds bs in tb's scope in force, failing the test where
// BindAll's outcome is not want; it returns the refused binding and the one
// it is refused beside.
func bindMinted(t *testing.T, tb *Table, want bool, bs ...Binding) (bad, with Binding) {
	t.Helper()
	pre, preWith := "", Binding{}
	for i, b := range bs {
		if why, with := tb.MintedWhy(b, bs[:i]); why != "" {
			pre, preWith = why, with
			break
		}
	}
	bad, with, stale, ok := tb.BindAll(tb.Gen(), bs)
	if stale || ok != want {
		t.Fatalf("BindAll(%v) = ok %v stale %v (refused %s beside %s), want ok %v", bs, ok, stale, bad.Name, with.Name, want)
	}
	if (pre == "") != want {
		t.Fatalf("the check before the bind (Table.MintedWhy) said %q, and BindAll's outcome is ok %v", pre, ok)
	}
	if !ok && MintedWhy(bad, with) != pre {
		t.Fatalf("BindAll's refusal reads %q, the check before it %q", MintedWhy(bad, with), pre)
	}
	if !ok && !sameBinding(preWith, with) {
		t.Fatalf("the check before the bind names %+v, BindAll's refusal %+v", preWith, with)
	}
	return bad, with
}

// sameBinding reports whether a and b are one binding: the same variable
// and value, bound at the same place of the same recording.
func sameBinding(a, b Binding) bool {
	return a.Name == b.Name && a.Value == b.Value && a.Mock == b.Mock && a.Place == b.Place && (a.Minted == nil) == (b.Minted == nil)
}

// The run's minted values outlive a test's scope and a test set: a minted
// value a later test binds again, under its variable's name or another
// minted variable's, is refused beside the first binding, as the check
// before the bind says. A name the scope bound to the value already binds
// nothing new and is not refused.
func TestTable_AMintedValueIsNewInTheRun(t *testing.T) {
	var tb Table
	first := mintedB("auditId", mintLive)
	bindMinted(t, &tb, true, first)
	bindMinted(t, &tb, true, first) // the scope's second call that sends it
	tb.Reset()
	_, with := bindMinted(t, &tb, false, mintedB("auditId", mintLive))
	if with.Name != "auditId" || with.Value != mintLive {
		t.Fatalf("refused beside %+v, want the first test's binding", with)
	}
	tb.NewSet()
	_, with = bindMinted(t, &tb, false, mintedB("clusterId", mintLive))
	if with.Name != "auditId" {
		t.Fatalf("refused beside %+v, want the earlier set's binding", with)
	}
	bindMinted(t, &tb, true, mintedB("auditId", mintNext))
	if _, ok := tb.Bind(mintedB("otherId", mintNext)); ok {
		t.Fatal("Bind bound a minted value the run bound before")
	}
	const bound = "2a7b8c9d-0e1f-4a5b-8c9d-5c1e7a9b3d2f"
	if _, ok := tb.Bind(mintedB("bindId", bound)); !ok {
		t.Fatal("Bind refused a new minted value")
	}
	tb.Reset()
	if _, with := bindMinted(t, &tb, false, mintedB("auditId", bound)); with.Name != "bindId" {
		t.Fatalf("refused beside %+v, want the value Bind bound", with)
	}
	// A variable that is no minted one may hold a minted value of the
	// run: a later call that sends the id the run made.
	tb.Reset()
	bindMinted(t, &tb, true, Binding{Name: "readId", Value: bound, Recorded: mintRec})
	// A value of no minted variable is not the run's minted value.
	tb.Reset()
	plain := Binding{Name: "appId", Value: "7d3b2a1c-7e6f-4a5b-8c9d-0e1f2a3b4c5d", Recorded: mintRec}
	bindMinted(t, &tb, true, plain)
	tb.Reset()
	bindMinted(t, &tb, true, mintedB("auditId", plain.Value))
	// Every value the run bound stays, the first as the last.
	tb.Reset()
	if _, with := bindMinted(t, &tb, false, mintedB("lateId", mintLive)); with.Name != "auditId" || with.Value != mintLive {
		t.Fatalf("refused beside %+v, want the run's first binding", with)
	}
}

// One call's bindings are all or none: two minted variables the call binds
// to one value are refused beside the first, its own value refuses a
// binding beside none, and a call refused binds none of its minted values,
// so a later call may bind them. A call that binds one variable twice is
// not refused.
func TestTable_AMintedValueIsNewInItsCall(t *testing.T) {
	var tb Table
	_, with := bindMinted(t, &tb, false, mintedB("auditId", mintLive), mintedB("clusterId", mintLive))
	if with.Name != "auditId" {
		t.Fatalf("refused beside %+v, want the call's first binding", with)
	}
	bad, with := bindMinted(t, &tb, false, mintedB("auditId", mintLive), mintedB("clusterId", mintRec))
	if bad.Name != "clusterId" || with.Name != "" {
		t.Fatalf("refused %s beside %+v, want clusterId refused by its own value", bad.Name, with)
	}
	bad, _ = bindMinted(t, &tb, false, mintedB("auditId", mintLive), mintedB("clusterId", mintLive[:35]))
	if bad.Name != "clusterId" {
		t.Fatalf("refused %s, want clusterId, of another shape", bad.Name)
	}
	bindMinted(t, &tb, true, mintedB("auditId", mintLive), mintedB("auditId", mintLive))
	bindMinted(t, &tb, true, mintedB("clusterId", mintNext))
	tb.Reset()
	const one, two = "1a2b3c4d-5e6f-4a5b-8c9d-0e1f2a3b4c5d", "6f5e4d3c-2b1a-4a5b-8c9d-0e1f2a3b4c5d"
	bindMinted(t, &tb, true, mintedB("auditId", one), mintedB("clusterId", two))
	// A variable of the call that is no minted one may hold its value.
	tb.Reset()
	const three = "3c4d5e6f-1a2b-4a5b-8c9d-0e1f2a3b4c5d"
	bindMinted(t, &tb, true, Binding{Name: "refId", Value: three, Recorded: mintRec}, mintedB("auditId", three))
	if why, with := (*Table)(nil).MintedWhy(mintedB("auditId", mintRec), nil); !strings.HasPrefix(why, "minted value reused") || with.Name != "" {
		t.Fatalf("a nil table's check of a recorded value said %q beside %+v", why, with)
	}
	if why, with := (*Table)(nil).MintedWhy(mintedB("clusterId", mintLive), []Binding{mintedB("auditId", mintLive)}); !strings.HasPrefix(why, "minted value reused") || with.Name != "auditId" {
		t.Fatalf("a nil table's check of a value bound with it said %q beside %+v", why, with)
	}
}

// The check before the bind names the binding whose value a minted value
// reuses, as BindAll's refusal does: the recording that bound it first in
// the run (an earlier test's), so the call's miss can say where. A value
// refused by itself (its recorded one, another shape) names none, and so
// does a value that may be bound.
func TestTable_AMintedReuseNamesTheEarlierBinding(t *testing.T) {
	var tb Table
	first := mintedB("auditId", mintLive)
	first.Mock, first.Place = "mock-insert", "body.cmd.documents.0._id"
	if why, with := tb.MintedWhy(first, nil); why != "" || with.Name != "" {
		t.Fatalf("a new value's check said %q beside %+v", why, with)
	}
	bindMinted(t, &tb, true, first)
	tb.Reset() // the next test
	again := mintedB("clusterId", mintLive)
	again.Mock = "mock-later"
	why, with := tb.MintedWhy(again, nil)
	if !strings.HasPrefix(why, "minted value reused") || with.Mock != "mock-insert" || with.Name != "auditId" || with.Place != first.Place {
		t.Fatalf("the check of a reused value said %q beside %+v, want the earlier test's binding at mock-insert", why, with)
	}
	for _, own := range []Binding{mintedB("clusterId", mintRec), mintedB("clusterId", mintNext[:35])} {
		if why, with := tb.MintedWhy(own, nil); why == "" || with.Name != "" || with.Mock != "" {
			t.Fatalf("the check of %q said %q beside %+v, want a refusal beside none", own.Value, why, with)
		}
	}
}

// A table that reads another's run's (the next test's calls in flight)
// holds one list of the run's minted values with it: what either binds,
// the other refuses, and what the reader bound before it read the other's
// is the other's too.
func TestTable_AReaderSharesTheRunsMintedValues(t *testing.T) {
	var vars, ahead Table
	bindMinted(t, &ahead, true, mintedB("auditId", mintNext))
	ahead.ReadRunOf(&vars)
	vars.Reset()
	if _, with := bindMinted(t, &vars, false, mintedB("clusterId", mintNext)); with.Name != "auditId" {
		t.Fatalf("refused beside %+v, want the reader's earlier binding", with)
	}
	bindMinted(t, &vars, true, mintedB("auditId", mintLive))
	ahead.Reset()
	if _, with := bindMinted(t, &ahead, false, mintedB("otherId", mintLive)); with.Name != "auditId" {
		t.Fatalf("refused beside %+v, want the run's binding", with)
	}
	if ahead.minted != nil {
		t.Fatal("the reader keeps a list of its own")
	}
}

// Where a table and its reader bound one minted value each before the
// reader read the other's, the run's list keeps the other's binding of it.
func TestTable_AReadersMintedValueTheOtherHadStaysTheOthers(t *testing.T) {
	var vars, ahead Table
	bindMinted(t, &vars, true, mintedB("auditId", mintLive))
	bindMinted(t, &ahead, true, mintedB("clusterId", mintLive))
	ahead.ReadRunOf(&vars)
	vars.Reset()
	if _, with := bindMinted(t, &vars, false, mintedB("otherId", mintLive)); with.Name != "auditId" {
		t.Fatalf("refused beside %+v, want the table's own binding", with)
	}
}

// A reader that turns from one table's run to another's carries the run's
// minted values with it: a value it bound at a minted place while it read
// the first is refused at the second, naming that binding.
func TestTable_AReaderCarriesTheRunsMintedValuesWhenItTurnsToAnotherTable(t *testing.T) {
	var first, second, ahead Table
	ahead.ReadRunOf(&first)
	bindMinted(t, &ahead, true, mintedB("auditId", mintNext))
	ahead.ReadRunOf(&second)
	if _, with := bindMinted(t, &second, false, mintedB("clusterId", mintNext)); with.Name != "auditId" {
		t.Fatalf("refused beside %+v, want the binding made while the reader read the first table", with)
	}
	ahead.Reset()
	if _, with := bindMinted(t, &ahead, false, mintedB("otherId", mintNext)); with.Name != "auditId" {
		t.Fatalf("the reader itself: refused beside %+v, want its own earlier binding", with)
	}
}

// A binding of the call alone (Binding.Call: a recording no test owns,
// whose value each serve mints anew) binds no name, so each serve binds
// a new value under the one name; its value is still checked against the
// run's minted values and kept among them, so a later reuse of it, by a
// call's binding or a test's variable, is refused, naming it. A test's
// variable at its own place of its recording, bound beside it in one call,
// is no reuse; one at another place is.
func TestTable_ACallsMintedBindingIsKeptInTheRunAndBindsNoName(t *testing.T) {
	var tb Table
	call := func(value string) Binding {
		b := mintedB("poolId", value)
		b.Call = true
		return b
	}
	bindMinted(t, &tb, true, call(mintLive))
	if b, ok := tb.Lookup("poolId"); ok {
		t.Fatalf("a call's binding bound its name: %+v", b)
	}
	bindMinted(t, &tb, true, call(mintNext))
	if bad, with := bindMinted(t, &tb, false, call(mintLive)); bad.Name != "poolId" || with.Value != mintLive || with.Name != "poolId" {
		t.Fatalf("a call's reuse refused %s beside %+v, want poolId beside the run's first", bad.Name, with)
	}
	if _, with := bindMinted(t, &tb, false, mintedB("auditId", mintNext)); with.Name != "poolId" || !with.Call {
		t.Fatalf("a test's reuse of a call's value refused beside %+v, want the call's binding", with)
	}
	tb.Reset() // the next test: the run's minted values stay
	if bad, _ := bindMinted(t, &tb, false, call(mintNext)); bad.Name != "poolId" {
		t.Fatalf("a reuse in the next test refused %s, want poolId", bad.Name)
	}
	const one, two = "1a2b3c4d-5e6f-4a5b-8c9d-0e1f2a3b4c5d", "6f5e4d3c-2b1a-4a5b-8c9d-0e1f2a3b4c5d"
	bindMinted(t, &tb, true, mintedB("auditId", one), call(one))
	elsewhere := call(two)
	elsewhere.Place = "body.ref"
	if bad, with := bindMinted(t, &tb, false, mintedB("clusterId", two), elsewhere); bad.Name != "poolId" || with.Name != "clusterId" {
		t.Fatalf("one call's value at two places refused %s beside %s, want poolId beside clusterId", bad.Name, with.Name)
	}
}

// A call's minted binding reads no name either: a test's variable of its
// name, bound to another value, neither refuses it nor spares it the
// minted check, before the bind (Table.MintedWhy) as in BindAll. A test's
// variable beside it at its own place of its recording is one value in
// either order; at that place of another recording it is a reuse.
func TestTable_ACallsMintedBindingReadsNoNameAndIsOneValueOnlyAtItsOwnPlace(t *testing.T) {
	var tb Table
	call := func(value string) Binding {
		b := mintedB("poolId", value)
		b.Call = true
		return b
	}
	plain := Binding{Name: "poolId", Value: "tenant-7", Recorded: "tenant-1", Mock: "mock-0", Place: "body.tenant"}
	if bad, with, stale, ok := tb.BindAll(tb.Gen(), []Binding{plain}); !ok || stale {
		t.Fatalf("binding the test's poolId refused %s beside %s", bad.Name, with.Name)
	}
	bindMinted(t, &tb, true, call(mintLive))
	if b, ok := tb.Lookup("poolId"); !ok || b.Value != "tenant-7" {
		t.Fatalf("a call's binding changed the test's poolId: %+v", b)
	}
	if bad, with := bindMinted(t, &tb, false, call(mintLive)); bad.Name != "poolId" || with.Value != mintLive || !with.Call {
		t.Fatalf("a call's reuse beside a bound name refused %s beside %+v, want poolId beside the call's first", bad.Name, with)
	}
	const one, two = "1a2b3c4d-5e6f-4a5b-8c9d-0e1f2a3b4c5d", "6f5e4d3c-2b1a-4a5b-8c9d-0e1f2a3b4c5d"
	bindMinted(t, &tb, true, call(one), mintedB("auditId", one))
	other := call(two)
	other.Mock = "mock-2"
	if bad, with := bindMinted(t, &tb, false, mintedB("clusterId", two), other); bad.Name != "poolId" || with.Name != "clusterId" {
		t.Fatalf("one value at one place of two recordings refused %s beside %s, want poolId beside clusterId", bad.Name, with.Name)
	}
}
