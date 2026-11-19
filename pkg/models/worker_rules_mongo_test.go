package models

import (
	"strings"
	"testing"
)

// mongoMsgMock is a Mongo mock of one OP_MSG request per command document
// (extended JSON), as the recorder writes its body.
func mongoMsgMock(dest string, commands ...string) *Mock {
	m := &Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": dest}}}
	for _, c := range commands {
		m.Spec.MongoRequests = append(m.Spec.MongoRequests, MongoRequest{Message: &MongoOpMessage{Sections: []string{
			"{ SectionSingle msg: " + c + " }",
			`{ SectionSingle identifier: documents , msgs: [ {"q":{"_id":"reaper"},"u":{"$set":{"at":"2026-11-19T10:00:00Z"}}} ] }`,
		}}})
	}
	return m
}

func mongoQueryMock(dest, collection, query string) *Mock {
	return &Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": dest}, MongoRequests: []MongoRequest{
		{Message: &MongoOpQuery{FullCollectionName: collection, Query: query}},
	}}}
}

// A Mongo rule names a command on a namespace: the command without case, the
// collection and database by glob, every request of the mock. One whose
// request keploy cannot read matches no rule that names a command or a
// namespace, and a rule on the destination alone still matches it.
func TestAMongoRuleNamesACommandOnANamespace(t *testing.T) {
	update := mongoMsgMock("mongo:27017", `{"update":"jobs","ordered":true,"lsid":{"id":{"$binary":{"base64":"dWXRpoU6SMmEdEXDm7tenQ==","subType":"04"}}},"$db":"bgtimer"}`)
	rule := func(match map[string]string) WorkerMockRule {
		return WorkerMockRule{Kind: string(Mongo), Match: match}
	}
	for _, row := range []struct {
		name  string
		match map[string]string
		mock  *Mock
		want  bool
	}{
		{"its command and collection", map[string]string{"command": "update", "collection": "jobs"}, update, true},
		{"its command in another case", map[string]string{"command": "UPDATE", "collection": "jobs"}, update, true},
		{"a collection glob", map[string]string{"collection": "job*"}, update, true},
		{"its database", map[string]string{"command": "update", "database": "bgtimer"}, update, true},
		{"another command", map[string]string{"command": "find", "collection": "jobs"}, update, false},
		{"another collection", map[string]string{"command": "update", "collection": "users"}, update, false},
		{"another database", map[string]string{"collection": "jobs", "database": "app"}, update, false},
		{"another server", map[string]string{"collection": "jobs", "destination": "other:27017"}, update, false},
		{"a command on no collection", map[string]string{"collection": "*s"}, mongoMsgMock("mongo:27017", `{"hello":1,"$db":"admin"}`), false},
		{"every request of the mock", map[string]string{"collection": "jobs"},
			mongoMsgMock("mongo:27017", `{"update":"jobs","$db":"app"}`, `{"update":"users","$db":"app"}`), false},
		{"a getMore by its collection member", map[string]string{"command": "getMore", "collection": "jobs"},
			mongoMsgMock("mongo:27017", `{"getMore":{"$numberLong":"77"},"collection":"jobs","$db":"app"}`), true},
		{"a legacy command", map[string]string{"command": "count", "collection": "jobs", "database": "app"},
			mongoQueryMock("mongo:27017", "app.$cmd", `{"count":"jobs"}`), true},
		{"a legacy command with a read preference", map[string]string{"command": "count", "collection": "jobs"},
			mongoQueryMock("mongo:27017", "app.$cmd", `{"$query":{"count":"jobs"},"$readPreference":{"mode":"secondary"}}`), true},
		{"a legacy query, a find", map[string]string{"command": "find", "collection": "jobs", "database": "app"},
			mongoQueryMock("mongo:27017", "app.jobs", `{"state":"due"}`), true},
		{"a request it cannot read", map[string]string{"command": "update"},
			&Mock{Kind: Mongo, Spec: MockSpec{MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{"{ SectionSingle msg: not json }"}}}}}}, false},
		{"no request", map[string]string{"collection": "jobs"}, &Mock{Kind: Mongo}, false},
		{"its destination alone, readable or not", map[string]string{"destination": "mongo:27017"},
			&Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": "mongo:27017"}}}, true},
	} {
		r := rule(row.match)
		if problems := r.Validate(); len(problems) > 0 {
			t.Fatalf("%s: rule %v refused: %v", row.name, row.match, problems)
		}
		if got := r.Matches(row.mock); got != row.want {
			t.Errorf("%s: rule %v matches = %v, want %v", row.name, row.match, got, row.want)
		}
	}
}

// A Mongo rule reads only its kind's keys, its globs must parse, and a
// namespace glob that matches every collection names no call.
func TestAMongoRuleIsValidated(t *testing.T) {
	for _, row := range []struct {
		match map[string]string
		want  string
	}{
		{map[string]string{"collection": "*"}, "matches everything"},
		{map[string]string{"database": "*", "collection": "*"}, "matches everything"},
		{map[string]string{"collection": "[jobs"}, "not a glob"},
		{map[string]string{"command": "update", "query": "x"}, `"query" is not one`},
		{map[string]string{"command": "update", "collection": ""}, "is empty"},
	} {
		problems := strings.Join(WorkerMockRule{Kind: string(Mongo), Match: row.match}.Validate(), "; ")
		if !strings.Contains(problems, row.want) {
			t.Errorf("rule %v: problems %q, want one saying %q", row.match, problems, row.want)
		}
	}
	for _, ok := range []map[string]string{
		{"command": "update"},
		{"command": "findAndModify", "collection": "jobs", "database": "app"},
		{"collection": "jobs"},
	} {
		if problems := (WorkerMockRule{Kind: string(Mongo), Match: ok}).Validate(); len(problems) > 0 {
			t.Errorf("rule %v refused: %v", ok, problems)
		}
	}
}

// A Mongo call is its command and namespace, at its server: a timer's update
// that writes a new time each run is one call, so a recording keeps one copy
// of it; another command, collection, database or server is another call.
func TestAMongoCallIsItsCommandAndNamespace(t *testing.T) {
	call := func(dest, cmd string) string {
		key, ok := WorkerMockCallOf(mongoMsgMock(dest, cmd))
		if !ok {
			t.Fatalf("%s at %s: no call", cmd, dest)
		}
		return key
	}
	first := call("mongo:27017", `{"update":"jobs","ordered":true,"lsid":{"id":"a"},"$db":"app"}`)
	if again := call("mongo:27017", `{"update":"jobs","ordered":false,"lsid":{"id":"b"},"$db":"app"}`); again != first {
		t.Fatal("one update of one collection, run twice, is two calls")
	}
	for name, other := range map[string]string{
		"another command":    call("mongo:27017", `{"delete":"jobs","$db":"app"}`),
		"another collection": call("mongo:27017", `{"update":"users","$db":"app"}`),
		"another database":   call("mongo:27017", `{"update":"jobs","$db":"other"}`),
		"another server":     call("other:27017", `{"update":"jobs","$db":"app"}`),
	} {
		if other == first {
			t.Errorf("%s is the same call", name)
		}
	}
	if _, ok := WorkerMockCallOf(&Mock{Kind: Mongo, Name: "mock-1"}); ok {
		t.Fatal("a Mongo mock with no request names a call")
	}
	if WorkerMockCallKey(&Mock{Kind: Mongo, Name: "mock-1"}) == WorkerMockCallKey(&Mock{Kind: Mongo, Name: "mock-2"}) {
		t.Fatal("two Mongo mocks keploy cannot read are one call")
	}
}

// mongoWriteMock is a Mongo mock of one OP_MSG write: its command document and
// its documents section (an extended JSON array), as the recorder writes them.
func mongoWriteMock(command, ident, docs string) *Mock {
	return &Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": "mongo:27017"}, MongoRequests: []MongoRequest{
		{Message: &MongoOpMessage{Sections: []string{
			"{ SectionSingle msg: " + command + " }",
			"{ SectionSingle identifier: " + ident + " , msgs: [ " + docs + " ] }",
		}}},
	}}}
}

// A Mongo call is also the shape of what it sends: the names of its fields,
// not their values. So a timer's update run again with a new time, in another
// session, is one call, and an app's upsert of other fields of the same
// collection is another: one copy of it would not answer the timer (the live
// run that found this kept the upsert as the timer's copy, and replay missed
// every timer call).
func TestAMongoCallIsTheShapeOfWhatItSends(t *testing.T) {
	key := func(m *Mock) string {
		k, ok := WorkerMockCallOf(m)
		if !ok {
			t.Fatal("no call")
		}
		return k
	}
	timer := key(mongoWriteMock(`{"update":"jobs","ordered":true,"lsid":{"id":{"$binary":{"base64":"AA==","subType":"04"}}},"$db":"bgtimer"}`,
		"documents", `{"q":{"_id":"reaper"},"u":{"$set":{"state":"swept","at":{"$date":{"$numberLong":"1795064052000"}},"runs":{"$numberInt":"7"}}}}`))
	same := map[string]*Mock{
		// A counter past int32 is written as a long: a value, not a field.
		"a new time and count, in another session": mongoWriteMock(`{"update":"jobs","ordered":true,"lsid":{"id":{"$binary":{"base64":"/w==","subType":"04"}}},"txnNumber":{"$numberLong":"7"},"$clusterTime":{"clusterTime":{"$timestamp":{"t":1,"i":2}}},"$db":"bgtimer"}`,
			"documents", `{"q":{"_id":"reaper"},"u":{"$set":{"state":"idle","at":{"$date":{"$numberLong":"1795064099000"}},"runs":{"$numberLong":"4294967296"}}}}`),
	}
	for name, m := range same {
		if key(m) != timer {
			t.Errorf("%s: another call", name)
		}
	}
	other := map[string]*Mock{
		"an upsert of other fields": mongoWriteMock(`{"update":"jobs","ordered":true,"$db":"bgtimer"}`,
			"documents", `{"q":{"_id":"reaper"},"u":{"$set":{"ticks":{"$numberInt":"0"}}},"upsert":true}`),
		"one more field set": mongoWriteMock(`{"update":"jobs","ordered":true,"$db":"bgtimer"}`,
			"documents", `{"q":{"_id":"reaper"},"u":{"$set":{"state":"swept","at":{"$date":{"$numberLong":"1"}},"runs":{"$numberInt":"1"},"by":"x"}}}`),
		"another filter": mongoWriteMock(`{"update":"jobs","ordered":true,"$db":"bgtimer"}`,
			"documents", `{"q":{"owner":"reaper"},"u":{"$set":{"state":"swept","at":{"$date":{"$numberLong":"1"}},"runs":{"$numberInt":"1"}}}}`),
	}
	for name, m := range other {
		if key(m) == timer {
			t.Errorf("%s: the same call as the timer's update", name)
		}
	}
	// A find is its filter's shape: one call whatever the value it looks up.
	pen := key(mongoMsgMock("mongo:27017", `{"find":"items","filter":{"name":"pen"},"limit":{"$numberInt":"1"},"$db":"app"}`))
	book := key(mongoMsgMock("mongo:27017", `{"find":"items","filter":{"name":"book"},"limit":{"$numberInt":"1"},"$db":"app"}`))
	all := key(mongoMsgMock("mongo:27017", `{"find":"items","filter":{},"$db":"app"}`))
	if pen != book || pen == all {
		t.Fatalf("finds: by name %v, all %v; want one call by name and another for all", pen == book, pen != all)
	}
	// A documents section keploy cannot read names no call.
	if _, ok := WorkerMockCallOf(mongoWriteMock(`{"update":"jobs","$db":"app"}`, "documents", `not json`)); ok {
		t.Fatal("a write keploy cannot read names a call")
	}
}
