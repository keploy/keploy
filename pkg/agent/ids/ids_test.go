package ids

import "testing"

func TestRewriteSwapsEveryKnownID(t *testing.T) {
	m := New(map[string]string{"6ac392f0dae46469d90e0ae3": "6ac396953b62b2e540e6c498"})
	got := m.Rewrite(`{"id":"6ac392f0dae46469d90e0ae3","url":"/apps/6ac392f0dae46469d90e0ae3/x"}`)
	if got != `{"id":"6ac396953b62b2e540e6c498","url":"/apps/6ac396953b62b2e540e6c498/x"}` {
		t.Fatal(got)
	}
}

func TestAConflictingPairDropsTheIDInsteadOfGuessing(t *testing.T) {
	m := &Map{}
	m.Add("aaaa", "bbbb")
	m.Add("aaaa", "bbbb")
	m.Add("eeee", "ff")
	if got := m.Pairs(); len(got) != 1 || got["aaaa"] != "bbbb" {
		t.Fatalf("pairs = %v", got)
	}
	m.Add("aaaa", "cccc")
	if got := m.Pairs(); len(got) != 0 {
		t.Fatalf("an id seen paired two ways must not be swapped at all: %v", got)
	}
	m.Add("aaaa", "bbbb")
	if got := m.Pairs(); len(got) != 0 {
		t.Fatalf("a dropped id stays dropped: %v", got)
	}
	m.Add("1111", "2222")
	m.Add("3333", "2222")
	if got := m.Pairs(); len(got) != 0 {
		t.Fatalf("two recorded ids claiming one new id are both dropped: %v", got)
	}
}

func TestRestoreTurnsTheRunsIDsBackIntoTheRecordedOnes(t *testing.T) {
	m := New(map[string]string{"11f4a513-bee3-45e7-bd44-62be219cdcfd": "5edbe3af-8cda-452a-8780-868f15a19ced"})
	if got := m.Restore("/keploy.io/5edbe3af-8cda-452a-8780-868f15a19ced"); got != "/keploy.io/11f4a513-bee3-45e7-bd44-62be219cdcfd" {
		t.Fatal(got)
	}
	m.Add("aaaa", "bbbb")
	if got := m.Restore("bbbb"); got != "aaaa" {
		t.Fatalf("a pair added later is restored too: %s", got)
	}
	m.Add("aaaa", "cccc")
	if got := m.Restore("bbbb"); got != "bbbb" {
		t.Fatalf("a dropped pair is not restored: %s", got)
	}
	m.Reset()
	if got := m.Restore("5edbe3af-8cda-452a-8780-868f15a19ced"); got != "5edbe3af-8cda-452a-8780-868f15a19ced" {
		t.Fatalf("nothing is restored after a reset: %s", got)
	}
}

func TestIsTimeReadsTheTimesARunMakes(t *testing.T) {
	for _, s := range []string{"1792054012", "1792055845379", "1792055845379123", "1792055845379123456", "2026-10-15T06:21:59Z", "2026-10-15T06:21:59.123456+05:30"} {
		if !IsTime(s) {
			t.Errorf("%s is a time", s)
		}
	}
	for _, s := range []string{"0", "200", "123456789", "99999999999", "9999999999", "4102444800000000", "2026-10-15", "tomorrow", ""} {
		if IsTime(s) {
			t.Errorf("%s is not a time", s)
		}
	}
	if !IsID("6ad092fc5b32851dc1de69be") || !IsID("5edbe3af-8cda-452a-8780-868f15a19ced") || IsID("6ad092fc5b32851dc1de69bz") || IsID("1792054012") {
		t.Fatal("ids")
	}
}

func TestANumberIsSwappedOnlyWhereItIsAWholeNumber(t *testing.T) {
	m := New(map[string]string{"1792054012": "1792100000"})
	in := `{"at":{"$numberLong":"1792054012"},"ms":{"$numberLong":"1792054012345"},"id":"6ad0701792054012ab958118","v":"x1792054012","f":0.1792054012,"q":"/k?t=1792054012&u=1"}`
	want := `{"at":{"$numberLong":"1792100000"},"ms":{"$numberLong":"1792054012345"},"id":"6ad0701792054012ab958118","v":"x1792054012","f":0.1792054012,"q":"/k?t=1792100000&u=1"}`
	if got := m.Rewrite(in); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if got := m.Restore(want); got != in {
		t.Fatalf("restore: %s", got)
	}
}

func TestPairPairsEachIDAndATimeByItsField(t *testing.T) {
	m := &Map{}
	m.PairJSON([]byte(`{"data":{"_id":"11f4a513-bee3-45e7-bd44-62be219cdcfd","createdAt":1792054012,"authTime":1792054012,"name":"k","n":3,"list":[{"at":"2026-10-15T06:21:59Z"}]}}`),
		[]byte(`{"data":{"_id":"5edbe3af-8cda-452a-8780-868f15a19ced","createdAt":1792100001,"authTime":1792100000,"name":"j","n":4,"list":[{"at":"2026-10-16T09:00:00Z"}]}}`))
	m.PairPath("/v1/keys/6ad092fc81cde9599b98d950/x", "/v1/keys/6ad092fc5b32851dc1de69be/x")
	m.PairPath("/v1/keys/a", "/v1/keys/a/b")
	got := m.Rewrite(`{"_id":"11f4a513-bee3-45e7-bd44-62be219cdcfd","owner":"6ad092fc81cde9599b98d950","createdAt":{"$numberLong":"1792054012"},"authTime":1792054012,"at":"2026-10-15T06:21:59Z","other":1792054012,"$gt":{"$date":{"$numberLong":"1792054012"}}}`)
	want := `{"_id":"5edbe3af-8cda-452a-8780-868f15a19ced","owner":"6ad092fc5b32851dc1de69be","createdAt":{"$numberLong":"1792100001"},"authTime":1792100000,"at":"2026-10-16T09:00:00Z","other":1792054012,"$gt":{"$date":{"$numberLong":"1792054012"}}}`
	if got != want {
		t.Fatalf("one recorded second, written by two fields in two live seconds, follows each field:\n got %s\nwant %s", got, want)
	}
	if back := m.Restore(want); back != `{"_id":"11f4a513-bee3-45e7-bd44-62be219cdcfd","owner":"6ad092fc81cde9599b98d950","createdAt":{"$numberLong":"1792054012"},"authTime":1792054012,"at":"2026-10-15T06:21:59Z","other":1792054012,"$gt":{"$date":{"$numberLong":"1792054012"}}}` {
		t.Fatalf("restore: %s", back)
	}
}
