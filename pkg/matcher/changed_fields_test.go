package matcher

import (
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ChangedJSONFieldPaths(tc.exp, tc.act, tc.known, tc.exclude)
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
