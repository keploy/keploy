package mocknoise

import (
	"maps"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Every text of a request and of a response an id can sit in is visited: a
// carrier left out is one a followed id is neither found in nor replaced in.
func TestEachTextVisitsEveryCarrier(t *testing.T) {
	req := &models.HTTPReq{
		URL:       "http://app/orders/in-url",
		Body:      "in-body",
		Header:    map[string]string{"X-Order": "in-header"},
		URLParams: map[string]string{"order": "in-query"},
		Form:      []models.FormData{{Key: "order", Values: []string{"in-form-1", "in-form-2"}}},
	}
	var got []string
	EachRequestText(req, func(s string) { got = append(got, s) })
	slices.Sort(got)
	want := []string{"http://app/orders/in-url", "in-body", "in-form-1", "in-form-2", "in-header", "in-query"}
	if !slices.Equal(got, want) {
		t.Fatalf("request texts visited: %v, want %v", got, want)
	}

	resp := &models.HTTPResp{Body: "in-body", Header: map[string]string{"Location": "in-header"}}
	got = nil
	EachResponseText(resp, func(s string) { got = append(got, s) })
	slices.Sort(got)
	if want := []string{"in-body", "in-header"}; !slices.Equal(got, want) {
		t.Fatalf("response texts visited: %v, want %v", got, want)
	}
}

// A response's ids are replaced in its body and its header values, as whole
// words, on a response of its own: the one it was made from — a recorded test
// case's, a report's — keeps its header map as it was.
func TestReplaceResponseWords(t *testing.T) {
	const rec, live = "0f8fad5b-d9cb-469f-a165-70867728950e", "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	lookup := func(w string) (string, bool) {
		if w == rec {
			return live, true
		}
		return "", false
	}
	header := map[string]string{"Location": "/orders/" + rec, "Content-Type": "application/json"}
	kept := maps.Clone(header)
	in := models.HTTPResp{StatusCode: 201, Body: `{"id":"` + rec + `","ref":"order-` + rec + `x"}`, Header: header}

	out, changed := ReplaceResponseWords(in, lookup)
	if !changed {
		t.Fatal("a response that names the id must be reported as changed")
	}
	if want := `{"id":"` + live + `","ref":"order-` + rec + `x"}`; out.Body != want {
		t.Fatalf("body = %s, want %s: the id is replaced where it is a whole word, and only there", out.Body, want)
	}
	if out.Header["Location"] != "/orders/"+live || out.Header["Content-Type"] != "application/json" {
		t.Fatalf("header = %v", out.Header)
	}
	if !maps.Equal(header, kept) {
		t.Fatalf("the header map the response came with was written: %v", header)
	}
	if out.StatusCode != 201 {
		t.Fatal("the rest of the response is kept")
	}

	// Only a header names the id: still a change.
	onlyHeader := models.HTTPResp{Body: "{}", Header: map[string]string{"Location": "/orders/" + rec}}
	if _, changed := ReplaceResponseWords(onlyHeader, lookup); !changed {
		t.Fatal("an id in a header alone is a change")
	}
	// Nothing names it: reported unchanged, with or without headers.
	for _, same := range []models.HTTPResp{{Body: `{"ok":true}`}, {Body: `{"ok":true}`, Header: map[string]string{"A": "b"}}} {
		if got, changed := ReplaceResponseWords(same, lookup); changed || got.Body != same.Body || !maps.Equal(got.Header, same.Header) {
			t.Fatalf("a response that names no id must come back as it was: %v", got)
		}
	}
}
