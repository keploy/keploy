package models

// MetadataUndeliveredKey is the Spec.Metadata key in which a recorder tags an
// answer the client never read, with MetadataUndelivered: the dependency sent
// it, and the client's socket ended before the client read it (enterprise
// Kafka: an empty Fetch answer to a poll the client gave up on). The
// recording keeps it, so it says what the dependency answered, and no replay
// serves it: its parser never does.
//
// So it is no call the application must make again: OutsideTestScopes reads
// it as no test's and no worker scope's, under either switch, at every site
// that decides what a test or the worker scope requires (the derived
// mappings, the mappings keploy mock record and a --sync recording write, the
// replay's plan, the agent's exact view of a test's own mocks); no test is
// judged for it (the per-test assertion); and the prune of unused mocks keeps
// it, as nothing ever consumes it. It stays a data-plane mock to every site
// that serves or matches (IsProtocolPhase does not read the tag), so no path
// that serves protocol-phase mocks ever takes it for one.
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

// OutsideTestScopes reports whether no test and no set's worker scope can
// require m: a protocol-phase mock (IsProtocolPhase), or an answer the client
// never read (IsUndelivered), which no replay serves. Only the sites that
// decide what a test or the worker scope requires ask it; a site that serves
// or matches asks IsProtocolPhase, which reads a tagged answer as the
// data-plane answer it is.
func (m *Mock) OutsideTestScopes() bool {
	return m.IsProtocolPhase() || m.IsUndelivered()
}
