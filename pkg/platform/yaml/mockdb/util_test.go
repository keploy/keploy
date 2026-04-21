package mockdb

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	yamlPkg "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func TestEncodeDecode_Pulsar(t *testing.T) {
	logger := zap.NewNop()
	now := time.Now().Truncate(time.Second)

	original := &models.Mock{
		Version: models.GetVersion(),
		Name:    "test-pulsar-mock",
		Kind:    models.PULSAR,
		Spec: models.MockSpec{
			Metadata: map[string]string{"topic": "test-topic"},
			PulsarRequests: []models.Payload{
				{Origin: "client", Message: []models.OutputBinary{{Type: "binary", Data: "AQID"}}},
			},
			PulsarResponses: []models.Payload{
				{Origin: "server", Message: []models.OutputBinary{{Type: "binary", Data: "BAUG"}}},
			},
			ReqTimestampMock: now,
			ResTimestampMock: now.Add(time.Millisecond * 100),
		},
	}

	encoded, err := EncodeMock(original, logger)
	if err != nil {
		t.Fatalf("EncodeMock failed: %v", err)
	}

	decoded, err := DecodeMocks([]*yamlPkg.NetworkTrafficDoc{encoded}, logger)
	if err != nil {
		t.Fatalf("DecodeMocks failed: %v", err)
	}

	if len(decoded) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(decoded))
	}

	m := decoded[0]
	if m.Kind != models.PULSAR {
		t.Errorf("expected kind %s, got %s", models.PULSAR, m.Kind)
	}
	if len(m.Spec.PulsarRequests) != 1 {
		t.Errorf("expected 1 pulsar request, got %d", len(m.Spec.PulsarRequests))
	}
	if len(m.Spec.PulsarResponses) != 1 {
		t.Errorf("expected 1 pulsar response, got %d", len(m.Spec.PulsarResponses))
	}
	if m.Spec.Metadata["topic"] != "test-topic" {
		t.Errorf("expected metadata topic=test-topic, got %s", m.Spec.Metadata["topic"])
	}
}

func TestEncodeDecode_Aerospike(t *testing.T) {
	logger := zap.NewNop()
	now := time.Now().Truncate(time.Second)

	original := &models.Mock{
		Version: models.GetVersion(),
		Name:    "test-aerospike-mock",
		Kind:    models.Aerospike,
		Spec: models.MockSpec{
			Metadata: map[string]string{"namespace": "test-ns"},
			AerospikeRequests: []models.Payload{
				{Origin: "client", Message: []models.OutputBinary{{Type: "binary", Data: "AQID"}}},
			},
			AerospikeResponses: []models.Payload{
				{Origin: "server", Message: []models.OutputBinary{{Type: "binary", Data: "BAUG"}}},
			},
			ReqTimestampMock: now,
			ResTimestampMock: now.Add(time.Millisecond * 100),
		},
	}

	encoded, err := EncodeMock(original, logger)
	if err != nil {
		t.Fatalf("EncodeMock failed: %v", err)
	}

	decoded, err := DecodeMocks([]*yamlPkg.NetworkTrafficDoc{encoded}, logger)
	if err != nil {
		t.Fatalf("DecodeMocks failed: %v", err)
	}

	if len(decoded) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(decoded))
	}

	m := decoded[0]
	if m.Kind != models.Aerospike {
		t.Errorf("expected kind %s, got %s", models.Aerospike, m.Kind)
	}
	if len(m.Spec.AerospikeRequests) != 1 {
		t.Errorf("expected 1 aerospike request, got %d", len(m.Spec.AerospikeRequests))
	}
	if len(m.Spec.AerospikeResponses) != 1 {
		t.Errorf("expected 1 aerospike response, got %d", len(m.Spec.AerospikeResponses))
	}
	if m.Spec.Metadata["namespace"] != "test-ns" {
		t.Errorf("expected metadata namespace=test-ns, got %s", m.Spec.Metadata["namespace"])
	}
}
