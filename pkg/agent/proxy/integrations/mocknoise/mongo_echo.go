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
// (appRandomClass) and that the recorded answer holds too, as findAndModify
// answers an upsert with the document it wrote. Such a value is seen twice in
// the recording, once sent and once answered. A value seen once (written and
// not answered, or only answered) is not a correlation, nor is a value in a
// filter.
func mongoCorrelations(m *models.Mock) []models.FieldCorrelation {
	req, resp := mongoOpMsgSections(m)
	answered := make(map[string][]string)
	for j, s := range resp {
		for path, vals := range mongoMsgDoc(s) {
			for _, v := range vals {
				answered[v] = append(answered[v], mongoSectionPlace+strconv.Itoa(j)+"."+path)
			}
		}
	}
	var out []models.FieldCorrelation
	for i, s := range req {
		for path, vals := range mongoMsgDoc(s) {
			if !mongoWritten(path) || len(vals) != 1 {
				continue
			}
			v := vals[0]
			class, ok := appRandomClass(v)
			if !ok {
				continue
			}
			places, echoed := answered[v]
			if !echoed {
				continue
			}
			rp := append([]string(nil), places...)
			sort.Strings(rp)
			out = append(out, models.FieldCorrelation{
				RequestPath:   mongoSectionPlace + strconv.Itoa(i) + "." + path,
				ResponsePaths: rp,
				RecordedValue: v,
				ValueClass:    class,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestPath < out[j].RequestPath })
	return out
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
// the live request, one value of the same class (appRandomClass) that differs
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
		if class, ok := appRandomClass(v); !ok || class != c.ValueClass {
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
