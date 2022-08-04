package mgo

import (
	"context"

	"github.com/keploy/go-sdk/integrations/kmongo"
	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.uber.org/zap"
)

func NewTestMockDB(c *kmongo.Collection, log *zap.Logger) *testMockDB {
	return &testMockDB{
		c:   c,
		log: log,
	}
}

type testMockDB struct {
	c   *kmongo.Collection
	log *zap.Logger
}

func (s *testMockDB) UpdateArr(ctx context.Context, app string, testName string, doc models.TestMock) error {
	filter := bson.M{"app_id": app, "test_name": testName}
	_, err := s.c.UpdateOne(ctx, filter, bson.M{"$push": bson.M{"deps": bson.M{"$each": doc.Deps}}})
	return err
}

func (s *testMockDB) CountDocs(ctx context.Context, app string, testName string) (int64, error) {
	filter := bson.M{"app_id": app, "test_name": testName}
	return s.c.CountDocuments(ctx, filter)
}

func (s *testMockDB) Insert(ctx context.Context, doc models.TestMock) error {
	_, err := s.c.InsertOne(ctx, doc)
	return err
}

func (s *testMockDB) Get(ctx context.Context, app string, testName string) ([]models.TestMock, error) {
	var result []models.TestMock
	filter := bson.M{"app_id": app, "test_name": testName}
	cur, err := s.c.Find(ctx, filter)
	if err != nil {
		return nil, err
	}

	// Loop through the cursor
	for cur.Next(ctx) {
		var doc models.TestMock
		err = cur.Decode(&doc)
		if err != nil {
			return nil, err

		}
		result = append(result, doc)
	}

	if err = cur.Err(); err != nil {
		return nil, err

	}

	err = cur.Close(ctx)
	if err != nil {
		return nil, err
	}
	return result, nil
}
