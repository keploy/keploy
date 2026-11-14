package secretname

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Name is the one rule that decides whether keploy hides a value by its
// name: a container's fingerprint leaves the value out and a learned line
// prints it as ***. Names of secrets in every common spelling must be caught,
// and ordinary names that merely contain a short keyword must not be.
func TestName(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"PGPASSWORD", true},
		{"requirepass", true},
		{"clientSecret", true},
		{"api_key", true},
		{"X-Api-Key", true},
		{"AUTHORIZATION", true},
		{"db.connection_string", true},
		{"sessionId", true},
		{"jwt", true},
		{"refresh-token", true},

		{"username", false},
		{"host", false},
		{"PORT", false},
		{"keyboard", false},
		{"monkey", false},
		{"pathname", false},
		// Current behavior, not a decision: "passenger" holds "pass", a word
		// that makes any name a secret wherever it appears, so the rule
		// counts it as one. Narrowing that rule is a product decision.
		{"passenger", true},

		{"", false},
		{"__", false},
	} {
		t.Run(fmt.Sprintf("%q", c.name), func(t *testing.T) {
			assert.Equal(t, c.want, Name(c.name))
		})
	}
}
