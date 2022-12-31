package mgo

import (
	"context"
	"time"

	"go.keploy.io/server/pkg/models"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/keploy/go-sdk/integrations/kmongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

func NewRun(c *kmongo.Collection, test *kmongo.Collection, log *zap.Logger) *RunDB {
	return &RunDB{
		c:    c,
		log:  log,
		test: test,
	}
}

type RunDB struct {
	c    *kmongo.Collection
	test *kmongo.Collection
	log  *zap.Logger
}

func (r *RunDB) ReadTest(ctx context.Context, id string) (models.Test, error) {

	// too repetitive
	// TODO write a generic FindOne for all get calls
	filter := bson.M{"_id": id}
	var t models.Test
	err := r.test.FindOne(ctx, filter).Decode(&t)
	if err != nil {
		return t, err
	}
	return t, nil
}

func (r *RunDB) ReadTests(ctx context.Context, runID string) ([]models.Test, error) {

	filter := bson.M{"run_id": runID}
	findOptions := options.Find()

	var res []models.Test
	cur, err := r.test.Find(ctx, filter, findOptions)
	if err != nil {
		return nil, err
	}

	// Loop through the cursor
	for cur.Next(ctx) {
		var t models.Test
		err = cur.Decode(&t)
		if err != nil {
			return nil, err
		}
		res = append(res, t)
	}

	if err = cur.Err(); err != nil {
		return nil, err

	}

	err = cur.Close(ctx)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (r *RunDB) PutTest(ctx context.Context, t models.Test) error {

	upsert := true
	opt := &options.UpdateOptions{
		Upsert: &upsert,
	}
	filter := bson.M{"_id": t.ID}
	update := bson.D{{Key: "$set", Value: t}}

	_, err := r.test.UpdateOne(ctx, filter, update, opt)
	if err != nil {
		//t.log.Error("failed to insert testcase into DB", zap.String("cid", tc.CID), zap.String("appid", tc.AppID), zap.String("id", tc.ID), zap.Error())
		return err
	}
	return nil
}

func (r *RunDB) ReadOne(ctx context.Context, id string) (*models.TestRun, error) {
	filter := bson.M{}
	if id != "" {
		filter["_id"] = id
	}
	testrun := &models.TestRun{}
	cur := r.c.FindOne(ctx, filter)
	err := cur.Decode(testrun)
	return testrun, err
}

func (r *RunDB) Read(ctx context.Context, cid string, user, app, id *string, from, to *time.Time, offset int, limit int) ([]*models.TestRun, error) {

	filter := bson.M{
		"cid": cid,
	}
	if user != nil {
		filter["user"] = user
	}

	if app != nil {
		filter["app"] = app
	}
	if id != nil {
		filter["_id"] = id
	}

	if from != nil {
		filter["updated"] = bson.M{"$gte": from.Unix()}
	}

	if to != nil {
		filter["updated"] = bson.M{"$lte": to.Unix()}
	}

	var tcs []*models.TestRun
	opt := options.Find()

	opt.SetSort(bson.M{"created": -1}) //for descending order
	opt.SetSkip(int64(offset))
	opt.SetLimit(int64(limit))

	cur, err := r.c.Find(ctx, filter, opt)
	if err != nil {
		return nil, err
	}

	// Loop through the cursor
	for cur.Next(ctx) {
		var tc *models.TestRun
		err = cur.Decode(&tc)
		if err != nil {
			return nil, err

		}
		tcs = append(tcs, tc)
	}

	if err = cur.Err(); err != nil {
		return nil, err

	}

	err = cur.Close(ctx)
	if err != nil {
		return nil, err
	}
	return tcs, nil
}

func (r *RunDB) Upsert(ctx context.Context, testRun models.TestRun) error {

	upsert := true
	opt := &options.UpdateOptions{
		Upsert: &upsert,
	}
	filter := bson.M{"_id": testRun.ID}
	update := bson.D{{Key: "$set", Value: testRun}}

	_, err := r.c.UpdateOne(ctx, filter, update, opt)
	if err != nil {
		//t.log.Error("failed to insert testcase into DB", zap.String("cid", tc.CID), zap.String("appid", tc.AppID), zap.String("id", tc.ID), zap.Error())
		return err
	}
	return nil
}

func (r *RunDB) Increment(ctx context.Context, success, failure bool, id string) error {

	update := bson.M{}
	if success {
		update["$inc"] = bson.D{{Key: "success", Value: 1}}
	}

	if failure {
		update["$inc"] = bson.D{{Key: "failure", Value: 1}}
	}

	_, err := r.c.UpdateOne(ctx, bson.M{
		"_id": id,
	}, update, options.Update().SetUpsert(true))

	if err != nil {
		return err
	}
	return nil
}
