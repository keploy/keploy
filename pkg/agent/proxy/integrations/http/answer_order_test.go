package http

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func gqlBody(query, op, vars string) []byte {
	b := `{"operationName":"` + op + `","query":` + jsonString(query)
	if vars != "" {
		b += `,"variables":` + vars
	}
	return []byte(b + "}")
}

func jsonString(s string) string {
	out := []byte{'"'}
	for _, c := range []byte(s) {
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, c)
		}
	}
	return string(append(out, '"'))
}

// A page's two queries that select one root field with the same arguments
// are filed by its cache under one entry: graphQLRoot reads each as that
// field with its arguments, a variable given its value.
func TestGraphQLRootReadsWhatAQuerySelectsAtItsRoot(t *testing.T) {
	page := "query GetBranchPage($branchId: String!) {\n  branch(branchId: $branchId) { id name commits { id } }\n  me { id }\n}"
	pulse := "query GetBranchPulse($id: String!) { branch(branchId: $id) { id updatedAt } }"
	for _, c := range []struct {
		name, ct string
		body     []byte
		want     []string
	}{
		{"the page", "application/json", gqlBody(page, "GetBranchPage", `{"branchId":"b-1"}`), []string{`branch(branchId="b-1")`, "me"}},
		{"the pulse, its variable named otherwise", "application/json; charset=utf-8", gqlBody(pulse, "GetBranchPulse", `{"id": "b-1"}`), []string{`branch(branchId="b-1")`}},
		{"another branch", "application/json", gqlBody(pulse, "GetBranchPulse", `{"id":"b-2"}`), []string{`branch(branchId="b-2")`}},
		{"an alias", "application/json", gqlBody(`query Q { b: branch(branchId: "b-1") { id } }`, "Q", ""), []string{`branch(branchId="b-1")`}},
		{"a list and an object argument", "application/json", gqlBody(`query Q($s: Int) { apps(filter: {ids: ["a", "b"], size: $s}, sort: [NAME]) { id } }`, "Q", `{"s":10}`), []string{`apps(filter={ids:["a""b"]size:10},sort=[NAME])`}},
		{"a mutation", "application/json", gqlBody(`mutation M { deleteBranch(id: "b-1") }`, "M", ""), nil},
		{"a fragment at the root", "application/json", gqlBody("query Q { ...F } fragment F on Query { branch(branchId: \"b-1\") { id } }", "Q", ""), nil},
		{"two operations, the one named", "application/json", gqlBody(`query A { a } query B { b(x: 1) }`, "B", ""), []string{"b(x=1)"}},
		{"two operations, none named", "application/json", gqlBody(`query A { a } query B { b }`, "", ""), nil},
		{"not JSON", "text/plain", []byte(`{"query":"{ a }"}`), nil},
	} {
		if got := graphQLRoot(c.ct, c.body); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func answeredAt(name string, req, res time.Duration) *models.Mock {
	base := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	return &models.Mock{Name: name, Spec: models.MockSpec{ReqTimestampMock: base.Add(req), ResTimestampMock: base.Add(res)}}
}

// A page sent GetBranchPage, then GetBranchPulse, and the recording answered
// the pulse first. The page's answer waits while the pulse's call is in
// flight and being matched, and while it is matched to the recording answered
// first; it is written once the pulse's answer is. The pulse's answer waits
// for nothing (the page's was answered after it). A call that selects another
// field, or one matched to a recording answered after, holds nothing back.
func TestAnswerOrderWritesInTheRecordedOrder(t *testing.T) {
	keys := []string{`branch(branchId="b-1")`}
	pageRec := answeredAt("page", 0, 41*time.Millisecond)
	pulseRec := answeredAt("pulse", time.Millisecond, 20*time.Millisecond)
	ctx := context.Background()
	released := func(o *answerOrder, c *orderedCall) chan struct{} {
		done := make(chan struct{})
		go func() { o.hold(ctx, c); close(done) }()
		return done
	}
	isHeld := func(done chan struct{}) bool {
		select {
		case <-done:
			return false
		case <-time.After(50 * time.Millisecond):
			return true
		}
	}

	var o answerOrder
	page, pulse := o.begin(keys), o.begin(keys)
	o.matched(page, pageRec)
	done := released(&o, page)
	if !isHeld(done) {
		t.Fatal("the page's answer was written while the pulse's call was being matched")
	}
	o.matched(pulse, pulseRec)
	if !isHeld(done) {
		t.Fatal("the page's answer was written before the pulse's, which the recording answered first")
	}
	pulseDone := released(&o, pulse)
	if isHeld(pulseDone) {
		t.Fatal("the pulse's answer waited for the page's, which the recording answered after it")
	}
	o.end(pulse)
	if isHeld(done) {
		t.Fatal("the page's answer still waits once the pulse's is written")
	}
	o.end(page)

	// Another field holds nothing back.
	page = o.begin(keys)
	other := o.begin([]string{"me"})
	o.matched(page, pageRec)
	if isHeld(released(&o, page)) {
		t.Fatal("a call that selects another field held the page's answer")
	}
	o.end(other)
	o.end(page)

	// A call matched to a recording answered after the page's holds nothing.
	page, late := o.begin(keys), o.begin(keys)
	o.matched(page, pageRec)
	o.matched(late, answeredAt("late-pulse", 2*time.Millisecond, 60*time.Millisecond))
	if isHeld(released(&o, page)) {
		t.Fatal("a call answered after the page's at record held it")
	}
	o.end(late)
	o.end(page)

	// Two recordings not in flight together at record: no order to keep.
	page, before := o.begin(keys), o.begin(keys)
	o.matched(page, pageRec)
	o.matched(before, answeredAt("earlier-pulse", -time.Second, -900*time.Millisecond))
	if isHeld(released(&o, page)) {
		t.Fatal("a recording answered before the page's was even sent held it")
	}

	// A hold returns when its call's context ends.
	o.end(before)
	o.end(page)
	page, pulse = o.begin(keys), o.begin(keys)
	o.matched(page, pageRec)
	cctx, cancel := context.WithCancel(ctx)
	cdone := make(chan struct{})
	go func() { o.hold(cctx, page); close(cdone) }()
	cancel()
	if isHeld(cdone) {
		t.Fatal("a hold outlived its call's context")
	}
}
