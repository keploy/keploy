package logging

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestCaseSummary(tc *models.TestCase) []zap.Field {
	if tc == nil {
		return []zap.Field{zap.Bool("test_case_nil", true)}
	}

	fields := []zap.Field{
		zap.String("kind", tc.GetKind()),
		zap.String("name", tc.Name),
		zap.Bool("has_binary_file", tc.HasBinaryFile),
		zap.Uint16("app_port", tc.AppPort),
		zap.Int("mock_count", len(tc.Mocks)),
	}

	if tc.HTTPReq.Method != "" {
		fields = append(fields,
			zap.String("http_method", string(tc.HTTPReq.Method)),
			zap.String("http_url", tc.HTTPReq.URL),
			zap.Int("request_body_bytes", len(tc.HTTPReq.Body)),
		)
	}

	if tc.HTTPResp.StatusCode != 0 {
		fields = append(fields,
			zap.Int("http_status_code", tc.HTTPResp.StatusCode),
			zap.Int("response_body_bytes", len(tc.HTTPResp.Body)),
			zap.String("response_content_type", tc.HTTPResp.Header["Content-Type"]),
		)
	}

	return fields
}
