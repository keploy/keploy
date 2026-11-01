package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// A test entry's `repeat: any` goes out and comes back whole, in YAML and
// JSON, on the one entry that carries it, so each rewrite of mappings.yaml
// keeps the mark its reader wrote; RepeatMarks reads it by test.
func TestARepeatMarkRoundTripsOnTheEntryThatCarriesIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
		{ID: "a::TestA", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http", Repeat: MockEntryRepeatAny}, {Name: "mock-1", Kind: "Http"}}},
		{ID: "b::TestB", Mocks: []MockEntry{{Name: "mock-2", Kind: "Http"}}},
	}}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(y), "repeat: any"), "only the marked entry writes the field:\n%s", y)
	var fromYAML Mapping
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))
	j, err := json.Marshal(in)
	require.NoError(t, err)
	var fromJSON Mapping
	require.NoError(t, json.Unmarshal(j, &fromJSON))
	for _, got := range []Mapping{fromYAML, fromJSON} {
		tests := map[string][]MockEntry{}
		for _, tc := range got.TestCases {
			tests[tc.ID] = tc.Mocks
		}
		require.Equal(t, map[string][]string{"a::TestA": {"mock-0"}}, RepeatMarks(tests))
	}
	require.Nil(t, RepeatMarks(map[string][]MockEntry{"b::TestB": {{Name: "mock-2"}}}), "no mark, no table")
}

// Under the switch a repeat mark keploy does not know is refused, naming the
// test, the recording and the two legal spellings: serving it once or at
// any count would each guess what its writer meant. With the switch off the
// entry reads as before.
func TestAnUnknownRepeatMarkIsRefusedUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, doc := range []string{
		"id: t1\nmock_entries:\n  - name: mock-0\n    repeat: twice\n",
		"id: t1\nmock_entries:\n  - name: mock-0\n    repeat: \"true\"\n",
	} {
		var tc MappedTestCase
		err := yaml.Unmarshal([]byte(doc), &tc)
		var rm *RepeatMarkError
		require.ErrorAs(t, err, &rm, doc)
		require.Equal(t, "t1", rm.Test)
		require.Equal(t, "mock-0", rm.Mock)
		require.Contains(t, err.Error(), "`repeat: any`")
	}
	var j MappedTestCase
	require.ErrorContains(t, json.Unmarshal([]byte(`{"id":"t1","mock_entries":[{"name":"mock-0","repeat":"2"}]}`), &j), "keploy knows only `repeat: any`")
	var ok MappedTestCase
	require.NoError(t, yaml.Unmarshal([]byte("id: t1\nmock_entries:\n  - name: mock-0\n    repeat: any\n"), &ok))
	require.Equal(t, MockEntryRepeatAny, ok.Mocks[0].Repeat)

	t.Setenv(provenswitch.Env, "")
	var off MappedTestCase
	require.NoError(t, yaml.Unmarshal([]byte("id: t1\nmock_entries:\n  - name: mock-0\n    repeat: twice\n"), &off))
	require.Equal(t, "mock-0", off.Mocks[0].Name)
}

// The mark serves a read only: an HTTP GET, HEAD or OPTIONS, whatever its
// case. An HTTP write and every call of another protocol is none, and a
// mark a person put on one refuses the test (RepeatOnWrite), naming the
// test, the recording and the call.
func TestOnlyAnHTTPReadMayCarryTheRepeatMark(t *testing.T) {
	call := func(kind Kind, method string) *Mock {
		m := &Mock{Name: "mock-0", Kind: kind}
		if kind == HTTP {
			m.Spec.HTTPReq = &HTTPReq{Method: Method(method), URL: "http://api/x"}
		}
		return m
	}
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "get"} {
		require.True(t, RepeatableRead(call(HTTP, method)), method)
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", ""} {
		require.False(t, RepeatableRead(call(HTTP, method)), method)
	}
	for _, kind := range []Kind{GENERIC, MySQL, Postgres, GRPC_EXPORT, REDIS} {
		require.False(t, RepeatableRead(call(kind, "")), kind)
	}
	require.False(t, RepeatableRead(nil))
	require.False(t, RepeatableRead(&Mock{Kind: HTTP}), "an HTTP mock with no request")

	get, post := call(HTTP, "GET"), call(HTTP, "POST")
	get.Name, post.Name = "mock-1", "mock-2"
	require.NoError(t, RepeatOnWrite("t1", []*Mock{get, post}, []string{"mock-1"}))
	require.NoError(t, RepeatOnWrite("t1", []*Mock{get}, []string{"mock-9"}), "a name the test does not hold serves nothing")
	err := RepeatOnWrite("t1", []*Mock{get, post}, []string{"mock-1", "mock-2"})
	var w *RepeatOnWriteError
	require.ErrorAs(t, err, &w)
	require.Equal(t, RepeatOnWriteError{Test: "t1", Mock: "mock-2", Call: "POST http://api/x"}, *w)
	require.Contains(t, err.Error(), "only on a read")
	g := call(GENERIC, "")
	require.ErrorContains(t, RepeatOnWrite("t1", []*Mock{g}, []string{"mock-0"}), `"mock-0" (Generic)`)
}

// Under the switch a repeat mark on a set-wide list (startup:, runner:,
// worker:, shared:) is refused, in YAML and JSON, naming the list and the
// recording: each of those is served at most once, so the mark would be
// dropped unseen. With the switch off the file reads as before.
func TestARepeatMarkOutsideATestsListIsRefusedUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, list := range []string{"startup", "runner", "worker", "shared"} {
		doc := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\n" + list + ":\n  - name: mock-3\n    repeat: any\n"
		var m Mapping
		err := yaml.Unmarshal([]byte(doc), &m)
		var r *RepeatOutsideTestError
		require.ErrorAs(t, err, &r, list)
		require.Equal(t, RepeatOutsideTestError{List: list, Mock: "mock-3", Value: "any"}, *r)
		require.Contains(t, err.Error(), list+": entry \"mock-3\"")
		var j Mapping
		require.ErrorAs(t, json.Unmarshal([]byte(`{"`+list+`":[{"name":"mock-3","repeat":"any"}]}`), &j), &r, list)
	}
	ok := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n  - id: t1\n    mock_entries:\n      - name: mock-0\n        repeat: any\nworker:\n  - name: mock-3\n"
	var m Mapping
	require.NoError(t, yaml.Unmarshal([]byte(ok), &m), "a mark on a test's own list reads")
	require.Equal(t, MockEntryRepeatAny, m.TestCases[0].Mocks[0].Repeat)
	require.Equal(t, "mock-3", m.Worker[0].Name)
	require.Equal(t, "s", m.TestSetID)

	t.Setenv(provenswitch.Env, "")
	var off Mapping
	require.NoError(t, yaml.Unmarshal([]byte("worker:\n  - name: mock-3\n    repeat: any\n"), &off))
	require.Equal(t, "mock-3", off.Worker[0].Name)
}
