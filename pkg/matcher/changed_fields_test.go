package matcher

import (
	"reflect"
	"sort"
	"testing"
)

func TestChangedJSONFieldPaths(t *testing.T) {
	tests := []struct {
		name     string
		exp      string
		act      string
		known    map[string][]string
		exclude  func(string) bool
		expected []string
	}{
		{
			name:     "value change is reported",
			exp:      `{"id":"a","name":"x"}`,
			act:      `{"id":"b","name":"x"}`,
			expected: []string{"id"},
		},
		{
			name:     "type change is reported",
			exp:      `{"id":1}`,
			act:      `{"id":"1"}`,
			expected: []string{"id"},
		},
		{
			name:     "removed field is reported",
			exp:      `{"id":"a","extra":"y"}`,
			act:      `{"id":"a"}`,
			expected: []string{"extra"},
		},
		{
			name:     "pure addition is NOT reported",
			exp:      `{"id":"a"}`,
			act:      `{"id":"a","new":"z"}`,
			expected: nil,
		},
		{
			name:     "nested value change uses dotted path",
			exp:      `{"user":{"id":"a","name":"x"}}`,
			act:      `{"user":{"id":"b","name":"x"}}`,
			expected: []string{"user.id"},
		},
		{
			name:     "array element churn collapses to [] path",
			exp:      `{"items":[{"v":"a"}]}`,
			act:      `{"items":[{"v":"b"}]}`,
			expected: []string{"items[].v"},
		},
		{
			name:     "known path is excluded",
			exp:      `{"id":"a","ts":"1"}`,
			act:      `{"id":"b","ts":"2"}`,
			known:    map[string][]string{"id": {}},
			expected: []string{"ts"},
		},
		{
			name:     "excludeRecordedValue drops matching field",
			exp:      `{"id":"a","secret":"****"}`,
			act:      `{"id":"b","secret":"realtoken"}`,
			exclude:  func(v string) bool { return v == "****" },
			expected: []string{"id"},
		},
		{
			name:     "non-JSON returns nil",
			exp:      `not json`,
			act:      `{"id":"a"}`,
			expected: nil,
		},
		{
			name:     "identical bodies return nil",
			exp:      `{"id":"a"}`,
			act:      `{"id":"a"}`,
			expected: nil,
		},
		// A pattern-guarded entry excuses a field only when the recorded and the
		// live value each match it.
		{
			name:     "pattern: a live value outside a pattern the recorded value matches is a change",
			exp:      `{"id":1,"name":"a"}`,
			act:      `{"id":2,"name":"a"}`,
			known:    map[string][]string{"id": {"^1$"}},
			expected: []string{"id"},
		},
		{
			name:     "pattern: a recorded value outside a pattern the live value matches is a change",
			exp:      `{"id":2,"name":"a"}`,
			act:      `{"id":1,"name":"a"}`,
			known:    map[string][]string{"id": {"^1$"}},
			expected: []string{"id"},
		},
		{
			name:     "pattern: both values inside it are excused",
			exp:      `{"id":1,"name":"a"}`,
			act:      `{"id":2,"name":"a"}`,
			known:    map[string][]string{"id": {"^[12]$"}},
			expected: nil,
		},
		{
			name:     "pattern: a matched field the live body lacks is removed",
			exp:      `{"id":1,"name":"a"}`,
			act:      `{"name":"a"}`,
			known:    map[string][]string{"id": {"^1$"}},
			expected: []string{"id"},
		},
		{
			name:     "pattern: an array element outside it is a change though another element matches",
			exp:      `{"ids":[1,1]}`,
			act:      `{"ids":[1,2]}`,
			known:    map[string][]string{"ids": {"^1$"}},
			expected: []string{"ids[]"},
		},
		{
			name:     "pattern: unmatched elements still compare first",
			exp:      `{"ids":[1,5]}`,
			act:      `{"ids":[1,6]}`,
			known:    map[string][]string{"ids": {"^1$"}},
			expected: []string{"ids[]"},
		},
		{
			name:     "pattern: an unmatched element against matched ones is a change",
			exp:      `{"ids":[5]}`,
			act:      `{"ids":[1]}`,
			known:    map[string][]string{"ids": {"^1$"}},
			expected: []string{"ids[]"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ChangedJSONFieldPaths(tc.exp, tc.act, tc.known, nil, false, tc.exclude)
			sort.Strings(got)
			want := append([]string(nil), tc.expected...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("got %v, want %v", got, want)
				}
			}
		})
	}
}

// A recorded value the obfuscator redacted is not compared, but where an
// entry with patterns guards the path the live value alone is held to them:
// one outside them, or none, is a change; one inside them is not. A path no
// pattern guards keeps the rule. Before, the redacted recorded value excused
// the path whatever the live value.
func TestChangedJSONFieldPathsHoldsTheLiveValueUnderARedactedRecording(t *testing.T) {
	redacted := func(v string) bool { return v == "REDACTED_abc" }
	guarded := map[string][]string{"token": {`^t-\d$`}}
	rec := `{"token":"REDACTED_abc","n":1}`
	for _, c := range []struct {
		name  string
		known map[string][]string
		live  string
		want  []string
	}{
		{"the live value inside", guarded, `{"token":"t-1","n":1}`, nil},
		{"the live value outside", guarded, `{"token":"evil","n":1}`, []string{"token"}},
		{"the live value of another type", guarded, `{"token":7,"n":1}`, []string{"token"}},
		{"no live value", guarded, `{"n":1}`, []string{"token"}},
		{"no pattern guards it", nil, `{"token":"evil","n":1}`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ChangedJSONFieldPaths(rec, c.live, c.known, nil, false, redacted)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("changed %v; want %v", got, c.want)
			}
		})
	}
}

// Under a redacted recorded value, every recorded value of the guarded path
// the obfuscator did not redact is held to the patterns too, wherever it
// stands in an array; a mixed-case key is named by the lowercase entry
// keploy.yml's keys are read as.
func TestChangedJSONFieldPathsHoldsEveryUnredactedRecordedValue(t *testing.T) {
	redacted := func(v string) bool { return v == "REDACTED_abc" }
	guarded := map[string][]string{"token": {`^t-\d$`}}
	for _, c := range []struct {
		name, rec, live string
		want            []string
	}{
		{"an unredacted value outside, the redacted one last", `{"items":[{"token":"bbbbbb"},{"token":"REDACTED_abc"}]}`, `{"items":[{"token":"t-1"},{"token":"t-2"}]}`, []string{"items[].token"}},
		{"an unredacted value outside, the redacted one first", `{"items":[{"token":"REDACTED_abc"},{"token":"bbbbbb"}]}`, `{"items":[{"token":"t-1"},{"token":"t-2"}]}`, []string{"items[].token"}},
		{"an unredacted value inside, the redacted one last", `{"items":[{"token":"t-3"},{"token":"REDACTED_abc"}]}`, `{"items":[{"token":"t-1"},{"token":"t-2"}]}`, nil},
		{"a mixed-case key, the live value outside", `{"Token":"REDACTED_abc","n":1}`, `{"Token":"evil","n":1}`, []string{"Token"}},
		{"a mixed-case key, the live value inside", `{"Token":"REDACTED_abc","n":1}`, `{"Token":"t-1","n":1}`, nil},
		{"a mixed-case array, an unredacted value inside", `{"Items":[{"Token":"t-3"},{"Token":"REDACTED_abc"}]}`, `{"Items":[{"Token":"t-1"},{"Token":"t-2"}]}`, nil},
		{"a mixed-case array, an unredacted value outside", `{"Items":[{"Token":"bbbbbb"},{"Token":"REDACTED_abc"}]}`, `{"Items":[{"Token":"t-1"},{"Token":"t-2"}]}`, []string{"Items[].Token"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ChangedJSONFieldPaths(c.rec, c.live, guarded, nil, false, redacted); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("changed %v; want %v", got, c.want)
			}
		})
	}
}
