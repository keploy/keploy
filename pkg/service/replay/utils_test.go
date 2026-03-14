package replay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
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
