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
