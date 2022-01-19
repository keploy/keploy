package mgo

import (
	"context"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

func NewApp(c *mongo.Collection, log *zap.Logger) *adb {
	return &adb{
		c:   c,
		log: log,
	}
}

type adb struct {
	c   *mongo.Collection
	log *zap.Logger
}

func (a *adb) GetByCompany(ctx context.Context, cid string) ([]*models.App, error) {
	var apps []*models.App
	filter := bson.M{"cid": cid}
	cur, err := a.c.Find(ctx, filter)
	if err != nil {
		return nil, err
	}

	// Loop through the cursor
	for cur.Next(ctx) {
		var app models.App
		err = cur.Decode(&app)
		if err != nil {
			return nil, err

		}
		apps = append(apps, &app)
	}

	if err = cur.Err(); err != nil {
		return nil, err

	}

	err = cur.Close(ctx)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

func (a *adb) Exists(ctx context.Context, cid, id string) (bool, error) {
	opts := options.Count().SetMaxTime(2 * time.Second)
	filters := bson.M{
		"_id": id,
		"cid": cid,
	}
	count, err := a.c.CountDocuments(ctx, filters, opts)
	if err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	return false, nil
}

func (a *adb) Put(ctx context.Context, app *models.App) error {
	_, err := a.c.InsertOne(ctx, app)
	if err != nil {
		//t.log.Error("failed to insert testcase into DB", zap.String("cid", tc.CID), zap.String("appid", tc.AppID), zap.String("id", tc.ID), zap.Error())
		return err
	}
	return nil
}
