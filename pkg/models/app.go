package models

import "context"

type App struct {
	ID      string `json:"id" bson:"_id"`
	Created int64  `json:"created_at" bson:"created,omitempty"`
	Updated int64  `json:"updated" bson:"updated,omitempty"`
	CID     string `json:"cid" bson:"CID"`
}

type AppDB interface {
	GetByCompany(ctx context.Context, cid string) ([]*App, error)
	Exists(ctx context.Context, cid, id string) (bool, error)
	Put(ctx context.Context, a *App) error
}
