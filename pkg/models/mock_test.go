package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Test generated using Keploy
func TestGetKind_ReturnsKindAsString_001(t *testing.T) {
	// Arrange
	mock := Mock{
		Kind: Kind("test-kind"),
	}

	// Act
	result := mock.GetKind()

	// Assert
	assert.Equal(t, "test-kind", result, "Expected GetKind to return the correct Kind as a string")
}
