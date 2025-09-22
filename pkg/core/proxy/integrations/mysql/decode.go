//go:build linux

// Package mysql provides functionality for decoding MySQL network traffic.
package mysql

import (
	"context"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// DecodeMySQLMock converts a NetworkTrafficDoc with MySQL kind to a Mock
func DecodeMySQLMock(networkDoc *yaml.NetworkTrafficDoc, logger *zap.Logger) (*models.Mock, error) {
	if networkDoc.Kind != models.MySQL {
		return nil, fmt.Errorf("expected MySQL mock kind, got %s", networkDoc.Kind)
	}

	mock := models.Mock{
		Version:      networkDoc.Version,
		Name:         networkDoc.Name,
		Kind:         networkDoc.Kind,
		ConnectionID: networkDoc.ConnectionID,
	}

	mySQLSpec := mysql.Spec{}
	err := networkDoc.Spec.Decode(&mySQLSpec)
	if err != nil {
		utils.LogError(logger, err, "failed to unmarshal a yaml doc into mysql mock", zap.String("mock name", networkDoc.Name))
		return nil, err
	}

	mockSpec, err := decodeMySQLMessage(context.Background(), logger, &mySQLSpec)
	if err != nil {
		return nil, err
	}
	mock.Spec = *mockSpec

	return &mock, nil
}

// decodeMySQLMessage decodes MySQL-specific message format
func decodeMySQLMessage(_ context.Context, logger *zap.Logger, yamlSpec *mysql.Spec) (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata:         yamlSpec.Metadata,
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

		case mysql.AuthStatusToString(mysql.AuthNextFactor):
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
