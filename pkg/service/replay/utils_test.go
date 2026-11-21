package replay

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestCloneGlobalNoise_DeepCopy_324(t *testing.T) {
	src := config.GlobalNoise{
		"body": {
			"id": []string{`\\d+`},
		},
		"header": {
			"date": []string{".*"},
		},
	}

	cloned := CloneGlobalNoise(src)
	require.Equal(t, src, cloned)

	cloned["body"]["id"][0] = "changed"
	cloned["header"]["x-request-id"] = []string{"uuid"}
	delete(cloned, "body")

	_, hasBody := src["body"]
	assert.True(t, hasBody)
	assert.Equal(t, `\\d+`, src["body"]["id"][0])
	_, hasNewHeaderField := src["header"]["x-request-id"]
	assert.False(t, hasNewHeaderField)
}

// TestPrepareMockNoiseConfig_IncludesBodyNoise guards that the mock-matching
// noise config handed to the proxy integrations (OutgoingOptions.NoiseConfig)
// carries BOTH header and body noise. Body noise was previously dropped, which
// left outgoing-payload matchers (e.g. the Pulsar SEND matcher) with no way to
// ignore non-deterministic fields and made noise-only differences fail replay.
func TestPrepareMockNoiseConfig_IncludesBodyNoise(t *testing.T) {
	global := config.GlobalNoise{
		"header": {"date": []string{".*"}},
		"body":   {"eventTs": []string{}, "eventId": []string{}},
	}

	got := PrepareMockNoiseConfig(global, config.TestsetNoise{}, "test-set-0")

	require.Contains(t, got, "header", "header noise must still flow")
	require.Contains(t, got, "body", "body noise must now flow (regression guard)")
	_, hasTs := got["body"]["eventTs"]
	_, hasID := got["body"]["eventId"]
	assert.True(t, hasTs && hasID, "expected eventTs and eventId body-noise fields to be carried")
}

func TestLeftJoinNoise_DoesNotMutateGlobal_325(t *testing.T) {
	global := config.GlobalNoise{
		"body": {
			"id": []string{".*"},
		},
	}
	testset := config.GlobalNoise{
		"body": {
			"session": []string{".*"},
		},
		"header": {
			"date": []string{".*"},
		},
	}

	merged := LeftJoinNoise(global, testset)

	require.Contains(t, merged, "body")
	require.Contains(t, merged["body"], "id")
	require.Contains(t, merged["body"], "session")
	require.Contains(t, merged, "header")
	require.Contains(t, merged["header"], "date")

	_, hasSessionOnGlobal := global["body"]["session"]
	assert.False(t, hasSessionOnGlobal)
	_, hasHeaderOnGlobal := global["header"]
	assert.False(t, hasHeaderOnGlobal)
}

func TestEffectiveStreamMockWindow_327(t *testing.T) {
	reqTs := time.Date(2026, 2, 20, 9, 31, 7, 83000000, time.UTC)
	respTs := reqTs.Add(3255 * time.Microsecond)
	tc := &models.TestCase{
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Timestamp: reqTs,
		},
		HTTPResp: models.HTTPResp{
			Timestamp: respTs,
		},
	}

	after, before := effectiveStreamMockWindow(tc, 5)
	assert.Equal(t, reqTs, after)
	assert.Equal(t, respTs.Add(11*time.Second), before)

	after, before = effectiveStreamMockWindow(tc, 30)
	assert.Equal(t, reqTs, after)
	assert.Equal(t, respTs.Add(30*time.Second), before)
}

func TestIsMockSubsetWithConfig(t *testing.T) {
	tests := []struct {
		name          string
		consumedMocks []models.MockState
		expectedMocks []string
		want          bool
	}{
		{
			name: "Exact match",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "test"},
				{Name: "mock-2", Type: "test"},
			},
			expectedMocks: []string{"mock-1", "mock-2"},
			want:          true,
		},
		{
			name: "User example: extra config mocks",
			consumedMocks: []models.MockState{
				{Name: "mock-22", Type: "config"},
				{Name: "mock-23", Type: "config"},
				{Name: "mock-56", Type: "test"},
				{Name: "mock-57", Type: "test"},
				{Name: "mock-58", Type: "test"},
			},
			expectedMocks: []string{"mock-56", "mock-57", "mock-58"},
			want:          true,
		},
		{
			name: "Extra non-config mock (mismatch)",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "test"},
				{Name: "mock-2", Type: "test"},
			},
			expectedMocks: []string{"mock-1"},
			want:          false,
		},
		{
			name: "Missing expected mocks (allowed)",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "test"},
			},
			expectedMocks: []string{"mock-1", "mock-2"},
			want:          true,
		},
		{
			name: "Extra config mock only",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "config"},
			},
			expectedMocks: []string{},
			want:          true,
		},
		{
			name: "Extra non-config mock only (mismatch)",
			consumedMocks: []models.MockState{
				{Name: "mock-1", Type: "test"},
			},
			expectedMocks: []string{},
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMockSubsetWithConfig(tt.consumedMocks, tt.expectedMocks); got != tt.want {
				t.Errorf("isMockSubsetWithConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetainNoisyTestCaseMocks(t *testing.T) {
	tests := []struct {
		name      string
		noisy     []string
		mapping   *models.Mapping
		consumed  map[string]models.MockState
		wantAdded int
		wantKeys  []string // expected keys in consumed after call
	}{
		{
			name:      "nil inputs",
			noisy:     nil,
			mapping:   nil,
			consumed:  nil,
			wantAdded: 0,
		},
		{
			name:      "empty noisy list",
			noisy:     []string{},
			mapping:   &models.Mapping{TestCases: []models.MappedTestCase{{ID: "tc-1"}}},
			consumed:  map[string]models.MockState{},
			wantAdded: 0,
		},
		{
			name:  "noisy test case not in mapping",
			noisy: []string{"tc-missing"},
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{
					{ID: "tc-1", Mocks: []models.MockEntry{{Name: "mock-1", Kind: "HTTP"}}},
				},
			},
			consumed:  map[string]models.MockState{},
			wantAdded: 0,
		},
		{
			name:  "noisy test case present adds mocks",
			noisy: []string{"tc-1"},
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{
					{ID: "tc-1", Mocks: []models.MockEntry{
						{Name: "mock-1", Kind: "HTTP", Timestamp: 100},
						{Name: "mock-2", Kind: "SQL", Timestamp: 200},
					}},
					{ID: "tc-2", Mocks: []models.MockEntry{
						{Name: "mock-3", Kind: "HTTP"},
					}},
				},
			},
			consumed:  map[string]models.MockState{},
			wantAdded: 2,
			wantKeys:  []string{"mock-1", "mock-2"},
		},
		{
			name:  "already consumed mocks not double counted",
			noisy: []string{"tc-1"},
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{
					{ID: "tc-1", Mocks: []models.MockEntry{
						{Name: "mock-1", Kind: "HTTP"},
						{Name: "mock-2", Kind: "SQL"},
					}},
				},
			},
			consumed: map[string]models.MockState{
				"mock-1": {Name: "mock-1"},
			},
			wantAdded: 1,
			wantKeys:  []string{"mock-1", "mock-2"},
		},
		{
			name:  "empty mock name is skipped",
			noisy: []string{"tc-1"},
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{
					{ID: "tc-1", Mocks: []models.MockEntry{
						{Name: "", Kind: "HTTP"},
						{Name: "mock-1", Kind: "SQL"},
					}},
				},
			},
			consumed:  map[string]models.MockState{},
			wantAdded: 1,
			wantKeys:  []string{"mock-1"},
		},
		{
			name:  "empty string in noisy list is ignored",
			noisy: []string{"", "tc-1"},
			mapping: &models.Mapping{
				TestCases: []models.MappedTestCase{
					{ID: "tc-1", Mocks: []models.MockEntry{
						{Name: "mock-1", Kind: "HTTP"},
					}},
				},
			},
			consumed:  map[string]models.MockState{},
			wantAdded: 1,
			wantKeys:  []string{"mock-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := retainNoisyTestCaseMocks(tt.noisy, tt.mapping, tt.consumed)
			if got != tt.wantAdded {
				t.Errorf("retainNoisyTestCaseMocks() = %d, want %d", got, tt.wantAdded)
			}
			for _, key := range tt.wantKeys {
				if _, ok := tt.consumed[key]; !ok {
					t.Errorf("expected key %q in consumed map, but not found", key)
				}
			}
		})
	}
}

// A noise edit (DenoiseTestCases: the dashboard's add-noise action on a local
// replay) adds its fields to the test case the TestDB returns. A TestDB may
// return a case with no noise map, as a store does for a case created without
// noise; adding to it must not write into the nil map, which panics.
func TestDenoiseAddsNoiseToATestCaseWithNoNoiseMap(t *testing.T) {
	db := &prTestDB{cases: []*models.TestCase{{Name: "test-1", Kind: models.HTTP}}}
	r := &Replayer{logger: zap.NewNop(), testDB: db}
	params := []*models.NoiseParams{{TestCaseID: "test-1", Ops: models.OpsAdd, Assertion: map[string][]string{"header.Date": {}}}}
	var (
		got []*models.NoiseParams
		err error
	)
	panicked := func() (p any) {
		defer func() { p = recover() }()
		got, err = r.DenoiseTestCases(context.Background(), "test-set-0", params)
		return nil
	}()
	require.Nil(t, panicked, "adding noise to a test case that has no noise map panicked")
	require.NoError(t, err)
	want := map[string][]string{"header.Date": {}}
	require.Equal(t, want, got[0].AfterNoise)
	require.Equal(t, want, db.cases[0].Noise)
}

// An add-noise edit merges its fields into the noise the test case already
// has: the case keeps every field it was already marked noisy on, and a field
// it already had gets the new values after its own. An edit that replaced the
// case's noise with the added fields would make the next replay assert on the
// fields the user marked noisy before.
func TestDenoiseKeepsTheNoiseATestCaseAlreadyHas(t *testing.T) {
	db := &prTestDB{cases: []*models.TestCase{{
		Name: "test-1",
		Kind: models.HTTP,
		Noise: map[string][]string{
			"body.id":          {},
			"header.X-Request": {"req-[0-9]+"},
		},
	}}}
	r := &Replayer{logger: zap.NewNop(), testDB: db}
	params := []*models.NoiseParams{{TestCaseID: "test-1", Ops: models.OpsAdd, Assertion: map[string][]string{
		"header.Date":      {},
		"header.X-Request": {"id-[a-f]+"},
	}}}
	got, err := r.DenoiseTestCases(context.Background(), "test-set-0", params)
	require.NoError(t, err)
	want := map[string][]string{
		"body.id":          {},
		"header.X-Request": {"req-[0-9]+", "id-[a-f]+"},
		"header.Date":      {},
	}
	require.Equal(t, want, got[0].AfterNoise)
	require.Equal(t, want, db.cases[0].Noise)
}

// What an add-noise edit makes of a field's entry. An entry is a list of
// patterns the field's value is excused by, or empty: excused whatever it reads
// (ignore-always). An ignore-always field stays so whatever is added (a pattern
// would narrow it, and the replay would fail on values it excused before);
// adding ignore-always to a field with patterns makes it ignore-always; and
// patterns are unioned, each once, in the order each first appears.
func TestDenoiseMergesAFieldsNoise(t *testing.T) {
	for _, c := range []struct {
		name           string
		has, add, want map[string][]string
	}{
		{name: "an ignore-always field stays ignore-always when a pattern is added",
			has: map[string][]string{"body.id": {}}, add: map[string][]string{"body.id": {"^[0-9]+$"}},
			want: map[string][]string{"body.id": {}}},
		{name: "adding ignore-always to a field with patterns makes it ignore-always",
			has: map[string][]string{"header.X-Request": {"req-[0-9]+"}}, add: map[string][]string{"header.X-Request": {}},
			want: map[string][]string{"header.X-Request": {}}},
		{name: "a pattern the field has is not added again",
			has:  map[string][]string{"header.X-Request": {"req-[0-9]+"}},
			add:  map[string][]string{"header.X-Request": {"req-[0-9]+", "id-[a-f]+"}},
			want: map[string][]string{"header.X-Request": {"req-[0-9]+", "id-[a-f]+"}}},
		{name: "a new field's patterns are kept once each, in order",
			has:  map[string][]string{},
			add:  map[string][]string{"header.X-Trace": {"t-[0-9]+", "u-[0-9]+", "t-[0-9]+"}},
			want: map[string][]string{"header.X-Trace": {"t-[0-9]+", "u-[0-9]+"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &prTestDB{cases: []*models.TestCase{{Name: "test-1", Kind: models.HTTP, Noise: c.has}}}
			r := &Replayer{logger: zap.NewNop(), testDB: db}
			params := []*models.NoiseParams{{TestCaseID: "test-1", Ops: models.OpsAdd, Assertion: c.add}}
			got, err := r.DenoiseTestCases(context.Background(), "test-set-0", params)
			require.NoError(t, err)
			require.Equal(t, c.want, got[0].AfterNoise)
			require.Equal(t, c.want, db.cases[0].Noise)
		})
	}
}
