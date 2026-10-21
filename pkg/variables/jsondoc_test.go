package variables

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/templates"
)

// randomJSON is a document of nested objects and arrays, with keys drawn
// from a small set (so a key repeats in one object: the reader's value is
// the last) and keys holding '.', '"' or ':'.
func randomJSON(r *rand.Rand, depth int) string {
	keys := []string{"id", "a", "a.b", `q"x`, "k:1", "id", "v"}
	scalar := func() string {
		switch r.Intn(5) {
		case 0:
			return fmt.Sprintf("%d", r.Intn(1000))
		case 1:
			return `"s` + fmt.Sprint(r.Intn(50)) + `"`
		case 2:
			return "true"
		case 3:
			return "null"
		}
		return `{"$numberLong":"` + fmt.Sprint(r.Int63n(1<<40)) + `"}`
	}
	if depth == 0 {
		return scalar()
	}
	switch r.Intn(3) {
	case 0:
		n := r.Intn(4)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = randomJSON(r, depth-1)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case 1:
		n := r.Intn(5)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("%q:%s", keys[r.Intn(len(keys))], randomJSON(r, depth-1))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return scalar()
}

// A document read once finds the scalar JSONLeafAt finds at every path, and
// makes the edit AnswerEdit makes: at each leaf's path (a shadowed one's
// too), at a path one step longer, and at one no leaf has.
func TestAJSONDocReadOnceIsJSONLeafAtAtEveryPath(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	docs := []string{
		`{"id":"a","id":{"x":1}}`,
		`{"a":[{"b":1},{"b":2}],"a.b":"dotted"}`,
		`{"cursor":{"firstBatch":[{"_id":"x","n":{"$numberLong":"5"}}]}}`,
		`[1,"two",[3]]`,
		`"scalar"`,
		`{"a":1,`,
	}
	for i := 0; i < 400; i++ {
		docs = append(docs, randomJSON(r, 4))
	}
	checked := 0
	for _, doc := range docs {
		d, ok := ReadJSONDoc(doc)
		if ok != IsJSONDoc(doc) {
			t.Fatalf("ReadJSONDoc(%s) read=%v, IsJSONDoc=%v", doc, ok, IsJSONDoc(doc))
		}
		if !ok {
			continue
		}
		leaves, _ := templates.JSONLeaves(doc)
		paths := [][]templates.PathSeg{nil, {{Key: "missing"}}}
		for _, l := range leaves {
			paths = append(paths, l.Path, append(append([]templates.PathSeg(nil), l.Path...), templates.PathSeg{Index: 0, IsIndex: true}))
		}
		for _, p := range paths {
			want, wantOK := JSONLeafAt(doc, p)
			got, gotOK := d.LeafAt(p)
			if gotOK != wantOK || fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s at %v: read once %+v %v, JSONLeafAt %+v %v", doc, p, got, gotOK, want, wantOK)
			}
			if wantOK {
				rec := want.Value(doc)
				for _, v := range []string{rec, "7", "new"} {
					we, wwhy := AnswerEdit(doc, p, rec, v)
					ge, gwhy := d.AnswerEdit(p, rec, v)
					if we != ge || wwhy != gwhy {
						t.Fatalf("%s at %v with %q: read once %+v %q, AnswerEdit %+v %q", doc, p, v, ge, gwhy, we, wwhy)
					}
				}
			}
			checked++
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d paths checked", checked)
	}
}

// A run report's answer of 67 suites, its variables at 300 places, as the
// mongo variable pass renders it.
func BenchmarkAnswerEditsOfAReport(b *testing.B) {
	var suites []string
	for i := 0; i < 67; i++ {
		suites = append(suites, fmt.Sprintf(`{"_id":"suite-%d","app_id":"app","job_id":"job","name":"Suite %d","steps":[{"id":"s%d-0","status":"PASSED"},{"id":"s%d-1","status":"FAILED"}],"created_at":{"$numberLong":"1792555003266"}}`, i, i, i, i))
	}
	doc := `{"cursor":{"firstBatch":[` + strings.Join(suites, ",") + `],"id":{"$numberLong":"0"}},"ok":1}`
	var paths [][]templates.PathSeg
	for i := 0; i < 300; i++ {
		paths = append(paths, []templates.PathSeg{{Key: "cursor"}, {Key: "firstBatch"}, {Index: i % 67, IsIndex: true}, {Key: "job_id"}})
	}
	b.Run("JSONLeafAt per place", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, p := range paths {
				JSONLeafAt(doc, p)
				AnswerEdit(doc, p, "job", "job-2")
			}
		}
	})
	b.Run("read once", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			d, _ := ReadJSONDoc(doc)
			for _, p := range paths {
				d.LeafAt(p)
				d.AnswerEdit(p, "job", "job-2")
			}
		}
	})
}
