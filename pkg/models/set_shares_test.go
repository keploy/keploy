package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

func shareEntry(name string, ms int, group string) MockEntry {
	return MockEntry{Name: name, Kind: string(PostgresV3), ReqTimestampMock: FormatMockTimestamp(wmAt(ms)), Group: group}
}

// shareShape is m's entries as "name/group/marker" by test, and shared's.
func shareShape(m *Mapping) map[string][]string {
	out := map[string][]string{}
	for _, tc := range m.TestCases {
		out[tc.ID] = []string{}
		for _, e := range tc.Mocks {
			out[tc.ID] = append(out[tc.ID], e.Name+"/"+e.Group+"/"+e.Unresolved)
		}
	}
	for _, e := range m.Shared {
		out["shared:"] = append(out["shared:"], e.Name+"/"+e.Group)
	}
	return out
}

func sharesMapping() *Mapping {
	return &Mapping{
		Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "test-set-0",
		TestCases: []MappedTestCase{
			{ID: "a", Mocks: []MockEntry{shareEntry("own-a", 900, ""), shareEntry("m1", 1000, "g1"), shareEntry("m2", 1100, "g1"), shareEntry("m3", 1200, "g1"), shareEntry("m4", 1300, "g2")}},
			{ID: "b", Mocks: []MockEntry{shareEntry("own-b", 1050, "")}},
			{ID: "c", Mocks: []MockEntry{}},
		},
		Groups: []ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b", "c"}}, {ID: "g2", Tests: []string{"a", "c"}, Unkept: 1}, {ID: "g3", Tests: []string{"b", "c"}}},
		Shared: []MockEntry{shareEntry("s1", 1150, "g3")},
	}
}

// What keploy record's full round settled is written onto the mapping: a pin
// moves its recording to the test that made the call, in recorded order,
// without its group; a move keeps its group and marks it; a recording put in
// shared leaves the test that listed it. A group no entry names any more is
// dropped, and nothing else changes.
func TestApplySharesPinsMovesAndShares(t *testing.T) {
	m := sharesMapping()
	changed := m.ApplyShares(SetShares{
		State:    SharesComplete,
		Pins:     map[string]string{"m1": "b", "m2": "a", "s1": "c"},
		Moves:    map[string]ShareMove{"m3": {To: "c", Marker: MockEntryUnresolvedAmbiguous}},
		ToShared: []string{"m4"},
	})
	if !changed {
		t.Fatal("ApplyShares reported no change")
	}
	want := map[string][]string{
		"a":       {"own-a//", "m2//"},
		"b":       {"m1//", "own-b//"},
		"c":       {"s1//", "m3/g1/ambiguous"},
		"shared:": {"m4/g2"},
	}
	if got := shareShape(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("after ApplyShares %v; want %v", got, want)
	}
	if ids := []string{m.Groups[0].ID, m.Groups[1].ID}; len(m.Groups) != 2 || !reflect.DeepEqual(ids, []string{"g1", "g2"}) {
		t.Fatalf("groups %+v; want g1 and g2 kept (named by m3 and m4), g3 dropped", m.Groups)
	}
	// Idempotent.
	before := shareShape(m)
	if m.ApplyShares(SetShares{State: SharesComplete, Pins: map[string]string{"m1": "b", "m2": "a", "s1": "c"},
		Moves: map[string]ShareMove{"m3": {To: "c", Marker: MockEntryUnresolvedAmbiguous}}, ToShared: []string{"m4"}}) {
		t.Fatal("ApplyShares applied twice reported a change")
	}
	if got := shareShape(m); !reflect.DeepEqual(got, before) {
		t.Fatalf("applied twice: %v; want %v", got, before)
	}
}

// A pin to a test the mapping lists no entry for adds its entry; a share that
// names a recording the mapping does not list changes nothing.
func TestApplySharesAddsAnEntryAndLeavesWhatItDoesNotList(t *testing.T) {
	m := sharesMapping()
	if m.ApplyShares(SetShares{State: SharesComplete, Pins: map[string]string{"nowhere": "a"}}) {
		t.Fatal("a pin of a recording the mapping does not list changed it")
	}
	if !m.ApplyShares(SetShares{State: SharesComplete, Pins: map[string]string{"m1": "new-test"}}) {
		t.Fatal("no change")
	}
	got := shareShape(m)
	if !reflect.DeepEqual(got["new-test"], []string{"m1//"}) {
		t.Fatalf("new-test lists %v; want [m1]", got["new-test"])
	}
}

// The concurrency fields are omitted when empty: a mapping recorded without
// concurrency writes what it wrote before, in YAML and JSON. With them, they
// round-trip through YAML, JSON and BSON.
func TestConcurrencyFieldsRoundTripAndAreOmittedWhenEmpty(t *testing.T) {
	plain := &Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "test-set-0",
		TestCases: []MappedTestCase{{ID: "a", Mocks: []MockEntry{shareEntry("m1", 1000, "")}}}}
	y, err := yaml.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"group", "groups", "shared", "unresolved", "unkept"} {
		if strings.Contains(string(y), key) || strings.Contains(string(j), `"`+key+`"`) {
			t.Fatalf("a mapping without concurrency writes %q:\n%s\n%s", key, y, j)
		}
	}
	full := sharesMapping()
	full.TestCases[0].Mocks[1].Unresolved = MockEntryUnresolvedAmbiguous
	y, err = yaml.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var back Mapping
	if err := yaml.Unmarshal(y, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shareShape(&back), shareShape(full)) || !reflect.DeepEqual(back.Groups, full.Groups) {
		t.Fatalf("yaml round trip: %v %+v; want %v %+v", shareShape(&back), back.Groups, shareShape(full), full.Groups)
	}
	j, err = json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	back = Mapping{}
	if err := json.Unmarshal(j, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shareShape(&back), shareShape(full)) || !reflect.DeepEqual(back.Groups, full.Groups) {
		t.Fatalf("json round trip: %v %+v", shareShape(&back), back.Groups)
	}
	b, err := bson.Marshal(full.Groups[1])
	if err != nil {
		t.Fatal(err)
	}
	var g ConcurrencyGroup
	if err := bson.Unmarshal(b, &g); err != nil || !reflect.DeepEqual(g, full.Groups[1]) {
		t.Fatalf("bson round trip of a group: %+v, %v", g, err)
	}
	b, err = bson.Marshal(full.TestCases[0].Mocks[1])
	if err != nil {
		t.Fatal(err)
	}
	var e MockEntry
	if err := bson.Unmarshal(b, &e); err != nil || e.Group != "g1" || e.Unresolved != MockEntryUnresolvedAmbiguous {
		t.Fatalf("bson round trip of an entry: %+v, %v", e, err)
	}
}
