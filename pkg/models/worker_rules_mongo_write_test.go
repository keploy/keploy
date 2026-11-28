package models

import "testing"

func mongoWriteGuardMock(sections ...string) *Mock {
	return &Mock{Name: "m", Kind: Mongo, Spec: MockSpec{
		Metadata:      map[string]string{"destAddr": "mongo:27017"},
		MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: sections}}},
		MongoResponses: []MongoResponse{{Message: &MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"n":{"$numberInt":"0"},"nModified":{"$numberInt":"0"},"ok":{"$numberDouble":"1.0"}} }`}}}},
	}}
}

// A Mongo rule that names no command marks reads only: a write is marked only
// by a rule whose command names it, so a rule on a collection (or a db, or a
// destination and an answer) never moves a test's own insert, update, delete
// or findAndModify into the worker scope.
func TestAMongoRuleThatNamesNoCommandMarksReadsOnly(t *testing.T) {
	body := func(cmd string) string {
		return `{ SectionSingle msg: {"` + cmd + `":"ptw_endpoints","ordered":true,"$db":"enterprise"} }`
	}
	noCommand := []WorkerMockRule{
		{ID: "coll", Kind: "Mongo", Match: map[string]string{"collection": "ptw_endpoints"}},
		{ID: "db-coll", Kind: "Mongo", Match: map[string]string{"db": "enterprise", "collection": "ptw_endpoints"}},
		{ID: "dest-resp", Kind: "Mongo", Match: map[string]string{"destination": "mongo:27017", "response": `"nModified":`}},
		{ID: "db-resp", Kind: "Mongo", Match: map[string]string{"db": "enterprise", "response": `"ok":`}},
	}
	for _, r := range noCommand {
		if p := r.Validate(); len(p) > 0 {
			t.Fatalf("%s: Validate: %v", r.ID, p)
		}
		for _, w := range []string{"insert", "update", "delete", "findAndModify", "bulkWrite", "drop", "createIndexes", "listIndexes"} {
			if r.Matches(mongoWriteGuardMock(body(w))) {
				t.Errorf("rule %s names no command but marked a %s", r.ID, w)
			}
		}
		for _, stage := range []string{"$out", "$merge", "$changeStream"} {
			agg := `{ SectionSingle msg: {"aggregate":"ptw_endpoints","pipeline":[{"$match":{}},{"` + stage + `":"x"}],"$db":"enterprise"} }`
			if r.Matches(mongoWriteGuardMock(agg)) {
				t.Errorf("rule %s names no command but marked an aggregate with %s", r.ID, stage)
			}
		}
		legacy := mongoWriteGuardMock()
		legacy.Spec.MongoRequests[0].Message = &MongoOpUpdate{FullCollectionName: "enterprise.ptw_endpoints"}
		if r.Matches(legacy) {
			t.Errorf("rule %s names no command but marked an OP_UPDATE", r.ID)
		}
		for _, read := range []string{"find", "count", "distinct"} {
			if !r.Matches(mongoWriteGuardMock(body(read))) {
				t.Errorf("rule %s stopped marking a %s", r.ID, read)
			}
		}
		agg := `{ SectionSingle msg: {"aggregate":"ptw_endpoints","pipeline":[{"$match":{}}],"$db":"enterprise"} }`
		if !r.Matches(mongoWriteGuardMock(agg)) {
			t.Errorf("rule %s stopped marking a read-only aggregate", r.ID)
		}
	}
	for _, w := range []string{"update", "findAndModify", "insert"} {
		named := WorkerMockRule{ID: "named", Kind: "Mongo", Match: map[string]string{"db": "enterprise", "command": w, "collection": "ptw_endpoints"}}
		if !named.Matches(mongoWriteGuardMock(body(w))) {
			t.Errorf("a rule naming %s did not mark one", w)
		}
	}
	// A destination-only rule reads no request, as before Mongo had keys.
	dest := WorkerMockRule{ID: "dest", Kind: "Mongo", Match: map[string]string{"destination": "mongo:27017"}}
	if !dest.Matches(mongoWriteGuardMock(body("insert"))) {
		t.Errorf("a destination-only rule changed what it marks")
	}
}
