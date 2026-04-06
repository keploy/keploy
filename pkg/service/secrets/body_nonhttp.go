package secrets

import (
	"fmt"
	"strings"

	ossModels "go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// ProcessNonHTTPMock scans and replaces secrets in non-HTTP mock payloads.
// Supported: gRPC (headers + JSON body), Redis, Generic (payload data).
// Postgres, MySQL, and Mongo have complex wire-protocol structures —
// we scan their string Payload fields for value patterns.
func ProcessNonHTTPMock(spec *ossModels.MockSpec, kind ossModels.Kind, detector *Detector, engine SecretEngine, tracker *FieldTracker, logger *zap.Logger) {
	switch kind {
	case ossModels.GRPC_EXPORT:
		processGRPCMock(spec, detector, engine, tracker)
	case ossModels.REDIS:
		processPayloads(spec.RedisRequests, detector, engine, tracker, "redis_req")
		processPayloads(spec.RedisResponses, detector, engine, tracker, "redis_resp")
	case ossModels.Postgres:
		processPostgresPayloads(spec, detector, engine, tracker)
	case ossModels.MySQL:
		processMySQLPayloads(spec, detector, engine, tracker)
	case ossModels.Mongo:
		processMongoPayloads(spec, detector, engine, tracker)
	case ossModels.GENERIC:
		processPayloads(spec.GenericRequests, detector, engine, tracker, "generic_req")
		processPayloads(spec.GenericResponses, detector, engine, tracker, "generic_resp")
	default:
		if logger != nil {
			logger.Debug("skipping non-HTTP mock kind for secret protection", zap.String("kind", string(kind)))
		}
	}
}

func processGRPCMock(spec *ossModels.MockSpec, detector *Detector, engine SecretEngine, tracker *FieldTracker) {
	if spec.GRPCReq != nil {
		processStringMap(spec.GRPCReq.Headers.OrdinaryHeaders, detector, engine, tracker, "header")
		processStringMap(spec.GRPCReq.Headers.PseudoHeaders, detector, engine, tracker, "header")
		// gRPC body may contain JSON data.
		if spec.GRPCReq.Body.DecodedData != "" {
			spec.GRPCReq.Body.DecodedData = ProcessJSONBody(
				spec.GRPCReq.Body.DecodedData, detector, engine, tracker,
			)
		}
	}
	if spec.GRPCResp != nil {
		processStringMap(spec.GRPCResp.Headers.OrdinaryHeaders, detector, engine, tracker, "header")
		processStringMap(spec.GRPCResp.Headers.PseudoHeaders, detector, engine, tracker, "header")
		if spec.GRPCResp.Body.DecodedData != "" {
			spec.GRPCResp.Body.DecodedData = ProcessJSONBody(
				spec.GRPCResp.Body.DecodedData, detector, engine, tracker,
			)
		}
		processStringMap(spec.GRPCResp.Trailers.OrdinaryHeaders, detector, engine, tracker, "header")
	}
}

func processPayloads(payloads []ossModels.Payload, detector *Detector, engine SecretEngine, tracker *FieldTracker, prefix string) {
	for i := range payloads {
		for j := range payloads[i].Message {
			data := payloads[i].Message[j].Data
			if data == "" {
				continue
			}
			// Try JSON first.
			trimmed := strings.TrimSpace(data)
			if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
				payloads[i].Message[j].Data = ProcessJSONBody(data, detector, engine, tracker)
				continue
			}
			// Plain text — scan for value patterns.
			if reason := detector.ScanValue(prefix, data); reason != "" {
				processed, err := engine.Process(data)
				if err != nil {
					processed = "[KEPLOY_REDACTED:process_error]"
				}
				payloads[i].Message[j].Data = processed
				tracker.AddBody(prefix)
			}
		}
	}
}

// processPostgresPayloads scans Postgres request/response data for secrets.
// NOTE: Detection-only — Postgres wire protocol structs have complex binary
// fields that cannot be safely modified in-place without breaking the protocol.
// Detected secrets are tracked for noise/audit but not replaced.
func processPostgresPayloads(spec *ossModels.MockSpec, detector *Detector, engine SecretEngine, tracker *FieldTracker) {
	for i := range spec.PostgresRequestsV2 {
		msg := fmt.Sprintf("%v", spec.PostgresRequestsV2[i])
		if reason := detector.ScanValue("pg_req", msg); reason != "" {
			tracker.AddBody("pg_req." + reason)
		}
	}
	for i := range spec.PostgresResponsesV2 {
		msg := fmt.Sprintf("%v", spec.PostgresResponsesV2[i])
		if reason := detector.ScanValue("pg_resp", msg); reason != "" {
			tracker.AddBody("pg_resp." + reason)
		}
	}
}

// processMySQLPayloads scans MySQL request/response payload data for secrets.
// NOTE: Detection-only — MySQL wire protocol messages use complex typed fields
// that cannot be safely modified without breaking protocol semantics.
func processMySQLPayloads(spec *ossModels.MockSpec, detector *Detector, engine SecretEngine, tracker *FieldTracker) {
	for i := range spec.MySQLRequests {
		msg := fmt.Sprintf("%v", spec.MySQLRequests[i].Message)
		if reason := detector.ScanValue("mysql_req", msg); reason != "" {
			// MySQL messages are complex; log detection but can't safely replace in-place
			tracker.AddBody("mysql_req." + reason)
		}
	}
	for i := range spec.MySQLResponses {
		msg := fmt.Sprintf("%v", spec.MySQLResponses[i].Message)
		if reason := detector.ScanValue("mysql_resp", msg); reason != "" {
			tracker.AddBody("mysql_resp." + reason)
		}
	}
}

// processMongoPayloads scans Mongo request/response data for secrets.
func processMongoPayloads(spec *ossModels.MockSpec, detector *Detector, engine SecretEngine, tracker *FieldTracker) {
	for i := range spec.MongoRequests {
		if spec.MongoRequests[i].Header != nil {
			// MongoHeader doesn't have a simple string map, but we can scan
			// the entire header as a string for patterns.
			headerStr := fmt.Sprintf("%+v", spec.MongoRequests[i].Header)
			if reason := detector.ScanValue("mongo_header", headerStr); reason != "" {
				tracker.AddBody("mongo_req_header." + reason)
			}
		}
	}
	for i := range spec.MongoResponses {
		if spec.MongoResponses[i].Header != nil {
			headerStr := fmt.Sprintf("%+v", spec.MongoResponses[i].Header)
			if reason := detector.ScanValue("mongo_header", headerStr); reason != "" {
				tracker.AddBody("mongo_resp_header." + reason)
			}
		}
	}
}

// processStringMap scans a map[string]string for secrets by field name and value pattern.
func processStringMap(m map[string]string, detector *Detector, engine SecretEngine, tracker *FieldTracker, category string) {
	if len(m) == 0 {
		return
	}
	for key, val := range m {
		if detector.IsAllowed(category + "." + key) {
			continue
		}
		detected := false
		if detector.IsBodyKeySensitive(key) {
			detected = true
		}
		if !detected {
			if _, ok := detector.nameHeaders[strings.ToLower(key)]; ok {
				detected = true
			}
		}
		if !detected {
			if reason := detector.ScanValue(key, val); reason != "" {
				detected = true
			}
		}
		if detected {
			processed, err := engine.Process(val)
			if err != nil {
				processed = "[KEPLOY_REDACTED:process_error]"
			}
			m[key] = processed
			if category == "header" {
				tracker.AddHeader(key)
			} else {
				tracker.AddBody(category + "." + key)
			}
		}
	}
}
