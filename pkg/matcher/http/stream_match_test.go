package http

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMatchStreamingResponse_WithNoiseOnSSEData(t *testing.T) {
	tc := &models.TestCase{
		Name: "stream-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{},
			StreamType: models.HTTPStreamTypeSSE,
			StreamEvents: []models.HTTPStreamEvent{
				{
					Sequence: 1,
					Data:     `event:TICKER` + "\n" + `data:[{"message":{"id":"abc","timestamp":1000}}]`,
				},
			},
		},
		Noise: map[string][]string{
			"body.data.message.timestamp": []string{},
		},
	}

	actual := &models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{},
		StreamType: models.HTTPStreamTypeSSE,
		StreamEvents: []models.HTTPStreamEvent{
			{
				Sequence: 1,
				Data:     `event:TICKER` + "\n" + `data:[{"message":{"id":"abc","timestamp":2000}}]`,
			},
		},
	}

	pass, _ := Match(tc, actual, map[string]map[string][]string{
		"body":   {},
		"header": {},
	}, false, false, zap.NewNop(), false)
	require.True(t, pass)
}

func TestMatchStreamingResponse_WithoutNoiseFailsOnSSEData(t *testing.T) {
	tc := &models.TestCase{
		Name: "stream-no-noise",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{},
			StreamType: models.HTTPStreamTypeSSE,
			StreamEvents: []models.HTTPStreamEvent{
				{
					Sequence: 1,
					Data:     `event:TICKER` + "\n" + `data:[{"message":{"id":"abc","timestamp":1000}}]`,
				},
			},
		},
		Noise: map[string][]string{},
	}

	actual := &models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{},
		StreamType: models.HTTPStreamTypeSSE,
		StreamEvents: []models.HTTPStreamEvent{
			{
				Sequence: 1,
				Data:     `event:TICKER` + "\n" + `data:[{"message":{"id":"abc","timestamp":2000}}]`,
			},
		},
	}

	pass, _ := Match(tc, actual, map[string]map[string][]string{
		"body":   {},
		"header": {},
	}, false, false, zap.NewNop(), false)
	require.False(t, pass)
}
