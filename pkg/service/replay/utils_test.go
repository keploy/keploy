package replay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
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
