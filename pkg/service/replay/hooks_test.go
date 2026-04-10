package replay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

func TestEffectiveHTTPConfigPort_OptionsPreflightOnSSEPort_UsesSSEPort(t *testing.T) {
	cfg := config.Test{
		Port:    8000,
		SSEPort: 8047,
		Protocol: config.ProtocolConfig{
			"http": {Port: 0},
			"sse":  {Port: 0},
		},
	}

	tc := &models.TestCase{
		Kind:    models.HTTP,
		AppPort: 8047,
		HTTPReq: models.HTTPReq{
			Method: "OPTIONS",
			Header: map[string]string{
				"Accept": "*/*",
			},
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				// no Content-Type, typical for a preflight response
			},
		},
	}

	got := effectiveHTTPConfigPort(tc, cfg)
	assert.EqualValues(t, 8047, got)
}

func TestEffectiveHTTPConfigPort_NormalHTTPRequest_UsesHTTPPort(t *testing.T) {
	cfg := config.Test{
		Port:    8000,
		SSEPort: 8047,
		Protocol: config.ProtocolConfig{
			"http": {Port: 0},
			"sse":  {Port: 0},
		},
	}

	tc := &models.TestCase{
		Kind:    models.HTTP,
		AppPort: 8000,
		HTTPReq: models.HTTPReq{
			Method: "GET",
			Header: map[string]string{
				"Accept": "application/json",
			},
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "application/json",
			},
		},
	}

	got := effectiveHTTPConfigPort(tc, cfg)
	assert.EqualValues(t, 8000, got)
}
