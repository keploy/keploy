package mocknoise

import (
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// A Mongo correlation names its places by the OP_MSG section that holds them:
// "section.<i>.<path>", where i is the section's index in the recorded
// request (RequestPath) or answer (ResponsePaths) and path is the
// matcher.Flatten path inside that section's document.
const mongoSectionPlace = "section."

// mongoMsgPrefix and mongoMsgSuffix frame a recorded OP_MSG body section:
// "{ SectionSingle msg: <extended JSON> }".
const (
	mongoMsgPrefix = "{ SectionSingle msg: "
	mongoMsgSuffix = " }"
)

// mongoMsgDoc returns the flattened document of an OP_MSG body section, or
// nil for any other section (a document sequence, a section the recorder
// could not write as JSON).
func mongoMsgDoc(section string) map[string][]string {
	body, ok := strings.CutPrefix(section, mongoMsgPrefix)
	if !ok {
		return nil
	}
	body, ok = strings.CutSuffix(body, mongoMsgSuffix)
	if !ok {
		return nil
	}
	return flattenJSONBody(body)
}

// mongoWritten reports whether path is a place whose value the application
// writes: a findAndModify's update ("update.<field>"), an update command's
// update document ("updates.u.<field>"), or an inserted document
// ("documents.<field>"). A command's filter (findAndModify's query, an
// update's q) is not one: a value there is the key of a lookup, and a lookup
// of another key must not be answered with this recording.
func mongoWritten(path string) bool {
	return strings.HasPrefix(path, "update.") ||
		strings.HasPrefix(path, "updates.u.") ||
		strings.HasPrefix(path, "documents.")
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
// parses as little as it can. A request section is parsed only when its raw
// text names a place a write sets (mongoMayWrite) and it shares a quoted
// value with its answer (mongoSharesAValue). A read (find, aggregate, getMore),
// however large its filter, costs a few substring searches, and a write whose
// answer echoes none of its values (an insert's answer is a count) one pass
// over each side with no parse. An answer
// section is parsed only when its raw text holds one of the values the
// request wrote (mongoAnswerHolds), so a read's answer is never parsed.
func mongoCorrelations(m *models.Mock) []models.FieldCorrelation {
	req, resp := mongoOpMsgSections(m)
	var written []models.FieldCorrelation
	for i, s := range req {
		if !mongoMayWrite(s) || !mongoSharesAValue(s, resp) {
			continue
		}
		for path, vals := range mongoMsgDoc(s) {
			if !mongoWritten(path) || len(vals) != 1 {
				continue
			}
			class, ok := mongoRandomClass(vals[0])
			if !ok {
				continue
			}
			written = append(written, models.FieldCorrelation{
				RequestPath:   mongoSectionPlace + strconv.Itoa(i) + "." + path,
				RecordedValue: vals[0],
				ValueClass:    class,
			})
		}
	}
	answered := make(map[string][]string, len(written))
	for _, c := range written {
		answered[c.RecordedValue] = nil
	}
	for j, s := range resp {
		if !mongoAnswerHolds(s, written) {
			continue
		}
		for path, vals := range mongoMsgDoc(s) {
			for _, v := range vals {
				if places, ok := answered[v]; ok {
					answered[v] = append(places, mongoSectionPlace+strconv.Itoa(j)+"."+path)
				}
			}
		}
	}
	var out []models.FieldCorrelation
	for _, c := range written {
		places := answered[c.RecordedValue]
		if len(places) == 0 {
			continue
		}
		c.ResponsePaths = append([]string(nil), places...)
		sort.Strings(c.ResponsePaths)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestPath < out[j].RequestPath })
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
	n := 0
	for _, s := range resp {
		n += len(s)
	}
	if n < len(req) {
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

// mongoAnswerHolds reports whether an answer section's raw text holds one of
// the written values. mongoRandomClass took each only when its JSON text is
// the value itself, so a section whose text does not hold a value does not
// answer it, and it is not parsed.
func mongoAnswerHolds(section string, written []models.FieldCorrelation) bool {
	for _, c := range written {
		if strings.Contains(section, c.RecordedValue) {
			return true
		}
	}
	return false
}

// mongoPlace splits a Mongo correlation place into its section index and the
// path inside that section.
func mongoPlace(place string) (int, string, bool) {
	rest, ok := strings.CutPrefix(place, mongoSectionPlace)
	if !ok {
		return 0, "", false
	}
	idx, path, _ := strings.Cut(rest, ".")
	if path == "" {
		return 0, "", false
	}
	i, err := strconv.Atoi(idx)
	if err != nil || i < 0 {
		return 0, "", false
	}
	return i, path, true
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
// from the recorded one. Otherwise that value is left as recorded, which is
// what the answer was before correlations. resp itself is never changed: a
// changed answer is a copy, so a pooled recording stays as recorded.
func RenderMongoEchoes(corrs []models.FieldCorrelation, liveSections []string, resp *models.MongoOpMessage) *models.MongoOpMessage {
	if resp == nil || len(corrs) == 0 {
		return resp
	}
	bound := make(map[string]string)
	refused := make(map[string]bool)
	for _, c := range corrs {
		if c.RecordedValue == "" {
			continue
		}
		i, path, ok := mongoPlace(c.RequestPath)
		if !ok {
			refused[c.RecordedValue] = true
			continue
		}
		var vals []string
		if i < len(liveSections) {
			vals = mongoMsgDoc(liveSections[i])[path]
		}
		if len(vals) != 1 {
			refused[c.RecordedValue] = true
			continue
		}
		v := vals[0]
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
	recorded := make([]string, 0, len(bound))
	for r, v := range bound {
		if !refused[r] && v != r {
			recorded = append(recorded, r)
		}
	}
	if len(recorded) == 0 {
		return resp
	}
	sort.Strings(recorded)
	pairs := make([]string, 0, 2*len(recorded))
	for _, r := range recorded {
		pairs = append(pairs, r, bound[r])
	}
	rep := strings.NewReplacer(pairs...)
	out := *resp
	out.Sections = make([]string, len(resp.Sections))
	for j, s := range resp.Sections {
		out.Sections[j] = rep.Replace(s)
	}
	return &out
}
