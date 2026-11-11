package models

import "testing"

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
		{"mongo no OP_MSG", &Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{Message: &MongoOpQuery{FullCollectionName: "api.sessions"}}}}}, RecordedCall{Kind: Mongo}},
		{"mongo no request", &Mock{Kind: Mongo}, RecordedCall{Kind: Mongo}},
		{"http no request", &Mock{Kind: HTTP}, RecordedCall{Kind: HTTP}},
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
