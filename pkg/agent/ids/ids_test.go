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
