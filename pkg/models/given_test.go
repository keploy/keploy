package models

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

// A given's name is a short identifier, and its n-th repeat is numbered from
// the second: the recording's n-th pairs with the replay's n-th.
func TestAGivensVariableNamesItsRepeats(t *testing.T) {
	for _, ok := range []string{"a", "appName", "app_name", "app-name", "A9", strings.Repeat("x", MaxGivenName)} {
		require.True(t, ValidGivenName(ok), ok)
	}
	for _, bad := range []string{"", "app.name", "app#2", "app name", "given:x", "given.x", "ü", strings.Repeat("x", MaxGivenName+1)} {
		require.False(t, ValidGivenName(bad), bad)
	}
	require.Equal(t, "given:app", GivenVariable("app", 0))
	require.Equal(t, "given:app", GivenVariable("app", 1))
	require.Equal(t, "given:app#2", GivenVariable("app", 2))
	require.Equal(t, "given:app#13", GivenVariable("app", 13))
	require.True(t, IsGivenVariable("given:app#2"))
	require.False(t, IsGivenVariable("app"))
	require.False(t, IsGivenVariable("givenapp"))
}

// A given's variable name is a key of the mock's variables (MockSpec.Variables),
// and a mock is a document in Keploy Cloud's MongoDB, where a '.' in a key is
// read as a path: no query, update or index by path can name such a key. So
// the name holds no '.', and comes back whole from YAML, JSON and BSON.
func TestAGivenVariableIsAKeyEveryStoreKeeps(t *testing.T) {
	name := GivenVariable("app", 2)
	require.NotContains(t, name, ".")
	in := MockSpec{Variables: MockVariables{name: {Recorded: "app-1", Use: []string{"body.name"}}, "userId": {Recorded: "7", Bind: "body.id"}}}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	j, err := json.Marshal(in)
	require.NoError(t, err)
	b, err := bson.Marshal(in)
	require.NoError(t, err)
	var fromYAML, fromJSON, fromBSON MockSpec
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))
	require.NoError(t, json.Unmarshal(j, &fromJSON))
	require.NoError(t, bson.Unmarshal(b, &fromBSON))
	for _, got := range []MockSpec{fromYAML, fromJSON, fromBSON} {
		require.Equal(t, in.Variables, got.Variables)
	}
}

// A test's givens go out and come back whole and in order, in YAML and JSON,
// and a test that gave none writes no field.
func TestMappedTestCaseGivenRoundTrip(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	given := []GivenValue{
		{Name: "given:app", Value: "[E2E] app-crud-1761609600123456789", Source: "tests/e2e/apps_test.go:20"},
		{Name: "given:app#2", Value: "42"},
	}
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
		{ID: "pkg::TestA", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, Given: given},
		{ID: "pkg::TestB", Mocks: []MockEntry{{Name: "mock-1", Kind: "Http"}}},
	}}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	require.Len(t, regexp.MustCompile(`(?m)^\s*given:$`).FindAllString(string(y), -1), 1, "only a test that gave a value writes the field:\n%s", y)
	var fromYAML Mapping
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))
	j, err := json.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(j), `"given"`))
	var fromJSON Mapping
	require.NoError(t, json.Unmarshal(j, &fromJSON))
	for _, got := range []Mapping{fromYAML, fromJSON} {
		require.Len(t, got.TestCases, 2)
		require.Equal(t, given, got.TestCases[0].Given)
		require.Equal(t, "mock-0", got.TestCases[0].Mocks[0].Name)
		require.Nil(t, got.TestCases[1].Given)
		require.Equal(t, "mock-1", got.TestCases[1].Mocks[0].Name)
	}
}

// Under the switch, a given: that is not a list of givens fails the decode:
// read without it, the replay would bind each given with no recorded value
// and check none.
func TestMappedTestCaseRefusesAGivenThatIsNotAList(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	var tc MappedTestCase
	err := yaml.Unmarshal([]byte("id: t1\ngiven: {name: a}\nmock_entries:\n  - name: mock-0\n"), &tc)
	require.ErrorContains(t, err, "not a list")
	err = json.Unmarshal([]byte(`{"id":"t1","given":"x","mock_entries":[{"name":"mock-0"}]}`), &tc)
	require.ErrorContains(t, err, "not a list")
}

// With the switch off, given: is not read, whatever it holds, so an entry
// keeps its mocks and its shape.
func TestMappedTestCaseWithoutTheSwitchIgnoresGiven(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, doc := range []string{
		"id: t1\ngiven:\n  - name: given:a\n    value: v\nmock_entries:\n  - name: mock-0\n",
		"id: t1\ngiven: {not: a list}\nmock_entries:\n  - name: mock-0\n",
	} {
		var tc MappedTestCase
		require.NoError(t, yaml.Unmarshal([]byte(doc), &tc), doc)
		require.Nil(t, tc.Given)
		require.Equal(t, "mock-0", tc.Mocks[0].Name)
	}
	var tc MappedTestCase
	require.NoError(t, json.Unmarshal([]byte(`{"id":"t1","given":"x","mock_entries":[{"name":"mock-0"}]}`), &tc))
	require.Nil(t, tc.Given)
}
