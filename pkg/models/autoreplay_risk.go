package models

// IsAutoReplayHighRisk reports whether a failure must not be suppressed as
// noise during auto-replay. Schema changes are protected regardless of the
// matcher's risk grade, since suppressing them would persist noise on the test
// case and hide the change on subsequent runs.
//
// This is the auto-replay policy, not the general replay risk policy.
func IsAutoReplayHighRisk(info FailureInfo) bool {
	if info.Risk == High {
		return true
	}
	for _, category := range info.Category {
		if category == SchemaAdded || category == SchemaBroken {
			return true
		}
	}
	return false
}
