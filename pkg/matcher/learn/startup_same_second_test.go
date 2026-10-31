package learn

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// api-server's start-up janitors (T2, keploy main 0ee73b13b, 1 run in 6):
// the notifications janitor sends "createdAt < now" (distinct on
// notifications) and the branch janitor "expires_at <= now" (find on
// branches), each reading time.Now().Unix() on its own, on two Mongo
// connections at once, in one second. Record sent the find first; record's
// full round (F) sent the distinct first, before it served the find. Before
// the fix the distinct's second was followed as a value from the find's
// recording, which F had not served before the call: integrations refused it
// ("its source recording, mock-17, was never served"), TestAppCRUD failed
// round F, and every later test moved its calls.
//
// A recording F had not served before the call is no source of an instant
// the application sent there: in F the application sent the call before it
// sent that one (or it missed), and two reads of the clock in one second
// are a coincidence, not dataflow. The value is judged as the call's own
// read, checked in its call's window at every replay, and the reason names
// the recording set aside.
func TestAStartUpReadOfTheClockIsNotFollowedFromASiblingFHadNotServed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const path = "body.cmd.query.createdAt.$lt.$numberLong"
	at := eRecAt
	recT := strconv.FormatInt(at.Unix(), 10)
	fAt := at.Add(FullRoundLead)
	liveT := strconv.FormatInt(fAt.Unix(), 10)
	w := &Window{Rec: Span{From: at.Add(-time.Millisecond), To: at}, F: Span{From: fAt.Add(-time.Millisecond), To: fAt.Add(time.Millisecond)}}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: v}}}
	}
	// find is the branch janitor's recording: its request holds sent (the
	// second it read), and its answer answer, recorded before before the
	// call and answered after (the two in flight together) or before.
	find := func(sent, answer string, before, answered time.Duration) Raw {
		req := at.Add(-before)
		return RawOf(&models.Mock{Name: "mock-17", Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"find":"branches","filter":{"status":{"$in":["merged","closed"]},"expires_at":{"$lte":{"$numberLong":"` + sent + `"},"$exists":true}},"$db":"api-server"} }`,
			}}}},
			MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"cursor":{"firstBatch":[` + answer + `],"id":{"$numberLong":"0"},"ns":"api-server.branches"},"ok":{"$numberDouble":"1.0"}} }`,
			}}}},
			ReqTimestampMock: req, ResTimestampMock: at.Add(answered),
		}}, false)
	}
	type classify func(dc *DriftCache, raw []Raw, w *Window) Learned
	for name, c := range map[string]classify{
		"ClassifyFollowing": func(dc *DriftCache, raw []Raw, w *Window) Learned {
			return ClassifyFollowing(dc, []string{path}, str(liveT), str(recT), at, nil, raw, nil, w, nil)[0]
		},
		"ClassifyGiven": func(dc *DriftCache, raw []Raw, w *Window) Learned {
			return ClassifyGiven(dc, []string{path}, str(liveT), str(recT), at, nil, raw, nil, w, nil)[0]
		},
	} {
		for _, mode := range []struct {
			name string
			dc   func() *DriftCache
		}{{"client test", clientTestCache}, {"keploy test", clockedDrifts}} {
			// The row: F had not served the find before the call, whether the
			// recording gave its answer after the call's request (in flight
			// together, the T2 shape) or before.
			for _, shape := range []struct {
				what     string
				answered time.Duration
			}{{"in flight together", 400 * time.Microsecond}, {"answered before the call", -100 * time.Microsecond}} {
				got := c(mode.dc(), []Raw{find(recT, "", 600*time.Microsecond, shape.answered)}, w)
				if !plainRead(got) || got.From.Mock != "" {
					t.Errorf("%s, %s, %s: a start-up read of the clock beside a sibling F had not served: %s from %q (%q); want the call's own read of the clock", name, mode.name, shape.what, got.Kind, got.From.Mock, got.Why)
					continue
				}
				// As it is with no such recording.
				if base := c(mode.dc(), nil, w); base.Kind != got.Kind || fmt.Sprint(base.Read) != fmt.Sprint(got.Read) {
					t.Errorf("%s, %s, %s: %s read %v; with no such recording %s read %v", name, mode.name, shape.what, got.Kind, got.Read, base.Kind, base.Read)
				}
				// ClassifyGiven reads no copy rule (fol nil), so it names no
				// place it went past; ClassifyFollowing names it.
				if name == "ClassifyFollowing" && (!strings.Contains(got.Refuted, "mock-17") || !strings.Contains(got.Refuted, "had not served")) {
					t.Errorf("%s, %s, %s: the reason does not name the recording set aside and why: %q", name, mode.name, shape.what, got.Refuted)
				}
			}
		}
	}
}

// What the rule above does not reach stays the variable of the recording,
// as before it: a recording F served before the call (with no view of it
// stated), an instant a recording's answer gave the application (followed
// whole, a founder decision), a recording whose text holds F's value too,
// and every value without the proven switch.
func TestAStartUpReadOfTheClockStaysFollowedWhereTheUnservedRuleDoesNotReach(t *testing.T) {
	const path = "body.cmd.query.createdAt.$lt.$numberLong"
	at := eRecAt
	recT := strconv.FormatInt(at.Unix(), 10)
	fAt := at.Add(FullRoundLead)
	liveT := strconv.FormatInt(fAt.Unix(), 10)
	w := &Window{Rec: Span{From: at.Add(-time.Millisecond), To: at}, F: Span{From: fAt.Add(-time.Millisecond), To: fAt.Add(time.Millisecond)}}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: v}}}
	}
	mongo := func(name, req, res string) Raw {
		return RawOf(&models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{req}}}},
			MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"cursor":{"firstBatch":[` + res + `],"id":{"$numberLong":"0"},"ns":"api-server.x"},"ok":{"$numberDouble":"1.0"}} }`,
			}}}},
			ReqTimestampMock: at.Add(-600 * time.Microsecond), ResTimestampMock: at.Add(400 * time.Microsecond),
		}}, false)
	}
	sent := func(v string) string {
		return `{ SectionSingle msg: {"find":"branches","filter":{"expires_at":{"$lte":{"$numberLong":"` + v + `"}}},"$db":"api-server"} }`
	}
	const ping = `{ SectionSingle msg: {"find":"leases","filter":{},"$db":"api-server"} }`
	elsewhere := &Window{Rec: Span{From: at.Add(-time.Millisecond), To: at}, F: Span{From: fAt.Add(time.Hour), To: fAt.Add(time.Hour + time.Millisecond)}}
	for _, g := range []struct {
		what    string
		switchV string
		raw     Raw
		w       *Window
	}{
		{"F served the find before the call, no view stated", "1", mongo("mock-17", sent(recT), "").ServedAs(1), w},
		{"the find's answer gave the application the instant", "1", mongo("mock-17", ping, `{"expires_at":{"$numberLong":"`+recT+`"}}`), w},
		{"the find's recorded text holds F's second too", "1", mongo("mock-17", sent(recT), `{"n":{"$numberLong":"`+liveT+`"}}`), w},
		{"the proven switch off", "", mongo("mock-17", sent(recT), ""), w},
	} {
		t.Run(g.what, func(t *testing.T) {
			t.Setenv(provenswitch.Env, g.switchV)
			for _, dc := range []func() *DriftCache{clientTestCache, clockedDrifts} {
				got := ClassifyFollowing(dc(), []string{path}, str(liveT), str(recT), at, nil, []Raw{g.raw}, nil, g.w, nil)[0]
				if got.Kind != KindVariable || got.From.Mock != "mock-17" {
					t.Errorf("%s from %q (%q); want the variable of mock-17, as before", got.Kind, got.From.Mock, got.Why)
				}
			}
		})
	}
	// F's window an hour from F's second. A client test's clock set reads
	// the window, which shows no read of the clock: nothing the rule reads,
	// so the variable stays. Keploy test's reads the lead the value moved
	// by, and learns it as it does with no such recording: a read of the
	// clock, which every replay checks in its call's span; never noise.
	t.Setenv(provenswitch.Env, "1")
	far := mongo("mock-17", sent(recT), "")
	if got := ClassifyFollowing(clientTestCache(), []string{path}, str(liveT), str(recT), at, nil, []Raw{far}, nil, elsewhere, nil)[0]; got.Kind != KindVariable || got.From.Mock != "mock-17" {
		t.Errorf("client test, the call's window an hour from F's second: %s from %q (%q); want the variable of mock-17, as before", got.Kind, got.From.Mock, got.Why)
	}
	got := ClassifyFollowing(clockedDrifts(), []string{path}, str(liveT), str(recT), at, nil, []Raw{far}, nil, elsewhere, nil)[0]
	base := ClassifyFollowing(clockedDrifts(), []string{path}, str(liveT), str(recT), at, nil, nil, nil, elsewhere, nil)[0]
	if got.Kind == KindNoise || got.Kind != base.Kind || fmt.Sprint(got.Read) != fmt.Sprint(base.Read) {
		t.Errorf("FALSE GREEN? keploy test, the call's window an hour from F's second: %s read %v (%q); want never noise, and as with no such recording: %s read %v", got.Kind, got.Read, got.Why, base.Kind, base.Read)
	}
}
