package mgo

import (
	"context"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"time"
)

func New(user, pwd, host, db string) (*mongo.Client, error) {
	clientOptions := options.Client()
	//clientOptions.ApplyURI("mongodb://" + host)
	//if user != "" && pwd != "" {
	//	cred := options.Credential{
	//		Username:                user,
	//		Password:                pwd,
	//	}
	//	clientOptions.SetAuth(cred)
	//}
	if user != "" && pwd != "" {
		clientOptions.ApplyURI("mongodb+srv://" + user + ":" + pwd + "@" + host + "/" + db + "?retryWrites=true&w=majority")
	} else {
		clientOptions.ApplyURI("mongodb://" + host + "/" + db + "?retryWrites=true&w=majority")

	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return mongo.Connect(ctx, clientOptions)
}
