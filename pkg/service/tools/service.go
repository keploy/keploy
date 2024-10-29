// Package tools provides utility functions for the service package.
package tools

import (
	"context"
)

type Service interface {
	Update(ctx context.Context) error
	CreateConfig(ctx context.Context, filePath string, config string) error
	SendTelemetry(event string, output ...map[string]interface{})
	Login(ctx context.Context) bool
	Export(ctx context.Context) error
}

type teleDB interface {
	SendTelemetry(event string, output ...map[string]interface{})
}
