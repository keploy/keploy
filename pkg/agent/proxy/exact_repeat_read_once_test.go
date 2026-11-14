package proxy

import (
	"fmt"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A read the test recorded ONCE, which the replay names a read recorded once
// (models.RepeatReadMark.Once: its namer knows it reads by its request
// alone): one call more is answered again from its one recording; a second
// call more misses; the recording left unmade is charged, never excused as
// one fewer. A mark of a read recorded once on two recordings, and one on a
// recording whose answer keploy cannot read, keep the exact count.
func TestAReadRecordedOnceIsAnsweredOnceMoreAndNeverExcused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const genJobs = `{"query":"query GetAllGenJobs { getAllGenJobs { id status } }","variables":{}}`
	repeatRun(t, []repeatCase{
		// the atg-features shape: a GraphQL query list refetched once more
		{name: "gen-jobs", method: "POST", path: "/query", body: genJobs, read: "jobs", once: true, answers: []string{`{"data":{"getAllGenJobs":[]}}`}, recorded: 1, sends: 2},
		{name: "as-recorded", method: "GET", path: "/api/o1", read: "o1", once: true, answers: []string{`{}`}, recorded: 1, sends: 1},
		{name: "twice-more", method: "GET", path: "/api/o2", read: "o2", once: true, answers: []string{`{}`}, recorded: 1, sends: 3, missed: []int{2}},
		{name: "none-made", method: "GET", path: "/api/o3", read: "o3", once: true, answers: []string{`{}`}, recorded: 1, sends: 0, unmade: []int{0}},
		// a mark of a read recorded once on two recordings is no read: neither
		// one more nor one fewer
		{name: "on-two-more", method: "GET", path: "/api/o4", read: "o4", once: true, answers: []string{`{}`}, recorded: 2, sends: 3, missed: []int{2}},
		{name: "on-two-fewer", method: "GET", path: "/api/o5", read: "o5", once: true, answers: []string{`{}`}, recorded: 2, sends: 1, unmade: []int{1}},
		// an answer whose body keploy left out is no answer it can say
		{name: "skipped", method: "GET", path: "/api/o6", read: "o6", once: true, answers: []string{``}, recorded: 1, sends: 2, missed: []int{1},
			edit: func(_ int, mk *models.Mock) { mk.Spec.HTTPResp.BodySkipped = true }},
		{name: "streamed", method: "GET", path: "/api/o7", read: "o7", once: true, answers: []string{``}, recorded: 1, sends: 2, missed: []int{1},
			edit: func(_ int, mk *models.Mock) { mk.Spec.HTTPResp.StreamBody = []models.HTTPStreamChunk{{}} }},
		// the same single recording named a repeated read (no Once) stays a
		// miss, as an older namer's would
		{name: "unmarked-once", method: "GET", path: "/api/o8", read: "o8", answers: []string{`{}`}, recorded: 1, sends: 2, missed: []int{1}},
	})
}

// The replay says a read recorded once that it answers once more as a read
// answered from its one recording, naming the test, the read and the count;
// a read recorded twice keeps its own line.
func TestAReplaySaysAReadRecordedOnceIsAnsweredFromItsOneRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.InfoLevel)
	const once, twice = "GET /api/one, answered once more: recorded once", "GET /api/two, answered once more: recorded 2 times with one answer"
	repeatRunLogged(t, zap.New(core), []repeatCase{
		{name: "once", method: "GET", path: "/api/one", read: once, once: true, answers: []string{`{"a":1}`}, recorded: 1, sends: 2},
		{name: "twice", method: "GET", path: "/api/two", read: twice, answers: []string{`{"b":1}`}, recorded: 2, sends: 3},
	})
	said := map[string][]string{}
	for _, snippet := range []string{"answered a read once more from its one recording", "answered a repeated read once more"} {
		for _, e := range logs.FilterMessageSnippet(snippet).All() {
			f := e.ContextMap()
			said[snippet] = append(said[snippet], fmt.Sprintf("%v: %v (%v, recorded %v)", f["test"], f["read"], f["mock"], f["recorded"]))
		}
	}
	want := map[string][]string{
		"answered a read once more from its one recording": {"once: " + once + " (once-0, recorded 1)"},
		"answered a repeated read once more":               {"twice: " + twice + " (twice-1, recorded 2)"},
	}
	if !reflect.DeepEqual(said, want) {
		t.Fatalf("the replay said %q; want %q", said, want)
	}
}
