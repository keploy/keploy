package mock

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// Each test's recording keeps what its harness gave in its window; a test run
// twice in the record (a retry) keeps its last run's, as its mocks are.
func TestStampGivenWritesEachTestsGivenValues(t *testing.T) {
	byCase := map[string]models.MappedTestCase{"spec.ts::Review": {Dir: "tests"}}
	stampGiven(byCase, []models.ScopeWindow{
		{Name: "spec.ts::Review", Given: []models.GivenValue{{Name: "cookie-jwt_token_staging-1", Value: "tok-first-run"}}},
		{Name: "spec.ts::Review", Given: []models.GivenValue{{Name: "cookie-jwt_token_staging-1", Value: "tok-retry"}}},
		{Name: "spec.ts::List"},
		{Claim: "spec.ts"},
	})
	require.Equal(t, models.MappedTestCase{Dir: "tests", Given: []models.GivenValue{{Name: "cookie-jwt_token_staging-1", Value: "tok-retry"}}}, byCase["spec.ts::Review"])
	_, ok := byCase["spec.ts::List"]
	require.False(t, ok, "a test given nothing gets no entry from it")
}

// Replay reads, by test, what each test's harness gave in its recording.
func TestTestGivenReadsTheRecordingsGivenValues(t *testing.T) {
	require.Nil(t, testGiven(nil))
	got := testGiven(&models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "spec.ts::Review", Given: []models.GivenValue{{Name: "cookie-a-1", Value: "x"}}},
		{ID: "spec.ts::List"},
	}})
	require.Equal(t, map[string][]models.GivenValue{"spec.ts::Review": {{Name: "cookie-a-1", Value: "x"}}}, got)
	require.Nil(t, testGiven(&models.Mapping{TestCases: []models.MappedTestCase{{ID: "spec.ts::List"}}}))
}

// Each claim key keeps what its harness gave outside every test, the key's
// last claim's that gave any.
func TestKeyGivenKeepsEachKeysLastClaim(t *testing.T) {
	got := keyGiven([]models.ScopeWindow{
		{Claim: "spec.ts > group", Given: []models.GivenValue{{Name: "outside-cookie-a-1", Value: "first"}}},
		{Claim: "spec.ts"},
		{Claim: "spec.ts > group", Given: []models.GivenValue{{Name: "outside-cookie-a-1", Value: "again"}}},
		{Claim: "other.spec.ts", Given: []models.GivenValue{{Name: "outside-cookie-b-1", Value: "x"}}},
	})
	require.Equal(t, []models.KeyGiven{
		{Key: "spec.ts > group", Given: []models.GivenValue{{Name: "outside-cookie-a-1", Value: "again"}}},
		{Key: "other.spec.ts", Given: []models.GivenValue{{Name: "outside-cookie-b-1", Value: "x"}}},
	}, got)
	require.Nil(t, keyGiven([]models.ScopeWindow{{Claim: "spec.ts"}}))
	require.Equal(t, map[string][]models.GivenValue{"other.spec.ts": {{Name: "outside-cookie-b-1", Value: "x"}}},
		testKeyGiven(&models.Mapping{KeyGiven: []models.KeyGiven{{Key: "other.spec.ts", Given: []models.GivenValue{{Name: "outside-cookie-b-1", Value: "x"}}}}}))
}
