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

// Restore undoes Rewrite: a call carrying an id this run made is matched as
// the recording's call; a pair the map banned (one id seen with two) is never
// restored.
func TestRestore(t *testing.T) {
	m := &Map{}
	if got := m.Restore(`{"_id":"6adba2fb728c417969374a1e"}`); got != `{"_id":"6adba2fb728c417969374a1e"}` {
		t.Fatalf("an empty map changed %s", got)
	}
	m.Add("65f0aaaaaaaaaaaaaaaaaaaa", "6adba2fb728c417969374a1e")
	live := `{"find":"c","filter":{"_id":{"$oid":"6adba2fb728c417969374a1e"}}}`
	want := `{"find":"c","filter":{"_id":{"$oid":"65f0aaaaaaaaaaaaaaaaaaaa"}}}`
	if got := m.Restore(live); got != want {
		t.Fatalf("Restore = %s, want %s", got, want)
	}
	if got := m.Rewrite(m.Restore(live)); got != live {
		t.Fatalf("Rewrite(Restore(s)) = %s, want %s", got, live)
	}
	m.Add("65f0aaaaaaaaaaaaaaaaaaaa", "6adba2fb728c417969374a1f") // the recorded id again, with another
	if got := m.Restore(live); got != live {
		t.Fatalf("a banned pair was restored: %s", got)
	}
}
