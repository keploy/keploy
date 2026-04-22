package mockdb

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

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
	case models.PostgresV3Session:
		// Structured session-profile spec; encode directly — no
		// request/response packet arrays for this kind.
		spec := postgresV3SessionYamlSpec{
			Metadata:         mock.Spec.Metadata,
			Session:          mock.Spec.PostgresV3Session,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		if err := yamlDoc.Spec.Encode(spec); err != nil {
			utils.LogError(logger, err, "failed to marshal PostgresV3Session as yaml",
				zap.String("mock_name", mock.Name),
				zap.String("mock_kind", string(mock.Kind)),
				zap.String("next_step", nextStepPostgresV3Encode))
			return nil, err
		}
	case models.PostgresV3Catalog:
		spec := postgresV3CatalogYamlSpec{
			Metadata:         mock.Spec.Metadata,
			Catalog:          mock.Spec.PostgresV3Catalog,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		if err := yamlDoc.Spec.Encode(spec); err != nil {
			utils.LogError(logger, err, "failed to marshal PostgresV3Catalog as yaml",
				zap.String("mock_name", mock.Name),
				zap.String("mock_kind", string(mock.Kind)),
				zap.String("next_step", nextStepPostgresV3Encode))
			return nil, err
		}
	case models.PostgresV3Data:
		spec := postgresV3DataYamlSpec{
			Metadata:         mock.Spec.Metadata,
			Data:             mock.Spec.PostgresV3Data,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		if err := yamlDoc.Spec.Encode(spec); err != nil {
			utils.LogError(logger, err, "failed to marshal PostgresV3Data as yaml",
				zap.String("mock_name", mock.Name),
				zap.String("mock_kind", string(mock.Kind)),
				zap.String("next_step", nextStepPostgresV3Encode))
			return nil, err
		}
	case models.PostgresV3Query:
		spec := postgresV3QueryYamlSpec{
			Metadata:         mock.Spec.Metadata,
			Query:            mock.Spec.PostgresV3Query,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		if err := yamlDoc.Spec.Encode(spec); err != nil {
			utils.LogError(logger, err, "failed to marshal PostgresV3Query as yaml",
				zap.String("mock_name", mock.Name),
				zap.String("mock_kind", string(mock.Kind)),
				zap.String("next_step", nextStepPostgresV3Encode))
			return nil, err
		}
	case models.PostgresV3Generator:
		spec := postgresV3GeneratorYamlSpec{
			Metadata:         mock.Spec.Metadata,
			Generator:        mock.Spec.PostgresV3Generator,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		if err := yamlDoc.Spec.Encode(spec); err != nil {
			utils.LogError(logger, err, "failed to marshal PostgresV3Generator as yaml",
				zap.String("mock_name", mock.Name),
				zap.String("mock_kind", string(mock.Kind)),
				zap.String("next_step", nextStepPostgresV3Encode))
			return nil, err
		}
	default:
		utils.LogError(logger, nil, "failed to marshal the recorded mock into yaml due to invalid kind of mock")
		return nil, errors.New("type of mock is invalid")
	}

	return &yamlDoc, nil
}

// ---------------------------------------------------------------------------
// PostgresV3 YAML Spec envelopes — one per Kind. Each carries the
// metadata + the typed payload + timestamps.
// ---------------------------------------------------------------------------

// Remediation strings attached to PostgresV3 yaml encode/decode errors
// via the structured `next_step` field. Kept here so both halves of
// the switch use identical guidance and operators get a consistent
// signal in logs.
const (
	nextStepPostgresV3Encode = "The mock could not be serialised to yaml — inspect mock_name + mock_kind for the offending record, then check the PostgresV3*Spec struct for YAML-specific failure modes: embedded NUL bytes or other control characters (yaml.v3 rejects them outright), invalid UTF-8 in any string field (e.g. raw binary leaking into an un-base64'd column cell), or anchor/alias cycles in map-typed fields. Re-record the affected test-set if the source data is corrupt. (Gob-path issues like nil slice elements are tracked separately — this remediation covers the yaml marshal path only.)"
	nextStepPostgresV3Decode = "The stored PostgresV3 yaml block could not be parsed — compare the mock_kind against the expected envelope (PostgresV3Session / Catalog / Data / Query / Generator) and verify the file was written by a compatible keploy version. If the file was edited by hand, restore from source-of-truth or re-record; otherwise upgrade keploy so the running binary matches the on-disk schema."
)

type postgresV3SessionYamlSpec struct {
	Metadata         map[string]string             `yaml:"metadata,omitempty"`
	Session          *models.PostgresV3SessionSpec `yaml:"session"`
	ReqTimestampMock time.Time                     `yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time                     `yaml:"resTimestampMock,omitempty"`
}

type postgresV3CatalogYamlSpec struct {
	Metadata         map[string]string             `yaml:"metadata,omitempty"`
	Catalog          *models.PostgresV3CatalogSpec `yaml:"catalog"`
	ReqTimestampMock time.Time                     `yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time                     `yaml:"resTimestampMock,omitempty"`
}

type postgresV3DataYamlSpec struct {
	Metadata         map[string]string          `yaml:"metadata,omitempty"`
	Data             *models.PostgresV3DataSpec `yaml:"data"`
	ReqTimestampMock time.Time                  `yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time                  `yaml:"resTimestampMock,omitempty"`
}

type postgresV3QueryYamlSpec struct {
	Metadata         map[string]string           `yaml:"metadata,omitempty"`
	Query            *models.PostgresV3QuerySpec `yaml:"query"`
	ReqTimestampMock time.Time                   `yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time                   `yaml:"resTimestampMock,omitempty"`
}

type postgresV3GeneratorYamlSpec struct {
	Metadata         map[string]string               `yaml:"metadata,omitempty"`
	Generator        *models.PostgresV3GeneratorSpec `yaml:"generator"`
	ReqTimestampMock time.Time                       `yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time                       `yaml:"resTimestampMock,omitempty"`
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
		case models.PostgresV3Session:
			var spec postgresV3SessionYamlSpec
			if err := m.Spec.Decode(&spec); err != nil {
				utils.LogError(logger, err, "failed to unmarshal PostgresV3Session yaml",
					zap.String("mock_name", m.Name),
					zap.String("mock_kind", string(m.Kind)),
					zap.String("next_step", nextStepPostgresV3Decode))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:          spec.Metadata,
				PostgresV3Session: spec.Session,
				ReqTimestampMock:  spec.ReqTimestampMock,
				ResTimestampMock:  spec.ResTimestampMock,
			}
		case models.PostgresV3Catalog:
			var spec postgresV3CatalogYamlSpec
			if err := m.Spec.Decode(&spec); err != nil {
				utils.LogError(logger, err, "failed to unmarshal PostgresV3Catalog yaml",
					zap.String("mock_name", m.Name),
					zap.String("mock_kind", string(m.Kind)),
					zap.String("next_step", nextStepPostgresV3Decode))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:          spec.Metadata,
				PostgresV3Catalog: spec.Catalog,
				ReqTimestampMock:  spec.ReqTimestampMock,
				ResTimestampMock:  spec.ResTimestampMock,
			}
		case models.PostgresV3Data:
			var spec postgresV3DataYamlSpec
			if err := m.Spec.Decode(&spec); err != nil {
				utils.LogError(logger, err, "failed to unmarshal PostgresV3Data yaml",
					zap.String("mock_name", m.Name),
					zap.String("mock_kind", string(m.Kind)),
					zap.String("next_step", nextStepPostgresV3Decode))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:         spec.Metadata,
				PostgresV3Data:   spec.Data,
				ReqTimestampMock: spec.ReqTimestampMock,
				ResTimestampMock: spec.ResTimestampMock,
			}
		case models.PostgresV3Query:
			var spec postgresV3QueryYamlSpec
			if err := m.Spec.Decode(&spec); err != nil {
				utils.LogError(logger, err, "failed to unmarshal PostgresV3Query yaml",
					zap.String("mock_name", m.Name),
					zap.String("mock_kind", string(m.Kind)),
					zap.String("next_step", nextStepPostgresV3Decode))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:         spec.Metadata,
				PostgresV3Query:  spec.Query,
				ReqTimestampMock: spec.ReqTimestampMock,
				ResTimestampMock: spec.ResTimestampMock,
			}
		case models.PostgresV3Generator:
			var spec postgresV3GeneratorYamlSpec
			if err := m.Spec.Decode(&spec); err != nil {
				utils.LogError(logger, err, "failed to unmarshal PostgresV3Generator yaml",
					zap.String("mock_name", m.Name),
					zap.String("mock_kind", string(m.Kind)),
					zap.String("next_step", nextStepPostgresV3Decode))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:            spec.Metadata,
				PostgresV3Generator: spec.Generator,
				ReqTimestampMock:    spec.ReqTimestampMock,
				ResTimestampMock:    spec.ResTimestampMock,
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
