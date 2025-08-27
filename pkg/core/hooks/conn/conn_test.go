package conn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEventBodyMaxSizeConstants_ValidValues_123 verifies that the constants `EventBodyMaxSize` and `EventBodyMaxSizeBig` are correctly defined.
func TestEventBodyMaxSizeConstants_ValidValues_123(t *testing.T) {
	assert.Equal(t, 16384, EventBodyMaxSize, "EventBodyMaxSize should be 16384 (16 KB)")
	assert.Equal(t, 4194304, EventBodyMaxSizeBig, "EventBodyMaxSizeBig should be 4194304 (4 MB)")
}
