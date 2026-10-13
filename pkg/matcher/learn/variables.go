package learn

// Variables is how L hands a learned variable to the agent's variable
// runtime (agent-side variables, row A2), which binds the value at its
// earlier place and asserts it at the later call, in this round and in every
// replay after it.
type Variables interface {
	// Follow makes the consumer leaf of mock a variable bound at from. It
	// reports whether the runtime follows it; when it does not, L refuses
	// the request.
	//
	// The runtime owns the producer place from then on: L may have learned
	// it as noise or a correlation when the earlier call was served, before
	// anything was known to flow from it, and a variable's producer is
	// captured (its value bound, its type and presence checked), never
	// noise. Follow may be called again for the same pair when a claim
	// races, so it must be idempotent.
	Follow(mock string, consumer Leaf, from Place) bool
}

// Unfollowed is the Variables of an agent without a variable runtime: it
// follows nothing. A request that needs a variable is then a miss
// (request_changed) rather than having its value learned as noise: a value
// that flows from an earlier place must be asserted, and noise is never
// compared, so learning it as noise would stop asserting exactly the value
// the test depends on.
var Unfollowed Variables = unfollowed{}

type unfollowed struct{}

func (unfollowed) Follow(string, Leaf, Place) bool { return false }
