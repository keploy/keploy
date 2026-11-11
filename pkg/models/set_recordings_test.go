package models

import (
	"context"
	"errors"
	"testing"
)

func httpMock(method, url string, vars MockVariables) *Mock {
	return &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: Method(method), URL: url}, Variables: vars}}
}

func mongoMock(sections ...string) *Mock {
	return &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: sections}}}}}
}

// Keploy reads the shape of an HTTP mock's call and of a MongoDB mock's OP_MSG
// command, as a miss's summary names them; no other mock's.
func TestRecordedCallOfReadsHTTPAndMongoCommands(t *testing.T) {
	for _, c := range []struct {
		name string
		m    *Mock
		want RecordedCall
	}{
		{"http", httpMock("GET", "http://clock:80/api/tick?zone=utc", nil), RecordedCall{Kind: HTTP, Shape: CallShape("HTTP", "GET /api/tick?zone=utc"), Read: true}},
		{"http declared place", httpMock("GET", "http://users/u/alice", MockVariables{"user": {Bind: "url.path[1]"}}), RecordedCall{Kind: HTTP, Shape: "GET /u/{}", Read: true}},
		{"mongo command on a collection", mongoMock(`{ SectionSingle msg: {"delete":"sessions","ordered":true,"$db":"api"} }`), RecordedCall{Kind: Mongo, Shape: "delete on sessions", Read: true}},
		{"mongo command with no collection", mongoMock(`{ SectionSingle msg: {"ping":1,"$db":"admin"} }`), RecordedCall{Kind: Mongo, Shape: "ping", Read: true}},
		{"mongo $cmd", mongoMock(`{ SectionSingle msg: {"isMaster":"$cmd"} }`), RecordedCall{Kind: Mongo, Shape: "isMaster", Read: true}},
		{"mongo sequence first", mongoMock(`{ SectionSingle identifier: documents }`, `{ SectionSingle msg: {"insert":"carts"} }`), RecordedCall{Kind: Mongo, Shape: "insert on carts", Read: true}},
		{"mongo unreadable body", mongoMock(`{ SectionSingle msg: [1] }`), RecordedCall{Kind: Mongo}},
		{"mongo body no document", mongoMock(`{ SectionSingle msg: ["find","sessions"] }`), RecordedCall{Kind: Mongo}},
		{"mongo body with an empty key", mongoMock(`{ SectionSingle msg: {"":"sessions"} }`), RecordedCall{Kind: Mongo}},
		{"mongo no OP_MSG", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{Message: &MongoOpQuery{FullCollectionName: "api.sessions"}}}}}, RecordedCall{Kind: Mongo}},
		{"mongo no request", &Mock{Kind: Mongo}, RecordedCall{Kind: Mongo}},
		{"http no request", &Mock{Kind: HTTP}, RecordedCall{Kind: HTTP}},
		{"http with no method or URL", httpMock("", "", nil), RecordedCall{Kind: HTTP}},
		// A value Keploy Enterprise sealed (read as on disk) where the
		// shape is read from leaves the recording unread; a sealed query
		// value does not, as the shape holds no query value.
		{"http sealed segment", httpMock("GET", "http://clock:80/api/ENC:t:k:dGljaw", nil), RecordedCall{Kind: HTTP}},
		{"http sealed segment of standard base64", httpMock("GET", "http://clock:80/api/ENC:t:k:dG/ljaw==", nil), RecordedCall{Kind: HTTP}},
		{"http sealed query name", httpMock("GET", "http://clock/api/tick?ENC:t:k:em9uZQ=utc", nil), RecordedCall{Kind: HTTP}},
		{"http sealed query value", httpMock("GET", "http://clock/api/tick?zone=ENC%3At%3Ak%3AdXRj&flag", nil),
			RecordedCall{Kind: HTTP, Shape: CallShape("HTTP", "GET /api/tick?zone=utc&flag"), Read: true}},
		{"http sealed query value not escaped", httpMock("GET", "http://clock/api/tick?zone=ENC:t:k:dXRj", nil),
			RecordedCall{Kind: HTTP, Shape: CallShape("HTTP", "GET /api/tick?zone=utc"), Read: true}},
		{"http sealed bare query component", httpMock("GET", "http://clock/api/tick?ENC:t:k:MTY5", nil),
			RecordedCall{Kind: HTTP, Shape: CallShape("HTTP", "GET /api/tick?1697040000"), Read: true}},
		{"mongo sealed collection", mongoMock(`{ SectionSingle msg: {"find":"ENC:t:k:dXNlcnM","$db":"api"} }`), RecordedCall{Kind: Mongo}},
		{"mongo collection holding a sealed part", mongoMock(`{ SectionSingle msg: {"find":"users_ENC:t:k:NDI","$db":"api"} }`), RecordedCall{Kind: Mongo}},
		{"mongo sealed command", mongoMock(`{ SectionSingle msg: {"ENC:t:k:ZmluZA":"users"} }`), RecordedCall{Kind: Mongo}},
		{"mongo sealed command with no collection", mongoMock(`{ SectionSingle msg: {"ENC:t:k:cGluZw":1} }`), RecordedCall{Kind: Mongo}},
		{"mongo command whose value does not read", mongoMock(`{ SectionSingle msg: {"find":ENC:t:k:dXNlcnM} }`), RecordedCall{Kind: Mongo}},
		{"mongo command whose value is a document", mongoMock(`{ SectionSingle msg: {"explain":{"find":"users"}} }`), RecordedCall{Kind: Mongo, Shape: "explain", Read: true}},
		{"mongo with an HTTP request", &Mock{Kind: Mongo, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "http://clock/api/tick"},
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"find":"ticks"} }`}}}}}}, RecordedCall{Kind: Mongo, Shape: "find on ticks", Read: true}},
		{"redis", &Mock{Kind: REDIS}, RecordedCall{Kind: REDIS}},
		{"nil", nil, RecordedCall{}},
	} {
		if got := RecordedCallOf(c.m); got != c.want {
			t.Errorf("%s: RecordedCallOf = %+v; want %+v", c.name, got, c.want)
		}
	}
}

// What a set's recordings hold of a call's shape is read from every one of
// them: a recording of its shape anywhere in the set is "shape", none of its
// kind with it "none", and a call whose kind keploy reads no shape of, or a
// recording of its kind keploy did not read, "": it cannot tell.
func TestSetShapeRecordingsReadsTheWholeSet(t *testing.T) {
	tick := RecordedCallOf(httpMock("GET", "http://clock:80/api/tick", nil))
	user := RecordedCallOf(httpMock("GET", "http://users/u/alice", MockVariables{"user": {Bind: "url.path[1]"}}))
	del := RecordedCallOf(mongoMock(`{ SectionSingle msg: {"delete":"sessions"} }`))
	unread := RecordedCall{Kind: HTTP}
	get := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/tick", MatchPhase: MatchPhaseNoMocks}
	for _, c := range []struct {
		name     string
		call     UnmatchedCall
		recorded []RecordedCall
		want     string
	}{
		{"recorded in another window", get, []RecordedCall{del, tick}, RecordedShape},
		{"recorded under https", UnmatchedCall{Protocol: "HTTPS", ActualSummary: "GET http://clock/api/tick"}, []RecordedCall{tick}, RecordedShape},
		{"a declared place holds another value", UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /u/bob"}, []RecordedCall{user}, RecordedShape},
		{"another method", UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /api/tick"}, []RecordedCall{tick}, RecordedNone},
		{"another path", UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/tock"}, []RecordedCall{tick, del}, RecordedNone},
		{"none of its kind", get, []RecordedCall{del}, RecordedNone},
		{"an empty set", get, nil, RecordedNone},
		{"one of its kind not read", get, []RecordedCall{unread, user}, ""},
		{"read after a match", get, []RecordedCall{unread, tick}, RecordedShape},
		{"mongo recorded", UnmatchedCall{Protocol: "Mongo", ActualSummary: "delete on sessions", MatchPhase: MatchPhaseSchema}, []RecordedCall{tick, del}, RecordedShape},
		{"mongo another collection", UnmatchedCall{Protocol: "MongoDB", ActualSummary: "delete on carts"}, []RecordedCall{del}, RecordedNone},
		{"a protocol keploy reads no shape of", UnmatchedCall{Protocol: "Redis", Command: "GET"}, nil, ""},
		{"http2", UnmatchedCall{Protocol: "HTTP2", ActualSummary: "GET /api/tick"}, []RecordedCall{tick}, ""},
		{"no summary", UnmatchedCall{Protocol: "HTTP"}, nil, ""},
		// A recording that holds a value Keploy Enterprise sealed where
		// its shape is read from (read as on disk, never opened) makes the
		// set's answer for the call's kind "cannot tell", never "none";
		// one recording of the call's shape still says "shape".
		{"a segment recorded sealed", get, []RecordedCall{RecordedCallOf(httpMock("GET", "http://clock:80/api/ENC:t:k:dGljaw", nil))}, ""},
		{"mongo collection recorded sealed", UnmatchedCall{Protocol: "Mongo", ActualSummary: "delete on sessions", MatchPhase: MatchPhaseNoMocks},
			[]RecordedCall{RecordedCallOf(mongoMock(`{ SectionSingle msg: {"delete":"ENC:t:k:c2Vzc2lvbnM","$db":"api"} }`)), tick}, ""},
		{"mongo collection recorded sealed and the call's own", UnmatchedCall{Protocol: "Mongo", ActualSummary: "delete on sessions"},
			[]RecordedCall{RecordedCallOf(mongoMock(`{ SectionSingle msg: {"delete":"ENC:t:k:c2Vzc2lvbnM"} }`)), del}, RecordedShape},
		{"http recorded sealed, a mongo call", UnmatchedCall{Protocol: "Mongo", ActualSummary: "delete on carts"},
			[]RecordedCall{RecordedCallOf(httpMock("GET", "http://clock:80/api/ENC:t:k:dGljaw", nil)), del}, RecordedNone},
		{"another kind with its shape", get, []RecordedCall{{Kind: Mongo, Shape: CallShape("HTTP", "GET /api/tick"), Read: true}}, RecordedNone},
		{"a shorter path", UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api"}, []RecordedCall{tick}, RecordedNone},
		{"a place the call holes", UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /u/bob", VariablePlaces: []string{"url.path[1]"}},
			[]RecordedCall{RecordedCallOf(httpMock("GET", "http://users/u/alice", nil))}, RecordedShape},
		{"mongo as received, with its database", UnmatchedCall{Protocol: "Mongo", ActualSummary: "delete on sessions", ReceivedReq: `{"cmd":{"delete":"sessions","$db":"api"}}`},
			[]RecordedCall{del}, RecordedShape},
	} {
		if got := SetShapeRecordings(c.call, c.recorded); got != c.want {
			t.Errorf("%s: SetShapeRecordings = %q; want %q", c.name, got, c.want)
		}
	}
}

// A phase that compared recordings of the call's shape says the set has
// some; any other phase says what a read of the whole set found, and nothing
// without one: a pool that held no recording of the shape is one test's.
func TestShapeRecordingsNeedsTheWholeSetToSayNone(t *testing.T) {
	for _, c := range []struct {
		phase, set, want string
	}{
		{MatchPhaseBody, "", RecordedShape},
		{MatchPhaseStrict, RecordedNone, RecordedShape},
		{MatchPhaseVariable, "", RecordedShape},
		{MatchPhaseNoMocks, "", ""},
		{MatchPhaseSchema, "", ""},
		{MatchPhaseNoMocks, RecordedNone, RecordedNone},
		{MatchPhaseSchema, RecordedShape, RecordedShape},
		{MatchPhaseExhausted, RecordedNone, RecordedNone},
		{"", "bogus", ""},
	} {
		if got := (UnmatchedCall{MatchPhase: c.phase, SetRecorded: c.set}).ShapeRecordings(); got != c.want {
			t.Errorf("phase %q, set %q: ShapeRecordings = %q; want %q", c.phase, c.set, got, c.want)
		}
	}
	if got := (UnmatchedCall{MatchPhase: MatchPhaseNoMocks, SetRecorded: RecordedNone}).Sanitized().ShapeRecordings(); got != RecordedNone {
		t.Errorf("a sanitized call says %q; it keeps what the set read found", got)
	}
}

// onDiskSource is a mock file holding one recording, which it reads back, or
// fails to with err.
type onDiskSource struct {
	recorded *Mock
	err      error
}

func (s onDiskSource) LoadPayload(context.Context, PayloadAt) (*Mock, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.recorded.DeepCopy(), nil
}
func (onDiskSource) SetPayloadMutator(PayloadMutator) {}
func (onDiskSource) PayloadMutator() PayloadMutator   { return nil }

// A recording whose payload a read left on disk is read from its payload
// read back; one whose payload cannot be read back is not read.
func TestRecordedCallOfReadsAPayloadLeftOnDisk(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want RecordedCall
	}{
		{"read back", nil, RecordedCall{Kind: HTTP, Shape: CallShape("HTTP", "GET /api/tick"), Read: true}},
		{"file replaced", ErrPayloadFileReplaced, RecordedCall{Kind: HTTP}},
		{"unreadable", errors.New("the file is cut"), RecordedCall{Kind: HTTP}},
	} {
		recorded := httpMock("GET", "http://clock:80/api/tick", nil)
		m := recorded.DeepCopy()
		m.LeavePayloadOnDisk(onDiskSource{recorded: recorded, err: c.err}, PayloadAt{Name: "tick"})
		if m.Spec.HTTPReq != nil {
			t.Fatalf("%s: the mock kept its payload in memory", c.name)
		}
		if got := RecordedCallOf(m); got != c.want {
			t.Errorf("%s: RecordedCallOf = %+v; want %+v", c.name, got, c.want)
		}
	}
}

// Two shapes are one call's where they are equal segment by segment, either
// side writing a segment {} for any, and of one length.
func TestSameCallShapeHolesEitherSide(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"GET /u/{}", "GET /u/alice", true},
		{"GET /u/alice", "GET /u/{}", true},
		{"GET /u/alice", "GET /u/bob", false},
		{"GET /u", "GET /u/{}", false},
		{"delete on sessions", "delete on sessions", true},
	} {
		if got := sameCallShape(c.a, c.b); got != c.want {
			t.Errorf("sameCallShape(%q, %q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}
