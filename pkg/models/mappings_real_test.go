package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// A test's mock.real destinations go out and come back whole, in YAML and
// JSON, and a test without any writes no field.
func TestMappedTestCaseRealRoundTrip(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
		{ID: "pkg::TestA", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, Dir: "pkg", Real: []string{"auth.example.com:443", "localhost:5432"}},
		{ID: "pkg::TestB", Dir: "pkg"},
		{ID: "pkg::TestC", Real: []string{"container:redis"}},
	}}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(y), "real:"), "only the tests that used one write the field:\n%s", y)
	var fromYAML Mapping
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))
	j, err := json.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(j), `"real"`))
	var fromJSON Mapping
	require.NoError(t, json.Unmarshal(j, &fromJSON))
	for _, got := range []Mapping{fromYAML, fromJSON} {
		require.Len(t, got.TestCases, 3)
		require.Equal(t, []string{"auth.example.com:443", "localhost:5432"}, got.TestCases[0].Real)
		require.Equal(t, []string{"mock-0"}, []string{got.TestCases[0].Mocks[0].Name})
		require.Nil(t, got.TestCases[1].Real)
		require.Equal(t, []string{"container:redis"}, got.TestCases[2].Real)
		require.Equal(t, "pkg::TestC", got.TestCases[2].ID)
	}
}

// Under the switch, a real: that is not a list of destinations fails the
// decode: read without it, the test would replay frozen and its verdict would
// not ask CI to reach them.
func TestMappedTestCaseRefusesARealThatIsNotAList(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	var tc MappedTestCase
	err := yaml.Unmarshal([]byte("id: t1\nreal: {a: b}\nmock_entries:\n  - name: mock-0\n"), &tc)
	require.ErrorContains(t, err, "not a list")
	err = json.Unmarshal([]byte(`{"id":"t1","real":"db:5432","mock_entries":[{"name":"mock-0"}]}`), &tc)
	require.ErrorContains(t, err, "not a list")
}

// With the switch off, real: is not read, whatever it holds, so an entry keeps
// its mocks and its shape.
func TestMappedTestCaseWithoutTheSwitchIgnoresReal(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	for _, doc := range []string{
		"id: t1\nreal: [db:5432]\nmock_entries:\n  - name: mock-0\n",
		"id: t1\nreal: {not: a list}\nmock_entries:\n  - name: mock-0\n",
	} {
		var tc MappedTestCase
		require.NoError(t, yaml.Unmarshal([]byte(doc), &tc), doc)
		require.Nil(t, tc.Real)
		require.Equal(t, "mock-0", tc.Mocks[0].Name)
	}
	var tc MappedTestCase
	require.NoError(t, json.Unmarshal([]byte(`{"id":"t1","real":"x","mock_entries":[{"name":"mock-0"}]}`), &tc))
	require.Nil(t, tc.Real)
}
