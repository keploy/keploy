package models

type Dependency struct {
	Name string            `json:"name" bson:"name,omitempty"`
	Type DependencyType    `json:"type" bson:"type,omitempty"`
	Meta map[string]string `json:"meta" bson:"meta,omitempty"`
	Data [][]byte          `json:"data" bson:"data,omitempty"`
}

type DependencyType string

const (
	NoSqlDB    DependencyType = "NO_SQL_DB"
	SqlDB      DependencyType = "SQL_DB"
	GRPC       DependencyType = "GRPC"
	HttpClient DependencyType = "HTTP_CLIENT"
)
