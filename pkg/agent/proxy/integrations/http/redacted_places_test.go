package http

import (
	"net/http"
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
// values (a refused clock read in the path) carries none, and a recording
// whose URL does not parse, so has no segments counted, keeps its path diff.
// A path diff value that is not the path it names is redacted whole: its
// segments cannot be counted.
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
	kept := []models.MockFieldDiff{{Path: "path", Kind: models.DiffKindValueChanged, Expected: "http://store/%zz", Actual: "/x"}}
	redactFieldDiffs(kept, unparsed, "/x")
	if kept[0].Expected != "http://store/%zz" || kept[0].Actual != "/x" {
		t.Errorf("a recording with no segments counted got %q, %q", kept[0].Expected, kept[0].Actual)
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
