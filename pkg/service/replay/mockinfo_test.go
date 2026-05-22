package replay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/pkg/models"
)

func TestBuildExpectedMockInfos_FiltersDNSByEntryKind(t *testing.T) {
	entries := []models.MockEntry{
		{Name: "mock-1", Kind: "Http"},
		{Name: "mock-2", Kind: "DNS"},
		{Name: "mock-3", Kind: "PostgresV3"},
	}

	out := buildExpectedMockInfos(entries, nil)

	assert.Equal(t, []models.MockMismatchMock{
		{Name: "mock-1", Kind: "Http"},
		{Name: "mock-3", Kind: "PostgresV3"},
	}, out)
}

func TestBuildExpectedMockInfos_FiltersDNSByLookup(t *testing.T) {
	// Entry has no Kind on it; mockKindByName says it's DNS — must still be
	// filtered. Mirrors what record-time loses when a recorded mock entry
	// lacks Kind metadata but the kind is recoverable from the mock pool.
	entries := []models.MockEntry{
		{Name: "mock-1", Kind: ""},
		{Name: "mock-dns", Kind: ""},
	}
	kindByName := map[string]models.Kind{
		"mock-1":   models.Kind("Http"),
		"mock-dns": models.DNS,
	}

	out := buildExpectedMockInfos(entries, kindByName)

	assert.Equal(t, []models.MockMismatchMock{
		{Name: "mock-1", Kind: "Http"},
	}, out)
}

func TestBuildExpectedMockInfos_ResolvesEmptyKindFromLookup(t *testing.T) {
	entries := []models.MockEntry{{Name: "mock-1", Kind: ""}}
	kindByName := map[string]models.Kind{
		"mock-1": models.Kind("Http"),
	}

	out := buildExpectedMockInfos(entries, kindByName)

	assert.Equal(t, []models.MockMismatchMock{{Name: "mock-1", Kind: "Http"}}, out)
}

func TestBuildExpectedMockInfos_EmptyInput(t *testing.T) {
	out := buildExpectedMockInfos(nil, nil)
	assert.Empty(t, out)
	assert.NotNil(t, out, "should return non-nil empty slice")
}

func TestBuildActualMockInfos_FiltersDNS(t *testing.T) {
	consumed := []models.MockState{
		{Name: "mock-1", Kind: models.Kind("Http")},
		{Name: "mock-2", Kind: models.DNS},
		{Name: "mock-3", Kind: models.Kind("PostgresV3")},
	}

	out := buildActualMockInfos(consumed, true)

	assert.Equal(t, []models.MockMismatchMock{
		{Name: "mock-1", Kind: "Http"},
		{Name: "mock-3", Kind: "PostgresV3"},
	}, out)
}

func TestBuildActualMockInfos_UnknownReturnsEmpty(t *testing.T) {
	// When per-test consumed data is unknown (e.g. GetConsumedMocks failed)
	// the helper must return an empty slice — even though consumed[] looks
	// populated, that data is from a different test and would be stale.
	consumed := []models.MockState{
		{Name: "stale-mock", Kind: models.Kind("Http")},
	}

	out := buildActualMockInfos(consumed, false)

	assert.Empty(t, out)
}

func TestBuildActualMockInfos_EmptyInput(t *testing.T) {
	out := buildActualMockInfos(nil, true)
	assert.Empty(t, out)
	assert.NotNil(t, out, "should return non-nil empty slice")
}
