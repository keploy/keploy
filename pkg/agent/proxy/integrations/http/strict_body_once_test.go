package http

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// largeSchemaBody is a GraphQL call carrying a ~45 KB document as one string,
// with nested members, an array and an integer too large for a float64: the
// shape of a schema-validation call.
func largeSchemaBody(t *testing.T, op string, revision string, tags []string) string {
	t.Helper()
	var schema strings.Builder
	schema.WriteString("openapi: 3.0.0\npaths:\n")
	for i := 0; schema.Len() < 45000; i++ {
		schema.WriteString("  /books/")
		schema.WriteString(strings.Repeat("x", i%7+1))
		schema.WriteString(":\n    get:\n      responses: {\"200\": {description: \"ok\"}}\n")
	}
	b, err := json.Marshal(map[string]interface{}{
		"operationName": op,
		"query":         "mutation " + op + "($schema: String!) { validateSchema(schema: $schema) { valid error } }",
		"variables": map[string]interface{}{
			"schema": schema.String(),
			"meta":   map[string]interface{}{"revision": json.Number(revision), "tags": tags},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A call compared with a pool reads its body as JSON once, and every recording
// is told exactly what a comparison reading the body afresh tells it: the same
// differences, in either order, and the same fits and closest recording.
func TestACallComparedWithAPoolReadsItsBodyOnceWithTheSameOutcome(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	body := largeSchemaBody(t, "ValidateSchema", "123456789012345678901234567890", []string{"a", "b"})
	pool := []*models.Mock{
		recorded("same", "/query", ct, body),
		recorded("other-op", "/query", ct, `{"operationName":"GetAllGenJobs","query":"query GetAllGenJobs { getAllGenJobs { id } }","variables":{}}`),
		recorded("changed-field", "/query", ct, largeSchemaBody(t, "ValidateSchema", "123456789012345678901234567890", []string{"a", "c"})),
		recorded("big-integer", "/query", ct, largeSchemaBody(t, "ValidateSchema", "123456789012345678901234567891", []string{"a", "b"})),
		recorded("array-order", "/query", ct, largeSchemaBody(t, "ValidateSchema", "123456789012345678901234567890", []string{"b", "a"})),
		recorded("not-json", "/query", ct, `operationName=ValidateSchema`),
		recorded("form", "/query", form, `a=1&b=2`),
	}
	learned := recorded("learned", "/query", ct, largeSchemaBody(t, "ValidateSchema", "123456789012345678901234567890", []string{"a", "z"}))
	learned.Spec.ReqBodyNoise = map[string][]string{"body.variables.meta.tags[1]": {}}
	pool = append(pool, learned)
	noisy := map[string][]string{"variables.meta.revision": {}}

	fresh := func() *req { return sent("/query", ct, body) }
	ctx := context.Background()
	live := liveDestination(ctx, fresh())
	for _, noise := range []map[string][]string{nil, noisy} {
		n := (&HTTP{}).newStrictNoise(nil, noise, nil)
		want := make([][]strictField, len(pool))
		for i, m := range pool {
			want[i] = strictDiff(fresh(), live, m, n)
		}
		for _, order := range [][]int{{0, 1, 2, 3, 4, 5, 6, 7}, {7, 6, 5, 4, 3, 2, 1, 0}} {
			one := fresh()
			for _, i := range order {
				if got := strictDiff(one, live, pool[i], n); !reflect.DeepEqual(got, want[i]) {
					t.Fatalf("noise %v, recording %s: one reading of the body gave %v, a fresh one %v", noise, pool[i].Name, got, want[i])
				}
			}
		}
		// The pass as a whole: the same fits and the same closest recording as
		// when each recording is compared with a fresh reading of the body.
		var fitsWant []*models.Mock
		var nearWant nearest
		for i, m := range pool {
			if len(want[i]) == 0 {
				fitsWant = append(fitsWant, m)
				continue
			}
			if c := (nearest{mock: m, diffs: want[i], route: routeMatches(want[i])}); c.closerThan(nearWant) {
				nearWant = c
			}
		}
		fitsGot, nearGot, _ := strictCompare(ctx, fresh(), live, pool, n)
		if !reflect.DeepEqual(fitsGot, fitsWant) || nearGot.mock != nearWant.mock || !reflect.DeepEqual(nearGot.diffs, nearWant.diffs) {
			t.Fatalf("noise %v: strictCompare differs: fits %v / %v", noise, fitsGot, fitsWant)
		}
		if len(fitsWant) == 0 || fitsWant[0].Name != "same" {
			t.Fatalf("noise %v: the recording of this very call did not fit: %v", noise, fitsWant)
		}
	}

	// Read once: every later comparison is given the same decoded value.
	one := fresh()
	a, okA := one.jsonBody()
	b, okB := one.jsonBody()
	if !okA || !okB || reflect.ValueOf(a).Pointer() != reflect.ValueOf(b).Pointer() {
		t.Fatal("the call's body was read as JSON more than once")
	}
}

// A view of the call (the variable pass's call with recorded values put back)
// shares the call's reading of its body while nothing was put back inside the
// body; once a value is put back there, or into a form part, it reads its own.
func TestAViewOfACallSharesItsBodyReadingOnlyWhileTheBodyIsTheCalls(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	body := largeSchemaBody(t, "ValidateSchema", "7", []string{"a", "b"})
	in := sent("/query", ct, body)
	in.header.Set("Cookie", "session=live")

	header, err := variables.Parse("header.Cookie")
	if err != nil {
		t.Fatal(err)
	}
	v, ok := liveView(in).putBack(header, held{value: "session=recorded"})
	if !ok {
		t.Fatal("the header could not be put back")
	}
	out := v.asReq(in)
	if out.doc == nil || out.doc != in.doc || &out.body[0] != &in.body[0] {
		t.Fatal("a view with only a header put back did not share the call's body and its reading")
	}
	if out.header.Get("Cookie") != "session=recorded" || in.header.Get("Cookie") != "session=live" {
		t.Fatal("the view's header is not its own")
	}

	inBody, err := variables.Parse("body.variables.meta.tags[1]")
	if err != nil {
		t.Fatal(err)
	}
	v, ok = liveView(in).putBack(inBody, held{value: "z", raw: `"z"`, body: true})
	if !ok {
		t.Fatal("the body value could not be put back")
	}
	out = v.asReq(in)
	if out.doc != nil && out.doc == in.doc {
		t.Fatal("a view with a value put back inside the body shares the call's reading of it")
	}
	doc, ok := out.jsonBody()
	if !ok {
		t.Fatal("the view's body is not JSON")
	}
	tags := doc.(map[string]interface{})["variables"].(map[string]interface{})["meta"].(map[string]interface{})["tags"].([]interface{})
	if tags[1] != "z" {
		t.Fatalf("the view read %v, not the body with the value put back", tags)
	}
	if live, _ := in.jsonBody(); live.(map[string]interface{})["variables"].(map[string]interface{})["meta"].(map[string]interface{})["tags"].([]interface{})[1] != "b" {
		t.Fatal("putting a value back into the view changed the call's reading")
	}

	formCT := "multipart/form-data; boundary=XyZ"
	fb := "--XyZ\r\nContent-Disposition: form-data; name=\"testSetId\"\r\n\r\nlive\r\n--XyZ--\r\n"
	fin := sent("/upload", map[string]string{"Content-Type": formCT}, fb)
	part, err := variables.Parse("form.testSetId")
	if err != nil {
		t.Fatal(err)
	}
	fv, ok := liveView(fin).putBack(part, held{value: "recorded"})
	if !ok {
		t.Fatal("the form part could not be put back")
	}
	if fv.liveBody || fv.asReq(fin).doc != nil {
		t.Fatal("a view with a form part put back still counts as the call's body")
	}
}
