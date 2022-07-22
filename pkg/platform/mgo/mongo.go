package mgo

import (
	"context"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"time"
)

func New(uri string) (*mongo.Client, error) {

	clientOptions := options.Client().ApplyURI(uri)
	ctx, _ := context.WithTimeout(context.Background(), 65*time.Second)
	// defer cancel()
	return mongo.Connect(ctx, clientOptions)

}
