package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// mongoCall is the command a Mongo request runs and the namespace it runs on,
// as a worker-mock rule reads it (WorkerMockRule.matchesMongo,
// WorkerMockCallOf): {"update": "jobs", ..., "$db": "app"} runs update on
// app.jobs. A command on no collection (hello, ping, a database's aggregate)
// has none.
type mongoCall struct {
	command, database, collection string
}

// mongoSectionSingle is how a recorded OP_MSG writes its body, its kind-0
// section: the command document as extended JSON, between this and " }".
const mongoSectionSingle = "{ SectionSingle msg: "

// mongoCalls is the call of each request m records, nil when keploy cannot
// read the command of one of them: a rule that names a command or namespace
// then matches no request of the mock, and the mock names no call. An OP_MSG
// is read from its body; a legacy OP_QUERY from its query, a command when it
// runs on "<db>.$cmd", else a find on the collection it names.
func mongoCalls(m *Mock) []mongoCall {
	if m == nil || len(m.Spec.MongoRequests) == 0 {
		return nil
	}
	out := make([]mongoCall, 0, len(m.Spec.MongoRequests))
	for _, req := range m.Spec.MongoRequests {
		c, ok := mongoCallOf(req.Message)
		if !ok {
			return nil
		}
		out = append(out, c)
	}
	return out
}

func mongoCallOf(msg interface{}) (mongoCall, bool) {
	switch v := msg.(type) {
	case *MongoOpMessage:
		if v == nil {
			return mongoCall{}, false
		}
		for _, section := range v.Sections {
			if !strings.HasPrefix(section, mongoSectionSingle) || !strings.HasSuffix(section, " }") {
				continue
			}
			return mongoCommandOf(section[len(mongoSectionSingle) : len(section)-len(" }")])
		}
	case *MongoOpQuery:
		if v == nil {
			return mongoCall{}, false
		}
		db, coll, ok := strings.Cut(v.FullCollectionName, ".")
		if !ok || db == "" || coll == "" {
			return mongoCall{}, false
		}
		if coll != "$cmd" {
			return mongoCall{command: "find", database: db, collection: coll}, true
		}
		c, ok := mongoCommandOf(v.Query)
		if !ok {
			return mongoCall{}, false
		}
		if c.database == "" {
			c.database = db
		}
		return c, true
	}
	return mongoCall{}, false
}

// mongoCommandOf reads a command document (extended JSON): its command is its
// first member's name, the collection that member's value when it is a string
// (getMore's is its "collection" member), and the database its "$db". A
// legacy command a driver wrapped with a read preference ({"$query": {...}})
// is read from inside. Each member's value is skipped, not decoded, but for
// the few read here.
func mongoCommandOf(doc string) (mongoCall, bool) {
	dec := json.NewDecoder(strings.NewReader(doc))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return mongoCall{}, false
	}
	var c mongoCall
	var named string
	first := true
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return mongoCall{}, false
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return mongoCall{}, false
		}
		switch {
		case first:
			first = false
			if key == "$query" {
				inner, ok := mongoCommandOf(string(raw))
				if !ok {
					return mongoCall{}, false
				}
				c.command, c.collection = inner.command, inner.collection
				if c.database == "" {
					c.database = inner.database
				}
				continue
			}
			c.command = key
			_ = json.Unmarshal(raw, &c.collection) // "" when not a string
		case key == "$db":
			_ = json.Unmarshal(raw, &c.database)
		case key == "collection":
			_ = json.Unmarshal(raw, &named)
		}
	}
	if strings.EqualFold(c.command, "getMore") {
		c.collection = named
	}
	return c, c.command != ""
}

// mongoSectionDocuments is how a recorded OP_MSG writes a kind-1 section: its
// identifier (documents, updates, deletes), then its documents as an extended
// JSON array.
const mongoSectionDocuments = "{ SectionSingle identifier: "

// mongoCallKeys is the call each request m records names, for
// WorkerMockCallOf: its command and namespace, and the shape of what it sends
// (mongoRequestShape). nil when keploy cannot read one of them.
func mongoCallKeys(m *Mock) []string {
	if m == nil || len(m.Spec.MongoRequests) == 0 {
		return nil
	}
	out := make([]string, 0, len(m.Spec.MongoRequests))
	for _, req := range m.Spec.MongoRequests {
		c, ok := mongoCallOf(req.Message)
		if !ok {
			return nil
		}
		shape, ok := mongoRequestShape(req.Message)
		if !ok {
			return nil
		}
		out = append(out, c.command+" "+c.database+"."+c.collection+" "+shape)
	}
	return out
}

// mongoVolatile are the members of a command document that name the session
// or the moment a call was made, not the call: a driver's pooled session, its
// transaction, the cluster time it last saw.
var mongoVolatile = map[string]bool{
	"lsid": true, "txnNumber": true, "autocommit": true, "startTransaction": true, "readConcern": true,
}

// mongoRequestShape is the shape of what a request sends, as a digest: the paths of
// the members of its command document (but the session's, mongoVolatile, and
// the $-members a driver adds: $db, $clusterTime, $readPreference) and of the
// documents of its kind-1 sections, under their identifier; names, not values.
// The same call made again with a new time, id or count has one shape; a call
// that sets other fields, or upserts, has another.
func mongoRequestShape(msg interface{}) (string, bool) {
	paths := map[string]struct{}{}
	switch v := msg.(type) {
	case *MongoOpMessage:
		if v == nil {
			return "", false
		}
		for _, section := range v.Sections {
			switch {
			case strings.HasPrefix(section, mongoSectionSingle) && strings.HasSuffix(section, " }"):
				var doc map[string]interface{}
				if json.Unmarshal([]byte(section[len(mongoSectionSingle):len(section)-len(" }")]), &doc) != nil {
					return "", false
				}
				for k, val := range doc {
					if mongoVolatile[k] || strings.HasPrefix(k, "$") {
						continue
					}
					shapePaths(k, val, paths)
				}
			case strings.HasPrefix(section, mongoSectionDocuments):
				rest := section[len(mongoSectionDocuments):]
				id, docs, ok := strings.Cut(rest, " , msgs: ")
				if !ok || !strings.HasSuffix(docs, " }") {
					return "", false
				}
				var list []interface{}
				if json.Unmarshal([]byte(strings.TrimSuffix(docs, " }")), &list) != nil {
					return "", false
				}
				shapePaths(strings.TrimSpace(id)+"[]", list, paths)
			}
		}
	case *MongoOpQuery:
		if v == nil {
			return "", false
		}
		var doc interface{}
		if json.Unmarshal([]byte(v.Query), &doc) != nil {
			return "", false
		}
		shapePaths("query", doc, paths)
	default:
		return "", false
	}
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:8]), true
}

// extJSONTypes are the members of an extended JSON value's wrapper: a value,
// not a document ({"$date": ...} is a time).
var extJSONTypes = map[string]bool{
	"$oid": true, "$date": true, "$numberInt": true, "$numberLong": true, "$numberDouble": true,
	"$numberDecimal": true, "$binary": true, "$timestamp": true, "$regularExpression": true,
	"$minKey": true, "$maxKey": true, "$undefined": true, "$symbol": true, "$code": true,
	"$dbPointer": true, "$uuid": true,
}

// shapePaths adds the paths of v's members, under prefix, to paths: a
// document's members by name, an array's elements as prefix[] (one shape for
// any number of them), a value (an extended JSON one too) as its path.
func shapePaths(prefix string, v interface{}, paths map[string]struct{}) {
	switch x := v.(type) {
	case map[string]interface{}:
		if len(x) == 1 {
			for k := range x {
				if extJSONTypes[k] {
					paths[prefix] = struct{}{}
					return
				}
			}
		}
		if len(x) == 0 {
			paths[prefix+"{}"] = struct{}{}
		}
		for k, val := range x {
			shapePaths(prefix+"."+k, val, paths)
		}
	case []interface{}:
		if len(x) == 0 {
			paths[prefix+"[]"] = struct{}{}
		}
		for _, e := range x {
			shapePaths(prefix+"[]", e, paths)
		}
	default:
		paths[prefix] = struct{}{}
	}
}
