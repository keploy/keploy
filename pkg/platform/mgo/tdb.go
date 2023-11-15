package mgo

import (
	"context"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	mongoClient "go.mongodb.org/mongo-driver/mongo/options"
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
	err := mgo.WriteTestData(mgo.TcsPath, tc.Name, tc)
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

	pageSize := 1
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
		"Spec.reqtimestampmock": bson.M{"$gt": tc.HttpReq.Timestamp},
		"Spec.restimestampmock": bson.M{"$lt": tc.HttpResp.Timestamp},
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

	return tcsMocks, nil
}

func (mgo *Mongo) ReadConfigMocks(path string) ([]*models.Mock, error) {
	var (
		configMocks = []*models.Mock{}
	)

	if path == "" {
		path = mgo.MockPath
	}

	filter := bson.M{"Spec.metadata.type": "config"}
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
	return configMocks, nil
}

func (mgo *Mongo) UpdateTest(mock *models.Mock, ctx context.Context) error {
	return nil
}

func (mgo *Mongo) DeleteTest(mock *models.Mock, ctx context.Context) error {
	return nil
}
