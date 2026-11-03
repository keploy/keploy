package matcher

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// patternReasons are the reasons of an assessment that name a keploy.yml
// noise pattern.
func patternReasons(t *testing.T, exp, act string, noise map[string][]string) []string {
	t.Helper()
	assess, err := ComputeFailureAssessmentJSON(exp, act, noise, false)
	if err != nil || assess == nil {
		t.Fatalf("assessment: %v, %v", assess, err)
	}
	var out []string
	for _, r := range assess.Reasons {
		if strings.HasPrefix(r, "keploy.yml's noise") {
			out = append(out, r)
		}
	}
	return out
}

// A test's response comparison names a field keploy.yml's noise lets vary
// only within patterns when a side's value is outside them: the field, the
// patterns and the side, by its shape and never its text.
func TestFailureAssessmentNamesTheNoisePatternAndTheSide(t *testing.T) {
	cases := []struct {
		name     string
		exp, act string
		noise    map[string][]string
		want     []string
	}{
		{
			name:  "the live value is outside the pattern",
			exp:   `{"id":1,"n":"a"}`,
			act:   `{"id":22,"n":"a"}`,
			noise: map[string][]string{"id": {"^1$"}},
			want:  []string{PatternWhy("id", []string{"^1$"}, "live", "a number of 2 characters")},
		},
		{
			name:  "the recorded value is outside the pattern",
			exp:   `{"id":22,"n":"a"}`,
			act:   `{"id":1,"n":"a"}`,
			noise: map[string][]string{"id": {"^1$"}},
			want:  []string{PatternWhy("id", []string{"^1$"}, "recorded", "a number of 2 characters")},
		},
		{
			name:  "the live side lacks the field",
			exp:   `{"id":1,"n":"a"}`,
			act:   `{"n":"a"}`,
			noise: map[string][]string{"id": {"^1$"}},
			want:  []string{PatternWhy("id", []string{"^1$"}, "live", "absent")},
		},
		{
			name:  "a string by its length, a second pattern quoted too",
			exp:   `{"token":"tok-abc"}`,
			act:   `{"token":"secret"}`,
			noise: map[string][]string{"token": {"^tok-", "^key-"}},
			want:  []string{PatternWhy("token", []string{"^tok-", "^key-"}, "live", "a string of 6 bytes")},
		},
		{
			name:  "a boolean and null",
			exp:   `{"ok":true,"v":null}`,
			act:   `{"ok":false,"v":"x"}`,
			noise: map[string][]string{"ok": {"^true$"}, "v": {"^null$"}},
			want: []string{
				PatternWhy("ok", []string{"^true$"}, "live", "a boolean"),
				PatternWhy("v", []string{"^null$"}, "live", "a string of 1 bytes"),
			},
		},
		{
			name:  "the recorded null is outside the pattern",
			exp:   `{"v":null}`,
			act:   `{"v":"x"}`,
			noise: map[string][]string{"v": {"^x$"}},
			want:  []string{PatternWhy("v", []string{"^x$"}, "recorded", "null")},
		},
		{
			name:  "an array's scalar elements are each held to its pattern",
			exp:   `{"o":{"tags":["t1"]}}`,
			act:   `{"o":{"tags":["x1"]}}`,
			noise: map[string][]string{"o.tags": {"^t"}},
			want:  []string{PatternWhy("o.tags[]", []string{"^t"}, "live", "a string of 2 bytes")},
		},
		{
			// The comparison accepts every value under an object a dotted
			// pattern-guarded entry names, so no pattern decided o.obj.a.
			name:  "under an object a dotted pattern guards no pattern is named",
			exp:   `{"o":{"obj":{"a":1}},"n":1}`,
			act:   `{"o":{"obj":{"a":2}},"n":2}`,
			noise: map[string][]string{"o.obj": {"^x$"}},
		},
		{
			// A dot-free entry excuses a container it names whole.
			name:  "under a container a field-name pattern guards no pattern is named",
			exp:   `{"obj":{"a":1},"n":1}`,
			act:   `{"obj":{"a":2},"n":2}`,
			noise: map[string][]string{"obj": {"^x$"}},
		},
		{
			// A dot-free entry names a field by its whole name; the path maps
			// read it as a substring of "valid".
			name:  "a field-name pattern does not name a field that contains it",
			exp:   `{"valid":true}`,
			act:   `{"valid":false}`,
			noise: map[string][]string{"id": {"^1$"}},
		},
		{
			// The comparison holds o.id to the field-name pattern first, the
			// path maps to the dotted one: no one pattern decided it.
			name:  "two patterns the comparison and the path maps read differently",
			exp:   `{"o":{"id":2}}`,
			act:   `{"o":{"id":3}}`,
			noise: map[string][]string{"id": {"^1$"}, "o.id": {"^2$"}},
		},
		{
			name:  "an array's elements, one inside the pattern and one outside",
			exp:   `{"o":{"tags":["t1","t2"]}}`,
			act:   `{"o":{"tags":["t1","x1"]}}`,
			noise: map[string][]string{"o.tags": {"^t"}},
			want:  []string{PatternWhy("o.tags[]", []string{"^t"}, "live", "a string of 2 bytes")},
		},
		{
			// A dot-free entry excuses an array it names whole.
			name:  "a field-name pattern on an array names none of its elements",
			exp:   `{"tags":["t1"],"n":1}`,
			act:   `{"tags":["x1"],"n":2}`,
			noise: map[string][]string{"tags": {"^t"}},
		},
		{
			name:  "a dotted pattern on an array of arrays names none of its values",
			exp:   `{"o":{"m":[["t1"]]},"n":1}`,
			act:   `{"o":{"m":[["x1"]]},"n":2}`,
			noise: map[string][]string{"o.m": {"^t"}},
		},
		{
			name:  "a dotted pattern on an array that holds an object names none of its values",
			exp:   `{"o":{"m":["t1",{"a":1}]},"n":1}`,
			act:   `{"o":{"m":["x1",{"a":1}]},"n":2}`,
			noise: map[string][]string{"o.m": {"^t"}},
		},
		{
			// The comparison excuses o whole, so o.id is not compared.
			name:  "a field under a container a field-name pattern guards, its own path guarded too",
			exp:   `{"o":{"id":1},"n":1}`,
			act:   `{"o":{"id":2},"n":2}`,
			noise: map[string][]string{"o": {"^x$"}, "o.id": {"^1$"}},
		},
		{
			// A field-name entry excuses a value that became an object.
			name:  "a field that became a container names no pattern",
			exp:   `{"id":1,"n":1}`,
			act:   `{"id":{"x":1},"n":2}`,
			noise: map[string][]string{"id": {"^1$"}},
		},
		{
			name:  "a field no pattern guards is not named",
			exp:   `{"id":1,"n":"a"}`,
			act:   `{"id":1,"n":"b"}`,
			noise: map[string][]string{"id": {"^1$"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := patternReasons(t, tc.exp, tc.act, tc.noise)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("pattern reasons:\n got %q\nwant %q", got, tc.want)
			}
			for _, r := range got {
				if strings.Contains(r, "secret") || strings.Contains(r, "22") {
					t.Fatalf("a reason holds a guarded value: %q", r)
				}
			}
		})
	}
}

func TestHeaderPatternWhysNameTheHeaderPatternAndSide(t *testing.T) {
	noise := map[string][]string{"X-Req": {"^r-"}}
	cases := []struct {
		name     string
		rec, act http.Header
		want     []string
	}{
		{
			name: "the live value is outside the pattern",
			rec:  http.Header{"X-Req": {"r-1"}},
			act:  http.Header{"X-Req": {"zz"}},
			want: []string{PatternWhy("header.X-Req", []string{"^r-"}, "live", "a string of 2 bytes")},
		},
		{
			name: "the recorded value is outside the pattern",
			rec:  http.Header{"X-Req": {"zzz"}},
			act:  http.Header{"X-Req": {"r-2"}},
			want: []string{PatternWhy("header.X-Req", []string{"^r-"}, "recorded", "a string of 3 bytes")},
		},
		{
			name: "the live side lacks the header",
			rec:  http.Header{"X-Req": {"r-1"}},
			act:  http.Header{},
			want: []string{PatternWhy("header.X-Req", []string{"^r-"}, "live", "absent")},
		},
		{
			name: "the recorded side lacks the header",
			rec:  http.Header{},
			act:  http.Header{"X-Req": {"r-1"}},
			want: []string{PatternWhy("header.X-Req", []string{"^r-"}, "recorded", "absent")},
		},
		{
			name: "a second live value outside the pattern",
			rec:  http.Header{"X-Req": {"r-1"}},
			act:  http.Header{"X-Req": {"r-1", "zz"}},
			want: []string{PatternWhy("header.X-Req", []string{"^r-"}, "live", "a string of 2 bytes")},
		},
		{
			name: "both inside the pattern, another header differs",
			rec:  http.Header{"X-Req": {"r-1"}, "X-Other": {"a"}},
			act:  http.Header{"X-Req": {"r-2"}, "X-Other": {"b"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var res []models.HeaderResult
			if CompareHeaders(tc.rec, tc.act, &res, noise) {
				t.Fatal("the headers matched")
			}
			if got := HeaderPatternWhys(res, noise); !slices.Equal(got, tc.want) {
				t.Fatalf("header reasons:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
