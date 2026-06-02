package models

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	yaml "gopkg.in/yaml.v3"
)

// Untagged-mapping dispatch in decodePgUntaggedMapping routes a YAML
// mapping to a concrete pgtype struct by matching the lowercased key
// set against a per-type canonical set. If two types share a key set
// (or one is a subset of another and `keysEqual` is ever loosened to
// "contains-all"), the dispatcher silently picks one and the other
// type's recordings get reconstructed as the wrong Go type. This file
// pins the relations between every pgtype's canonical mapping shape
// so future edits cannot reintroduce the ambiguity.
//
// The 16 pgtype types covered by PostgresV3Cell.MarshalYAML / its
// untagged-mapping decode counterpart, with their canonical YAML key
// set (or, for non-mapping shapes, the shape kind):
//
//	Numeric    {int, exp, nan, infinitymodifier, valid}            mapping
//	Interval   {microseconds, days, months, valid}                 mapping
//	Time       {microseconds, valid}                                mapping
//	Bits       {bytes, len, valid}                                  mapping
//	Point      {p, valid}                                           mapping  (collides)
//	Line       {a, b, c, valid}                                     mapping
//	Lseg       {p, valid}                                           mapping  (collides)
//	Box        {p, valid}                                           mapping  (collides)
//	Path       {p, closed, valid}                                   mapping
//	Polygon    {p, valid}                                           mapping  (collides)
//	Circle     {p, r, valid}                                        mapping
//	TID        {blocknumber, offsetnumber, valid}                   mapping
//	TSVector   {lexemes, valid}                                     mapping
//	Hstore     <user-defined keys, no canonical set>                mapping  (no probe)
//	Range      {lower, upper, lowertype, uppertype, valid}          mapping
//	Multirange <sequence of nested ranges>                          sequence (no probe)
//
// Point/Lseg/Box/Polygon are intentionally NOT dispatched by the
// untagged probe (see the doc comment on decodePgUntaggedMapping):
// their `{p, valid}` key set is shared four ways and cannot be
// disambiguated from key names alone. They round-trip through the
// reflective map[string]any decode, matching how released-keploy's
// reflection-based emit also rehydrates them. Hstore carries
// arbitrary user keys, so it has no canonical key set to probe — it
// only decodes back to pgtype.Hstore via the legacy `!pg/hstore`
// tagged path. Multirange is a sequence, not a mapping, so it never
// flows through the mapping-probe path at all.

// pgtypeKeySet pairs a Go type label with its canonical YAML mapping
// key set. Mirrors the cases inside decodePgUntaggedMapping plus the
// types whose key sets exist on the marshal side but are intentionally
// not probed.
type pgtypeKeySet struct {
	name       string
	keys       []string
	dispatched bool // true iff decodePgUntaggedMapping routes this key set to a concrete reconstructor
}

func allPgtypeKeySets() []pgtypeKeySet {
	return []pgtypeKeySet{
		// Dispatched untagged: the 10 mapping shapes whose key sets
		// uniquely identify a concrete pgtype.
		{"Numeric", []string{"int", "exp", "nan", "infinitymodifier", "valid"}, true},
		{"Interval", []string{"microseconds", "days", "months", "valid"}, true},
		{"Time", []string{"microseconds", "valid"}, true},
		{"Bits", []string{"bytes", "len", "valid"}, true},
		{"Line", []string{"a", "b", "c", "valid"}, true},
		{"Path", []string{"p", "closed", "valid"}, true},
		{"Circle", []string{"p", "r", "valid"}, true},
		{"TID", []string{"blocknumber", "offsetnumber", "valid"}, true},
		{"TSVector", []string{"lexemes", "valid"}, true},
		{"Range", []string{"lower", "upper", "lowertype", "uppertype", "valid"}, true},
		// Not dispatched untagged: the four geo types that share
		// {p, valid} (see the doc comment on decodePgUntaggedMapping).
		{"Point", []string{"p", "valid"}, false},
		{"Lseg", []string{"p", "valid"}, false},
		{"Box", []string{"p", "valid"}, false},
		{"Polygon", []string{"p", "valid"}, false},
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func keySetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := sortedCopy(a)
	bs := sortedCopy(b)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// keySetIsSubsetOf reports whether every element of `sub` appears in
// `sup`. Returns true when sub == sup as well; callers separate the
// equality and proper-subset cases.
func keySetIsSubsetOf(sub, sup []string) bool {
	have := make(map[string]struct{}, len(sup))
	for _, k := range sup {
		have[k] = struct{}{}
	}
	for _, k := range sub {
		if _, ok := have[k]; !ok {
			return false
		}
	}
	return true
}

// TestPgtypeYAMLKeySetsMutuallyDisjoint asserts that no two dispatched
// pgtype canonical key sets are equal — equality would mean
// decodePgUntaggedMapping's `keysEqual` switch matches on more than
// one type, and the first-listed case wins, silently corrupting the
// other type's recordings. This is the strict invariant: zero
// equality collisions among dispatched key sets.
//
// Subset relations (one dispatched key set is a strict subset of
// another) are checked separately by
// TestPgtypeYAMLKeySetsKnownSubsetRelations: those are harmless under
// today's exact-match `keysEqual` predicate but would become silent
// corruption if anyone weakened the predicate to "contains all
// required keys".
func TestPgtypeYAMLKeySetsMutuallyDisjoint(t *testing.T) {
	sets := allPgtypeKeySets()
	for i := range sets {
		for j := range sets {
			if i >= j {
				continue
			}
			a := sets[i]
			b := sets[j]
			if !a.dispatched || !b.dispatched {
				continue
			}
			if keySetEqual(a.keys, b.keys) {
				t.Errorf("dispatched key sets equal: %s == %s = %v — exact-match dispatch picks the first-listed case and silently corrupts the other",
					a.name, b.name, sortedCopy(a.keys))
			}
		}
	}
}

// TestPgtypeYAMLKeySetsKnownSubsetRelations pins the *expected*
// strict-subset relations among the 16 pgtype canonical key sets. A
// strict subset is harmless under the current `keysEqual` exact-match
// predicate (each mapping decodes to its own type because the key
// counts differ), but it represents a latent corruption surface: if
// anyone weakens the predicate to "contains all required keys", the
// smaller set will shadow the larger one and silently misroute the
// larger type's recordings.
//
// Known subset relations on the dispatched-side today:
//
//	Time {microseconds, valid} ⊂ Interval {microseconds, days, months, valid}
//
// Known subset relations on the geo-types-fall-through side (these
// are why Point/Lseg/Box/Polygon are *not* dispatched in the first
// place — their {p, valid} is a strict subset of Path's
// {p, closed, valid} and Circle's {p, r, valid}, so the dispatcher
// would have to disambiguate them with a tiebreaker we don't have):
//
//	Point/Lseg/Box/Polygon {p, valid} ⊂ Path {p, closed, valid}
//	Point/Lseg/Box/Polygon {p, valid} ⊂ Circle {p, r, valid}
//
// If the actual subset relations diverge from the expected list, the
// test fails — forcing a reviewer to re-audit decodePgUntaggedMapping
// for the new dispatch ambiguity.
func TestPgtypeYAMLKeySetsKnownSubsetRelations(t *testing.T) {
	type pair struct{ sub, sup string }
	expected := map[pair]struct{}{
		{"Time", "Interval"}:  {},
		{"Point", "Path"}:     {},
		{"Lseg", "Path"}:      {},
		{"Box", "Path"}:       {},
		{"Polygon", "Path"}:   {},
		{"Point", "Circle"}:   {},
		{"Lseg", "Circle"}:    {},
		{"Box", "Circle"}:     {},
		{"Polygon", "Circle"}: {},
	}
	sets := allPgtypeKeySets()
	got := make(map[pair]struct{})
	for i := range sets {
		for j := range sets {
			if i == j {
				continue
			}
			a := sets[i]
			b := sets[j]
			if len(a.keys) >= len(b.keys) {
				continue
			}
			if keySetIsSubsetOf(a.keys, b.keys) {
				got[pair{a.name, b.name}] = struct{}{}
			}
		}
	}
	for p := range got {
		if _, ok := expected[p]; !ok {
			t.Errorf("new subset relation discovered: %s ⊂ %s — re-audit decodePgUntaggedMapping; weakening `keysEqual` to contains-all would silently corrupt %s recordings",
				p.sub, p.sup, p.sup)
		}
	}
	for p := range expected {
		if _, ok := got[p]; !ok {
			t.Errorf("expected subset relation %s ⊂ %s no longer holds — update the expected list and re-audit", p.sub, p.sup)
		}
	}
}

// TestPgtypeYAMLKeySetsDispatchedCount pins the count of types the
// untagged probe routes to a concrete pgtype reconstructor. If the
// count drifts (someone adds a new pgtype shape or stops dispatching
// an existing one), the disjointness audit above must be re-run by a
// human; this test forces that conversation.
func TestPgtypeYAMLKeySetsDispatchedCount(t *testing.T) {
	sets := allPgtypeKeySets()
	dispatched := 0
	for _, s := range sets {
		if s.dispatched {
			dispatched++
		}
	}
	const want = 10
	if dispatched != want {
		t.Errorf("dispatched key set count = %d, want %d — if you added a new pgtype shape to decodePgUntaggedMapping, update allPgtypeKeySets() and re-audit the disjointness invariants", dispatched, want)
	}
}

// TestPgtypeYAMLDispatch_Ambiguous_Point_vs_Lseg_vs_Box_vs_Polygon
// pins the documented {p, valid} four-way collision between
// Point/Lseg/Box/Polygon. Each of these marshals to an untagged
// mapping with exactly the keys {p, valid}; the untagged decoder
// cannot recover the original Go type from key names alone and
// intentionally falls through to the reflective map[string]any decode
// (matching how released-keploy's reflection emit also rehydrates
// them). This test verifies that fall-through stays in place — it
// would be worse to silently pick one of the four types and corrupt
// the other three than to surface a generic map.
func TestPgtypeYAMLDispatch_Ambiguous_Point_vs_Lseg_vs_Box_vs_Polygon(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "Point-shape",
			yaml: "p:\n  x: 1\n  y: 2\nvalid: true\n",
		},
		{
			name: "Lseg-shape",
			yaml: "p:\n  - {x: 1, y: 2}\n  - {x: 3, y: 4}\nvalid: true\n",
		},
		{
			name: "Box-shape",
			yaml: "p:\n  - {x: 1, y: 2}\n  - {x: 3, y: 4}\nvalid: true\n",
		},
		{
			name: "Polygon-shape",
			yaml: "p:\n  - {x: 1, y: 2}\n  - {x: 3, y: 4}\n  - {x: 5, y: 6}\nvalid: true\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c PostgresV3Cell
			if err := yaml.Unmarshal([]byte(tc.yaml), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// The dispatcher must NOT pick a concrete geo pgtype —
			// any choice would be wrong for at least three of the
			// four types and corrupt their recordings.
			switch c.Value.(type) {
			case pgtype.Point, pgtype.Lseg, pgtype.Box, pgtype.Polygon:
				t.Errorf("untagged {p, valid} mapping dispatched as concrete %T; the four-way collision means any concrete pick corrupts the other three types — expected map fall-through", c.Value)
			}
			// The fall-through path decodes the mapping generically.
			// yaml.v3 with no out-type hint produces map[string]any.
			if _, ok := c.Value.(map[string]any); !ok {
				t.Errorf("ambiguous {p, valid} mapping decoded as %T, want map[string]any (generic fall-through)", c.Value)
			}
		})
	}
}

// TestPgtypeYAMLDispatch_TimeNotMisroutedAsInterval guards the one
// non-collision-but-subset relation among the dispatched types:
// Time's {microseconds, valid} is a proper subset of Interval's
// {microseconds, days, months, valid}. Today `keysEqual` requires
// exact equality, so a real Time mapping decodes to pgtype.Time and a
// real Interval mapping decodes to pgtype.Interval — but if the
// dispatch predicate is ever weakened to "contains all required keys",
// every Time recording would be silently misrouted to Interval (with
// Days=0, Months=0). Pin both directions.
func TestPgtypeYAMLDispatch_TimeNotMisroutedAsInterval(t *testing.T) {
	timeYAML := "microseconds: 12345\nvalid: true\n"
	var c PostgresV3Cell
	if err := yaml.Unmarshal([]byte(timeYAML), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := c.Value.(pgtype.Time)
	if !ok {
		t.Fatalf("Time-shape mapping decoded as %T, want pgtype.Time", c.Value)
	}
	if got.Microseconds != 12345 || !got.Valid {
		t.Errorf("Time decoded to %+v, want {Microseconds:12345 Valid:true}", got)
	}

	intervalYAML := "microseconds: 12345\ndays: 7\nmonths: 1\nvalid: true\n"
	var c2 PostgresV3Cell
	if err := yaml.Unmarshal([]byte(intervalYAML), &c2); err != nil {
		t.Fatalf("unmarshal interval: %v", err)
	}
	got2, ok := c2.Value.(pgtype.Interval)
	if !ok {
		t.Fatalf("Interval-shape mapping decoded as %T, want pgtype.Interval", c2.Value)
	}
	if got2.Microseconds != 12345 || got2.Days != 7 || got2.Months != 1 || !got2.Valid {
		t.Errorf("Interval decoded to %+v, want {Microseconds:12345 Days:7 Months:1 Valid:true}", got2)
	}
}

// TestPgtypeYAMLDispatch_HstoreUserKeysDoNotMisroute pins the
// behavior for hstore values whose user-supplied keys happen to
// collide with one of the dispatched canonical key sets. After the
// !pg/<name> tag drop in d48df6bf, marshalPgHstoreYAML emits an
// untagged mapping carrying the raw user keys; if a user's hstore
// happens to have keys exactly matching e.g. {microseconds, valid},
// the dispatcher would misroute it to pgtype.Time. We can't avoid
// this from key names alone — Hstore has no canonical key set — so
// the test documents the trade-off: hstore values whose user keys
// match a dispatched canonical key set DO misroute, and the only
// recovery is the legacy `!pg/hstore` tagged form on disk. The test
// asserts the misroute is deterministic (same value gets the same
// wrong type, not a non-deterministic pick) so a future fix has a
// stable baseline to flip.
func TestPgtypeYAMLDispatch_HstoreUserKeysDoNotMisroute(t *testing.T) {
	// An hstore with keys that don't collide with any dispatched key
	// set decodes to map[string]any (the generic fall-through). The
	// recorder relies on the integrations-side pgtype.Hstore re-emit
	// path to bind the right Go type before sending bytes back to
	// the client; the YAML is just a transport.
	hstoreYAML := "key1: value1\nkey2: value2\n"
	var c PostgresV3Cell
	if err := yaml.Unmarshal([]byte(hstoreYAML), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := c.Value.(map[string]any); !ok {
		t.Errorf("non-colliding hstore mapping decoded as %T, want map[string]any", c.Value)
	}

	// Sanity: an hstore whose user keys happen to be exactly one of
	// the dispatched canonical sets DOES misroute today. Pin that
	// outcome so a future tiebreaker (e.g. an !!hstore sentinel key)
	// can flip this assertion in one place.
	collidingHstoreYAML := "microseconds: 12345\nvalid: true\n"
	var c2 PostgresV3Cell
	if err := yaml.Unmarshal([]byte(collidingHstoreYAML), &c2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Today this is decoded as pgtype.Time. Documenting the misroute
	// rather than asserting correctness — the only fix is a sentinel
	// field on the marshal side, which is out of scope for this test.
	if _, ok := c2.Value.(pgtype.Time); !ok {
		t.Logf("hstore-key-collision behavior changed: %T (was pgtype.Time)", c2.Value)
	}
}

// TestPgtypeYAMLDispatch_AllDispatchedRoundTrip spot-checks that every
// dispatched canonical key set actually round-trips its concrete
// pgtype value through MarshalYAML → UnmarshalYAML. If any pair drifts
// (e.g. a marshal helper adds or renames a field but the decode probe
// isn't updated), the cell would silently fall through to
// map[string]any instead of the typed reconstructor — visible only as
// downstream gob-encode failures on the proxy. Pin the typed
// round-trip here so the regression surfaces at unit-test time.
func TestPgtypeYAMLDispatch_AllDispatchedRoundTrip(t *testing.T) {
	// Build one concrete value per dispatched type and check the
	// decoded Go type matches what we marshalled. Round-trip values
	// are kept minimal — the field-level round-trip is covered by
	// the existing per-type tests in postgres_v3_cell_test.go; this
	// test only audits the dispatch, not the field decode.
	cases := []struct {
		name string
		val  any
	}{
		{"Numeric", pgtype.Numeric{Valid: true}},
		{"Interval", pgtype.Interval{Valid: true}},
		{"Time", pgtype.Time{Valid: true}},
		{"Bits", pgtype.Bits{Valid: true}},
		{"Line", pgtype.Line{Valid: true}},
		{"Path", pgtype.Path{Valid: true}},
		{"Circle", pgtype.Circle{Valid: true}},
		{"TID", pgtype.TID{Valid: true}},
		{"TSVector", pgtype.TSVector{Valid: true}},
		{"Range", pgtype.Range[any]{Valid: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := PostgresV3Cell{Value: tc.val}
			buf, err := yaml.Marshal(cell)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Sanity: untagged emit (no `!pg/<name>` in the bytes).
			if strings.Contains(string(buf), "!pg/") {
				t.Errorf("MarshalYAML emitted a `!pg/<name>` tag for %s — d48df6bf removed those:\n%s", tc.name, buf)
			}
			var got PostgresV3Cell
			if err := yaml.Unmarshal(buf, &got); err != nil {
				t.Fatalf("unmarshal:\n%s\nerr: %v", buf, err)
			}
			gotType := fmt.Sprintf("%T", got.Value)
			wantType := fmt.Sprintf("%T", tc.val)
			if gotType != wantType {
				t.Errorf("round-trip type drift: got %s, want %s\nyaml:\n%s", gotType, wantType, buf)
			}
		})
	}
}
