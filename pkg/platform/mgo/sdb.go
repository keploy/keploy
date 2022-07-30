package mgo

import (
	"context"

	"github.com/keploy/go-sdk/integrations/kmongo"
	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.uber.org/zap"
)

func NewSDeps(c *kmongo.Collection, log *zap.Logger) *sDepsDB {
	return &sDepsDB{
		c:   c,
		log: log,
	}
}

type sDepsDB struct {
	c   *kmongo.Collection
	log *zap.Logger
}

func (s *sDepsDB) UpdateArr(ctx context.Context, app string, testName string, doc models.InfraDeps) error {
	filter := bson.M{"app_id": app, "test_name": testName}
	_, err := s.c.UpdateOne(ctx, filter, bson.M{"$push": bson.M{"deps": bson.M{"$each": doc.Deps}}})
	return err
}

func (s *sDepsDB) CountDocs(ctx context.Context, app string, testName string) (int64, error) {
	filter := bson.M{"app_id": app, "test_name": testName}
	return s.c.CountDocuments(ctx, filter)
}

func (s *sDepsDB) Insert(ctx context.Context, doc models.InfraDeps) error {
	_, err := s.c.InsertOne(ctx, doc)
	return err
}

func (s *sDepsDB) Get(ctx context.Context, app string, testName string) ([]models.InfraDeps, error) {
	var result []models.InfraDeps
	filter := bson.M{"app_id": app, "test_name": testName}
	cur, err := s.c.Find(ctx, filter)
	if err != nil {
		return nil, err
	}

	// Loop through the cursor
	for cur.Next(ctx) {
		var doc models.InfraDeps
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
