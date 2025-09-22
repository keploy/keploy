//go:build !linux

package integrations

import (
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
)

type IntegrationType string

// constants for different types of integrations
const (
	HTTP        IntegrationType = "http"
	GRPC        IntegrationType = "grpc"
	GENERIC     IntegrationType = "generic"
	MYSQL       IntegrationType = "mysql"
	POSTGRES_V1 IntegrationType = "postgres_v1"
	POSTGRES_V2 IntegrationType = "postgres_v2"
	MONGO       IntegrationType = "mongo"
	REDIS       IntegrationType = "redis"
)

// DecoderFunc represents a function that can decode YAML content for a specific integration type
type DecoderFunc func(networkDoc *yaml.NetworkTrafficDoc, logger *zap.Logger) (*models.Mock, error)

// DecoderRegistry manages decoder functions for different integration types
type DecoderRegistry struct {
	decoders map[IntegrationType]DecoderFunc
}

// Global decoder registry instance
var DecoderReg = &DecoderRegistry{
	decoders: make(map[IntegrationType]DecoderFunc),
}

// RegisterDecoder registers a decoder function for a specific integration type
func (dr *DecoderRegistry) RegisterDecoder(integrationType IntegrationType, decoder DecoderFunc) {
	dr.decoders[integrationType] = decoder
}

// GetDecoder retrieves a decoder function for a specific integration type
func (dr *DecoderRegistry) GetDecoder(integrationType IntegrationType) (DecoderFunc, bool) {
	decoder, exists := dr.decoders[integrationType]
	return decoder, exists
}

// RegisterDecoder is a global convenience function to register a decoder
func RegisterDecoder(integrationType IntegrationType, decoder DecoderFunc) {
	DecoderReg.RegisterDecoder(integrationType, decoder)
}

// GetDecoder is a global convenience function to get a decoder
func GetDecoder(integrationType IntegrationType) (DecoderFunc, bool) {
	return DecoderReg.GetDecoder(integrationType)
}
