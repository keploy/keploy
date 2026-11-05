package models

// MetadataUndeliveredKey is the Spec.Metadata key in which a recorder tags an
// answer the client never read, with MetadataUndelivered: the dependency sent
// it, and the client's socket ended before the client read it (enterprise
// Kafka: an empty Fetch answer to a poll the client gave up on). The
// recording keeps it, so it says what the dependency answered, and no replay
// serves it: its parser never does.
//
// So it is no call the application must make again: no test owns it, no
// set's worker scope requires it (NewWindowedMock, the replay's plan), no
// test is judged for it under either switch (the per-test assertion), and
// the prune of unused mocks keeps it, as nothing ever consumes it.
const MetadataUndeliveredKey = "undelivered"

// MetadataUndelivered is the value of MetadataUndeliveredKey on an answer the
// client never read.
const MetadataUndelivered = "true"

// IsUndelivered reports whether m is an answer its recorder tagged as one the
// client never read (MetadataUndeliveredKey).
func (m *Mock) IsUndelivered() bool {
	return m != nil && m.Spec.Metadata[MetadataUndeliveredKey] == MetadataUndelivered
}
