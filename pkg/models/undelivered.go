package models

// MetadataUndeliveredKey is the Spec.Metadata key in which a recorder tags an
// answer the client never read, with MetadataUndelivered: the dependency sent
// it, and the client's socket ended before the client read it (enterprise
// Kafka: an empty Fetch answer to a poll the client gave up on). The
// recording keeps it, so it says what the dependency answered, and no replay
// serves it: its parser never does.
//
// So it is no call the application must make again. IsProtocolPhase reads
// it as no data-plane mock, so under the proven switch every site that asks
// whether a mock is a test's or the worker scope's (the derived mappings,
// keploy mock record's, the replay's plan, the agent's exact view) reads it
// as none; under either switch no test is judged for it (the per-test
// assertion) and no mapping keploy mock record writes lists it; and the
// prune of unused mocks keeps it, as nothing ever consumes it.
const MetadataUndeliveredKey = "undelivered"

// MetadataUndelivered is the value of MetadataUndeliveredKey on an answer the
// client never read.
const MetadataUndelivered = "true"

// MarkUndelivered tags m as an answer the client never read
// (MetadataUndeliveredKey).
func (m *Mock) MarkUndelivered() {
	if m.Spec.Metadata == nil {
		m.Spec.Metadata = map[string]string{}
	}
	m.Spec.Metadata[MetadataUndeliveredKey] = MetadataUndelivered
}

// IsUndelivered reports whether m is an answer its recorder tagged as one the
// client never read (MetadataUndeliveredKey).
func (m *Mock) IsUndelivered() bool {
	return m != nil && m.Spec.Metadata[MetadataUndeliveredKey] == MetadataUndelivered
}
