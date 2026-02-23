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

func TestReorderForStreamingByRequestTime_326(t *testing.T) {
	base := time.Date(2026, 2, 20, 9, 31, 7, 0, time.UTC)
	tc1 := &models.TestCase{
		Name: "test-1",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Timestamp: base.Add(500 * time.Millisecond),
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{"Content-Type": "application/json"},
		},
	}
	tc2 := &models.TestCase{
		Name: "test-2",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Timestamp: base,
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{"Content-Type": "text/event-stream"},
		},
	}
	tc3 := &models.TestCase{
		Name: "test-3",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Timestamp: base.Add(600 * time.Millisecond),
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{"Content-Type": "application/json"},
		},
	}

	ordered, changed := reorderForStreamingByRequestTime([]*models.TestCase{tc1, tc2, tc3})
	require.True(t, changed)
	require.Len(t, ordered, 3)
	assert.Equal(t, "test-2", ordered[0].Name)
	assert.Equal(t, "test-1", ordered[1].Name)
	assert.Equal(t, "test-3", ordered[2].Name)
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
