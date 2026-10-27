package models

import "testing"

// A stateful sequence's identity (StatefulSequenceKey) is the request and its
// lifetime class, for a cursor recording only, and looking at an underived
// recording neither derives it nor counts it as a pre-tag mock a replay
// loaded.
func TestStatefulSequenceKey(t *testing.T) {
	call := func(typ, body string) *Mock {
		md := map[string]string{}
		if typ != "" {
			md["type"] = typ
		}
		return &Mock{Kind: HTTP, Spec: MockSpec{Metadata: md, HTTPReq: &HTTPReq{Method: "GET", URL: "http://api/counter", Body: body}}}
	}
	fires := LegacyKindFallbackFires()
	untagged := call("", "")
	key, ok := StatefulSequenceKey(untagged)
	if !ok || IsPerTestSequenceKey(key) {
		t.Fatalf("an untagged recording: key %q ok %v; want a reusable sequence", key, ok)
	}
	if LegacyKindFallbackFires() != fires || untagged.TestModeInfo != (TestModeInfo{}) {
		t.Fatalf("StatefulSequenceKey counted a kind fallback (%d -> %d) or changed the mock (%+v)", fires, LegacyKindFallbackFires(), untagged.TestModeInfo)
	}
	// The format is pinned: it must stay apart from the cursor store's other
	// keys (a served mark is "served\x00" and a name), and code that builds a
	// sequence's key on its own (the mock prune) must agree with it.
	const golden = "GET http://api/counter\x00e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if HTTPSequenceKey("GET", "http://api/counter", "") != golden || key != golden {
		t.Fatalf("key %q, HTTPSequenceKey %q; want %q", key, HTTPSequenceKey("GET", "http://api/counter", ""), golden)
	}
	if again, _ := StatefulSequenceKey(call("", "")); again != key {
		t.Fatalf("two recordings of one request have keys %q and %q", key, again)
	}
	if other, ok := StatefulSequenceKey(call("", "x")); !ok || other == key {
		t.Fatalf("a recording with another body: key %q ok %v; want a sequence of its own", other, ok)
	}
	if _, ok := StatefulSequenceKey(call("config", "")); ok {
		t.Fatal("a config-tagged recording is no cursor recording")
	}
	derivedPerTest := call("", "")
	derivedPerTest.TestModeInfo = TestModeInfo{Lifetime: LifetimePerTest, Consume: ConsumeCursorSaturate, LifetimeDerived: true}
	if pt, ok := StatefulSequenceKey(derivedPerTest); !ok || !IsPerTestSequenceKey(pt) || pt == key {
		t.Fatalf("a per-test cursor recording: key %q ok %v; want a per-test sequence apart from the reusable one", pt, ok)
	}
	if _, ok := StatefulSequenceKey(&Mock{Kind: MySQL, Spec: MockSpec{Metadata: map[string]string{"connID": "c1"}}}); ok {
		t.Fatal("a MySQL mock belongs to no HTTP sequence")
	}
}
