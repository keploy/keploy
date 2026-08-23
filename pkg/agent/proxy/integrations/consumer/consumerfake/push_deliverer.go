package consumerfake

import (
	"context"
	"fmt"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
	"go.keploy.io/server/v3/pkg/models"
)

// PushDeliverer is the PUSH-protocol half of the Deliverer contract, shaped
// like a Pulsar connection. It is the sketch design §4 P1 requires before this
// SPI tags, because the pull and push shapes are the one genuine one-way door
// in the interface.
//
// # WHY A PULL-SHAPED SPI WOULD HAVE BEEN WRONG, CONCRETELY
//
// An earlier shape for this was `TakeTrigger(protocol) (*models.Mock, bool)` —
// the parser asks whether there is a trigger waiting for it, and answers the
// application's next poll from what it got. That works for Kafka Fetch and for
// SQS ReceiveMessage, where the application asks for a message every time it
// wants one.
//
// It cannot express Pulsar. A pulsar-client-go consumer sends ONE flow-control
// frame at subscribe carrying its whole receiver queue (receiverQueueSize,
// 1000 by default) and then says nothing more until it has consumed about half
// of them. The broker — here, keploy — is expected to push messages
// unprompted, spending those credits. Under a pull model the parser would have
// exactly ONE prompt for the whole run: test-1 would consume it and tests
// 2..N would never be handed anything at all, every one of them closing on the
// backstop with trigger_not_delivered. Discovering that after the interface
// had shipped on a tagged module would be a breaking change to a published
// SPI, in another repository, with a public retraction.
//
// Arm + Deliver costs the same code and closes it: the GATE decides when a
// test's message goes out, and the parser decides HOW it goes out. A pull
// parser stashes it and answers the next poll; a push parser writes it
// immediately, as this one does.
//
// # WHAT A REAL PULSAR PARSER WOULD DO WITH THIS
//
//  1. On the client's FLOW frame, call GrantFlow with the permits it carries.
//     Nothing else consumes them — in particular, arming does not.
//  2. On Deliver, write one MESSAGE frame to the connection and spend one
//     permit. Deliver is called once per armed test, so N tests spend N of the
//     1000 credits the single FLOW granted.
//  3. Call Gate.MarkTriggerAccepted once the client acknowledges the message,
//     which for Pulsar is the ACK frame — positive evidence the application
//     took it, not merely that bytes were written. Without it a
//     consume-and-write-to-a-database test (which expects zero protocol
//     effects) cannot close on the count rule and times out.
//  4. Refuse rather than guess when there are no permits: the client's
//     receiver queue is full, so writing anyway would have the client drop the
//     message and the test would report "the worker stopped producing" for
//     something keploy did.
type PushDeliverer struct {
	mu      sync.Mutex
	permits int
	pushed  []*models.Mock

	// gate is called back exactly as a real parser would: the trigger is
	// accepted when the client acknowledges it, never when the bytes were
	// written.
	gate *consumer.Gate
	// autoAck models a client that acknowledges every message it is pushed.
	// A real parser sets this from the wire.
	autoAck bool
}

// NewPushDeliverer returns a push-shaped Deliverer bound to gate. autoAck
// models a client that acknowledges what it is pushed, which is what makes
// MarkTriggerAccepted fire.
func NewPushDeliverer(gate *consumer.Gate, autoAck bool) *PushDeliverer {
	return &PushDeliverer{gate: gate, autoAck: autoAck}
}

// GrantFlow records the credits the client's FLOW frame carried. A Pulsar
// client sends this ONCE at subscribe for its whole receiver queue.
func (d *PushDeliverer) GrantFlow(permits int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.permits += permits
}

// Deliver implements consumer.Deliverer by writing the message out
// immediately, unprompted, spending one flow-control permit.
func (d *PushDeliverer) Deliver(_ context.Context, m *models.Mock) error {
	d.mu.Lock()
	if d.permits <= 0 {
		d.mu.Unlock()
		// Refused, never written anyway: a client whose receiver queue is
		// full drops what it is pushed, and the test would then blame the
		// worker for a message keploy threw away.
		return fmt.Errorf("%s: the client has no flow-control permits left, so a pushed message would be dropped", models.CategoryConsumerTriggerDiscarded)
	}
	d.permits--
	d.pushed = append(d.pushed, m)
	gate, ack := d.gate, d.autoAck
	d.mu.Unlock()

	if gate != nil && ack {
		if arm, ok := gate.ArmedTest(); ok {
			gate.MarkTriggerAccepted(arm.TestID)
		}
	}
	return nil
}

// Pushed returns the messages written to the connection, in order.
func (d *PushDeliverer) Pushed() []*models.Mock {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*models.Mock(nil), d.pushed...)
}

// Permits returns the flow-control credits left unspent.
func (d *PushDeliverer) Permits() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.permits
}
