package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// Under the switch, the seed each test's browser drew from goes out and comes
// back whole, in YAML and JSON, one of digits only included, and a test
// without one writes no field: its replay's begin is answered it
// (MappedTestCase.Seed).
func TestMappedTestCaseSeedRoundTrip(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	in := Mapping{Version: "api.keploy.io/v1beta1", Kind: MappingKind, TestSetID: "set", TestCases: []MappedTestCase{
		{ID: "ui::tr", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, Seed: "0123456789abcdef0123456789abcdef"},
		{ID: "ui::history", Mocks: []MockEntry{{Name: "mock-1", Kind: "Http"}}, Seed: "01234567890123456789012345678901"},
		{ID: "ui::old"},
	}}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(y), "\n      seed:"), "only the tests with a seed write the field:\n%s", y)
	var fromYAML Mapping
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))
	j, err := json.Marshal(in)
	require.NoError(t, err)
	var fromJSON Mapping
	require.NoError(t, json.Unmarshal(j, &fromJSON))
	for _, got := range []Mapping{fromYAML, fromJSON} {
		require.Len(t, got.TestCases, 3)
		require.Equal(t, []string{in.TestCases[0].Seed, in.TestCases[1].Seed, ""}, []string{got.TestCases[0].Seed, got.TestCases[1].Seed, got.TestCases[2].Seed})
		require.Equal(t, "mock-1", got.TestCases[1].Mocks[0].Name)
	}
}

// A test's seed: is read whatever the switch, as its key: (mapdb keeps
// every field keploy writes on a test entry whatever the switch; a replay
// answers a begin with it under the switch only). One that is not a string
// fails the decode either way, never reads the entry without it (its replay
// would draw other numbers than its recording).
func TestMappedTestCaseSeedIsReadWhateverTheSwitch(t *testing.T) {
	for _, sw := range []string{"1", ""} {
		t.Setenv(provenswitch.Env, sw)
		var bad MappedTestCase
		require.ErrorContains(t, yaml.Unmarshal([]byte("id: t1\nseed: [a]\nmock_entries:\n  - name: mock-0\n"), &bad), "browser seed", "switch=%q", sw)
		require.ErrorContains(t, json.Unmarshal([]byte(`{"id":"t1","seed":{"a":1},"mock_entries":[{"name":"mock-0"}]}`), &bad), "browser seed", "switch=%q", sw)
		var y MappedTestCase
		require.NoError(t, yaml.Unmarshal([]byte("id: t1\nseed: 0123456789abcdef\nmock_entries:\n  - name: mock-0\n"), &y))
		require.Equal(t, "0123456789abcdef", y.Seed, "switch=%q", sw)
		require.Equal(t, "mock-0", y.Mocks[0].Name)
		var j MappedTestCase
		require.NoError(t, json.Unmarshal([]byte(`{"id":"t1","seed":"0123456789abcdef","mock_entries":[{"name":"mock-0"}]}`), &j))
		require.Equal(t, "0123456789abcdef", j.Seed, "switch=%q", sw)
		require.Equal(t, "mock-0", j.Mocks[0].Name)
	}
}

// A begin's seed is kept in lower case when it is 8 to 64 hex digits, and
// is none otherwise: keploy writes back only a seed it can write as it came.
func TestNormalizeScopeSeed(t *testing.T) {
	for in, want := range map[string]string{
		"0123456789ABCDEF":      "0123456789abcdef",
		" 01234567 ":            "01234567",
		"0123456":               "",
		strings.Repeat("f", 64): strings.Repeat("f", 64),
		strings.Repeat("f", 65): "",
		"0123456789abcdeg":      "",
		"01234567-89abcdef":     "",
		"":                      "",
	} {
		require.Equal(t, want, NormalizeScopeSeed(in), in)
	}
}

// A test entry's variables (MappedTestCase.Variables) are written and read
// back in both formats, whatever the switch, bound as CaseBind says; an
// entry whose variables do not read is an error. ParseCaseBind reads a
// CaseBind back, and nothing else.
func TestATestsCaseVariablesAreReadBack(t *testing.T) {
	vars := MockVariables{"name": {Recorded: "app-1", Bind: CaseBind("post-apps-1", "body.name")}}
	tc := MappedTestCase{ID: "t1", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}, Variables: vars}
	y, err := yaml.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML MappedTestCase
	if err := yaml.Unmarshal(y, &fromYAML); err != nil || !reflect.DeepEqual(fromYAML.Variables, vars) {
		t.Fatalf("YAML %s read back %+v (%v); want %v", y, fromYAML.Variables, err, vars)
	}
	j, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON MappedTestCase
	if err := json.Unmarshal(j, &fromJSON); err != nil || !reflect.DeepEqual(fromJSON.Variables, vars) {
		t.Fatalf("JSON %s read back %+v (%v); want %v", j, fromJSON.Variables, err, vars)
	}
	var bad MappedTestCase
	if err := yaml.Unmarshal([]byte("id: t1\nmock_entries: []\nvariables: [a, b]\n"), &bad); err == nil {
		t.Fatal("variables that are not a map were read")
	}
	if err := json.Unmarshal([]byte(`{"id":"t1","mock_entries":[],"variables":[1]}`), &bad); err == nil {
		t.Fatal("JSON variables that are not a map were read")
	}
	if c, p, ok := ParseCaseBind(vars["name"].Bind); !ok || c != "post-apps-1" || p != "body.name" {
		t.Fatalf("ParseCaseBind(%q) = %q, %q, %v", vars["name"].Bind, c, p, ok)
	}
	for _, b := range []string{"body.name", "test case , request body.name", "test case post-apps-1, request ", "test case post-apps-1 body.name", "mock-1, request body.name"} {
		if _, _, ok := ParseCaseBind(b); ok {
			t.Errorf("ParseCaseBind(%q) read", b)
		}
	}
}
