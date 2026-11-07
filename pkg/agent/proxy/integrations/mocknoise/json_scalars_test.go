package mocknoise

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// flattenOfScalars is what matcher.Flatten gives for doc, built from the
// scalars jsonScalars walks: each text decoded and written as Flatten writes
// a value.
func flattenOfScalars(t *testing.T, doc string) map[string][]string {
	t.Helper()
	out := make(map[string][]string)
	jsonScalars(doc, func(path []byte, text string) {
		if strings.TrimSpace(text) != text {
			t.Errorf("scalar %q at %q has space around it: a scalar's text is the scalar alone", text, path)
		}
		var v interface{}
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatalf("scalar %q at %q is not a JSON value: %v", text, path, err)
		}
		var s string
		switch v := v.(type) {
		case string:
			s = v
		case float64:
			s = strconv.FormatFloat(v, 'E', -1, 64)
		case bool:
			s = strconv.FormatBool(v)
		}
		out[string(path)] = append(out[string(path)], s)
	})
	return out
}

// jsonScalars walks a document as matcher.Flatten reads it: the same paths,
// with the same values in the same order, whatever space, nesting, escapes
// and scalars the document holds.
func TestJSONScalarsGivesFlattensPathsAndValues(t *testing.T) {
	for _, doc := range []string{
		claimRequest(recKeyID),
		claimAnswer(recKeyID),
		" {\t\"a\" :\r\n[ 1 ,\t-2.5e-3 , true,false , null ] ,\n\"b\":{ \"c\" : \"d\" }\r} \n",
		`{"a":[[1,[2]],{"b":[3,{"c":4}]},[]],"e":{},"f":[{}],"g":{"h":[]}}`,
		`{"s":"a 5\" screen","t":"x\\","u":"\u00e9\n","v":"é","w":"","x":0,"y":-0.5,"z":1E+21}`,
		`{"a":{"b":{"c":{"d":"e"}}},"a2":[{"b":"c"},{"b":"d"},{"x":"y"}]}`,
		`[{"a":1},{"a":2}]`,
		`{"":{"a":1},"b":{"":{"c":2}}}`,
		"{\"a\":1\t,\"b\":2\r,\"c\":3\n,\"d\":[4 ,5\t,6\r,7\n]}",
		`"a lone string"`,
		`12345`,
	} {
		var j interface{}
		if err := json.Unmarshal([]byte(doc), &j); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if got, want := flattenOfScalars(t, doc), matcher.Flatten(j); !reflect.DeepEqual(got, want) {
			t.Errorf("%q:\n walked    %v\n flattened %v", doc, got, want)
		}
	}
}

// refMongoCorrelations is the detector as a full parse gives it, with no gate
// and no walk: each request section flattened, its written places that hold
// one random value kept, and each answer section flattened for their values,
// listed on the first correlation of each value.
func refMongoCorrelations(m *models.Mock) []models.FieldCorrelation {
	doc := func(section string) map[string][]string {
		body, ok := strings.CutPrefix(section, mongoMsgPrefix)
		if body, ok2 := strings.CutSuffix(body, mongoMsgSuffix); ok && ok2 {
			return flattenJSONBody(body)
		}
		return nil
	}
	req, resp := mongoOpMsgSections(m)
	var written []models.FieldCorrelation
	for i, s := range req {
		for path, vals := range doc(s) {
			if !mongoWritten([]byte(path)) || len(vals) != 1 {
				continue
			}
			if class, ok := mongoRandomClass(vals[0]); ok {
				written = append(written, models.FieldCorrelation{RequestPath: mongoSectionPlace + strconv.Itoa(i) + "." + path, RecordedValue: vals[0], ValueClass: class})
			}
		}
	}
	answered := make(map[string][]string)
	for _, c := range written {
		answered[c.RecordedValue] = nil
	}
	for j, s := range resp {
		for path, vals := range doc(s) {
			for _, v := range vals {
				if places, ok := answered[v]; ok {
					answered[v] = append(places, mongoSectionPlace+strconv.Itoa(j)+"."+path)
				}
			}
		}
	}
	var out []models.FieldCorrelation
	for _, c := range written {
		if len(answered[c.RecordedValue]) > 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestPath < out[j].RequestPath })
	listed := make(map[string]bool)
	for i := range out {
		if v := out[i].RecordedValue; !listed[v] {
			listed[v] = true
			out[i].ResponsePaths = append([]string(nil), answered[v]...)
			sort.Strings(out[i].ResponsePaths)
		}
	}
	return out
}

// sameCorrelations reports whether the detector's correlations are the ones a
// full parse gives, which lists every answer place of a value where the
// detector lists as many as it keeps.
func sameCorrelations(got, want []models.FieldCorrelation) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		g, w := got[i], want[i]
		if len(g.ResponsePaths) != min(mongoAnswerPlaces, len(w.ResponsePaths)) {
			return false
		}
		places := make(map[string]int)
		for _, p := range w.ResponsePaths {
			places[p]++
		}
		for _, p := range g.ResponsePaths {
			if places[p]--; places[p] < 0 {
				return false
			}
		}
		g.ResponsePaths, w.ResponsePaths = nil, nil
		if !reflect.DeepEqual(g, w) {
			return false
		}
	}
	return true
}

// mockGen makes Mongo mocks of the shapes the detector reads: findAndModify,
// update, insert and find requests of one or two sections, answers of one to
// three, and values minted, plain, spaced, quoted, escaped, numbers and
// extended JSON, at places written and not.
type mockGen struct {
	r    *rand.Rand
	pool []string
}

var mockGenKeys = []string{"update", "updates", "documents", "query", "q", "u", "$set", "$setOnInsert", "value", "key_id", "_id", "n", "lastErrorObject", "upserted", "note", "alias", "history", "token"}

func (g *mockGen) scalar() string {
	v := g.pool[g.r.Intn(len(g.pool))]
	switch g.r.Intn(16) {
	case 0, 1, 2, 3, 4:
		return `"` + v + `"`
	case 5:
		return `" ` + v + `"`
	case 6:
		return `"pre-` + v + `"`
	case 7:
		return `"a 5\" screen"`
	case 8:
		return `"x\\"`
	case 9:
		return `"` + v + `\n"`
	case 10:
		return `{"$oid":"65a1b2c3d4e5f6a7b8c9d0e` + strconv.Itoa(g.r.Intn(3)) + `"}`
	case 11:
		return `"\"` + v + `\""`
	case 12:
		return `{"$numberInt":"` + strconv.Itoa(g.r.Intn(100)) + `"}`
	case 13:
		return `"read"`
	case 14:
		return `true`
	default:
		return strconv.Itoa(g.r.Intn(1000))
	}
}

func (g *mockGen) value(depth int) string {
	if depth <= 0 {
		return g.scalar()
	}
	switch g.r.Intn(5) {
	case 0:
		return g.object(depth - 1)
	case 1:
		parts := make([]string, g.r.Intn(3))
		for i := range parts {
			parts[i] = g.value(depth - 1)
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		return g.scalar()
	}
}

func (g *mockGen) object(depth int) string {
	seen := map[string]bool{}
	var parts []string
	for n := 1 + g.r.Intn(4); n > 0; n-- {
		k := mockGenKeys[g.r.Intn(len(mockGenKeys))]
		if !seen[k] {
			seen[k] = true
			parts = append(parts, `"`+k+`":`+g.value(depth))
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (g *mockGen) request() string {
	var parts []string
	switch g.r.Intn(5) {
	case 0:
		parts = append(parts, `"findAndModify":"t"`, `"query":`+g.object(2), `"update":`+g.object(3))
	case 1:
		parts = append(parts, `"findAndModify":"t"`, `"query":`+g.object(2), `"update":[`+g.object(3)+`]`)
	case 2:
		parts = append(parts, `"update":"t"`, `"updates":[{"q":`+g.object(1)+`,"u":`+g.object(2)+`,"upsert":true}]`)
	case 3:
		parts = append(parts, `"insert":"t"`, `"documents":[`+g.object(2)+`,`+g.object(2)+`]`)
	default:
		parts = append(parts, `"find":"t"`, `"filter":`+g.object(2))
	}
	parts = append(parts, `"$db":"api"`)
	g.r.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
	return "{" + strings.Join(parts, ",") + "}"
}

func (g *mockGen) mock() *models.Mock {
	req := []string{mongoMsg(g.request())}
	if g.r.Intn(2) == 0 {
		req = append(req, mongoMsg(g.object(2)))
	}
	resp := make([]string, 1+g.r.Intn(3))
	for i := range resp {
		resp[i] = mongoMsg(g.object(3))
	}
	return &models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests:  []models.MongoRequest{{Message: &models.MongoOpMessage{Sections: req}}},
		MongoResponses: []models.MongoResponse{{Message: &models.MongoOpMessage{Sections: resp}}},
	}}
}

// The detector, with its gates and its walk, finds exactly what a full parse
// of every section finds, on generated mocks of every shape it reads.
func TestTheMongoDetectorFindsWhatAFullParseFinds(t *testing.T) {
	g := &mockGen{r: rand.New(rand.NewSource(7)), pool: []string{recKeyID, liveKeyID, "0123456789abcdef0123", "aB3dE5gH7jK9mN1pQ3sT5vX", "plainwordnotrandom", "cid-1:user-1:jti-1"}}
	found := 0
	for i := 0; i < 20000; i++ {
		m := g.mock()
		got, want := mongoCorrelations(zap.NewNop(), m), refMongoCorrelations(m)
		if !sameCorrelations(got, want) {
			t.Fatalf("mock %d: detector %+v, full parse %+v\nrequest %q\nanswer %q", i, got, want,
				m.Spec.MongoRequests[0].Message.(*models.MongoOpMessage).Sections, m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage).Sections)
		}
		if len(want) > 0 {
			found++
		}
	}
	if found < 500 {
		t.Fatalf("only %d of the mocks have a correlation: the generator no longer reaches the detector", found)
	}
}

// jsonReplaceStrings replaces a whole string scalar and nothing else, and keeps
// every byte around it: the same text in a key, a number or a longer string
// stays, and a value longer or shorter than the one it replaces fits.
func TestJSONReplaceStringsReplacesOnlyAWholeStringScalar(t *testing.T) {
	repl := map[string]string{"abc1": "zz", "123e4": "longer-than-it-was", "swap1": "swap2", "swap2": "swap1"}
	same := func(doc string) [2]string { return [2]string{doc, doc} }
	for _, c := range [][2]string{
		{`"abc1"`, `"zz"`},
		{`{"a":"abc1"}`, `{"a":"zz"}`},
		{`["abc1","abc1"]`, `["zz","zz"]`},
		{" {\t\"a\" : [ \"abc1\" ,\n\"abc1\" ] , \"b\" : \"abc1\" } \n", " {\t\"a\" : [ \"zz\" ,\n\"zz\" ] , \"b\" : \"zz\" } \n"},
		{`{"a":"abc1","b":"123e4","c":"abc1","d":1}`, `{"a":"zz","b":"longer-than-it-was","c":"zz","d":1}`},
		{`{"abc1":1,"x":"abc1"}`, `{"abc1":1,"x":"zz"}`},
		{`{"n":123e4,"s":"123e4"}`, `{"n":123e4,"s":"longer-than-it-was"}`},
		{`{"x":"swap1","y":"swap2"}`, `{"x":"swap2","y":"swap1"}`},
		same(`{"a":"pre-abc1","b":"abc1-post","c":"\"abc1\"","d":"abc1\n"}`),
		same(`{"abc1":{"abc1":["x"]}}`),
		same(`{"a":"","b":0,"c":true,"d":null,"e":[],"f":{}}`),
		same(`123e4`),
	} {
		var b strings.Builder
		jsonReplaceStrings(&b, c[0], repl)
		if b.String() != c[1] {
			t.Errorf("%q: got %q, want %q", c[0], b.String(), c[1])
		}
	}
}

// replaceParsed is jsonReplaceStrings on a parsed document.
func replaceParsed(v interface{}, repl map[string]string) interface{} {
	switch x := v.(type) {
	case string:
		if r, ok := repl[x]; ok {
			return r
		}
	case []interface{}:
		for i := range x {
			x[i] = replaceParsed(x[i], repl)
		}
	case map[string]interface{}:
		for k := range x {
			x[k] = replaceParsed(x[k], repl)
		}
	}
	return v
}

// On generated documents of every shape the detector reads, the replace gives
// a valid document, and it is the document a parse with the same strings
// replaced gives.
func TestJSONReplaceStringsGivesWhatAParseGives(t *testing.T) {
	g := &mockGen{r: rand.New(rand.NewSource(11)), pool: []string{recKeyID, liveKeyID, "0123456789abcdef0123", "aB3dE5gH7jK9mN1pQ3sT5vX"}}
	repl := map[string]string{recKeyID: "fedcba9876543210fedc", "0123456789abcdef0123": liveKeyID}
	changed := 0
	for i := 0; i < 20000; i++ {
		doc := g.object(3)
		var b strings.Builder
		jsonReplaceStrings(&b, doc, repl)
		var got, want interface{}
		if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
			t.Fatalf("%q gave %q: %v", doc, b.String(), err)
		}
		if err := json.Unmarshal([]byte(doc), &want); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if want = replaceParsed(want, repl); !reflect.DeepEqual(got, want) {
			t.Fatalf("%q gave %q, a parse gives %v", doc, b.String(), want)
		}
		if b.String() != doc {
			changed++
		}
	}
	if changed < 500 {
		t.Fatalf("only %d of the documents changed: the generator no longer reaches the replace", changed)
	}
}

// A number is not a string: a number whose text, quotes taken off, would
// spell a written hex key is neither a written place of it nor an answer's.
func TestANumberIsNotReadAsAString(t *testing.T) {
	const hex = "234567890123456789e1"
	m := mongoMock(`{"findAndModify":"t","query":{"_id":"k"},"update":{"$set":{"n":1234567890123456789e10,"k":"`+hex+`"}},"new":true}`,
		`{"value":{"n":1234567890123456789e10,"k":"`+hex+`"},"ok":1}`)
	want := []models.FieldCorrelation{{RequestPath: "section.0.update.$set.k", ResponsePaths: []string{"section.0.value.k"}, RecordedValue: hex, ValueClass: "hex"}}
	if got := CorrelationsFromMock(zap.NewNop(), m); !reflect.DeepEqual(got, want) {
		t.Fatalf("correlations = %+v\nwant %+v", got, want)
	}
}

// claimWithLines is a claim that also writes n plain order lines, and the
// answer with the document after the write.
func claimWithLines(keyID string, n int) (req, answer string) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"line":"item number %d of the order","qty":{"$numberInt":"%d"}}`, i, i%9)
	}
	lines := "[" + b.String() + "]"
	return `{"findAndModify":"t","query":{"_id":"s"},"update":{"$set":{"key_id":"` + keyID + `","lines":` + lines + `}},"upsert":true,"new":true,"$db":"api"}`,
		`{"lastErrorObject":{"n":1,"updatedExisting":false},"value":{"_id":"s","key_id":"` + recKeyID + `","lines":` + lines + `},"ok":1}`
}

// A write whose answer echoes its minted key is read at ingest and on each
// serve in about one copy of its text: its sections are walked, not parsed,
// and only the key's places are kept. A parse of these 1.3 MB sections costs
// a million allocations and over 100 MB.
func TestAnEchoedWriteIsReadInAboutACopyOfItsText(t *testing.T) {
	switchOff(t)
	req, answer := claimWithLines(recKeyID, 20000)
	m := mongoMock(req, answer)
	allocs, bytes, got := ingestCost(m)
	if len(got) != 1 || got[0].RequestPath != "section.0.update.$set.key_id" {
		t.Fatalf("correlations = %+v, want the key", got)
	}
	if limit := uint64(len(req)+len(answer)) + 256<<10; !raceDetector && (allocs > 1000 || bytes > limit) {
		t.Errorf("ingest: %d allocations and %d bytes, want at most 1000 and %d", allocs, bytes, limit)
	}
	m.Spec.Correlations = got
	live, _ := claimWithLines(liveKeyID, 20000)
	recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
	var served *models.MongoOpMessage
	allocs = uint64(testing.AllocsPerRun(1, func() { served = RenderMongoEchoes(got, []string{mongoMsg(live)}, recorded) }))
	if served == recorded || !strings.Contains(served.Sections[0], liveKeyID) {
		t.Fatalf("serve: the live key is not answered")
	}
	if !raceDetector && allocs > 1000 {
		t.Errorf("serve: %.0f allocations, want at most 1000", float64(allocs))
	}
	// A write that sets many minted values, each at a place that holds many,
	// and is answered with them keeps none of them past the read: what it
	// costs is the copy of the request it walks and the set of the answer's
	// values that mongoSharesAValue looks the request's up in.
	docs := manyDocs(20000)
	set := mongoMock(`{"findAndModify":"t","query":{"_id":"k"},"update":{"$set":{"history":`+docs+`}},"new":true,"$db":"api"}`,
		`{"value":{"_id":"k","history":`+docs+`},"ok":1}`)
	checkIngestCostWithin(t, "a write of many minted values answered with them", set, 3*uint64(len(docs)))
}

// checkIngestCostWithin is checkIngestCost for a mock whose sections are
// walked: it may cost up to limit bytes.
func checkIngestCostWithin(t *testing.T, name string, m *models.Mock, limit uint64) {
	t.Helper()
	allocs, bytes, got := ingestCost(m)
	if len(got) != 0 {
		t.Errorf("%s: correlations = %+v, want none", name, got)
	}
	if !raceDetector && (allocs > 1000 || bytes > limit) {
		t.Errorf("%s: %d allocations and %d bytes, want at most 1000 and %d: its values were kept", name, allocs, bytes, limit)
	}
}
