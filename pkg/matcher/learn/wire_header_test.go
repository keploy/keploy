package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// api-server's e2e suite (acceptance run 5, round F): the application mints
// an app's id (a random UUID). A word of it ("2486", its characters 10 to
// 13) is, by chance, the number of a Mongo frame's header in an earlier
// recording: the requestId the server gave its answer to an ATGJobs find
// (mock-2124), or the length of an eventsV2 insert (mock-2812). A frame's
// header (its length, request id, the id it answers and its opcode) is the
// wire's framing, which the driver and the server count: the application
// never reads it, so no value flows from there. 4 of run 5's round-F misses
// named a header field as the value's source: of the recorded value
// (mock-3049, mock-4224) or of what F saw (mock-2124, mock-2812). The value
// classifies as it does with no such recording, served or not: never
// served (as in run 5), served as recorded, or served with a view whose
// frames' headers hold the word.
//
// The same word in a frame's message (a document of it) is still a
// candidate source.
func TestAWordOfAValueIsNeverFollowedFromAFramesHeader(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 39, 20, 0, time.UTC)
	w := &Window{
		Rec: Span{From: at.Add(-900 * time.Millisecond), To: at},
		F:   Span{From: at.Add(60_500 * time.Millisecond), To: at.Add(61_400 * time.Millisecond)},
	}
	const p = "body.docs.msgs.0.appId"
	before := at.Add(-16 * time.Second)
	frame := func(n int32) *models.MongoHeader {
		return &models.MongoHeader{Length: n, RequestID: n, ResponseTo: n, Opcode: wiremessage.OpMsg}
	}
	// mock-2124's shape: an ATGJobs find and its answer, the word as one
	// number of each frame's header (the real one held it as the answer's
	// requestId; mock-2812 as its request's length).
	atgFind := func(word int32, doc string, served bool) []Raw {
		return []Raw{RawOf(&models.Mock{Name: "mock-2124", Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: frame(word), Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"find":"ATGJobs","filter":{"_id":"08a11460-f3e0-4613-8960-592870d494cc"},"limit":{"$numberLong":"1"},"singleBatch":true,"$db":"api-server"} }`,
			}}}},
			MongoResponses: []models.MongoResponse{{Header: frame(word), Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"08a11460-f3e0-4613-8960-592870d494cc","app_id":"8d4c6630-7400-4022-9769-cafc541a0f68"` + doc + `}],"id":{"$numberLong":"0"},"ns":"api-server.ATGJobs"},"ok":{"$numberDouble":"1.0"}} }`,
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
			for _, v := range []struct {
				what      string
				rec, live string
			}{
				{"a word of what F saw", "ed35a78f-905c-4631-bc3d-a07311678c73", "3f0c9a12-2486-4b8a-9e21-5c6d7e8f9a0b"},
				{"a word of the recorded value", "ed35a78f-2486-4631-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"},
			} {
				base := c(mode.dc(), nil, v.rec, v.live)
				for how, raw := range map[string][]Raw{
					"never served":         atgFind(2486, "", false),
					"served as recorded":   {atgFind(2486, "", true)[0].ServedAsRecorded()},
					"served, a view of it": {atgFind(7, "", true)[0].ServedLive(atgFind(2486, "", true)[0])},
				} {
					got := c(mode.dc(), raw, v.rec, v.live)
					if got.Kind != base.Kind || got.Bindable != base.Bindable || strings.Contains(got.Why, "mock-2124") {
						t.Errorf("%s, %s, %s, %s: the word a frame header's number: %s bindable=%v (%q); want as with no such recording: %s bindable=%v", name, mode.name, v.what, how, got.Kind, got.Bindable, got.Why, base.Kind, base.Bindable)
					}
				}
				// Still a candidate source: the word in the frame's message.
				got := c(mode.dc(), atgFind(7, `,"run":"2486"`, false), v.rec, v.live)
				if (got.Kind != KindVariable || got.From.Mock != "mock-2124") && !(got.Kind == KindUnaccounted && !got.Bindable && strings.Contains(got.Why, "mock-2124")) {
					t.Errorf("FALSE GREEN? %s, %s, %s, the word in the frame's message: %s from %q bindable=%v (%q); want a variable of mock-2124 or refused naming it", name, mode.name, v.what, got.Kind, got.From.Mock, got.Bindable, got.Why)
				}
			}
		}
	}
}

// A MySQL packet's header (its payload's length and sequence id) is the
// wire's framing too: what a recording reads of its payload holds neither,
// and keeps the packet's message and type. Reading a recording leaves its
// frames as recorded.
func TestARecordingsTextHoldsNoFrameHeader(t *testing.T) {
	mongoMock := &models.Mock{Name: "mock-1", Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests:  []models.MongoRequest{{Header: &models.MongoHeader{Length: 4307, RequestID: 2794, ResponseTo: 1931, Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"find":"apps","n":"6618"} }`}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Length: 45, RequestID: 3094, ResponseTo: 2794, Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"ok":{"$numberDouble":"1.0"}} }`}}}},
	}}
	myMock := &models.Mock{Name: "mock-2", Kind: models.MySQL, Spec: models.MockSpec{
		MySQLRequests:  []mysql.Request{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 4307, SequenceID: 211}, Type: "COM_QUERY"}, Message: map[string]interface{}{"query": "SELECT 6618"}}}},
		MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 3094, SequenceID: 212}, Type: "OK"}, Message: map[string]interface{}{"affected_rows": 1}}}},
	}}
	mongo, my := RawOf(mongoMock, true), RawOf(myMock, true)
	// The recordings keep their frames: the proxy serves them.
	if h := mongoMock.Spec.MongoRequests[0].Header; h == nil || h.RequestID != 2794 {
		t.Errorf("reading the Mongo recording changed its request's header: %+v", h)
	}
	if h := mongoMock.Spec.MongoResponses[0].Header; h == nil || h.RequestID != 3094 {
		t.Errorf("reading the Mongo recording changed its answer's header: %+v", h)
	}
	if h := myMock.Spec.MySQLRequests[0].Header; h == nil || h.Header == nil || h.Header.SequenceID != 211 {
		t.Errorf("reading the MySQL recording changed its request's header: %+v", h)
	}
	if h := myMock.Spec.MySQLResponses[0].Header; h == nil || h.Header == nil || h.Header.SequenceID != 212 {
		t.Errorf("reading the MySQL recording changed its answer's header: %+v", h)
	}
	for _, x := range []Raw{mongo, my} {
		b := x.payload()
		for _, word := range []string{"4307", "2794", "1931", "3094", "211", "212", "2013"} {
			if containsToken(b.text, word) {
				t.Errorf("%s: its text holds the frame header's %s: %q", x.mock, word, b.text)
			}
		}
		for _, f := range b.fields {
			if strings.Contains(f.path, "header.length") || strings.Contains(f.path, "requestId") || strings.Contains(f.path, "responseTo") || strings.Contains(f.path, "Opcode") || strings.Contains(f.path, "payload_length") || strings.Contains(f.path, "sequence_id") {
				t.Errorf("%s: a field of the frame header: %s", x.mock, f.path)
			}
		}
	}
	if !strings.Contains(my.payload().text, "COM_QUERY") || !strings.Contains(my.payload().text, "SELECT 6618") {
		t.Errorf("the MySQL packet's type and message are gone: %q", my.payload().text)
	}
	if !strings.Contains(mongo.payload().text, "6618") {
		t.Errorf("the Mongo frame's message is gone: %q", mongo.payload().text)
	}
}

// containsToken reports whether word stands in text bounded by non-digits.
func containsToken(text, word string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], word)
		if j < 0 {
			return false
		}
		s, e := i+j, i+j+len(word)
		if (s == 0 || !isDigitByte(text[s-1])) && (e == len(text) || !isDigitByte(text[e])) {
			return true
		}
		i = s + 1
	}
}
