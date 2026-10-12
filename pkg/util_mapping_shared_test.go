package pkg

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mockAt builds a minimal per-test HTTP mock with a request timestamp, which is
// the key FilterTcsMocksMapping* sorts each tier by.
func mockAt(name string, tsSec int) *models.Mock {
	return &models.Mock{
		Name:    name,
		Kind:    models.HTTP,
		Version: "api.keploy.io/v1beta1",
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			ReqTimestampMock: time.Unix(int64(tsSec), 0),
		},
	}
}

func names(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// The pool a scoped test sees: its own mapped mocks FIRST, then the mocks that
// belong to no test as overflow. Order is the contract — the matcher takes the
// first match in slice order, so a shared recording of the same URL must never
// pre-empt the test's own.
func TestFilterTcsMocksMappingWithShared_MappedFirstThenShared(t *testing.T) {
	logger := zap.NewNop()
	// mock-0 is recorded first (a beforeAll) and belongs to no test.
	all := []*models.Mock{
		mockAt("mock-0", 10),
		mockAt("mock-1", 20),
		mockAt("mock-2", 30),
		mockAt("mock-3", 40),
	}
	mine := []string{"mock-1", "mock-2"}               // this test's mapping
	universe := []string{"mock-1", "mock-2", "mock-3"} // every test's mapping, unioned

	got := FilterTcsMocksMappingWithShared(context.Background(), logger, all, mine, universe)

	// mock-0 is present but LAST, despite being the earliest by timestamp.
	// mock-3 is another test's and must not appear at all.
	require.Equal(t, []string{"mock-1", "mock-2", "mock-0"}, names(got))
}

// A nil/empty universe must reproduce the pre-existing mapped-only behaviour
// byte for byte, so the `keploy test` replay path (which sets UseMappingBased
// but never sets a universe) is untouched.
func TestFilterTcsMocksMappingWithShared_NilUniverseIsIdentityToOldPath(t *testing.T) {
	logger := zap.NewNop()
	all := []*models.Mock{mockAt("mock-0", 10), mockAt("mock-1", 20), mockAt("mock-2", 30)}
	mine := []string{"mock-1"}

	old := FilterTcsMocksMapping(context.Background(), logger, all, mine)
	require.Equal(t, names(old), names(FilterTcsMocksMappingWithShared(context.Background(), logger, all, mine, nil)))
	require.Equal(t, names(old), names(FilterTcsMocksMappingWithShared(context.Background(), logger, all, mine, []string{})))
}

// Within each tier, recorded order is preserved — the ordered-tape guarantee
// (N identical requests, N different responses) must survive the two-tier split.
func TestFilterTcsMocksMappingWithShared_PreservesTapeOrderWithinTiers(t *testing.T) {
	logger := zap.NewNop()
	all := []*models.Mock{
		mockAt("mock-3", 40), // deliberately out of timestamp order in the input
		mockAt("mock-1", 20),
		mockAt("mock-4", 50),
		mockAt("mock-0", 10),
		mockAt("mock-2", 30),
	}
	mine := []string{"mock-1", "mock-2", "mock-3"}
	universe := []string{"mock-1", "mock-2", "mock-3"}

	got := FilterTcsMocksMappingWithShared(context.Background(), logger, all, mine, universe)
	require.Equal(t, []string{"mock-1", "mock-2", "mock-3", "mock-0", "mock-4"}, names(got))
}

// Every mock mapped somewhere ⇒ no overflow tier, and the result is exactly the
// test's own mapping.
func TestFilterTcsMocksMappingWithShared_NoUnmappedMocksMeansNoOverflow(t *testing.T) {
	logger := zap.NewNop()
	all := []*models.Mock{mockAt("mock-0", 10), mockAt("mock-1", 20)}
	mine := []string{"mock-0"}
	universe := []string{"mock-0", "mock-1"}

	got := FilterTcsMocksMappingWithShared(context.Background(), logger, all, mine, universe)
	require.Equal(t, []string{"mock-0"}, names(got))
}
