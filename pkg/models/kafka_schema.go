package models

import (
	"time"

	"go.keploy.io/server/v3/pkg/models/kafka"
)

// KafkaSchema represents the YAML-friendly structure for Kafka mocks
type KafkaSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Requests         []kafka.Request   `json:"requests" yaml:"requests"`
	Responses        []kafka.Response  `json:"responses" yaml:"responses"`
	CreatedAt        int64             `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
}
