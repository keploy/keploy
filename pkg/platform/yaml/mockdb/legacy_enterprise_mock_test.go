package mockdb

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func TestLegacyRedisAndKafkaYamlFallback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name string
		mock *models.Mock
		want func(models.MockSpec) []models.Payload
	}{
		{
			name: "redis",
			mock: &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Name:    "redis-mock",
				Kind:    models.REDIS,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"type": "config"},
					RedisRequests:    []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: "PING"}}}},
					RedisResponses:   []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "utf-8", Data: "PONG"}}}},
					ReqTimestampMock: now,
					ResTimestampMock: now.Add(time.Second),
				},
			},
			want: func(spec models.MockSpec) []models.Payload { return spec.RedisRequests },
		},
		{
			name: "kafka",
			mock: &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Name:    "kafka-mock",
				Kind:    models.KAFKA,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"type": "config"},
					KafkaRequests:    []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "binary", Data: "request"}}}},
					KafkaResponses:   []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "binary", Data: "response"}}}},
					ReqTimestampMock: now,
					ResTimestampMock: now.Add(time.Second),
				},
			},
			want: func(spec models.MockSpec) []models.Payload { return spec.KafkaRequests },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := EncodeMock(tt.mock, zap.NewNop())
			if err != nil {
				t.Fatalf("EncodeMock() error = %v", err)
			}
			got, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
			if err != nil {
				t.Fatalf("DecodeMocks() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("DecodeMocks() returned %d mocks, want 1", len(got))
			}
			if got[0].Kind != tt.mock.Kind {
				t.Fatalf("Kind = %q, want %q", got[0].Kind, tt.mock.Kind)
			}
			if !reflect.DeepEqual(tt.want(got[0].Spec), tt.want(tt.mock.Spec)) {
				t.Fatalf("decoded request payload mismatch\nwant %#v\n got %#v", tt.want(tt.mock.Spec), tt.want(got[0].Spec))
			}
			if !reflect.DeepEqual(got[0].Spec.Metadata, tt.mock.Spec.Metadata) {
				t.Fatalf("metadata mismatch\nwant %#v\n got %#v", tt.mock.Spec.Metadata, got[0].Spec.Metadata)
			}
		})
	}
}
