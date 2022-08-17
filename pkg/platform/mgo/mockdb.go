package mgo

import (
	"context"

	"github.com/keploy/go-sdk/integrations/kmongo"
	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.uber.org/zap"
)

func NewMockDB(c *kmongo.Collection, log *zap.Logger) *mockDB {
	return &mockDB{
		c:   c,
		log: log,
	}
}

type mockDB struct {
	c   *kmongo.Collection
	log *zap.Logger
}

func (s *mockDB) UpdateArr(ctx context.Context, app string, testName string, doc models.Mock) error {
	filter := bson.M{"app_id": app, "test_name": testName}
	_, err := s.c.UpdateOne(ctx, filter, bson.M{"$push": bson.M{"deps": bson.M{"$each": doc.Deps}}})
	return err
}

func (s *mockDB) CountDocs(ctx context.Context, app string, testName string) (int64, error) {
	filter := bson.M{"app_id": app, "test_name": testName}
	return s.c.CountDocuments(ctx, filter)
}

func (s *mockDB) Put(ctx context.Context, doc models.Mock) error {
	_, err := s.c.InsertOne(ctx, doc)
	return err
}

func (s *mockDB) Get(ctx context.Context, app string, testName string) ([]models.Mock, error) {
	var result []models.Mock
	filter := bson.M{"app_id": app, "test_name": testName}
	cur, err := s.c.Find(ctx, filter)
	if err != nil {
		return nil, err
	}

	// Loop through the cursor
	for cur.Next(ctx) {
		var doc models.Mock
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
