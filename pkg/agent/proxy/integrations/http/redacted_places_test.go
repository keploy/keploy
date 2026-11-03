package http

import (
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A body field is redacted when the recording redacted it, a field inside
// it, or the whole body.
func TestRedactedPlacesFieldIsARedactedFieldOrInsideOne(t *testing.T) {
	p := redactedPlaces{fields: map[string]bool{"a": true}}
	for name, want := range map[string]bool{"a": true, "a.b": true, "a[]": true, "a[].b": true, "ab": false, "b": false} {
		if got := p.field(name); got != want {
			t.Errorf("field(%q) = %v, want %v", name, got, want)
		}
	}
	if !(redactedPlaces{body: true}).field("x") {
		t.Error("a field of a body the recording redacted whole is not redacted")
	}
}

// No mock is no place redacted, not a crash.
func TestPlacesOfNoMockIsNoPlaces(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("placesOf(nil) panicked: %v", r)
		}
	}()
	p := placesOf(nil)
	if p.body || len(p.fields) > 0 || p.decodedPath("/x/y") != "/x/y" {
		t.Errorf("placesOf(nil) = %+v, want no places", p)
	}
}

// A redacted path keeps what holds no value: a path with no leading "/" (the
// request line "OPTIONS *") stays without one, a path diff that carries no
// values (a refused clock read in the path) carries none. A recording whose
// URL does not parse has no segments counted: at a mock that redacted
// something its path diff is redacted whole on both sides, at one that
// redacted nothing it is kept. A path diff value that is not the path it
// names is redacted whole: its segments cannot be counted.
func TestRedactedPathKeepsWhatHoldsNoValue(t *testing.T) {
	noise := []string{`^REDACTED_abc$`}
	places := redactedPlacesOf(&models.HTTPReq{URL: "/x/REDACTED_abc"}, noise)
	if got := places.summary("OPTIONS", "*"); got != "OPTIONS *" {
		t.Errorf("summary of OPTIONS * = %q", got)
	}
	if got := places.summary("GET", ""); got != "GET " {
		t.Errorf("summary of an empty path = %q", got)
	}
	diffs := []models.MockFieldDiff{
		{Path: "path", Kind: models.DiffKindValueChanged},
		{Path: "path", Kind: models.DiffKindValueChanged, Expected: "/x/REDACTED_abc", Actual: "/x/" + liveSecret},
	}
	redactFieldDiffs(diffs, places, "/other/"+liveSecret)
	if diffs[0].Expected != "" || diffs[0].Actual != "" {
		t.Errorf("a path diff with no values got %q, %q", diffs[0].Expected, diffs[0].Actual)
	}
	if diffs[1].Expected != "/x/"+valueRedacted || diffs[1].Actual != valueRedacted {
		t.Errorf("a live path value that is not the call's path got %q, %q, want it redacted whole", diffs[1].Expected, diffs[1].Actual)
	}
	unparsed := redactedPlacesOf(&models.HTTPReq{URL: "http://store/%zz"}, noise)
	whole := []models.MockFieldDiff{{Path: "path", Kind: models.DiffKindValueChanged, Expected: "http://store/%zz", Actual: "/x/" + liveSecret}}
	redactFieldDiffs(whole, unparsed, "/x/"+liveSecret)
	if whole[0].Expected != "/"+valueRedacted || whole[0].Actual != "/"+valueRedacted {
		t.Errorf("a recording with no segments counted, at a mock that redacted something, got %q, %q", whole[0].Expected, whole[0].Actual)
	}
	kept := []models.MockFieldDiff{{Path: "path", Kind: models.DiffKindValueChanged, Expected: "http://store/%zz", Actual: "/x"}}
	redactFieldDiffs(kept, redactedPlacesOf(&models.HTTPReq{URL: "http://store/%zz"}, nil), "/x")
	if kept[0].Expected != "http://store/%zz" || kept[0].Actual != "/x" {
		t.Errorf("a recording with no segments counted, at a mock that redacted nothing, got %q, %q", kept[0].Expected, kept[0].Actual)
	}
}

// The miss's error names the call as its report does, or, with no report,
// by its path's shape.
func TestMissedCallNamesTheCallWithoutItsSecret(t *testing.T) {
	request, _ := http.NewRequest("GET", "http://store/x/"+liveSecret, nil)
	if got := missedCall(request, nil); got != "GET /{}/{}" {
		t.Errorf("with no report: %q", got)
	}
	if got := missedCall(request, &models.MockMismatchReport{ActualSummary: "GET /x/" + valueRedacted}); got != "GET /x/"+valueRedacted {
		t.Errorf("with a report: %q", got)
	}
}

// The recording's side of a report names its path as the call's request
// line is read: a path-only recording that url.Parse cannot read
// ("//api:v1/..." reads there as a host with a bad port) is a path, its
// redacted segment shown redacted; a recorded URL that does not read at all
// is redacted whole at a mock that redacted something (its places cannot be
// found), and shown as it was recorded at one that redacted nothing.
func TestRenderedRecordingReadsAPathOnlyURLAsARequestLine(t *testing.T) {
	value := []string{`^REDACTED_abc$`}
	for _, c := range []struct {
		url   string
		noise []string
		want  string
	}{
		{"//api:v1/REDACTED_abc", value, "GET //api:v1/" + valueRedacted},
		{"/%zz/REDACTED_abc", value, "GET /" + valueRedacted},
		{"/%zz/a", nil, "GET /%zz/a"},
	} {
		got, _, _ := strings.Cut(renderMockRequest(&models.HTTPReq{Method: "GET", URL: c.url}, c.noise), "\n")
		if got != c.want {
			t.Errorf("the recording %q renders as %q, want %q", c.url, got, c.want)
		}
	}
}

// A body not read for fields holds a value one of the mock's patterns names
// when the pattern, its anchors dropped, matches anywhere in it: a leading
// "^" and a trailing "$" are anchors, a quoted "\$" is not, and a pattern
// that names nothing, or does not compile, matches nothing.
func TestHoldsPatternReadsAValueAnywhere(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns []string
		text     string
		want     bool
	}{
		{"an anchored value inside XML", []string{`^REDACTED_abc$`}, `<a>REDACTED_abc</a>`, true},
		{"a value not there", []string{`^REDACTED_abc$`}, `<a>alpha1</a>`, false},
		{"a quoted dollar is the value's", []string{`REDACTED_abc\$`}, `pay REDACTED_abc$ now`, true},
		{"a quoted dollar is not an anchor", []string{`REDACTED_abc\$`}, `pay REDACTED_abc now`, false},
		{"a quoted backslash before the anchor", []string{`^REDACTED\\$`}, `x REDACTED\ y`, true},
		{"a pattern that names nothing", []string{`^$`}, `alpha1`, false},
		{"a pattern that does not compile", []string{`^(REDACTED$`}, `(REDACTED`, false},
		{"the second pattern", []string{`^REDACTED_def$`, `^REDACTED_abc$`}, `x REDACTED_abc`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("holdsPattern(%q, %q) panicked: %v", tc.patterns, tc.text, r)
				}
			}()
			if got := holdsPattern(tc.patterns, tc.text); got != tc.want {
				t.Errorf("holdsPattern(%q, %q) = %v, want %v", tc.patterns, tc.text, got, tc.want)
			}
		})
	}
}
