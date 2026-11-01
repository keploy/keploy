package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.uber.org/zap"
)

// A `repeat:` key a person writes on a test's entry in mappings.yaml is no
// keploy field: the mappings file still reads, the key is dropped, and the
// recording answers its test once. The test's second identical call misses
// as one call more than recorded, exactly as without the key, so replay
// stays exact (it fails closed) and no hand-written key tolerates a count
// the recording does not hold.
func TestAHandWrittenRepeatKeyLeavesServingExact(t *testing.T) {
	dir := t.TempDir()
	set := filepath.Join(dir, "test-set-0")
	require.NoError(t, os.MkdirAll(set, 0o755))
	doc := `version: api.keploy.io/v1beta1
kind: TestMocksMapping
test_set_id: test-set-0
tests:
    - id: TestCluster
      mock_entries:
        - name: mock-1
          kind: Http
          repeat: any
`
	require.NoError(t, os.WriteFile(filepath.Join(set, "mappings.yaml"), []byte(doc), 0o644))
	byTest, present, err := mapdb.New(zap.NewNop(), dir, "mappings").Get(context.Background(), "test-set-0")
	require.NoError(t, err, "a mappings file with a hand-written repeat: key must still read")
	require.True(t, present)
	require.Equal(t, []models.MockEntry{{Name: "mock-1", Kind: "Http"}}, byTest["TestCluster"], "the entry kept something of the repeat: key")

	table := map[string][]string{}
	for id, entries := range byTest {
		for _, e := range entries {
			table["o::"+id] = append(table["o::"+id], e.Name)
		}
	}
	a, p := exactAgent(t, []*models.Mock{pingMock("mock-1", "/cluster", "c1", t0)}, table, map[string][]string{})
	got, e := runTest(t, a, p, "TestCluster", "/cluster", "/cluster")
	require.Equal(t, "200 c1", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "the hand-written repeat: key answered the second read: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /cluster to 127.0.0.1:18093 was called 2 times, recorded 1")
}
