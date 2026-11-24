package matcher

import "testing"

func TestJSONMatchKeyLeavesOutAnExactEntry(t *testing.T) {
	noise := map[string][]string{ExactKey("version"): {}, ExactKey("data[].at"): {}}
	a := JSONMatchKey(map[string]interface{}{"version": "a", "data": []interface{}{map[string]interface{}{"at": 1.0, "n": 1.0}}}, noise)
	b := JSONMatchKey(map[string]interface{}{"version": "b", "data": []interface{}{map[string]interface{}{"at": 2.0, "n": 1.0}}}, noise)
	if a != b {
		t.Fatal("two documents the comparison finds equal under the exact entries have different keys")
	}
}

func TestExactCovers(t *testing.T) {
	for _, c := range []struct {
		path, entry string
		want        bool
	}{
		{"version", "version", true},
		{"data.version", "version", false},
		{"data.versionlabel", "data.version", false},
		{"data.version.major", "data.version", true},
		{"data[].at", "data.at", true},
		{"[].heartbeats[].at", "heartbeats.at", true},
		{"[].heartbeats[].at", "heartbeats", true},
		{"version", "", false},
		{".version", "version", true},
	} {
		if got := exactCovers(c.path, c.entry); got != c.want {
			t.Errorf("exactCovers(%q, %q) = %v, want %v", c.path, c.entry, got, c.want)
		}
	}
}

func TestHeaderKeyCovers(t *testing.T) {
	for _, c := range []struct {
		name, key string
		want      bool
	}{
		{"X-Build", ExactKey("x-build"), true},
		{"X-Build-Id", ExactKey("x-build"), false},
		{"X-Build-Id", "x-build", true},
	} {
		if got := HeaderKeyCovers(c.name, c.key); got != c.want {
			t.Errorf("HeaderKeyCovers(%q, %q) = %v, want %v", c.name, c.key, got, c.want)
		}
	}
}

func TestExactKeyCovers(t *testing.T) {
	for _, c := range []struct {
		path, key, rank string
		covers, exact   bool
	}{
		{"version", ExactKey("version"), "version", true, true},
		{"data.Version", ExactKey("data.version"), "data.version", true, true},
		{"data.version", ExactKey("version"), "version", false, true},
		{"items[].at", ExactKey("items.at"), "items.at", true, true},
		{"results.0b7c5d9e-1c1f-4a55-9a8e-7f3c2e1d4b6a.traceid", ExactKey("results.{id}.traceId"), "results.{id}.traceid", true, true},
		{"data.version", "version", "", false, false},
	} {
		rank, covers, exact := ExactKeyCovers(c.path, c.key)
		if rank != c.rank || covers != c.covers || exact != c.exact {
			t.Errorf("ExactKeyCovers(%q, %q) = %q, %v, %v; want %q, %v, %v", c.path, c.key, rank, covers, exact, c.rank, c.covers, c.exact)
		}
	}
}
