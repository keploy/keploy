package mockdb

import (
	"errors"
	"testing"

	"context"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
	yamlV3 "gopkg.in/yaml.v3"
)

// TestEncodeMock_AllTypes_And_Errors_371 provides comprehensive testing for the EncodeMock function.
type erroringYAML struct {
	err error
}

func (e *erroringYAML) MarshalYAML() (interface{}, error) {
	if e.err == nil {
		return nil, errors.New("mock yaml marshal error")
	}
	return nil, e.err
}

func TestEncodeMock_AllTypes_And_Errors_371(t *testing.T) {
	logger := zap.NewNop()

	// --- Success Cases ---
	t.Run("SuccessCases", func(t *testing.T) {
		testCases := []struct {
			name string
			mock *models.Mock
		}{
			{
				name: "HTTP",
				mock: &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{}, HTTPResp: &models.HTTPResp{}}},
			},
			{
				name: "Mongo",
				mock: &models.Mock{Kind: models.Mongo, Spec: models.MockSpec{MongoRequests: []models.MongoRequest{}, MongoResponses: []models.MongoResponse{}}},
			},
			{
				name: "GRPC",
				mock: &models.Mock{Kind: models.GRPC_EXPORT, Spec: models.MockSpec{GRPCReq: &models.GrpcReq{}, GRPCResp: &models.GrpcResp{}}},
			},
			{
				name: "Generic",
				mock: &models.Mock{Kind: models.GENERIC, Spec: models.MockSpec{}},
			},
			{
				name: "Redis",
				mock: &models.Mock{Kind: models.REDIS, Spec: models.MockSpec{}},
			},
			{
				name: "Postgres",
				mock: &models.Mock{Kind: models.Postgres, Spec: models.MockSpec{}},
			},
			{
				name: "MySQL",
				mock: &models.Mock{
					Kind: models.MySQL,
					Spec: models.MockSpec{
						MySQLRequests: []mysql.Request{
							{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: mysql.CommandStatusToString(mysql.COM_QUIT)}, Message: &mysql.QuitPacket{}}},
						},
						MySQLResponses: []mysql.Response{
							{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: mysql.StatusToString(mysql.OK)}, Message: &mysql.OKPacket{}}},
						},
					},
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				doc, err := EncodeMock(tc.mock, logger)
				require.NoError(t, err)
				require.NotNil(t, doc)
				assert.Equal(t, tc.mock.Kind, doc.Kind)
			})
		}
	})

	// --- Error Cases ---
	t.Run("ErrorCases", func(t *testing.T) {
		// Case: MySQL Request Encoding Error
		t.Run("MySQLEncodeRequestError", func(t *testing.T) {
			mock := &models.Mock{
				Kind: models.MySQL,
				Spec: models.MockSpec{
					MySQLRequests: []mysql.Request{
						{PacketBundle: mysql.PacketBundle{Message: &erroringYAML{}}},
					},
				},
			}
			doc, err := EncodeMock(mock, logger)
			require.Error(t, err)
			assert.Nil(t, doc)
			assert.Contains(t, err.Error(), "mock yaml marshal error")
		})

		// Case: MySQL Response Encoding Error
		t.Run("MySQLEncodeResponseError", func(t *testing.T) {
			mock := &models.Mock{
				Kind: models.MySQL,
				Spec: models.MockSpec{
					MySQLResponses: []mysql.Response{
						{PacketBundle: mysql.PacketBundle{Message: &erroringYAML{}}},
					},
				},
			}
			doc, err := EncodeMock(mock, logger)
			require.Error(t, err)
			assert.Nil(t, doc)
			assert.Contains(t, err.Error(), "mock yaml marshal error")
		})

		// Case: Invalid Mock Kind
		t.Run("InvalidKind", func(t *testing.T) {
			mock := &models.Mock{Kind: "InvalidKind"}
			doc, err := EncodeMock(mock, logger)
			require.Error(t, err)
			assert.Nil(t, doc)
			assert.Equal(t, "type of mock is invalid", err.Error())
		})
	})
}

// TestDecodeMocks_AllTypes_And_Errors_482 covers various decoding scenarios for the decodeMocks function.
func TestDecodeMocks_AllTypes_And_Errors_482(t *testing.T) {
	logger := zap.NewNop()

	encodeToNode := func(t *testing.T, v interface{}) yamlV3.Node {
		var node yamlV3.Node
		err := node.Encode(v)
		require.NoError(t, err)
		return node
	}

	t.Run("SuccessAndSkipCases", func(t *testing.T) {
		httpSpecNode := encodeToNode(t, models.HTTPSchema{Request: models.HTTPReq{}, Response: models.HTTPResp{}})
		mongoSpecNode := encodeToNode(t, models.MongoSpec{})
		grpcSpecNode := encodeToNode(t, models.GrpcSpec{})
		genericSpecNode := encodeToNode(t, models.GenericSchema{})
		redisSpecNode := encodeToNode(t, models.RedisSchema{})
		postgresSpecNode := encodeToNode(t, models.PostgresSpec{})
		mysqlSpecNode := encodeToNode(t, mysql.Spec{})

		yamlDocs := []*yaml.NetworkTrafficDoc{
			{Kind: models.HTTP, Spec: httpSpecNode},
			{Kind: models.Mongo, Spec: mongoSpecNode},
			{Kind: models.GRPC_EXPORT, Spec: grpcSpecNode},
			{Kind: models.GENERIC, Spec: genericSpecNode},
			{Kind: models.REDIS, Spec: redisSpecNode},
			{Kind: models.Postgres, Spec: postgresSpecNode},
			{Kind: models.MySQL, Spec: mysqlSpecNode},
			{Kind: "Should-Be-Skipped"},
			{Kind: "Unknown-Kind"},
		}

		mocks, err := decodeMocks(yamlDocs, logger)
		require.NoError(t, err)
		// 7 successful, 2 skipped
		require.Len(t, mocks, 7)
	})

	t.Run("ErrorCases", func(t *testing.T) {
		errorNode := encodeToNode(t, "this will cause a decoding error")

		testCases := []struct {
			name string
			kind models.Kind
		}{
			{"HTTP", models.HTTP},
			{"Mongo", models.Mongo},
			{"GRPC", models.GRPC_EXPORT},
			{"Generic", models.GENERIC},
			{"Redis", models.REDIS},
			{"Postgres", models.Postgres},
			{"MySQL", models.MySQL},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				yamlDocs := []*yaml.NetworkTrafficDoc{
					{Kind: tc.kind, Spec: errorNode},
				}
				_, err := decodeMocks(yamlDocs, logger)
				require.Error(t, err)
			})
		}
	})
}

// TestDecodeMySQLMessage_PacketTypes_And_Errors_593 tests the decoding of various MySQL packet types and error handling.
func TestDecodeMySQLMessage_PacketTypes_And_Errors_593(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	encodeToNode := func(t *testing.T, v interface{}) yamlV3.Node {
		var node yamlV3.Node
		err := node.Encode(v)
		require.NoError(t, err)
		return node
	}

	t.Run("SuccessCases", func(t *testing.T) {
		testCases := []struct {
			name         string
			isRequest    bool
			packetType   string
			message      interface{}
			expectedType interface{}
		}{
			{"COM_QUIT Request", true, mysql.CommandStatusToString(mysql.COM_QUIT), &mysql.QuitPacket{}, &mysql.QuitPacket{}},
			{"OK Response", false, mysql.StatusToString(mysql.OK), &mysql.OKPacket{}, &mysql.OKPacket{}},
			{"ERR Response", false, mysql.StatusToString(mysql.ERR), &mysql.ERRPacket{}, &mysql.ERRPacket{}},
			{"EOF Response", false, mysql.StatusToString(mysql.EOF), &mysql.EOFPacket{}, &mysql.EOFPacket{}},
			{"HandshakeV10 Response", false, mysql.AuthStatusToString(mysql.HandshakeV10), &mysql.HandshakeV10Packet{}, &mysql.HandshakeV10Packet{}},
			{"TextResultSet Response", false, string(mysql.Text), &mysql.TextResultSet{}, &mysql.TextResultSet{}},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				yamlSpec := &mysql.Spec{}
				if tc.isRequest {
					yamlSpec.Requests = []mysql.RequestYaml{{Header: &mysql.PacketInfo{Type: tc.packetType}, Message: encodeToNode(t, tc.message)}}
				} else {
					yamlSpec.Response = []mysql.ResponseYaml{{Header: &mysql.PacketInfo{Type: tc.packetType}, Message: encodeToNode(t, tc.message)}}
				}

				mockSpec, err := decodeMySQLMessage(ctx, logger, yamlSpec)
				require.NoError(t, err)
				if tc.isRequest {
					require.Len(t, mockSpec.MySQLRequests, 1)
					assert.IsType(t, tc.expectedType, mockSpec.MySQLRequests[0].Message)
				} else {
					require.Len(t, mockSpec.MySQLResponses, 1)
					assert.IsType(t, tc.expectedType, mockSpec.MySQLResponses[0].Message)
				}
			})
		}
	})

	t.Run("ErrorCases", func(t *testing.T) {
		errorNode := encodeToNode(t, "not a valid packet struct")
		// Request decode error
		t.Run("RequestDecodeError", func(t *testing.T) {
			yamlSpec := &mysql.Spec{Requests: []mysql.RequestYaml{{Header: &mysql.PacketInfo{Type: mysql.CommandStatusToString(mysql.COM_QUIT)}, Message: errorNode}}}
			_, err := decodeMySQLMessage(ctx, logger, yamlSpec)
			require.Error(t, err)
		})
		// Response decode error
		t.Run("ResponseDecodeError", func(t *testing.T) {
			yamlSpec := &mysql.Spec{Response: []mysql.ResponseYaml{{Header: &mysql.PacketInfo{Type: mysql.StatusToString(mysql.OK)}, Message: errorNode}}}
			_, err := decodeMySQLMessage(ctx, logger, yamlSpec)
			require.Error(t, err)
		})
	})
}

// TestDecodeMongoMessage_FullCoverage_333 provides full coverage for the decodeMongoMessage function.
func TestDecodeMongoMessage_FullCoverage_333(t *testing.T) {
	logger := zap.NewNop()

	encodeToNode := func(t *testing.T, v interface{}) yamlV3.Node {
		var node yamlV3.Node
		err := node.Encode(v)
		require.NoError(t, err)
		return node
	}

	errorNode := encodeToNode(t, "this will fail decoding")

	testCases := []struct {
		name         string
		isRequest    bool
		opcode       wiremessage.OpCode
		goodMessage  interface{}
		expectedType interface{}
	}{
		{"RequestOpMsg", true, wiremessage.OpMsg, &models.MongoOpMessage{}, &models.MongoOpMessage{}},
		{"RequestOpReply", true, wiremessage.OpReply, &models.MongoOpReply{}, &models.MongoOpReply{}},
		{"RequestOpQuery", true, wiremessage.OpQuery, &models.MongoOpQuery{}, &models.MongoOpQuery{}},
		{"ResponseOpMsg", false, wiremessage.OpMsg, &models.MongoOpMessage{}, &models.MongoOpMessage{}},
		{"ResponseOpReply", false, wiremessage.OpReply, &models.MongoOpReply{}, &models.MongoOpReply{}},
		{"ResponseOpQuery", false, wiremessage.OpQuery, &models.MongoOpQuery{}, &models.MongoOpQuery{}},
	}

	for _, tc := range testCases {
		t.Run(tc.name+"_Success", func(t *testing.T) {
			yamlSpec := &models.MongoSpec{}
			if tc.isRequest {
				yamlSpec.Requests = []models.RequestYaml{
					{Header: &models.MongoHeader{Opcode: tc.opcode}, Message: encodeToNode(t, tc.goodMessage)},
				}
			} else {
				yamlSpec.Response = []models.ResponseYaml{
					{Header: &models.MongoHeader{Opcode: tc.opcode}, Message: encodeToNode(t, tc.goodMessage)},
				}
			}
			mockSpec, err := decodeMongoMessage(yamlSpec, logger)
			require.NoError(t, err)
			if tc.isRequest {
				require.Len(t, mockSpec.MongoRequests, 1)
				assert.IsType(t, tc.expectedType, mockSpec.MongoRequests[0].Message)
			} else {
				require.Len(t, mockSpec.MongoResponses, 1)
				assert.IsType(t, tc.expectedType, mockSpec.MongoResponses[0].Message)
			}
		})

		t.Run(tc.name+"_Error", func(t *testing.T) {
			yamlSpec := &models.MongoSpec{}
			if tc.isRequest {
				yamlSpec.Requests = []models.RequestYaml{
					{Header: &models.MongoHeader{Opcode: tc.opcode}, Message: errorNode},
				}
			} else {
				yamlSpec.Response = []models.ResponseYaml{
					{Header: &models.MongoHeader{Opcode: tc.opcode}, Message: errorNode},
				}
			}
			_, err := decodeMongoMessage(yamlSpec, logger)
			require.Error(t, err)
		})
	}

	t.Run("UnknownOpcode", func(t *testing.T) {
		yamlSpec := &models.MongoSpec{
			Requests: []models.RequestYaml{
				{Header: &models.MongoHeader{Opcode: 9999}, Message: encodeToNode(t, "noop")},
			},
			Response: []models.ResponseYaml{
				{Header: &models.MongoHeader{Opcode: 9999}, Message: encodeToNode(t, "noop")},
			},
		}
		mockSpec, err := decodeMongoMessage(yamlSpec, logger)
		require.NoError(t, err)
		require.Len(t, mockSpec.MongoRequests, 1)
		assert.Nil(t, mockSpec.MongoRequests[0].Message)
		require.Len(t, mockSpec.MongoResponses, 1)
		assert.Nil(t, mockSpec.MongoResponses[0].Message)
	})
}

// TestEncodeMock_ErrorPaths_789 tests error paths in EncodeMock.
func TestEncodeMock_ErrorPaths_789(t *testing.T) {
	logger := zap.NewNop()
	erroringMsg := &erroringYAML{}

	testCases := []struct {
		name          string
		mock          *models.Mock
		expectedError string
	}{
		{
			name: "MongoRequestEncodeError",
			mock: &models.Mock{
				Kind: models.Mongo,
				Spec: models.MockSpec{
					MongoRequests: []models.MongoRequest{{Message: erroringMsg}},
				},
			},
			expectedError: "mock yaml marshal error",
		},
		{
			name: "MongoResponseEncodeError",
			mock: &models.Mock{
				Kind: models.Mongo,
				Spec: models.MockSpec{
					MongoResponses: []models.MongoResponse{{Message: erroringMsg}},
				},
			},
			expectedError: "mock yaml marshal error",
		},
		{
			name: "MySQLRequestEncodeError",
			mock: &models.Mock{
				Kind: models.MySQL,
				Spec: models.MockSpec{
					MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: erroringMsg}}},
				},
			},
			expectedError: "mock yaml marshal error",
		},
		{
			name: "MySQLResponseEncodeError",
			mock: &models.Mock{
				Kind: models.MySQL,
				Spec: models.MockSpec{
					MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: erroringMsg}}},
				},
			},
			expectedError: "mock yaml marshal error",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := EncodeMock(tc.mock, logger)
			require.Error(t, err)
			assert.Nil(t, doc)
			assert.Contains(t, err.Error(), tc.expectedError)
		})
	}
}
