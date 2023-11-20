package mgo

import (
	"context"
	"fmt"
	"strings"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	mongoClient "go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

type Mongo struct {
	TcsPath         string
	MockPath        string
	MockName        string
	TcsName         string
	Logger          *zap.Logger
	tele            *telemetry.Telemetry
	MongoCollection *mongo.Client
}

func NewMongoStore(tcsPath string, mockPath string, tcsName string, mockName string, Logger *zap.Logger, tele *telemetry.Telemetry, client *mongo.Client) platform.TestCaseDB {
	return &Mongo{
		TcsPath:         tcsPath,
		MockPath:        mockPath,
		MockName:        mockName,
		TcsName:         tcsName,
		Logger:          Logger,
		tele:            tele,
		MongoCollection: client,
	}
}

// write is used to generate the mongo file for the recorded calls and writes into the mongo collection.
func (mgo *Mongo) WriteMockData(path, fileName string, doc *models.Mock) error {
	collection := mgo.MongoCollection.Database(models.Keploy).Collection(path)
	_, err := collection.InsertOne(context.TODO(), doc)
	if err != nil {
		mgo.Logger.Error("failed to write the mock data", zap.Error(err), zap.Any("mongo file name", path))
	}
	return nil
}

func (mgo *Mongo) WriteTestData(path, fileName string, doc *models.TestCase) error {
	// Get a handle for your collection
	collection := mgo.MongoCollection.Database(models.Keploy).Collection(path)
	_, err := collection.InsertOne(context.TODO(), doc)
	if err != nil {
		mgo.Logger.Error("failed to write the test case", zap.Error(err), zap.Any("mongo file name", fileName))
	}
	return nil
}

func (mgo *Mongo) WriteTestcase(tc *models.TestCase, ctx context.Context) error {
	mgo.tele.RecordedTestAndMocks()
	testsTotal, ok := ctx.Value("testsTotal").(*int)
	if !ok {
		mgo.Logger.Debug("failed to get testsTotal from context")
	} else {
		*testsTotal++
	}
	if tc.Name == "" {
		tc.Name = mgo.TcsPath
	}
	// find noisy fields
	m, err := yaml.FlattenHttpResponse(pkg.ToHttpHeader(tc.HttpResp.Header), tc.HttpResp.Body)
	if err != nil {
		msg := "error in flattening http response"
		mgo.Logger.Error(msg, zap.Error(err))
	}
	noise := tc.Noise

	noiseFieldsFound := yaml.FindNoisyFields(m, func(k string, vals []string) bool {
		for _, v := range vals {
			if pkg.IsTime(v) {
				return true
			}
		}
		return pkg.IsTime(strings.Join(vals, ", "))
	})

	for _, v := range noiseFieldsFound {
		noise[v] = []string{}
	}
	tc.Noise = noise
	tc.Name = fmt.Sprint("test-", *testsTotal)
	err = mgo.WriteTestData(mgo.TcsPath, tc.Name, tc)
	if err != nil {
		mgo.Logger.Error("failed to write testcase mongo", zap.Error(err))
		return err
	}
	mgo.Logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", mgo.TcsPath), zap.String("testcase name", tc.Name))
	return nil
}

func (mgo *Mongo) ReadTestcase(path string, lastSeenId *primitive.ObjectID, options interface{}) ([]*models.TestCase, error) {

	if path == "" {
		path = mgo.TcsPath
	}

	tcs := []*models.TestCase{}

	collection := mgo.MongoCollection.Database(models.Keploy).Collection(path)

	pageSize := 10000
	ctx := context.Background()

	filter := bson.M{}
	if !lastSeenId.IsZero() {
		filter = bson.M{"_id": bson.M{"$gt": lastSeenId}}
	}

	findOptions := mongoClient.Find().SetSort(bson.M{"_id": 1}).SetLimit(int64(pageSize))
	cursor, err := collection.Find(context.TODO(), filter, findOptions)
	if err != nil {
		mgo.Logger.Error("failed to find testcase mongo", zap.Error(err))
	}
	defer cursor.Close(ctx)

	err = cursor.All(context.Background(), &tcs)
	if err != nil {
		mgo.Logger.Error("failed to fetch testcase mongo", zap.Error(err))
	}
	if len(tcs) > 0 {
		*lastSeenId = *tcs[len(tcs)-1].ID
	} else {
		*lastSeenId = primitive.NilObjectID
	}
	return tcs, nil
}

func (mgo *Mongo) WriteMock(mock *models.Mock, ctx context.Context) error {
	mocksTotal, ok := ctx.Value("mocksTotal").(*map[string]int)
	if !ok {
		mgo.Logger.Debug("failed to get mocksTotal from context")
	}
	(*mocksTotal)[string(mock.Kind)]++
	if ctx.Value("cmd") == "mockrecord" {
		mgo.tele.RecordedMock(string(mock.Kind))
	}
	if mgo.MockName != "" {
		mock.Name = mgo.MockPath
	}

	if mock.Name == "" {
		mock.Name = "mocks"
	}

	err := mgo.WriteMockData(mgo.MockPath, mock.Name, mock)
	if err != nil {
		return err
	}

	return nil
}

func (mgo *Mongo) ReadTcsMocks(tc *models.TestCase, path string) ([]*models.Mock, error) {
	var (
		tcsMocks = []*models.Mock{}
	)
	filter := bson.M{
		"Spec.ReqTimestampMock": bson.M{"$gt": tc.HttpReq.Timestamp},
		"Spec.ResTimestampMock": bson.M{"$lt": tc.HttpResp.Timestamp},
	}

	collection := mgo.MongoCollection.Database(models.Keploy).Collection(path)
	cursor, err := collection.Find(context.TODO(), filter)
	if err != nil {
		mgo.Logger.Error("failed to find tcs mocks mongo", zap.Error(err))
	}
	defer cursor.Close(context.Background())
	err = cursor.All(context.Background(), &tcsMocks)
	if err != nil {
		mgo.Logger.Error("failed to fetch tcs mocks mongo", zap.Error(err))
	}
	err = decodeMocks(tcsMocks, mgo.Logger)
	if err != nil {
		mgo.Logger.Error("failed to decode tcs mocks mongo", zap.Error(err))
	}
	return tcsMocks, nil
}

func (mgo *Mongo) ReadConfigMocks(path string) ([]*models.Mock, error) {
	var (
		configMocks = []*models.Mock{}
	)

	if path == "" {
		path = mgo.MockPath
	}

	filter := bson.M{"Spec.Metadata.type": "config"}
	collection := mgo.MongoCollection.Database(models.Keploy).Collection(path)
	cursor, err := collection.Find(context.TODO(), filter)
	if err != nil {
		mgo.Logger.Error("failed to fetch config mocks", zap.Error(err))
	}
	defer cursor.Close(context.Background())
	err = cursor.All(context.Background(), &configMocks)
	if err != nil {
		mgo.Logger.Error("failed to fetch config mocks mongo", zap.Error(err))
	}
	err = decodeMocks(configMocks, mgo.Logger)
	if err != nil {
		mgo.Logger.Error("failed to decode config mocks mongo", zap.Error(err))
	}
	return configMocks, nil
}

func (mgo *Mongo) UpdateTest(mock *models.Mock, ctx context.Context) error {
	return nil
}

func (mgo *Mongo) DeleteTest(mock *models.Mock, ctx context.Context) error {
	return nil
}

func decodeMocks(mockSpecs []*models.Mock, logger *zap.Logger) error {
	for _, m := range mockSpecs {
		switch m.Kind {
		case models.Mongo:
			mockSpec, err := decodeMongoMessage(&m.Spec, logger)
			if err != nil {
				return err
			}
			m.Spec = *mockSpec
		case models.SQL:
			mockSpec, err := decodeMySqlMessage(&m.Spec, logger)
			if err != nil {
				return err
			}
			m.Spec = *mockSpec
		default:
			continue
		}
	}

	return nil
}

func decodeMySqlMessage(mockSpec *models.MockSpec, logger *zap.Logger) (*models.MockSpec, error) {
	requests := []models.MySQLRequest{}
	for _, v := range mockSpec.MySqlRequests {
		req := models.MySQLRequest{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		bsonData, err := bson.Marshal(v.Message)
		if err != nil {
			logger.Error(yaml.Emoji+"failed to marshal mongo request document into decodeMySqlMessage ", zap.Error(err))
			return nil, err
		}

		switch v.Header.PacketType {
		case "HANDSHAKE_RESPONSE":
			var requestMessage *models.MySQLHandshakeResponse
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLHandshakeResponse ", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "MySQLQuery":
			var requestMessage *models.MySQLQueryPacket
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLQueryPacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_PREPARE":
			var requestMessage *models.MySQLComStmtPreparePacket
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLComStmtPreparePacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_EXECUTE":
			var requestMessage *models.MySQLComStmtExecute
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLComStmtExecute", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_SEND_LONG_DATA":
			var requestMessage *models.MySQLCOM_STMT_SEND_LONG_DATA
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLCOM_STMT_SEND_LONG_DATA", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_RESET":
			var requestMessage *models.MySQLCOM_STMT_RESET
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLCOM_STMT_RESET", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_FETCH":
			var requestMessage *models.MySQLComStmtFetchPacket
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLComStmtFetchPacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_CLOSE":
			var requestMessage *models.MySQLComStmtClosePacket
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLComStmtClosePacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "AUTH_SWITCH_RESPONSE":
			var requestMessage *models.AuthSwitchRequestPacket
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLComStmtClosePacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_CHANGE_USER":
			var requestMessage *models.MySQLComChangeUserPacket
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLComChangeUserPacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		}
		requests = append(requests, req)
	}
	mockSpec.MySqlRequests = requests

	responses := []models.MySQLResponse{}
	for _, v := range mockSpec.MySqlResponses {
		bsonData, err := bson.Marshal(v.Message)
		if err != nil {
			logger.Error(yaml.Emoji+"failed to marshal mongo response document into decodeMySqlMessage ", zap.Error(err))
			return nil, err
		}
		resp := models.MySQLResponse{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		// decode the yaml document to mysql structs
		switch v.Header.PacketType {
		case "HANDSHAKE_RESPONSE_OK":
			var responseMessage *models.MySQLHandshakeResponseOk
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLHandshakeResponseOk ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLHandshakeV10":
			var responseMessage *models.MySQLHandshakeV10Packet
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLHandshakeV10Packet", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLOK":
			var responseMessage *models.MySQLOKPacket
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLOKPacket ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "COM_STMT_PREPARE_OK":
			var responseMessage *models.MySQLStmtPrepareOk
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLStmtPrepareOk ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "RESULT_SET_PACKET":
			var responseMessage *models.MySQLResultSet
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLResultSet ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "AUTH_SWITCH_REQUEST":
			var responseMessage *models.AuthSwitchRequestPacket
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLResultSet ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLErr":
			var responseMessage *models.MySQLERRPacket
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error(yaml.Emoji+"failed to unmarshal mongo document into MySQLERRPacket ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		}
		responses = append(responses, resp)
	}
	mockSpec.MySqlResponses = responses
	return mockSpec, nil
}

func decodeMongoMessage(mongoSpec *models.MockSpec, logger *zap.Logger) (*models.MockSpec, error) {
	requests := []models.MongoRequest{}
	for _, v := range mongoSpec.MongoRequests {
		req := models.MongoRequest{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		bsonData, err := bson.Marshal(v.Message)
		if err != nil {
			logger.Error(yaml.Emoji+"failed to marshal mongo document into decodeMySqlMessage ", zap.Error(err))
			return nil, err
		}
		switch v.Header.Opcode {
		case wiremessage.OpMsg:
			var requestMessage *models.MongoOpMessage
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error("failed to unmarshal mongo document into mongo OpMsg request wiremessage", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpReply:
			var requestMessage *models.MongoOpReply
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error("failed to unmarshal mongo document into mongo OpReply wiremessage", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpQuery:
			var requestMessage *models.MongoOpQuery
			err = bson.Unmarshal(bsonData, &requestMessage)
			if err != nil {
				logger.Error("failed to unmarshal mongo document into mongo OpQuery wiremessage", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		default:
		}
		requests = append(requests, req)
	}
	mongoSpec.MongoRequests = requests

	responses := []models.MongoResponse{}
	for _, v := range mongoSpec.MongoResponses {
		resp := models.MongoResponse{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		bsonData, err := bson.Marshal(v.Message)
		if err != nil {
			logger.Error(yaml.Emoji+"failed to marshal mongo document into decodeMySqlMessage ", zap.Error(err))
			return nil, err
		}
		switch v.Header.Opcode {
		case wiremessage.OpMsg:
			var responseMessage *models.MongoOpMessage
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error("failed to unmarshal mongo document into mongo OpMsg response wiremessage", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case wiremessage.OpReply:
			var responseMessage *models.MongoOpReply
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error("failed to unmarshal mongo document into mongo OpMsg response wiremessage", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case wiremessage.OpQuery:
			var responseMessage *models.MongoOpQuery
			err = bson.Unmarshal(bsonData, &responseMessage)
			if err != nil {
				logger.Error("failed to unmarshal mongo document into mongo OpMsg response wiremessage", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		default:
		}
		responses = append(responses, resp)
	}
	mongoSpec.MongoResponses = responses
	return mongoSpec, nil
}
