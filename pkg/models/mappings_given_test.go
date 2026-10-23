package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// What a test's harness gave goes out and comes back whole, in order, in YAML
// and JSON, and a test that was given none writes no field.
func TestMappedTestCaseGivenRoundTrip(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	given := []GivenValue{{Name: "cookie-jwt_token_staging-1", Value: "tok-a"}, {Name: "cookie-jwt_token_staging-2", Value: "tok-b"}}
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
		{ID: "spec.ts::Review", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, Given: given},
		{ID: "spec.ts::List", Mocks: []MockEntry{{Name: "mock-1", Kind: "Http"}}},
	}}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(y), "given:"), "only the test given values writes the field:\n%s", y)
	var fromYAML Mapping
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))
	j, err := json.Marshal(in)
	require.NoError(t, err)
	var fromJSON Mapping
	require.NoError(t, json.Unmarshal(j, &fromJSON))
	for _, got := range []Mapping{fromYAML, fromJSON} {
		require.Len(t, got.TestCases, 2)
		require.Equal(t, given, got.TestCases[0].Given)
		require.Equal(t, "mock-0", got.TestCases[0].Mocks[0].Name)
		require.Nil(t, got.TestCases[1].Given)
	}
}

// Under the switch a given: that is not a list of named values fails the
// decode; with the switch off it is not read, and the entry keeps its mocks.
func TestMappedTestCaseGivenIsReadOnlyUnderTheSwitch(t *testing.T) {
	doc := "id: t1\ngiven: tok\nmock_entries:\n  - name: mock-0\n"
	t.Setenv(provenswitch.Env, "1")
	var tc MappedTestCase
	require.ErrorContains(t, yaml.Unmarshal([]byte(doc), &tc), "given")
	t.Setenv(provenswitch.Env, "")
	tc = MappedTestCase{}
	require.NoError(t, yaml.Unmarshal([]byte(doc), &tc))
	require.Nil(t, tc.Given)
	require.Equal(t, "mock-0", tc.Mocks[0].Name)
}

// What claim keys' harnesses gave outside every test goes out and comes back
// whole, in YAML and JSON, under key_given / keyGiven.
func TestMappingKeyGivenRoundTrip(t *testing.T) {
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set",
		KeyGiven: []KeyGiven{{Key: "spec.ts > group", Given: []GivenValue{{Name: "outside-cookie-a-1", Value: "tok"}}}}}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	require.Contains(t, string(y), "key_given:")
	var fromYAML Mapping
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))
	j, err := json.Marshal(in)
	require.NoError(t, err)
	var fromJSON Mapping
	require.NoError(t, json.Unmarshal(j, &fromJSON))
	require.Equal(t, in.KeyGiven, fromYAML.KeyGiven)
	require.Equal(t, in.KeyGiven, fromJSON.KeyGiven)
}
