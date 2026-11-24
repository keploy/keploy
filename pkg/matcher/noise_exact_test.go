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
