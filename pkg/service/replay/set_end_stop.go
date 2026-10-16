package replay

import "time"

// setEndDrainBudget bounds the read of what the app consumed as the set's
// end stopped it (P101-b): the agent's client has no deadline of its own, so
// a read that never ends would hold the run for good. A read that runs out
// leaves the set not judged.
var setEndDrainBudget = 30 * time.Second
