package mockdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

// EncodeMockJSON builds a NetworkTrafficDocJSON directly for HTTP-shape mocks
// (HTTP, DNS, Generic, Redis, Kafka, HTTP2 — anything whose spec struct is
// JSON-marshal-clean). Unlike EncodeMock, this path does NOT build a
// yaml.Node tree first, so it avoids the gopkg.in/yaml.v3 emitter/parser
// allocation cost under recording load. Wire-message-based kinds
// (Mongo/MySQL/Postgres) still go through the legacy path because their
// specs contain pre-encoded yaml.Node fields (req.Message).
func EncodeMockJSON(mock *models.Mock, logger *zap.Logger) (*yaml.NetworkTrafficDocJSON, bool, error) {
	doc := &yaml.NetworkTrafficDocJSON{
		Version:      mock.Version,
		Kind:         mock.Kind,
		Name:         mock.Name,
		Noise:        mock.Noise,
		ConnectionID: mock.ConnectionID,
	}

	var spec any
	switch mock.Kind {
	case models.HTTP:
		spec = models.HTTPSchema{
			Metadata:         mock.Spec.Metadata,
			Request:          *mock.Spec.HTTPReq,
			Response:         *mock.Spec.HTTPResp,
			Created:          mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.DNS:
		var dnsReq models.DNSReq
		if mock.Spec.DNSReq != nil {
			dnsReq = *mock.Spec.DNSReq
		}
		var dnsResp models.DNSResp
		if mock.Spec.DNSResp != nil {
			dnsResp = *mock.Spec.DNSResp
		}
		spec = models.DNSSchema{
			Metadata:         mock.Spec.Metadata,
			Request:          dnsReq,
			Response:         dnsResp,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.GENERIC:
		spec = models.GenericSchema{
			Metadata:         mock.Spec.Metadata,
			GenericRequests:  mock.Spec.GenericRequests,
			GenericResponses: mock.Spec.GenericResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.REDIS:
		spec = models.RedisSchema{
			Metadata:         mock.Spec.Metadata,
			RedisRequests:    mock.Spec.RedisRequests,
			RedisResponses:   mock.Spec.RedisResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.KAFKA:
		spec = models.KafkaSchema{
			Metadata:         mock.Spec.Metadata,
			KafkaRequests:    mock.Spec.KafkaRequests,
			KafkaResponses:   mock.Spec.KafkaResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.HTTP2:
		var http2Req models.HTTP2Req
		if mock.Spec.HTTP2Req != nil {
			http2Req = *mock.Spec.HTTP2Req
		}
		var http2Resp models.HTTP2Resp
		if mock.Spec.HTTP2Resp != nil {
			http2Resp = *mock.Spec.HTTP2Resp
		}
		spec = models.HTTP2Schema{
			Metadata:         mock.Spec.Metadata,
			Request:          http2Req,
			Response:         http2Resp,
			Created:          mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.GRPC_EXPORT:
		spec = models.GrpcSpec{
			Metadata:         mock.Spec.Metadata,
			GrpcReq:          *mock.Spec.GRPCReq,
			GrpcResp:         *mock.Spec.GRPCResp,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.PostgresV2:
		// JSON-path spec for Postgres: serialise the raw PacketBundle
		// structs directly instead of going through req.Message.Encode
		// (yaml.Node) → MarshalDoc → yaml→JSON round-trip.
		type pgRequest struct {
			Meta         map[string]string     `json:"meta,omitempty"`
			PacketBundle postgres.PacketBundle `json:"packet_bundle"`
		}
		type pgResponse struct {
			Meta         map[string]string     `json:"meta,omitempty"`
			PacketBundle postgres.PacketBundle `json:"packet_bundle"`
		}
		type pgSpec struct {
			Metadata         map[string]string `json:"metadata"`
			Requests         []pgRequest       `json:"requests"`
			Response         []pgResponse      `json:"responses"`
			CreatedAt        int64             `json:"created,omitempty"`
			ReqTimestampMock interface{}       `json:"ReqTimestampMock,omitempty"`
			ResTimestampMock interface{}       `json:"ResTimestampMock,omitempty"`
		}
		reqs := make([]pgRequest, 0, len(mock.Spec.PostgresRequestsV2))
		for _, v := range mock.Spec.PostgresRequestsV2 {
			reqs = append(reqs, pgRequest{PacketBundle: v.PacketBundle})
		}
		resps := make([]pgResponse, 0, len(mock.Spec.PostgresResponsesV2))
		for _, v := range mock.Spec.PostgresResponsesV2 {
			resps = append(resps, pgResponse{PacketBundle: v.PacketBundle})
		}
		spec = pgSpec{
			Metadata:         mock.Spec.Metadata,
			Requests:         reqs,
			Response:         resps,
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.Mongo:
		// JSON-path spec for Mongo: emit the raw in-memory MongoRequest /
		// MongoResponse structs directly. Their Message field is already
		// `interface{}` with json tags on every concrete wire-message
		// type (MongoOpMessage, MongoOpQuery, MongoOpReply, …), so
		// encoding/json handles them natively — no yaml.Node round-trip.
		type mgSpec struct {
			Metadata         map[string]string      `json:"metadata"`
			Requests         []models.MongoRequest  `json:"requests"`
			Response         []models.MongoResponse `json:"responses"`
			CreatedAt        int64                  `json:"created,omitempty"`
			ReqTimestampMock interface{}            `json:"ReqTimestampMock,omitempty"`
			ResTimestampMock interface{}            `json:"ResTimestampMock,omitempty"`
		}
		spec = mgSpec{
			Metadata:         mock.Spec.Metadata,
			Requests:         mock.Spec.MongoRequests,
			Response:         mock.Spec.MongoResponses,
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	case models.MySQL:
		// JSON-path spec for MySQL. mysql.Request / mysql.Response embed
		// PacketBundle whose Message is `interface{}` with JSON-ready
		// packet types, so we can emit them as-is without the
		// per-packet req.Message.Encode(...) yaml.Node dance.
		type mySpec struct {
			Metadata         map[string]string `json:"metadata"`
			Requests         []mysql.Request   `json:"requests"`
			Response         []mysql.Response  `json:"responses"`
			CreatedAt        int64             `json:"created,omitempty"`
			ReqTimestampMock interface{}       `json:"ReqTimestampMock,omitempty"`
			ResTimestampMock interface{}       `json:"ResTimestampMock,omitempty"`
		}
		spec = mySpec{
			Metadata:         mock.Spec.Metadata,
			Requests:         mock.Spec.MySQLRequests,
			Response:         mock.Spec.MySQLResponses,
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
	default:
		// Unknown kind — caller falls back to the legacy path.
		return nil, false, nil
	}

	specBytes, err := json.Marshal(spec)
	if err != nil {
		utils.LogError(logger, err, "failed to marshal mock spec to JSON")
		return nil, true, err
	}
	doc.Spec = specBytes
	return doc, true, nil
}

func EncodeMock(mock *models.Mock, logger *zap.Logger) (*yaml.NetworkTrafficDoc, error) {

	yamlDoc := yaml.NetworkTrafficDoc{
		Version:      mock.Version,
		Kind:         mock.Kind,
		Name:         mock.Name,
		Noise:        mock.Noise,
		ConnectionID: mock.ConnectionID,
	}
	switch mock.Kind {
	case models.Mongo:
		requests := []models.RequestYaml{}
		for _, v := range mock.Spec.MongoRequests {
			req := models.RequestYaml{
				Header:    v.Header,
				ReadDelay: v.ReadDelay,
			}
			err := req.Message.Encode(v.Message)
			if err != nil {
				utils.LogError(logger, err, "failed to encode mongo request wiremessage into yaml")
				return nil, err
			}
			requests = append(requests, req)
		}
		responses := []models.ResponseYaml{}
		for _, v := range mock.Spec.MongoResponses {
			resp := models.ResponseYaml{
				Header:    v.Header,
				ReadDelay: v.ReadDelay,
			}
			err := resp.Message.Encode(v.Message)
			if err != nil {
				utils.LogError(logger, err, "failed to encode mongo response wiremessage into yaml")
				return nil, err
			}
			responses = append(responses, resp)
		}
		mongoSpec := models.MongoSpec{
			Metadata: mock.Spec.Metadata,

			Requests:         requests,
			Response:         responses,
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}

		err := yamlDoc.Spec.Encode(mongoSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the mongo input-output as yaml")
			return nil, err
		}

	case models.HTTP:
		httpSpec := models.HTTPSchema{
			Metadata: mock.Spec.Metadata,

			Request:          *mock.Spec.HTTPReq,
			Response:         *mock.Spec.HTTPResp,
			Created:          mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(httpSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the http input-output as yaml")
			return nil, err
		}
	case models.DNS:
		var dnsReq models.DNSReq
		if mock.Spec.DNSReq != nil {
			dnsReq = *mock.Spec.DNSReq
		}
		var dnsResp models.DNSResp
		if mock.Spec.DNSResp != nil {
			dnsResp = *mock.Spec.DNSResp
		}
		dnsSpec := models.DNSSchema{
			Metadata: mock.Spec.Metadata,

			Request:          dnsReq,
			Response:         dnsResp,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(dnsSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the dns input-output as yaml")
			return nil, err
		}
	case models.GENERIC:
		genericSpec := models.GenericSchema{
			Metadata: mock.Spec.Metadata,

			GenericRequests:  mock.Spec.GenericRequests,
			GenericResponses: mock.Spec.GenericResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(genericSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the generic input-output as yaml")
			return nil, err
		}
	case models.REDIS:
		redisSpec := models.RedisSchema{
			Metadata: mock.Spec.Metadata,

			RedisRequests:    mock.Spec.RedisRequests,
			RedisResponses:   mock.Spec.RedisResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(redisSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the redis input-output as yaml")
			return nil, err
		}
	case models.KAFKA:
		kafkaSpec := models.KafkaSchema{
			Metadata: mock.Spec.Metadata,

			KafkaRequests:    mock.Spec.KafkaRequests,
			KafkaResponses:   mock.Spec.KafkaResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(kafkaSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the kafka input-output as yaml")
			return nil, err
		}
	case models.PostgresV2:
		requests := []postgres.RequestYaml{}
		for _, v := range mock.Spec.PostgresRequestsV2 {

			req := postgres.RequestYaml{}
			err := req.Message.Encode(v.PacketBundle)
			if err != nil {
				utils.LogError(logger, err, "failed to encode postgres request wiremessage into yaml")
				return nil, err
			}
			requests = append(requests, req)
		}
		responses := []postgres.ResponseYaml{}
		for _, v := range mock.Spec.PostgresResponsesV2 {
			resp := postgres.ResponseYaml{}
			err := resp.Message.Encode(v.PacketBundle)
			if err != nil {
				utils.LogError(logger, err, "failed to encode postgres response wiremessage into yaml")
				return nil, err
			}
			responses = append(responses, resp)
		}

		sqlSpec := postgres.Spec{
			Metadata: mock.Spec.Metadata,

			Requests:         requests,
			Response:         responses,
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(sqlSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the Postgres input-output as yaml")
			return nil, err
		}
	case models.GRPC_EXPORT:
		gRPCSpec := models.GrpcSpec{
			Metadata: mock.Spec.Metadata,

			GrpcReq:          *mock.Spec.GRPCReq,
			GrpcResp:         *mock.Spec.GRPCResp,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(gRPCSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal gRPC of external call into yaml")
			return nil, err
		}
	case models.MySQL:
		requests := []mysql.RequestYaml{}
		for _, v := range mock.Spec.MySQLRequests {

			req := mysql.RequestYaml{
				Header: v.Header,
				Meta:   v.Meta,
			}
			err := req.Message.Encode(v.Message)
			if err != nil {
				utils.LogError(logger, err, "failed to encode mysql request wiremessage into yaml")
				return nil, err
			}
			requests = append(requests, req)
		}
		responses := []mysql.ResponseYaml{}
		for _, v := range mock.Spec.MySQLResponses {
			resp := mysql.ResponseYaml{
				Header: v.Header,
				Meta:   v.Meta,
			}
			err := resp.Message.Encode(v.Message)
			if err != nil {
				utils.LogError(logger, err, "failed to encode mysql response wiremessage into yaml")
				return nil, err
			}
			responses = append(responses, resp)
		}

		sqlSpec := mysql.Spec{
			Metadata: mock.Spec.Metadata,

			Requests:         requests,
			Response:         responses,
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(sqlSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the MySQL input-output as yaml")
			return nil, err
		}
	case models.HTTP2:
		var http2Req models.HTTP2Req
		if mock.Spec.HTTP2Req != nil {
			http2Req = *mock.Spec.HTTP2Req
		}
		var http2Resp models.HTTP2Resp
		if mock.Spec.HTTP2Resp != nil {
			http2Resp = *mock.Spec.HTTP2Resp
		}
		http2Spec := models.HTTP2Schema{
			Metadata: mock.Spec.Metadata,

			Request:          http2Req,
			Response:         http2Resp,
			Created:          mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(http2Spec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the HTTP/2 input-output as yaml")
			return nil, err
		}
	default:
		utils.LogError(logger, nil, "failed to marshal the recorded mock into yaml due to invalid kind of mock")
		return nil, errors.New("type of mock is invalid")
	}

	return &yamlDoc, nil
}

func DecodeMocks(yamlMocks []*yaml.NetworkTrafficDoc, logger *zap.Logger) ([]*models.Mock, error) {
	mocks := []*models.Mock{}

	for _, m := range yamlMocks {
		mock := models.Mock{
			Version:      m.Version,
			Name:         m.Name,
			Kind:         m.Kind,
			Noise:        m.Noise,
			ConnectionID: m.ConnectionID,
		}
		mockCheck := strings.Split(string(m.Kind), "-")
		if len(mockCheck) > 1 {
			logger.Debug("This dependency does not belong to open source version, will be skipped", zap.String("mock kind:", string(m.Kind)))
			continue
		}
		switch m.Kind {
		case models.HTTP:
			httpSpec := models.HTTPSchema{}
			err := m.Spec.Decode(&httpSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into http mock", zap.String("mock name", m.Name))
				return nil, err
			}

			mock.Spec = models.MockSpec{
				Metadata: httpSpec.Metadata,

				HTTPReq:          &httpSpec.Request,
				HTTPResp:         &httpSpec.Response,
				Created:          httpSpec.Created,
				ReqTimestampMock: httpSpec.ReqTimestampMock,
				ResTimestampMock: httpSpec.ResTimestampMock,
			}
		case models.DNS:
			dnsSpec := models.DNSSchema{}
			err := m.Spec.Decode(&dnsSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into dns mock", zap.String("mock name", m.Name))
				return nil, err
			}
			metadata := dnsSpec.Metadata
			if metadata == nil {
				metadata = map[string]string{}
			}
			mock.Spec = models.MockSpec{
				Metadata: metadata,

				DNSReq:           &dnsSpec.Request,
				DNSResp:          &dnsSpec.Response,
				ReqTimestampMock: dnsSpec.ReqTimestampMock,
				ResTimestampMock: dnsSpec.ResTimestampMock,
			}
		case models.Mongo:
			mongoSpec := models.MongoSpec{}
			err := m.Spec.Decode(&mongoSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into mongo mock", zap.String("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decodeMongoMessage(&mongoSpec, logger)
			if err != nil {
				return nil, err
			}
			mock.Spec = *mockSpec
		case models.GRPC_EXPORT:
			grpcSpec := models.GrpcSpec{}
			err := m.Spec.Decode(&grpcSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into http mock", zap.String("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: grpcSpec.Metadata,

				GRPCResp:         &grpcSpec.GrpcResp,
				GRPCReq:          &grpcSpec.GrpcReq,
				ReqTimestampMock: grpcSpec.ReqTimestampMock,
				ResTimestampMock: grpcSpec.ResTimestampMock,
			}
		case models.GENERIC:
			genericSpec := models.GenericSchema{}
			err := m.Spec.Decode(&genericSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into generic mock", zap.String("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: genericSpec.Metadata,

				GenericRequests:  genericSpec.GenericRequests,
				GenericResponses: genericSpec.GenericResponses,
				ReqTimestampMock: genericSpec.ReqTimestampMock,
				ResTimestampMock: genericSpec.ResTimestampMock,
			}
		case models.REDIS:
			redisSpec := models.RedisSchema{}
			err := m.Spec.Decode(&redisSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into redis mock", zap.String("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: redisSpec.Metadata,

				RedisRequests:    redisSpec.RedisRequests,
				RedisResponses:   redisSpec.RedisResponses,
				ReqTimestampMock: redisSpec.ReqTimestampMock,
				ResTimestampMock: redisSpec.ResTimestampMock,
			}
		case models.KAFKA:
			kafkaSpec := models.KafkaSchema{}
			err := m.Spec.Decode(&kafkaSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into kafka mock", zap.String("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: kafkaSpec.Metadata,

				KafkaRequests:    kafkaSpec.KafkaRequests,
				KafkaResponses:   kafkaSpec.KafkaResponses,
				ReqTimestampMock: kafkaSpec.ReqTimestampMock,
				ResTimestampMock: kafkaSpec.ResTimestampMock,
			}
		case models.PostgresV2:

			PostSpec := postgres.Spec{}
			err := m.Spec.Decode(&PostSpec)

			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into postgresV2 mock", zap.String("mock name", m.Name))
				return nil, err
			}

			// Convert YAML-friendly Spec to in-memory MockSpec with decoded PacketBundles
			mockSpec, err := decodePostgresV2Message(logger, &PostSpec)
			if err != nil {
				return nil, err
			}
			mock.Spec = *mockSpec
		case models.MySQL:
			mySQLSpec := mysql.Spec{}
			err := m.Spec.Decode(&mySQLSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into mysql mock", zap.String("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decodeMySQLMessage(context.Background(), logger, &mySQLSpec)
			if err != nil {
				return nil, err
			}
			mock.Spec = *mockSpec
		case models.HTTP2:
			http2Spec := models.HTTP2Schema{}
			err := m.Spec.Decode(&http2Spec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into http2 mock", zap.String("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: http2Spec.Metadata,

				HTTP2Req:         &http2Spec.Request,
				HTTP2Resp:        &http2Spec.Response,
				Created:          http2Spec.Created,
				ReqTimestampMock: http2Spec.ReqTimestampMock,
				ResTimestampMock: http2Spec.ResTimestampMock,
			}
		default:
			utils.LogError(logger, nil, "failed to unmarshal a mock yaml doc of unknown type", zap.String("type", string(m.Kind)))
			continue
		}
		mocks = append(mocks, &mock)
	}

	return mocks, nil
}

func decodeMySQLMessage(_ context.Context, logger *zap.Logger, yamlSpec *mysql.Spec) (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata: yamlSpec.Metadata,

		Created:          yamlSpec.CreatedAt,
		ReqTimestampMock: yamlSpec.ReqTimestampMock,
		ResTimestampMock: yamlSpec.ResTimestampMock,
	}

	// Decode the requests

	requests := []mysql.Request{}
	for _, v := range yamlSpec.Requests {
		req := mysql.Request{
			PacketBundle: mysql.PacketBundle{
				Header: v.Header,
				Meta:   v.Meta,
			},
		}

		switch v.Header.Type {
		// connection phase

		case mysql.SSLRequest:
			msg := &mysql.SSLRequestPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql SSLRequestPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.HandshakeResponse41:
			msg := &mysql.HandshakeResponse41Packet{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql HandshakeResponse41Packet")
				return nil, err
			}
			req.Message = msg

		case mysql.CachingSha2PasswordToString(mysql.RequestPublicKey):
			var msg string
			err := v.Message.Decode(&msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql (string) RequestPublicKey")
				return nil, err
			}
			req.Message = msg

		case mysql.EncryptedPassword:
			var msg string
			err := v.Message.Decode(&msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql (string) encrypted_password")
				return nil, err
			}
			req.Message = msg
		case mysql.PlainPassword:
			var msg string
			err := v.Message.Decode(&msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql (string) plain_password")
				return nil, err
			}
			req.Message = msg

		// command phase

		// utility packets
		case mysql.CommandStatusToString(mysql.COM_QUIT):
			msg := &mysql.QuitPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql QuitPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_INIT_DB):
			msg := &mysql.InitDBPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql InitDBPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STATISTICS):
			msg := &mysql.StatisticsPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StatisticsPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_DEBUG):
			msg := &mysql.DebugPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql DebugPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_PING):
			msg := &mysql.PingPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql PingPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_CHANGE_USER):
			msg := &mysql.ChangeUserPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql ChangeUserPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION):
			msg := &mysql.ResetConnectionPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql ResetConnectionPacket")
				return nil, err
			}
			req.Message = msg

		// case mysql.CommandStatusToString(mysql.COM_SET_OPTION):	// not supported yet

		// query packets
		case mysql.CommandStatusToString(mysql.COM_QUERY):
			msg := &mysql.QueryPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql QueryPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STMT_PREPARE):
			msg := &mysql.StmtPreparePacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtPreparePacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE):
			msg := &mysql.StmtExecutePacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtExecutePacket")
				return nil, err
			}
			req.Message = msg

		// case mysql.CommandStatusToString(mysql.COM_FETCH): // not supported yet

		case mysql.CommandStatusToString(mysql.COM_STMT_CLOSE):
			msg := &mysql.StmtClosePacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtClosePacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STMT_RESET):
			msg := &mysql.StmtResetPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtResetPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA):
			msg := &mysql.StmtSendLongDataPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtSendLongDataPacket")
				return nil, err
			}
			req.Message = msg
		}
		requests = append(requests, req)
	}

	mockSpec.MySQLRequests = requests

	// Decode the responses

	responses := []mysql.Response{}
	for _, v := range yamlSpec.Response {

		resp := mysql.Response{
			PacketBundle: mysql.PacketBundle{
				Header: v.Header,
				Meta:   v.Meta,
			},
		}

		switch v.Header.Type {
		// generic response
		case mysql.StatusToString(mysql.EOF):
			msg := &mysql.EOFPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql EOFPacket")
				return nil, err
			}
			resp.Message = msg

		case mysql.StatusToString(mysql.ERR):
			msg := &mysql.ERRPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql ERRPacket")
				return nil, err
			}
			resp.Message = msg

		case mysql.StatusToString(mysql.OK):
			msg := &mysql.OKPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql OKPacket")
				return nil, err
			}
			resp.Message = msg

		// connection phase
		case mysql.AuthStatusToString(mysql.HandshakeV10):
			msg := &mysql.HandshakeV10Packet{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql HandshakeV10Packet")
				return nil, err
			}
			resp.Message = msg

		case mysql.AuthStatusToString(mysql.AuthSwitchRequest):
			msg := &mysql.AuthSwitchRequestPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql AuthSwitchRequestPacket")
				return nil, err
			}
			resp.Message = msg

		case mysql.AuthStatusToString(mysql.AuthMoreData):
			msg := &mysql.AuthMoreDataPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql AuthMoreDataPacket")
				return nil, err
			}
			resp.Message = msg

		case mysql.AuthStatusToString(mysql.AuthNextFactor): // not supported yet
			msg := &mysql.AuthNextFactorPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql AuthNextFactorPacket")
				return nil, err
			}
			resp.Message = msg

		// command phase
		case mysql.COM_STMT_PREPARE_OK:
			msg := &mysql.StmtPrepareOkPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql StmtPrepareOkPacket")
				return nil, err
			}
			resp.Message = msg

		case string(mysql.Text):
			msg := &mysql.TextResultSet{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql TextResultSet")
				return nil, err
			}
			resp.Message = msg

		case string(mysql.Binary):
			msg := &mysql.BinaryProtocolResultSet{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql BinaryProtocolResultSet")
				return nil, err
			}
			resp.Message = msg
		}
		responses = append(responses, resp)
	}

	mockSpec.MySQLResponses = responses

	return &mockSpec, nil
}

func decodeMongoMessage(yamlSpec *models.MongoSpec, logger *zap.Logger) (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata: yamlSpec.Metadata,

		Created:          yamlSpec.CreatedAt,
		ReqTimestampMock: yamlSpec.ReqTimestampMock,
		ResTimestampMock: yamlSpec.ResTimestampMock,
	}

	// mongo request
	requests := []models.MongoRequest{}
	for _, v := range yamlSpec.Requests {
		req := models.MongoRequest{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		// decode the yaml document to mongo request wiremessage
		switch v.Header.Opcode {
		case wiremessage.OpMsg:
			requestMessage := &models.MongoOpMessage{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpMsg request wiremessage")
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpReply:
			requestMessage := &models.MongoOpReply{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpReply request wiremessage")
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpQuery:
			requestMessage := &models.MongoOpQuery{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpQuery request wiremessage")
				// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			req.Message = requestMessage
		default:
		}
		requests = append(requests, req)
	}
	mockSpec.MongoRequests = requests

	// mongo response
	responses := []models.MongoResponse{}
	for _, v := range yamlSpec.Response {
		resp := models.MongoResponse{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		// decode the yaml document to mongo response wiremessage
		switch v.Header.Opcode {
		case wiremessage.OpMsg:
			responseMessage := &models.MongoOpMessage{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpMsg response wiremessage")
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
		case wiremessage.OpReply:
			responseMessage := &models.MongoOpReply{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpMsg response wiremessage")
				return nil, err
			}
			resp.Message = responseMessage
		case wiremessage.OpQuery:
			responseMessage := &models.MongoOpQuery{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpMsg response wiremessage")
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
		default:
		}
		responses = append(responses, resp)
	}
	mockSpec.MongoResponses = responses
	return &mockSpec, nil
}

// decodePostgresV2Message decodes a postgres.Spec (YAML-friendly format) into a models.MockSpec
// by converting RequestYaml/ResponseYaml into concrete postgres.Request/Response with PacketBundles.
func decodePostgresV2Message(logger *zap.Logger, yamlSpec *postgres.Spec) (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata: yamlSpec.Metadata,

		Created:          yamlSpec.CreatedAt,
		ReqTimestampMock: yamlSpec.ReqTimestampMock,
		ResTimestampMock: yamlSpec.ResTimestampMock,
	}

	// Decode requests
	reqs := []postgres.Request{}
	for _, v := range yamlSpec.Requests {
		var bundle postgres.PacketBundle
		if err := v.Message.Decode(&bundle); err != nil {
			utils.LogError(logger, err, "failed to unmarshal yaml document into postgresV2 request PacketBundle")
			return nil, err
		}
		reqs = append(reqs, postgres.Request{PacketBundle: bundle})
	}
	mockSpec.PostgresRequestsV2 = reqs

	// Decode responses
	resps := []postgres.Response{}
	for _, v := range yamlSpec.Response {
		var bundle postgres.PacketBundle
		if err := v.Message.Decode(&bundle); err != nil {
			utils.LogError(logger, err, "failed to unmarshal yaml document into postgresV2 response PacketBundle")
			return nil, err
		}
		resps = append(resps, postgres.Response{PacketBundle: bundle})
	}
	mockSpec.PostgresResponsesV2 = resps
	return &mockSpec, nil
}

// DecodeMocksJSON is the JSON-native read companion to EncodeMockJSON: it
// unmarshals the json.RawMessage spec of each NetworkTrafficDocJSON directly
// into the concrete in-memory models (HTTPSchema, MongoSpec, mysql.Spec,
// ...). No yaml.Node, no DocToJSON, no gopkg.in/yaml.v3 involved.
//
// For the wire-message kinds (Mongo / MySQL / PostgresV2) the decoder mirrors
// the shape written by EncodeMockJSON: Messages are emitted as JSON-native
// concrete types so unmarshal into `interface{}` yields a map — we recover
// the typed Message by dispatching on Header.Opcode / Header.Type.
func DecodeMocksJSON(docs []*yaml.NetworkTrafficDocJSON, logger *zap.Logger) ([]*models.Mock, error) {
	mocks := make([]*models.Mock, 0, len(docs))
	for _, m := range docs {
		// Skip enterprise-only kinds the way DecodeMocks does.
		if strings.Count(string(m.Kind), "-") > 0 {
			logger.Debug("This dependency does not belong to open source version, will be skipped", zap.String("mock kind:", string(m.Kind)))
			continue
		}

		mock := models.Mock{
			Version:      m.Version,
			Name:         m.Name,
			Kind:         m.Kind,
			Noise:        m.Noise,
			ConnectionID: m.ConnectionID,
		}

		switch m.Kind {
		case models.HTTP:
			var s models.HTTPSchema
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal http mock %q: %w", m.Name, err)
			}
			mock.Spec = models.MockSpec{
				Metadata:         s.Metadata,
				HTTPReq:          &s.Request,
				HTTPResp:         &s.Response,
				Created:          s.Created,
				ReqTimestampMock: s.ReqTimestampMock,
				ResTimestampMock: s.ResTimestampMock,
			}
		case models.DNS:
			var s models.DNSSchema
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal dns mock %q: %w", m.Name, err)
			}
			metadata := s.Metadata
			if metadata == nil {
				metadata = map[string]string{}
			}
			mock.Spec = models.MockSpec{
				Metadata:         metadata,
				DNSReq:           &s.Request,
				DNSResp:          &s.Response,
				ReqTimestampMock: s.ReqTimestampMock,
				ResTimestampMock: s.ResTimestampMock,
			}
		case models.GRPC_EXPORT:
			var s models.GrpcSpec
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal grpc mock %q: %w", m.Name, err)
			}
			mock.Spec = models.MockSpec{
				Metadata:         s.Metadata,
				GRPCReq:          &s.GrpcReq,
				GRPCResp:         &s.GrpcResp,
				ReqTimestampMock: s.ReqTimestampMock,
				ResTimestampMock: s.ResTimestampMock,
			}
		case models.GENERIC:
			var s models.GenericSchema
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal generic mock %q: %w", m.Name, err)
			}
			mock.Spec = models.MockSpec{
				Metadata:         s.Metadata,
				GenericRequests:  s.GenericRequests,
				GenericResponses: s.GenericResponses,
				ReqTimestampMock: s.ReqTimestampMock,
				ResTimestampMock: s.ResTimestampMock,
			}
		case models.REDIS:
			var s models.RedisSchema
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal redis mock %q: %w", m.Name, err)
			}
			mock.Spec = models.MockSpec{
				Metadata:         s.Metadata,
				RedisRequests:    s.RedisRequests,
				RedisResponses:   s.RedisResponses,
				ReqTimestampMock: s.ReqTimestampMock,
				ResTimestampMock: s.ResTimestampMock,
			}
		case models.KAFKA:
			var s models.KafkaSchema
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal kafka mock %q: %w", m.Name, err)
			}
			mock.Spec = models.MockSpec{
				Metadata:         s.Metadata,
				KafkaRequests:    s.KafkaRequests,
				KafkaResponses:   s.KafkaResponses,
				ReqTimestampMock: s.ReqTimestampMock,
				ResTimestampMock: s.ResTimestampMock,
			}
		case models.HTTP2:
			var s models.HTTP2Schema
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal http2 mock %q: %w", m.Name, err)
			}
			mock.Spec = models.MockSpec{
				Metadata:         s.Metadata,
				HTTP2Req:         &s.Request,
				HTTP2Resp:        &s.Response,
				Created:          s.Created,
				ReqTimestampMock: s.ReqTimestampMock,
				ResTimestampMock: s.ResTimestampMock,
			}
		case models.PostgresV2:
			type pgRequest struct {
				Meta         map[string]string     `json:"meta,omitempty"`
				PacketBundle postgres.PacketBundle `json:"packet_bundle"`
			}
			type pgResponse struct {
				Meta         map[string]string     `json:"meta,omitempty"`
				PacketBundle postgres.PacketBundle `json:"packet_bundle"`
			}
			type pgSpec struct {
				Metadata         map[string]string `json:"metadata"`
				Requests         []pgRequest       `json:"requests"`
				Response         []pgResponse      `json:"responses"`
				CreatedAt        int64             `json:"created,omitempty"`
				ReqTimestampMock interface{}       `json:"ReqTimestampMock,omitempty"`
				ResTimestampMock interface{}       `json:"ResTimestampMock,omitempty"`
			}
			var s pgSpec
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal postgresV2 mock %q: %w", m.Name, err)
			}
			reqs := make([]postgres.Request, 0, len(s.Requests))
			for _, r := range s.Requests {
				reqs = append(reqs, postgres.Request{PacketBundle: r.PacketBundle})
			}
			resps := make([]postgres.Response, 0, len(s.Response))
			for _, r := range s.Response {
				resps = append(resps, postgres.Response{PacketBundle: r.PacketBundle})
			}
			mockSpec := models.MockSpec{
				Metadata:            s.Metadata,
				Created:             s.CreatedAt,
				PostgresRequestsV2:  reqs,
				PostgresResponsesV2: resps,
			}
			if t, ok := s.ReqTimestampMock.(string); ok {
				_ = t // timestamps preserved as written; left typed as interface{} on purpose
			}
			mock.Spec = mockSpec
		case models.Mongo:
			// Mongo: Message is interface{} — json.Unmarshal puts a
			// map[string]interface{} there. We dispatch on
			// Header.Opcode to recover the typed message, mirroring
			// what decodeMongoMessage does on the YAML path.
			type mgSpec struct {
				Metadata         map[string]string      `json:"metadata"`
				Requests         []models.MongoRequest  `json:"requests"`
				Response         []models.MongoResponse `json:"responses"`
				CreatedAt        int64                  `json:"created,omitempty"`
				ReqTimestampMock interface{}            `json:"ReqTimestampMock,omitempty"`
				ResTimestampMock interface{}            `json:"ResTimestampMock,omitempty"`
			}
			var s mgSpec
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal mongo mock %q: %w", m.Name, err)
			}
			// Re-type each Message from generic map into the
			// concrete MongoOp* struct based on the opcode.
			for i := range s.Requests {
				typed, err := retypeMongoMessage(s.Requests[i].Header, s.Requests[i].Message)
				if err != nil {
					return nil, fmt.Errorf("failed to retype mongo request message in %q: %w", m.Name, err)
				}
				s.Requests[i].Message = typed
			}
			for i := range s.Response {
				typed, err := retypeMongoMessage(s.Response[i].Header, s.Response[i].Message)
				if err != nil {
					return nil, fmt.Errorf("failed to retype mongo response message in %q: %w", m.Name, err)
				}
				s.Response[i].Message = typed
			}
			mock.Spec = models.MockSpec{
				Metadata:       s.Metadata,
				Created:        s.CreatedAt,
				MongoRequests:  s.Requests,
				MongoResponses: s.Response,
			}
		case models.MySQL:
			type mySpec struct {
				Metadata         map[string]string `json:"metadata"`
				Requests         []mysql.Request   `json:"requests"`
				Response         []mysql.Response  `json:"responses"`
				CreatedAt        int64             `json:"created,omitempty"`
				ReqTimestampMock interface{}       `json:"ReqTimestampMock,omitempty"`
				ResTimestampMock interface{}       `json:"ResTimestampMock,omitempty"`
			}
			var s mySpec
			if err := json.Unmarshal(m.Spec, &s); err != nil {
				return nil, fmt.Errorf("failed to unmarshal mysql mock %q: %w", m.Name, err)
			}
			for i := range s.Requests {
				typed, err := retypeMySQLRequest(s.Requests[i].Header, s.Requests[i].Message)
				if err != nil {
					return nil, fmt.Errorf("failed to retype mysql request message in %q: %w", m.Name, err)
				}
				s.Requests[i].Message = typed
			}
			for i := range s.Response {
				typed, err := retypeMySQLResponse(s.Response[i].Header, s.Response[i].Message)
				if err != nil {
					return nil, fmt.Errorf("failed to retype mysql response message in %q: %w", m.Name, err)
				}
				s.Response[i].Message = typed
			}
			mock.Spec = models.MockSpec{
				Metadata:       s.Metadata,
				Created:        s.CreatedAt,
				MySQLRequests:  s.Requests,
				MySQLResponses: s.Response,
			}
		default:
			logger.Debug("skipping unsupported mock kind on JSON read", zap.String("kind", string(m.Kind)))
			continue
		}
		mocks = append(mocks, &mock)
	}
	return mocks, nil
}

// retypeMongoMessage re-marshals the already-decoded generic map into the
// concrete MongoOp* struct implied by the opcode. This is cheap: one
// json.Marshal + one json.Unmarshal on a small struct.
func retypeMongoMessage(header *models.MongoHeader, raw interface{}) (interface{}, error) {
	if raw == nil || header == nil {
		return raw, nil
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	switch header.Opcode {
	case wiremessage.OpMsg:
		v := &models.MongoOpMessage{}
		if err := json.Unmarshal(buf, v); err != nil {
			return nil, err
		}
		return v, nil
	case wiremessage.OpReply:
		v := &models.MongoOpReply{}
		if err := json.Unmarshal(buf, v); err != nil {
			return nil, err
		}
		return v, nil
	case wiremessage.OpQuery:
		v := &models.MongoOpQuery{}
		if err := json.Unmarshal(buf, v); err != nil {
			return nil, err
		}
		return v, nil
	default:
		return raw, nil
	}
}

// retypeMySQLRequest / retypeMySQLResponse mirror decodeMySQLMessage: they
// select the concrete packet struct based on PacketInfo.Type and re-unmarshal
// the already-decoded generic map into it.
func retypeMySQLRequest(header *mysql.PacketInfo, raw interface{}) (interface{}, error) {
	if raw == nil || header == nil {
		return raw, nil
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var target interface{}
	switch header.Type {
	case mysql.SSLRequest:
		target = &mysql.SSLRequestPacket{}
	case mysql.HandshakeResponse41:
		target = &mysql.HandshakeResponse41Packet{}
	case mysql.CachingSha2PasswordToString(mysql.RequestPublicKey),
		mysql.EncryptedPassword,
		mysql.PlainPassword:
		var s string
		if err := json.Unmarshal(buf, &s); err != nil {
			return nil, err
		}
		return s, nil
	case mysql.CommandStatusToString(mysql.COM_QUIT):
		target = &mysql.QuitPacket{}
	case mysql.CommandStatusToString(mysql.COM_INIT_DB):
		target = &mysql.InitDBPacket{}
	case mysql.CommandStatusToString(mysql.COM_STATISTICS):
		target = &mysql.StatisticsPacket{}
	case mysql.CommandStatusToString(mysql.COM_DEBUG):
		target = &mysql.DebugPacket{}
	case mysql.CommandStatusToString(mysql.COM_PING):
		target = &mysql.PingPacket{}
	case mysql.CommandStatusToString(mysql.COM_CHANGE_USER):
		target = &mysql.ChangeUserPacket{}
	case mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION):
		target = &mysql.ResetConnectionPacket{}
	case mysql.CommandStatusToString(mysql.COM_QUERY):
		target = &mysql.QueryPacket{}
	case mysql.CommandStatusToString(mysql.COM_STMT_PREPARE):
		target = &mysql.StmtPreparePacket{}
	case mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE):
		target = &mysql.StmtExecutePacket{}
	case mysql.CommandStatusToString(mysql.COM_STMT_CLOSE):
		target = &mysql.StmtClosePacket{}
	case mysql.CommandStatusToString(mysql.COM_STMT_RESET):
		target = &mysql.StmtResetPacket{}
	case mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA):
		target = &mysql.StmtSendLongDataPacket{}
	default:
		return raw, nil
	}
	if err := json.Unmarshal(buf, target); err != nil {
		return nil, err
	}
	return target, nil
}

func retypeMySQLResponse(header *mysql.PacketInfo, raw interface{}) (interface{}, error) {
	if raw == nil || header == nil {
		return raw, nil
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var target interface{}
	switch header.Type {
	case mysql.StatusToString(mysql.EOF):
		target = &mysql.EOFPacket{}
	case mysql.StatusToString(mysql.ERR):
		target = &mysql.ERRPacket{}
	case mysql.StatusToString(mysql.OK):
		target = &mysql.OKPacket{}
	case mysql.AuthStatusToString(mysql.HandshakeV10):
		target = &mysql.HandshakeV10Packet{}
	case mysql.AuthStatusToString(mysql.AuthSwitchRequest):
		target = &mysql.AuthSwitchRequestPacket{}
	case mysql.AuthStatusToString(mysql.AuthMoreData):
		target = &mysql.AuthMoreDataPacket{}
	case mysql.AuthStatusToString(mysql.AuthNextFactor):
		target = &mysql.AuthNextFactorPacket{}
	case mysql.COM_STMT_PREPARE_OK:
		target = &mysql.StmtPrepareOkPacket{}
	case string(mysql.Text):
		target = &mysql.TextResultSet{}
	case string(mysql.Binary):
		target = &mysql.BinaryProtocolResultSet{}
	default:
		return raw, nil
	}
	if err := json.Unmarshal(buf, target); err != nil {
		return nil, err
	}
	return target, nil
}
