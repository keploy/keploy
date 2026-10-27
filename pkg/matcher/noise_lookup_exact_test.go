package matcher

import "testing"

// Declared body noise names whole fields (P093): an entry is a run of whole
// fields of the path, at any depth, with what lies under it, never a part of
// a field's name. As a substring, noise "id" excused "paid" and "identifier",
// and "user.id" excused "superuser.id": a request differing there was served
// a recording it is not.
func TestDeclaredNoiseNamesWholeFields(t *testing.T) {
	for _, tc := range []struct {
		entry string
		path  string
		want  bool
	}{
		{"id", "id", true},
		{"id", "user.id", true},
		{"id", "items[].id", true},
		{"id", "id.sub", true},   // what lies under it
		{"id", "id[]", true},     // an array of that name, and its elements
		{"id", "valid.id", true}, // a later whole field, after a part of a name
		{"id", "[].id", true},    // under a top-level array
		{"id", "paid", false},    // part of another field's name
		{"id", "identifier", false},
		{"id", "paid.amount", false},
		{"id", "ids[]", false},
		{"user.id", "user.id", true},
		{"user.id", "data.user.id", true},
		{"user.id", "user.id.x", true},
		{"user.id", "superuser.id", false},
		{"user.id", "user.identity", false},
		{"user.id", "data.superuser.id", false},
		{"items[].at", "items[].at", true},
		{"items[].at", "order.items[].at", true},
		{"items[].at", "items[].attempt", false},
		{"items[].at", "myitems[].at", false},
		{"user", "user[id]", false},    // a form field's name, not an array of user
		{"price", "price[usd]", false}, // a key that holds a bracket is one name
		{"user[id]", "user[id]", true},
		{"ids", "ids[]", true},                // a repeated form field is the array of that name
		{"createdAt", "data.createdAt", true}, // field names compare without case, as keploy.yml's keys are read
		{"", "anything", false},               // an empty entry names no field
	} {
		l := NewNoiseLookup(map[string][]string{tc.entry: {}})
		if got := l.Covers(tc.path, "1", true); got != tc.want {
			t.Errorf("noise %q covers %q: %v, want %v", tc.entry, tc.path, got, tc.want)
		}
	}
}
