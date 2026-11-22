package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// Under the switch, the key of the claim each test ran under goes out and
// comes back whole, in YAML and JSON, and a test without one writes no
// field: a reader who moves one of its calls to worker: names the key from
// it (MappedTestCase.Key).
func TestMappedTestCaseKeyRoundTrip(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
		{ID: "a::TestA", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, Key: "a"},
		{ID: "b::TestB", Mocks: []MockEntry{{Name: "mock-1", Kind: "Http"}}, Key: "b"},
		{ID: "TestC"},
	}}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(y), "\n      key:"), "only the tests begun under a claim write the field:\n%s", y)
	var fromYAML Mapping
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))
	j, err := json.Marshal(in)
	require.NoError(t, err)
	var fromJSON Mapping
	require.NoError(t, json.Unmarshal(j, &fromJSON))
	for _, got := range []Mapping{fromYAML, fromJSON} {
		require.Len(t, got.TestCases, 3)
		require.Equal(t, []string{"a", "b", ""}, []string{got.TestCases[0].Key, got.TestCases[1].Key, got.TestCases[2].Key})
		require.Equal(t, "mock-1", got.TestCases[1].Mocks[0].Name)
	}
}

// A key: that is not a string fails the decode, never reads the entry
// without it, and a string one is read, whatever the switch: a writer that
// rewrites the file with the switch off keeps each test's key.
func TestMappedTestCaseKeyIsReadWhateverTheSwitch(t *testing.T) {
	for _, sw := range []string{"1", ""} {
		t.Setenv(provenswitch.Env, sw)
		var bad MappedTestCase
		require.ErrorContains(t, yaml.Unmarshal([]byte("id: t1\nkey: [a]\nmock_entries:\n  - name: mock-0\n"), &bad), "claim key", "switch=%q", sw)
		require.ErrorContains(t, json.Unmarshal([]byte(`{"id":"t1","key":{"a":1},"mock_entries":[{"name":"mock-0"}]}`), &bad), "claim key", "switch=%q", sw)
		var y MappedTestCase
		require.NoError(t, yaml.Unmarshal([]byte("id: t1\nkey: a\nmock_entries:\n  - name: mock-0\n"), &y))
		require.Equal(t, "a", y.Key, "switch=%q", sw)
		require.Equal(t, "mock-0", y.Mocks[0].Name)
		var j MappedTestCase
		require.NoError(t, json.Unmarshal([]byte(`{"id":"t1","key":"a","mock_entries":[{"name":"mock-0"}]}`), &j))
		require.Equal(t, "a", j.Key, "switch=%q", sw)
		require.Equal(t, "mock-0", j.Mocks[0].Name)
	}
}
