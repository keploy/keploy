package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// api-server's e2e suite (acceptance run 5, round F): the application mints
// an app's id (a random UUID) and inserts it into app_name_reservations. A
// word of the recorded id ("905c", its characters 10 to 13) stands, by
// chance, inside another id in an earlier recording that record's full
// round never served (a background job's jobEvents insert, mock-1786). The
// id changed whole: every word of it is new. An id is copied whole, and F
// gave no view of that recording to show the word was copied, so the word
// inside another id there is no source: the id is the application's own
// value, first sent here, classified as it is with no such recording. 20 of
// run 5's round-F misses were this shape.
//
// Where the unserved recording holds the word as an id of its own, or the
// whole id, or where the id kept a word, it is still a candidate source and
// the call is not learned as the test's own value.
func TestAMintedIdIsNotFollowedFromAPartOfItInsideAnotherIdOfAnUnservedRecording(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 39, 0, 0, time.UTC)
	w := &Window{
		Rec: Span{From: at.Add(-900 * time.Millisecond), To: at},
		F:   Span{From: at.Add(60_500 * time.Millisecond), To: at.Add(61_400 * time.Millisecond)},
	}
	const (
		p    = "body.docs.msgs.0.appId"
		rec  = "ed35a78f-905c-4631-bc3d-a07311678c73"
		live = "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		// jobEvents' own id, which holds "905c" by chance.
		other = "c1b3abf7-905c-4f49-ba0d-9797211f864f"
	)
	jobEvent := func(id string, served bool) []Raw {
		before := at.Add(-200 * time.Millisecond)
		return []Raw{RawOf(&models.Mock{Name: "mock-1786", Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"insert":"jobEvents","ordered":true,"$db":"api-server"} }`,
				`{ SectionSingle identifier: documents , msgs: [ {"_id":"` + id + `","job_id":"08a11460-f3e0-4613-8960-592870d494cc","kind":"AGENT_REQ"} ] }`,
			}}}},
			ReqTimestampMock: before, ResTimestampMock: before.Add(time.Millisecond),
		}}, served)}
	}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	type classify func(dc *DriftCache, raw []Raw, recorded, now string) Learned
	for name, c := range map[string]classify{
		"ClassifyIn": func(dc *DriftCache, raw []Raw, recorded, now string) Learned {
			return ClassifyIn(dc, []string{p}, str(now), str(recorded), at, nil, raw, nil, w)[0]
		},
		"ClassifyFollowing": func(dc *DriftCache, raw []Raw, recorded, now string) Learned {
			return ClassifyFollowing(dc, []string{p}, str(now), str(recorded), at, nil, raw, nil, w, nil)[0]
		},
	} {
		for _, mode := range []struct {
			name string
			dc   func() *DriftCache
		}{{"client test", clientTestCache}, {"keploy test", clockedDrifts}} {
			base := c(mode.dc(), nil, rec, live)
			// The second: the word glued inside a longer word ("zz905c")
			// beside a word of the id is no place of it at all.
			for _, held := range []string{other, other + `","tag":"zz905c-4631`} {
				got := c(mode.dc(), jobEvent(held, false), rec, live)
				if got.Kind != base.Kind || got.Bindable != base.Bindable || strings.Contains(got.Why, "mock-1786") {
					t.Errorf("%s, %s: the id's word inside another id of an unserved recording (%s): %s bindable=%v (%q); want as with no such recording: %s bindable=%v", name, mode.name, held, got.Kind, got.Bindable, got.Why, base.Kind, base.Bindable)
				}
			}
			// Still candidate sources: the word as an id of its own there,
			// the whole id there, and a word of an id that kept another.
			for _, g := range []struct {
				what      string
				raw       []Raw
				rec, live string
			}{
				{"the word as an id of its own", jobEvent("905c", false), rec, live},
				{"the whole id", jobEvent(rec, false), rec, live},
				{"an id that kept a word", jobEvent(other, false), rec, "ed35a78f-7e6d-4b8a-9e21-5c6d7e8f9a0b"},
				{"the id inside a longer id of the value", jobEvent(rec, false), rec + "-1", live + "-2"},
				{"the word also alone in the value", jobEvent(other, false), rec + " 905c", "3f0c9a12-1111-4b8a-9e21-5c6d7e8f9a0b 2222"},
				{"a part of the id beside another of its words", jobEvent("ffff0000-905c-4631-aaaa-bbbbccccdddd", false), rec, live},
			} {
				got := c(mode.dc(), g.raw, g.rec, g.live)
				variable := got.Kind == KindVariable && got.From.Mock == "mock-1786"
				refused := got.Kind == KindUnaccounted && !got.Bindable && strings.Contains(got.Why, "mock-1786")
				if !variable && !refused {
					t.Errorf("FALSE GREEN? %s, %s, %s at an unserved recording: %s from %q bindable=%v (%q); want a variable of mock-1786 or refused naming it", name, mode.name, g.what, got.Kind, got.From.Mock, got.Bindable, got.Why)
				}
			}
		}
	}
}

// The chance-word rows of a recording record's full round did not serve
// (follows_test.go's chanceVal*): before, a variable of mock-6, whose
// source F never served, so the call was refused. INTENTIONAL change (run
// 5): "48d1" stands in mock-6 only inside another id (the job's), which
// shares no other word with the value's id, and the value's id changed
// whole, so mock-6 is no source: the value classifies as it does with no
// such recording.
func TestAChanceWordOfAnUnservedRecordingIsNoSourceOfAnIdThatChangedWhole(t *testing.T) {
	base := classifyF(t, chanceValRec, chanceValLive, nil, eStartup(), nil)
	for name, raw := range map[string][]Raw{
		"not served, a view handed anyway": append(eStartup(), eJobs(chanceSrcRec, "5").ServedLive(eJobs(chanceSrcLive, "6"))),
		"a view dropped as not served":     append(eStartup(), eJobs(chanceSrcRec, "5").ServedAs(6).ServedLive(eJobs(chanceSrcLive, "6")).WithServed(false)),
	} {
		got := classifyF(t, chanceValRec, chanceValLive, nil, raw, nil)
		if got.Kind != base.Kind || got.From.Mock != base.From.Mock || strings.Contains(got.Why, "mock-6") {
			t.Errorf("%s: %s from %q (%q); want as with no such recording: %s from %q", name, got.Kind, got.From.Mock, got.Why, base.Kind, base.From.Mock)
		}
		// The job's id whole in the value: mock-6 stays its source.
		if got := classifyF(t, chanceSrcRec+"-1", chanceValLive+"-2", nil, raw, nil); got.Kind != KindVariable || got.From.Mock != "mock-6" {
			t.Errorf("FALSE GREEN? %s, the job's id inside the value: %s from %q (%q); want a variable of mock-6", name, got.Kind, got.From.Mock, got.Why)
		}
	}
}
