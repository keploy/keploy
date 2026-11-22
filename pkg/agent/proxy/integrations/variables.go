package integrations

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// VariableFollower reports whether the parser of one kind of mock follows,
// under exact serving (the proven switch), every variable a mock of that kind
// declares (models.MockVariables): takes the live value where the variable is
// bound, and answers each of its answer places with the value it has in the
// replay. why says what it does not follow, for a mock it does not. A mock
// whose variables its parser does not follow is never served: served as
// recorded, it would answer a call with a value its variable no longer holds.
type VariableFollower func(m *models.Mock) (ok bool, why string)

var (
	followersMu sync.RWMutex
	followers   = map[models.Kind]VariableFollower{}
)

// RegisterVariableFollower registers the follower of kind's mocks, as a parser
// that follows variables does from its init, beside Register (the agent reads
// it when it loads mocks). A kind with no follower follows no variables. HTTP
// mocks' variables are followed by keploy itself, which never consults a
// follower registered for models.HTTP. A nil f unregisters kind's.
func RegisterVariableFollower(kind models.Kind, f VariableFollower) {
	followersMu.Lock()
	defer followersMu.Unlock()
	if f == nil {
		delete(followers, kind)
		return
	}
	followers[kind] = f
}

// VariableFollowerOf is the follower registered for kind's mocks; ok is false
// when none is.
func VariableFollowerOf(kind models.Kind) (VariableFollower, bool) {
	followersMu.RLock()
	defer followersMu.RUnlock()
	f, ok := followers[kind]
	return f, ok
}
