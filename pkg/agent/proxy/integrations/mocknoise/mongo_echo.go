package mocknoise

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A Mongo correlation names its places by the OP_MSG section that holds them:
// "section.<i>.<path>", where i is the section's index in the recorded
// request (RequestPath) or answer (ResponsePaths) and path is the
// jsonScalars path inside that section's document.
const mongoSectionPlace = "section."

// mongoMsgPrefix and mongoMsgSuffix frame a recorded OP_MSG body section:
// "{ SectionSingle msg: <extended JSON> }".
const (
	mongoMsgPrefix = "{ SectionSingle msg: "
	mongoMsgSuffix = " }"
)

// What the detector keeps of one mock is bounded. A place keeps its whole path,
// so places times path length, not the mock's size, is what they cost, and the
// correlations stay on the mock for the whole replay.
const (
	// mongoKeepBytes is what the places of one mock may cost, written and
	// answered together: 16 MiB, the largest document Mongo stores. A claim
	// keeps a few hundred bytes; with field names of ordinary length this is
	// about 100,000 places. The heap may hold a quarter more, as the allocator
	// rounds a path just over 32 KB up to 40 KB.
	mongoKeepBytes = 16 << 20
	// mongoPlaceCost is what a place costs besides its path's text: the
	// correlation or list entry that holds it, the value and the section's name.
	mongoPlaceCost = 128
	// mongoAnswerPlaces is how many places of a value's answer are listed. The
	// detector needs one, to know the value is answered. The render needs none
	// (RenderMongoEchoes goes by the value and RequestPath), so the list only
	// shows a reader where the answer holds the value.
	mongoAnswerPlaces = 4
)

// mongoKeep is what the detector may still keep of one mock, in bytes. Below
// zero, the mock holds more than it may keep.
type mongoKeep int

// take charges a place with a path of n bytes, and reports whether it fits.
func (k *mongoKeep) take(n int) bool {
	*k -= mongoKeep(n + mongoPlaceCost)
	return *k >= 0
}

// mongoMsgJSON returns the extended JSON document of an OP_MSG body section,
// and false for any other section (a document sequence, a section the
// recorder could not write as JSON) and for one that is not valid JSON, which
// is not read. The check copies the section's text once, and allocates
// nothing else.
func mongoMsgJSON(section string) (string, bool) {
	body, ok := strings.CutPrefix(section, mongoMsgPrefix)
	if !ok {
		return "", false
	}
	body, ok = strings.CutSuffix(body, mongoMsgSuffix)
	if !ok {
		return "", false
	}
	return body, json.Valid([]byte(body))
}

// mongoWritten reports whether path is a place whose value the application
// writes: a findAndModify's update ("update.<field>"), an update command's
// update document ("updates.u.<field>"), or an inserted document
// ("documents.<field>"). A command's filter (findAndModify's query, an
// update's q) is not one: a value there is the key of a lookup, and a lookup
// of another key must not be answered with this recording.
func mongoWritten(path []byte) bool {
	return bytes.HasPrefix(path, []byte("update.")) ||
		bytes.HasPrefix(path, []byte("updates.u.")) ||
		bytes.HasPrefix(path, []byte("documents."))
}

// mongoOpMsgSections returns the sections of a Mongo mock's one request and
// one answer (the caller's dispatch decides the kind), or none when the mock
// holds several requests or answers. A message of another opcode has no
// sections.
func mongoOpMsgSections(m *models.Mock) (req, resp []string) {
	if len(m.Spec.MongoRequests) != 1 || len(m.Spec.MongoResponses) != 1 {
		return nil, nil
	}
	return opMsgSections(m.Spec.MongoRequests[0].Message), opMsgSections(m.Spec.MongoResponses[0].Message)
}

func opMsgSections(msg interface{}) []string {
	if m, _ := msg.(*models.MongoOpMessage); m != nil {
		return m.Sections
	}
	return nil
}

// mongoCorrelations detects the values a recorded Mongo write echoes: a value
// the application writes (mongoWritten) that looks minted at random
// (mongoRandomClass) and that the recorded answer holds too, as findAndModify
// answers an upsert with the document it wrote. Such a value is seen twice in
// the recording, once sent and once answered. A value seen once (written and
// not answered, or only answered) is not a correlation, nor is a value in a
// filter.
//
// This runs for every Mongo mock when the agent takes the mocks in, so it
// reads as little as it can. A request section is read only when its raw
// text names a place a write sets (mongoMayWrite) and it shares a quoted
// value with its answer (mongoSharesAValue). A read (find, aggregate, getMore)
// costs a few substring searches, and a write whose answer echoes none of its
// values (an insert's answer is a count) one pass over each side. An answer
// section is read only when it quotes one of the values the request wrote
// (mongoAnswerHolds). A section that is read is walked (jsonScalars), never
// parsed into a document: what is kept is the written places that hold a
// random value and a few of the answer's places of those values.
//
// A value written at several places is a correlation per place, and at most
// mongoAnswerPlaces of the answer's places of that value are listed, once, on
// the first of them. The others only say that the live request must write the
// same value there (RenderMongoEchoes), so a value written at n places costs n
// entries, on the mock and on each copy of it, however often it is answered.
//
// A mock whose places cost more than mongoKeepBytes has no correlation at all,
// and one line says so: a list that left a place out could bind a value the
// live request writes at that place otherwise. Such a mock is served as
// recorded, as a mock with no echo is, so what reads its answer sees the
// recorded value and a test that needs the live one fails.
func mongoCorrelations(logger *zap.Logger, m *models.Mock) []models.FieldCorrelation {
	keep := mongoKeep(mongoKeepBytes)
	out := mongoEchoes(m, &keep)
	if keep >= 0 {
		return out
	}
	req, resp := mongoOpMsgSections(m)
	logger.Warn("a Mongo mock holds too many places to read its echoes: its answer is served as recorded, so a value the application mints is not rendered into it",
		zap.String("mock", m.Name), zap.Int("sectionBytes", mongoTextLen(req)+mongoTextLen(resp)), zap.Int("boundBytes", mongoKeepBytes))
	return nil
}

// mongoEchoes is mongoCorrelations' detection. Each place it keeps is charged
// to keep, which it leaves below zero when they cost too much, and what it
// returns then is not all there is.
func mongoEchoes(m *models.Mock, keep *mongoKeep) []models.FieldCorrelation {
	req, resp := mongoOpMsgSections(m)
	var written []models.FieldCorrelation
	for i, s := range req {
		if !mongoMayWrite(s) || !mongoSharesAValue(s, resp) {
			continue
		}
		written = append(written, mongoWrites(i, s, keep)...)
	}
	if len(written) == 0 {
		return nil
	}
	answered := make(map[string][]string, len(written))
	for _, c := range written {
		answered[c.RecordedValue] = nil
	}
	for j, s := range resp {
		if !mongoAnswerHolds(s, answered) {
			continue
		}
		doc, ok := mongoMsgJSON(s)
		if !ok {
			continue
		}
		jsonScalars(doc, func(path []byte, text string) {
			v := jsonStringText(text)
			if places, ok := answered[v]; ok && len(places) < mongoAnswerPlaces && keep.take(len(path)) {
				answered[v] = append(places, mongoSectionPlace+strconv.Itoa(j)+"."+string(path))
			}
		})
	}
	var out []models.FieldCorrelation
	for _, c := range written {
		if len(answered[c.RecordedValue]) > 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestPath < out[j].RequestPath })
	for i := range out {
		v := out[i].RecordedValue
		out[i].ResponsePaths, answered[v] = answered[v], nil
		sort.Strings(out[i].ResponsePaths)
	}
	return out
}

// A mongoWrite is a place of a request section that a write sets to a value
// mongoRandomClass takes, and the number of values the section holds there.
type mongoWrite struct {
	value, class string
	values       int
}

// mongoWrites returns the places of request section i that a write sets
// (mongoWritten) to one value, a string mongoRandomClass takes, and to no
// other, as correlations whose answer places are still to be found. A first
// walk of the section keeps the places that hold such a value; a second
// counts every value at them, so a place that holds several (the key_id of
// each document an insert writes) is not taken. Each place the first walk keeps
// is charged to keep, and one that does not fit is not kept.
func mongoWrites(i int, section string, keep *mongoKeep) []models.FieldCorrelation {
	doc, ok := mongoMsgJSON(section)
	if !ok {
		return nil
	}
	places := make(map[string]*mongoWrite)
	jsonScalars(doc, func(path []byte, text string) {
		if !mongoWritten(path) || places[string(path)] != nil {
			return
		}
		v := jsonStringText(text)
		if class, ok := mongoRandomClass(v); ok && keep.take(len(path)) {
			places[string(path)] = &mongoWrite{value: v, class: class}
		}
	})
	jsonScalars(doc, func(path []byte, _ string) {
		if w := places[string(path)]; w != nil {
			w.values++
		}
	})
	var out []models.FieldCorrelation
	for path, w := range places {
		if w.values == 1 {
			out = append(out, models.FieldCorrelation{
				RequestPath:   mongoSectionPlace + strconv.Itoa(i) + "." + path,
				RecordedValue: w.value,
				ValueClass:    w.class,
			})
		}
	}
	return out
}

// mongoRandomClass is appRandomClass for a value read from a Mongo section.
// The detector finds the value in an answer's raw text and the render writes
// the live value into it as raw text, so a value is taken only when its JSON
// text is the value itself. Every class's letters, digits, '-' and '_' are
// written as they are; space around the value is refused, as JSON escapes a
// tab or a newline (and the render would write it unescaped).
func mongoRandomClass(v string) (string, bool) {
	if strings.TrimSpace(v) != v {
		return "", false
	}
	return appRandomClass(v)
}

// mongoMayWrite reports whether a request section's raw text names a field
// that holds the places mongoWritten reads: findAndModify's "update", an
// update command's "updates" or an insert's "documents". The recorder writes a
// section as compact extended JSON (a key is followed by its colon), so a
// section that names none of them writes no value mongoWritten reads.
func mongoMayWrite(section string) bool {
	return strings.Contains(section, `"update":`) ||
		strings.Contains(section, `"updates":`) ||
		strings.Contains(section, `"documents":`)
}

// mongoSharesAValue reports whether the request section and the answer quote
// a common value that mongoRandomClass takes. Such a value has no quote,
// backslash or space in it, so its JSON text is the value between two
// consecutive quotes (mongoQuoted). A value a write sets and its answer echoes
// is always found; a value found that is not one only costs the parse.
//
// The smaller side's values are classified and kept, and the larger side's
// quoted texts are only looked up, so this costs one pass over each side and
// holds the smaller side's values: an insert of many documents answered with a
// count, or a findAndModify answered with a large document, is not classified
// value by value.
func mongoSharesAValue(req string, resp []string) bool {
	small, large := []string{req}, resp
	if mongoTextLen(resp) < len(req) {
		small, large = resp, small
	}
	values := make(map[string]bool)
	mongoQuoted(small, func(v string) bool {
		if _, random := mongoRandomClass(v); random {
			values[v] = true
		}
		return false
	})
	if len(values) == 0 {
		return false
	}
	return mongoQuoted(large, func(v string) bool { return values[v] })
}

// mongoTextLen is the size of the sections' text.
func mongoTextLen(sections []string) int {
	n := 0
	for _, s := range sections {
		n += len(s)
	}
	return n
}

// mongoQuoted calls f with the text between each pair of consecutive quotes in
// the sections, and reports whether f returned true for one, where it stops.
func mongoQuoted(sections []string, f func(string) bool) bool {
	for _, s := range sections {
		_, rest, more := strings.Cut(s, `"`)
		for more {
			var v string
			v, rest, more = strings.Cut(rest, `"`)
			if f(v) {
				return true
			}
		}
	}
	return false
}

// mongoAnswerHolds reports whether an answer section quotes one of the written
// values, the keys of answered. mongoRandomClass took each only when its JSON
// text is the value itself, so a section that does not quote a value does not
// answer it, and it is not read. This is one pass over the section, however
// many values were written.
func mongoAnswerHolds(section string, answered map[string][]string) bool {
	return mongoQuoted([]string{section}, func(v string) bool {
		_, ok := answered[v]
		return ok
	})
}

// RenderMongoEchoes returns the answer a recorded Mongo write serves to the
// live request liveSections, with each value the recording wrote and its
// answer echoed (corrs, from CorrelationsFromMock) replaced by the value the
// live request writes at the same place. The application minted that value
// on this run and reads its echo back: a findAndModify upsert that claims a
// slot answers with the document it wrote, and an answer carrying the
// recorded value tells the application it lost the claim.
//
// A value is replaced only when every place the recording wrote it holds, in
// the live request, one value of the same class (mongoRandomClass) that differs
// from the recorded one, and each such place is one a write sets
// (mongoWritten). Otherwise that value is left as recorded, which is what the
// answer was before correlations. resp itself is never changed: a changed
// answer is a copy, so a pooled recording stays as recorded.
//
// A value is replaced where it is a whole string scalar of an answer section
// the detector reads, which is every place the detector named for it. The same
// text inside a number, a longer string or a key is not that value, and stays.
//
// This runs on every serve of a mock with correlations, so the live request
// and the answer are walked (jsonScalars) once, not parsed, and only the live
// request's values at the correlations' places are kept.
func RenderMongoEchoes(corrs []models.FieldCorrelation, liveSections []string, resp *models.MongoOpMessage) *models.MongoOpMessage {
	if resp == nil || len(corrs) == 0 {
		return resp
	}
	live := make(map[string]*[]string, len(corrs)) // a correlation's place → the live JSON texts there
	for _, c := range corrs {
		live[c.RequestPath] = new([]string)
	}
	for i, s := range liveSections {
		doc, ok := mongoMsgJSON(s)
		if !ok {
			continue
		}
		place := []byte(mongoSectionPlace + strconv.Itoa(i) + ".")
		n := len(place)
		jsonScalars(doc, func(path []byte, text string) {
			if !mongoWritten(path) {
				return
			}
			place = append(place[:n], path...)
			if texts := live[string(place)]; texts != nil {
				*texts = append(*texts, text)
			}
		})
	}
	bound := make(map[string]string)
	refused := make(map[string]bool)
	for _, c := range corrs {
		if c.RecordedValue == "" {
			continue
		}
		vals := *live[c.RequestPath]
		if len(vals) != 1 {
			refused[c.RecordedValue] = true
			continue
		}
		v := jsonStringText(vals[0])
		if class, ok := mongoRandomClass(v); !ok || class != c.ValueClass {
			refused[c.RecordedValue] = true
			continue
		}
		if was, ok := bound[c.RecordedValue]; ok && was != v {
			refused[c.RecordedValue] = true
			continue
		}
		bound[c.RecordedValue] = v
	}
	renders := make(map[string]string, len(bound))
	for r, v := range bound {
		if !refused[r] && v != r {
			renders[r] = v
		}
	}
	if len(renders) == 0 {
		return resp
	}
	out := *resp
	out.Sections = make([]string, len(resp.Sections))
	for j, s := range resp.Sections {
		out.Sections[j] = mongoRenderSection(s, renders)
	}
	return &out
}

// mongoRenderSection returns an answer section with each string scalar that
// renders keys replaced by the live value it gives. A section the detector
// does not read names no place, and is returned as it is.
func mongoRenderSection(section string, renders map[string]string) string {
	doc, ok := mongoMsgJSON(section)
	if !ok {
		return section
	}
	var b strings.Builder
	b.Grow(len(section))
	b.WriteString(mongoMsgPrefix)
	jsonReplaceStrings(&b, doc, renders)
	b.WriteString(mongoMsgSuffix)
	return b.String()
}
