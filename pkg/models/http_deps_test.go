package models

import (
	"reflect"
	"testing"
)

func httpRecording(method, url, body string, metadata map[string]string) *Mock {
	return &Mock{Kind: HTTP, Spec: MockSpec{
		Metadata: metadata,
		HTTPReq:  &HTTPReq{Method: Method(method), URL: url, Body: body},
		HTTPResp: &HTTPResp{StatusCode: 200},
	}}
}

// The recordings of one reusable stateful HTTP sequence (the HTTP cursor's
// grouping, StatefulSequenceKey) provide and require it, so whatever keeps
// one keeps all (decision 27); every other HTTP mock has no links.
func TestHTTPMockDeps_LinkTheRecordingsOfAReusableStatefulSequence(t *testing.T) {
	next := httpRecording("GET", "http://counter/next", "", nil) // a recording made before lifetimes were stated: reusable
	key, ok := StatefulSequenceKey(next)
	if !ok || IsPerTestSequenceKey(key) {
		t.Fatalf("precondition: a reusable stateful recording, got key %q ok %v", key, ok)
	}
	want := MockDeps{Provides: []string{HTTPStatefulSequenceKey(key)}, Requires: []MockRequirement{{Key: HTTPStatefulSequenceKey(key), Standalone: true}}}
	if got := DepsOf(next); !reflect.DeepEqual(got, want) {
		t.Fatalf("DepsOf = %+v, want %+v", got, want)
	}
	again := httpRecording("GET", "http://counter/next", "", nil)
	if !reflect.DeepEqual(DepsOf(again), want) {
		t.Errorf("another recording of the same request is not of the same sequence: %+v", DepsOf(again))
	}
	if other := DepsOf(httpRecording("POST", "http://counter/next", `{"a":1}`, nil)); reflect.DeepEqual(other, want) {
		t.Errorf("another request is of the same sequence: %+v", other)
	}

	for name, m := range map[string]*Mock{
		"stated per-test (its cursor starts over for each test)": httpRecording("GET", "http://counter/next", "", map[string]string{MetadataLifetimeKey: MetadataLifetimePerTest}),
		"an async lane's (served by the async engine)": func() *Mock {
			m := httpRecording("GET", "http://counter/next", "", nil)
			m.Spec.Async = &AsyncMeta{Lane: "cfg", Seq: 1}
			return m
		}(),
		"no request": {Kind: HTTP},
	} {
		if got := DepsOf(m); len(got.Provides)+len(got.Requires) != 0 {
			t.Errorf("%s: DepsOf = %+v, want none", name, got)
		}
	}
	// A recording stated per-test that a declared session call covers
	// (keploy.yml sessionCalls) is replayed as a session's: reusable, one
	// sequence across tests.
	SetSessionCalls([]SessionCallRule{{Host: "counter"}})
	t.Cleanup(func() { SetSessionCalls(nil) })
	if got := DepsOf(httpRecording("GET", "http://counter/next", "", map[string]string{MetadataLifetimeKey: MetadataLifetimePerTest})); !reflect.DeepEqual(got, want) {
		t.Errorf("a declared session call's recordings are not one reusable sequence: %+v", got)
	}

	if m := next; m.TestModeInfo.LifetimeDerived {
		t.Error("reading a mock's links derived its lifetime on it: they are read without a side effect")
	}
}
