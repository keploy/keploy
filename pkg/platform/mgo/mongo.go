package mgo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func New(mongoUri string) (*mongo.Client, error) {

	clientOptions := options.Client().ApplyURI(mongoUri)
	ctx, _ := context.WithTimeout(context.Background(), 65*time.Second)
	// defer cancel()
	return mongo.Connect(ctx, clientOptions)
}
