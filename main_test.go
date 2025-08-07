package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v2/utils"
)

// TestSetVersion_Empty_001 tests that setVersion correctly sets the version when it's initially empty.
func TestSetVersion_Empty_001(t *testing.T) {
	originalVersion := version
	t.Cleanup(func() {
		version = originalVersion
	})

	version = ""
	setVersion()

	assert.Equal(t, "2-dev", version)
	assert.Equal(t, "2-dev", utils.Version)
	assert.Equal(t, "version", utils.VersionIdenitfier)
}
