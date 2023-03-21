package mgo

import (
	"context"
	"sort"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/keploy/go-sdk/integrations/kmongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

func NewTestCase(c *kmongo.Collection, log *zap.Logger) *testCaseDB {

	return &testCaseDB{
		c:   c,
		log: log,
	}
}

type testCaseDB struct {
	c   *kmongo.Collection
	log *zap.Logger
}

func (t *testCaseDB) Delete(ctx context.Context, id string) error {
	_, err := t.c.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	return nil

}

func (t *testCaseDB) GetApps(ctx context.Context, cid string) ([]string, error) {

	filter := bson.M{"cid": cid}
	values, err := t.c.Distinct(ctx, "app_id", filter)
	if err != nil {
		return nil, err
	}
	var apps []string
	for _, v := range values {
		s, ok := v.(string)
		if ok {
			apps = append(apps, s)
		}
	}

	return apps, nil
}

func (t *testCaseDB) GetKeys(ctx context.Context, cid, app, uri, tcsType string) ([]models.TestCase, error) {
	var filter primitive.M
	switch tcsType {
	case string(models.HTTP):
		filter = bson.M{"cid": cid, "app_id": app, "uri": uri}
	case string(models.GRPC_EXPORT):
		filter = bson.M{"cid": cid, "app_id": app, "grpc_req.method": uri}
	}
	findOptions := options.Find()
	findOptions.SetProjection(bson.M{"anchors": 1, "all_keys": 1})
	return t.getAll(ctx, filter, findOptions)
}

// <---- TO-DO ---->
//func (t *testCaseDB) Exists(ctx context.Context, tc models.TestCase) (bool, error) {
//	opts := options.Count().SetMaxTime(2 * time.Second)
//	filters := bson.M{
//		"cid":    tc.CID,
//		"app_id": tc.AppID,
//		"uri":    tc.URI,
//	}
//	for k, v := range tc.Anchors {
//		//if len(v) == 1 {
//		//	filters[k] = v[0]
//		//	continue
//		//}
//		filters["anchors."+k] = bson.M{
//			"$size": len(v),
//			"$all":  v,
//		}
//	}
//	count, err := t.c.CountDocuments(ctx, filters, opts)
//	if err != nil {
//		return false, err
//	}
//	if count > 0 {
//		return true, nil
//	}
//	return false, nil
//}

func (t *testCaseDB) DeleteByAnchor(ctx context.Context, cid, app, uri, tcsType string, filterKeys map[string][]string) error {

	filters := bson.M{}
	switch tcsType {
	case string(models.HTTP):
		filters = bson.M{
			"cid":    cid,
			"app_id": app,
			"uri":    uri,
		}
	case string(models.GRPC_EXPORT):
		filters = bson.M{
			"cid":             cid,
			"app_id":          app,
			"grpc_req.method": uri,
		}
	}
	_, err := t.c.UpdateMany(ctx, filters, bson.M{
		"$set": bson.M{"anchors": filterKeys},
	})
	if err != nil {
		return err
	}
	// remove duplicates
	var dups []string

	filters["anchors"] = bson.M{"$ne": ""}

	pipeline := []bson.M{
		{
			"$match": filters,
		},
		{
			"$group": bson.M{
				"_id":   bson.M{"anchors": "$anchors"},
				"dups":  bson.M{"$addToSet": "$_id"},
				"count": bson.M{"$sum": 1},
			},
		},
		{
			"$match": bson.M{
				"count": bson.M{"$gt": 1},
			},
		},
	}

	opts := options.Aggregate().SetMaxTime(10 * time.Second)

	cur, err := t.c.Aggregate(ctx, pipeline, opts)
	if err != nil {
		return err
	}

	var results []bson.M
	if err = cur.All(ctx, &results); err != nil {
		return err
	}

	for _, result := range results {
		arr := result["dups"].(bson.A)
		for i, v := range arr {
			if i == 1 {
				continue
			}
			dups = append(dups, v.(string))
		}
	}

	if len(dups) > 0 {
		t.log.Info("duplicate testcases deleted", zap.Any("testcase ids: ", dups))
		_, err = t.c.DeleteMany(ctx, bson.M{
			"_id": bson.M{
				"$in": dups,
			},
		})
	}

	if err != nil {
		return err
	}
	return nil
}

// UpdateTC only updates the http request and response of the given testcase.
func (t *testCaseDB) UpdateTC(ctx context.Context, tc models.TestCase) error {
	filter := bson.M{"_id": tc.ID}
	update := bson.D{{Key: "$set", Value: bson.M{"http_req": tc.HttpReq, "http_resp": tc.HttpResp}}}
	_, err := t.c.UpdateOne(ctx, filter, update)
	if err != nil {
		return err
	}
	return nil
}

func (t *testCaseDB) Upsert(ctx context.Context, tc models.TestCase) error {

	// sort arrays before insert
	for _, v := range tc.Anchors {
		sort.Strings(v)
	}
	upsert := true
	opt := &options.UpdateOptions{
		Upsert: &upsert,
	}
	filter := bson.M{"_id": tc.ID}
	update := bson.D{{Key: "$set", Value: tc}}

	_, err := t.c.UpdateOne(ctx, filter, update, opt)
	if err != nil {
		//t.log.Error("failed to insert testcase into DB", zap.String("cid", tc.CID), zap.String("appid", tc.AppID), zap.String("id", tc.ID), zap.Error())
		return err
	}
	return nil
}

func (t *testCaseDB) Get(ctx context.Context, cid, id string) (models.TestCase, error) {
	// too repetitive
	// TODO write a generic FindOne for all get calls
	filter := bson.M{"_id": id}
	if cid != "" {
		filter["cid"] = cid
	}

	var tc models.TestCase

	err := t.c.FindOne(ctx, filter).Decode(&tc)
	if err != nil {
		return tc, err
	}
	return tc, nil
}

func (t *testCaseDB) getAll(ctx context.Context, filter bson.M, findOptions *options.FindOptions) ([]models.TestCase, error) {
	var tcs []models.TestCase
	cur, err := t.c.Find(ctx, filter, findOptions)
	if err != nil {
		return nil, err
	}

	// Loop through the cursor
	for cur.Next(ctx) {
		var tc models.TestCase
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

func (t *testCaseDB) GetAll(ctx context.Context, cid, app string, anchors bool, offset int, limit int) ([]models.TestCase, error) {
	filter := bson.M{"cid": cid, "app_id": app}
	findOptions := options.Find()
	if !anchors {
		findOptions.SetProjection(bson.M{"anchors": 0, "all_keys": 0})
	}
	if offset < 0 {
		offset = 0
	}

	findOptions.SetSkip(int64(offset))
	findOptions.SetLimit(int64(limit))
	findOptions.SetSort(bson.M{"created": -1}) //reverse sort

	return t.getAll(ctx, filter, findOptions)
}
