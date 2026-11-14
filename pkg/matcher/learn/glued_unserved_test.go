package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A changed value whose long word (32 bits or more) stands glued inside a
// longer word of an earlier recording record's full round never served is
// refused in keploy mock (fail closed: the application may have cut it
// from there, and F gave it nothing there to show it did not), and learned
// as noise in keploy test, as before, but naming where the word stands. A
// short group glued, a long word inside another id, and a recording F
// served keep today's reading in both modes.
func TestAWordGluedInsideAnUnservedRecordingsWord(t *testing.T) {
	const (
		path = "header.x-keploy-test-step-id"
		rec  = "3f2a9c1e-4b7d-4e2a-9f3c-1a2b3c4d5e6f"
		live = "8e1d2c3b-a0f9-4c8b-b7a6-5f4e3d2c1b0a"
	)
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: v}}}
	}
	headed := func(name, value string, served bool, before time.Duration) Raw {
		at := eRecAt.Add(-before)
		return RawOf(&models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "https://example.invalid/", Header: map[string]string{"X-Job": value}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `ok`},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		}}, served)
	}
	servedAsRecorded := func(name, value string) Raw {
		return headed(name, value, true, 2*time.Second).ServedAs(1).ServedAsRecorded()
	}
	none := func() ([]GivenPlaces, error) { return nil, nil }
	type mode struct {
		name string
		dc   func() *DriftCache
	}
	modes := []mode{{"keploy mock", mockDrifts}, {"keploy test", clockedDrifts}}
	classify := func(m mode, raw ...Raw) Learned {
		got := ClassifyGiven(m.dc(), []string{path}, shape(live), shape(rec), eRecAt, nil, raw, nil, nil, none)
		if len(got) != 1 {
			t.Fatalf("%s: learned %d leaves, want 1: %+v", m.name, len(got), got)
		}
		return got[0]
	}
	for _, tc := range []struct {
		name, value string
		served      bool
		// glued: the place the reason names; "" where the rule does not
		// read the value (noise in both modes, no glued reason).
		glued string
	}{
		{"the 48-bit word glued after a letter", "c0ffee11-5c8e-4a1b-8c2d-x1a2b3c4d5e6f", false, "mock-1290 at spec.req.header.X-Job"},
		{"the 48-bit word glued before a letter", "c0ffee11-5c8e-4a1b-8c2d-1a2b3c4d5e6fx", false, "mock-1290 at spec.req.header.X-Job"},
		{"the 32-bit word glued in a text", "job x3f2a9c1e", false, "mock-1290 at spec.req.header.X-Job"},
		{"a 16-bit group glued", "job 4e2ax", false, ""},
		{"a 16-bit group inside another id", "c0ffee11-5c8e-4a1b-8c2d-0123456789ab", false, ""},
		{"the 48-bit word as a group of another id", "c0ffee11-5c8e-4a1b-8c2d-1a2b3c4d5e6f", false, ""},
		{"the 32-bit word glued in a decoded form", "job am9iIHgzZjJhOWMxZSE=", false, "mock-1290's payload"},
		{"the 48-bit word glued in a recording F served as recorded", "c0ffee11-5c8e-4a1b-8c2d-x1a2b3c4d5e6f", true, ""},
	} {
		for _, m := range modes {
			x := headed("mock-1290", tc.value, false, 2*time.Second)
			if tc.served {
				x = servedAsRecorded("mock-1290", tc.value)
			}
			got := classify(m, x)
			switch {
			case tc.glued == "":
				if got.Kind != KindNoise || strings.Contains(got.Why, "glued inside") {
					t.Errorf("%s: %s: %s (%q), want noise with no glued reason", m.name, tc.name, got.Kind, got.Why)
				}
			case m.name == "keploy mock":
				if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "stands glued inside a longer word of an earlier recording, in "+tc.glued+", which record's full round never served before this call") {
					t.Errorf("FALSE GREEN? %s: %s: %s (%q), want refused naming %s", m.name, tc.name, got.Kind, got.Why, tc.glued)
				}
			default:
				if got.Kind != KindNoise || !strings.Contains(got.Why, "glued inside "+tc.glued) || !strings.Contains(got.Why, "'s word, an earlier recording record's full round never served before this call") || !strings.HasSuffix(got.Why, "keploy learns it as varying") {
					t.Errorf("SILENT? %s: %s: %s (%q), want noise naming %s's word", m.name, tc.name, got.Kind, got.Why, tc.glued)
				}
			}
		}
	}

	// A value keploy test learns as an echo of its call's own answer is
	// answered with this run's value, never asserted: only noise names the
	// glued word.
	answer := []Leaf{{Path: "body.ref", Type: String, Value: "ref-" + rec + "-x"}}
	for _, m := range modes {
		got := ClassifyGiven(m.dc(), []string{path}, shape(live), shape(rec), eRecAt, nil, []Raw{headed("mock-1290", "job x3f2a9c1e", false, 2*time.Second)}, answer, nil, none)
		if m.name == "keploy test" && (len(got) != 1 || got[0].Kind != KindEcho || strings.Contains(got[0].Why, "glued inside")) {
			t.Errorf("%s: the answer echoes the value: %+v, want an echo with no glued reason", m.name, got)
		}
	}

	// Two long words glued at two recordings: the first word of the value
	// names its place, in both modes.
	for _, m := range modes {
		got := classify(m, headed("mock-1290", "job x3f2a9c1e", false, 3*time.Second), headed("mock-1291", "job x1a2b3c4d5e6f", false, 2*time.Second))
		if !strings.Contains(got.Why, "mock-1290 at spec.req.header.X-Job") || strings.Contains(got.Why, "mock-1291") {
			t.Errorf("%s: two glued places: %s (%q), want the first word's place, mock-1290, alone", m.name, got.Kind, got.Why)
		}
	}
}

// The glued words of an unserved recording are read once per value and
// recording, and only where the value changed a word of 32 bits or more.
func TestGluedWordsAreReadOncePerUnservedRecording(t *testing.T) {
	const path = "header.x-keploy-test-step-id"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: v}}}
	}
	unserved := func(name string) Raw {
		at := eRecAt.Add(-2 * time.Second)
		return RawOf(&models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "https://example.invalid/", Header: map[string]string{"X-Job": "job 0f0f"}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `ok`},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		}}, false)
	}
	none := func() ([]GivenPlaces, error) { return nil, nil }
	raw := []Raw{unserved("mock-1"), unserved("mock-2"), unserved("mock-3")}
	for _, tc := range []struct {
		name, rec, live string
		reads           int64
	}{
		{"a UUID: its long words, three payloads", "3f2a9c1e-4b7d-4e2a-9f3c-1a2b3c4d5e6f", "8e1d2c3b-a0f9-4c8b-b7a6-5f4e3d2c1b0a", 3},
		{"short words alone: no payload read", "job 4b7d", "job a0f9", 0},
	} {
		for _, dc := range []func() *DriftCache{mockDrifts, clockedDrifts} {
			before := gluedReads.Load()
			ClassifyGiven(dc(), []string{path}, shape(tc.live), shape(tc.rec), eRecAt, nil, raw, nil, nil, none)
			if got := gluedReads.Load() - before; got != tc.reads {
				t.Errorf("%s: read %d payloads for glued words, want %d", tc.name, got, tc.reads)
			}
		}
	}
}
