package mockrecord

import (
	"go.keploy.io/server/v3/pkg/models"
)

type metadataCollector struct {
	meta          *models.MockMetadata
	seenProtocols map[string]struct{}
}

func newMetadataCollector() *metadataCollector {
	return &metadataCollector{
		meta:          &models.MockMetadata{},
		seenProtocols: make(map[string]struct{}),
	}
}

func (c *metadataCollector) addProtocol(name string) {
	if name == "" {
		return
	}
	if _, exists := c.seenProtocols[name]; exists {
		return
	}
	c.seenProtocols[name] = struct{}{}
	c.meta.Protocols = append(c.meta.Protocols, name)
}

func (c *metadataCollector) addMock(mock *models.Mock) {
	if mock == nil {
		return
	}

	switch mock.Kind {
	case models.HTTP:
		c.addProtocol("HTTP")
	case models.GRPC_EXPORT:
		c.addProtocol("gRPC")
	case models.Postgres:
		c.addProtocol("Postgres")
	case models.MySQL:
		c.addProtocol("MySQL")
	case models.REDIS:
		c.addProtocol("Redis")
	case models.Mongo:
		c.addProtocol("MongoDB")
	case models.GENERIC:
		c.addProtocol("Generic")
	default:
		c.addProtocol(string(mock.Kind))
	}
}
