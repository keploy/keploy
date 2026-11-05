package models

import (
	"reflect"
	"testing"
)

// MockOwners gives each mock the tests that own it, once each and sorted,
// so both replays name the same test: the first of them.
func TestMockOwnersAreEachMocksTestsSorted(t *testing.T) {
	got := MockOwners(map[string][]string{
		"o::TestE": {"mock-0"},
		"o::TestC": {"mock-0", "mock-1", "mock-0"},
		"o::TestA": {"mock-0"},
		"o::TestD": {"mock-0"},
		"o::TestB": {"mock-0"},
		"o::TestF": nil,
	})
	want := map[string][]string{
		"mock-0": {"o::TestA", "o::TestB", "o::TestC", "o::TestD", "o::TestE"},
		"mock-1": {"o::TestC"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owners %v; want %v", got, want)
	}
}

// NameRecordedIn names the first other test that owns the recording a
// NotOwned call is, and only that: a call that is not NotOwned, has no
// closest recording, is a recording no test owns, or one its own test owns
// too, keeps RecordedIn as it was.
func TestNameRecordedInNamesTheFirstOtherOwner(t *testing.T) {
	owners := MockOwners(map[string][]string{"o::TestC": {"mock-0"}, "o::TestA": {"mock-0"}, "o::TestB": {"mock-1"}, "o::TestD": {"mock-1"}, "o::TestE": {""}})
	for _, tc := range []struct {
		name string
		call UnmatchedCall
		want string
	}{
		{"another test's recording", UnmatchedCall{NotOwned: true, ClosestMock: "mock-0"}, "o::TestA"},
		{"not the recording it is closest to", UnmatchedCall{ClosestMock: "mock-0"}, ""},
		{"no closest recording", UnmatchedCall{NotOwned: true}, ""},
		{"a recording no test owns", UnmatchedCall{NotOwned: true, ClosestMock: "mock-9"}, ""},
		{"a recording its own test owns too", UnmatchedCall{NotOwned: true, ClosestMock: "mock-1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.call
			named := c.NameRecordedIn("o::TestB", owners)
			if c.RecordedIn != tc.want || named != (tc.want != "") {
				t.Fatalf("recorded in %q, named %v; want %q", c.RecordedIn, named, tc.want)
			}
		})
	}
}

// A miss's sanitized copy keeps the test that recorded it and whether it is
// order-dependent: both name tests, no value, and a receipt that writes the
// copy must still say why the test failed.
func TestASanitizedMissKeepsWhereItWasRecordedAndThatItIsOrderDependent(t *testing.T) {
	c := UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /token", NotOwned: true, ClosestMock: "mock-0", RecordedIn: "o::TestA", OrderDependent: true}
	s := c.Sanitized()
	if s.RecordedIn != "o::TestA" || !s.OrderDependent || !s.NotOwned {
		t.Fatalf("the sanitized miss lost where it was recorded or that it is order-dependent: %+v", s)
	}
}
